package agent

// graph_adapter — WorkflowDef→GraphSpec 直译器 + 图引擎接入 (design/01 §五 P1.4)。
//
// 迁移策略 (strangler): 图引擎不重写 stage 执行, 而是经 stageNodeRunner 复用
// WorkflowExecutor.ExecuteSingleStage 的全部既有能力 (角色 prompt 组装/黑板交接/
// 进化注入/重试/超时) —— 图引擎只接管**调度与恢复** (ready-set 并行 + Journal
// 事件溯源取代 checkpoints.json)。
//
// 接入两路:
//  1. wf.Mode == "graph": 显式图模式 (动态工作流可直接声明)
//  2. CLAUDE_GO_GRAPH_ENGINE=1: pipeline/fanout 全量切图引擎 (灰度开关,
//     行为等价验收后成默认, design/01 M1)
//
// ---------------------------------------------------------------------------
// 图能力如何在生产输入上被表达 (本文件的第二职责)
// ---------------------------------------------------------------------------
//
// StageDef 只有 Name/Role/Prompt/DependsOn/Parallel 五个字段, 于是**条件边 /
// 节点重试 / 超时 / Loop / 工具画像 / MaxTurns / Deterministic 这些图能力在生产
// 输入上无法被任何输入表达**: pkg/graph 里实现完备且有测试, 直译器却恒填零值,
// 生产图永远是"无条件边 + maxRetries=0 + TimeoutSec=0 + 无 Loop"。本文件用两条
// 路补上, 两条都不需要改 workflow.go:
//
//  1. **从 StageDef 已有信息推导**: 角色 → 节点总预算 (复用 computeStageTimeout
//     这一份角色超时真源); 门禁关键词 → gate 形态与 Deterministic; 工作流元数据
//     → GraphMeta; 图级默认重试策略 (见下文"重试让位")。
//  2. **per-workflow 图覆盖表** (WorkflowGraphOverride): 按 工作流名 + 阶段名
//     声明条件边 / Loop / 重试 / 超时 / 工具画像 / MaxTurns。两个注入入口:
//     进程内 RegisterGraphOverride (内置或动态工作流注册时一并声明),
//     以及 CLAUDE_GO_GRAPH_OVERRIDES=<file.json> (免重编译, 运维可注入)。
//
// **条件边只接受显式声明, 绝不从阶段名猜**: 从名字推断路由等于把 design/01 明确
// 反对的"按工作流名白名单"搬到调度层; 而且猜错的代价是静默跳过整条分支
// (skipped 不是 failed, 日志上不显眼), 排查成本远高于收益。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/trace"
)

// ---------------------------------------------------------------------------
// 重试单层化: 谁重试, 谁让位 (design/01 §4.3)
// ---------------------------------------------------------------------------
//
// pipeline 路径的既有形态是两层: Coordinator.executeStageWithRetry (MaxRetries,
// 生产取 6 见 teams.go) 在外, WorkflowExecutor.executeStageWithRetry (3 次 + 429
// 限流 20 次) 在内; 内层看到 ctx 上的 outerRetryDriven 标记后退化 (effMaxRetries=1
// 只保留输出校验的快速修正, 限流耐心降到 3), 以此消除两层相乘 (design/01 §1.2-4)。
//
// 图路径此前**两层都没接**: 直译器不填 Retry 也不填 Policies.DefaultRetry ⇒ 图层
// maxRetries=0, 图的 RetryPolicy/指数退避/journal node.retried 全部空转; 而
// stageNodeRunner 又没打 outerRetryDriven 标记 ⇒ 真正在重试的是内层的 3+20。
//
// 本次确定的语义 (与 pipeline 同构, 便于灰度期直接对比):
//   - **图层负责重试**: 直译器给出图级 DefaultRetry, 次数与 pipeline 侧
//     Coordinator 的生产值 (6) 对齐, 退避基数 5s (指数 5/10/20/40/80/160s);
//   - **内层让位**: stageNodeRunner 用 WithOuterRetryDriven 标记 ctx, 内层退化为
//     "1 次 + 1 次输出校验修正";
//   - 于是总尝试数 = 图层 (1+6) × 内层 (≤2), 与 pipeline 路径同量级——不是新的
//     乘法放大, 也不是比 pipeline 更少的重试。429 限流耐心同理: 内层退化后每次
//     尝试仍等 3 轮 (每轮 ≥30s), 乘上图层 7 次尝试与图层退避, 与 pipeline 的
//     Coordinator(6, 限流退避 15s 基数) 量级相当, 不会因为让位而变得不耐烦。
//   - 覆盖表可按工作流/阶段改次数与退避; 图层 MaxRetries 设 0 即"完全不重试"
//     (此时内层仍是退化态, 语义清晰: 重试只存在于图层)。
const (
	graphOuterMaxRetries = 6 // 图层重试次数 (与 Coordinator 生产值对齐)
	graphRetryBackoffSec = 5 // 图层指数退避基数 (秒)
)

// 节点总预算 (NodeSpec.TimeoutSec) 的推导常量。
const (
	// graphNodeBudgetCapSec 单节点墙钟硬上限 (2h): 只作卡死兜底。
	graphNodeBudgetCapSec = 2 * 60 * 60
	// graphDeterministicBudgetSec 纯代码节点 (Deterministic gate) 预算: 零 LLM, 秒级完成。
	graphDeterministicBudgetSec = 120
	// graphInnerAttemptFactor 内层"1 次 + 1 次校验修正"= 每个图层尝试最多两次 LLM 调用。
	graphInnerAttemptFactor = 2
)

