package graph

// suspend.go —— Suspended 节点态 + suspend/revive 调度 + human 人在环路
// (design/01 §4.3 RunStatus/事件枚举里的 human.requested/human.responded)。
//
// ---------------------------------------------------------------------------
// 一、为什么需要第四种状态 (三态表达不了什么)
// ---------------------------------------------------------------------------
//
// 改造前节点只有 completed/failed/skipped 三态, 于是两件事没法表达:
//
//  1. **"限流后再等等"的耐心**。被退役的 pkg/orchestrator 有一个 TaskSuspended:
//     瞬态错误额度耗尽时任务挂起、不级联、等停滞恢复唤醒。M4 把 orchestrated 迁到图
//     引擎时只能一律判 failed, 并在 orchestrated_runner.go 里留了记账:
//     "图层没有 suspended 态, 这里一律判 failed —— 少了'限流后再等等'的耐心"。
//     判 failed 的实际后果不是"少等一会": failed 会让 `fail` 条件边触发、让平台把一次
//     限流当成阶段失败去做补救 (重跑/告警), 而节点其实一点错都没有。
//  2. **人在环路**。human 节点遇上"还没人答复"时既不是成功也不是失败也不是"没走这条
//     分支"。
//
// ## 为什么不能用 skipped 顶替 (最容易踩的坑)
//
// skipped 只让**本条边**不满足, 不阻止别的入边满足; 而无条件入边对任何前驱结果都恒真。
// 所以把挂起记成 skipped 后, 下游会拿着**空产出**照跑 —— 表现为"人还没批, 后面已经
// 按空白批复往下做了"。挂起节点因此**不进** dagRun.state (不是终态), 它的下游永不就绪。
//
// ---------------------------------------------------------------------------
// 二、human: 阻塞等待 vs 挂起-恢复 (先想清再写的那一步)
// ---------------------------------------------------------------------------
//
// 方案 A "遇到 human 节点就阻塞等答复": 一个节点 goroutine + 一张并发票被占几小时;
// maxPar=4 时四个待批节点就让整张图停摆; 进程重启 (发版/OOM/机器重调度) 等待即丢,
// 而人在环路的等待期正好是最容易跨越进程生命周期的。
//
// 方案 B (本文件采用) "挂起并结束本次运行, 答复到达后 resume": 零 goroutine 占用,
// 等待期天然跨进程, 且**不需要新机制** —— 事件溯源已经把恢复定义成重放 journal:
//   - 引擎发现没答复 ⇒ 记 human.requested + 挂起 ⇒ run 以 suspended 收尾;
//   - 平台问人, 拿到答复后 RespondHuman() 往 journal 追加 human.responded;
//   - 下一次 Resume 运行时 Replay 读出答复, human 节点直接 completed, 已完成的节点
//     全部吃缓存 (零 LLM), 图从断点继续。
//
// human 节点**不经 runner**: 判据只是"journal 里有没有答复", 是确定性的, 引擎自己就能
// 算。好处不只是省一次 LLM —— 8+ 下游平台的 runner 一行都不用改就能支持人在环路,
// 平台侧只需要读 journal 的 human.requested + 调 RespondHuman 两件事。
//
// ---------------------------------------------------------------------------
// 三、in-run revive: 为什么等待要占着槽位
// ---------------------------------------------------------------------------
//
// runner 回报 ReviveAfter>0 (典型来源: 服务端的 Retry-After) 时, 引擎在**本次运行内**
// 等这么久再重新执行该节点, 等待发生在派发它的那个 goroutine 里 —— 也就是占着本层的
// 并发槽位, 与 retry 退避完全同一处、同一取舍。
//
// 这是刻意的: 限流时让整张图**减速**正是背压的目的。若把等待挪到槽位之外, 4 个被限流
// 的节点会立刻放出 4 个新节点去撞同一个限流器, 于是"退避"反而变成了放大。
// 代价是长等待会拖住并发度 —— 所以 SuspendSpec.MaxWaitSec 会把单次等待夹到上限
// (缺省 5 分钟), 更长的等待应该走跨运行挂起 (ReviveAfter=0) 而不是干等。
//
// ---------------------------------------------------------------------------
// 四、授权与额度 (与 Expand/Spawn 同一原则)
// ---------------------------------------------------------------------------
//
// 只有声明了 NodeSpec.Suspend 的节点 (以及 Kind=human) 能挂起; 未声明的节点回报挂起
// 会被 authorizeSuspend 归一为 failed。理由与"展开/派生必须显式授予"完全相同:
// 挂起会让一次运行停在半路, 而调用方 (平台/CLI/团队层) 是按"图跑完了"来解释返回值的
// —— 图上看不出有个会挂起的节点, 就等于把 run 的终点交给了 runner。
//
// 额度**不跨运行累积** (MaxRevives 是"一次运行内"的次数): resume 是新一轮运行, 与
// BudgetManager"重放时不重建台账"同一口径, 否则一个挂起过几次的图会永远无法 resume。

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// reviveState 一个挂起节点在本层调度里的唤醒进度。
type reviveState struct {
	count  int           // 本次运行内已唤醒次数 (1 起)
	wait   time.Duration // 下一次派发前要等多久 (派发时消费掉)
	reason string        // 上一次挂起的原因 (交回给节点, 见 Revival.Reason)
}

