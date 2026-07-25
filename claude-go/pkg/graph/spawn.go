package graph

// spawn.go —— Subagent 派生 SpawnSubgraph (design/01 §4.8)。
//
// 改造前: subagent 是节点内 agent 自己造的裸 QueryEngine (`pkg/feishu/session.go`
// runNestedAgent、`cmd/claude-go/main.go`), 于是它**对编排层完全不可见** —— 没有
// NodeID、不进 Journal、不受 hook/预算/轨迹覆盖。一个节点可以在里面烧掉任意多
// token 而图这一层什么都看不到。
//
// 本文件让派生走编排层: 子图节点经**同一个** rc.nodeExec 执行, 于是
//   - 自动过同一条拦截器链 → 预算台账把子图的开销记在父运行头上 (§4.10);
//   - 自动进同一份 journal, NodeID 形如 <父>~sp<指纹>/<子> → 可归因、可重放;
//   - 自动受同一套 hook 生命周期覆盖。
// 这三样都不是额外写的, 是"复用 rc"白拿的 —— 也正是为什么派生必须回到编排层。
//
// ## 与动态展开 (§4.2 expand.go) 的分工
//
//	动态展开: planner 节点**跑完后**把分解结果并入当前 run 继续调度 (异步、改图)。
//	SpawnSubgraph: 节点**执行期间**由 agent 的工具调用派生, 同步等结果返回 (不改父图)。
//
// 两者边界闸的理由相同 (子图内容来自 LLM 产出), 故 spawn 复用 expand 的
// narrowToParent 做单调收窄, 不另写一套。
//
// ## 命名空间用请求内容指纹, 不用序号
//
// 用序号 (`~sp0`/`~sp1`) 会埋一个隐蔽的错: 父节点重跑时序号从 0 重新开始, 而
// agent 这次可能请求了**不同**的子图 —— 同一个 `~sp0/` 命名空间下, resume 的
// 缓存会把上一次的产出错配给这次的子图。用请求指纹后: 请求相同 → 命中缓存
// (省掉真金白银的 LLM 调用); 请求不同 → 命名空间不同, 不可能误命中。
//
// ## 授权是必须的
//
// 只有声明了 NodeSpec.Spawn 的节点才拿到 Spawner (否则 NodeInput.Spawn 为 nil)。
// 与 Expand 同一原则: 派生能力必须由图显式授予, 否则任何 runner 都能凭工具调用
// 往编排里塞执行单元, 等于把调度权交给模型。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SpawnIDInfix 派生子图命名空间的中缀: <父节点>~sp<指纹>/<子节点>。
const SpawnIDInfix = "~sp"

// SpawnMemberIDSep 派生命名空间与成员 ID 的分隔符。
const SpawnMemberIDSep = "/"

// 派生边界的缺省值。
const (
	DefaultSpawnMaxDepth  = 1 // 子图里的节点默认不能再派生
	DefaultSpawnMaxNodes  = 8 // 单次派生的节点数上限
	DefaultSpawnMaxSpawns = 4 // 一个节点一次执行里最多派生几次
)

// SpawnSpec 派生授权与边界 (design/01 §4.8)。声明它即授予该节点派生能力。
type SpawnSpec struct {
	// MaxDepth 派生深度 (父→子→孙…), 0 → DefaultSpawnMaxDepth。
	// 挡住"子代理再派生子代理"的递归膨胀 —— 这是最容易失控的一维, 因为每一层
	// 看起来都只多派生了一点。
	MaxDepth int `json:"max_depth,omitempty"`
	// MaxNodes 单次派生的节点数上限, 0 → DefaultSpawnMaxNodes。
	MaxNodes int `json:"max_nodes,omitempty"`
	// MaxSpawns 一个节点的一次执行里最多派生几次, 0 → DefaultSpawnMaxSpawns。
	// 与 MaxNodes 是两道不同的闸: 前者挡"派生 500 次每次 1 个节点"。
	MaxSpawns int `json:"max_spawns,omitempty"`
}

func (s *SpawnSpec) maxDepth() int {
	if s == nil || s.MaxDepth <= 0 {
		return DefaultSpawnMaxDepth
	}
	return s.MaxDepth
}

func (s *SpawnSpec) maxNodes() int {
	if s == nil || s.MaxNodes <= 0 {
		return DefaultSpawnMaxNodes
	}
	return s.MaxNodes
}

func (s *SpawnSpec) maxSpawns() int {
	if s == nil || s.MaxSpawns <= 0 {
		return DefaultSpawnMaxSpawns
	}
	return s.MaxSpawns
}

