// Package mysql 是 MySQL 域只读采集包。
//
// 设计边界（ops-rca-agent-design.md §5、§10.2）：
//   - 只读连接 + 只读会话 + 单条语句超时，三层兜底；
//   - 采集失败不是“值为 0”：每条证据带 ProbeValid，失败的探针显式进入 Missing；
//   - 事实在这里产出，模型只解释，不做自由查询。
//
// 本包只执行**写死**的采集 SQL。模型发起的临时 SQL 走 guard.ValidateReadOnly +
// QueryReadOnly，不经过这里的采集路径。
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

// 默认采集参数。采样 3 次是为了拿到“基线与当前”的对比，单点快照无法判断异常。
const (
	defaultSamples        = 3
	defaultSampleInterval = 2 * time.Second
	defaultQueryTimeout   = 8 * time.Second
	defaultWindow         = 30 * time.Minute
)

// Config 采集配置。DSN 只从环境/状态目录读取，不接受模型输入。
type Config struct {
	Instance       string
	DSN            string
	Samples        int
	SampleInterval time.Duration
	QueryTimeout   time.Duration
	Window         time.Duration
	Now            func() time.Time
}

func (c *Config) defaults() {
	if c.Samples <= 0 {
		c.Samples = defaultSamples
	}
	if c.Samples > 10 {
		c.Samples = 10
	}
	if c.SampleInterval <= 0 {
		c.SampleInterval = defaultSampleInterval
	}
	if c.QueryTimeout <= 0 {
		c.QueryTimeout = defaultQueryTimeout
	}
	if c.Window <= 0 {
		c.Window = defaultWindow
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Collector 一次排障会话的采集器。连接池只保留 1–2 条连接，避免采集本身放大负载。
type Collector struct {
	cfg Config
	db  *sql.DB
	now func() time.Time

	// 采集失败记录，最终转成 Missing + ProbeValid=false 证据。
	probeProblems []probeProblem
}

type probeProblem struct {
	Probe  string
	Reason string
	Next   string
}

// New 建立只读连接。返回后的 Collector 必须 Close。
//
// DSN 只做两件事：强制关闭多语句（防注入面），设置连接/读超时（防挂死）。
// 只读账号仍是最后一道闸门 —— 这里不会去 SET 全局变量或提权。
func New(cfg Config) (*Collector, error) {
	cfg.defaults()
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, errors.New("mysql: DSN 为空")
	}
	if strings.TrimSpace(cfg.Instance) == "" {
		return nil, errors.New("mysql: instance 为空")
	}
	p, err := driver.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("mysql: DSN 解析失败: %w", err)
	}
	// 不信任传入 DSN 的这几个开关：多语句会让“只读”变成“可批量”，
	// 而丢失超时会让一次排障卡住整个工具调用。
	p.MultiStatements = false
	p.Timeout = cfg.QueryTimeout
	p.ReadTimeout = cfg.QueryTimeout
	p.WriteTimeout = cfg.QueryTimeout
	p.ParseTime = true
	if p.Loc == nil {
		p.Loc = time.Local
	}

	db, err := sql.Open("mysql", p.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("mysql: 打开连接失败: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.QueryTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("mysql: 连接 %s 失败: %w", cfg.Instance, redactDSNErr(err))
	}
	return &Collector{cfg: cfg, db: db, now: cfg.Now}, nil
}

// Close 释放连接池。
func (c *Collector) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// Instance 返回采集目标名。
func (c *Collector) Instance() string { return c.cfg.Instance }

// redactDSNErr 去掉驱动错误里可能出现的 DSN 片段（凭证）。
func redactDSNErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if i := strings.Index(msg, "@"); i >= 0 {
		// 形如 user:pass@tcp(...) / user:pass@unix(...)
		if j := strings.LastIndex(msg[:i], ":"); j >= 0 {
			msg = msg[:j] + ":***" + msg[i:]
		}
	}
	return errors.New(msg)
}