// graphNodeBudgetSec 推导节点总预算 (秒)。
//
// 必须分清两种超时, 否则一改就把重试掐死:
//   - **单次尝试**的角色级超时 (coder 25min / researcher 6min / ...) 早已由内层
//     ExecuteSingleStage 施加 (workflow.go computeStageTimeout), 图模式一样生效;
//   - 引擎的 NodeSpec.TimeoutSec 包住**整个节点** (retry 环 + loop 环, 见
//     engine.go execNode), 属于"总预算"。
//
// 所以这里不能直接填单次超时——那会让第二次重试必然撞 deadline。预算 =
// 单次角色超时 × 图层尝试数 × 内层尝试因子, 再夹到 2h。它的作用是 runner 不响应
// ctx / 外层无 deadline 时的最后一道闸 (pipeline 侧连这道闸都没有), 不是省钱闸。
func graphNodeBudgetSec(role string, retries int, deterministic bool) int {
	if deterministic {
		return graphDeterministicBudgetSec
	}
	if retries < 0 {
		retries = 0
	}
	// 复用 computeStageTimeout 这一份角色超时真源 (它只读 role/attempt, 零值 executor 足够),
	// 而不是在这里另抄一张角色→超时表——两张表必然漂移。
	per := (&WorkflowExecutor{}).computeStageTimeout(role, 0)
	budget := int(per.Seconds()) * (retries + 1) * graphInnerAttemptFactor
	if budget > graphNodeBudgetCapSec {
		budget = graphNodeBudgetCapSec
	}
	if budget <= 0 {
		budget = graphDeterministicBudgetSec
	}
	return budget
}

// ---------------------------------------------------------------------------
// per-workflow 图能力覆盖表
// ---------------------------------------------------------------------------

// StageGraphOverride 单个阶段的图能力声明 (JSON 可序列化, 供外部文件注入)。
// 全部字段可选; 零值 = 按推导规则处理。
type StageGraphOverride struct {
	// Kind 强制节点形态 ""|"agent"|"gate"。显式 "agent" 可压制 stageIsGate 的
	// 关键词推断 (例如名字里带"门禁"但其实是普通写作阶段)。
	Kind string `json:"kind,omitempty"`
	// Condition 本阶段**全部入边**的条件 (语法见 pkg/graph/condition.go:
	// ok | fail | score <op> <数字> | output contains "..." | output not_contains "...")。
	Condition string `json:"condition,omitempty"`
	// EdgeConditions 精确到上游阶段名的入边条件, 优先于 Condition。
	// 典型用法: {"compile-gate": "score >= 75"} 与 {"compile-gate": "score < 75"}
	// 分挂在"继续交付"与"返工"两个下游阶段上, 形成评分路由。
	EdgeConditions map[string]string `json:"edgeConditions,omitempty"`
	// Retry 节点级重试策略 (覆盖图级 DefaultRetry)。
	Retry *graph.RetryPolicy `json:"retry,omitempty"`
	// TimeoutSec 节点总预算 (0=按角色推导, 见 graphNodeBudgetSec)。
	TimeoutSec int `json:"timeoutSec,omitempty"`
	// Loop 节点级循环 (max_iterations 必填 >0; until 条件与 feedback 模板)。
	Loop *graph.LoopPolicy `json:"loop,omitempty"`
	// ToolProfile 显式工具画像, 取代宿主按角色名子串猜 (design/01 §4.1)。
	// 经 NodeExecHints 下传给 CreateAgentFunc 实现消费。
	ToolProfile string `json:"toolProfile,omitempty"`
	// MaxTurns 覆盖本节点的模型最大轮数 (0=不覆盖)。
	MaxTurns int `json:"maxTurns,omitempty"`
	// Deterministic 声明本节点为纯代码判定 (gate 节点走 runGate 的确定性分支, 零 LLM)。
	Deterministic bool `json:"deterministic,omitempty"`
}

// WorkflowGraphOverride 一个工作流的图能力声明。
type WorkflowGraphOverride struct {
	MaxParallel  int                           `json:"maxParallel,omitempty"`  // 图级并发上限 (0=用 executor 的 effectiveParallel)
	DefaultRetry *graph.RetryPolicy            `json:"defaultRetry,omitempty"` // 图级默认重试 (nil=用 graphOuterMaxRetries)
	Stages       map[string]StageGraphOverride `json:"stages,omitempty"`       // 按阶段名
}

var (
	graphOverrideMu   sync.RWMutex
	graphOverrideTbl  = map[string]WorkflowGraphOverride{}
	graphOverrideOnce sync.Once
	graphOverrideErr  error
)

// RegisterGraphOverride 注册 (或整体替换) 一个工作流的图能力声明。并发安全。
func RegisterGraphOverride(workflow string, ov WorkflowGraphOverride) {
	name := strings.TrimSpace(workflow)
	if name == "" {
		return
	}
	graphOverrideMu.Lock()
	graphOverrideTbl[name] = ov
	graphOverrideMu.Unlock()
}

// UnregisterGraphOverride 移除一个工作流的图能力声明 (动态工作流被删除时应同时清理)。
func UnregisterGraphOverride(workflow string) {
	graphOverrideMu.Lock()
	delete(graphOverrideTbl, strings.TrimSpace(workflow))
	graphOverrideMu.Unlock()
}

// LookupGraphOverride 读取某工作流的图能力声明 (第二返回值 = 是否声明过)。
// 首次调用时懒加载 CLAUDE_GO_GRAPH_OVERRIDES 指向的文件。
func LookupGraphOverride(workflow string) (WorkflowGraphOverride, bool) {
	graphOverrideOnce.Do(loadGraphOverridesFromEnv)
	graphOverrideMu.RLock()
	defer graphOverrideMu.RUnlock()
	ov, ok := graphOverrideTbl[strings.TrimSpace(workflow)]
	return ov, ok
}

