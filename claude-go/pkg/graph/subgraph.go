package graph

// subgraph.go —— subgraph 节点: 引用另一张**已注册**的图作为一个节点
// (design/01 §4.1 全表第 6 种形态)。
//
// ---------------------------------------------------------------------------
// 一、三个"都在跑一个子图"的机制, 边界在哪 (不写清就没人知道该用哪个)
// ---------------------------------------------------------------------------
//
//	                子图从哪来            何时确定        改父图  谁在等
//	loop-group      内嵌在节点里          声明期          否      —— (它就是父节点自己)
//	  (group.go)    GroupPolicy.Nodes
//	  为什么内嵌: 组成员若是顶层节点, 顶层 Validate 的"入口可达/无环"会把它们当孤岛
//	  报错, 且调度器会把它们当普通节点各跑一次。用途 = **一组节点整体反复**(对抗轮次)。
//
//	subgraph        注册表里的**图名**    声明期          否      父节点 (同步)
//	  (本文件)      SubgraphSpec.Graph
//	  为什么引用而不内嵌: 同一张子流程被多张图引用 —— 内嵌等于抄 N 份, 改一处要改 N 处。
//	  用途 = **图的组合复用**(composite: 前端图 + 后端图 + 联调 gate)。不循环。
//
//	SpawnSubgraph   agent 的工具调用      **运行期(LLM)**  否     节点内 agent (同步)
//	  (spawn.go)    SpawnRequest
//	  为什么另一套: 内容来自 LLM ⇒ 必须有四道边界闸 + 内容指纹命名空间 (相同请求命中
//	  resume 缓存、不同请求不可能误命中)。用途 = **agent 自主派生 subagent**。
//
//	动态展开        runner 的产出         **运行期(LLM)**  是     不等 (父跑完才并入)
//	  (expand.go)   NodeResult.Expansion
//	  唯一会**改父图**的一个: 展开产物插在父节点与它原有下游之间, 继续被顶层调度。
//
// 一句话选型: 要反复 → loop-group; 要复用一张具名图 → subgraph; 要让模型自己决定跑
// 什么 → spawn (同步等) 或 expand (改图继续调度)。
//
// ---------------------------------------------------------------------------
// 二、为什么复用 rc 而不是新起一个 Engine.Run
// ---------------------------------------------------------------------------
//
// spawn.go 已经把这条路走通了: 子图节点经**同一个** rc.nodeExec 执行、进**同一份**
// journal、受**同一套** hook 与预算覆盖 —— 这三样不是额外写的, 是"复用 rc"白拿的。
// 反过来说, 若在 subgraph 节点里嵌一次 Engine.Run:
//   - 预算台账 (BudgetManager) 会从零开始 ⇒ 子图的开销不计在父运行头上, MaxNodeRuns
//     形同虚设 (这正是 §4.8 收编裸 QueryEngine 要解决的那个问题的翻版);
//   - journal 要么另开一份 (进度真源分裂), 要么共用但 RunID 不同 ⇒ Replay 按 RunID
//     过滤会把子图事件全部丢掉, resume 直接失效;
//   - 嵌套 Run 会各自持有一套并发闸 ⇒ 峰值并发变成 maxPar 的乘积。
// 所以 subgraph 与 spawn 走**同一条**路: newDagRun + scheduleDAG + 前缀 scope。
// 差异只有三处: ① 子图来自注册表而非请求 (故命名空间不需要内容指纹, 见下);
// ② 校验发生在开图时 (声明期就能解析) 而非运行期; ③ 允许成员挂起并向上传播 (见四)。
//
// ## 命名空间: <父节点>~sg/<成员>
//
// 不用内容指纹 (spawn 用它是因为请求内容每次可能不同, 用序号会让 resume 缓存错配)。
// subgraph 的引用是**声明期固定**的, 同一个节点每次跑的就是同一张图, 用节点 ID 做
// 命名空间既稳定又可读。另起一个中缀 ~sg 而不复用展开的 <父>/<子>: 后者会与同一节点的
// 展开产物撞名, 撞了之后 journal 归因串台且 resume 缓存互相污染。
//
// ---------------------------------------------------------------------------
// 三、注册表是可变的, 所以子图要冻结进 journal
// ---------------------------------------------------------------------------
//
// RegisterGraph 是进程内的表, 生产上工作流是可以热注册的。若 resume 时重新去注册表取,
// 恢复出来的子图可能与首跑不是同一张 —— 那正是 map.expanded/graph.expanded 反复强调的
// 红线 (恢复出的图必须与首跑一致, 因为"这次跑的到底是哪张图"决定了产出怎么解释)。
// 故 subgraph.entered 事件里存**当时真正执行的那张图**, resume 时优先照它重建
// (Data.source = journal), 注册表只在没有记录时用 (source = registry)。
//
// ## 被引用图的 Policies 不生效 (记账而不是静默)
//
// 同一份 GraphSpec 既可能被独立运行 (那时它的 Policies 当然生效), 也可能被引用。
// 被引用时**运行级**的闸 (MaxParallel/MaxTotalNodes/DefaultRetry/DefaultJoin) 一律以
// 顶层为准: 否则一张被引用的图能靠自己的 Policies 把并发或总量上限抬高, 那是"约束
// 放宽"(§4.2 单调收窄反过来的一面)。但也不静默 —— 被忽略了哪些字段进
// subgraph.entered 的 ignored_policies, 否则"我明明设了 max_parallel"无从解释。
//
// ---------------------------------------------------------------------------
// 四、成员挂起会向上传播 (这是 subgraph 与 loop-group 的一处真差异)
// ---------------------------------------------------------------------------
//
// 被引用图的成员可以是 human 节点或声明了 Suspend 的节点。任一成员挂起 ⇒ subgraph
// 节点自己也报挂起 ⇒ 整个 run 以 suspended 收尾; resume 时 subgraph 节点重跑, 已完成的
// 成员**按限定 ID 吃 journal 缓存**(零 LLM), 挂起的那个成员继续等 —— 于是"审批卡在
// 子流程中间"这件事是可恢复的。
//
// loop-group 与 spawn 里则**禁止**挂起 (validate.go):
//   - loop-group: 轮次进度靠 loop.group.iteration 计数, 一轮跑到一半挂起表达不了
//     (resume 会从下一轮开始, 挂起的那半轮永久丢失);
//   - spawn: 派生是节点内 agent 的**同步**调用, agent 正卡在一次 LLM 会话里等返回值,
//     没有任何办法把一个进行中的会话挂起几小时。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SubgraphIDInfix 被引用子图的命名空间中缀: <父节点>~sg/<成员>。
const SubgraphIDInfix = "~sg"