// SpawnRequest 一次派生请求 (由节点内 agent 的工具调用构造)。
type SpawnRequest struct {
	// Nodes/Edges 子图。节点 ID 由调用方 (最终是 LLM) 给出, 会被命名空间化。
	Nodes []NodeSpec `json:"nodes"`
	Edges []EdgeSpec `json:"edges,omitempty"`
	// ResultFrom 取哪个成员的产出作为派生结果。空 = 唯一出度 0 节点;
	// 有多个出度 0 节点时**必须显式声明** —— 否则"结果是谁"取决于声明顺序,
	// 是个隐蔽的不确定性 (与 loop-group 的 ResultFrom 同一口径)。
	ResultFrom string `json:"result_from,omitempty"`
	// Params 追加的图参数 (与父运行的 Params 合并, 同名以本请求为准)。
	Params map[string]string `json:"params,omitempty"`
}

// SpawnResult 一次派生的结果。
type SpawnResult struct {
	// Status/Output/Score 取 ResultFrom 成员的结果。
	Status string  `json:"status"`
	Output string  `json:"output,omitempty"`
	Score  float64 `json:"score,omitempty"`
	// Namespace 本次派生在 journal 里的命名空间前缀 (调用方据此对齐自己的记录)。
	Namespace string `json:"namespace"`
	// Nodes 全部成员的结果 (键为**未**命名空间化的成员 ID, 便于调用方按自己给的 ID 取)。
	Nodes map[string]NodeResult `json:"nodes,omitempty"`
	// Cached true = 本次派生的成员全部命中 resume 缓存, 未真正执行 (零 LLM)。
	Cached bool `json:"cached,omitempty"`
}

// Spawner 节点内 agent 派生子图的入口 (design/01 §4.8)。
// 只有声明了 NodeSpec.Spawn 的节点会在 NodeInput.Spawn 拿到非 nil 实现。
type Spawner interface {
	Spawn(ctx context.Context, req SpawnRequest) (SpawnResult, error)
}

// 派生被拒的原因 (都是 fail-closed: 拒绝而不是"尽力跑一部分")。
var (
	ErrSpawnNotAuthorized = errors.New("graph: 本节点未声明 spawn, 无派生权限")
	ErrSpawnTooDeep       = errors.New("graph: 派生深度超限")
	ErrSpawnTooMany       = errors.New("graph: 派生次数超限")
	ErrSpawnTooLarge      = errors.New("graph: 单次派生节点数超限")
	ErrSpawnNoRoom        = errors.New("graph: 运行图节点总量配额耗尽")
)

// nodeSpawner 绑定到一个具体节点执行的 Spawner 实现。
type nodeSpawner struct {
	e      *Engine
	rc     *runCtx
	scope  execScope
	parent NodeSpec
	in     NodeInput

	mu     sync.Mutex
	spawns int
	seen   map[string]SpawnResult // 指纹 → 结果 (同一次执行里重复请求直接复用)
}

// newNodeSpawner 为一次节点执行造 Spawner; 节点未声明 Spawn 时返回 nil
// (让 NodeInput.Spawn 保持 nil —— 授权缺失表现为"没有这个能力", 而不是调了才报错)。
func newNodeSpawner(e *Engine, rc *runCtx, scope execScope, node NodeSpec, in NodeInput) Spawner {
	if node.Spawn == nil {
		return nil
	}
	return &nodeSpawner{e: e, rc: rc, scope: scope, parent: node, in: in, seen: map[string]SpawnResult{}}
}

