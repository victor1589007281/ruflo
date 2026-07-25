// TaskService — 任务机制抽象 (design/01 §4.12) 与 :7777 动作队列的消费方。
//
// 为什么要这层:
//
//  1. 现状是 RunTeam + tryStartTeam 的"状态判断式去重": 两个并发请求先各自读到
//     非 running 状态, 再靠 manager 级 starting 标志抢锁 —— 抢不到的那个直接吃
//     "正在启动中，请稍后再试" 错误, 调用方无从知道自己想要的那次运行到底是谁的。
//     TaskService 用幂等键把这件事翻过来: 重复提交返回**同一个任务档案**, 调用方
//     拿着同一个 ID 去 Wait, 语义确定, 没有"再试一次"的竞态窗口。
//
//  2. :7777 的动作队列 (<stateDir>/.dashboard/actions/*.json) 历史上**只写不读**:
//     pkg/dashboard/extra_handlers.go 与 v13_handlers.go 各写一处, 全仓零消费方,
//     响应里"等待 claude-go 主进程消费"是空头承诺, 记录烂在盘上。本文件把它升级成
//     TaskService 的 file-queue 后端: 认领 (原子 rename) → 转成任务 / 交注入的执行器
//     → 把消费结果回写进同一个 JSON。动作文件里出现 consumedAt/taskId/status 就是
//     "真被消费"的硬证据。
//
// 幂等键语义 (务必读):
//   - 未显式给 IdempotencyKey 时, 由 team|workflow|objective 派生 (DeriveIdemKey);
//   - IdemScopeActive (默认): 仅当被指向的任务**未到终态**时才归并。同 team 同目标
//     的第二次提交拿到第一次的任务; 第一次跑完 (completed/failed/stopped) 之后再提交
//     则是**新任务** —— 否则"重跑一遍"永远做不到。
//   - IdemScopeForever: 无论终态与否都归并, 用于"这条动作只许被消费一次"。
//   - 另有一道 active 索引 (team|workflow|objective → 活跃任务), 即使调用方给了不同
//     的显式幂等键, 同一团队同一目标也不会被并发跑两遍 —— 这才是 tryStartTeam 的替代。
//   - **不承诺跨进程幂等**: pkg/statestore 的 FileStore 锁是 per-instance 的
//     (见其包注释"跨进程并发写不在本实现保证范围"), 本文件的 submitMu 同样是
//     per-instance。两个 claude-go 进程指向同一 stateDir 并发 Submit 仍可能各建一份档案。
//     动作队列的认领走 os.Rename, 那一步是跨进程原子的。
//
// 向后兼容: 本文件不改 RunTeam/WaitDone/tryStartTeam 的任何签名或行为。TeamRunner 是
// 骑在它们上面的适配器, 两套入口可以并存。
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/statestore"
)

// ---------------------------------------------------------------------------
// 数据模型
// ---------------------------------------------------------------------------

// TaskState 任务生命周期状态。
//
// 与 teams.go 的 TeamStatus 是两套东西, 故意不复用: TeamStatus 描述"团队这个长期
// 存在的实体现在怎么样", TaskState 描述"这一次提交的运行怎么样"。一个团队会有多次
// 任务 (首跑 / 恢复 / 精修), 混用会让"同一团队第二次跑"无处安放。
type TaskState string

const (
	TaskStatePending   TaskState = "pending"   // 已建档, 尚未开跑 (无 runner 时会停在这里)
	TaskStateRunning   TaskState = "running"   // 运行器正在执行
	TaskStatePaused    TaskState = "paused"    // 被 Pause 中断, 可 Resume (团队检查点保留)
	TaskStateCompleted TaskState = "completed" // 成功终态
	TaskStateFailed    TaskState = "failed"    // 失败终态
	TaskStateStopped   TaskState = "stopped"   // 被 Stop 终止的终态
)

// Terminal 是否终态 (Wait 的退出条件)。
// paused 不是终态: 它等着 Resume, Wait 应继续等而不是当成结果返回。
func (s TaskState) Terminal() bool {
	switch s {
	case TaskStateCompleted, TaskStateFailed, TaskStateStopped:
		return true
	}
	return false
}

// TaskSpec 一次任务提交的规格。
//
// 设计稿写的是 Submit(spec GraphSpec, params, opts): 那是 M4 图引擎统一之后的形态。
// 现在生产入口 (:7777 动作队列 / 飞书 /team run / CLI) 携带的全部信息就是
// team+workflow+objective+参数, 用 GraphSpec 反而要在这里凭空造图。等 pkg/graph 的
// GraphSpec 成为唯一分发点后, 给本结构加一个 Graph 字段即可平滑过渡。
type TaskSpec struct {
	Team      string         `json:"team"`               // 团队名 (= 运行的身份)
	Workflow  string         `json:"workflow,omitempty"` // 工作流名; 团队已存在时可省
	Objective string         `json:"objective"`          // 目标描述
	ChatID    string         `json:"chatId,omitempty"`   // 通知回传目标 (飞书 chat)
	Language  string         `json:"language,omitempty"` // 编程语言 (影响编译门禁)
	Params    map[string]any `json:"params,omitempty"`   // 附加参数; feedback/fromNode 由 Refine 写入
}

// IdemScope 幂等归并范围。
type IdemScope string

const (
	// IdemScopeActive 仅归并未到终态的任务 (默认)。
	IdemScopeActive IdemScope = "active"
	// IdemScopeForever 无论终态与否都归并 (动作队列的"只消费一次")。
	IdemScopeForever IdemScope = "forever"
)

// SubmitOpts 提交选项。
type SubmitOpts struct {
	IdempotencyKey string            // 显式幂等键; 空 = 由 spec 派生
	IdemScope      IdemScope         // 归并范围; 空 = IdemScopeActive
	Source         string            // 来源标记: dashboard / cron / feishu / api
	ActionID       string            // 若由动作队列消费而来, 记录动作 ID (可追溯)
	Meta           map[string]string // 自由标签
}

// TaskRefine 一次精修留痕。
type TaskRefine struct {
	At       time.Time `json:"at"`
	Feedback string    `json:"feedback"`
	FromNode string    `json:"fromNode,omitempty"`
}

