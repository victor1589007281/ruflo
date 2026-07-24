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
// 检查项: 图非空; 节点 ID 非空且唯一; Kind 合法 (v1 仅 agent|gate);
// Loop.MaxIterations>0; 边端点存在; 无自环; 存在入度 0 入口; 全部节点从
// 入口可达; 无有向环 (Kahn 拓扑); 边 Condition 与 Loop.Until 语法合法。
func (g GraphSpec) Validate() error {
	if len(g.Nodes) == 0 {
		return errors.New("graph: 图为空, 至少需要一个节点")
	}

	// —— 节点: ID 唯一性 / Kind / Loop 策略 ——
	ids := make(map[string]bool, len(g.Nodes))
	for i, n := range g.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			return fmt.Errorf("graph: 第 %d 个节点的 ID 为空", i+1)
		}
		if ids[n.ID] {
			return fmt.Errorf("graph: 节点 ID %q 重复", n.ID)
		}
		ids[n.ID] = true
		switch n.Kind {
		case NodeKindAgent, NodeKindGate:
			// v1 支持的两种形态
		case NodeKindMap, NodeKindSubgraph, NodeKindHuman, "router", "reduce", "loop-group":
			return fmt.Errorf("graph: 节点 %q 的 Kind %q v1 暂不支持 (design/01 §4.1 预留形态)", n.ID, n.Kind)
		default:
			return fmt.Errorf("graph: 节点 %q 的 Kind %q 未知 (v1 仅支持 agent|gate)", n.ID, n.Kind)
		}
		if n.Loop != nil {
			if n.Loop.MaxIterations <= 0 {
				return fmt.Errorf("graph: 节点 %q 的 Loop.max_iterations 必须 > 0 (无界循环违法, design/01 §4.4)", n.ID)
			}
			if _, err := ParseCondition(n.Loop.Until); err != nil {
				return fmt.Errorf("graph: 节点 %q 的 Loop.until 条件语法错误: %w", n.ID, err)
			}
		}
	}

	// —— 边: 端点存在 / 自环 / 条件语法 ——
	out := map[string][]string{}
	indeg := map[string]int{}
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