// Spawn 见 Spawner。
func (s *nodeSpawner) Spawn(ctx context.Context, req SpawnRequest) (SpawnResult, error) {
	spec := s.parent.Spawn
	fp := spawnFingerprint(req)

	// 同一次节点执行里请求过同样的子图 → 直接复用, 不重复烧 LLM。
	s.mu.Lock()
	if prev, ok := s.seen[fp]; ok {
		s.mu.Unlock()
		return prev, nil
	}
	if s.spawns >= spec.maxSpawns() {
		s.mu.Unlock()
		s.reject(fp, "max_spawns", fmt.Sprintf("上限 %d", spec.maxSpawns()))
		return SpawnResult{}, fmt.Errorf("%w: 上限 %d", ErrSpawnTooMany, spec.maxSpawns())
	}
	s.spawns++
	s.mu.Unlock()

	// —— 边界闸 ——
	if s.scope.depth >= spec.maxDepth() {
		s.reject(fp, "max_depth", fmt.Sprintf("上限 %d, 当前 %d", spec.maxDepth(), s.scope.depth))
		return SpawnResult{}, fmt.Errorf("%w: 上限 %d, 当前深度 %d", ErrSpawnTooDeep, spec.maxDepth(), s.scope.depth)
	}
	if len(req.Nodes) == 0 {
		return SpawnResult{}, errors.New("graph: 派生请求为空 (无节点)")
	}
	if len(req.Nodes) > spec.maxNodes() {
		s.reject(fp, "max_nodes", fmt.Sprintf("上限 %d, 请求 %d", spec.maxNodes(), len(req.Nodes)))
		return SpawnResult{}, fmt.Errorf("%w: 上限 %d, 请求 %d", ErrSpawnTooLarge, spec.maxNodes(), len(req.Nodes))
	}

	// —— 校验 + 单调收窄 (复用 expand 的实现, 不另写一套) ——
	sub, resultFrom, err := prepareSpawn(s.parent, req, s.scope.depth, spec.maxDepth())
	if err != nil {
		s.reject(fp, "invalid", err.Error())
		return SpawnResult{}, err
	}

	// 运行图总量配额: 全有或全无 —— 部分接纳会留下引用被丢弃节点的边, 得到断图。
	if !s.rc.reserveAll(len(sub.Nodes)) {
		s.reject(fp, "max_total_nodes", fmt.Sprintf("已用 %d", s.rc.usedNodes()))
		return SpawnResult{}, fmt.Errorf("%w: 已用 %d", ErrSpawnNoRoom, s.rc.usedNodes())
	}

	// —— 执行: 复用 scheduleDAG, 于是组内 OR-join/条件边/重试/循环语义与顶层一致 ——
	ns := s.scope.prefix + s.parent.ID + SpawnIDInfix + fp + SpawnMemberIDSep
	subScope := execScope{
		prefix: ns,
		nested: true, // 子图叶子取 nestSem (与 loop-group 组内同一口径)
		depth:  s.scope.depth + 1,
		extra:  s.scope.with(map[string]any{"spawned_by": s.parent.ID, "spawn": fp}),
	}
	dr := newDagRun(sub.Nodes, sub.Edges, subScope)
	dr.basePrev = s.in.PrevOutputs // 父节点的上游产出对子图同样可见
	if len(req.Params) > 0 {
		dr.params = req.Params
	}

	// resume: 用**限定 ID** 从重放状态里回填已完成的成员。
	// 指纹在命名空间里 ⇒ 只有请求逐字节相同才可能命中, 不存在错配。
	cachedAll := len(sub.Nodes) > 0
	if s.rc.replay != nil {
		for _, n := range sub.Nodes {
			if r, ok := s.rc.replay.Completed[ns+n.ID]; ok {
				dr.state[n.ID] = r
			} else {
				cachedAll = false
			}
		}
	} else {
		cachedAll = false
	}

	s.rc.appendEv(EvSubgraphSpawned, s.scope.evID(s.parent.ID), s.scope.with(map[string]any{
		"spawn": fp, "namespace": ns, "nodes": len(sub.Nodes),
		"result_from": resultFrom, "depth": subScope.depth, "subgraph": mustJSON(sub),
	}))

	cancelled := s.e.scheduleDAG(ctx, s.rc, dr)

	out := SpawnResult{Namespace: ns, Nodes: map[string]NodeResult{}, Cached: cachedAll}
	for id, r := range dr.state {
		out.Nodes[id] = r
	}
	res, ok := dr.state[resultFrom]
	if !ok {
		reason := "未达终态"
		if cancelled {
			reason = "调度被取消"
		}
		res = NodeResult{Status: NodeStatusFailed,
			Err: fmt.Sprintf("graph: 派生子图产出节点 %q %s", resultFrom, reason)}
	}
	out.Status, out.Output, out.Score = res.Status, res.Output, res.Score

	s.mu.Lock()
	s.seen[fp] = out
	s.mu.Unlock()
	return out, nil
}

// reject 记一条 subgraph.rejected —— 没有它, "agent 说它派生了但图里什么都没有"
// 这件事在事后完全不可解释 (与 graph.expand_rejected 同一理由)。
func (s *nodeSpawner) reject(fp, kind, detail string) {
	s.rc.appendEv(EvSubgraphRejected, s.scope.evID(s.parent.ID), s.scope.with(map[string]any{
		"spawn": fp, "kind": kind, "detail": detail,
	}))
}