// SubgraphMemberIDSep 命名空间与成员 ID 的分隔符。
const SubgraphMemberIDSep = "/"

var (
	graphRegMu sync.RWMutex
	graphReg   = map[string]GraphSpec{}
)

// RegisterGraph 注册一张可被 subgraph 节点引用的图 (键 = spec.Name)。
//
// **重名一律拒绝**而不是覆盖 (与 RegisterTerminator 同一口径, 理由更强): 一张被引用的
// 图被静默换掉, 等于让全部引用它的图在不改一个字的情况下换掉行为。要替换请显式
// UnregisterGraph 再注册 —— 那时"我知道我在换掉别人引用的东西"这件事是写在代码里的。
//
// 注册时**不做全量 Validate**: 被引用的图自己可能也含 subgraph 节点, 而它引用的图可能
// 还没注册 —— 让注册顺序决定合法性是个纯粹的坑。全量校验发生在引用它的图 Validate 时
// (那时整条引用链都能解析, 还能顺带查引用环)。这里只拦"连引用都没法开始"的两项。
func RegisterGraph(spec GraphSpec) error {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return fmt.Errorf("graph: 注册的图缺少 name (subgraph 节点靠它引用)")
	}
	if len(spec.Nodes) == 0 {
		return fmt.Errorf("graph: 注册的图 %q 为空 (零节点的子图会让引用它的节点恒失败)", name)
	}
	graphRegMu.Lock()
	defer graphRegMu.Unlock()
	if _, dup := graphReg[name]; dup {
		return fmt.Errorf("graph: 图 %q 已注册 (重名注册会静默换掉引用方的行为, 请先 UnregisterGraph)", name)
	}
	graphReg[name] = spec
	return nil
}

// UnregisterGraph 注销一张已注册的图 (返回是否真的存在过)。
// 存在是为了让"替换"必须显式两步走, 以及测试能自己清场。
func UnregisterGraph(name string) bool {
	graphRegMu.Lock()
	defer graphRegMu.Unlock()
	name = strings.TrimSpace(name)
	if _, ok := graphReg[name]; !ok {
		return false
	}
	delete(graphReg, name)
	return true
}