// LoadGraphOverridesJSON 从 JSON 载入覆盖表, 形如
// {"<工作流名>": {"maxParallel":2, "stages": {"<阶段名>": {...}}}}。
// 同名工作流整体替换。
func LoadGraphOverridesJSON(data []byte) error {
	var tbl map[string]WorkflowGraphOverride
	if err := json.Unmarshal(data, &tbl); err != nil {
		return fmt.Errorf("graph_adapter: 图覆盖表 JSON 解析失败: %w", err)
	}
	graphOverrideMu.Lock()
	for name, ov := range tbl {
		if n := strings.TrimSpace(name); n != "" {
			graphOverrideTbl[n] = ov
		}
	}
	graphOverrideMu.Unlock()
	return nil
}

// GraphOverrideLoadError 返回 CLAUDE_GO_GRAPH_OVERRIDES 的加载错误 (nil=无错)。
// 加载是 fail-open 的 (坏文件不该让团队起不来), 但错误必须能被 CLI/dashboard 看到,
// 不能静默吞掉——否则运维改了覆盖表却发现条件边没生效, 无从定位。
func GraphOverrideLoadError() error {
	graphOverrideOnce.Do(loadGraphOverridesFromEnv)
	graphOverrideMu.RLock()
	defer graphOverrideMu.RUnlock()
	return graphOverrideErr
}

// stageDeclaredOverride 读取 StageDef **自身**声明的图能力。
//
// 现在恒返回零值 —— StageDef 只有五个字段 (定义在 workflow.go, 不在本次改动范围)。
// 这里是唯一的接线口: 一旦给 StageDef 补上字段 (建议 json 名与 StageGraphOverride
// 保持一致: kind/condition/edgeConditions/retry/timeoutSec/loop/toolProfile/
// maxTurns/deterministic), 只需在此把字段搬进 StageGraphOverride —— 直译器与
// "覆盖表优先"的合并规则一行都不用改, 动态工作流的 JSON 也就自动能表达图能力。
func stageDeclaredOverride(_ StageDef) StageGraphOverride {
	return StageGraphOverride{}
}

// mergeStageOverride 合并"阶段自身声明"(decl) 与"覆盖表声明"(ov), **覆盖表优先**。
// 覆盖表是运维/灰度期的最后一跳 (可经 JSON 热注入), 必须压得住工作流定义里的声明;
// 逐字段合并而非整体替换, 是为了让覆盖表只写要改的那一项 (例如只补一条条件边)。
func mergeStageOverride(decl, ov StageGraphOverride) StageGraphOverride {
	out := decl
	if ov.Kind != "" {
		out.Kind = ov.Kind
	}
	if ov.Condition != "" {
		out.Condition = ov.Condition
	}
	if len(ov.EdgeConditions) > 0 || len(decl.EdgeConditions) > 0 {
		merged := make(map[string]string, len(decl.EdgeConditions)+len(ov.EdgeConditions))
		for k, v := range decl.EdgeConditions { // 不改入参的 map: 两侧都可能被复用
			merged[k] = v
		}
		for k, v := range ov.EdgeConditions {
			merged[k] = v
		}
		out.EdgeConditions = merged
	}
	if ov.Retry != nil {
		out.Retry = ov.Retry
	}
	if ov.TimeoutSec > 0 {
		out.TimeoutSec = ov.TimeoutSec
	}
	if ov.Loop != nil {
		out.Loop = ov.Loop
	}
	if ov.ToolProfile != "" {
		out.ToolProfile = ov.ToolProfile
	}
	if ov.MaxTurns > 0 {
		out.MaxTurns = ov.MaxTurns
	}
	if ov.Deterministic {
		out.Deterministic = true
	}
	return out
}

// loadGraphOverridesFromEnv 懒加载环境变量指向的覆盖表文件 (只跑一次)。
func loadGraphOverridesFromEnv() {
	path := strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_OVERRIDES"))
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err == nil {
		err = LoadGraphOverridesJSON(data)
	}
	if err != nil {
		graphOverrideMu.Lock()
		graphOverrideErr = fmt.Errorf("graph_adapter: 加载 %s 失败: %w", path, err)
		graphOverrideMu.Unlock()
		logging.For("agent").Warn("图覆盖表加载失败, 按无覆盖继续", "path", path, "err", err)
	}
}

// ---------------------------------------------------------------------------
// 直译: WorkflowDef → GraphSpec
// ---------------------------------------------------------------------------

