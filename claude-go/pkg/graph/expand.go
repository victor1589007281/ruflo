package graph

// expand.go —— 动态展开 ExpandSpec (design/01 §4.2)。
//
// 用途: 统一收编三套目标分解 (GoalTree HTN / WBS ParsePlanToDAG / swarm 动态分解)
// —— planner 节点跑完后把分解结果当子图追加进**当前 run** 继续调度, 而不是各写一个
// 专用执行器。
//
// 为什么每一条边界都必须在: 子图内容来自 LLM 产出。
//  1. **授权**: 只有声明了 NodeSpec.Expand 的节点的 Expansion 才被接受。否则任何
//     runner 都能凭产出往运行图里塞节点 (等于把调度权交给模型)。
//  2. **深度** MaxDepth (默认 1): 挡住"子节点再展开子节点"的递归膨胀。
//  3. **单次条数** MaxNodes: 挡住一次返回 500 个子任务。
//  4. **运行图总量** GraphPolicies.MaxTotalNodes: 挡住"每层只加 3 个但加了 20 层"
//     的累积膨胀 —— 前两道闸单看都合法, 合起来照样打爆。
//  5. **幂等**: 一个节点在一次 run 里只展开一次; resume 时按 journal 重建
//     (不重新问 runner), 因此恢复出来的图与首跑逐字节一致。
//  6. **单调收窄**: 展开产物继承父节点约束, 只能更严不能更松 (§4.2)。
//  7. **结构合法**: 子节点必须从父节点可达、组内无环、边端点闭合。
//
// 拒绝一律是**整段拒绝**而非截断: 截断会留下引用了被丢弃节点的边, 得到一张断图。
// 每次拒绝都记 journal (graph.expand_rejected) —— 否则"模型给了子图但图没变大"
// 这件事在事后完全不可解释。

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// applyExpansion 把节点返回的子图并入运行图。在**调度器 goroutine 内**单线程调用,
// 且必须早于下一轮 ready 扫描 (晚一步父节点的原下游就先跑掉了)。
func (e *Engine) applyExpansion(ctx context.Context, rc *runCtx, dr *dagRun, id string, res NodeResult) {
	if res.Expansion == nil || len(res.Expansion.Nodes) == 0 {
		return
	}
	node, ok := dr.byID[id]
	if !ok {
		return
	}
	evID := dr.scope.evID(id)
	reject := func(format string, args ...any) {
		reason := fmt.Sprintf(format, args...)
		rc.appendEv(EvExpandRejected, evID, dr.scope.with(map[string]any{
			"reason": reason, "requested": len(res.Expansion.Nodes),
		}))
	}

	if node.Expand == nil {
		reject("节点未声明 Expand, 引擎不接受 runner 返回的子图 (展开能力必须由图显式授予)")
		return
	}
	if dr.expanded[id] {
		reject("该节点在本 run 已展开过 (journal 重建或 runner 重复返回), 幂等忽略")
		return
	}
	maxDepth := node.Expand.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultExpandDepth
	}
	depth := dr.depthOf[id] + 1
	if depth > maxDepth {
		reject("展开深度 %d 超过上限 %d", depth, maxDepth)
		return
	}
	if n := len(res.Expansion.Nodes); n > node.Expand.MaxNodes {
		reject("单次展开 %d 个节点超过上限 %d", n, node.Expand.MaxNodes)
		return
	}
	sub, err := prepareExpansion(node, *res.Expansion, dr, depth, maxDepth)
	if err != nil {
		reject("%v", err)
		return
	}
	if !rc.reserveAll(len(sub.Nodes)) {
		reject("运行图节点总数将超过上限 %d (当前 %d, 本次 +%d)",
			rc.maxTotalNodes(), rc.usedNodes(), len(sub.Nodes))
		return
	}

	dr.attach(sub.Nodes, sub.Edges, depth)
	dr.expanded[id] = true
	ids := make([]string, 0, len(sub.Nodes))
	for _, n := range sub.Nodes {
		ids = append(ids, n.ID)
	}
	rc.appendEv(EvGraphExpanded, evID, dr.scope.with(map[string]any{
		"parent": id, "depth": depth, "count": len(sub.Nodes),
		"ids": strings.Join(ids, ","),
		// subgraph 存**已命名空间化并收窄后**的最终形态: resume 直接照它重建,
		// 不需要再跑一遍 prepareExpansion (规则若将来变了, 老 run 也不会漂移)。
		"subgraph": mustJSON(sub),
	}))
	rc.hooks.Emit(ctx, HookEvent{Scope: ScopeGraph, Phase: "expanded", RunID: rc.runID, NodeID: evID,
		Payload: dr.scope.with(map[string]any{
			"graph": "expand", "parent": id, "depth": depth, "nodes": len(sub.Nodes),
		})})
}

