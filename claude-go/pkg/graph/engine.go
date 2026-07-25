package graph

// engine.go —— ready-set 并行调度器 (design/01 §4.3 执行模型)。
//
// 调度语义 (与任务规格逐条对应):
//  1. Run 先 Validate; Resume 时重放 journal, completed 节点作为缓存产出跳过执行,
//     并按 journal 重建上一轮的动态展开与 map 分片集 (不重新问 runner, 见 §4.2)。
//  2. 节点就绪 = 全部入边的 From 已达终态 (completed/failed/skipped)。就绪后逐条
//     入边判定: From=skipped ⇒ 本边不满足; From=completed/failed ⇒ 用边 Condition
//     对该前驱结果求值 (空条件恒真; `fail` 条件即"失败分支路由")。
//     join 语义 = OR-join: 至少一条入边满足才执行, 否则整节点 skipped。
//     这是 v1 决策: 对抗模式"评分不过走重做边、过了走下一步边"两条边汇入同一
//     下游时, 必然只有一条满足, AND-join 会把该下游永远饿死, 故取 OR。
//     入度 0 节点直接就绪执行。skipped 沿"skipped 前驱边不满足"规则自然级联。
//  3. 就绪节点最多 MaxParallel (默认 4) 并发, goroutine+chan 收结果;
//     每个节点 ctx 注入 trace NodeID, TimeoutSec>0 时叠加 deadline。
//  4. 节点执行单元按 Kind 分派 (execNode):
//     agent/gate  = retry 环 (外, 指数退避) 内嵌 loop 环 (内, §4.4);
//     map         = 扇出 N 个分片, **重试在分片级** (见 fanout.go);
//     reduce      = 等 map 组终态后聚合 (确定性策略零 LLM, 见 fanout.go);
//     loop-group  = 组内子图整体循环, 每轮复用同一个 scheduleDAG (见 group.go)。
//  5. PrevOutputs 只含直接前驱中 completed 的 (与该前驱的边条件是否满足无关)。
//  6. 全部节点终态后: completed=全 completed; failed=无 completed; 其余 partial。
//  7. journal 每个状态迁移都 Append; Append 出错不中断执行, 错误累积到返回值。
//  8. ctx 取消: 未跑节点不再调度, 在跑节点等待收尾, Status=failed, error 含 ctx.Err()。
//
// 并发闸有两层, 这是刻意的 (别改成单闸):
//   - 顶层节点受 running < maxPar 约束 (与改造前完全一致);
//   - 嵌套层的**叶子**节点 (map 分片 / loop-group 组内节点) 从 run 级 nestSem
//     (容量同样是 maxPar) 取票。容器节点 (map / loop-group 自身) **绝不取票** ——
//     若容器也取票, maxPar=1 时容器占着唯一的票、它自己的子任务永远拿不到票, 死锁。
//     代价是极端情况峰值 runner 并发 ≤ 2×maxPar (顶层满 + 嵌套满), 这是有意的取舍:
//     容器节点自己不烧 token, 让它占着顶层槽位换来嵌套层不被饿死。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/trace"
)

// 图运行终态 (design/01 §4.3 RunStatus 的 v1 子集)。
const (
	RunStatusCompleted = "completed"
	RunStatusPartial   = "partial"
	RunStatusFailed    = "failed"
)

// Engine 图执行引擎。零值不可用: 必须注入 Runner;
// Journal 为 nil 时退化为进程内 MemoryJournal (无持久化), Hooks 为 nil 时用 NopBus。
type Engine struct {
	Runner      NodeRunner
	Journal     Journal
	Hooks       HookBus // nil → NopBus
	MaxParallel int     // 引擎级并发上限; spec.Policies.MaxParallel 优先; 双 0 → 4

	// sleepFn 重试退避的测试注入点; nil = 真实 ctx 感知休眠。
	// 返回 false 表示 ctx 已取消, 应停止重试。
	sleepFn func(ctx context.Context, d time.Duration) bool
}

