package graph

// group.go —— loop-group: 一组节点整体循环 (design/01 §4.4 组级循环)。
//
// 与节点级 Loop 的职责边界 (谁管谁), 一句话: **粒度不同, 互不嵌套**。
//
//	节点级 Loop (engine.go runLoop)
//	  管一个节点自己的反复: 同一 agent 生成→自评→改写。
//	  Until 对**该节点自己**的结果求值; journal 记 loop.iteration。
//	  在 map 节点上声明时, 作用于**每个分片内部** (见 fanout.go)。
//
//	loop-group (本文件)
//	  管一组节点的反复: "生成→评审"两节点整轮重来 (对抗模式的标准表达)。
//	  组内保持完整 DAG 语义 —— 每轮都复用同一个 scheduleDAG, 于是 OR-join、
//	  条件边、skipped 级联、节点级 Loop/Retry 在组内与顶层逐条一致
//	  (若另写一套顺序执行, 两套语义必然漂移)。
//	  Until 对**组产出节点** (GroupPolicy.ResultFrom) 的结果求值;
//	  journal 记 loop.group.iteration, 组内成员事件的 NodeID 形如
//	  <组节点>#it<轮次>/<成员>, 于是每轮都能被独立归因。
//
//	两者不得并存于同一节点 (Validate 强制): 轮次相乘后预算无法推理。
//	组内也不得再嵌套 loop-group, 同理。
//
// Replay 幂等: 每轮结束记一条 loop.group.iteration; resume 时 Replay 汇出
// GroupIterState{Done, Output, Score} —— 已完成的轮次**不重跑**, 直接从第 Done 轮
// 继续, 并用记下的上一轮产出做 Feedback 回灌。若上一轮的结果已满足 Until, 组直接
// 收尾, 一轮 LLM 都不烧。

import (
	"context"
	"fmt"
	"strconv"
)