// snapshot 一次采集的原始读数。所有字段都是“读到才写”，未读到保持零值，
// 由证据的 ProbeValid 区分“确实是 0”与“没读到”。
type snapshot struct {
	window     evidence.TimeWindow
	serverAddr string
	samples    []statusSample

	variables map[string]string
	process   []processRow
	procValid bool
	// probeUser 是本次采集实际使用的账号（CURRENT_USER 的 user 部分）。
	// 用它区分“实例侧异常”与“观测侧自造信号”——监控账号自己的空闲连接
	// 同样会占满 max_connections，如果当成业务泄漏就会得出错误根因。
	probeUser      string
	probeUserValid bool

	lockWaits   int64
	lockDetails []lockRow
	lockValid   bool

	mdlPending int64
	mdlValid   bool

	trx       []trxRow
	trxValid  bool
	replica   *replicaRow
	replValid bool
	replNote  string

	digest   []digestRow
	digValid bool

	capacity   []tableRow
	capValid   bool
	capTotals  capacityTotals
	errorLog   []errorRow
	errLogOK   bool
	errLogNote string

	deadlock      *deadlockInfo
	deadlockValid bool
	deadlockNote  string
}

type statusSample struct {
	at     time.Time
	values map[string]float64
}

type processRow struct {
	User    string
	Host    string
	DB      string
	Command string
	State   string
	Count   int64
	MaxTime int64
}

type lockRow struct {
	WaitingTrx  string
	BlockingTrx string
	Schema      string
	Table       string
	LockType    string
	LockMode    string
}

type trxRow struct {
	TrxID      string
	State      string
	AgeSeconds int64
	ThreadID   int64
	RowsLocked int64
	RowsMod    int64
	Query      string
}

type replicaRow struct {
	IO, SQL         string
	LagSeconds      int64
	SourceHost      string
	LastError       string
	RetrievedGTID   string
	ExecutedGTID    string
	SQLDelaySeconds int64
}

type digestRow struct {
	Schema     string
	Digest     string
	Count      int64
	TotalPS    float64 // 皮秒
	RowsExam   int64
	RowsSent   int64
	NoIndexUse int64
}

func (d digestRow) AvgLatency() time.Duration {
	if d.Count <= 0 {
		return 0
	}
	// SUM_TIMER_WAIT 单位是皮秒；time.Duration 以纳秒计，先除以 1000。
	avgPS := d.TotalPS / float64(d.Count)
	return time.Duration(avgPS/1000) * time.Nanosecond
}

type tableRow struct {
	Schema string
	Table  string
	Engine string
	Rows   int64
	Data   int64
	Index  int64
	Free   int64
}

type capacityTotals struct {
	Data, Index, Free int64
	Tables            int64
}

type errorRow struct {
	At      time.Time
	Prio    string
	Code    string
	Subsys  string
	Message string
}

type deadlockInfo struct {
	At      time.Time
	Recent  bool
	Excerpt string
	HasDate bool
}

// Collect 执行完整采集并返回证据包。分析在 analyze.go。
func (c *Collector) Collect(ctx context.Context) (*evidence.Bundle, error) {
	start := c.now()
	s := &snapshot{window: evidence.TimeWindow{Start: start.Add(-c.cfg.Window), End: start}}

	// 采样：只取状态计数器，单次很轻。三次是为了得到增量（QPS/连接变化）。
	for i := 0; i < c.cfg.Samples; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.cfg.SampleInterval):
			}
		}
		sample, err := c.sampleStatus(ctx)
		if err != nil {
			c.problem("status", err.Error(), "检查只读账号对 performance_schema/全局状态变量的权限")
			continue
		}
		s.samples = append(s.samples, sample)
	}

	c.collectVariables(ctx, s)
	c.collectProbeUser(ctx, s)
	c.collectProcesslist(ctx, s)
	c.collectLocks(ctx, s)
	c.collectTransactions(ctx, s)
	c.collectReplication(ctx, s)
	c.collectDigest(ctx, s)
	c.collectCapacity(ctx, s)
	c.collectErrorLog(ctx, s)
	c.collectDeadlock(ctx, s)

	b := c.buildBundle(s)
	analyze(b, s)
	b.Missing = append(b.Missing, c.missingFromProblems()...)
	b.GeneratedAt = c.now()
	b.Summary = summarize(b)
	return b, nil
}