// RunOpts 一次图运行的选项。
type RunOpts struct {
	RunID     string // 空 → trace.NewRunID(spec.Name)
	Objective string
	Params    map[string]string
	Resume    bool // true: 重放 Journal, completed 节点作为缓存产出跳过执行
}

// RunResult 一次图运行的结果。
type RunResult struct {
	RunID  string
	Status string // completed|partial|failed
	// Nodes 全部达到终态的节点结果 (含 resume 缓存命中的节点与动态展开产物;
	// ctx 取消时未调度的节点不在其中)。
	Nodes map[string]NodeResult
	// Order 本次运行达到终态的节点顺序 (含 failed/skipped;
	// resume 缓存命中的节点未执行, 不在其中)。
	Order []string
	// Graph 运行图冻结快照 (design/01 §4.3 GraphRun.Graph): 含动态展开追加的节点与边。
	// 调用方要判断"这一轮到底跑的是哪张图"只能看它, 不能看传入的 spec。
	Graph GraphSpec
}

// evAppender journal 记账函数 (并发安全, Append 错误在内部累积)。
type evAppender func(typ, nodeID string, data map[string]any)

// doneMsg 节点 goroutine → 调度器的完成消息。
type doneMsg struct {
	id  string
	res NodeResult
}

// inEdge 预解析的入边 (Validate 已保证条件语法合法)。
type inEdge struct {
	from string
	cond Condition
}

// runCtx 一次图运行的共享上下文 (顶层与嵌套层共用)。
type runCtx struct {
	runID     string
	objective string
	params    map[string]string
	hooks     HookBus
	appendEv  evAppender
	policies  GraphPolicies
	maxPar    int
	replay    *RunState     // resume 的重放状态 (nil = 非 resume)
	nestSem   chan struct{} // 嵌套层叶子节点的并发票 (见文件头"并发闸有两层")

	mu         sync.Mutex
	totalNodes int // 运行图当前节点数 (含展开产物与 map 分片), 受 MaxTotalNodes 约束
}

// maxTotalNodes 运行图节点总数上限。
func (rc *runCtx) maxTotalNodes() int {
	if rc.policies.MaxTotalNodes > 0 {
		return rc.policies.MaxTotalNodes
	}
	return DefaultMaxTotalNodes
}

// reserveNodes 为新增节点 (展开产物 / map 分片) 申请总量配额,
// 返回实际获批的条数 (可能少于申请数, 0 = 已耗尽)。
// 只减不还: 一次运行里节点只增不减, 归还配额会让"总量上限"变成"同时存在上限"。
func (rc *runCtx) reserveNodes(want int) int {
	if want <= 0 {
		return 0
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	room := rc.maxTotalNodes() - rc.totalNodes
	if room <= 0 {
		return 0
	}
	if want > room {
		want = room
	}
	rc.totalNodes += want
	return want
}

// reserveAll 全有或全无地申请配额 (动态展开用)。
// 展开不能部分接纳: 少接几个节点会留下引用被丢弃节点的边, 得到一张断图。
func (rc *runCtx) reserveAll(want int) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.totalNodes+want > rc.maxTotalNodes() {
		return false
	}
	rc.totalNodes += want
	return true
}

// usedNodes 当前已占用的运行图节点配额 (仅供拒绝原因文案使用)。
func (rc *runCtx) usedNodes() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.totalNodes
}

