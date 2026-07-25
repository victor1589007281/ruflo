package graph

// validate.go —— GraphSpec 编译期校验 (design/01 §4.6):
// 在图进入引擎前拦截结构性地雷 (死锁/环/悬空边/无界循环/坏条件),
// 吸收 docs/dynamic-workflow-design.md 的"拆地雷"结论。

import (
	"errors"
	"fmt"
	"strings"
)

// Validate 校验图结构, 返回首个发现的错误 (中文信息, 含节点/边定位)。
// 检查项: 图非空; 节点 ID 非空且唯一; Kind 合法 (agent|gate|map|reduce|loop-group,
// router/subgraph/human 显式报错); 各形态的策略字段完备 (map 切分/reduce 来源/
// 组内子图递归校验/Expand 边界); Loop.MaxIterations>0; 边端点存在; 无自环;
// 存在入度 0 入口; 全部节点从入口可达; 无有向环 (Kahn 拓扑);
// 边 Condition 与 Loop.Until 语法合法。
func (g GraphSpec) Validate() error { return g.validate(false) }

// validate 真正的校验体。insideGroup=true 时用于 loop-group 的组内子图递归校验
// (差异: 组内不允许再嵌套 loop-group, 见 GroupPolicy 注释的"轮次相乘"论证)。
func (g GraphSpec) validate(insideGroup bool) error {
	if len(g.Nodes) == 0 {
		return errors.New("graph: 图为空, 至少需要一个节点")
	}

	// —— 节点: ID 唯一性 / Kind / Loop 策略 ——
	ids := make(map[string]bool, len(g.Nodes))
	kinds := make(map[string]NodeKind, len(g.Nodes))
	for i, n := range g.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			return fmt.Errorf("graph: 第 %d 个节点的 ID 为空", i+1)
		}
		if ids[n.ID] {
			return fmt.Errorf("graph: 节点 ID %q 重复", n.ID)
		}
		// 分片/组内/展开产物的 ID 是引擎按分隔符合成的 (<map>#<i> / <组>#it<k>/<成员>
		// / <父>/<子>), 声明期就带这些分隔符会让 journal 归因串台。
		if strings.ContainsAny(n.ID, ShardIDSep+GroupMemberIDSep) {
			return fmt.Errorf("graph: 节点 ID %q 含保留分隔符 %q (引擎用它合成分片/组内/展开产物的 ID)",
				n.ID, ShardIDSep+GroupMemberIDSep)
		}
		ids[n.ID] = true
		kinds[n.ID] = n.Kind
		if err := validateNodeShape(n, insideGroup); err != nil {
			return err
		}
	}

	// —— 边: 端点存在 / 自环 / 条件语法 ——
	out := map[string][]string{}
	indeg := map[string]int{}
	preds := map[string][]string{}
	for _, ed := range g.Edges {
		if !ids[ed.From] {
			return fmt.Errorf("graph: 边 %s→%s 的起点 %q 不存在", ed.From, ed.To, ed.From)
		}
		if !ids[ed.To] {
			return fmt.Errorf("graph: 边 %s→%s 的终点 %q 不存在", ed.From, ed.To, ed.To)
		}
		if ed.From == ed.To {
			return fmt.Errorf("graph: 节点 %q 存在自环边 (循环请用 Loop 策略, design/01 §4.4)", ed.From)
		}
		if _, err := ParseCondition(ed.Condition); err != nil {
			return fmt.Errorf("graph: 边 %s→%s 的条件语法错误: %w", ed.From, ed.To, err)
		}
		out[ed.From] = append(out[ed.From], ed.To)
		indeg[ed.To]++
		preds[ed.To] = append(preds[ed.To], ed.From)
	}

	// —— reduce 必须有 map 组可聚合 ——
	// 没有 map 前驱的 reduce 是纯误声明: 它会拿到空 Shards 然后"成功"聚合出空产出,
	// 静默劣化成一个普通节点。开图就拦住比事后查空产出便宜得多。
	for _, n := range g.Nodes {
		if n.Kind != NodeKindReduce {
			continue
		}
		want := map[string]bool{}
		if n.Reduce != nil {
			for _, from := range n.Reduce.From {
				want[from] = true
			}
		}
		found := 0
		for _, p := range preds[n.ID] {
			if kinds[p] != NodeKindMap {
				continue
			}
			if len(want) > 0 && !want[p] {
				continue
			}
			found++
		}
		if found == 0 {
			if len(want) > 0 {
				return fmt.Errorf("graph: reduce 节点 %q 的 reduce.from=%v 里没有一个是它的 map 类直接前驱", n.ID, n.Reduce.From)
			}
			return fmt.Errorf("graph: reduce 节点 %q 没有 map 类直接前驱 (聚合对象为空)", n.ID)
		}
		for from := range want {
			if !ids[from] {
				return fmt.Errorf("graph: reduce 节点 %q 的 reduce.from 引用了不存在的节点 %q", n.ID, from)
			}
			if kinds[from] != NodeKindMap {
				return fmt.Errorf("graph: reduce 节点 %q 的 reduce.from 引用的 %q 不是 map 节点 (Kind=%q)", n.ID, from, kinds[from])
			}
		}
	}

	// —— 入口集: 入度 0 的节点。全图无入口 ⇒ 必含有向环。 ——
	var roots []string
	for _, n := range g.Nodes {
		if indeg[n.ID] == 0 {
			roots = append(roots, n.ID)
		}
	}
	if len(roots) == 0 {
		return errors.New("graph: 不存在入度为 0 的入口节点, 图必然包含有向环")
	}

	// —— 可达性: 从入口集 BFS; 不可达节点报错 (检出脱离主图的游离环/孤岛)。 ——
	seen := map[string]bool{}
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, out[id]...)
	}
	var unreachable []string
	for _, n := range g.Nodes {
		if !seen[n.ID] {
			unreachable = append(unreachable, n.ID)
		}
	}
	if len(unreachable) > 0 {
		return fmt.Errorf("graph: 节点 %s 从任何入度为 0 的入口节点均不可达", strings.Join(unreachable, ", "))
	}

	// —— 有向环: Kahn 拓扑排序, 未能出队的节点即环上/环下游节点。 ——
	deg := make(map[string]int, len(indeg))
	for k, v := range indeg {
		deg[k] = v
	}
	queue = append([]string(nil), roots...)
	processed := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		processed++
		for _, to := range out[id] {
			deg[to]--
			if deg[to] == 0 {
				queue = append(queue, to)
			}
		}
	}
	if processed < len(g.Nodes) {
		var cyc []string
		for _, n := range g.Nodes {
			if deg[n.ID] > 0 {
				cyc = append(cyc, n.ID)
			}
		}
		return fmt.Errorf("graph: 存在有向环, 涉及节点: %s", strings.Join(cyc, ", "))
	}
	return nil
}

