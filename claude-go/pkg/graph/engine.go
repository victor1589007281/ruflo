package graph

// engine.go —— ready-set 并行调度器 (design/01 §4.3 执行模型)。
//
// 调度语义 (v1, 与任务规格逐条对应):
//  1. Run 先 Validate; Resume 时重放 journal, completed 节点作为缓存产出跳过执行。
//  2. 节点就绪 = 全部入边的 From 已达终态 (completed/failed/skipped)。就绪后逐条
//     入边判定: From=skipped ⇒ 本边不满足; From=completed/failed ⇒ 用边 Condition
//     对该前驱结果求值 (空条件恒真; `fail` 条件即"失败分支路由")。
//     join 语义 = OR-join: 至少一条入边满足才执行, 否则整节点 skipped。
//     这是 v1 决策: 对抗模式"评分不过走重做边、过了走下一步边"两条边汇入同一
//     下游时, 必然只有一条满足, AND-join 会把该下游永远饿死, 故取 OR。
//     入度 0 节点直接就绪执行。skipped 沿"skipped 前驱边不满足"规则自然级联。
//  3. 就绪节点最多 MaxParallel (默认 4) 并发, goroutine+chan 收结果;
//     每个节点 ctx 注入 trace NodeID, TimeoutSec>0 时叠加 deadline。
//  4. 节点执行单元 = retry 环 (外, 指数退避 BackoffSec*2^attempt, ctx.Done 感知)
//     内嵌 loop 环 (内, design/01 §4.4)。retry 只对 Status=="failed"。
//  5. PrevOutputs 只含直接前驱中 completed 的 (与该前驱的边条件是否满足无关)。
//  6. 全部节点终态后: completed=全 completed; failed=无 completed; 其余 partial。
//  7. journal 每个状态迁移都 Append; Append 出错不中断执行, 错误累积到返回值。
//  8. ctx 取消: 未跑节点不再调度, 在跑节点等待收尾, Status=failed, error 含 ctx.Err()。

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
	// Nodes 全部达到终态的节点结果 (含 resume 缓存命中的节点;
	// ctx 取消时未调度的节点不在其中)。
	Nodes map[string]NodeResult
	// Order 本次运行达到终态的节点顺序 (含 failed/skipped;
	// resume 缓存命中的节点未执行, 不在其中)。
	Order []string
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

	nodesByID := make(map[string]NodeSpec, len(spec.Nodes))
	for _, n := range spec.Nodes {
		nodesByID[n.ID] = n
	}
	preds := map[string][]inEdge{}
	for _, ed := range spec.Edges {
		c, _ := ParseCondition(ed.Condition) // Validate 已保证语法
		preds[ed.To] = append(preds[ed.To], inEdge{from: ed.From, cond: c})
	}

	// —— resume: 重放 journal, completed 节点直接作为缓存产出 (语义1, §4.3) ——
	state := map[string]NodeResult{} // 终态表, 仅调度器 goroutine 读写
	if opts.Resume {
		evs, err := journal.ReadAll()
		if err != nil {
			return RunResult{}, fmt.Errorf("graph: resume 读取 journal 失败: %w", err)
		}
		for id, r := range Replay(evs).Completed {
			if _, ok := nodesByID[id]; ok {
				state[id] = r
			}
		}
	}

	appendEv(EvRunCreated, "", map[string]any{"graph": spec.Name, "objective": opts.Objective, "resume": opts.Resume})
	hooks.Emit(ctx, HookEvent{Scope: ScopeGraph, Phase: "pre", RunID: runID})

	// —— 2/3. ready-set 调度主循环 ——
	results := make(chan doneMsg)
	dispatched := map[string]bool{} // 已派发执行 (在跑) 的节点
	running := 0
	var order []string
	cancelled := false

	// prevOutputs 直接前驱中 completed 的产出 (语义5)。
	prevOutputs := func(id string) map[string]string {
		outs := map[string]string{}
		for _, in := range preds[id] {
			if r, ok := state[in.from]; ok && r.Status == NodeStatusCompleted {
				outs[in.from] = r.Output
			}
		}
		return outs
	}

	for {
		if ctx.Err() != nil {
			cancelled = true // 语义8: 不再调度新节点
		}
		if !cancelled {
			// 反复扫描直到无新迁移: skipped 级联在此闭环内完成, 不必等在跑节点。
			for progress := true; progress; {
				progress = false
				for _, n := range spec.Nodes {
					id := n.ID
					if dispatched[id] {
						continue
					}
					if _, done := state[id]; done {
						continue
					}
					ready := true
					for _, in := range preds[id] {
						if _, ok := state[in.from]; !ok {
							ready = false
							break
						}
					}
					if !ready {
						continue
					}
					// OR-join 判定 (语义2)。
					satisfied := len(preds[id]) == 0 // 入度 0 直接就绪执行
					for _, in := range preds[id] {
						pr := state[in.from]
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
						state[id] = NodeResult{Status: NodeStatusSkipped, Err: reason}
						order = append(order, id)
						appendEv(EvNodeSkipped, id, map[string]any{"reason": reason})
						progress = true
						continue
					}
					if running >= maxPar {
						continue // 并发已满, 等一个完成后重扫
					}
					dispatched[id] = true
					running++
					appendEv(EvNodeScheduled, id, nil)
					in := NodeInput{Objective: opts.Objective, Params: opts.Params, PrevOutputs: prevOutputs(id)}
					node := n
					go func() {
						results <- doneMsg{id: node.ID, res: e.execNode(ctx, spec, node, in, hooks, appendEv, runID)}
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
		state[msg.id] = msg.res
		order = append(order, msg.id)
	}

	// —— 6. 汇总终态 ——
	nCompleted, nOther := 0, 0
	for _, n := range spec.Nodes {
		r, ok := state[n.ID]
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
	hooks.Emit(ctx, HookEvent{Scope: ScopeGraph, Phase: "post", RunID: runID, Payload: map[string]any{"status": status}})

	jmu.Lock()
	retErr := errors.Join(jerrs...)
	jmu.Unlock()
	if cancelled {
		retErr = errors.Join(ctx.Err(), retErr)
	}
	return RunResult{RunID: runID, Status: status, Nodes: state, Order: order}, retErr
}

// execNode 单节点执行单元 (在独立 goroutine 中运行):
// hook pre (deny→skipped) → retry 环内嵌 loop 环 → journal 终态 + hook post/failure。
func (e *Engine) execNode(ctx context.Context, spec GraphSpec, node NodeSpec, in NodeInput, hooks HookBus, appendEv evAppender, runID string) NodeResult {
	// node pre hook: deny 则该节点 skipped 且记 journal node.skipped, reason 入 Data (§4.5)。
	dec := hooks.Emit(ctx, HookEvent{Scope: ScopeNode, Phase: "pre", RunID: runID, NodeID: node.ID})
	if dec.Action == HookDeny {
		appendEv(EvNodeSkipped, node.ID, map[string]any{"reason": dec.Reason, "denied_by": "hook"})
		return NodeResult{Status: NodeStatusSkipped, Err: dec.Reason}
	}

	// 语义3: trace NodeID 注入 + 节点级超时。
	nctx := trace.With(ctx, trace.IDs{NodeID: node.ID})
	if node.TimeoutSec > 0 {
		var cancel context.CancelFunc
		nctx, cancel = context.WithTimeout(nctx, time.Duration(node.TimeoutSec)*time.Second)
		defer cancel()
	}

	appendEv(EvNodeStarted, node.ID, nil)

	// 语义4: retry 环 (外)。节点未声明 Retry 时回退图级 DefaultRetry。
	retry := node.Retry
	if retry == nil {
		retry = spec.Policies.DefaultRetry
	}
	maxRetries, backoff := 0, 2 // BackoffSec 默认 2 秒
	if retry != nil {
		maxRetries = retry.MaxRetries
		if retry.BackoffSec > 0 {
			backoff = retry.BackoffSec
		}
	}
	var res NodeResult
	for attempt := 0; ; attempt++ {
		res = e.runLoop(nctx, node, in, appendEv)
		if res.Status != NodeStatusFailed || attempt >= maxRetries {
			break
		}
		appendEv(EvNodeRetried, node.ID, map[string]any{"attempt": attempt + 1, "error": res.Err})
		// 指数退避 BackoffSec*2^attempt 秒, ctx.Done 感知。
		if !e.sleep(nctx, time.Duration(backoff<<attempt)*time.Second) {
			break // ctx 已取消, 停止重试, 保留最后一次失败结果
		}
	}

	switch res.Status {
	case NodeStatusCompleted:
		appendEv(EvNodeCompleted, node.ID, map[string]any{"output": res.Output, "score": res.Score})
		hooks.Emit(nctx, HookEvent{Scope: ScopeNode, Phase: "post", RunID: runID, NodeID: node.ID,
			Payload: map[string]any{"score": res.Score}})
	case NodeStatusFailed:
		appendEv(EvNodeFailed, node.ID, map[string]any{"error": res.Err})
		hooks.Emit(nctx, HookEvent{Scope: ScopeNode, Phase: "failure", RunID: runID, NodeID: node.ID,
			Payload: map[string]any{"error": res.Err}})
	case NodeStatusSkipped: // runner 主动跳过
		appendEv(EvNodeSkipped, node.ID, map[string]any{"reason": res.Err, "by": "runner"})
	default: // 防御性归一: runner 返回未知状态按 failed 处理
		res = NodeResult{Status: NodeStatusFailed, Output: res.Output,
			Err: fmt.Sprintf("runner 返回未知状态 %q", res.Status)}
		appendEv(EvNodeFailed, node.ID, map[string]any{"error": res.Err})
		hooks.Emit(nctx, HookEvent{Scope: ScopeNode, Phase: "failure", RunID: runID, NodeID: node.ID,
			Payload: map[string]any{"error": res.Err}})
	}
	return res
}

// runLoop loop 环 (design/01 §4.4): 先跑一次; 若声明 Loop, 在 Until 满足或达到
// MaxIterations 硬上限前, 以 Feedback 模板 ({prev_output} 替换为上一轮 Output)
// 回灌重跑, 每轮记 journal loop.iteration。
// v1 决策: 循环不区分轮次成败 (失败轮的产出照样回灌), 最终仍 failed 时由外层
// retry 环接管; Until 为空 = 无退出条件, 只受 MaxIterations 约束
// (注意: 空串对 ParseCondition 是恒真, 但在 Until 语境下语义是"不设条件", 故特判)。
func (e *Engine) runLoop(ctx context.Context, node NodeSpec, in NodeInput, appendEv evAppender) NodeResult {
	in.Iteration = 0
	in.Feedback = ""
	res := e.Runner.RunNode(ctx, node, in)
	if node.Loop == nil {
		return res
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
		appendEv(EvLoopIteration, node.ID, map[string]any{"iteration": iter, "prev_score": res.Score})
		in.Iteration = iter
		in.Feedback = replacePrevOutput(node.Loop.Feedback, res.Output)
		res = e.Runner.RunNode(ctx, node, in)
	}
	return res
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