// TranslateWorkflow 把 WorkflowDef 直译为 GraphSpec (Stages/DependsOn 一一对应),
// 并补齐 StageDef 表达不出的图能力: 图级默认重试 + 角色级节点预算 + gate 形态,
// 以及覆盖表声明的条件边 / Loop / 工具画像 / MaxTurns。
// Parallel 字段无需翻译 —— ready-set 调度天然并行无依赖节点。
func TranslateWorkflow(wf *WorkflowDef) (graph.GraphSpec, error) {
	if wf == nil || len(wf.Stages) == 0 {
		return graph.GraphSpec{}, fmt.Errorf("graph_adapter: 空工作流")
	}
	ov, _ := LookupGraphOverride(wf.Name)
	defaultRetry := ov.DefaultRetry
	if defaultRetry == nil {
		// 图层接过重试职责 (见文件头"重试单层化"), 内层由 stageNodeRunner 打标退化。
		defaultRetry = &graph.RetryPolicy{MaxRetries: graphOuterMaxRetries, BackoffSec: graphRetryBackoffSec}
	}
	spec := graph.GraphSpec{
		Name:    wf.Name,
		Version: "wfdef-v1",
		Meta:    WorkflowGateMeta(wf),
		Policies: graph.GraphPolicies{
			MaxParallel:  ov.MaxParallel, // 0 → executeGraph 用 effectiveParallel 兜
			DefaultRetry: defaultRetry,
		},
	}
	for _, st := range wf.Stages {
		// 阶段自身声明 (StageDef 补字段后即生效) + 覆盖表 (后者优先)。
		so := mergeStageOverride(stageDeclaredOverride(st), ov.Stages[st.Name])

		kind := graph.NodeKindAgent
		switch strings.ToLower(strings.TrimSpace(so.Kind)) {
		case string(graph.NodeKindAgent):
			// 显式声明普通 agent: 压制关键词推断
		case string(graph.NodeKindGate):
			kind = graph.NodeKindGate
		case "":
			if stageIsGate(st) {
				kind = graph.NodeKindGate // 门禁阶段 → gate 节点 (输出 score 供条件边路由)
			}
		default:
			return graph.GraphSpec{}, fmt.Errorf("graph_adapter: 阶段 %q 的覆盖 kind=%q 非法 (仅 agent|gate)", st.Name, so.Kind)
		}

		// Deterministic: 显式声明优先; 否则 compile/test/build 类门禁本来就走 runGate
		// 的纯代码分支, 这里把它显式化, 使字段与实际行为一致 (也让预算按秒级给)。
		deterministic := so.Deterministic || (kind == graph.NodeKindGate && gateIsDeterministic(st.Name, st.Role))

		retries := defaultRetry.MaxRetries
		if so.Retry != nil {
			retries = so.Retry.MaxRetries
		}
		timeoutSec := so.TimeoutSec
		if timeoutSec <= 0 {
			timeoutSec = graphNodeBudgetSec(st.Role, retries, deterministic)
		}

		spec.Nodes = append(spec.Nodes, graph.NodeSpec{
			ID:   st.Name,
			Kind: kind,
			Agent: graph.AgentSpec{
				Role:          st.Role,
				Prompt:        st.Prompt,
				ToolProfile:   so.ToolProfile,
				MaxTurns:      so.MaxTurns,
				Deterministic: deterministic,
			},
			Loop:       so.Loop,
			Retry:      so.Retry,
			TimeoutSec: timeoutSec,
		})
		for _, dep := range st.DependsOn {
			cond := so.Condition
			if c, ok := so.EdgeConditions[dep]; ok {
				cond = c // 精确到上游的声明优先于本阶段的统一条件
			}
			spec.Edges = append(spec.Edges, graph.EdgeSpec{From: dep, To: st.Name, Condition: cond})
		}
	}
	if err := spec.Validate(); err != nil {
		return graph.GraphSpec{}, fmt.Errorf("graph_adapter: 直译结果非法: %w", err)
	}
	return spec, nil
}

// WorkflowGateMeta 组装图的声明式门禁元数据 (design/01 §4.1: 门禁凭元数据,
// 不凭工作流名白名单)。
//
// 动态工作流读自己声明的字段; 内置工作流回落到既有的名字判定
// (teams.go workflowProducesCode / content_gate.go contentQualityGated) ——
// 于是同一份 GraphMeta 对内置与动态工作流都成立, 门禁侧可以只读它。
// 直译器此前只填 wf.ProducesCode/wf.QualityGate, 对内置工作流恒为零值:
// "development" 这种真产码的工作流在 GraphMeta 里居然 ProducesCode=false,
// 谁真去读这份元数据就会静默丢门禁。
func WorkflowGateMeta(wf *WorkflowDef) graph.GraphMeta {
	if wf == nil {
		return graph.GraphMeta{QualityGate: "none"}
	}
	m := graph.GraphMeta{
		ProducesCode: wf.ProducesCode,
		QualityGate:  strings.ToLower(strings.TrimSpace(wf.QualityGate)),
	}
	if !m.ProducesCode {
		m.ProducesCode = workflowProducesCode(wf.Name)
	}
	if m.QualityGate == "" {
		if contentQualityGated(wf.Name) {
			m.QualityGate = "content"
		} else {
			m.QualityGate = "none"
		}
	}
	return m
}

// WorkflowGateMetaByName 按工作流名取门禁元数据, 供门禁侧 (teams.go /
// content_gate.go) 以元数据取代名字白名单。未注册的名字退回名字判定,
// 与现状一致 (历史团队 / 已删工作流不会因此丢门禁)。
func WorkflowGateMetaByName(workflow string) graph.GraphMeta {
	if wf := GetWorkflow(workflow); wf != nil {
		return WorkflowGateMeta(wf)
	}
	m := graph.GraphMeta{ProducesCode: workflowProducesCode(workflow), QualityGate: "none"}
	if contentQualityGated(workflow) {
		m.QualityGate = "content"
	}
	return m
}

// ---------------------------------------------------------------------------
// 节点执行: 复用 ExecuteSingleStage
// ---------------------------------------------------------------------------