// validateNodeShape 单个节点的形态校验 (Kind + 各形态策略 + Loop/Expand 边界)。
// 独立成函数是为了让**动态展开产物**走同一套规则 (expand.go prepareExpansion 调用它):
// 展开进来的节点若能绕过形态校验, 等于给了 LLM 一条把非法节点塞进运行图的路。
func validateNodeShape(n NodeSpec, insideGroup bool) error {
	switch n.Kind {
	case NodeKindAgent, NodeKindGate:
		// 叶子形态: 一次 runner 调用
	case NodeKindMap:
		if err := validateMapNode(n); err != nil {
			return err
		}
	case NodeKindReduce:
		if err := validateReduceNode(n); err != nil {
			return err
		}
	case NodeKindLoopGroup:
		if insideGroup {
			return fmt.Errorf("graph: 节点 %q: loop-group 组内不允许再嵌套 loop-group (轮次相乘后预算无法推理, design/01 §4.4)", n.ID)
		}
		if err := validateGroupNode(n); err != nil {
			return err
		}
	case NodeKindRouter, NodeKindSubgraph, NodeKindHuman:
		// 明确报错而不是静默接受: 静默接受等于把一个什么都不做的节点混进图,
		// 表现为"整条分支莫名 skipped", 排查成本远高于开图时就失败。
		return fmt.Errorf("graph: 节点 %q 的 Kind %q 尚未实现 (design/01 §4.1 预留形态, 已实现: agent|gate|map|reduce|loop-group)", n.ID, n.Kind)
	default:
		return fmt.Errorf("graph: 节点 %q 的 Kind %q 未知 (支持 agent|gate|map|reduce|loop-group)", n.ID, n.Kind)
	}
	if n.Loop != nil {
		if n.Kind == NodeKindLoopGroup {
			return fmt.Errorf("graph: 节点 %q 是 loop-group, 不得再声明节点级 Loop (组级循环用 group.loop, 两层并存会让轮次相乘)", n.ID)
		}
		if err := validateLoopPolicy(*n.Loop, fmt.Sprintf("节点 %q 的 Loop", n.ID)); err != nil {
			return err
		}
	}
	if err := validatePlacement(n.Agent.Placement, n.ID); err != nil {
		return err
	}
	if n.Expand != nil && n.Expand.MaxNodes <= 0 {
		return fmt.Errorf("graph: 节点 %q 的 Expand.max_nodes 必须 > 0 (无界展开违法, design/01 §4.2)", n.ID)
	}
	// Spawn 的三个上限允许留 0 (走缺省), 但**负值必须拒**: 负数会让 maxXxx() 的
	// "<=0 取缺省"判定把它当成"没设", 于是一个写错的 -1 会静默变成默认值而不是报错
	// —— 派生边界是安全属性, 不能靠猜。
	if n.Spawn != nil {
		if n.Spawn.MaxDepth < 0 || n.Spawn.MaxNodes < 0 || n.Spawn.MaxSpawns < 0 {
			return fmt.Errorf("graph: 节点 %q 的 Spawn 上限不得为负 (0 = 取缺省, design/01 §4.8)", n.ID)
		}
	}
	return nil
}