// TaskRecord 任务档案 (KV 中的持久形态)。
type TaskRecord struct {
	ID          string            `json:"id"`
	Spec        TaskSpec          `json:"spec"`
	State       TaskState         `json:"state"`
	IdemKey     string            `json:"idemKey"`
	IdemScope   IdemScope         `json:"idemScope"`
	Source      string            `json:"source,omitempty"`
	ActionID    string            `json:"actionId,omitempty"`
	Attempts    int               `json:"attempts"` // 启动次数 (首跑 + Resume + Refine)
	SubmittedAt time.Time         `json:"submittedAt"`
	StartedAt   time.Time         `json:"startedAt,omitempty"`
	FinishedAt  time.Time         `json:"finishedAt,omitempty"`
	Output      string            `json:"output,omitempty"`
	Error       string            `json:"error,omitempty"`
	Refines     []TaskRefine      `json:"refines,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
}

// TaskResult Wait 的返回值。
type TaskResult struct {
	ID         string    `json:"id"`
	State      TaskState `json:"state"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// TaskFilter List 过滤条件 (零值 = 全部)。
type TaskFilter struct {
	States []TaskState
	Team   string
	Source string
	Limit  int // <=0 不限
}

// TaskService 任务机制抽象 (design/01 §4.12)。
type TaskService interface {
	// Submit 异步提交任务; 幂等 —— 同幂等键重复提交返回同一份档案而非新建。
	Submit(spec TaskSpec, opts SubmitOpts) (TaskRecord, error)
	// Get 读取任务档案。
	Get(id string) (TaskRecord, bool)
	// Wait 阻塞到任务进入终态 (或 ctx 取消)。
	Wait(ctx context.Context, id string) (TaskResult, error)
	// Pause 中断执行, 保留检查点, 可 Resume。
	Pause(id string) error
	// Resume 从中断/失败处继续 (团队侧走检查点恢复)。
	Resume(id string) error
	// Stop 终止执行 (终态)。
	Stop(id string) error
	// Refine 带反馈精修: 追加留痕并重跑 (fromNode 为空 = 整体重跑)。
	Refine(id, feedback, fromNode string) error
	// List 列出任务档案 (按提交时间倒序)。
	List(f TaskFilter) []TaskRecord
}

// TaskRunner 任务执行器: 把一份任务档案真正跑到终态。
//
// 阻塞调用, 由 TaskService 在独立 goroutine 里驱动; 必须尊重 ctx 取消
// (Pause/Stop 就是靠 cancel + Interrupt 实现的)。返回的 output 是给人看的摘要
// (如报告路径), 不是产物本身。
type TaskRunner interface {
	Run(ctx context.Context, rec TaskRecord) (output string, err error)
}

// TaskRunnerFunc 函数形态的 TaskRunner。
type TaskRunnerFunc func(ctx context.Context, rec TaskRecord) (string, error)

func (f TaskRunnerFunc) Run(ctx context.Context, rec TaskRecord) (string, error) {
	return f(ctx, rec)
}

// TaskInterrupter 可选: 运行器若能主动中断 (而不只依赖 ctx 取消) 实现本接口。
// 团队执行的 ctx 是 RunTeam 自己 context.Background() 派生的, 外部 cancel 到不了,
// 所以 TeamRunner 必须实现这个接口, 靠 StopTeam 才能真停下来。
type TaskInterrupter interface {
	Interrupt(rec TaskRecord) error
}

// ---------------------------------------------------------------------------
// file-queue 实现
// ---------------------------------------------------------------------------

const (
	defaultTaskPollInterval = 200 * time.Millisecond
	defaultStaleClaim       = 10 * time.Minute
)

// TaskServiceOptions FileQueueTaskService 的构造参数。
type TaskServiceOptions struct {
	// Store 持久化后端; nil = statestore.NewMemStore() (测试/嵌入)。
	Store statestore.StateStore
	// Namespace 多实例隔离用的命名空间 (进 bucket 名, 会被扁平化)。
	Namespace string
	// ActionsDir :7777 动作队列目录 (<stateDir>/.dashboard/actions); 空 = 不消费动作。
	ActionsDir string
	// Runner 任务执行器; nil = 任务建档后停在 pending (诚实: 不假装被执行了)。
	Runner TaskRunner
	// ActionExecutor 非任务型动作 (cron.* / swarm.* / team.delete) 的执行器;
	// nil = 这类动作被标记 unsupported 并写明原因, 而不是静默丢弃。
	ActionExecutor ActionExecutor
	// PollInterval Wait 的轮询兜底间隔 (跨进程状态变化只能靠轮询看见); 0 = 200ms。
	PollInterval time.Duration
	// StaleClaim 认领超时: 认领后超过此时长仍未落终态的动作会被回收重投; 0 = 10min。
	StaleClaim time.Duration
}

// FileQueueTaskService TaskService 的 file-queue 实现 (statestore 承载档案 + 目录承载动作)。
type FileQueueTaskService struct {
	records  statestore.KVStore   // 任务档案: id → TaskRecord
	idem     statestore.KVStore   // 幂等索引: idemKey → id
	active   statestore.KVStore   // 活跃索引: team|workflow|objective → id
	journal  statestore.AppendLog // 状态迁移审计流 (append-only)
	runner   TaskRunner
	pollIval time.Duration

	actionsDir string
	staleClaim time.Duration

	execMu   sync.RWMutex
	executor ActionExecutor

	submitMu sync.Mutex // 让"查重 + 建档"在本实例内原子 (跨进程不保证)

	mu       sync.Mutex
	cancels  map[string]context.CancelFunc // id → 运行 ctx 的 cancel
	desired  map[string]TaskState          // id → 被中断后应落到的终态 (paused/stopped)
	waiters  map[string][]chan struct{}    // id → Wait 的唤醒通道
	inFlight sync.WaitGroup

	writeErr atomic.Int64 // 落盘失败计数 (档案缺失不该打断执行, 但须可观测)
	idSeq    atomic.Int64 // 同毫秒内的任务 ID 去撞
}

var _ TaskService = (*FileQueueTaskService)(nil)

// NewFileQueueTaskService 构造 file-queue 任务服务。
func NewFileQueueTaskService(opts TaskServiceOptions) *FileQueueTaskService {
	store := opts.Store
	if store == nil {
		store = statestore.NewMemStore()
	}
	ns := opts.Namespace
	poll := opts.PollInterval
	if poll <= 0 {
		poll = defaultTaskPollInterval
	}
	stale := opts.StaleClaim
	if stale <= 0 {
		stale = defaultStaleClaim
	}
	s := &FileQueueTaskService{
		records:    store.KV(flattenBucket("tasks", ns)),
		idem:       store.KV(flattenBucket("tasks-idem", ns)),
		active:     store.KV(flattenBucket("tasks-active", ns)),
		journal:    store.Log(flattenBucket("tasks-journal", ns)),
		runner:     opts.Runner,
		pollIval:   poll,
		actionsDir: opts.ActionsDir,
		staleClaim: stale,
		executor:   opts.ActionExecutor,
		cancels:    make(map[string]context.CancelFunc),
		desired:    make(map[string]TaskState),
		waiters:    make(map[string][]chan struct{}),
	}
	return s
}

// flattenBucket 把 "prefix" + 命名空间拼成合法 bucket 名。
//
// 为什么要这一层: statestore 的 validateBucket 只放行 [a-zA-Z0-9._-], 禁止 '/'。
// 命名空间常常来自路径或团队名 (含 '/'、中文), 直接拼会让所有读写都退化成
// badBucketKV (每个方法都报错) 且很难察觉。这里做两件事: 消毒非法字符,
// 以及**只要发生过替换就追加内容哈希** —— 否则 "a/b" 与 "a-b" 会被消毒成同一个
// bucket, 两个命名空间的数据静默混在一起。
func flattenBucket(prefix, ns string) string {
	if strings.TrimSpace(ns) == "" {
		return prefix
	}
	var sb strings.Builder
	changed := false
	for _, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
			changed = true
		}
	}
	flat := sb.String()
	if len(flat) > 40 {
		flat = flat[:40]
		changed = true
	}
	if changed {
		flat += "-" + shortHash(ns)
	}
	return prefix + "." + flat
}