// LookupGraph 取一张已注册的图。
func LookupGraph(name string) (GraphSpec, bool) {
	graphRegMu.RLock()
	defer graphRegMu.RUnlock()
	g, ok := graphReg[strings.TrimSpace(name)]
	return g, ok
}

// RegisteredGraphs 已注册的图名 (排序, 供报错文案与 CLI 展示)。
func RegisteredGraphs() []string {
	graphRegMu.RLock()
	defer graphRegMu.RUnlock()
	out := make([]string, 0, len(graphReg))
	for n := range graphReg {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// subgraphMaxDepth 本节点允许的子图嵌套深度。
func subgraphMaxDepth(s *SubgraphSpec) int {
	if s == nil || s.MaxDepth <= 0 {
		return DefaultSubgraphDepth
	}
	return s.MaxDepth
}

// subgraphResultFrom 解析"取哪个成员的产出": 显式声明优先, 否则要求唯一出度 0 节点。
func subgraphResultFrom(sub GraphSpec, declared string) (string, error) {
	members := map[string]bool{}
	for _, n := range sub.Nodes {
		members[n.ID] = true
	}
	if rf := strings.TrimSpace(declared); rf != "" {
		if !members[rf] {
			return "", fmt.Errorf("result_from %q 不是图 %q 的成员", rf, sub.Name)
		}
		return rf, nil
	}
	sinks := subgraphSinks(sub.Nodes, sub.Edges)
	if len(sinks) != 1 {
		return "", fmt.Errorf("被引用图 %q 有 %d 个出度 0 节点 (%s), 必须显式声明 subgraph.result_from (否则结果取决于声明顺序)",
			sub.Name, len(sinks), strings.Join(sinks, ", "))
	}
	return sinks[0], nil
}

// ignoredSubgraphPolicies 被引用图声明了、但在引用场景下不生效的策略字段 (记账用)。
func ignoredSubgraphPolicies(p GraphPolicies) string {
	var out []string
	if p.MaxParallel > 0 {
		out = append(out, "max_parallel")
	}
	if p.MaxTotalNodes > 0 {
		out = append(out, "max_total_nodes")
	}
	if p.DefaultRetry != nil {
		out = append(out, "default_retry")
	}
	if p.DefaultJoin != "" {
		out = append(out, "default_join")
	}
	return strings.Join(out, ",")
}

// runSubgraphNode 执行一个 subgraph 节点。
// 返回 (节点结果, 形态特有的 hook 载荷)。
func (e *Engine) runSubgraphNode(ctx context.Context, rc *runCtx, scope execScope, node NodeSpec, in NodeInput) (NodeResult, map[string]any) {
	evID := scope.evID(node.ID)
	spec := node.Subgraph
	if spec == nil || strings.TrimSpace(spec.Graph) == "" { // Validate 已拦, 防御性兜底
		return NodeResult{Status: NodeStatusFailed, Err: "subgraph 节点缺少 subgraph.graph 引用"}, nil
	}

	// —— 1. 解析被引用的图: journal 优先 (见文件头三), 注册表兜底 ——
	var (
		sub        GraphSpec
		resultFrom string
		source     = "registry"
	)
	if rc.replay != nil {
		if rec, ok := rc.replay.Subgraphs[evID]; ok && len(rec.Sub.Nodes) > 0 {
			sub, resultFrom, source = rec.Sub, rec.ResultFrom, "journal"
		}
	}
	if source == "registry" {
		g, ok := LookupGraph(spec.Graph)
		if !ok {
			// fail-closed: 注册表在 Validate 之后被改过 (注销/进程换过)。
			// 不猜一张图跑 —— 那会静默换掉这个节点的语义。
			return NodeResult{Status: NodeStatusFailed,
				Err: fmt.Sprintf("subgraph: 图 %q 未注册 (已注册: %s)", spec.Graph, strings.Join(RegisteredGraphs(), ", "))}, nil
		}
		sub = g
		rf, err := subgraphResultFrom(sub, spec.ResultFrom)
		if err != nil {
			return NodeResult{Status: NodeStatusFailed, Err: "subgraph: " + err.Error()}, nil
		}
		resultFrom = rf
	}
	if resultFrom == "" || len(sub.Nodes) == 0 {
		return NodeResult{Status: NodeStatusFailed,
			Err: fmt.Sprintf("subgraph: 图 %q 的子图或产出节点为空", spec.Graph)}, nil
	}

	// —— 2. 深度闸: Validate 已查过引用环, 这道闸挡"合法但过深"与注册表被改成环 ——
	depth := scope.sgDepth + 1
	if max := subgraphMaxDepth(spec); depth > max {
		return NodeResult{Status: NodeStatusFailed,
			Err: fmt.Sprintf("subgraph: 引用深度 %d 超过上限 %d", depth, max)}, nil
	}

	// —— 3. 运行图总量配额: 全有或全无 (部分接纳会留下引用被丢弃节点的边) ——
	if !rc.reserveAll(len(sub.Nodes)) {
		return NodeResult{Status: NodeStatusFailed,
			Err: fmt.Sprintf("subgraph: 运行图节点总数将超过上限 %d (当前 %d, 本次 +%d)",
				rc.maxTotalNodes(), rc.usedNodes(), len(sub.Nodes))}, nil
	}

	ns := scope.prefix + node.ID + SubgraphIDInfix + SubgraphMemberIDSep
	rc.appendEv(EvSubgraphEntered, evID, scope.with(map[string]any{
		"graph": sub.Name, "version": sub.Version, "namespace": ns,
		"nodes": len(sub.Nodes), "result_from": resultFrom, "depth": depth,
		"source": source, "ignored_policies": ignoredSubgraphPolicies(sub.Policies),
		// 冻结当时执行的那张图: resume 照它重建 (见文件头三)。
		"subgraph": mustJSON(sub),
	}))

	// —— 4. 执行: 复用 scheduleDAG, 于是子图内的 join/条件边/重试/循环与顶层一致 ——
	subScope := execScope{
		prefix:  ns,
		nested:  true, // 成员是嵌套层叶子: 取 nestSem (与 loop-group/spawn 同一口径)
		depth:   scope.depth,
		sgDepth: depth,
		extra:   scope.with(map[string]any{"subgraph_of": node.ID, "graph": sub.Name}),
	}
	dr := newDagRun(sub.Nodes, sub.Edges, subScope)
	dr.basePrev = in.PrevOutputs // 父节点的上游产出对子图成员同样可见 (子图是容器不是隔离舱)
	if len(spec.Params) > 0 {
		dr.params = spec.Params
	}
	// resume: 用**限定 ID** 回填已完成的成员 (与 spawn 同一手法)。命名空间是节点 ID,
	// 于是"同一个 subgraph 节点的同一个成员"跨运行是同一个键, 缓存不会错配。
	cachedAll := true
	if rc.replay != nil {
		for _, n := range sub.Nodes {
			if r, ok := rc.replay.Completed[ns+n.ID]; ok {
				dr.state[n.ID] = r
			} else {
				cachedAll = false
			}
		}
	} else {
		cachedAll = false
	}

	cancelled := e.scheduleDAG(ctx, rc, dr)

	// —— 5. 汇总 ——
	extra := map[string]any{
		"subgraph": sub.Name, "nodes": len(sub.Nodes), "result_from": resultFrom,
		"source": source, "depth": depth, "cached": cachedAll,
	}
	// 成员挂起 ⇒ 整个 subgraph 节点挂起 (见文件头四)。**先判挂起再判产出**:
	// 产出节点可能已完成而另一条并行支路还在等人, 那时子图作为一个整体并没有跑完,
	// 报 completed 会让下游拿着"还没批完"的结果继续往下走。
	if len(dr.suspended) > 0 {
		ids := make([]string, 0, len(dr.suspended))
		for id := range dr.suspended {
			ids = append(ids, id)
		}
		sort.Strings(ids) // 确定性文案
		extra["suspended"] = strings.Join(ids, ",")
		return NodeResult{Status: NodeStatusSuspended,
			Err: fmt.Sprintf("subgraph: 成员 %s 挂起中 (答复/额度到位后 resume 续跑)", strings.Join(ids, ", "))}, extra
	}
	res, ok := dr.state[resultFrom]
	if !ok {
		reason := "未达终态"
		if cancelled {
			reason = "调度被取消"
		}
		return NodeResult{Status: NodeStatusFailed,
			Err: fmt.Sprintf("subgraph: 图 %q 的产出节点 %q %s", sub.Name, resultFrom, reason)}, extra
	}
	// 产出 1:1 取 ResultFrom 成员 (含 skipped —— "未走的分支不算失败"要在这一层同样
	// 成立, 否则子图里一条条件边就能把引用它的整张图判死, 与 loop-group 同一口径)。
	// **不透传 Expansion**: 成员的展开请求属于成员那一层 (它自己的 dagRun 已经处理过),
	// 冒名让父图接受它等于把展开归因错人。
	res.Expansion = nil
	res.Shards = nil
	return res, extra
}