// onNodeSuspended 处理一次挂起 (在调度器 goroutine 内单线程调用)。
//
// 两条出路: 额度与 ReviveAfter 都允许 ⇒ 本次运行内再叫它一次 (清 dispatched 让 ready
// 扫描重新派发); 否则 ⇒ 终局挂起, 记进 dr.suspended, 本次运行到此为止。
//
// journal 记账**只在这里**: 只有调度器知道这次挂起会不会被唤醒, 而"是否终局"正是
// Replay 重建挂起态的关键 —— 在 execNode 里记会得到一条无法判断终局性的事件。
func (e *Engine) onNodeSuspended(rc *runCtx, dr *dagRun, id string, res NodeResult) {
	node := dr.byID[id]
	evID := dr.scope.evID(id)
	prev := dr.revive[id]
	reason := res.Err
	if strings.TrimSpace(reason) == "" {
		// 空原因会让"它在等什么"永久无解 (与终止器空信号同款处理: 补一个可查的占位)。
		reason = "unspecified"
	}

	wait, clamped := reviveWait(node, res)
	maxRevives := node.Suspend.maxRevives()
	if wait > 0 && prev.count < maxRevives {
		dr.revive[id] = reviveState{count: prev.count + 1, wait: wait, reason: reason}
		dr.dispatched[id] = false // 让 ready 扫描重新派发它
		rc.appendEv(EvNodeSuspended, evID, dr.scope.with(map[string]any{
			"reason": reason, "revive_after_ms": wait.Milliseconds(),
			"revives": prev.count + 1, "clamped": clamped, "final": false,
		}))
		rc.appendEv(EvNodeRevived, evID, dr.scope.with(map[string]any{
			"revive": prev.count + 1, "after_ms": wait.Milliseconds(),
			"clamped": clamped, "reason": reason,
		}))
		return
	}

	// 终局挂起。额度耗尽也**不转失败**: 节点没出错, 只是这次没等到 —— 转失败会让
	// fail 条件边误触发, 也会让平台把一次等待当成失败去补救。
	if wait > 0 && prev.count >= maxRevives {
		reason = fmt.Sprintf("%s (本次运行的唤醒额度已用尽: %d/%d, 留待 resume 续跑)",
			reason, prev.count, maxRevives)
	}
	final := res
	final.Err = reason
	dr.suspended[id] = final
	data := map[string]any{
		"reason": reason, "revives": prev.count, "final": true,
		"revive_after_ms": wait.Milliseconds(),
	}
	if node.Kind == NodeKindHuman {
		// human 标记 + 问题原文进事件: 平台读 journal 就能知道"要问谁什么",
		// 不必回头去解图 (runHumanNode 另发的 human.requested 同样带 prompt,
		// 两条都带是为了 Replay 的合并不依赖事件先后)。
		data["human"] = true
		data["prompt"] = node.Agent.Prompt
	}
	rc.appendEv(EvNodeSuspended, evID, dr.scope.with(data))
}