// NodeExecHints 图节点声明的执行参数 —— AgentSpec 中"引擎不解释、由 runner 或宿主
// 消费"的那几个字段 (design/01 §4.1)。
//
// 为什么走 context: 消费点在 CreateAgentFunc 的实现方 (如 pkg/feishu 的
// SessionManager.CreateAgentRunner, 它目前按角色名子串猜工具画像), 而 StageDef 与
// ExecuteSingleStage 的签名不在本次改动范围内。图层把节点声明放进 ctx, 宿主读到
// 后即可用**显式声明**取代按名字猜; 读不到就保持现状 (fail-open, 零行为变化)。
type NodeExecHints struct {
	Node          string // 节点 ID
	Role          string
	Kind          string // agent|gate
	ToolProfile   string // 显式工具画像 (空=宿主按角色推断, 即现状)
	MaxTurns      int    // 0=不覆盖
	Deterministic bool
	Iteration     int // loop 轮次 (0 起)
}

type nodeExecHintsKey struct{}

// WithNodeExecHints 把节点执行声明写入 context。
func WithNodeExecHints(ctx context.Context, h NodeExecHints) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, nodeExecHintsKey{}, h)
}

// NodeExecHintsFromContext 读取节点执行声明 (无则返回零值)。
func NodeExecHintsFromContext(ctx context.Context) NodeExecHints {
	if ctx == nil {
		return NodeExecHints{}
	}
	h, _ := ctx.Value(nodeExecHintsKey{}).(NodeExecHints)
	return h
}

// stageNodeRunner 图节点执行器: 复用 ExecuteSingleStage (design/01 §4.9 local runner)。
type stageNodeRunner struct {
	we        *WorkflowExecutor
	team      *ProductionTeam
	objective string
}

func (r *stageNodeRunner) RunNode(ctx context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	// 节点声明下传 (工具画像 / MaxTurns / 确定性 / loop 轮次), 供宿主 factory 消费。
	ctx = WithNodeExecHints(ctx, NodeExecHints{
		Node: node.ID, Role: node.Agent.Role, Kind: string(node.Kind),
		ToolProfile: node.Agent.ToolProfile, MaxTurns: node.Agent.MaxTurns,
		Deterministic: node.Agent.Deterministic, Iteration: in.Iteration,
	})

	// gate 节点差异化执行 (design/01 §4.1): 门禁节点输出 score, 供条件边
	// `score >= 75` 路由; 不是普通 agent。
	if node.Kind == graph.NodeKindGate {
		return r.runGate(ctx, node, in)
	}

	stage := StageDef{
		Name:   node.ID,
		Role:   node.Agent.Role,
		Prompt: node.Agent.Prompt,
	}
	objective := r.objective
	if in.Feedback != "" {
		// loop 回灌: 作为用户反馈注入 (复用 {user_feedback} 管线语义)
		objective = objective + "\n\n## 上一轮反馈\n" + in.Feedback
	}
	// 重试让位 (design/01 §4.3, 见文件头"重试单层化"): 图层已持有重试策略
	// (NodeSpec.Retry / Policies.DefaultRetry), 内层 executeStageWithRetry 必须
	// 退化为"1 次 + 1 次校验修正", 否则 图(1+6) × 内层(1+3+限流20) 会变成
	// 嵌套放大 —— 这正是 design/01 §4.3 要消除的东西。
	sr := r.executorForNode(node).ExecuteSingleStage(WithOuterRetryDriven(ctx), stage, objective, in.PrevOutputs, r.team)
	res := graph.NodeResult{Output: sr.Output, Err: sr.Error}
	if sr.Status == TaskCompleted {
		res.Status = graph.NodeStatusCompleted
	} else {
		res.Status = graph.NodeStatusFailed
	}
	return res
}

// executorForNode 需要覆盖 MaxTurns 时返回一个包了 factory 的浅拷贝 executor。
//
// 为什么必须包 factory 而不是提前塞 ctx: runAgent 会在调用 factory **之前**用
// planCfgResolver.Resolve 覆写 ModelConfigKey (workflow.go), 图层提前放进 ctx 的
// ResolvedConfig 会被整体冲掉。只有 factory 这一层能看到最终的 ResolvedConfig,
// 在其上改 MaxTurns 才有效。WorkflowExecutor 无锁字段, 浅拷贝安全 (只换 factory)。
func (r *stageNodeRunner) executorForNode(node graph.NodeSpec) *WorkflowExecutor {
	if r.we == nil || node.Agent.MaxTurns <= 0 || r.we.factory == nil {
		return r.we
	}
	inner := r.we.factory
	maxTurns := node.Agent.MaxTurns
	cp := *r.we
	cp.factory = func(ctx context.Context, role, systemPrompt string) (AgentRunner, error) {
		if mc, ok := ctx.Value(ModelConfigKey{}).(modelconfig.ResolvedConfig); ok {
			mc.MaxTurns = maxTurns
			ctx = context.WithValue(ctx, ModelConfigKey{}, mc)
		}
		return inner(ctx, role, systemPrompt)
	}
	return &cp
}

