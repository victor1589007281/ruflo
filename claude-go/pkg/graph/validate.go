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
// 检查项: 图非空; 节点 ID 非空且唯一; Kind 合法 (agent|gate|map|reduce|loop-group|
// subgraph|human, router 显式报错); 各形态的策略字段完备 (map 切分/reduce 来源/
// 组内子图递归校验/被引用子图递归校验与引用环/Expand 边界/Suspend 授权与位置);
// Loop.MaxIterations>0; join 语义取值合法; 边端点存在; 无自环;
// 存在入度 0 入口; 全部节点从入口可达; 无有向环 (Kahn 拓扑);
// 边 Condition 与 Loop.Until 语法合法。
func (g GraphSpec) Validate() error { return g.validate(validateCtx{}) }

// validateCtx 校验上下文: 递归校验时"我在图的什么位置"。
//
// 两个布尔刻意分开而不是合成一个"nested": insideGroup 只管一件历史规则 (组内不许再
// 嵌套 loop-group, 见 GroupPolicy 的"轮次相乘"论证), 而 nested 管的是**这一层是不是
// 顶层调度层**。把它们并成一个会改变既有行为 —— 派生子图 (spawn) 现在是允许内嵌
// loop-group 的, 合并后会连带把那条路禁掉。
type validateCtx struct {
	// insideGroup 本层是 loop-group 的组内子图。
	insideGroup bool
	// nested 本层**不是顶层调度层**: 组内成员 / 派生子图 (spawn) / 展开产物。
	// 这些位置一律不许声明挂起 (Suspend / Kind=human), 也不许再引用子图 (Kind=subgraph):
	//   - 挂起: 组的轮次进度与 spawn 的同步语义都表达不了"跑到一半停下"(见 suspend.go
	//     与 subgraph.go 文件头四); 展开产物与 spawn 的内容还来自 LLM, 让模型能插入一个
	//     挂起节点等于把"这次运行到哪结束"交给模型;
	//   - 引用子图: 组轮次前缀/派生指纹再叠一层子图命名空间, journal 归因与 resume
	//     都需要另一套口径, 而生产用例 (composite 组合) 全在顶层。
	// **被引用图的成员不算 nested**: 它自己就是一层完整的顶层调度 (有独立命名空间、
	// 按限定 ID 吃缓存), 所以子图里可以有 human 节点, 也可以再引用子图 (受深度闸约束)。
	nested bool
	// sgPath 当前 subgraph 引用链 (由外到内的图名), 用于查引用环与深度、并定位报错。
	sgPath []string
}