func (c *Collector) problem(probe, reason, next string) {
	c.probeProblems = append(c.probeProblems, probeProblem{Probe: probe, Reason: reason, Next: next})
}

func (c *Collector) missingFromProblems() []evidence.Missing {
	out := make([]evidence.Missing, 0, len(c.probeProblems))
	seen := map[string]bool{}
	for _, p := range c.probeProblems {
		key := p.Probe + "|" + p.Reason
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, evidence.Missing{
			What:       "采集探针失败: " + p.Probe,
			Why:        p.Reason,
			NextAction: p.Next,
		})
	}
	return out
}

// query 执行一条采集 SQL。超时/权限错误只返回 error，由调用方决定如何标记。
func (c *Collector) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	// 不在这里建带 cancel 的子 context：rows 的生命周期在调用方，提前 cancel 会
	// 在读到一半时中断；而 cancel 不调用又会让计时器泄漏。超时由 DSN 的
	// readTimeout 与工具调用 ctx 兜底。
	return c.db.QueryContext(ctx, q, args...)
}

// queryOne 执行只返回一行的查询。
func (c *Collector) queryOne(ctx context.Context, q string, args ...any) (map[string]string, error) {
	rows, err := c.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return map[string]string{}, nil
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(cols))
	for i, col := range cols {
		out[strings.ToLower(col)] = valueString(vals[i])
	}
	return out, nil
}

// sampleStatus 读一次 SHOW GLOBAL STATUS，只保留白名单计数器。
func (c *Collector) sampleStatus(ctx context.Context) (statusSample, error) {
	rows, err := c.query(ctx, "SHOW GLOBAL STATUS")
	if err != nil {
		return statusSample{}, err
	}
	defer rows.Close()
	want := map[string]bool{
		"uptime": true, "threads_connected": true, "threads_running": true,
		"max_used_connections": true, "queries": true, "questions": true,
		"aborted_connects": true, "connection_errors_max_connections": true,
		"innodb_row_lock_current_waits": true, "innodb_row_lock_waits": true,
		"innodb_buffer_pool_read_requests": true, "innodb_buffer_pool_reads": true,
		"slow_queries": true, "created_threads": true, "threads_created": true,
		"com_select": true, "com_insert": true, "com_update": true, "com_delete": true,
	}
	out := statusSample{at: c.now(), values: map[string]float64{}}
	for rows.Next() {
		var name, val string
		if err := rows.Scan(&name, &val); err != nil {
			return statusSample{}, err
		}
		key := strings.ToLower(name)
		if !want[key] {
			continue
		}
		out.values[key] = parseFloat(val)
	}
	if err := rows.Err(); err != nil {
		return statusSample{}, err
	}
	if len(out.values) == 0 {
		return statusSample{}, errors.New("SHOW GLOBAL STATUS 返回 0 行")
	}
	return out, nil
}

func (c *Collector) collectVariables(ctx context.Context, s *snapshot) {
	rows, err := c.query(ctx, "SHOW GLOBAL VARIABLES")
	if err != nil {
		c.problem("variables", err.Error(), "确认只读账号可读取全局变量")
		return
	}
	defer rows.Close()
	vars := map[string]string{}
	for rows.Next() {
		var name, val string
		if err := rows.Scan(&name, &val); err != nil {
			c.problem("variables", err.Error(), "确认只读账号可读取全局变量")
			return
		}
		vars[strings.ToLower(name)] = val
	}
	if err := rows.Err(); err != nil {
		c.problem("variables", err.Error(), "确认只读账号可读取全局变量")
		return
	}
	s.variables = vars
}