// validateMapNode map 节点的策略校验 (design/01 §4.2)。
func validateMapNode(n NodeSpec) error {
	if n.Map == nil {
		return fmt.Errorf("graph: map 节点 %q 缺少 map 策略 (至少要声明 max_shards)", n.ID)
	}
	if n.Map.MaxShards <= 0 {
		return fmt.Errorf("graph: map 节点 %q 的 map.max_shards 必须 > 0 (无界扇出违法: 集合来自上游 LLM 产出)", n.ID)
	}
	if n.Map.MinShards < 0 {
		return fmt.Errorf("graph: map 节点 %q 的 map.min_shards 不得为负", n.ID)
	}
	if n.Map.MinShards > n.Map.MaxShards {
		return fmt.Errorf("graph: map 节点 %q 的 map.min_shards(%d) > max_shards(%d), 恒不可满足",
			n.ID, n.Map.MinShards, n.Map.MaxShards)
	}
	switch n.Map.Split {
	case "", SplitLines, SplitParagraph, SplitJSONArray, SplitWhole:
	default:
		return fmt.Errorf("graph: map 节点 %q 的 map.split=%q 未知 (支持 %s|%s|%s|%s)",
			n.ID, n.Map.Split, SplitLines, SplitParagraph, SplitJSONArray, SplitWhole)
	}
	switch src := n.Map.Source; {
	case src == "", src == SourcePrev, src == SourceObjective:
	case strings.HasPrefix(src, SourcePrevPrefix):
		if strings.TrimSpace(strings.TrimPrefix(src, SourcePrevPrefix)) == "" {
			return fmt.Errorf("graph: map 节点 %q 的 map.source=%q 缺少上游节点 ID", n.ID, src)
		}
	case strings.HasPrefix(src, SourceParamPrefix):
		if strings.TrimSpace(strings.TrimPrefix(src, SourceParamPrefix)) == "" {
			return fmt.Errorf("graph: map 节点 %q 的 map.source=%q 缺少参数键", n.ID, src)
		}
	default:
		return fmt.Errorf("graph: map 节点 %q 的 map.source=%q 未知 (支持 %s|%s|%s<节点ID>|%s<键>)",
			n.ID, src, SourcePrev, SourceObjective, SourcePrevPrefix, SourceParamPrefix)
	}
	return nil
}