// runLoopGroup 执行 loop-group 节点。
// 返回 (组结果 = 最后一轮 ResultFrom 节点的结果, 实际执行轮数, 形态特有 hook 载荷)。
func (e *Engine) runLoopGroup(ctx context.Context, rc *runCtx, scope execScope, node NodeSpec, in NodeInput) (NodeResult, int, map[string]any) {
	g := node.Group
	if g == nil || len(g.Nodes) == 0 { // Validate 已拦, 防御性兜底
		return NodeResult{Status: NodeStatusFailed, Err: "loop-group 节点缺少组内子图"}, 1, nil
	}
	resultFrom := g.ResultFrom
	if resultFrom == "" {
		if sinks := groupSinks(*g); len(sinks) == 1 {
			resultFrom = sinks[0]
		} else { // Validate 已拦
			return NodeResult{Status: NodeStatusFailed, Err: "loop-group: 无法确定组产出节点 (group.result_from 未声明且组内出度 0 节点不唯一)"}, 1, nil
		}
	}

	hasUntil := g.Loop.Until != ""
	until, _ := ParseCondition(g.Loop.Until) // Validate 已保证语法

	// 可插拔终止器 (与 Until 互斥, Validate 强制)。解析失败时 fail-closed:
	// 只跑一轮就收尾, 并记账 —— 见 engine.go runLoopWithTerminator 同款论证
	// (没有判据还继续跑, 等于用"跑满轮次"冒充判据)。
	var term LoopTerminator
	if g.Loop.Terminator != nil {
		t, err := resolveTerminator(g.Loop.Terminator)
		if err != nil || t == nil {
			rc.appendEv(EvGroupTerminated, scope.evID(node.ID), scope.with(map[string]any{
				"terminator": g.Loop.Terminator.Name, "signal": "terminator_unavailable",
				"detail": errText(err), "stop": true, "result_from": resultFrom,
			}))
			return NodeResult{Status: NodeStatusFailed,
					Err: fmt.Sprintf("loop-group: 终止器不可用 (%s), 拒绝按无判据跑满轮次", errText(err))}, 0,
				map[string]any{"group_iterations": 0, "result_from": resultFrom, "term_signal": "terminator_unavailable"}
		}
		term = t
	}

	// —— resume: 跳过已完成的轮次 ——
	start := 0
	var last NodeResult
	if rc.replay != nil {
		if gs, ok := rc.replay.GroupIters[scope.evID(node.ID)]; ok && gs.Done > 0 {
			start = gs.Done
			last = NodeResult{Status: orDefault(gs.Status, NodeStatusCompleted), Output: gs.Output, Score: gs.Score}
			if gs.Terminated {
				// 上一次运行里终止器已判定该停 (含 best-of-N 回滚后的最终产出)。
				// 新进程里的终止器没有历史, 跨轮判据 (收敛/退化) 全部作废, 继续跑
				// 只会把早停判定推翻 —— 直接拿 journal 里的最终产出收尾, 零 LLM。
				return last, 0, map[string]any{"group_iterations": 0, "resumed_from": start,
					"result_from": resultFrom, "term_signal": gs.TermSignal, "terminated": true}
			}
		}
	}
	if start >= g.Loop.MaxIterations {
		// 上一轮已把轮次用尽: 直接拿 journal 里的最后一轮结果收尾, 不再执行。
		return last, 0, map[string]any{"group_iterations": 0, "resumed_from": start, "result_from": resultFrom}
	}
	if start > 0 && hasUntil && until.Eval(last) {
		return last, 0, map[string]any{"group_iterations": 0, "resumed_from": start, "result_from": resultFrom, "until_met": true}
	}

	iters := 0
	untilMet := false
	// 终止器路径的跨轮状态: rounds 给判据看历史, pendingSuffix 是策略转换要追加给
	// **下一轮全体成员**的回灌 (组的回灌在下一轮开头统一下发, 故必须跨轮带过去)。
	var (
		rounds        []LoopRound
		pendingSuffix string
		termDec       TerminateDecision
		termStopped   bool
	)
	for iter := start; iter < g.Loop.MaxIterations; iter++ {
		if ctx.Err() != nil {
			break // 取消/超时感知: 保留已完成轮次的结果
		}
		// 每轮一张全新的 dagRun: 组内节点必须真的重跑, 不能吃上一轮的终态表
		// (那就退化成"只跑一轮"了)。轮次进 NodeID 前缀, 于是 journal 里每轮独立。
		iterScope := execScope{
			prefix: scope.prefix + node.ID + GroupIterIDInfix + strconv.Itoa(iter) + GroupMemberIDSep,
			nested: true, // 组内叶子节点取 nestSem
			depth:  scope.depth,
			extra:  scope.with(map[string]any{"group": node.ID, "group_iteration": iter}),
		}
		dr := newDagRun(g.Nodes, g.Edges, iterScope)
		dr.groupIter = iter
		dr.basePrev = in.PrevOutputs // 组外上游产出对组内成员同样可见
		if iter > start || start > 0 {
			// 第 2 轮起 (含 resume 续跑的第一轮) 回灌上一轮产出给**全部**成员。
			dr.feedback = replacePrevOutput(g.Loop.Feedback, last.Output)
			// 策略转换的追加文案: 终止器上一轮说"换个方向", 靠它真正改变本轮输入。
			if pendingSuffix != "" {
				dr.feedback += pendingSuffix
				pendingSuffix = ""
			}
		}
		// 组内节点数也算运行图节点 (§4.2 有界): 只记账不设闸 —— 组的轮次上限已由
		// MaxIterations 约束, 再在这里拒绝会让"最后一轮跑不完"这种半截状态出现。
		rc.reserveNodes(len(g.Nodes))

		cancelled := e.scheduleDAG(ctx, rc, dr)
		iters++

		cur, ok := dr.state[resultFrom]
		if !ok {
			cur = NodeResult{Status: NodeStatusFailed,
				Err: fmt.Sprintf("loop-group: 第 %d 轮组产出节点 %q 未达终态 (调度被取消?)", iter, resultFrom)}
		}
		last = cur
		groupIterData := map[string]any{
			"iteration": iter, "status": cur.Status, "output": cur.Output, "score": cur.Score,
			"result_from": resultFrom,
		}
		if term != nil {
			// 终止器信号落在**本轮**的 iteration 事件上 (与节点级把信号记在下一轮
			// 的 loop.iteration 上不同): 组的下一轮不再单独发 iteration 事件,
			// 信号无处可挂, 只能与产生它的那一轮记在一起。
			rounds = append(rounds, LoopRound{Iteration: iter, Result: cur})
			termDec = term.Decide(LoopContext{
				NodeID: node.ID, Scope: TerminatorScopeGroup,
				MaxIterations: g.Loop.MaxIterations, Rounds: rounds,
			})
			groupIterData["signal"] = termDec.Signal
			if termDec.Detail != "" {
				groupIterData["detail"] = termDec.Detail
			}
			pendingSuffix = termDec.FeedbackSuffix
		}
		rc.appendEv(EvGroupIteration, scope.evID(node.ID), scope.with(groupIterData))
		if cancelled {
			break
		}
		if term != nil && termDec.Stop {
			var note map[string]any
			last, note = applyLoopRollback(rounds, termDec, g.Loop.Terminator.AllowRollback)
			data := termEvData(g.Loop.Terminator, termDec, rounds, note)
			// 组的最终产出必须进终止事件: resume 靠它收尾 (回滚后它与最后一轮的
			// loop.group.iteration 记的产出不是同一份)。
			data["result_from"] = resultFrom
			data["output"] = last.Output
			data["status"] = last.Status
			data["score"] = last.Score
			rc.appendEv(EvGroupTerminated, scope.evID(node.ID), scope.with(data))
			termStopped = true
			break
		}
		if hasUntil && until.Eval(cur) {
			untilMet = true
			break // 退出条件满足
		}
	}

	extra := map[string]any{
		"group_iterations": iters,
		"result_from":      resultFrom,
		"until_met":        untilMet,
	}
	if termStopped {
		extra["term_signal"] = termDec.Signal
		extra["terminated"] = true
		if termDec.Rollback {
			extra["term_best_round"] = termDec.BestRound
		}
	}
	if start > 0 {
		extra["resumed_from"] = start
	}
	// 组结果 = 最后一轮组产出节点的结果, 状态 1:1 映射。
	// ResultFrom 被条件边路由掉 (skipped) 时组也是 skipped —— "未走的分支不算失败"
	// 的口径要在组这一层同样成立, 否则一条组内条件边就能把整个团队判死。
	return last, iters, extra
}