// prepareSpawn 校验子图 + 命名空间无关的结构检查 + 单调收窄。
// 返回 (收窄后的子图, 产出节点 ID)。节点 ID 保持**未**命名空间化 ——
// 命名空间体现在 execScope.prefix 上, 与 loop-group 一致 (expand 走的是另一条
// 路: 它把子图并入父图, 故必须把 ID 写成 <父>/<子>)。
func prepareSpawn(parent NodeSpec, req SpawnRequest, depth, maxDepth int) (Expansion, string, error) {
	local := map[string]bool{}
	for _, n := range req.Nodes {
		id := strings.TrimSpace(n.ID)
		if id == "" {
			return Expansion{}, "", errors.New("graph: 派生子图含空节点 ID")
		}
		if local[id] {
			return Expansion{}, "", fmt.Errorf("graph: 派生子图节点 ID 重复: %q", id)
		}
		local[id] = true
	}

	out := Expansion{Nodes: make([]NodeSpec, 0, len(req.Nodes))}
	for _, n := range req.Nodes {
		n.ID = strings.TrimSpace(n.ID)
		// 单调收窄: 子图只继承或收紧父节点约束 (复用 expand 的实现)。
		out.Nodes = append(out.Nodes, narrowToParent(parent, n, depth, maxDepth))
	}
	for _, ed := range req.Edges {
		from, to := strings.TrimSpace(ed.From), strings.TrimSpace(ed.To)
		if !local[from] || !local[to] {
			// 派生子图是独立执行单元, 边只能连子图内部 —— 允许指向父图节点会让
			// "同步等结果"的语义崩掉 (要等的东西可能还没跑)。
			return Expansion{}, "", fmt.Errorf("graph: 派生子图的边端点必须都在子图内: %s→%s", ed.From, ed.To)
		}
		ed.From, ed.To = from, to
		out.Edges = append(out.Edges, ed)
	}

	// 结构合法性: 用一次性 GraphSpec 走同一套 Validate, 不另写检查
	// (另写必然与顶层校验漂移)。
	// nested=true 而 insideGroup 仍为 false: 前者禁掉挂起与子图引用 (派生是 agent 的
	// **同步**调用, 一个进行中的 LLM 会话没法挂起几小时; 且请求内容来自 LLM),
	// 后者保持原样 —— 派生子图内嵌 loop-group 是既有能力, 不在本次改动范围。
	probe := GraphSpec{Name: "spawn:" + parent.ID, Nodes: out.Nodes, Edges: out.Edges}
	if err := probe.validate(validateCtx{nested: true}); err != nil {
		return Expansion{}, "", fmt.Errorf("graph: 派生子图非法: %w", err)
	}

	resultFrom := strings.TrimSpace(req.ResultFrom)
	if resultFrom == "" {
		sinks := subgraphSinks(out.Nodes, out.Edges)
		if len(sinks) != 1 {
			return Expansion{}, "", fmt.Errorf(
				"graph: 派生子图有 %d 个出度 0 节点, 必须显式声明 result_from (否则结果取决于声明顺序)", len(sinks))
		}
		resultFrom = sinks[0]
	} else if !local[resultFrom] {
		return Expansion{}, "", fmt.Errorf("graph: result_from %q 不在派生子图内", resultFrom)
	}
	return out, resultFrom, nil
}

// subgraphSinks 出度 0 的节点 (按 ID 升序, 保证判定确定性)。
func subgraphSinks(nodes []NodeSpec, edges []EdgeSpec) []string {
	hasOut := map[string]bool{}
	for _, ed := range edges {
		hasOut[ed.From] = true
	}
	var out []string
	for _, n := range nodes {
		if !hasOut[n.ID] {
			out = append(out, n.ID)
		}
	}
	sort.Strings(out)
	return out
}

// spawnFingerprint 请求内容指纹 (命名空间用)。
//
// 只取影响"跑出来是什么"的字段: 节点 (ID/Kind/角色/prompt/策略) 与边、ResultFrom、
// Params。刻意**不含**序号或时间 —— 那会让相同请求每次落在不同命名空间, resume
// 永远命中不了缓存, 每次崩溃重跑都要重烧一遍子图的 LLM。
func spawnFingerprint(req SpawnRequest) string {
	h := sha256.New()
	// 节点按 ID 排序后再喂: 同一子图因声明顺序不同而算出不同指纹会白丢缓存。
	idx := make([]int, 0, len(req.Nodes))
	for i := range req.Nodes {
		idx = append(idx, i)
	}
	sort.SliceStable(idx, func(a, b int) bool { return req.Nodes[idx[a]].ID < req.Nodes[idx[b]].ID })
	for _, i := range idx {
		fmt.Fprintf(h, "n|%s\n", mustJSON(req.Nodes[i]))
	}
	eidx := make([]int, 0, len(req.Edges))
	for i := range req.Edges {
		eidx = append(eidx, i)
	}
	sort.SliceStable(eidx, func(a, b int) bool {
		if req.Edges[eidx[a]].From != req.Edges[eidx[b]].From {
			return req.Edges[eidx[a]].From < req.Edges[eidx[b]].From
		}
		return req.Edges[eidx[a]].To < req.Edges[eidx[b]].To
	})
	for _, i := range eidx {
		fmt.Fprintf(h, "e|%s\n", mustJSON(req.Edges[i]))
	}
	fmt.Fprintf(h, "r|%s\n", req.ResultFrom)
	keys := make([]string, 0, len(req.Params))
	for k := range req.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "p|%s=%s\n", k, req.Params[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}