// reviveWait 本次挂起要等多久再唤醒 (0 = 不在本次运行内唤醒), 以及是否被夹到上限。
func reviveWait(node NodeSpec, res NodeResult) (time.Duration, bool) {
	if res.ReviveAfter <= 0 {
		return 0, false
	}
	if node.Kind == NodeKindHuman {
		// human 的等待以"人什么时候答"为准, 不是一段可预估的退避。在本次运行内
		// 干等等于方案 A (占槽位+进程重启即丢), 见文件头二。
		return 0, false
	}
	max := time.Duration(node.Suspend.maxWaitSec()) * time.Second
	if res.ReviveAfter > max {
		return max, true
	}
	return res.ReviveAfter, false
}

// consumeReviveWait 取出并清掉本节点待消费的等待时长 (每次挂起只等一次)。
func (dr *dagRun) consumeReviveWait(id string) time.Duration {
	rs, ok := dr.revive[id]
	if !ok || rs.wait <= 0 {
		return 0
	}
	wait := rs.wait
	rs.wait = 0
	dr.revive[id] = rs
	return wait
}

// reviveInput 组装 NodeInput.Revive: 本次运行内的唤醒进度 + journal 重建的挂起态与答复。
// 两个来源都要看 —— 前者是同一进程内的 revive, 后者是崩溃/重启后的续跑。
// 都没有则返回 nil (nil = 首次执行, 与"授权缺失表现为没有这个能力"同一手法)。
func (dr *dagRun) reviveInput(rc *runCtx, id string) *Revival {
	var (
		rv  Revival
		has bool
	)
	if rs, ok := dr.revive[id]; ok && rs.count > 0 {
		rv.Reason, rv.Count, has = rs.reason, rs.count, true
	}
	if rc.replay != nil {
		evID := dr.scope.evID(id)
		if ss, ok := rc.replay.Suspended[evID]; ok {
			has, rv.FromJournal = true, true
			if rv.Reason == "" {
				rv.Reason = ss.Reason
			}
		}
		if hr, ok := rc.replay.HumanResponses[evID]; ok {
			// 答复可能在节点挂起之前就到 (平台提前批复): 那时没有挂起记录, 但答复
			// 照样要交给节点 —— 否则它会白挂一轮。
			has = true
			rv.Response, rv.RespondedAt = hr.Response, hr.TS
		}
	}
	if !has {
		return nil
	}
	return &rv
}

// ---------------------------------------------------------------------------
// human 节点
// ---------------------------------------------------------------------------

