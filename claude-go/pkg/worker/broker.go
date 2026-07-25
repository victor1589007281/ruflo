package worker

// broker.go —— 控制面侧: 把租约式任务队列包装成 agent.AgentRuntime 的远程实现。
//
// 数据流 (一次远程节点执行):
//
//	控制面                                          worker
//	  Pick(Placement) → remoteRuntime
//	  Execute → subscribe(taskID) → Enqueue ──────▶ PullWithCaps (钉住 worker:<name>)
//	                                                  ↓ 本地 runtime 真跑
//	  ◀── POST /cluster/node-events (started/output) ─┘  (顺带取回 cancel 指令)
//	  ◀── POST /cluster/complete|fail ────────────────┘
//	  队列状态 = 终态真源 → NodeEventDone / NodeEventFailed → 关闭事件通道
//
// 三条 fail-closed 规则 (这个包存在的意义就是不许静默成功):
//
//  1. 终态只认队列。worker 上报的 done/failed 事件**不会**被转发成终态 —— 事件
//     走 best-effort HTTP, 若拿它当终态, 丢包/伪造都会变成"成功"。
//  2. 结果载荷不可解析 = 失败。worker Complete 了但 Result 不是 StageResult 形状,
//     说明协议不匹配, 此时返回空产出并报成功等于把 bug 洗成"模型没话说"。
//  3. worker 掉线 = 失败。任务钉住的 worker 从注册表消失且超过宽限期仍无人拉取,
//     直接失败, 不让调用方等到节点超时 (等超时也会失败, 但归因会指向 LLM)。
//     已被拉走却租约过期的任务, 由队列的 reap 判失败 (MaxAttempts=1, 见下)。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/cluster"
	"github.com/anthropic/claude-go/pkg/trace"
)

// 默认参数。
const (
	// DefaultMaxAttempts stage 任务的队列级尝试次数上限。
	//
	// **故意是 1**: design/01 §4.3 的"重试单层化"是这个仓踩过的坑 —— 图层
	// (NodeSpec.Retry) 与内层各自重试会把 1 次失败放大成 (1+6)×(1+3+限流20)。
	// 队列若再补一层重派, 就是第三层。所以 worker 崩溃时队列只判失败, 由**图层**
	// 的 RetryPolicy 重跑该节点; 那时死掉的 worker 已被租约剔除, Pick 自然换机器,
	// design/02 R3 验收的"节点 worker 崩溃自动重派"照样成立, 且只有一层重试。
	DefaultMaxAttempts = 1
	// DefaultPollInterval 控制面轮询队列终态的间隔。
	DefaultPollInterval = 300 * time.Millisecond
	// DefaultDeadWorkerGrace 钉住的 worker 从注册表消失后, 判"无人可拉"的宽限期。
	// 给足一次心跳抖动/重启的时间, 否则 worker 滚动升级期间的任务会被误杀。
	DefaultDeadWorkerGrace = 60 * time.Second
	// DefaultSubBuffer 单个任务的事件订阅缓冲。满了丢增量 (只是观测流)。
	DefaultSubBuffer = 64
	// cancelTombstoneTTL 取消标记的保留时长 (防 map 无界增长)。
	cancelTombstoneTTL = 30 * time.Minute
)

// BrokerOptions Broker 构造参数。
type BrokerOptions struct {
	// Queue 必填: 控制面任务队列 (与 cluster.Mount 挂的同一实例)。
	Queue *cluster.Queue
	// Registry 可空: worker 注册表。为空时 Sync/SyncLoop 不可用, 且失去
	// "钉住的 worker 已掉线"检查 (退化为等调用方 ctx 超时)。
	Registry *cluster.Registry
	// MaxAttempts 0 → DefaultMaxAttempts。
	MaxAttempts int
	// PollInterval 0 → DefaultPollInterval。
	PollInterval time.Duration
	// DeadWorkerGrace 0 → DefaultDeadWorkerGrace。
	DeadWorkerGrace time.Duration
	// Logf 可空 → 丢弃。
	Logf func(format string, args ...any)
}

// Broker 控制面侧的远程运行时工厂 + 事件汇聚点。
type Broker struct {
	q               *cluster.Queue
	reg             *cluster.Registry
	maxAttempts     int
	poll            time.Duration
	deadWorkerGrace time.Duration
	logf            func(string, ...any)

	mu        sync.Mutex
	subs      map[string]*subscription  // taskID → 订阅
	cancels   map[string]time.Time      // taskID → 取消时刻 (墓碑)
	runtimes  map[string]*remoteRuntime // runtime 名 → 实例 (跨 Sync 复用, 保住在途表)
	capsSig   map[string]string         // runtime 名 → 上次注册的能力签名
	lastAlive map[string]time.Time      // worker 名 → 最近一次在注册表里见到的时刻
}