// validateReduceNode reduce 节点的策略校验 (来源与 map 前驱的关系在主流程里查)。
func validateReduceNode(n NodeSpec) error {
	if n.Reduce == nil {
		return nil // 全默认: 聚合全部 map 前驱, 交给 runner
	}
	switch n.Reduce.Strategy {
	case "", ReduceRunner, ReduceConcat, ReduceLongest, ReduceVote, ReduceTrimmedMean:
	default:
		return fmt.Errorf("graph: reduce 节点 %q 的 reduce.strategy=%q 未知 (支持 %s|%s|%s|%s|%s)",
			n.ID, n.Reduce.Strategy, ReduceRunner, ReduceConcat, ReduceLongest, ReduceVote, ReduceTrimmedMean)
	}
	if n.Reduce.MinSamples < 0 {
		return fmt.Errorf("graph: reduce 节点 %q 的 reduce.min_samples 不得为负 (0 = 取策略缺省)", n.ID)
	}
	// min_samples 只有投票/截尾均值读得到。声明在 concat/longest/runner 上是死配置,
	// 作者多半以为自己设了一道闸 —— 拦住比让它静默无效好查 (与 map.min_shards 同款口径)。
	if n.Reduce.MinSamples > 0 {
		switch n.Reduce.Strategy {
		case ReduceVote, ReduceTrimmedMean:
		default:
			return fmt.Errorf("graph: reduce 节点 %q 声明了 reduce.min_samples=%d, 但 strategy=%q 不做样本融合 (只有 %s|%s 读它)",
				n.ID, n.Reduce.MinSamples, orDefault(n.Reduce.Strategy, ReduceRunner), ReduceVote, ReduceTrimmedMean)
		}
	}
	return nil
}

// validateLoopPolicy 节点级 Loop 与组级 group.loop 共用的循环策略校验。
func validateLoopPolicy(l LoopPolicy, where string) error {
	if l.MaxIterations <= 0 {
		return fmt.Errorf("graph: %s.max_iterations 必须 > 0 (无界循环违法, design/01 §4.4)", where)
	}
	if _, err := ParseCondition(l.Until); err != nil {
		return fmt.Errorf("graph: %s.until 条件语法错误: %w", where, err)
	}
	if l.Terminator == nil {
		return nil
	}
	// Until 与终止器并存 = 两套退出判定, 谁先谁后是纯实现细节。拒绝而不是定个优先级:
	// 定优先级等于让另一套判据静默失效, 而声明它的人以为两条都在生效。
	if strings.TrimSpace(l.Until) != "" {
		return fmt.Errorf("graph: %s 同时声明了 until=%q 与 terminator=%q (二者都能决定退出, 并存等于把'第几轮停'交给求值顺序; 判据请二选一)",
			where, l.Until, l.Terminator.Name)
	}
	// 参数校验交给终止器工厂本身 (同一条解析路径, 于是"能开图"与"能构造"永不漂移)。
	if _, err := resolveTerminator(l.Terminator); err != nil {
		return fmt.Errorf("graph: %s.terminator 非法: %w", where, err)
	}
	return nil
}