// runHumanNode 执行一个 human 节点: 零 LLM、不经 runner。
//
// 有答复 ⇒ completed, 产出 = 答复原文 (于是下游用 {node.<id>} / PrevOutputs 就能拿到,
// 条件边也能对它做 output contains 判定); 无答复 ⇒ 记 human.requested 并挂起。
//
// 返回 (结果, 形态特有的 hook 载荷)。
func (e *Engine) runHumanNode(rc *runCtx, scope execScope, node NodeSpec, in NodeInput) (NodeResult, map[string]any) {
	if in.Revive != nil && in.Revive.Response != "" {
		return NodeResult{Status: NodeStatusCompleted, Output: in.Revive.Response},
			map[string]any{"human": "responded", "responded_at": in.Revive.RespondedAt}
	}
	// 问题原文 = 节点的 prompt 模板**原样**。引擎不做占位符替换 (与"占位符替换归
	// runner"同一分工, 而 human 节点没有 runner), 故把 objective 与上游产出一并记进
	// 事件, 让平台自己组装要问的话 —— 引擎不产提示词。
	prev := make([]string, 0, len(in.PrevOutputs))
	for k := range in.PrevOutputs {
		prev = append(prev, k)
	}
	sort.Strings(prev) // 确定性: map 迭代序不稳定, 事件载荷不该随运行漂移
	rc.appendEv(EvHumanRequested, scope.evID(node.ID), scope.with(map[string]any{
		"prompt": node.Agent.Prompt, "role": node.Agent.Role,
		"objective": rc.objective, "prev_nodes": strings.Join(prev, ","),
	}))
	reason := "等待人工答复"
	if in.Revive != nil && in.Revive.FromJournal {
		reason = "等待人工答复 (上一次运行已发起询问, 仍无答复)"
	}
	return NodeResult{Status: NodeStatusSuspended, Err: reason},
		map[string]any{"human": "requested"}
}

// RespondHuman 追加一条 human.responded 事件 —— 人在环路的**唯一**写入口。
//
// 为什么答复要走 journal 而不是给引擎一个"注入答复"的 API: 一次运行结束后进程可能已经
// 退出 (挂起的等待期正好最容易跨进程), 内存里的引擎实例根本不在了。journal 是唯一
// 进度真源, 答复进 journal 才既能被下一次 resume 读到, 又留下"谁在什么时候答了什么"。
//
// runID 必须是**发起询问的那次运行**的 ID: Replay 只重放最近一次 run 并按 RunID 过滤,
// 写错的答复会被静默丢掉 (与 InvalidateFrom 同一个坑, 故同样在参数缺失时直接报错)。
// nodeID 必须是**限定 ID** (journal 里 human.requested 事件的 node_id): 顶层节点即节点
// ID, 被引用子图里的成员形如 <subgraph节点>~sg/<成员>。
func RespondHuman(j Journal, runID, nodeID, response, by string) error {
	if j == nil {
		return fmt.Errorf("graph: RespondHuman 需要 journal")
	}
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("graph: RespondHuman 需要 runID (写错 RunID 的答复会被 Replay 静默过滤)")
	}
	if strings.TrimSpace(nodeID) == "" {
		return fmt.Errorf("graph: RespondHuman 需要 nodeID (journal 里 human.requested 的 node_id)")
	}
	// 空答复照写: "人明确说了不用改"也是一次答复。但 runHumanNode 靠"答复非空"判定
	// 完成, 故这里拦住空串 —— 否则会挂在"有答复但视为无答复"的死角里。
	if response == "" {
		return fmt.Errorf("graph: RespondHuman 的答复不得为空 (空答复无法与'尚无答复'区分)")
	}
	return j.Append(Event{
		TS: time.Now().UnixMilli(), Type: EvHumanResponded,
		RunID: runID, NodeID: nodeID,
		Data: map[string]any{"response": response, "by": by},
	})
}

// PendingHuman 从 journal 里列出**当前仍在等人**的节点 (限定 ID → 挂起态)。
// 平台的轮询入口: 有它就不必自己解 Replay 的全部字段。
func PendingHuman(j Journal) (map[string]SuspendState, string, error) {
	if j == nil {
		return nil, "", fmt.Errorf("graph: PendingHuman 需要 journal")
	}
	evs, err := j.ReadAll()
	if err != nil {
		return nil, "", err
	}
	st := Replay(evs)
	out := map[string]SuspendState{}
	for id, ss := range st.Suspended {
		if ss.Human {
			out[id] = ss
		}
	}
	// 一并回传本轮 RunID: 答复必须带着它写回 (见 RespondHuman)。
	runID := ""
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == EvRunCreated {
			runID = evs[i].RunID
			break
		}
	}
	return out, runID, nil
}