func (c *Collector) collectProbeUser(ctx context.Context, s *snapshot) {
	var current string
	if err := c.db.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&current); err != nil {
		c.problem("current_user", err.Error(), "确认只读账号可执行 SELECT CURRENT_USER()")
		return
	}
	if i := strings.Index(current, "@"); i >= 0 {
		current = current[:i]
	}
	s.probeUser = strings.TrimSpace(current)
	s.probeUserValid = s.probeUser != ""
}

func (c *Collector) collectProcesslist(ctx context.Context, s *snapshot) {
	// 按 host 的网络地址分组，而不是 host:port —— 每个连接都有独立的临时端口，
	// 按完整 host 分组会把 130 个同源空闲会话拆成 130 行，再被 LIMIT 截断成
	// “只有 40 个会话”的假读数。
	const q = `SELECT COALESCE(user,''), SUBSTRING_INDEX(COALESCE(host,''), ':', 1) AS host,
	                  COALESCE(db,''), command, COALESCE(state,''),
	                  COUNT(*) AS n, MAX(time) AS max_time
	           FROM information_schema.processlist
	           GROUP BY 1,2,3,4,5
	           ORDER BY n DESC, max_time DESC
	           LIMIT 40`
	rows, err := c.query(ctx, q)
	if err != nil {
		c.problem("processlist", err.Error(), "确认账号对 information_schema.processlist 有 SELECT 权限")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r processRow
		if err := rows.Scan(&r.User, &r.Host, &r.DB, &r.Command, &r.State, &r.Count, &r.MaxTime); err != nil {
			c.problem("processlist", err.Error(), "processlist 字段扫描失败")
			return
		}
		s.process = append(s.process, r)
	}
	if err := rows.Err(); err != nil {
		c.problem("processlist", err.Error(), "processlist 读行失败")
		return
	}
	s.procValid = true
}

func (c *Collector) collectLocks(ctx context.Context, s *snapshot) {
	// data_lock_waits 在 MySQL 8.0+ 可用；8.4 已移除信息架构的 innodb_lock_waits，
	// 因此这里优先用 performance_schema，失败才回退 sys。
	var waits int64
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM performance_schema.data_lock_waits").Scan(&waits); err != nil {
		if err2 := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sys.innodb_lock_waits").Scan(&waits); err2 != nil {
			c.problem("lock_waits", err.Error()+"; fallback: "+err2.Error(), "确认 performance_schema 已开启且账号可读 data_lock_waits")
			return
		}
		s.lockDetails, _ = c.lockWaitDetails(ctx)
		s.lockWaits = waits
		s.lockValid = true
		return
	}
	s.lockWaits = waits
	if waits > 0 {
		s.lockDetails, _ = c.lockWaitDetails(ctx)
	}
	s.lockValid = true

	var pending int64
	if err := c.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM performance_schema.metadata_locks WHERE lock_status='PENDING'").Scan(&pending); err != nil {
		c.problem("metadata_locks", err.Error(), "确认 performance_schema.metadata_locks 可读")
	} else {
		s.mdlPending = pending
		s.mdlValid = true
	}
}