// runGate 执行门禁节点: 对上游产出打分, Score 落 NodeResult 供条件边路由。
// Status 恒 completed (无论分数, 由条件边决定后续), 除非评审彻底不可用。
// gate 语义分派 (design/01 §4.1):
//   - Agent.Deterministic 或 compile/test/build 关键词: 确定性门禁 (零 LLM,
//     上游产出非空 + 结构化即通过; 真编译门禁在 GateEnforcer/产码工作流)
//   - 其余 (content/quality/评审/默认): LLM 内容质量评审 (复用 runContentCritic)
func (r *stageNodeRunner) runGate(ctx context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	// 取待评审产出: 优先直接前驱的产出, 否则全部上游拼接
	deliverable := gatePickDeliverable(in.PrevOutputs)

	// 确定性门禁: 无 LLM, 按产出存在性/结构给分。
	if node.Agent.Deterministic || gateIsDeterministic(node.ID, node.Agent.Role) {
		score := 0.0
		if strings.TrimSpace(deliverable) != "" {
			score = 80 // 有产出即视为通过 (占位; 真编译门禁在 GateEnforcer/产码工作流)
			if looksLikeCodeArtifact(deliverable) {
				score = 90
			}
		}
		return graph.NodeResult{
			Status: graph.NodeStatusCompleted,
			Score:  score,
			Output: fmt.Sprintf(`{"gate":"deterministic","score":%.0f}`, score),
		}
	}

	// LLM 内容评审 (content/quality/评审/默认): 复用 runContentCriticLLM (走 we.llm)
	if r.we != nil && r.we.llm != nil {
		if v := runContentCriticLLM(ctx, r.we.llm, r.objective, deliverable); v != nil {
			out, _ := json.Marshal(v)
			// 奖励持久化 (design/03 §4.2): 图 gate 分数也入 rewards.jsonl
			if r.we.evolution != nil && r.team != nil {
				r.we.evolution.RecordReward(RewardEvent{
					RunID:  trace.From(ctx).RunID,
					NodeID: node.ID,
					Source: "gate.content",
					Value:  float64(v.Score)/50.0 - 1.0,
					Raw:    v.Score,
					Team:   r.team.Name,
				})
			}
			return graph.NodeResult{
				Status: graph.NodeStatusCompleted,
				Score:  float64(v.Score),
				Output: string(out),
			}
		}
	}
	// 评审不可用: 给中性分, 不阻断 (fail-open)
	return graph.NodeResult{Status: graph.NodeStatusCompleted, Score: 60, Output: `{"gate":"unavailable","score":60}`}
}

// stageIsGate 判断一个 stage 是否门禁阶段 (按 role/name 关键词)。
// 门禁阶段被译为 gate 节点, 输出 score 供条件边路由, 不做常规 agent 产出。
func stageIsGate(st StageDef) bool {
	key := strings.ToLower(st.Name + " " + st.Role)
	for _, kw := range []string{"gate", "门禁", "quality-gate", "content-gate"} {
		if strings.Contains(key, kw) {
			return true
		}
	}
	return false
}

// gateIsDeterministic 门禁是否纯代码判定 (compile/test/build 类)。
// 直译期与执行期共用同一判据, 避免"标了 Deterministic 却走 LLM"这种两处漂移。
func gateIsDeterministic(name, role string) bool {
	key := strings.ToLower(name + " " + role)
	return strings.Contains(key, "compile") || strings.Contains(key, "test") || strings.Contains(key, "build")
}