type subscription struct {
	ch      chan agent.NodeEvent
	cancel  chan struct{} // 关闭 = 控制面要求取消 (唤醒 watcher)
	closeMu sync.Once
	dropped int
}

// NewBroker 构造 Broker。Queue 为空返回错误 —— 没有队列的 broker 只会静默无所作为。
func NewBroker(opt BrokerOptions) (*Broker, error) {
	if opt.Queue == nil {
		return nil, fmt.Errorf("worker: BrokerOptions.Queue 不能为空")
	}
	if opt.MaxAttempts <= 0 {
		opt.MaxAttempts = DefaultMaxAttempts
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = DefaultPollInterval
	}
	if opt.DeadWorkerGrace <= 0 {
		opt.DeadWorkerGrace = DefaultDeadWorkerGrace
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	return &Broker{
		q: opt.Queue, reg: opt.Registry,
		maxAttempts: opt.MaxAttempts, poll: opt.PollInterval,
		deadWorkerGrace: opt.DeadWorkerGrace, logf: opt.Logf,
		subs: map[string]*subscription{}, cancels: map[string]time.Time{},
		runtimes: map[string]*remoteRuntime{}, capsSig: map[string]string{},
		lastAlive: map[string]time.Time{},
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 事件回传端点
// ─────────────────────────────────────────────────────────────────────────────

// Mount 把事件回传端点挂到 mux (与 cluster.Mount 同一个 mux)。
func (b *Broker) Mount(mux *http.ServeMux) { b.MountAt(mux, DefaultEventsPath) }

// MountAt 指定路径挂载 (测试/自定义前缀用)。
func (b *Broker) MountAt(mux *http.ServeMux, path string) {
	mux.HandleFunc(path, b.handleEvents)
}

func (b *Broker) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	var batch EventBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(batch.TaskID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id 不能为空"})
		return
	}
	ack := b.ingest(batch)
	writeJSON(w, http.StatusOK, ack)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ingest 消费一批事件, 返回应答 (含取消指令)。导出给测试与进程内直连使用。
func (b *Broker) ingest(batch EventBatch) EventAck {
	b.mu.Lock()
	sub := b.subs[batch.TaskID]
	_, cancelled := b.cancels[batch.TaskID]
	b.pruneCancelsLocked()
	b.mu.Unlock()

	ack := EventAck{OK: true, Cancel: cancelled}
	if sub == nil {
		ack.Unknown = true
		return ack
	}
	for _, ev := range batch.Events {
		// 只转发观测事件; done/failed 由队列状态决定 (fail-closed 规则 1)。
		if ev.Kind != agent.NodeEventStarted && ev.Kind != agent.NodeEventOutput {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// 缓冲满: 丢增量并计数。不阻塞 HTTP handler —— 阻塞会让 worker 卡在
			// 上报上, 把观测通道的背压传染给执行。
			b.mu.Lock()
			sub.dropped++
			ack.Dropped = sub.dropped
			b.mu.Unlock()
		}
	}
	return ack
}

func (b *Broker) pruneCancelsLocked() {
	if len(b.cancels) == 0 {
		return
	}
	cut := time.Now().Add(-cancelTombstoneTTL)
	for id, ts := range b.cancels {
		if ts.Before(cut) {
			delete(b.cancels, id)
		}
	}
}

func (b *Broker) subscribe(taskID string) *subscription {
	sub := &subscription{ch: make(chan agent.NodeEvent, DefaultSubBuffer), cancel: make(chan struct{})}
	b.mu.Lock()
	b.subs[taskID] = sub
	b.mu.Unlock()
	return sub
}

func (b *Broker) unsubscribe(taskID string) {
	b.mu.Lock()
	delete(b.subs, taskID)
	b.mu.Unlock()
}

// markCancel 记录取消并唤醒对应 watcher。
func (b *Broker) markCancel(taskID string) {
	b.mu.Lock()
	b.cancels[taskID] = time.Now()
	sub := b.subs[taskID]
	b.mu.Unlock()
	if sub != nil {
		sub.closeMu.Do(func() { close(sub.cancel) })
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// worker 注册表 → RuntimeRegistry 同步
// ─────────────────────────────────────────────────────────────────────────────

// RuntimeName 由 worker 名推导 runtime 名。
//
// 唯一的加工: worker 名以 "local-" 打头时加 "remote-" 前缀。因为
// `agent.isLocalRuntimeName` 按 "local-" 前缀判定本地, 一个叫 local-foo 的远程
// worker 会被 `Prefer:"local"` 误当成本地 runtime 加分 —— 那是把跨机执行伪装成
// 本机执行, 必须挡掉。
func RuntimeName(worker string) string {
	w := strings.TrimSpace(worker)
	if strings.HasPrefix(w, "local-") {
		return "remote-" + w
	}
	return w
}

// Sync 把注册表里存活的 worker 同步进 agent.RuntimeRegistry, 返回本轮存活的
// runtime 名 (升序)。
//
//   - 首次见到 / 能力变化 → Register(lease)
//   - 已注册且能力未变 → Heartbeat (续租)
//   - 掉线 → **什么都不做**: 交给 RuntimeRegistry 既有的租约过期剔除
//     (memRuntimeRegistry.alive), 这正是那套机制存在的理由。
//
// lease 应显著大于 Sync 间隔 (建议 ≥3 倍), 否则一次抖动就把 worker 摘掉。
func (b *Broker) Sync(reg agent.RuntimeRegistry, lease time.Duration) ([]string, error) {
	if reg == nil {
		return nil, fmt.Errorf("worker: Sync 需要 RuntimeRegistry")
	}
	if b.reg == nil {
		return nil, fmt.Errorf("worker: Broker 未配置 cluster.Registry, 无法同步")
	}
	alive, err := b.reg.Alive()
	if err != nil {
		return nil, fmt.Errorf("worker: 读取 worker 注册表失败: %w", err)
	}
	now := time.Now()
	var names []string
	for _, w := range alive {
		if strings.TrimSpace(w.Name) == "" || !acceptsStage(w.Kinds) {
			continue
		}
		name := RuntimeName(w.Name)
		caps := RuntimeCapsFromLabels(w.Caps)
		sig := capsSignature(w.Caps)

		b.mu.Lock()
		b.lastAlive[w.Name] = now
		rt, known := b.runtimes[name]
		if !known {
			rt = &remoteRuntime{b: b, name: name, worker: w.Name, caps: caps, inflight: map[string]string{}}
			b.runtimes[name] = rt
		} else {
			rt.setCaps(caps)
		}
		changed := b.capsSig[name] != sig
		b.capsSig[name] = sig
		b.mu.Unlock()

		if !known || changed {
			reg.Register(rt, lease)
			b.logf("[broker] runtime %s 已注册 (worker=%s caps=%v lease=%s)", name, w.Name, w.Caps, lease)
		} else {
			reg.Heartbeat(name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// SyncLoop 周期同步 (阻塞直到 ctx 取消)。首轮立即执行。
func (b *Broker) SyncLoop(ctx context.Context, interval time.Duration, reg agent.RuntimeRegistry, lease time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := b.Sync(reg, lease); err != nil {
			b.logf("[broker] 同步 worker 注册表失败: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Runtime 直接取一个远程 runtime (不经注册表同步; 测试与显式接线用)。
func (b *Broker) Runtime(workerName string, caps agent.RuntimeCaps) agent.AgentRuntime {
	name := RuntimeName(workerName)
	b.mu.Lock()
	defer b.mu.Unlock()
	if rt, ok := b.runtimes[name]; ok {
		rt.setCaps(caps)
		return rt
	}
	rt := &remoteRuntime{b: b, name: name, worker: workerName, caps: caps, inflight: map[string]string{}}
	b.runtimes[name] = rt
	return rt
}

func acceptsStage(kinds []string) bool {
	if len(kinds) == 0 {
		return true // 未声明 = 接受全部 (与 Queue.PullFor 的 kinds 语义一致)
	}
	for _, k := range kinds {
		if strings.EqualFold(strings.TrimSpace(k), TaskKindStage) {
			return true
		}
	}
	return false
}

func capsSignature(caps []string) string {
	s := append([]string(nil), caps...)
	sort.Strings(s)
	return strings.Join(s, ",")
}

// workerAbsentSince 返回该 worker 从注册表消失的起始时刻; 仍存活返回零值。
func (b *Broker) workerAbsentSince(name string) time.Time {
	if b.reg == nil {
		return time.Time{}
	}
	alive, err := b.reg.Alive()
	if err != nil {
		return time.Time{} // 读不出来时不做掉线判定 (宁可等超时, 不误杀)
	}
	for _, w := range alive {
		if w.Name == name {
			b.mu.Lock()
			b.lastAlive[name] = time.Now()
			b.mu.Unlock()
			return time.Time{}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ts, ok := b.lastAlive[name]
	if !ok {
		// 从未见过 (例如 Runtime() 手工构造的 runtime): 以首次询问时刻起算。
		ts = time.Now()
		b.lastAlive[name] = ts
	}
	return ts
}

// ─────────────────────────────────────────────────────────────────────────────
// remoteRuntime —— AgentRuntime 的远程实现
// ─────────────────────────────────────────────────────────────────────────────

type remoteRuntime struct {
	b      *Broker
	name   string // runtime 名 (放置策略里的身份)
	worker string // cluster worker 名 (钉住标签用)

	mu       sync.Mutex
	caps     agent.RuntimeCaps
	inflight map[string]string // "runID/nodeID" → taskID
}

func (r *remoteRuntime) Name() string { return r.name }

func (r *remoteRuntime) Capabilities() agent.RuntimeCaps {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.caps
}

func (r *remoteRuntime) setCaps(c agent.RuntimeCaps) {
	r.mu.Lock()
	r.caps = c
	r.mu.Unlock()
}

// Execute 入队一个 stage 任务并返回流式事件通道。
//
// 通道一定会被关闭 (与 localRuntime 同一契约, 否则调用方 range 不结束)。
func (r *remoteRuntime) Execute(ctx context.Context, task agent.RuntimeNodeTask) (<-chan agent.NodeEvent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	require := MergeCaps(PlacementCaps(task.Placement), []string{WorkerCap(r.worker)})
	payload, err := EncodeStageTask(StageTask{
		RunID: task.RunID, NodeID: task.NodeID, Role: task.Role,
		SystemPrompt: task.SystemPrompt, UserPrompt: task.UserPrompt,
		Workspace: task.Workspace, ToolProfile: task.ToolProfile,
		MaxTurns: task.MaxTurns, Require: require,
	})
	if err != nil {
		return nil, err
	}

	// 先订阅再入队: 反序会漏掉 worker 抢在订阅之前上报的 started 事件。
	// 任务 ID 由控制面预生成 (Queue.Enqueue 接受调用方指定的 ID)。
	taskID := trace.NewID("task")
	sub := r.b.subscribe(taskID)

	if _, err := r.b.q.Enqueue(cluster.Task{
		ID: taskID, Kind: TaskKindStage,
		RunID: task.RunID, NodeID: task.NodeID,
		Payload: payload, RequireCaps: require,
		MaxAttempts: r.b.maxAttempts,
	}); err != nil {
		r.b.unsubscribe(taskID)
		// 入队失败必须**同步**报错: 调用方据此立刻失败/换 runtime,
		// 而不是拿到一个永远不出事件的通道。
		return nil, fmt.Errorf("%s: 入队失败: %w", r.name, err)
	}

	key := inflightKey(task.RunID, task.NodeID)
	r.mu.Lock()
	r.inflight[key] = taskID
	r.mu.Unlock()

	out := make(chan agent.NodeEvent, 16)
	go r.watch(ctx, taskID, key, task, sub, out)
	return out, nil
}

// Cancel 取消一个在途节点。
//
// 跨进程取消是**异步**的: 标记取消 → worker 下一次事件上报/keepalive 时取回
// 指令 → 取消本地执行。控制面侧不等它, 立刻判失败并关闭事件通道 (与 localRuntime
// 的"cancel 后 runner 报错"语义对齐)。取消之后即使 worker 又 Complete 了,
// 控制面也不会把它翻回成功 —— 一次执行只允许有一个终态。
func (r *remoteRuntime) Cancel(runID, nodeID string) error {
	key := inflightKey(runID, nodeID)
	r.mu.Lock()
	taskID, ok := r.inflight[key]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s: 无进行中的节点 %s/%s", r.name, runID, nodeID)
	}
	r.b.markCancel(taskID)
	return nil
}

func inflightKey(runID, nodeID string) string { return runID + "/" + nodeID }

// watch 把队列状态 + 事件流归约成一条事件流, 结束时关闭 out。
func (r *remoteRuntime) watch(ctx context.Context, taskID, key string, task agent.RuntimeNodeTask,
	sub *subscription, out chan agent.NodeEvent) {

	defer close(out)
	defer r.b.unsubscribe(taskID)
	defer func() {
		r.mu.Lock()
		delete(r.inflight, key)
		r.mu.Unlock()
	}()

	// 增量事件: 非阻塞发送, 满则丢 (观测流)。
	fwd := func(ev agent.NodeEvent) {
		select {
		case out <- ev:
		default:
		}
	}
	// 终态: **必须**送达, 否则 CollectRuntimeOutput 会把"没有终态"读成空产出+nil
	// 错误 = 静默成功。只在调用方连 ctx 都取消了才放弃。
	// 送终态前先把已到达的增量清空 —— worker 是"先冲刷事件再回报终态", 若这里直接
	// 收摊, 最后一批增量就永远送不出去了。
	term := func(ev agent.NodeEvent) {
		for {
			select {
			case pending := <-sub.ch:
				fwd(pending)
				continue
			default:
			}
			break
		}
		select {
		case out <- ev:
		case <-ctx.Done():
		}
	}
	failed := func(format string, args ...any) {
		term(agent.NodeEvent{Kind: agent.NodeEventFailed, RunID: task.RunID, NodeID: task.NodeID,
			Err: fmt.Sprintf(format, args...)})
	}

	t := time.NewTicker(r.b.poll)
	defer t.Stop()
	pendingSince := time.Now()

	for {
		select {
		case <-ctx.Done():
			// 调用方取消 (节点超时 / 图运行被取消): 通知 worker 别白跑, 并诚实报失败。
			r.b.markCancel(taskID)
			failed("%s: 已取消 (%v)", r.name, ctx.Err())
			return

		case <-sub.cancel:
			failed("%s: 已被取消 (Cancel)", r.name)
			return

		case ev := <-sub.ch:
			fwd(ev)

		case <-t.C:
			ct, err := r.b.taskState(taskID)
			if err != nil {
				failed("%s: 查询任务 %s 状态失败: %v", r.name, taskID, err)
				return
			}
			switch ct.Status {
			case "completed":
				res, perr := decodeStageResult(ct.Result)
				if perr != nil {
					// fail-closed 规则 2: 协议不匹配不许洗成空产出的成功。
					failed("%s: 任务 %s 回报完成但结果不可解析: %v", r.name, taskID, perr)
					return
				}
				term(agent.NodeEvent{Kind: agent.NodeEventDone, RunID: task.RunID, NodeID: task.NodeID,
					Output: res.Output})
				return
			case "failed":
				msg := strings.TrimSpace(ct.Err)
				if msg == "" {
					msg = "worker 回报失败但未给出原因"
				}
				failed("%s: %s", r.name, msg)
				return
			case "pending":
				// fail-closed 规则 3: 钉住的 worker 掉线且宽限期已过 → 无人可拉。
				// 先看等待时长再查注册表: 查注册表要扫一遍 KV 桶, 不该每个轮询周期都做。
				if time.Since(pendingSince) <= r.b.deadWorkerGrace {
					break
				}
				if since := r.b.workerAbsentSince(r.worker); !since.IsZero() &&
					time.Since(maxTime(since, pendingSince)) > r.b.deadWorkerGrace {
					failed("%s: worker %s 已掉线, 任务 %s 无人可拉 (等待 %s)",
						r.name, r.worker, taskID, time.Since(pendingSince).Round(time.Second))
					return
				}
			case "leased":
				pendingSince = time.Now() // 已被拉走: 掉线判定交给队列租约
			}
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// taskState 查询任务状态, 并在租约已过期时**逼队列做一次回收**。
//
// 为什么需要这一步: Queue.reapLocked 只在 Pull/List 里被调用, Get 不回收。一个
// worker 拉走任务后进程被 kill, 若集群里恰好没有其他 worker 在 Pull, 该任务会
// 永远停在 leased —— 控制面就永远等不到终态 (静默挂死)。这里主动 List 一次触发
// 回收, 再读一遍状态。
func (b *Broker) taskState(taskID string) (*cluster.Task, error) {
	ct, err := b.q.Get(taskID)
	if err != nil {
		return nil, err
	}
	if ct.Status == "leased" && ct.LeaseUntil > 0 && ct.LeaseUntil < time.Now().UnixMilli() {
		if _, lerr := b.q.List(); lerr != nil {
			return nil, lerr
		}
		return b.q.Get(taskID)
	}
	return ct, nil
}

func decodeStageResult(raw json.RawMessage) (StageResult, error) {
	var res StageResult
	if len(raw) == 0 {
		return res, errors.New("结果载荷为空")
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return res, err
	}
	// 协议标记不符 = 对端不是本包的 worker (例如改造前的回显桩)。
	// 不认它, 否则空产出会被当成"模型没话说"的成功。
	if res.Proto != ResultProto {
		return res, fmt.Errorf("结果协议标记不符: 期望 %q, 实得 %q", ResultProto, res.Proto)
	}
	return res, nil
}