// shortHash 取 sha256 前 8 位十六进制 (够避免命名空间/幂等键撞车, 又不啰嗦)。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// DeriveIdemKey 由规格派生默认幂等键: 同 team + 同 workflow + 同 objective 视为同一次提交。
// 用哈希而非原文: objective 可能有几 KB, 直接当 KV key 会把 bucket 文件撑爆。
func DeriveIdemKey(spec TaskSpec) string {
	return "derived-" + shortHash(strings.Join([]string{
		strings.TrimSpace(spec.Team),
		strings.TrimSpace(spec.Workflow),
		strings.TrimSpace(spec.Objective),
	}, "\x00"))
}

// activeKey 活跃索引的键 (与幂等键同构, 但独立成表: 显式幂等键不该绕过"同团队不并发跑")。
func activeKey(spec TaskSpec) string {
	return "active-" + shortHash(strings.Join([]string{
		strings.TrimSpace(spec.Team),
		strings.TrimSpace(spec.Workflow),
		strings.TrimSpace(spec.Objective),
	}, "\x00"))
}

// newTaskID 生成任务 ID: 时间有序 + 序号防撞 + 内容短哈希便于肉眼归类。
func (s *FileQueueTaskService) newTaskID(spec TaskSpec) string {
	return fmt.Sprintf("t-%d-%d-%s", time.Now().UnixMilli(), s.idSeq.Add(1), shortHash(spec.Team+"|"+spec.Objective))
}

// WriteErrors 返回档案落盘失败计数 (fail-open 的可观测出口)。
func (s *FileQueueTaskService) WriteErrors() int64 { return s.writeErr.Load() }

// SetRunner 注入/替换任务执行器 (进程启动接线用; 运行中替换只影响后续任务)。
func (s *FileQueueTaskService) SetRunner(r TaskRunner) {
	s.mu.Lock()
	s.runner = r
	s.mu.Unlock()
}

func (s *FileQueueTaskService) currentRunner() TaskRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runner
}

// Submit 见 TaskService.Submit。
func (s *FileQueueTaskService) Submit(spec TaskSpec, opts SubmitOpts) (TaskRecord, error) {
	if strings.TrimSpace(spec.Team) == "" && strings.TrimSpace(spec.Objective) == "" {
		return TaskRecord{}, fmt.Errorf("taskservice: spec 至少需要 team 或 objective")
	}
	scope := opts.IdemScope
	if scope == "" {
		scope = IdemScopeActive
	}
	key := strings.TrimSpace(opts.IdempotencyKey)
	if key == "" {
		key = DeriveIdemKey(spec)
	}

	s.submitMu.Lock()
	defer s.submitMu.Unlock()

	// ① 显式/派生幂等键命中
	if rec, ok := s.lookupIdem(key, scope); ok {
		return rec, nil
	}
	// ② 同 team+workflow+objective 已有活跃任务 —— 这是 tryStartTeam 的替代:
	//    不再返回"正在启动中，请稍后再试", 而是把那个任务的档案给回去。
	if rec, ok := s.lookupActive(spec); ok {
		if err := s.idem.Put(key, rec.ID); err != nil {
			s.writeErr.Add(1)
			logging.For("taskservice").Warn("幂等索引写入失败", "key", key, "err", err)
		}
		return rec, nil
	}

	rec := TaskRecord{
		ID:          s.newTaskID(spec),
		Spec:        spec,
		State:       TaskStatePending,
		IdemKey:     key,
		IdemScope:   scope,
		Source:      opts.Source,
		ActionID:    opts.ActionID,
		SubmittedAt: time.Now(),
		Meta:        opts.Meta,
	}
	if err := s.records.Put(rec.ID, rec); err != nil {
		return TaskRecord{}, fmt.Errorf("taskservice: 建档失败: %w", err)
	}
	if err := s.idem.Put(key, rec.ID); err != nil {
		// fail-open: 索引写不进去只会退化成"可能重复提交", 不该让任务提交失败。
		s.writeErr.Add(1)
		logging.For("taskservice").Warn("幂等索引写入失败", "key", key, "err", err)
	}
	if err := s.active.Put(activeKey(spec), rec.ID); err != nil {
		s.writeErr.Add(1)
		logging.For("taskservice").Warn("活跃索引写入失败", "task", rec.ID, "err", err)
	}
	s.appendJournal(rec.ID, "", TaskStatePending, "submit")
	s.startRun(rec)
	return rec, nil
}