// gatePickDeliverable 从上游产出中选待评审内容: 取最长的非空产出。
func gatePickDeliverable(prev map[string]string) string {
	best := ""
	for _, v := range prev {
		if len(v) > len(best) {
			best = v
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// HookBus 生产实现: 把节点生命周期桥到既有观测设施
// ---------------------------------------------------------------------------

// teamGraphHooks 图引擎 HookBus 的生产实现 (design/01 §4.5)。
//
// executeGraph 此前构造 Engine 时不传 Hooks ⇒ 生产恒 NopBus, 后果是图模式**没有
// 阶段级可观测**: pipeline 侧 coordinator.go 每个阶段都做 flushTeamStages
// (dashboard 运行中能看到进度) + recordStageMetrics (阶段耗时/成败指标), 图侧两者
// 皆无 —— 灰度打开后运行中的团队在 dashboard 上一片空白, 直到整图跑完才一次性出现。
//
// 桥接四件事:
//   - graph pre/post : 结构化日志 (整图开始/结束)
//   - node  pre      : 落 running 占位 + 增量刷盘 + watchdog 心跳 + 进展上报
//   - node  post     : 落终态 (产出/耗时取自 hook 载荷) + 增量刷盘 + 阶段指标
//   - node  failure  : 同 post, 另记失败与重试次数指标
//
// 决策恒放行: 观测桥不做准入判断 (node pre 返回 deny 会让节点 skipped),
// 灰度打开不引入新的阻断路径。
type teamGraphHooks struct {
	we    *WorkflowExecutor
	team  *ProductionTeam
	order []string          // 节点声明序: 刷盘顺序与 pipeline 产出顺序一致
	roles map[string]string // 节点 → 角色 (hook 载荷已带 role, 这里作兜底)

	mu    sync.Mutex
	byID  map[string]StageResult
	start map[string]time.Time
}

func newTeamGraphHooks(we *WorkflowExecutor, team *ProductionTeam, spec graph.GraphSpec) *teamGraphHooks {
	h := &teamGraphHooks{
		we: we, team: team,
		roles: make(map[string]string, len(spec.Nodes)),
		byID:  map[string]StageResult{},
		start: map[string]time.Time{},
	}
	for _, n := range spec.Nodes {
		h.order = append(h.order, n.ID)
		h.roles[n.ID] = n.Agent.Role
	}
	return h
}

// Emit 实现 graph.HookBus。恒返回放行决策。
func (h *teamGraphHooks) Emit(ctx context.Context, ev graph.HookEvent) graph.HookDecision {
	switch ev.Scope {
	case graph.ScopeGraph:
		logging.Event(ctx, "graph."+ev.Phase, "team", h.teamName(),
			"graph", payloadStr(ev.Payload, "graph"), "status", payloadStr(ev.Payload, "status"),
			"nodes", payloadStr(ev.Payload, "nodes"), "completed", payloadStr(ev.Payload, "completed"))
	case graph.ScopeNode:
		switch ev.Phase {
		case "pre":
			h.nodeStarted(ctx, ev)
		case "post", "failure":
			h.nodeFinished(ctx, ev)
		}
	}
	return graph.HookDecision{}
}

// nodeStarted 节点开始: 写 running 占位并立刻刷盘, 让 dashboard 看到"正在跑哪个阶段"。
func (h *teamGraphHooks) nodeStarted(ctx context.Context, ev graph.HookEvent) {
	now := time.Now()
	h.mu.Lock()
	h.start[ev.NodeID] = now
	h.byID[ev.NodeID] = StageResult{
		Name:      ev.NodeID,
		Role:      h.roleOf(ev),
		Status:    TaskRunning,
		StartedAt: now,
	}
	h.mu.Unlock()

	h.flush()
	h.touch(ev.NodeID, 0)
	logging.Event(ctx, "graph.node.start", "team", h.teamName(), "node", ev.NodeID,
		"role", h.roleOf(ev), "kind", payloadStr(ev.Payload, "kind"))
}

// nodeFinished 节点终态: 落 StageResult (产出/耗时/错误来自 hook 载荷) + 刷盘 + 指标。
func (h *teamGraphHooks) nodeFinished(ctx context.Context, ev graph.HookEvent) {
	sr := StageResult{
		Name:   ev.NodeID,
		Role:   h.roleOf(ev),
		Output: payloadStr(ev.Payload, "output"),
		Error:  payloadStr(ev.Payload, "error"),
		Status: TaskCompleted,
	}
	if ev.Phase == "failure" {
		sr.Status = TaskFailed
	}
	durMs := payloadInt(ev.Payload, "duration_ms")
	attempts := payloadInt(ev.Payload, "attempts")
	iterations := payloadInt(ev.Payload, "iterations")

	h.mu.Lock()
	if st, ok := h.start[ev.NodeID]; ok {
		sr.StartedAt = st
	}
	// 恒填耗时 (哪怕 0ms): 阶段记录里 Duration 为空会被下游当成"没跑过"。
	sr.Duration = (time.Duration(durMs) * time.Millisecond).String()
	h.byID[ev.NodeID] = sr
	h.mu.Unlock()

	h.flush()
	h.touch(ev.NodeID, iterations)
	retries := attempts - 1
	if retries < 0 {
		retries = 0
	}
	recordGraphStageMetrics(h.team, sr, retries)
	logging.Event(ctx, "graph.node."+ev.Phase, "team", h.teamName(), "node", ev.NodeID,
		"role", sr.Role, "status", string(sr.Status), "attempts", fmt.Sprintf("%d", attempts),
		"iterations", fmt.Sprintf("%d", iterations), "duration_ms", fmt.Sprintf("%d", durMs),
		"output_len", fmt.Sprintf("%d", len(sr.Output)), "error", sr.Error)
}

// flush 把当前节点快照按声明序增量写回 team (对齐 coordinator.flushTeamStages)。
func (h *teamGraphHooks) flush() {
	if h.team == nil {
		return
	}
	h.mu.Lock()
	snapshot := make([]StageResult, 0, len(h.byID))
	for _, id := range h.order {
		if sr, ok := h.byID[id]; ok {
			snapshot = append(snapshot, sr)
		}
	}
	h.mu.Unlock() // 先放锁再碰 team 锁: 避免与 team.mu 形成锁序耦合

	h.team.mu.Lock()
	h.team.Stages = snapshot
	h.team.mu.Unlock()
	h.team.persist()
}

// touch watchdog 心跳 + 进展上报 (Coordinator 靠这两路判断团队是否还活着)。
func (h *teamGraphHooks) touch(nodeID string, iteration int) {
	if h.we == nil {
		return
	}
	if h.we.activityCallback != nil {
		h.we.activityCallback()
	}
	if h.we.progressCallback != nil {
		h.we.progressCallback("graph:"+nodeID, iteration, 0, "")
	}
}

func (h *teamGraphHooks) roleOf(ev graph.HookEvent) string {
	if r := payloadStr(ev.Payload, "role"); r != "" {
		return r
	}
	return h.roles[ev.NodeID]
}

func (h *teamGraphHooks) teamName() string {
	if h.team == nil {
		return ""
	}
	return h.team.Name
}

func payloadStr(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	switch v := p[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

func payloadInt(p map[string]any, key string) int {
	if p == nil {
		return 0
	}
	switch v := p[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

// recordGraphStageMetrics 图侧阶段指标 (与 coordinator.recordStageMetrics 同名同标签,
// 保证 dashboard 现有面板在灰度期照样出图; 另补记图层重试次数)。
func recordGraphStageMetrics(team *ProductionTeam, sr StageResult, retries int) {
	if team == nil {
		return
	}
	mc := team.metrics()
	if mc == nil {
		return
	}
	labels := map[string]string{
		"workflow": team.Workflow,
		"stage":    sr.Name,
		"role":     sr.Role,
		"status":   string(sr.Status),
	}
	if sr.Duration != "" {
		if d, err := time.ParseDuration(sr.Duration); err == nil && d > 0 {
			mc.RecordRun("team", metrics.MTeamStageDurationSec, d.Seconds(), team.Name, labels)
		}
	}
	mc.RecordRun("team", metrics.MTeamStageCount, 1, team.Name, labels)
	if sr.Status == TaskCompleted {
		mc.RecordRun("team", metrics.MTeamStageSuccessCount, 1, team.Name, labels)
	} else {
		mc.RecordRun("team", metrics.MTeamStageFailCount, 1, team.Name, labels)
	}
	if retries > 0 {
		mc.RecordRun("team", metrics.MTeamStageRetryCount, float64(retries), team.Name, labels)
	}
	if sr.Output != "" {
		mc.RecordRun("team", metrics.MTeamStageOutputLen, float64(len(sr.Output)), team.Name, labels)
	}
}

// ---------------------------------------------------------------------------
// 图引擎执行入口
// ---------------------------------------------------------------------------

// executeGraph 图引擎执行入口 (wf.Mode=="graph" 或灰度开关命中时由 Execute/executePipeline 转入)。
// Journal 落 <team.dataDir>/graph-journal.jsonl; Resume 恒开 —— 重放即恢复,
// 取代 pipeline 路径的 checkpoints.json (design/01 §4.3)。
func (we *WorkflowExecutor) executeGraph(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	spec, err := TranslateWorkflow(wf)
	if err != nil {
		return nil, err
	}
	// NewFileJournal 收目录名, 内部落 <dir>/journal.jsonl
	journalDir := filepath.Join(team.dataDir, "graph-journal")
	journal, err := graph.NewFileJournal(journalDir)
	if err != nil {
		return nil, fmt.Errorf("graph_adapter: 打开 journal 失败: %w", err)
	}
	defer journal.Close() // 不关会每跑一次团队泄一个 fd

	hooks := newTeamGraphHooks(we, team, spec)
	eng := &graph.Engine{
		Runner:  &stageNodeRunner{we: we, team: team, objective: objective},
		Journal: journal,
		Hooks:   hooks, // 生产此前恒 NopBus: 图模式没有阶段级刷盘/指标/心跳
		// 并发上限与 pipeline 侧对齐: effectiveParallel 会按 API 流控状态动态收敛
		// (图引擎自己的默认是硬编码 4)。覆盖表声明的 Policies.MaxParallel 优先于此。
		MaxParallel: we.effectiveParallel(),
	}
	runID := trace.From(ctx).RunID
	if runID == "" {
		runID = trace.NewRunID(team.Name)
	}
	// 灰度期要能一眼核对"图到底带了哪些能力": 节点数/并发/重试/门禁元数据全打出来。
	defaultRetries := 0
	if spec.Policies.DefaultRetry != nil {
		defaultRetries = spec.Policies.DefaultRetry.MaxRetries
	}
	logging.Event(ctx, "graph.run.start", "team", team.Name, "workflow", wf.Name,
		"nodes", fmt.Sprintf("%d", len(spec.Nodes)),
		"max_parallel", fmt.Sprintf("%d", eng.MaxParallel),
		"default_retry", fmt.Sprintf("%d", defaultRetries),
		"produces_code", fmt.Sprintf("%v", spec.Meta.ProducesCode),
		"quality_gate", spec.Meta.QualityGate)
	rr, runErr := eng.Run(ctx, spec, graph.RunOpts{
		RunID:     runID,
		Objective: objective,
		Resume:    true, // 重放即恢复: journal 有已完成节点则直接吃缓存
	})

	// RunResult → []StageResult (按 spec 节点声明序, 与 pipeline 产出形态一致)
	results := make([]StageResult, 0, len(spec.Nodes))
	for _, n := range spec.Nodes {
		nr, ok := rr.Nodes[n.ID]
		if !ok {
			continue // 未被调度 (取消等), 不占位
		}
		if nr.Status == graph.NodeStatusSkipped {
			// 条件边把流程路由到了别的分支 / hook deny / 上游 skipped 级联:
			// 这个阶段**没有执行**, 不是失败。此前一律映射成 TaskFailed —— 一旦
			// 条件边真的用起来, 被路由掉的那条分支就会被 teams.go 统计成"阶段失败"
			// 并把整个团队判死 (评分过关反而团队失败)。故按"未执行"处理: 不进
			// results, 只留日志与 journal 记录。
			logging.Event(ctx, "graph.node.skipped", "team", team.Name, "node", n.ID, "reason", nr.Err)
			continue
		}
		sr := StageResult{
			Name:     n.ID,
			Role:     n.Agent.Role,
			Output:   nr.Output,
			Error:    nr.Err,
			Duration: "", // 图引擎粒度的时长在 journal 事件里, StageResult 不重复记
		}
		if nr.Status == graph.NodeStatusCompleted {
			sr.Status = TaskCompleted
		} else {
			sr.Status = TaskFailed
		}
		// 耗时/起始时间由 hook 桥接方按本次运行实测填充 (resume 缓存命中的节点没有)。
		if hs, ok := hooks.stageResultByID(n.ID); ok {
			sr.Duration, sr.StartedAt = hs.Duration, hs.StartedAt
		}
		results = append(results, sr)
		// 黑板回写与 pipeline 路径对齐 (下游 harvest/handoff 依赖 <stage>-result 键)
		if team.Blackboard != nil && nr.Status == graph.NodeStatusCompleted {
			team.Blackboard.Write(n.ID+"-result", nr.Output, n.Agent.Role, "result")
		}
	}
	if runErr != nil {
		return results, runErr
	}
	if rr.Status == graph.RunStatusFailed {
		return results, fmt.Errorf("graph_adapter: 图执行失败 (无节点完成)")
	}
	logging.Event(ctx, "graph.run.finish", "team", team.Name, "status", rr.Status)
	return results, nil
}

// stageResultByID 取某节点的 hook 侧记录 (含实测耗时)。
func (h *teamGraphHooks) stageResultByID(id string) (StageResult, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sr, ok := h.byID[id]
	return sr, ok
}

// graphEngineEnabled 灰度开关 (design/01 M1 验收后默认开)。
func graphEngineEnabled() bool {
	v := strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_ENGINE"))
	return v == "1" || strings.EqualFold(v, "true")
}