// prepareExpansion 校验 + 命名空间化 + 单调收窄 + 与父节点下游重接线。
// 返回的 Expansion 是可直接 attach 的最终形态 (节点 ID 已是 <父>/<子>)。
func prepareExpansion(parent NodeSpec, req Expansion, dr *dagRun, depth, maxDepth int) (Expansion, error) {
	local := map[string]bool{} // 未命名空间化的子节点 ID
	for _, n := range req.Nodes {
		id := strings.TrimSpace(n.ID)
		if id == "" {
			return Expansion{}, fmt.Errorf("展开产物含空 ID 节点")
		}
		if strings.ContainsAny(id, ShardIDSep+GroupMemberIDSep) {
			return Expansion{}, fmt.Errorf("展开产物节点 ID %q 含保留分隔符 %q", id, ShardIDSep+GroupMemberIDSep)
		}
		if local[id] {
			return Expansion{}, fmt.Errorf("展开产物节点 ID %q 重复", id)
		}
		local[id] = true
	}

	// —— 节点: 命名空间化 + 形态校验 + 单调收窄 ——
	out := Expansion{}
	for _, n := range req.Nodes {
		child := narrowToParent(parent, n, depth, maxDepth)
		child.ID = expandedNodeID(parent.ID, strings.TrimSpace(n.ID))
		if _, exists := dr.byID[child.ID]; exists {
			return Expansion{}, fmt.Errorf("展开产物节点 %q 与运行图中已有节点重名", child.ID)
		}
		if err := validateNodeShape(child, false); err != nil {
			return Expansion{}, fmt.Errorf("展开产物非法: %w", err)
		}
		out.Nodes = append(out.Nodes, child)
	}

	// —— 边: From ∈ {父} ∪ 子; To ∈ 子 ——
	// 不允许 To 指向图里已有的节点: 那个节点可能已经终态 (边永远不会被求值), 或者
	// 正在跑 (它的 ready 判定已经做完了) —— 两种都是静默失效, 比报错难查得多。
	// 父节点原有下游由下面的"重接线"统一处理。
	childOut := map[string][]string{}
	childIn := map[string]int{}
	for _, ed := range req.Edges {
		from, to := strings.TrimSpace(ed.From), strings.TrimSpace(ed.To)
		if !local[to] {
			return Expansion{}, fmt.Errorf("展开产物的边 %s→%s 的终点必须是本次展开的新节点", from, to)
		}
		if from != parent.ID && !local[from] {
			return Expansion{}, fmt.Errorf("展开产物的边 %s→%s 的起点只能是父节点 %q 或本次展开的新节点", from, to, parent.ID)
		}
		if from == to {
			return Expansion{}, fmt.Errorf("展开产物的节点 %q 存在自环边", from)
		}
		if _, err := ParseCondition(ed.Condition); err != nil {
			return Expansion{}, fmt.Errorf("展开产物的边 %s→%s 条件语法错误: %w", from, to, err)
		}
		out.Edges = append(out.Edges, EdgeSpec{
			From:      expandRef(parent.ID, from, local),
			To:        expandRef(parent.ID, to, local),
			Condition: ed.Condition,
		})
		if from != parent.ID {
			childOut[from] = append(childOut[from], to)
		}
		childIn[to]++
	}

	// —— 可达性: 每个子节点都必须 (直接或间接) 挂在父节点下 ——
	// 悬空的子节点入度为 0, 会被 ready-set 当成一个新入口**立刻**执行, 完全不等父
	// 节点 —— 表现为"展开出来的任务提前跑了", 极难察觉。
	reach := map[string]bool{}
	queue := []string{}
	for _, ed := range req.Edges {
		if strings.TrimSpace(ed.From) == parent.ID {
			to := strings.TrimSpace(ed.To)
			if !reach[to] {
				reach[to] = true
				queue = append(queue, to)
			}
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, to := range childOut[cur] {
			if !reach[to] {
				reach[to] = true
				queue = append(queue, to)
			}
		}
	}
	var orphans []string
	for id := range local {
		if !reach[id] {
			orphans = append(orphans, id)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return Expansion{}, fmt.Errorf("展开产物节点 %s 未从父节点 %q 可达 (悬空节点会脱离父节点抢先执行)",
			strings.Join(orphans, ", "), parent.ID)
	}
	// —— 环: 子图内 Kahn 拓扑 ——
	deg := map[string]int{}
	for id := range local {
		deg[id] = childIn[id]
	}
	queue = nil
	for _, ed := range req.Edges {
		if strings.TrimSpace(ed.From) == parent.ID {
			// 父→子的入度不参与子图内部拓扑
			deg[strings.TrimSpace(ed.To)]--
		}
	}
	for id, d := range deg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	processed := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		processed++
		for _, to := range childOut[cur] {
			deg[to]--
			if deg[to] == 0 {
				queue = append(queue, to)
			}
		}
	}
	if processed < len(local) {
		return Expansion{}, fmt.Errorf("展开产物存在有向环")
	}

	// —— 重接线: 子图插在父节点与它原有下游之间 ——
	// 不接线的后果是父节点的下游与展开子图**并行**跑, 于是"先分解再逐项执行再汇总"
	// 里的汇总会拿不到子任务产出。
	//
	// v1 边界: 父节点若有**带条件的**出边则整段拒绝展开。条件是对**父节点结果**的
	// 判定 (score/ok/fail), 转嫁到 sink 子节点上就变成了对子节点结果的判定, 语义不
	// 等价; 而 OR-join 下补一条恒真的 sink→下游 边又会盖掉父节点的条件路由
	// (评分不过本该 skip 的下游会照跑)。宁可开图时报错。
	sinks := []string{}
	for id := range local {
		if len(childOut[id]) == 0 {
			sinks = append(sinks, id)
		}
	}
	sort.Strings(sinks) // 确定性: local 是 map, 迭代序不稳定
	for _, ed := range dr.edges {
		if ed.From != parent.ID {
			continue
		}
		if strings.TrimSpace(ed.Condition) != "" {
			return Expansion{}, fmt.Errorf("父节点 %q 有带条件的出边 (%s→%s: %q), v1 不允许在其上展开子图 (条件针对父节点结果, 无法安全转嫁给展开产物)",
				parent.ID, ed.From, ed.To, ed.Condition)
		}
		for _, s := range sinks {
			out.Edges = append(out.Edges, EdgeSpec{From: expandedNodeID(parent.ID, s), To: ed.To})
		}
	}
	return out, nil
}

// narrowToParent 单调收窄 (design/01 §4.2): 展开产物只继承或收紧父节点的约束。
// 放宽的字段被夹回父节点的值 —— 不报错而是夹紧, 因为这些字段来自 LLM 产出, 给出
// 一个更大的 max_turns 更像"没意识到有上限"而不是恶意, 夹紧后仍可执行。
// (结构性问题——空 ID/环/悬空——才整段拒绝: 那些夹不回来。)
func narrowToParent(parent, child NodeSpec, depth, maxDepth int) NodeSpec {
	out := child
	// 预算: 父节点有上限时子节点不得更长 (0=未声明, 直接继承父节点)。
	if parent.TimeoutSec > 0 && (out.TimeoutSec <= 0 || out.TimeoutSec > parent.TimeoutSec) {
		out.TimeoutSec = parent.TimeoutSec
	}
	// 重试: 不得多于父节点 (父未声明时按图级 DefaultRetry 兜, 这里只管父声明的情形)。
	if parent.Retry != nil {
		max := parent.Retry.MaxRetries
		if out.Retry == nil {
			r := *parent.Retry
			out.Retry = &r
		} else if out.Retry.MaxRetries > max {
			r := *out.Retry
			r.MaxRetries = max
			out.Retry = &r
		}
	}
	// 循环轮次: 不得多于父节点声明的上限。
	if parent.Loop != nil && out.Loop != nil && out.Loop.MaxIterations > parent.Loop.MaxIterations {
		l := *out.Loop
		l.MaxIterations = parent.Loop.MaxIterations
		out.Loop = &l
	}
	// 工具画像: 父节点显式声明过就强制继承 —— 换画像 = 换工具集 = 放宽权限,
	// 这是最不能让模型自己决定的一项。
	if parent.Agent.ToolProfile != "" {
		out.Agent.ToolProfile = parent.Agent.ToolProfile
	}
	if parent.Agent.MaxTurns > 0 && (out.Agent.MaxTurns <= 0 || out.Agent.MaxTurns > parent.Agent.MaxTurns) {
		out.Agent.MaxTurns = parent.Agent.MaxTurns
	}
	// 放置约束 (§4.9): 与工具画像同理 —— 换放置 = 换机器 = 换能力集 = 放宽,
	// 是最不能让模型自己决定的一类。父节点声明过就强制继承其 Prefer/Affinity,
	// Require 取**并集** (只能更严): 子节点少写一条硬约束不该等于把它去掉,
	// 否则一个声明了 require:["browser"] 的父节点展开出的子节点会落到没有浏览器的
	// 机器上, 而"约束只收窄"这条不变量在图上看起来还是成立的。
	if parent.Agent.Placement != nil {
		out.Agent.Placement = narrowPlacement(parent.Agent.Placement, out.Agent.Placement)
	}
	// 再展开: 只在深度还有余量时保留, 且额度不得超过父节点。
	switch {
	case depth >= maxDepth:
		out.Expand = nil
	case out.Expand != nil:
		ex := *out.Expand
		if ex.MaxDepth <= 0 || ex.MaxDepth > maxDepth {
			ex.MaxDepth = maxDepth
		}
		if parent.Expand != nil && (ex.MaxNodes <= 0 || ex.MaxNodes > parent.Expand.MaxNodes) {
			ex.MaxNodes = parent.Expand.MaxNodes
		}
		out.Expand = &ex
	}
	return out
}

// narrowPlacement 把子节点的放置约束收窄到不弱于父节点 (parent 非 nil)。
// 返回**新对象**: 父/子的声明都属于图规格, 被多个节点 goroutine 共享读, 不得就地改。
func narrowPlacement(parent, child *PlacementSpec) *PlacementSpec {
	out := PlacementSpec{Prefer: parent.Prefer, Affinity: parent.Affinity, AffinityKey: parent.AffinityKey}
	if child != nil {
		// 父没表达偏好时才让子的偏好生效 (软偏好不涉及能力, 放宽风险低)。
		if out.Prefer == "" {
			out.Prefer = child.Prefer
		}
		if out.Affinity == "" {
			out.Affinity, out.AffinityKey = child.Affinity, child.AffinityKey
		}
	}
	seen := map[string]bool{}
	add := func(reqs []string) {
		for _, r := range reqs {
			if r = strings.TrimSpace(r); r != "" && !seen[r] {
				seen[r] = true
				out.Require = append(out.Require, r)
			}
		}
	}
	add(parent.Require) // 父的硬约束先入, 顺序确定性
	if child != nil {
		add(child.Require)
	}
	return &out
}

// expandedNodeID 展开产物的命名空间化 ID: <父节点>/<子节点>。
// 命名空间化而非要求全局唯一, 是因为 ID 由 LLM 给出 —— 重名概率高, 且带上父节点
// 前缀后 journal/trace 一眼能看出这个节点是谁展开出来的。
func expandedNodeID(parent, child string) string {
	return parent + expandedNodeIDSeparator + child
}

// expandRef 把边端点翻译成运行图里的真实 ID (父节点 ID 原样, 子节点加命名空间)。
func expandRef(parent, ref string, local map[string]bool) string {
	if local[ref] {
		return expandedNodeID(parent, ref)
	}
	return ref
}

// attach 把 (已命名空间化的) 子图并入运行图。
func (dr *dagRun) attach(nodes []NodeSpec, edges []EdgeSpec, depth int) {
	for _, n := range nodes {
		if _, exists := dr.byID[n.ID]; exists {
			continue
		}
		dr.nodes = append(dr.nodes, n)
		dr.byID[n.ID] = n
		dr.depthOf[n.ID] = depth
	}
	for _, ed := range edges {
		dr.edges = append(dr.edges, ed)
		dr.addEdge(ed)
	}
}