// lookupIdem 查幂等索引; scope=Active 时终态任务视为未命中 (允许重跑)。
func (s *FileQueueTaskService) lookupIdem(key string, scope IdemScope) (TaskRecord, bool) {
	var id string
	ok, err := s.idem.Get(key, &id)
	if err != nil || !ok || id == "" {
		return TaskRecord{}, false
	}
	rec, found := s.Get(id)
	if !found {
		_ = s.idem.Delete(key) // 档案已被清理, 索引是垃圾
		return TaskRecord{}, false
	}
	if scope == IdemScopeForever {
		return rec, true
	}
	if rec.State.Terminal() {
		return TaskRecord{}, false
	}
	return rec, true
}

// lookupActive 查活跃索引; 指向的任务已终态则顺手清理。
func (s *FileQueueTaskService) lookupActive(spec TaskSpec) (TaskRecord, bool) {
	k := activeKey(spec)
	var id string
	ok, err := s.active.Get(k, &id)
	if err != nil || !ok || id == "" {
		return TaskRecord{}, false
	}
	rec, found := s.Get(id)
	if !found || rec.State.Terminal() {
		_ = s.active.Delete(k)
		return TaskRecord{}, false
	}
	return rec, true
}

// Get 见 TaskService.Get。
func (s *FileQueueTaskService) Get(id string) (TaskRecord, bool) {
	var rec TaskRecord
	ok, err := s.records.Get(id, &rec)
	if err != nil {
		logging.For("taskservice").Warn("读取任务档案失败", "task", id, "err", err)
		return TaskRecord{}, false
	}
	return rec, ok
}