// validate 真正的校验体。
func (g GraphSpec) validate(vc validateCtx) error {
	if len(g.Nodes) == 0 {
		return errors.New("graph: 图为空, 至少需要一个节点")
	}
	switch j := strings.TrimSpace(g.Policies.DefaultJoin); j {
	case "", JoinOr, JoinAnd:
	default:
		// 写错的 default_join 若被当成 or, 表现是"我明明整图设了 AND 却没生效",
		// 而 AND 与 OR 的差别是整条汇总链跑不跑 —— 必须拒。
		return fmt.Errorf("graph: policies.default_join=%q 未知 (支持 %s|%s)", j, JoinOr, JoinAnd)
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
		if err := validateNodeShape(n, vc); err != nil {
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

// validateNodeShape 单个节点的形态校验 (Kind + 各形态策略 + Loop/Expand/Suspend/join 边界)。
// 独立成函数是为了让**动态展开产物**走同一套规则 (expand.go prepareExpansion 调用它):
// 展开进来的节点若能绕过形态校验, 等于给了 LLM 一条把非法节点塞进运行图的路。
func validateNodeShape(n NodeSpec, vc validateCtx) error {
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
		if vc.insideGroup {
			return fmt.Errorf("graph: 节点 %q: loop-group 组内不允许再嵌套 loop-group (轮次相乘后预算无法推理, design/01 §4.4)", n.ID)
		}
		if err := validateGroupNode(n, vc); err != nil {
			return err
		}
	case NodeKindSubgraph:
		if vc.nested {
			return fmt.Errorf("graph: 节点 %q: subgraph 不得出现在组内/派生子图/展开产物里 (命名空间再叠一层后 journal 归因与 resume 需要另一套口径; 图的组合请在顶层做, 见 subgraph.go)", n.ID)
		}
		if err := validateSubgraphNode(n, vc); err != nil {
			return err
		}
	case NodeKindHuman:
		if vc.nested {
			return fmt.Errorf("graph: 节点 %q: human 不得出现在组内/派生子图/展开产物里 (组的轮次与 spawn 的同步语义表达不了'跑到一半等人'; 展开产物还来自 LLM 产出, 等于让模型自己插入一个审批闸)", n.ID)
		}
		if n.Suspend != nil {
			return fmt.Errorf("graph: human 节点 %q 不得再声明 suspend (human 的挂起是形态自带的, 两个真源会让'能不能挂起/几次'各说一套)", n.ID)
		}
	case NodeKindRouter:
		// **不实现且不计划实现**, 但仍显式报错而不是静默接受: 静默接受等于把一个什么
		// 都不做的节点混进图, 表现为"整条分支莫名 skipped", 排查成本远高于开图就失败。
		//
		// 为什么判定它冗余 (而不是"以后再做"): router 的职责是"按前驱结果选下游", 而
		// 引擎的条件边**就是**逐条入边对前驱结果求值 (condition.go: ok|fail|score <op> N|
		// output contains "..."), 加上默认 OR-join 的"无满足入边即 skipped", 分支路由已经
		// 完整表达。设计文档提到的另一半"LLM 意图判断"也不需要新形态: 让一个 agent/gate
		// 节点产出分数或关键词, 再用条件边路由即可 —— 那还顺带把判据留在了 journal 里。
		// 15 个 mode 逐个核实后没有一个需要它。多一种 Kind = 多一套语义 + 多一份
		// Validate + 多一条 resume 路径, 为一个零收益的形态付这份维护成本不值得。
		return fmt.Errorf("graph: 节点 %q 的 Kind %q 尚未实现, 且核实后判定不需要实现 —— 分支路由请用条件边 (edge.condition: ok|fail|score <op> N|output contains \"...\"), 语义等价且判据进 journal; 理由见 validate.go 的论证", n.ID, n.Kind)
	default:
		return fmt.Errorf("graph: 节点 %q 的 Kind %q 未知 (支持 agent|gate|map|reduce|loop-group|subgraph|human)", n.ID, n.Kind)
	}
	if err := validateJoin(n); err != nil {
		return err
	}
	if err := validateSuspend(n, vc); err != nil {
		return err
	}
	if n.Loop != nil {
		if n.Kind == NodeKindLoopGroup {
			return fmt.Errorf("graph: 节点 %q 是 loop-group, 不得再声明节点级 Loop (组级循环用 group.loop, 两层并存会让轮次相乘)", n.ID)
		}
		if n.Kind == NodeKindSubgraph {
			return fmt.Errorf("graph: 节点 %q 是 subgraph, 不得声明节点级 Loop (让整张子图反复是 loop-group 的职责; 轮次不进子图命名空间, 于是同一成员在 journal 里会有多轮同 ID 的产出, resume 分不清该用哪一轮)", n.ID)
		}
		if n.Kind == NodeKindHuman {
			return fmt.Errorf("graph: 节点 %q 是 human, 不得声明节点级 Loop (循环的下一轮要回灌上一轮产出, 而'等人'没有产出可回灌; 多轮询问请用多个 human 节点)", n.ID)
		}
		if err := validateLoopPolicy(*n.Loop, fmt.Sprintf("节点 %q 的 Loop", n.ID)); err != nil {
			return err
		}
	}
	if err := validatePlacement(n.Agent.Placement, n.ID); err != nil {
		return err
	}
	if n.Expand != nil {
		if n.Expand.MaxNodes <= 0 {
			return fmt.Errorf("graph: 节点 %q 的 Expand.max_nodes 必须 > 0 (无界展开违法, design/01 §4.2)", n.ID)
		}
		if n.Kind == NodeKindSubgraph {
			return fmt.Errorf("graph: 节点 %q 是 subgraph, 不得声明 Expand (它的产出是子图内某个成员的产出, 让父图按'这个节点展开了'去接线会把展开归因错人; 要动态展开请在被引用图内部声明)", n.ID)
		}
		if n.Kind == NodeKindHuman {
			return fmt.Errorf("graph: 节点 %q 是 human, 不得声明 Expand (它不经 runner, 没有产出 Expansion 的地方)", n.ID)
		}
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

// validateJoin 节点级 join 语义取值校验 (见 join.go)。
// 非法值必须拒而不是当成 or: 写错的 "AND"/"all" 若被静默当成 or, 表现是"我声明了
// AND 汇聚但上游挂了下游照跑", 而作者以为自己钉住了"拿不全就不跑"。
func validateJoin(n NodeSpec) error {
	switch j := strings.TrimSpace(n.Join); j {
	case "", JoinOr, JoinAnd:
		return nil
	default:
		return fmt.Errorf("graph: 节点 %q 的 join=%q 未知 (支持 %s|%s, 空 = 取图级 default_join, 再空 = %s)",
			n.ID, j, JoinOr, JoinAnd, JoinOr)
	}
}

// validateSuspend 挂起授权的形状与位置校验 (见 suspend.go)。
func validateSuspend(n NodeSpec, vc validateCtx) error {
	if n.Suspend == nil {
		return nil
	}
	if vc.nested {
		return fmt.Errorf("graph: 节点 %q 声明了 suspend, 但它在组内/派生子图/展开产物里 (那些位置表达不了'跑到一半停下': 组的轮次进度会把挂起的那半轮丢掉, spawn 是 agent 的同步调用停不下来; 见 suspend.go)", n.ID)
	}
	// 负值必须拒: maxRevives()/maxWaitSec() 的"<=0 取缺省"会把一个写错的 -1 静默变成
	// 默认值而不是报错 —— 与 Spawn 上限同款口径 (那个坑已经踩过)。
	if n.Suspend.MaxRevives < 0 || n.Suspend.MaxWaitSec < 0 {
		return fmt.Errorf("graph: 节点 %q 的 suspend 上限不得为负 (0 = 取缺省: max_revives=%d, max_wait_sec=%d)",
			n.ID, DefaultMaxRevives, DefaultReviveWaitSec)
	}
	switch n.Kind {
	case NodeKindAgent, NodeKindGate, NodeKindReduce:
		// 这三种是**一次 runner 调用**, 挂起有明确含义 ("这次先别跑, 过会儿再来")。
	case NodeKindMap:
		return fmt.Errorf("graph: map 节点 %q 不得声明 suspend (分片是 map 节点的副本, 声明会被每个分片继承, 而一个分片挂起在汇总里既不是成功也不是失败 —— 限流退避请声明在分片会调到的重试上, 或把扇出改成 map→reduce 之外的形态)", n.ID)
	case NodeKindLoopGroup:
		return fmt.Errorf("graph: loop-group 节点 %q 不得声明 suspend (组的轮次进度靠 loop.group.iteration 计数, 一轮跑到一半挂起表达不了: resume 会从下一轮开始, 挂起的那半轮永久丢失)", n.ID)
	case NodeKindSubgraph:
		return fmt.Errorf("graph: subgraph 节点 %q 不得声明 suspend (它自己不调 runner, 声明是死配置; 它的挂起来自成员的挂起并自动向上传播, 见 subgraph.go)", n.ID)
	default:
		return fmt.Errorf("graph: 节点 %q (Kind=%q) 不得声明 suspend", n.ID, n.Kind)
	}
	if n.Loop != nil {
		return fmt.Errorf("graph: 节点 %q 同时声明了 loop 与 suspend (循环的下一轮靠回灌上一轮产出, 而挂起意味着这一轮没有产出 —— 拿一份空产出回灌等于把一次等待变成一轮真实迭代, 还会白烧一轮 token)", n.ID)
	}
	return nil
}

// validateSubgraphNode subgraph 节点校验: 引用可解析 + 无引用环 + 深度 + 被引用图递归合法。
//
// 为什么在开图时就去注册表把图取出来递归校验 (而不是留到运行期):
// 引用是**声明期固定**的, 开图时就能解析 —— 而运行期才发现"被引用的图不存在/自己成环"
// 时, 半张图已经跑掉了 (烧掉的 token 拿不回来), 且那半张图的产出还会被当成正常交付。
func validateSubgraphNode(n NodeSpec, vc validateCtx) error {
	if n.Subgraph == nil || strings.TrimSpace(n.Subgraph.Graph) == "" {
		return fmt.Errorf("graph: subgraph 节点 %q 缺少 subgraph.graph (要引用哪张已注册的图)", n.ID)
	}
	if n.Subgraph.MaxDepth < 0 {
		return fmt.Errorf("graph: subgraph 节点 %q 的 subgraph.max_depth 不得为负 (0 = 取缺省 %d)", n.ID, DefaultSubgraphDepth)
	}
	name := strings.TrimSpace(n.Subgraph.Graph)
	// 引用环: A 引用 B、B 又引用 A ⇒ 运行期是无限递归。深度闸能兜住它 (跑到上限就
	// 失败), 但那是"跑一半才炸"; 环在开图时是可判定的, 就该在开图时拒。
	for _, prev := range vc.sgPath {
		if prev == name {
			return fmt.Errorf("graph: subgraph 节点 %q 造成图引用环: %s → %s",
				n.ID, strings.Join(vc.sgPath, " → "), name)
		}
	}
	if d := len(vc.sgPath) + 1; d > subgraphMaxDepth(n.Subgraph) {
		return fmt.Errorf("graph: subgraph 节点 %q 的引用深度 %d 超过 subgraph.max_depth=%d (链: %s → %s)",
			n.ID, d, subgraphMaxDepth(n.Subgraph), strings.Join(vc.sgPath, " → "), name)
	}
	sub, ok := LookupGraph(name)
	if !ok {
		return fmt.Errorf("graph: subgraph 节点 %q 引用的图 %q 未注册 (已注册: %s)",
			n.ID, name, strings.Join(RegisteredGraphs(), ", "))
	}
	// 被引用图按**同一套规则**递归校验 (另写一套必然与顶层漂移)。
	// nested 保持 false: 被引用图自己就是一层完整的顶层调度层 (独立命名空间 + 按限定 ID
	// 吃缓存), 所以它里面可以有 human 节点、也可以再引用子图 —— 见 validateCtx.nested。
	childCtx := validateCtx{sgPath: append(append([]string(nil), vc.sgPath...), name)}
	if err := sub.validate(childCtx); err != nil {
		return fmt.Errorf("graph: subgraph 节点 %q 引用的图 %q 非法: %w", n.ID, name, err)
	}
	if _, err := subgraphResultFrom(sub, n.Subgraph.ResultFrom); err != nil {
		return fmt.Errorf("graph: subgraph 节点 %q: %w", n.ID, err)
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
func validateGroupNode(n NodeSpec, vc validateCtx) error {
	if n.Group == nil || len(n.Group.Nodes) == 0 {
		return fmt.Errorf("graph: loop-group 节点 %q 缺少组内子图 (group.nodes 为空)", n.ID)
	}
	if err := validateLoopPolicy(n.Group.Loop, fmt.Sprintf("loop-group 节点 %q 的 group.loop", n.ID)); err != nil {
		return err
	}
	sub := GraphSpec{Name: n.ID + "-group", Nodes: n.Group.Nodes, Edges: n.Group.Edges}
	// insideGroup + nested 都置上, 且带上引用链 (组内节点若引用子图会被 nested 拒,
	// 但链要传下去才能让报错读得懂自己在哪一层)。
	if err := sub.validate(validateCtx{insideGroup: true, nested: true, sgPath: vc.sgPath}); err != nil {
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