// acquireNested 取一张嵌套层并发票; 返回的 release 必须调用。
// ok=false 表示 ctx 已取消 (调用方应放弃执行)。
func (rc *runCtx) acquireNested(ctx context.Context) (func(), bool) {
	if rc.nestSem == nil {
		return func() {}, true
	}
	select {
	case rc.nestSem <- struct{}{}:
		return func() { <-rc.nestSem }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

// execScope 一个节点的执行上下文 (它属于哪一层调度)。
type execScope struct {
	prefix string         // journal/hook 的 NodeID 前缀 ("" = 顶层)
	nested bool           // true = 嵌套层 (叶子节点要取 nestSem)
	extra  map[string]any // 附加 journal/hook 载荷 (组内: group/group_iteration)
	depth  int            // 展开深度 (顶层节点 0)
}

// evID journal/hook 里使用的节点 ID (带层前缀)。
func (s execScope) evID(nodeID string) string { return s.prefix + nodeID }

// with 复制并附加载荷 (不改原 map: 同一 scope 被多个节点 goroutine 共享)。
func (s execScope) with(kv map[string]any) map[string]any {
	out := make(map[string]any, len(s.extra)+len(kv))
	for k, v := range s.extra {
		out[k] = v
	}
	for k, v := range kv {
		out[k] = v
	}
	return out
}

// Run 执行一张图。语义见文件头注释。
func (e *Engine) Run(ctx context.Context, spec GraphSpec, opts RunOpts) (RunResult, error) {
	// —— 1. 编译期校验 ——
	if err := spec.Validate(); err != nil {
		return RunResult{}, err
	}
	if e.Runner == nil {
		return RunResult{}, errors.New("graph: Engine.Runner 未注入, 无法执行节点")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	journal := e.Journal
	if journal == nil {
		journal = NewMemoryJournal()
	}
	hooks := e.Hooks
	if hooks == nil {
		hooks = NopBus{}
	}
	// 并发上限: 图级 Policies 优先于引擎级默认, 双 0 → 4。
	maxPar := spec.Policies.MaxParallel
	if maxPar <= 0 {
		maxPar = e.MaxParallel
	}
	if maxPar <= 0 {
		maxPar = 4
	}

	runID := opts.RunID
	if runID == "" {
		runID = trace.NewRunID(spec.Name)
	}
	ctx = trace.With(ctx, trace.IDs{RunID: runID})

	// —— journal 记账: Append 出错不中断执行, 累积到返回 error (语义7) ——
	var (
		jmu   sync.Mutex
		jerrs []error
	)
	appendEv := evAppender(func(typ, nodeID string, data map[string]any) {
		jmu.Lock()
		defer jmu.Unlock()
		err := journal.Append(Event{TS: time.Now().UnixMilli(), Type: typ, RunID: runID, NodeID: nodeID, Data: data})
		if err != nil {
			jerrs = append(jerrs, fmt.Errorf("graph: journal 追加 %s(%s) 失败: %w", typ, nodeID, err))
		}
	})

	rc := &runCtx{
		runID: runID, objective: opts.Objective, params: opts.Params,
		hooks: hooks, appendEv: appendEv, policies: spec.Policies, maxPar: maxPar,
		nestSem:    make(chan struct{}, maxPar),
		totalNodes: len(spec.Nodes),
	}

	dr := newDagRun(spec.Nodes, spec.Edges, execScope{})

	// —— resume: 重放 journal (语义1, §4.3) ——
	//  a) completed 节点直接作为缓存产出;
	//  b) **上一轮的动态展开按 journal 重建**——不重新问 runner, 否则恢复出来的图
	//     与首跑不同 (展开内容来自 LLM 产出), 事件溯源就失效了。
	if opts.Resume {
		evs, err := journal.ReadAll()
		if err != nil {
			return RunResult{}, fmt.Errorf("graph: resume 读取 journal 失败: %w", err)
		}
		rc.replay = Replay(evs)
		for _, rec := range rc.replay.Expansions {
			if _, ok := dr.byID[rec.Parent]; !ok {
				continue // 展开记录的父节点已不在图里 (工作流改过): 整段丢弃
			}
			dr.attach(rec.Sub.Nodes, rec.Sub.Edges, rec.Depth)
			// 标记已展开: 父节点若在本轮重跑 (它上一轮 failed) 并再次返回子图,
			// 必须被幂等闸拦下 —— 否则同一父节点会有两代子图并存, 恢复出的图与
			// 首跑不同, 事件溯源失效。
			dr.expanded[rec.Parent] = true
			rc.reserveNodes(len(rec.Sub.Nodes)) // 重建的节点同样占总量配额
		}
		for id, r := range rc.replay.Completed {
			if _, ok := dr.byID[id]; ok {
				dr.state[id] = r
			}
		}
	}

	appendEv(EvRunCreated, "", map[string]any{"graph": spec.Name, "objective": opts.Objective, "resume": opts.Resume})
	hooks.Emit(ctx, HookEvent{Scope: ScopeGraph, Phase: "pre", RunID: runID,
		Payload: map[string]any{"graph": spec.Name, "nodes": len(spec.Nodes), "resume": opts.Resume}})

	// —— 2/3. ready-set 调度 ——
	cancelled := e.scheduleDAG(ctx, rc, dr)

	// —— 6. 汇总终态 (按运行图而非声明图: 展开产物也算) ——
	nCompleted, nOther := 0, 0
	for _, n := range dr.nodes {
		r, ok := dr.state[n.ID]
		switch {
		case !ok: // ctx 取消导致未调度
			nOther++
		case r.Status == NodeStatusCompleted:
			nCompleted++
		default:
			nOther++
		}
	}
	status := RunStatusPartial
	switch {
	case cancelled: // 语义8: 取消一律 failed
		status = RunStatusFailed
	case nOther == 0:
		status = RunStatusCompleted
	case nCompleted == 0:
		status = RunStatusFailed
	}
	appendEv(EvRunFinished, "", map[string]any{"status": status})
	hooks.Emit(ctx, HookEvent{Scope: ScopeGraph, Phase: "post", RunID: runID,
		Payload: map[string]any{"graph": spec.Name, "status": status, "completed": nCompleted, "other": nOther}})

	jmu.Lock()
	retErr := errors.Join(jerrs...)
	jmu.Unlock()
	if cancelled {
		retErr = errors.Join(ctx.Err(), retErr)
	}
	final := spec
	final.Nodes, final.Edges = dr.nodes, dr.edges
	return RunResult{RunID: runID, Status: status, Nodes: dr.state, Order: dr.order, Graph: final}, retErr
}

// ---------------------------------------------------------------------------
// dagRun —— 一层 DAG 的可变调度状态 (顶层运行 / loop-group 的一轮 / 都用它)
// ---------------------------------------------------------------------------

type dagRun struct {
	scope execScope

	nodes []NodeSpec // 声明序 (动态展开会追加)
	edges []EdgeSpec // 同上, 仅为回传运行图快照
	byID  map[string]NodeSpec
	preds map[string][]inEdge
	succs map[string][]string

	state      map[string]NodeResult
	dispatched map[string]bool
	order      []string
	depthOf    map[string]int  // 节点 → 展开深度
	expanded   map[string]bool // 已展开过的节点 (幂等闸)

	// 组循环给全部成员的公共输入 (仅 loop-group 的组内 scope 非零)。
	feedback  string
	groupIter int
	// basePrev 组**外**上游的产出: 组内成员照样要看得到 (组是个容器, 不是隔离舱),
	// 否则组内第一个节点拿不到进组前的交接内容。组内同名前驱覆盖它。
	basePrev map[string]string
}

func newDagRun(nodes []NodeSpec, edges []EdgeSpec, scope execScope) *dagRun {
	dr := &dagRun{
		scope: scope,
		nodes: append([]NodeSpec(nil), nodes...),
		edges: append([]EdgeSpec(nil), edges...),
		byID:  make(map[string]NodeSpec, len(nodes)),
		preds: map[string][]inEdge{}, succs: map[string][]string{},
		state: map[string]NodeResult{}, dispatched: map[string]bool{},
		depthOf: map[string]int{}, expanded: map[string]bool{},
	}
	for _, n := range dr.nodes {
		dr.byID[n.ID] = n
		dr.depthOf[n.ID] = scope.depth
	}
	for _, ed := range dr.edges {
		dr.addEdge(ed)
	}
	return dr
}

func (dr *dagRun) addEdge(ed EdgeSpec) {
	c, _ := ParseCondition(ed.Condition) // Validate 已保证语法
	dr.preds[ed.To] = append(dr.preds[ed.To], inEdge{from: ed.From, cond: c})
	dr.succs[ed.From] = append(dr.succs[ed.From], ed.To)
}

// prevOutputs 直接前驱中 completed 的产出 (语义5)。
func (dr *dagRun) prevOutputs(id string) map[string]string {
	outs := make(map[string]string, len(dr.basePrev)+len(dr.preds[id]))
	for k, v := range dr.basePrev {
		outs[k] = v
	}
	for _, in := range dr.preds[id] {
		if r, ok := dr.state[in.from]; ok && r.Status == NodeStatusCompleted {
			outs[in.from] = r.Output
		}
	}
	return outs
}

// scheduleDAG ready-set 调度一层 DAG; 返回是否因 ctx 取消而提前收尾。
// 顶层与 loop-group 的每一轮共用它 —— 组内"保持 DAG 语义"就是靠复用同一个调度器,
// 而不是另写一套顺序执行 (那必然与顶层的 OR-join/skipped 级联语义漂移)。
func (e *Engine) scheduleDAG(ctx context.Context, rc *runCtx, dr *dagRun) bool {
	results := make(chan doneMsg)
	running := 0
	cancelled := false

	for {
		if ctx.Err() != nil {
			cancelled = true // 语义8: 不再调度新节点
		}
		if !cancelled {
			// 反复扫描直到无新迁移: skipped 级联在此闭环内完成, 不必等在跑节点。
			for progress := true; progress; {
				progress = false
				for i := 0; i < len(dr.nodes); i++ { // 索引遍历: 动态展开会在循环中追加节点
					n := dr.nodes[i]
					id := n.ID
					if dr.dispatched[id] {
						continue
					}
					if _, done := dr.state[id]; done {
						continue
					}
					ready := true
					for _, in := range dr.preds[id] {
						if _, ok := dr.state[in.from]; !ok {
							ready = false
							break
						}
					}
					if !ready {
						continue
					}
					// OR-join 判定 (语义2)。
					satisfied := len(dr.preds[id]) == 0 // 入度 0 直接就绪执行
					for _, in := range dr.preds[id] {
						pr := dr.state[in.from]
						if pr.Status == NodeStatusSkipped {
							continue // skipped 前驱: 本边不满足
						}
						if in.cond.Eval(pr) {
							satisfied = true
							break
						}
					}
					if !satisfied {
						const reason = "or-join: 无满足的入边"
						dr.state[id] = NodeResult{Status: NodeStatusSkipped, Err: reason}
						dr.order = append(dr.order, id)
						rc.appendEv(EvNodeSkipped, dr.scope.evID(id), dr.scope.with(map[string]any{"reason": reason}))
						progress = true
						continue
					}
					if !dr.scope.nested && running >= rc.maxPar {
						continue // 顶层并发已满, 等一个完成后重扫 (嵌套层受 nestSem 约束)
					}
					dr.dispatched[id] = true
					running++
					rc.appendEv(EvNodeScheduled, dr.scope.evID(id), dr.scope.extra)
					in := NodeInput{
						Objective: rc.objective, Params: rc.params,
						PrevOutputs:    dr.prevOutputs(id),
						Feedback:       dr.feedback,
						GroupIteration: dr.groupIter,
					}
					if n.Kind == NodeKindReduce {
						in.Shards = dr.gatherShards(n)
					}
					// 嵌套层的**叶子**节点要取 run 级并发票; 容器节点 (map/loop-group)
					// 不取 —— 见文件头"并发闸有两层"的死锁论证。
					in.nested = dr.scope.nested && n.Kind != NodeKindMap && n.Kind != NodeKindLoopGroup
					node := n
					go func() {
						results <- doneMsg{id: node.ID, res: e.execNode(ctx, rc, dr.scope, node, in)}
					}()
					progress = true
				}
			}
		}
		if running == 0 {
			// 无在跑节点且无可派发: 全部终态, 或已取消 (剩余节点放弃调度)。
			break
		}
		msg := <-results
		running--
		dr.state[msg.id] = msg.res
		dr.order = append(dr.order, msg.id)
		// 动态展开在**调度器 goroutine 内**并入 (单线程, 无需加锁), 且必须在
		// 下一轮 ready 扫描之前完成 —— 父节点的原下游要等新子图跑完才就绪,
		// 晚一步并入就会让下游先跑掉 (见 expand.go 的 attach 重接线)。
		e.applyExpansion(ctx, rc, dr, msg.id, msg.res)
	}
	return cancelled
}

// execNode 单节点执行单元 (在独立 goroutine 中运行):
// hook pre (deny→skipped) → 按 Kind 分派执行体 → journal 终态 + hook post/failure。
func (e *Engine) execNode(ctx context.Context, rc *runCtx, scope execScope, node NodeSpec, in NodeInput) NodeResult {
	evID := scope.evID(node.ID)
	// node pre hook: deny 则该节点 skipped 且记 journal node.skipped, reason 入 Data (§4.5)。
	// Payload 带节点静态信息: 桥接方 (如 pkg/agent 的 teamGraphHooks) 据此立刻落一条
	// "运行中" 占位记录, 无需自己再查 spec。
	dec := rc.hooks.Emit(ctx, HookEvent{Scope: ScopeNode, Phase: "pre", RunID: rc.runID, NodeID: evID,
		Payload: scope.with(map[string]any{"kind": string(node.Kind), "role": node.Agent.Role})})
	if dec.Action == HookDeny {
		rc.appendEv(EvNodeSkipped, evID, scope.with(map[string]any{"reason": dec.Reason, "denied_by": "hook"}))
		return NodeResult{Status: NodeStatusSkipped, Err: dec.Reason}
	}

	// 语义3: trace NodeID 注入 + 节点级超时。
	nctx := trace.With(ctx, trace.IDs{NodeID: evID})
	if node.TimeoutSec > 0 {
		var cancel context.CancelFunc
		nctx, cancel = context.WithTimeout(nctx, time.Duration(node.TimeoutSec)*time.Second)
		defer cancel()
	}

	rc.appendEv(EvNodeStarted, evID, scope.extra)
	started := time.Now()

	var (
		res      NodeResult
		attempts int            // 实际尝试次数 (1 起)
		iters    int            // 全部尝试累计的轮数 (无 Loop 时 = attempts)
		extra    map[string]any // 形态特有的终态载荷 (分片数/组轮次…)
	)
	switch node.Kind {
	case NodeKindMap:
		// 重试在**分片级** (见 fanout.go runMapNode): 组级重试会把已成功的分片
		// 连带重跑一遍, 对 LLM 就是白烧 N 倍 token, 且分片产出不幂等。
		res, iters, extra = e.runMapNode(nctx, rc, scope, node, in)
		attempts = 1
	case NodeKindLoopGroup:
		res, iters, extra = e.runLoopGroup(nctx, rc, scope, node, in)
		attempts = 1
	case NodeKindReduce:
		// 确定性聚合策略 (concat/longest) 由引擎直接算出: 零 LLM、零重试必要,
		// 但仍走完整节点生命周期 (本函数的 hook/journal/预算), 符合 §4.1 对
		// Deterministic 的要求。Strategy=runner 时落回常规 retry 环。
		nOK := 0
		for _, s := range in.Shards {
			if s.Status == NodeStatusCompleted {
				nOK++
			}
		}
		extra = map[string]any{"shards": len(in.Shards), "shards_ok": nOK}
		if det, handled := reduceResult(node, in); handled {
			res, attempts, iters = det, 1, 1
		} else {
			res, attempts, iters = e.runWithRetry(nctx, rc, scope, node, in)
		}
	default:
		res, attempts, iters = e.runWithRetry(nctx, rc, scope, node, in)
	}

	// termPayload 终态 hook 载荷。hook 是唯一"节点刚结束"的同步时机, 桥接方要靠它
	// 落业务侧的阶段记录与指标 (耗时/重试/产出长度), 所以这里给全: 否则桥接方只能
	// 回头去解 journal, 既慢又拿不到与本次运行一一对应的耗时。
	termPayload := func(r NodeResult) map[string]any {
		p := map[string]any{
			"kind":        string(node.Kind),
			"role":        node.Agent.Role,
			"status":      r.Status,
			"score":       r.Score,
			"output":      r.Output,
			"output_len":  len(r.Output),
			"error":       r.Err,
			"attempts":    attempts,
			"iterations":  iters,
			"duration_ms": time.Since(started).Milliseconds(),
		}
		for k, v := range extra {
			p[k] = v
		}
		return scope.with(p)
	}

	switch res.Status {
	case NodeStatusCompleted:
		data := scope.with(map[string]any{"output": res.Output, "score": res.Score})
		if len(res.Shards) > 0 {
			// 分片结果随 node.completed 落盘: resume 后 map 是缓存命中不再执行的,
			// 下游 reduce 若要重跑, 分片结果只能从这里恢复 (见 journal.go Replay)。
			data["shards"] = mustJSON(res.Shards)
		}
		rc.appendEv(EvNodeCompleted, evID, data)
		rc.hooks.Emit(nctx, HookEvent{Scope: ScopeNode, Phase: "post", RunID: rc.runID, NodeID: evID,
			Payload: termPayload(res)})
	case NodeStatusFailed:
		rc.appendEv(EvNodeFailed, evID, scope.with(map[string]any{"error": res.Err}))
		rc.hooks.Emit(nctx, HookEvent{Scope: ScopeNode, Phase: "failure", RunID: rc.runID, NodeID: evID,
			Payload: termPayload(res)})
	case NodeStatusSkipped: // runner 主动跳过 / 新形态的"未走的分支"(空集合的 map 等)
		rc.appendEv(EvNodeSkipped, evID, scope.with(map[string]any{"reason": res.Err, "by": "runner"}))
	default: // 防御性归一: runner 返回未知状态按 failed 处理
		res = NodeResult{Status: NodeStatusFailed, Output: res.Output,
			Err: fmt.Sprintf("runner 返回未知状态 %q", res.Status)}
		rc.appendEv(EvNodeFailed, evID, scope.with(map[string]any{"error": res.Err}))
		rc.hooks.Emit(nctx, HookEvent{Scope: ScopeNode, Phase: "failure", RunID: rc.runID, NodeID: evID,
			Payload: termPayload(res)})
	}
	return res
}

// runWithRetry 叶子节点的执行体: retry 环 (外, 指数退避 BackoffSec*2^attempt,
// ctx.Done 感知) 内嵌 loop 环 (内, §4.4)。retry 只对 Status=="failed" (语义4)。
// 返回 (结果, 尝试次数, 累计轮数)。
func (e *Engine) runWithRetry(ctx context.Context, rc *runCtx, scope execScope, node NodeSpec, in NodeInput) (NodeResult, int, int) {
	// 节点未声明 Retry 时回退图级 DefaultRetry。
	retry := node.Retry
	if retry == nil {
		retry = rc.policies.DefaultRetry
	}
	maxRetries, backoff := 0, 2 // BackoffSec 默认 2 秒
	if retry != nil {
		maxRetries = retry.MaxRetries
		if retry.BackoffSec > 0 {
			backoff = retry.BackoffSec
		}
	}
	var (
		res      NodeResult
		attempts int
		iters    int
	)
	for attempt := 0; ; attempt++ {
		var loopRuns int
		res, loopRuns = e.runLoop(ctx, rc, scope, node, in)
		attempts, iters = attempt+1, iters+loopRuns
		if res.Status != NodeStatusFailed || attempt >= maxRetries {
			break
		}
		rc.appendEv(EvNodeRetried, scope.evID(node.ID), scope.with(map[string]any{"attempt": attempt + 1, "error": res.Err}))
		// 指数退避 BackoffSec*2^attempt 秒, ctx.Done 感知。
		if !e.sleep(ctx, time.Duration(backoff<<attempt)*time.Second) {
			break // ctx 已取消, 停止重试, 保留最后一次失败结果
		}
	}
	return res, attempts, iters
}

// runLoop loop 环 (design/01 §4.4): 先跑一次; 若声明 Loop, 在 Until 满足或达到
// MaxIterations 硬上限前, 以 Feedback 模板 ({prev_output} 替换为上一轮 Output)
// 回灌重跑, 每轮记 journal loop.iteration。
// 第二个返回值 = 本次实际执行的轮数 (≥1), 供 execNode 汇总进 hook 载荷。
// v1 决策: 循环不区分轮次成败 (失败轮的产出照样回灌), 最终仍 failed 时由外层
// retry 环接管; Until 为空 = 无退出条件, 只受 MaxIterations 约束
// (注意: 空串对 ParseCondition 是恒真, 但在 Until 语境下语义是"不设条件", 故特判)。
//
// 注意 in.Feedback **不清零**: 组循环 (loop-group) 会经 NodeInput.Feedback 给全部
// 成员下发本轮回灌, 清零会把组的回灌吞掉; 节点级 Loop 的第 2 轮起自然覆盖它。
func (e *Engine) runLoop(ctx context.Context, rc *runCtx, scope execScope, node NodeSpec, in NodeInput) (NodeResult, int) {
	in.Iteration = 0
	res := e.callRunner(ctx, rc, node, in)
	runs := 1
	if node.Loop == nil {
		return res, runs
	}
	hasUntil := node.Loop.Until != ""
	until, _ := ParseCondition(node.Loop.Until) // Validate 已保证语法
	for iter := 1; iter < node.Loop.MaxIterations; iter++ {
		if hasUntil && until.Eval(res) {
			break // 退出条件满足
		}
		if ctx.Err() != nil {
			break // 取消/超时感知
		}
		rc.appendEv(EvLoopIteration, scope.evID(node.ID), scope.with(map[string]any{"iteration": iter, "prev_score": res.Score}))
		in.Iteration = iter
		in.Feedback = replacePrevOutput(node.Loop.Feedback, res.Output)
		res = e.callRunner(ctx, rc, node, in)
		runs++
	}
	return res, runs
}

// callRunner 真正调 runner 的唯一出口。嵌套层的叶子节点在此取并发票
// (容器节点不走这里, 故不会自己占票饿死子任务, 见文件头)。
func (e *Engine) callRunner(ctx context.Context, rc *runCtx, node NodeSpec, in NodeInput) NodeResult {
	if in.nested {
		release, ok := rc.acquireNested(ctx)
		if !ok {
			return NodeResult{Status: NodeStatusFailed, Err: "graph: 等待嵌套层并发票时 ctx 取消"}
		}
		defer release()
	}
	return e.Runner.RunNode(ctx, node, in)
}

// replacePrevOutput 替换 Feedback 模板中的 {prev_output} 占位。
func replacePrevOutput(tmpl, prevOutput string) string {
	if tmpl == "" {
		return ""
	}
	return strings.ReplaceAll(tmpl, "{prev_output}", prevOutput)
}

// sleep ctx 感知休眠; 返回 false 表示 ctx 已取消 (调用方应停止重试)。
func (e *Engine) sleep(ctx context.Context, d time.Duration) bool {
	if e.sleepFn != nil {
		return e.sleepFn(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