// List 见 TaskService.List。
func (s *FileQueueTaskService) List(f TaskFilter) []TaskRecord {
	keys, err := s.records.Keys()
	if err != nil {
		logging.For("taskservice").Warn("列举任务失败", "err", err)
		return nil
	}
	want := map[TaskState]bool{}
	for _, st := range f.States {
		want[st] = true
	}
	out := make([]TaskRecord, 0, len(keys))
	for _, k := range keys {
		rec, ok := s.Get(k)
		if !ok {
			continue
		}
		if len(want) > 0 && !want[rec.State] {
			continue
		}
		if f.Team != "" && rec.Spec.Team != f.Team {
			continue
		}
		if f.Source != "" && rec.Source != f.Source {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SubmittedAt.After(out[j].SubmittedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// Wait 见 TaskService.Wait。
//
// 双保险: ① 本进程内的状态迁移直接唤醒 waiter (毫秒级);
// ② pollIval 轮询兜底 —— 档案可能被同 stateDir 的另一个进程改写, 那种变化只能看见。
func (s *FileQueueTaskService) Wait(ctx context.Context, id string) (TaskResult, error) {
	if _, ok := s.Get(id); !ok {
		return TaskResult{}, fmt.Errorf("taskservice: 任务 %q 不存在", id)
	}
	ch := s.subscribe(id)
	defer s.unsubscribe(id, ch)

	ticker := time.NewTicker(s.pollIval)
	defer ticker.Stop()
	for {
		rec, ok := s.Get(id)
		if !ok {
			return TaskResult{}, fmt.Errorf("taskservice: 任务 %q 档案已消失", id)
		}
		if rec.State.Terminal() {
			return TaskResult{
				ID: rec.ID, State: rec.State, Output: rec.Output,
				Error: rec.Error, FinishedAt: rec.FinishedAt,
			}, nil
		}
		select {
		case <-ch:
		case <-ticker.C:
		case <-ctx.Done():
			return TaskResult{ID: id, State: rec.State}, ctx.Err()
		}
	}
}

func (s *FileQueueTaskService) subscribe(id string) chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.waiters[id] = append(s.waiters[id], ch)
	s.mu.Unlock()
	return ch
}

func (s *FileQueueTaskService) unsubscribe(id string, ch chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.waiters[id]
	for i, c := range list {
		if c == ch {
			s.waiters[id] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(s.waiters[id]) == 0 {
		delete(s.waiters, id)
	}
}

// notifyWaiters 非阻塞唤醒 (缓冲 1 的通道, 满了说明已有待处理唤醒)。
func (s *FileQueueTaskService) notifyWaiters(id string) {
	s.mu.Lock()
	list := append([]chan struct{}(nil), s.waiters[id]...)
	s.mu.Unlock()
	for _, ch := range list {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// startRun 启动执行 goroutine。runner 为 nil 时任务停在 pending —— 这是有意的诚实:
// 没有执行器就不该把状态写成 running 假装在跑。
func (s *FileQueueTaskService) startRun(rec TaskRecord) {
	runner := s.currentRunner()
	if runner == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[rec.ID] = cancel
	delete(s.desired, rec.ID)
	s.mu.Unlock()

	s.inFlight.Add(1)
	go func() {
		defer s.inFlight.Done()
		defer cancel()
		defer func() {
			s.mu.Lock()
			delete(s.cancels, rec.ID)
			s.mu.Unlock()
		}()
		// 崩溃隔离: 运行器 panic 不该带走整个进程 (团队执行器里有大量第三方路径)。
		defer func() {
			if p := recover(); p != nil {
				s.transition(rec.ID, TaskStateFailed, "", fmt.Sprintf("运行器 panic: %v", p))
			}
		}()

		cur := s.transition(rec.ID, TaskStateRunning, "", "")
		out, err := runner.Run(ctx, cur)

		// 被 Pause/Stop 中断时, 落到请求的那个状态而不是 failed —— 否则用户点了
		// "暂停" 却在界面上看到 "失败", 是误导。
		s.mu.Lock()
		want, interrupted := s.desired[rec.ID]
		delete(s.desired, rec.ID)
		s.mu.Unlock()
		switch {
		case interrupted:
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			s.transition(rec.ID, want, out, msg)
		case err != nil:
			s.transition(rec.ID, TaskStateFailed, out, err.Error())
		default:
			s.transition(rec.ID, TaskStateCompleted, out, "")
		}
	}()
}

// transition 落一次状态迁移 (档案 + 审计流 + 唤醒 waiter), 返回迁移后的档案。
func (s *FileQueueTaskService) transition(id string, to TaskState, output, errMsg string) TaskRecord {
	rec, ok := s.Get(id)
	if !ok {
		return TaskRecord{}
	}
	from := rec.State
	rec.State = to
	now := time.Now()
	switch to {
	case TaskStateRunning:
		rec.StartedAt = now
		rec.Attempts++
		rec.Error = ""
	case TaskStatePending:
		// 从终态被"复活"(Resume/Refine): 清掉上一轮的结束时间与错误,
		// 否则档案会同时带着 pending 状态和上一轮的 FinishedAt, 观测者读不懂。
		rec.FinishedAt = time.Time{}
		rec.Error = ""
	default:
		if to.Terminal() || to == TaskStatePaused {
			rec.FinishedAt = now
		}
		if output != "" {
			rec.Output = output
		}
		rec.Error = errMsg
	}
	if err := s.records.Put(id, rec); err != nil {
		// fail-open: 档案写不进去不回滚执行 (活已经干了), 但计数 + 告警。
		s.writeErr.Add(1)
		logging.For("taskservice").Warn("任务档案落盘失败", "task", id, "state", string(to), "err", err)
	}
	if to.Terminal() {
		s.clearActive(rec)
	}
	s.appendJournal(id, from, to, errMsg)
	s.notifyWaiters(id)
	return rec
}

// clearActive 释放活跃索引 (只在仍指向本任务时删, 免得踩掉后来者)。
func (s *FileQueueTaskService) clearActive(rec TaskRecord) {
	k := activeKey(rec.Spec)
	var id string
	ok, err := s.active.Get(k, &id)
	if err != nil || !ok || id != rec.ID {
		return
	}
	if err := s.active.Delete(k); err != nil {
		s.writeErr.Add(1)
	}
}

func (s *FileQueueTaskService) appendJournal(id string, from, to TaskState, note string) {
	err := s.journal.Append(map[string]any{
		"ts": time.Now().UnixMilli(), "task": id,
		"from": string(from), "to": string(to), "note": note,
	})
	if err != nil {
		s.writeErr.Add(1)
	}
}

// interrupt Pause/Stop 的公共实现: 记下期望终态 → 主动中断 → 取消 ctx。
func (s *FileQueueTaskService) interrupt(id string, want TaskState) error {
	rec, ok := s.Get(id)
	if !ok {
		return fmt.Errorf("taskservice: 任务 %q 不存在", id)
	}
	if rec.State.Terminal() {
		return fmt.Errorf("taskservice: 任务 %q 已是终态 %s", id, rec.State)
	}
	s.mu.Lock()
	s.desired[id] = want
	cancel := s.cancels[id]
	s.mu.Unlock()

	// 运行器可能有自己的 ctx (团队执行的 ctx 由 RunTeam 内部 Background 派生),
	// 只 cancel 外层 ctx 停不下来, 必须给它一次主动中断的机会。
	if it, okI := s.currentRunner().(TaskInterrupter); okI {
		if err := it.Interrupt(rec); err != nil {
			logging.For("taskservice").Warn("中断运行器失败", "task", id, "err", err)
		}
	}
	if cancel != nil {
		cancel()
		return nil
	}
	// 没有在跑的 goroutine (pending / 无 runner): 直接落状态。
	s.mu.Lock()
	delete(s.desired, id)
	s.mu.Unlock()
	s.transition(id, want, "", "")
	return nil
}

// Pause 见 TaskService.Pause。
func (s *FileQueueTaskService) Pause(id string) error { return s.interrupt(id, TaskStatePaused) }

// Stop 见 TaskService.Stop。
func (s *FileQueueTaskService) Stop(id string) error { return s.interrupt(id, TaskStateStopped) }

// Resume 见 TaskService.Resume。paused / failed / stopped 都可续跑 (团队侧走检查点)。
func (s *FileQueueTaskService) Resume(id string) error {
	rec, ok := s.Get(id)
	if !ok {
		return fmt.Errorf("taskservice: 任务 %q 不存在", id)
	}
	switch rec.State {
	case TaskStateRunning:
		return fmt.Errorf("taskservice: 任务 %q 正在运行", id)
	case TaskStatePending:
		// 无 runner 时提交的任务, 现在有 runner 了 → 直接开跑。
	}
	if s.currentRunner() == nil {
		return fmt.Errorf("taskservice: 未注入 TaskRunner, 无法续跑")
	}
	// 复活活跃索引: 续跑期间同 team 同目标不应再被并发提交。
	if err := s.active.Put(activeKey(rec.Spec), rec.ID); err != nil {
		s.writeErr.Add(1)
	}
	// 同步把状态挪出终态再异步开跑: 否则调用方紧接着 Wait 会读到上一轮的终态而立刻返回,
	// 误以为续跑已经结束 (startRun 里的 running 迁移是异步的, 赶不上)。
	rec = s.transition(rec.ID, TaskStatePending, "", "")
	s.startRun(rec)
	return nil
}

// Refine 见 TaskService.Refine。
// 反馈写进 Spec.Params (feedback/fromNode), 由运行器决定怎么用 —— TeamRunner 会
// 转成 RefineTeam(name, feedback, targetStage)。
func (s *FileQueueTaskService) Refine(id, feedback, fromNode string) error {
	if strings.TrimSpace(feedback) == "" {
		return fmt.Errorf("taskservice: 精修反馈不能为空")
	}
	rec, ok := s.Get(id)
	if !ok {
		return fmt.Errorf("taskservice: 任务 %q 不存在", id)
	}
	if rec.State == TaskStateRunning {
		return fmt.Errorf("taskservice: 任务 %q 正在运行, 先 Pause/Stop 再精修", id)
	}
	if s.currentRunner() == nil {
		return fmt.Errorf("taskservice: 未注入 TaskRunner, 无法精修")
	}
	rec.Refines = append(rec.Refines, TaskRefine{At: time.Now(), Feedback: feedback, FromNode: fromNode})
	if rec.Spec.Params == nil {
		rec.Spec.Params = map[string]any{}
	}
	rec.Spec.Params["feedback"] = feedback
	rec.Spec.Params["fromNode"] = fromNode
	if err := s.records.Put(rec.ID, rec); err != nil {
		return fmt.Errorf("taskservice: 精修留痕失败: %w", err)
	}
	if err := s.active.Put(activeKey(rec.Spec), rec.ID); err != nil {
		s.writeErr.Add(1)
	}
	// 同 Resume: 先同步挪出终态, 再异步开跑 (让紧随其后的 Wait 等的是这一轮)。
	rec = s.transition(rec.ID, TaskStatePending, "", "")
	s.startRun(rec)
	return nil
}

// Close 等待在跑的任务 goroutine 退出 (进程收尾用; 不取消它们)。
func (s *FileQueueTaskService) Close() { s.inFlight.Wait() }

// ---------------------------------------------------------------------------
// :7777 动作队列 (file-queue 后端的"入口")
// ---------------------------------------------------------------------------

// ActionRecord <stateDir>/.dashboard/actions/<id>.json 的形状。
//
// 前 7 个字段与 dashboard 写入侧逐字对齐 (extra_handlers.go:1077 / v13_handlers.go:403),
// 后 4 个是消费侧回填的 —— 它们的存在就是"这条动作真被读过并处理了"的证据。
type ActionRecord struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Action    string         `json:"action"`
	Target    string         `json:"target"`
	Status    string         `json:"status"`
	Requested string         `json:"requested"`
	Source    string         `json:"source,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`

	ConsumedAt string `json:"consumedAt,omitempty"`
	TaskID     string `json:"taskId,omitempty"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
}

// 动作消费后的 Status 取值。
const (
	ActionStatusPending     = "pending"     // 待消费 (dashboard 写入时的初值)
	ActionStatusAccepted    = "accepted"    // 已转成 TaskService 任务 (taskId 有效)
	ActionStatusDone        = "done"        // 由注入的 ActionExecutor 执行完毕
	ActionStatusFailed      = "failed"      // 消费时出错
	ActionStatusUnsupported = "unsupported" // 本进程没有执行该动作的能力 (写明原因, 不静默丢)
)

// inflightSuffix 认领标记后缀。认领 = 把 <id>.json 原子 rename 成 <id>.json.inflight,
// 因此两个消费者不可能同时认领同一条 (rename 是跨进程原子的, 输的那个拿到 ENOENT)。
const inflightSuffix = ".inflight"

// ActionExecutor 动作执行器: 那些不能表达为 TaskService 任务的动作 (cron.trigger /
// swarm.simulate / team.delete) 的真实执行能力。
//
// 为什么要注入而不是直接调: 真正执行需要 ProductionTeamManager / cron 调度器 /
// swarm_intel.Engine, 它们的接线在 cmd/ 与 pkg/feishu, 不在本文件职责范围。
// 注入形态让本文件只负责"取出、归档、留痕", 谁有能力谁来执行。
type ActionExecutor interface {
	Execute(ctx context.Context, act ActionRecord) (result string, err error)
}

// ActionExecutorFunc 函数形态的 ActionExecutor。
type ActionExecutorFunc func(ctx context.Context, act ActionRecord) (string, error)

func (f ActionExecutorFunc) Execute(ctx context.Context, act ActionRecord) (string, error) {
	return f(ctx, act)
}

// TeamActionExecutor 把 dashboard.Config.TeamAction 那个形状的回调
// (func(action, teamName string, payload map[string]interface{}) error) 适配成
// ActionExecutor —— 主进程已有这个回调, 接线成本为零。
func TeamActionExecutor(fn func(action, target string, payload map[string]any) error) ActionExecutor {
	return ActionExecutorFunc(func(_ context.Context, act ActionRecord) (string, error) {
		if fn == nil {
			return "", fmt.Errorf("未注入 TeamAction 回调")
		}
		if err := fn(act.Action, act.Target, act.Payload); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s.%s(%s) 已执行", act.Kind, act.Action, act.Target), nil
	})
}

// SetActionExecutor 注入/替换动作执行器。
func (s *FileQueueTaskService) SetActionExecutor(x ActionExecutor) {
	s.execMu.Lock()
	s.executor = x
	s.execMu.Unlock()
}

func (s *FileQueueTaskService) currentExecutor() ActionExecutor {
	s.execMu.RLock()
	defer s.execMu.RUnlock()
	return s.executor
}

// ActionsDir 返回动作队列目录 (空 = 未配置)。
func (s *FileQueueTaskService) ActionsDir() string { return s.actionsDir }

// ListActions 列出队列里的全部动作 (按文件名/时间序; 坏 JSON 跳过不报错, 一条坏记录
// 不该让整个队列不可读)。
func (s *FileQueueTaskService) ListActions() ([]ActionRecord, error) {
	if s.actionsDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.actionsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("taskservice: 读取动作队列失败: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	out := make([]ActionRecord, 0, len(names))
	for _, n := range names {
		act, err := readAction(filepath.Join(s.actionsDir, n))
		if err != nil {
			logging.For("taskservice").Warn("跳过损坏的动作记录", "file", n, "err", err)
			continue
		}
		out = append(out, act)
	}
	return out, nil
}

// ActionConsumeStats 一轮消费的统计。
type ActionConsumeStats struct {
	Scanned     int               `json:"scanned"`
	Accepted    int               `json:"accepted"`
	Executed    int               `json:"executed"`
	Failed      int               `json:"failed"`
	Unsupported int               `json:"unsupported"`
	Reclaimed   int               `json:"reclaimed"` // 回收的过期认领
	TaskIDs     map[string]string `json:"taskIds,omitempty"`
}

// ConsumeActions 消费一轮待处理动作。
//
// 流程: 回收过期认领 → 逐条原子认领 → 转任务 / 交执行器 → 结果回写同名 JSON。
// 认领失败 (被别的消费者抢走) 静默跳过。
func (s *FileQueueTaskService) ConsumeActions(ctx context.Context) (ActionConsumeStats, error) {
	stats := ActionConsumeStats{TaskIDs: map[string]string{}}
	if s.actionsDir == "" {
		return stats, nil
	}
	stats.Reclaimed = s.reclaimStale()

	entries, err := os.ReadDir(s.actionsDir)
	if os.IsNotExist(err) {
		return stats, nil
	}
	if err != nil {
		return stats, fmt.Errorf("taskservice: 读取动作队列失败: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // 文件名前缀含毫秒时间戳 → 近似 FIFO

	for _, n := range names {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		path := filepath.Join(s.actionsDir, n)
		act, err := readAction(path)
		if err != nil {
			continue // 坏记录已在 ListActions 侧告警; 这里不阻塞整轮
		}
		if act.Status != "" && act.Status != ActionStatusPending {
			continue // 已处理过
		}
		claimed := path + inflightSuffix
		if err := os.Rename(path, claimed); err != nil {
			continue // 被抢走或已消失
		}
		stats.Scanned++
		if act.ID == "" {
			act.ID = strings.TrimSuffix(n, ".json")
		}
		s.handleAction(ctx, act, path, claimed, &stats)
	}
	return stats, nil
}

// handleAction 处理单条已认领动作并回写结果。
func (s *FileQueueTaskService) handleAction(ctx context.Context, act ActionRecord, path, claimed string, stats *ActionConsumeStats) {
	act.ConsumedAt = time.Now().Format(time.RFC3339)
	switch act.Kind + "." + act.Action {
	case "team.run", "team.restart", "team.resume":
		// 提交成任务。IdemScopeForever + action:<id> 为键 = 这条动作只会变成一个任务,
		// 即使消费进程在回写前崩溃、动作被回收重投也不会跑两遍。
		rec, err := s.Submit(specFromAction(act), SubmitOpts{
			IdempotencyKey: "action:" + act.ID,
			IdemScope:      IdemScopeForever,
			Source:         firstNonEmptyStr(act.Source, "dashboard"),
			ActionID:       act.ID,
		})
		if err != nil {
			act.Status, act.Error = ActionStatusFailed, err.Error()
			stats.Failed++
		} else {
			act.Status, act.TaskID = ActionStatusAccepted, rec.ID
			act.Result = fmt.Sprintf("已提交任务 %s (state=%s)", rec.ID, rec.State)
			stats.Accepted++
			stats.TaskIDs[act.ID] = rec.ID
		}
	case "team.refine":
		fb, _ := act.Payload["feedback"].(string)
		if fb == "" {
			fb, _ = act.Payload["objective"].(string)
		}
		from, _ := act.Payload["fromNode"].(string)
		if from == "" {
			from, _ = act.Payload["targetStage"].(string)
		}
		if err := s.refineLatestForTeam(act.Target, fb, from); err != nil {
			act.Status, act.Error = ActionStatusFailed, err.Error()
			stats.Failed++
		} else {
			act.Status, act.Result = ActionStatusDone, "已提交精修"
			stats.Executed++
		}
	case "team.stop", "team.pause":
		want := TaskStateStopped
		if act.Action == "pause" {
			want = TaskStatePaused
		}
		id, err := s.interruptActiveForTeam(act.Target, want)
		switch {
		case err != nil:
			act.Status, act.Error = ActionStatusFailed, err.Error()
			stats.Failed++
		case id != "":
			act.Status, act.TaskID, act.Result = ActionStatusDone, id, fmt.Sprintf("任务 %s → %s", id, want)
			stats.Executed++
		default:
			// 没有本服务管理的活跃任务 → 交给注入的执行器 (团队可能是经 RunTeam 直启的)
			s.delegate(ctx, &act, stats, "无 TaskService 管理的活跃任务")
		}
	default:
		s.delegate(ctx, &act, stats, "本进程未注入 ActionExecutor")
	}

	// 回写: 先写回原名 (dashboard/前端仍按 <id>.json 读), 再删认领标记。
	if err := writeActionAtomic(path, act); err != nil {
		s.writeErr.Add(1)
		logging.For("taskservice").Warn("回写动作结果失败", "action", act.ID, "err", err)
		return // 保留 .inflight, 让 reclaimStale 后续回收重投
	}
	_ = os.Remove(claimed)
}

// delegate 把动作交给注入的执行器; 没有执行器就标 unsupported 并写明原因
// (绝不悄悄标成 done —— 那正是本次要消灭的假承诺)。
func (s *FileQueueTaskService) delegate(ctx context.Context, act *ActionRecord, stats *ActionConsumeStats, why string) {
	ex := s.currentExecutor()
	if ex == nil {
		act.Status = ActionStatusUnsupported
		act.Error = why + " (接线: FileQueueTaskService.SetActionExecutor)"
		stats.Unsupported++
		return
	}
	res, err := ex.Execute(ctx, *act)
	if err != nil {
		act.Status, act.Error = ActionStatusFailed, err.Error()
		stats.Failed++
		return
	}
	act.Status, act.Result = ActionStatusDone, res
	stats.Executed++
}

// interruptActiveForTeam 中断该团队名下的活跃任务; 返回被中断的任务 ID (无则空串)。
func (s *FileQueueTaskService) interruptActiveForTeam(team string, want TaskState) (string, error) {
	if team == "" {
		return "", fmt.Errorf("动作缺少 target (团队名)")
	}
	for _, rec := range s.List(TaskFilter{Team: team}) {
		if rec.State.Terminal() {
			continue
		}
		if err := s.interrupt(rec.ID, want); err != nil {
			return "", err
		}
		return rec.ID, nil
	}
	return "", nil
}

// refineLatestForTeam 对该团队最近一次任务提交精修; 没有历史任务则新建一个带反馈的任务。
func (s *FileQueueTaskService) refineLatestForTeam(team, feedback, fromNode string) error {
	if strings.TrimSpace(feedback) == "" {
		return fmt.Errorf("动作缺少 feedback")
	}
	list := s.List(TaskFilter{Team: team, Limit: 1})
	if len(list) == 0 {
		_, err := s.Submit(TaskSpec{
			Team: team, Objective: feedback,
			Params: map[string]any{"feedback": feedback, "fromNode": fromNode},
		}, SubmitOpts{Source: "dashboard"})
		return err
	}
	return s.Refine(list[0].ID, feedback, fromNode)
}

// specFromAction 从动作载荷提取任务规格。
func specFromAction(act ActionRecord) TaskSpec {
	str := func(k string) string {
		v, _ := act.Payload[k].(string)
		return v
	}
	spec := TaskSpec{
		Team:      act.Target,
		Workflow:  str("workflow"),
		Objective: str("objective"),
		ChatID:    str("chatId"),
		Language:  firstNonEmptyStr(str("lang"), str("language")),
	}
	if spec.Objective == "" {
		spec.Objective = str("scenario") // swarm.* 系动作用 scenario 承载目标
	}
	if len(act.Payload) > 0 {
		spec.Params = act.Payload
	}
	return spec
}

// reclaimStale 回收过期认领: 认领后超过 staleClaim 仍未落终态的动作重投为 pending。
// 消费进程在"认领后、回写前"崩溃时, 动作不会永久卡死。
func (s *FileQueueTaskService) reclaimStale() int {
	entries, err := os.ReadDir(s.actionsDir)
	if err != nil {
		return 0
	}
	n := 0
	cutoff := time.Now().Add(-s.staleClaim)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), inflightSuffix) {
			continue
		}
		p := filepath.Join(s.actionsDir, e.Name())
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		orig := strings.TrimSuffix(p, inflightSuffix)
		if _, err := os.Stat(orig); err == nil {
			_ = os.Remove(p) // 结果已回写, 认领标记是残骸
			continue
		}
		if err := os.Rename(p, orig); err == nil {
			n++
		}
	}
	return n
}

// StartActionConsumer 起一个后台消费循环 (interval<=0 用 2s)。ctx 取消即退出。
func (s *FileQueueTaskService) StartActionConsumer(ctx context.Context, interval time.Duration) {
	if s.actionsDir == "" {
		return
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	s.inFlight.Add(1)
	go func() {
		defer s.inFlight.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := s.ConsumeActions(ctx); err != nil && ctx.Err() == nil {
					logging.For("taskservice").Warn("消费动作队列失败", "err", err)
				}
			}
		}
	}()
}

func readAction(path string) (ActionRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ActionRecord{}, err
	}
	var act ActionRecord
	if err := json.Unmarshal(b, &act); err != nil {
		return ActionRecord{}, err
	}
	return act, nil
}

// writeActionAtomic 同目录临时文件 + rename, 避免读侧看到半个 JSON。
func writeActionAtomic(path string, act ActionRecord) error {
	b, err := json.MarshalIndent(act, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".act-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// TeamRunner: 把任务真正跑起来 (骑在既有 RunTeam/ResumeTeam/RefineTeam 之上)
// ---------------------------------------------------------------------------

// TeamRunner 用 ProductionTeamManager 执行任务的 TaskRunner。
//
// 一行代码都没动 teams.go: 团队不存在就 CreateTeam, 失败/停止过就 ResumeTeam
// (走检查点), 带反馈就 RefineTeam, 否则 RunTeam; 然后 WaitDone 等终态并读团队状态。
// 两套入口 (直接 RunTeam / 经 TaskService) 因此可以并存。
type TeamRunner struct {
	mgr *ProductionTeamManager
}

var (
	_ TaskRunner      = (*TeamRunner)(nil)
	_ TaskInterrupter = (*TeamRunner)(nil)
)

// NewTeamRunner 构造团队运行器。
func NewTeamRunner(mgr *ProductionTeamManager) *TeamRunner { return &TeamRunner{mgr: mgr} }

// Run 见 TaskRunner。
func (r *TeamRunner) Run(ctx context.Context, rec TaskRecord) (string, error) {
	if r == nil || r.mgr == nil {
		return "", fmt.Errorf("TeamRunner: 未注入 ProductionTeamManager")
	}
	name := strings.TrimSpace(rec.Spec.Team)
	if name == "" {
		return "", fmt.Errorf("TeamRunner: 任务缺少团队名")
	}
	objective := rec.Spec.Objective

	team := r.mgr.GetTeam(name)
	if team == nil {
		if strings.TrimSpace(rec.Spec.Workflow) == "" {
			return "", fmt.Errorf("TeamRunner: 团队 %q 不存在且未提供 workflow", name)
		}
		t, err := r.mgr.CreateTeam(name, rec.Spec.Workflow, objective, rec.Spec.ChatID)
		if err != nil {
			return "", err
		}
		if rec.Spec.Language != "" {
			t.SetLanguage(rec.Spec.Language)
		}
		team = t
	}

	feedback, _ := rec.Spec.Params["feedback"].(string)
	fromNode, _ := rec.Spec.Params["fromNode"].(string)
	prev := teamStatusOf(team)

	switch {
	case feedback != "":
		if err := r.mgr.RefineTeam(name, feedback, fromNode); err != nil {
			return "", err
		}
	case (prev == TeamStatusFailed || prev == TeamStatusStopped) && teamObjectiveOf(team) == objective:
		if err := r.mgr.ResumeTeam(name); err != nil {
			return "", err
		}
	default:
		if err := r.mgr.RunTeam(name, objective); err != nil {
			return "", err
		}
	}

	// WaitDone 会一直等到工作流 goroutine 结束; 包一层是为了同时响应 ctx 取消
	// (取消后团队仍在后台跑, 由 Interrupt→StopTeam 负责真正停下)。
	done := make(chan struct{})
	go func() {
		team.WaitDone()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	status := teamStatusOf(team)
	_, reportPath := r.mgr.GetTeamReport(name)
	out := fmt.Sprintf("团队 %s 终态=%s", name, status)
	if reportPath != "" {
		out += " 报告=" + reportPath
	}
	if isSuccessfulTeamStatus(status) {
		return out, nil
	}
	return out, fmt.Errorf("团队 %s 未成功交付 (status=%s): %s", name, status, teamErrorOf(team))
}

// Interrupt 见 TaskInterrupter: 停掉团队 (检查点保留, 之后 Resume 可续跑)。
func (r *TeamRunner) Interrupt(rec TaskRecord) error {
	if r == nil || r.mgr == nil {
		return nil
	}
	if strings.TrimSpace(rec.Spec.Team) == "" {
		return nil
	}
	return r.mgr.StopTeam(rec.Spec.Team)
}

// teamStatusOf / teamObjectiveOf / teamErrorOf 持锁读团队字段。
// team.mu 是包内私有锁, 这几个小助手让读操作不必散落在各处重复加锁。
func teamStatusOf(t *ProductionTeam) TeamStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Status
}

func teamObjectiveOf(t *ProductionTeam) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Objective
}

func teamErrorOf(t *ProductionTeam) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Error
}