// validatePlacement 放置约束的形状校验 (design/01 §4.9)。
//
// 为什么非法值必须**拒**而不是当成 any: 放置是治理侧约束, 一个写错的
// prefer="remote" (漏了冒号与 runtime 名) 若被当成 any, 表现是节点随机落在任意
// 机器上却毫无报错, 而作者以为自己把它钉住了。能力标签 (Require) 反过来不校验 ——
// 取值域由宿主的 RuntimeCaps 定义, 内核持一张白名单必然与宿主漂移。
func validatePlacement(p *PlacementSpec, nodeID string) error {
	if p == nil {
		return nil
	}
	for i, req := range p.Require {
		if strings.TrimSpace(req) == "" {
			return fmt.Errorf("graph: 节点 %q 的 placement.require[%d] 为空 (空标签对 RuntimeCaps.Has 恒真, 等于一条不起作用的硬约束)", nodeID, i)
		}
	}
	switch pref := strings.TrimSpace(p.Prefer); {
	case pref == "" || pref == PlacementPreferLocal || pref == PlacementPreferAny:
	case strings.HasPrefix(pref, PlacementPreferRemote):
		if strings.TrimSpace(strings.TrimPrefix(pref, PlacementPreferRemote)) == "" {
			return fmt.Errorf("graph: 节点 %q 的 placement.prefer=%q 缺少 runtime 名 (格式 %s<name>)", nodeID, p.Prefer, PlacementPreferRemote)
		}
	default:
		return fmt.Errorf("graph: 节点 %q 的 placement.prefer=%q 非法 (支持 %s|%s<name>|%s)",
			nodeID, p.Prefer, PlacementPreferLocal, PlacementPreferRemote, PlacementPreferAny)
	}
	switch aff := strings.TrimSpace(p.Affinity); aff {
	case "":
		if strings.TrimSpace(p.AffinityKey) != "" {
			return fmt.Errorf("graph: 节点 %q 声明了 placement.affinity_key=%q 但没声明 affinity (死配置: 分组键不会被任何人读)", nodeID, p.AffinityKey)
		}
	case PlacementAffinityTeam:
	default:
		return fmt.Errorf("graph: 节点 %q 的 placement.affinity=%q 未知 (目前只支持 %s)", nodeID, p.Affinity, PlacementAffinityTeam)
	}
	return nil
}

// validateGroupNode loop-group 节点校验: 组级循环策略 + 组内子图**递归**走同一套规则。
func validateGroupNode(n NodeSpec) error {
	if n.Group == nil || len(n.Group.Nodes) == 0 {
		return fmt.Errorf("graph: loop-group 节点 %q 缺少组内子图 (group.nodes 为空)", n.ID)
	}
	if err := validateLoopPolicy(n.Group.Loop, fmt.Sprintf("loop-group 节点 %q 的 group.loop", n.ID)); err != nil {
		return err
	}
	sub := GraphSpec{Name: n.ID + "-group", Nodes: n.Group.Nodes, Edges: n.Group.Edges}
	if err := sub.validate(true); err != nil {
		return fmt.Errorf("graph: loop-group 节点 %q 的组内子图非法: %w", n.ID, err)
	}
	// ResultFrom: 显式声明须是成员; 未声明则要求组内恰好一个出度 0 节点
	// (多个 sink 时"组产出是谁"取决于声明顺序 —— 那是个隐蔽的不确定性, 必须显式)。
	members := map[string]bool{}
	for _, m := range n.Group.Nodes {
		members[m.ID] = true
	}
	if rf := strings.TrimSpace(n.Group.ResultFrom); rf != "" {
		if !members[rf] {
			return fmt.Errorf("graph: loop-group 节点 %q 的 group.result_from=%q 不是组内成员", n.ID, rf)
		}
		return nil
	}
	if sinks := groupSinks(*n.Group); len(sinks) != 1 {
		return fmt.Errorf("graph: loop-group 节点 %q 的组内有 %d 个出度 0 节点 (%s), 必须显式声明 group.result_from",
			n.ID, len(sinks), strings.Join(sinks, ", "))
	}
	return nil
}

// groupSinks 组内出度 0 的成员 (按声明序)。
func groupSinks(g GroupPolicy) []string {
	hasOut := map[string]bool{}
	for _, ed := range g.Edges {
		hasOut[ed.From] = true
	}
	var sinks []string
	for _, m := range g.Nodes {
		if !hasOut[m.ID] {
			sinks = append(sinks, m.ID)
		}
	}
	return sinks
}