func (c *Collector) lockWaitDetails(ctx context.Context) ([]lockRow, error) {
	const q = `SELECT CAST(r.ENGINE_TRANSACTION_ID AS CHAR) AS waiting_trx,
	                  CAST(b.ENGINE_TRANSACTION_ID AS CHAR) AS blocking_trx,
	                  COALESCE(r.OBJECT_SCHEMA,''), COALESCE(r.OBJECT_NAME,''),
	                  COALESCE(r.LOCK_TYPE,''), COALESCE(r.LOCK_MODE,'')
	           FROM performance_schema.data_lock_waits w
	           JOIN performance_schema.data_locks r
	             ON r.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
	           JOIN performance_schema.data_locks b
	             ON b.ENGINE_LOCK_ID = w.BLOCKING_ENGINE_LOCK_ID
	           LIMIT 10`
	rows, err := c.query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lockRow
	for rows.Next() {
		var r lockRow
		if err := rows.Scan(&r.WaitingTrx, &r.BlockingTrx, &r.Schema, &r.Table, &r.LockType, &r.LockMode); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (c *Collector) collectTransactions(ctx context.Context, s *snapshot) {
	const q = `SELECT CAST(trx_id AS CHAR) AS trx_id, trx_state,
	                  TIMESTAMPDIFF(SECOND, trx_started, NOW()) AS age_s,
	                  trx_mysql_thread_id, trx_rows_locked, trx_rows_modified,
	                  LEFT(COALESCE(trx_query,''), 240) AS q
	           FROM information_schema.innodb_trx
	           ORDER BY trx_started
	           LIMIT 10`
	rows, err := c.query(ctx, q)
	if err != nil {
		c.problem("innodb_trx", err.Error(), "确认 performance_schema/innodb 状态可读")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r trxRow
		var thread sql.NullInt64
		if err := rows.Scan(&r.TrxID, &r.State, &r.AgeSeconds, &thread, &r.RowsLocked, &r.RowsMod, &r.Query); err != nil {
			c.problem("innodb_trx", err.Error(), "innodb_trx 字段扫描失败")
			return
		}
		r.ThreadID = thread.Int64
		s.trx = append(s.trx, r)
	}
	if err := rows.Err(); err != nil {
		c.problem("innodb_trx", err.Error(), "innodb_trx 读行失败")
		return
	}
	s.trxValid = true
}

func (c *Collector) collectReplication(ctx context.Context, s *snapshot) {
	rows, err := c.query(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		if rows2, err2 := c.query(ctx, "SHOW SLAVE STATUS"); err2 == nil {
			rows = rows2
			err = nil
		}
	}
	if err != nil {
		c.problem("replication", err.Error(), "确认账号可执行 SHOW REPLICA STATUS")
		return
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		c.problem("replication", err.Error(), "读取复制状态列失败")
		return
	}
	if !rows.Next() {
		if rows.Err() != nil {
			c.problem("replication", rows.Err().Error(), "读取复制状态失败")
			return
		}
		s.replValid = true
		s.replNote = "实例不是副本（SHOW REPLICA STATUS 返回 0 行）"
		return
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		c.problem("replication", err.Error(), "复制状态字段扫描失败")
		return
	}
	m := map[string]string{}
	for i, col := range cols {
		m[strings.ToLower(col)] = valueString(vals[i])
	}
	r := &replicaRow{
		IO:            firstNonEmpty(m["replica_io_running"], m["slave_io_running"]),
		SQL:           firstNonEmpty(m["replica_sql_running"], m["slave_sql_running"]),
		SourceHost:    firstNonEmpty(m["source_host"], m["master_host"]),
		LastError:     firstNonEmpty(m["last_error"], m["last_sql_error"]),
		RetrievedGTID: firstNonEmpty(m["retrieved_gtid_set"], m["master_log_file"]),
		ExecutedGTID:  firstNonEmpty(m["executed_gtid_set"], m["relay_master_log_file"]),
	}
	if v := firstNonEmpty(m["seconds_behind_source"], m["seconds_behind_master"]); v != "" && v != "NULL" {
		r.LagSeconds = int64(parseFloat(v))
	} else {
		r.LagSeconds = -1
	}
	if v := m["sql_delay"]; v != "" && v != "NULL" {
		r.SQLDelaySeconds = int64(parseFloat(v))
	}
	s.replica = r
	s.replValid = true
}

func (c *Collector) collectDigest(ctx context.Context, s *snapshot) {
	const q = `SELECT COALESCE(SCHEMA_NAME,''), COALESCE(DIGEST_TEXT,''), COUNT_STAR,
	                  SUM_TIMER_WAIT, SUM_ROWS_EXAMINED, SUM_ROWS_SENT, SUM_NO_INDEX_USED
	           FROM performance_schema.events_statements_summary_by_digest
	           WHERE SCHEMA_NAME IS NOT NULL AND DIGEST_TEXT IS NOT NULL AND COUNT_STAR > 0
	           ORDER BY SUM_TIMER_WAIT DESC
	           LIMIT 5`
	rows, err := c.query(ctx, q)
	if err != nil {
		c.problem("slow_digest", err.Error(), "确认 performance_schema 的 statements_digest 消费者已启用")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r digestRow
		var total sql.NullFloat64
		if err := rows.Scan(&r.Schema, &r.Digest, &r.Count, &total, &r.RowsExam, &r.RowsSent, &r.NoIndexUse); err != nil {
			c.problem("slow_digest", err.Error(), "statement digest 字段扫描失败")
			return
		}
		r.TotalPS = total.Float64
		s.digest = append(s.digest, r)
	}
	if err := rows.Err(); err != nil {
		c.problem("slow_digest", err.Error(), "statement digest 读行失败")
		return
	}
	s.digValid = true
}

func (c *Collector) collectCapacity(ctx context.Context, s *snapshot) {
	const q = `SELECT COALESCE(TABLE_SCHEMA,''), COALESCE(TABLE_NAME,''), COALESCE(ENGINE,''),
	                  COALESCE(TABLE_ROWS,0), COALESCE(DATA_LENGTH,0), COALESCE(INDEX_LENGTH,0),
	                  COALESCE(DATA_FREE,0)
	           FROM information_schema.tables
	           WHERE TABLE_SCHEMA NOT IN ('mysql','information_schema','performance_schema','sys')
	           ORDER BY DATA_LENGTH + INDEX_LENGTH DESC
	           LIMIT 10`
	rows, err := c.query(ctx, q)
	if err != nil {
		c.problem("capacity", err.Error(), "确认账号可读 information_schema.tables")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r tableRow
		if err := rows.Scan(&r.Schema, &r.Table, &r.Engine, &r.Rows, &r.Data, &r.Index, &r.Free); err != nil {
			c.problem("capacity", err.Error(), "information_schema.tables 字段扫描失败")
			return
		}
		s.capacity = append(s.capacity, r)
	}
	if err := rows.Err(); err != nil {
		c.problem("capacity", err.Error(), "information_schema.tables 读行失败")
		return
	}
	s.capValid = true

	var totals capacityTotals
	err = c.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(DATA_LENGTH),0), COALESCE(SUM(INDEX_LENGTH),0),
	                                        COALESCE(SUM(DATA_FREE),0), COUNT(*)
	                                 FROM information_schema.tables
	                                 WHERE TABLE_SCHEMA NOT IN ('mysql','information_schema','performance_schema','sys')`).
		Scan(&totals.Data, &totals.Index, &totals.Free, &totals.Tables)
	if err != nil {
		c.problem("capacity_totals", err.Error(), "汇总表空间失败")
		return
	}
	s.capTotals = totals
}

func (c *Collector) collectErrorLog(ctx context.Context, s *snapshot) {
	// MySQL 8.0.22+ 提供 performance_schema.error_log。log_output 为 FILE 时该表仍
	// 有内容（由 log_error_services 决定），但被过滤的可能性存在，故 ProbeNote 里注明。
	minutes := int(c.cfg.Window.Minutes())
	if minutes < 1 {
		minutes = 1
	}
	q := fmt.Sprintf(`SELECT LOGGED, PRIO, ERROR_CODE, SUBSYSTEM, LEFT(DATA,300)
	                  FROM performance_schema.error_log
	                  WHERE LOGGED >= NOW() - INTERVAL %d MINUTE
	                  ORDER BY LOGGED DESC
	                  LIMIT 30`, minutes)
	rows, err := c.query(ctx, q)
	if err != nil {
		c.problem("error_log", err.Error(), "确认 performance_schema.error_log 可读（MySQL 8.0.22+）")
		s.errLogNote = err.Error()
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r errorRow
		var at time.Time
		if err := rows.Scan(&at, &r.Prio, &r.Code, &r.Subsys, &r.Message); err != nil {
			c.problem("error_log", err.Error(), "error_log 字段扫描失败")
			return
		}
		r.At = at
		s.errorLog = append(s.errorLog, r)
	}
	if err := rows.Err(); err != nil {
		c.problem("error_log", err.Error(), "error_log 读行失败")
		return
	}
	s.errLogOK = true
}

func (c *Collector) collectDeadlock(ctx context.Context, s *snapshot) {
	rows, err := c.query(ctx, "SHOW ENGINE INNODB STATUS")
	if err != nil {
		c.problem("innodb_status", err.Error(), "确认 PROCESS 权限可执行 SHOW ENGINE INNODB STATUS")
		return
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		c.problem("innodb_status", err.Error(), "读取 InnoDB 状态列失败")
		return
	}
	if !rows.Next() {
		c.problem("innodb_status", "SHOW ENGINE INNODB STATUS 返回 0 行", "确认 InnoDB 引擎已启用")
		return
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		c.problem("innodb_status", err.Error(), "InnoDB 状态字段扫描失败")
		return
	}
	var status string
	for i, col := range cols {
		if strings.EqualFold(col, "Status") {
			status = valueString(vals[i])
			break
		}
	}
	s.deadlockValid = true
	if status == "" {
		return
	}
	dl := extractLatestDeadlock(status)
	if dl == nil {
		return
	}
	s.deadlock = dl
	if !dl.Recent {
		s.deadlockNote = "最近一次死锁早于本次时间窗，仅作历史参考"
	}
}

// extractLatestDeadlock 从 SHOW ENGINE INNODB STATUS 文本截取最近一次死锁段。
func extractLatestDeadlock(status string) *deadlockInfo {
	const marker = "LATEST DETECTED DEADLOCK"
	i := strings.Index(status, marker)
	if i < 0 {
		return nil
	}
	rest := status[i:]
	end := strings.Index(rest[len(marker):], "--------")
	var seg string
	if end < 0 {
		seg = rest
	} else {
		seg = rest[:len(marker)+end]
	}
	if len(seg) > 4000 {
		seg = seg[:4000] + "\n... (truncated)"
	}
	dl := &deadlockInfo{Excerpt: strings.TrimSpace(seg)}
	// 段首下一行是时间戳：2026-10-26 10:00:00
	for _, line := range strings.Split(seg, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, marker) {
			continue
		}
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", line, time.Local); err == nil {
			dl.At = t
			dl.HasDate = true
		}
		break
	}
	dl.Recent = dl.HasDate && time.Since(dl.At) <= 30*time.Minute
	return dl
}

// QueryReadOnly 执行一条模型发起的只读 SQL（守卫已在调用方完成）。
// 返回列名与最多 maxRows 行；超限标记 truncated。
func (c *Collector) QueryReadOnly(ctx context.Context, q string, maxRows int) (cols []string, rows [][]string, truncated bool, err error) {
	if maxRows <= 0 {
		maxRows = 200
	}
	qctx, cancel := context.WithTimeout(ctx, c.cfg.QueryTimeout)
	defer cancel()
	rs, err := c.db.QueryContext(qctx, q)
	if err != nil {
		return nil, nil, false, err
	}
	defer rs.Close()
	cols, err = rs.Columns()
	if err != nil {
		return nil, nil, false, err
	}
	for rs.Next() {
		if len(rows) >= maxRows {
			truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, nil, false, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = valueString(v)
		}
		rows = append(rows, row)
	}
	return cols, rows, truncated, rs.Err()
}

// valueString 把驱动返回的任意标量转成可读字符串，且不把 NULL 伪装成 "0"。
func valueString(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05")
	case string:
		return x
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return trimFloat(x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprint(x)
	}
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%.6f", f)
}

func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// normalizeAddr 去掉 host 的端口/协议前缀，便于证据里展示（不暴露内网 IP 的完整形式
// 由上层决定；这里只保留 host:port 的形态供复核）。
func normalizeAddr(host string) string {
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// sortedKeys 仅用于稳定的报告输出。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
