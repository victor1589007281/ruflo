package observability

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JSONLStore 将 observability 事件持久化到 JSONL 文件。
// 每个事件类型一个文件, 便于按 scrape 节奏读取。
type JSONLStore struct {
	mu     sync.Mutex
	dir    string
	files  map[string]*os.File // event type -> file handle
	closed bool
}

// NewJSONLStore 创建事件存储, 事件写入 dir/ 下。
func NewJSONLStore(dir string) (*JSONLStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建目录 %s: %w", dir, err)
	}
	return &JSONLStore{
		dir:   dir,
		files: make(map[string]*os.File),
	}, nil
}

// WriteEvent 将事件追加到对应类型的 JSONL 文件。
func (s *JSONLStore) WriteEvent(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("store closed")
	}

	fname := string(e.Type) + ".jsonl"
	fname = sanitizeFilename(fname)
	f, ok := s.files[fname]
	if !ok {
		path := filepath.Join(s.dir, fname)
		var err error
		f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("打开 %s: %w", path, err)
		}
		s.files[fname] = f
	}

	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// Close 关闭所有文件句柄。
func (s *JSONLStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var firstErr error
	for _, f := range s.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.files = nil
	return firstErr
}

// Flush 强制刷盘所有打开的文件。
func (s *JSONLStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.files {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// JSONLStoreSubscriber 将 Bus 事件写入 JSONL 的 subscriber。
type JSONLStoreSubscriber struct {
	store *JSONLStore
}

// NewJSONLStoreSubscriber 创建 JSONL subscriber。
func NewJSONLStoreSubscriber(store *JSONLStore) *JSONLStoreSubscriber {
	return &JSONLStoreSubscriber{store: store}
}

// OnEvent 实现 Subscriber。
func (s *JSONLStoreSubscriber) OnEvent(e Event) {
	_ = s.store.WriteEvent(e)
}

// RotateDaily 每日轮转: 关闭当前文件, 让新事件自动创建新文件。
// 可由外部 cron 每天 00:00 调用。
func (s *JSONLStore) RotateDaily() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.files {
		_ = f.Close()
	}
	s.files = make(map[string]*os.File)
	return nil
}

// sanitizeFilename 把任意字符串净化成安全的文件名: 白名单外的字符一律替换成 '_'。
//
// 注意：白名单里**保留了 ':'** —— 它在 Windows 上是非法文件名字符, 且 UNC 路径里
// 有特殊含义; 在 Linux 上无害。另: 白名单不含路径分隔符, 所以 '..' 这类穿越串
// 会被逐字符改写 ('/' 变 '_'), 但 '.' 本身保留, 纯 ".." 仍会原样通过。
func sanitizeFilename(name string) string {
	// 简单过滤危险字符
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':' {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

// EventFilterSubscriber 按事件类型过滤的 subscriber 包装器。
type EventFilterSubscriber struct {
	types      map[EventType]struct{}
	delegate   Subscriber
}

// NewEventFilterSubscriber 创建过滤 subscriber。
func NewEventFilterSubscriber(delegate Subscriber, types ...EventType) *EventFilterSubscriber {
	m := make(map[EventType]struct{}, len(types))
	for _, t := range types {
		m[t] = struct{}{}
	}
	return &EventFilterSubscriber{types: m, delegate: delegate}
}

// OnEvent 实现 Subscriber, 只转发匹配类型的事件。
func (s *EventFilterSubscriber) OnEvent(e Event) {
	if _, ok := s.types[e.Type]; ok {
		s.delegate.OnEvent(e)
	}
}

// TraceQuery 按 trace_id 查询所有相关事件 (从 JSONL 文件中扫描)。
func TraceQuery(dir, traceID string, maxEvents int) ([]Event, error) {
	if maxEvents <= 0 {
		maxEvents = 10000
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var result []Event
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !stringsHasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		events, err := scanTraceID(path, traceID, maxEvents-len(result))
		if err != nil {
			continue
		}
		result = append(result, events...)
		if len(result) >= maxEvents {
			break
		}
	}
	return result, nil
}

// scanTraceID 流式扫描单个 .jsonl 文件, 收集 trace_id 匹配的事件 (最多 limit 条)。
//
// 算法解读：
//
//	原理：用 json.Decoder 顺序解码而不是整文件读入 —— 日志文件可以远大于内存,
//		流式解码把峰值内存压到"一条事件"的量级。
//	关键不变量：limit 必须 > 0。循环条件 `len(result) < limit` 在 limit <= 0 时
//		恒为假, 一条都读不到; 调用方 TraceQuery 用 maxEvents-len(result) 传参,
//		所以**读满 maxEvents 后本函数会静默返回空**, 那是预期的截断而非故障。
//	代价与边界：任何一条解码失败就 break 并返回已收集的部分 (不报错) —— 文件中间
//		损坏会让后面的匹配全部丢失, 且错误被吞掉 (返回的 err 只有打开文件失败)。
//	设计权衡：不做索引 (按 trace_id 建索引要改写入格式), 用全文件扫描换零维护成本;
//		这也是 TraceQuery 必须设 maxEvents 上限的原因。
func scanTraceID(path, traceID string, limit int) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var result []Event
	dec := json.NewDecoder(f)
	for dec.More() && len(result) < limit {
		var e Event
		if err := dec.Decode(&e); err != nil {
			break
		}
		if e.TraceID == traceID {
			result = append(result, e)
		}
	}
	return result, nil
}

// stringsHasSuffix 是 strings.HasSuffix 的本地实现 (避免为此引入依赖)。
func stringsHasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// DailyRotator 每日自动轮转的定时器。
type DailyRotator struct {
	store   *JSONLStore
	ticker  *time.Ticker
	stop    chan struct{}
	mu      sync.Mutex
	running bool
}

// StartDailyRotator 启动每日轮转 (默认每天检查一次, 在 00:00 附近触发)。
func StartDailyRotator(store *JSONLStore) *DailyRotator {
	r := &DailyRotator{
		store: store,
		stop:  make(chan struct{}),
	}
	go r.loop()
	return r
}

// loop 轮转定时器主循环: 先对齐到下一个 00:00 触发一次, 之后每 24 小时触发一次。
//
// 算法解读：
//
//	原理：两段式 —— 第一段用一次性的 time.After 把相位**对齐到本地零点**, 第二段才
//		换成 24h 的 Ticker 做稳态轮转。若直接开 Ticker, 首次轮转会在启动后 24 小时
//		才发生, 与"每天零点"这个语义不符。
//	关键不变量：对齐只在启动时做一次。Ticker 按**固定周期**发信号而不看真实时钟,
//		所以进程长时间运行后相位会因调度与闰秒缓慢漂移, 不再是精确的 00:00。
//	代价与边界：stop 关闭后 loop 退出, ticker 由 defer 停掉; 但 Stop() 只关通道,
//		**不保证 loop 已退出**, 也不重置 r.ticker —— 重复调用 Stop() 会向已关闭的
//		通道再 close 一次而 panic。
//	设计权衡：不引入 cron 库, 也不在每轮重新计算"距下一个零点还有多久" (那样能消掉
//		漂移, 但要多一次时钟读取与一次 select 重排)。当前实现选的是简单。
func (r *DailyRotator) loop() {
	// 计算到下一个 00:00 的时间
	now := time.Now()
	tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	firstWait := tomorrow.Sub(now)

	select {
	case <-time.After(firstWait):
		r.store.RotateDaily()
	case <-r.stop:
		return
	}

	r.ticker = time.NewTicker(24 * time.Hour)
	defer r.ticker.Stop()
	for {
		select {
		case <-r.ticker.C:
			r.store.RotateDaily()
		case <-r.stop:
			return
		}
	}
}

// Stop 停止轮转器。
func (r *DailyRotator) Stop() {
	close(r.stop)
}
