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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
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
	// Kind 强制节点形态 ""|"agent"|"gate"|"map"|"reduce"。显式 "agent" 可压制
	// stageIsGate 的关键词推断 (例如名字里带"门禁"但其实是普通写作阶段)。
	// map/reduce 必须同时给出对应的 Map/Reduce 策略 (直译器强制, 不猜)。
	Kind string `json:"kind,omitempty"`
	// Map 扇出策略 (Kind="map" 必填): 集合来源 + 切分策略 + 分片数上下限。
	// 这是 fanout 类工作流从"转调 pipeline 的空壳"变成真 map→reduce 的入口。
	Map *graph.MapPolicy `json:"map,omitempty"`
	// Reduce 聚合策略 (Kind="reduce" 可选, 不填=交给 runner 用 LLM 聚合)。
	Reduce *graph.ReducePolicy `json:"reduce,omitempty"`
	// Expand 授权本阶段动态展开子图 (GoalTree/WBS/swarm 三套分解的统一落点)。
	// 只有声明了它, stageNodeRunner 才会去解析阶段产出里的子图 (见 parseStageExpansion)。
	Expand *graph.ExpandSpec `json:"expand,omitempty"`
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
	// Placement 本阶段的放置约束 (design/01 §4.9 逐节点放置): 例如需要无头浏览器的
	// 渲染阶段写 {"require":["browser"]}, 于是它只会被派到有 browser 能力的 runtime。
	// 不声明时沿用进程级默认 (`--placement-prefer` + 团队亲和), 行为与改造前一致。
	Placement *graph.PlacementSpec `json:"placement,omitempty"`
}

// WorkflowGraphOverride 一个工作流的图能力声明。
type WorkflowGraphOverride struct {
	MaxParallel  int                           `json:"maxParallel,omitempty"`  // 图级并发上限 (0=用 executor 的 effectiveParallel)
	DefaultRetry *graph.RetryPolicy            `json:"defaultRetry,omitempty"` // 图级默认重试 (nil=用 graphOuterMaxRetries)
	Stages       map[string]StageGraphOverride `json:"stages,omitempty"`       // 按阶段名
	// MaxTotalNodes 运行图节点总数上限 (0=引擎默认): 动态展开与 map 扇出的总闸。
	MaxTotalNodes int `json:"maxTotalNodes,omitempty"`
	// Groups 组级循环声明: 把若干**已有阶段**折成一个 loop-group 节点整体循环。
	// 这是 adversarial 的 Rounds / 内容质量重做环在生产输入上的表达方式 ——
	// 组成员写阶段名, 不必在覆盖表里重新描述一遍节点 (那必然与工作流定义漂移)。
	Groups []StageGroupOverride `json:"groups,omitempty"`
}

// StageGroupOverride 一组阶段的组级循环声明 (design/01 §4.4 loop-group)。
type StageGroupOverride struct {
	// ID 组节点 ID (空=loop-<第一个成员>)。它会出现在 journal/hook/阶段记录里。
	ID string `json:"id,omitempty"`
	// Members 组内阶段名 (≥1, 必须是本工作流的阶段, 且不得被两个组同时收编)。
	// 组内阶段之间的 DependsOn 成为组内边; 与组外的依赖被改接到组节点上。
	Members []string `json:"members"`
	// MaxIterations 组循环硬上限, 必填 >0 (无界循环违法)。
	MaxIterations int `json:"maxIterations"`
	// Until 退出条件 (对组产出节点的结果求值), 语法见 pkg/graph/condition.go。
	Until string `json:"until,omitempty"`
	// Terminator 可插拔终止器 (与 Until **二选一**, 见 pkg/graph/terminator.go)。
	// 这是 creative_media / app_composite / game_composite / novel_writing /
	// swarm_novel 五个 mode 的 AdaptiveTerminator 在图上的表达方式:
	// 它们的退出条件是"达标/收敛/退化/策略转换/best-of-N 回滚"五路信号的组合,
	// 其中三路是跨轮判断, Until 只看当前轮结果, 表达不了。
	Terminator *graph.TerminatorSpec `json:"terminator,omitempty"`
	// Feedback 每轮回灌模板 ({prev_output} = 上一轮组产出), 下发给全部成员。
	Feedback string `json:"feedback,omitempty"`
	// ResultFrom 组产出取哪个成员 (空=组内唯一出度 0 成员; 多个时必须显式)。
	ResultFrom string `json:"resultFrom,omitempty"`
	// Role 组节点角色 (仅用于日志/hook 载荷, 空=coordinator)。
	Role string `json:"role,omitempty"`
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
	if ov.Map != nil {
		out.Map = ov.Map
	}
	if ov.Reduce != nil {
		out.Reduce = ov.Reduce
	}
	if ov.Expand != nil {
		out.Expand = ov.Expand
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
	// 放置整份替换而不是逐字段合并: Require 是硬约束集合, 半份继承半份覆盖会拼出
	// 一个谁都没声明过的约束集 (例如覆盖表想放宽到只要 bash, 却留下了阶段声明的 gpu)。
	if ov.Placement != nil {
		out.Placement = ov.Placement
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
			MaxParallel:   ov.MaxParallel, // 0 → executeGraph 用 effectiveParallel 兜
			DefaultRetry:  defaultRetry,
			MaxTotalNodes: ov.MaxTotalNodes, // 0 → 引擎默认 (动态展开/扇出的总闸)
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
		case string(graph.NodeKindMap):
			// map 的策略绝不推导: 集合从哪来、怎么切、最多几片, 猜错就是扇出 800 次
			// LLM 调用或静默只跑一片。必须显式声明。
			if so.Map == nil || so.Map.MaxShards <= 0 {
				return graph.GraphSpec{}, fmt.Errorf("graph_adapter: 阶段 %q 声明 kind=map 但缺少 map 策略 (至少要给 maxShards)", st.Name)
			}
			kind = graph.NodeKindMap
		case string(graph.NodeKindReduce):
			kind = graph.NodeKindReduce
		case "":
			if stageIsGate(st) {
				kind = graph.NodeKindGate // 门禁阶段 → gate 节点 (输出 score 供条件边路由)
			}
		default:
			return graph.GraphSpec{}, fmt.Errorf("graph_adapter: 阶段 %q 的覆盖 kind=%q 非法 (仅 agent|gate|map|reduce; 组级循环用 groups 声明)", st.Name, so.Kind)
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
			// map 节点的预算要包住**全部分片**: 分片并发但并发度未知 (executeGraph
			// 才知道 effectiveParallel), 按最坏情况 (全串行) 给, 再由 2h 硬顶夹住。
			// 给紧了的后果是合法的扇出被 deadline 掐死, 比给松严重得多。
			if kind == graph.NodeKindMap {
				timeoutSec = capNodeBudget(timeoutSec * so.Map.MaxShards)
			}
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
				Placement:     so.Placement,
			},
			Loop:       so.Loop,
			Retry:      so.Retry,
			TimeoutSec: timeoutSec,
			Map:        so.Map,
			Reduce:     so.Reduce,
			Expand:     so.Expand,
		})
		for _, dep := range st.DependsOn {
			cond := so.Condition
			if c, ok := so.EdgeConditions[dep]; ok {
				cond = c // 精确到上游的声明优先于本阶段的统一条件
			}
			spec.Edges = append(spec.Edges, graph.EdgeSpec{From: dep, To: st.Name, Condition: cond})
		}
	}
	// 组级循环: 把声明的阶段组折成 loop-group 节点 (在 Validate 之前, 折完才是终图)。
	spec, err := foldStageGroups(spec, ov.Groups)
	if err != nil {
		return graph.GraphSpec{}, err
	}
	if err := spec.Validate(); err != nil {
		return graph.GraphSpec{}, fmt.Errorf("graph_adapter: 直译结果非法: %w", err)
	}
	return spec, nil
}

// capNodeBudget 夹住节点预算上限 (卡死兜底闸, 不是省钱闸)。
func capNodeBudget(sec int) int {
	if sec > graphNodeBudgetCapSec || sec <= 0 {
		return graphNodeBudgetCapSec
	}
	return sec
}

// foldStageGroups 把 WorkflowGraphOverride.Groups 声明的阶段组折成 loop-group 节点
// (design/01 §4.4)。
//
// 折叠而不是"引用外部节点": pkg/graph 的 loop-group 把组内子图**内嵌**在节点里
// (组成员若留在顶层, 顶层的入口可达/无环校验会把它们当孤岛报错, 调度器还会把它们
// 各跑一次)。所以这里做三件事:
//  1. 把成员节点从顶层摘出, 放进 GroupPolicy.Nodes;
//  2. 成员之间的边 → 组内边;
//  3. 跨组边界的边改接到组节点上 (外→成员 变 外→组; 成员→外 变 组→外, 条件保留 ——
//     组的结果就是 ResultFrom 成员的结果, 条件语义因此仍然成立)。
func foldStageGroups(spec graph.GraphSpec, groups []StageGroupOverride) (graph.GraphSpec, error) {
	if len(groups) == 0 {
		return spec, nil
	}
	consumed := map[string]string{} // 成员阶段 → 收编它的组 ID
	for _, g := range groups {
		if len(g.Members) == 0 {
			return spec, fmt.Errorf("graph_adapter: 组声明 %q 没有成员", g.ID)
		}
		if g.MaxIterations <= 0 {
			return spec, fmt.Errorf("graph_adapter: 组 %q 的 maxIterations 必须 > 0 (无界循环违法)", g.ID)
		}
		groupID := strings.TrimSpace(g.ID)
		if groupID == "" {
			groupID = "loop-" + g.Members[0]
		}
		byID := map[string]graph.NodeSpec{}
		for _, n := range spec.Nodes {
			byID[n.ID] = n
		}
		if _, clash := byID[groupID]; clash {
			return spec, fmt.Errorf("graph_adapter: 组 ID %q 与已有阶段同名", groupID)
		}
		member := map[string]bool{}
		var members []graph.NodeSpec
		for _, name := range g.Members {
			// 先查"是否已被别的组收编": 前一个组折叠后成员已不在顶层节点表里,
			// 若先查存在性会报成"不是本工作流的阶段", 把真实病因 (重复收编) 藏起来。
			if other, dup := consumed[name]; dup {
				return spec, fmt.Errorf("graph_adapter: 阶段 %q 被两个组同时收编 (%s / %s)", name, other, groupID)
			}
			if _, ok := byID[name]; !ok {
				return spec, fmt.Errorf("graph_adapter: 组 %q 的成员 %q 不是本工作流的阶段", groupID, name)
			}
			consumed[name] = groupID
			member[name] = true
		}
		// 成员按**原声明序**入组: 与 pipeline 的阶段顺序一致, 便于灰度期比对。
		for _, n := range spec.Nodes {
			if member[n.ID] {
				members = append(members, n)
			}
		}
		if rf := strings.TrimSpace(g.ResultFrom); rf != "" && !member[rf] {
			return spec, fmt.Errorf("graph_adapter: 组 %q 的 resultFrom=%q 不是组成员", groupID, rf)
		}

		var innerEdges, outerEdges []graph.EdgeSpec
		seen := map[string]bool{}
		addOuter := func(ed graph.EdgeSpec) {
			key := ed.From + "→" + ed.To + "|" + ed.Condition
			if seen[key] || ed.From == ed.To {
				return // 组内两个成员各自与组外同一节点相连时会重复; 自环直接丢
			}
			seen[key] = true
			outerEdges = append(outerEdges, ed)
		}
		for _, ed := range spec.Edges {
			switch {
			case member[ed.From] && member[ed.To]:
				innerEdges = append(innerEdges, ed)
			case member[ed.To]: // 外 → 成员
				addOuter(graph.EdgeSpec{From: ed.From, To: groupID, Condition: ed.Condition})
			case member[ed.From]: // 成员 → 外
				addOuter(graph.EdgeSpec{From: groupID, To: ed.To, Condition: ed.Condition})
			default:
				addOuter(ed)
			}
		}

		role := strings.TrimSpace(g.Role)
		if role == "" {
			role = "coordinator"
		}
		budget := 0
		for _, m := range members {
			budget += m.TimeoutSec
		}
		groupNode := graph.NodeSpec{
			ID:    groupID,
			Kind:  graph.NodeKindLoopGroup,
			Agent: graph.AgentSpec{Role: role},
			// 组节点自己不调 runner, 预算只作卡死兜底: 成员预算之和 × 轮次上限。
			TimeoutSec: capNodeBudget(budget * g.MaxIterations),
			Group: &graph.GroupPolicy{
				Nodes: members,
				Edges: innerEdges,
				Loop: graph.LoopPolicy{
					MaxIterations: g.MaxIterations,
					Until:         g.Until,
					Feedback:      g.Feedback,
					Terminator:    g.Terminator, // 与 Until 二选一, 由 Validate 强制
				},
				ResultFrom: strings.TrimSpace(g.ResultFrom),
			},
		}
		// 组节点占据第一个成员的位置: 顶层节点序仍与阶段声明序一致。
		var nodes []graph.NodeSpec
		placed := false
		for _, n := range spec.Nodes {
			if !member[n.ID] {
				nodes = append(nodes, n)
				continue
			}
			if !placed {
				nodes = append(nodes, groupNode)
				placed = true
			}
		}
		spec.Nodes, spec.Edges = nodes, outerEdges
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
	Node string // 节点 ID (语义名 = 阶段名; 分片为 <阶段>#<序号>)
	// NodeRef journal/hook 里的限定 ID: 顶层 = Node; loop-group 组内 =
	// <组>#it<轮次>/<成员>。宿主要把自己的记录与 journal 对齐只能用它
	// (Node 在组的每一轮都相同, 单看它无法区分是第几轮)。
	NodeRef       string
	Role          string
	Kind          string // agent|gate
	ToolProfile   string // 显式工具画像 (空=宿主按角色推断, 即现状)
	MaxTurns      int    // 0=不覆盖
	Deterministic bool
	Iteration     int // loop 轮次 (0 起)
	// Placement 本节点声明的放置约束 (design/01 §4.9 逐节点放置)。
	// nil = 未声明, 宿主用进程级默认 (与改造前行为一致)。
	// 指针而不是值: NodeExecHints 被用 `!= NodeExecHints{}` 判空 (worker/factory.go
	// 与 feishu/session.go 各一处), 放切片进去会让结构体不可比较, 那两处直接编译不过。
	Placement *Placement
}

// PlacementFromSpec 把图侧的放置声明镜像转成 agent.Placement。
//
// 两个结构体字段一一对应 (见 graph.PlacementSpec 的注释: 内核不 import pkg/agent,
// 故必须有一份镜像)。转换点只此一处 —— 镜像结构体最大的风险是"加了字段忘了搬",
// 集中在这里比散在各处好查。
func PlacementFromSpec(p *graph.PlacementSpec) *Placement {
	if p == nil {
		return nil
	}
	out := &Placement{
		Prefer:      strings.TrimSpace(p.Prefer),
		Affinity:    strings.TrimSpace(p.Affinity),
		AffinityKey: strings.TrimSpace(p.AffinityKey),
	}
	if len(p.Require) > 0 {
		// 值拷贝: 图规格在整个 run 期间被多个节点 goroutine 共享读, 绝不能让
		// 下游 (factory 的合并逻辑) 拿到能就地改的底层数组。
		out.Require = append([]string(nil), p.Require...)
	}
	return out
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
	// deps 节点 → 上游节点 ID (按**边声明序**), 由 graphNodeDeps 从运行图抽出。
	// 见 RunNode 里 DependsOn 的注释: pipeline 侧拼依赖块用的是声明序, 这里必须同序。
	deps map[string][]string
}

func (r *stageNodeRunner) RunNode(ctx context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	// 节点声明下传 (工具画像 / MaxTurns / 确定性 / loop 轮次), 供宿主 factory 消费。
	ctx = WithNodeExecHints(ctx, NodeExecHints{
		Node: node.ID, NodeRef: orNodeID(in.NodeRef, node.ID),
		Role: node.Agent.Role, Kind: string(node.Kind),
		ToolProfile: node.Agent.ToolProfile, MaxTurns: node.Agent.MaxTurns,
		Deterministic: node.Agent.Deterministic, Iteration: in.Iteration,
		// 逐节点放置 (§4.9): map 分片自动继承 (shardNodeSpec 复制整个 Agent),
		// 于是"每个分片都要 browser"只需在 map 节点上声明一次。
		Placement: PlacementFromSpec(node.Agent.Placement),
	})

	// gate 节点差异化执行 (design/01 §4.1): 门禁节点输出 score, 供条件边
	// `score >= 75` 路由; 不是普通 agent。
	if node.Kind == graph.NodeKindGate {
		return r.runGate(ctx, node, in)
	}

	// DependsOn 必须填: buildStagePromptWithRoles 是**按 stage.DependsOn 遍历**
	// prevResults 来拼 "### Dependency output from X" 块的 (workflow.go:1804), 而
	// {prev_result} 占位符替换的就是这个块。此前图路径构造 StageDef 时不填 DependsOn
	// ⇒ 依赖块恒空 ⇒ **上游产出根本没进 prompt**, {prev_result} 被替换成空串,
	// 每个阶段都在没有上游交接的情况下干活 (灰度期没被发现是因为桩 runner 只回显
	// prompt 长度)。这里用 PrevOutputs 的键 (即 completed 的直接前驱) 补上。
	//
	// 顺序取**图里的边声明序**而不是键名字典序: pipeline 侧拼依赖块遍历的是
	// StageDef.DependsOn 的声明序, 字典序会让同一个阶段在两条路径上拿到不同的
	// 提示词 (research 的 cross-verification 声明序是 tech/market/risk, 字典序是
	// market/risk/tech) —— 那是等价性的直接破口, 也会打散 prompt 前缀缓存。
	// 查不到声明序时 orderedPrevDeps 退化为字典序 (仍然确定性), 见 graph_templates.go。
	stage := StageDef{
		Name:      node.ID,
		Role:      node.Agent.Role,
		Prompt:    node.Agent.Prompt,
		DependsOn: prevDepsWithDynamic(r.deps, node.ID, in.PrevOutputs),
	}
	// —— 图特有的执行上下文 (回灌 / 分片 / 待聚合分片) ——
	var extraCtx strings.Builder
	if in.Feedback != "" {
		// loop 回灌 (节点级或组级): 作为用户反馈注入 (复用 {user_feedback} 管线语义)
		fmt.Fprintf(&extraCtx, "\n\n## 上一轮反馈\n%s", in.Feedback)
	}
	// map 分片: 引擎只做确定性切分, "这一片是什么"必须由 runner 送进 prompt,
	// 否则 N 个分片会拿到完全一样的输入、产出 N 份重复内容 (还烧 N 倍 token)。
	if in.Shard != nil {
		fmt.Fprintf(&extraCtx, "\n\n## 本分片任务 (第 %d/%d 片)\n%s",
			in.Shard.Index+1, in.Shard.Total, in.Shard.Value)
	}
	// reduce (runner 策略): 把各分片产出按序摆给聚合者。
	if node.Kind == graph.NodeKindReduce && len(in.Shards) > 0 {
		fmt.Fprintf(&extraCtx, "\n\n## 待聚合的分片产出\n%s", formatShardsForPrompt(in.Shards))
	}
	objective := r.objective
	if s := extraCtx.String(); s != "" {
		// objective 与 stage.Prompt **两处都要挂**, 因为 buildStagePromptWithRoles 有
		// 三条互斥路径 (workflow.go:1803):
		//   ① 角色模板已含 {objective} ⇒ **stage.Prompt 被整个丢弃**, 只有 objective 到得了;
		//   ② 纯人格角色 ⇒ merged + stage.Prompt 的替换结果, 两者都到;
		//   ③ 无角色注册表 ⇒ 只有 stage.Prompt 的替换结果到得了。
		// 只挂 objective (改造前对 Feedback 的做法) 在 ③ 和"模板不含 {objective}"的 ②
		// 下会被静默丢掉 —— 分片内容丢了就是 N 片跑同一个输入、产出 N 份重复内容。
		// 挂 stage.Prompt 时避开"模板已含 {objective}"的情形, 免得同一段话出现两次。
		objective += s
		if !strings.Contains(stage.Prompt, "{objective}") {
			stage.Prompt += s
		}
	}
	// 重试让位 (design/01 §4.3, 见文件头"重试单层化"): 图层已持有重试策略
	// (NodeSpec.Retry / Policies.DefaultRetry), 内层 executeStageWithRetry 必须
	// 退化为"1 次 + 1 次校验修正", 否则 图(1+6) × 内层(1+3+限流20) 会变成
	// 嵌套放大 —— 这正是 design/01 §4.3 要消除的东西。
	nodeStart := time.Now()
	sr := r.executorForNode(node).ExecuteSingleStage(WithOuterRetryDriven(ctx), stage, objective, in.PrevOutputs, r.team)
	res := graph.NodeResult{Output: sr.Output, Err: sr.Error}
	r.writeNodeSpan(ctx, node, in, sr, nodeStart)
	if sr.Status == TaskCompleted {
		res.Status = graph.NodeStatusCompleted
	} else {
		res.Status = graph.NodeStatusFailed
	}
	// 动态展开: **只有图显式授予了 Expand 才解析**产出里的子图 (design/01 §4.2)。
	// 解析失败一律当"没给子图"处理 (fail-open): 展开是增量能力, 解析不出来不该把
	// 一个本来成功的分解阶段判失败; 而引擎侧的边界闸会把非法子图拒掉并留痕。
	if node.Expand != nil && res.Status == graph.NodeStatusCompleted {
		if ex := parseStageExpansion(node.ID, node.Agent.Role, sr.Output); ex != nil {
			res.Expansion = ex
			logging.Event(ctx, "graph.expand.parsed", "node", node.ID, "nodes", fmt.Sprintf("%d", len(ex.Nodes)))
		}
	}
	return res
}

// prevDepsWithDynamic = 声明序上游 (orderedPrevDeps) + **运行期才出现的上游**。
//
// orderedPrevDeps 只认声明期的边 (graphNodeDeps 从传入的 spec 抽), 于是动态展开
// 追加的节点 (<父>/<子>) 虽然经重接线成了下游的真实前驱、产出也在 PrevOutputs 里,
// 却不会出现在 DependsOn ⇒ 依赖块把它们整段丢掉 ⇒ "先分解、再逐项执行、再汇总"的
// 汇总阶段看不到任何子任务产出。这里把漏掉的补在声明序之后 (自身按键名升序,
// 保持确定性)。
func prevDepsWithDynamic(deps map[string][]string, nodeID string, prev map[string]string) []string {
	out := orderedPrevDeps(deps, nodeID, prev)
	if len(out) == len(prev) {
		return out
	}
	covered := make(map[string]bool, len(out))
	for _, d := range out {
		covered[d] = true
	}
	extra := make([]string, 0, len(prev)-len(out))
	for k := range prev {
		if !covered[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// orNodeID 归因名兜底: 引擎未给 NodeRef (例如被单元测试直接调用) 时退回节点 ID。
func orNodeID(ref, id string) string {
	if strings.TrimSpace(ref) == "" {
		return id
	}
	return ref
}

// formatShardsForPrompt 把分片产出排成可读的聚合输入 (按分片序, 带状态)。
func formatShardsForPrompt(shards []graph.ShardResult) string {
	var b strings.Builder
	for _, s := range shards {
		if s.Status != graph.NodeStatusCompleted {
			continue // 失败分片不进 prompt: 半截产出只会污染聚合
		}
		fmt.Fprintf(&b, "### 分片 %d (%s)\n%s\n\n", s.Index+1, s.NodeID, s.Output)
	}
	return strings.TrimSpace(b.String())
}

// ---------------------------------------------------------------------------
// 动态展开产出解析 (design/01 §4.2: 收编 GoalTree / WBS / swarm 三套分解)
// ---------------------------------------------------------------------------

// stageSubtask 分解器友好形式的一条子任务。
//
// 字段别名刻意对齐**现有 WBS 产出格式** (orchestrator.go wbsJSONTask:
// tasks[].id/title/role/dependsOn, id 可以是数字) —— 于是既有 planner prompt
// 一个字都不用改, 它的输出就能被展开器吃下, 这才叫"收编三套分解器"而不是
// "再发明第四种格式"。id 复用 flexibleWBSID 以容忍数字 ID。
type stageSubtask struct {
	ID             flexibleWBSID   `json:"id"`
	Role           string          `json:"role,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	Task           string          `json:"task,omitempty"`  // prompt 的别名
	Title          string          `json:"title,omitempty"` // WBS 用 title
	DependsOn      []flexibleWBSID `json:"depends_on,omitempty"`
	DependsOnCamel []flexibleWBSID `json:"dependsOn,omitempty"` // WBS 用 dependsOn
}

// deps 合并两种依赖字段写法。
func (s stageSubtask) deps() []flexibleWBSID {
	if len(s.DependsOn) > 0 {
		return s.DependsOn
	}
	return s.DependsOnCamel
}

// promptText 取任务描述 (prompt / task / title 三种写法)。
func (s stageSubtask) promptText() string {
	for _, v := range []string{s.Prompt, s.Task, s.Title} {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// stageExpansionPayload 阶段产出里可被接受的两种子图形态。
type stageExpansionPayload struct {
	// 原生形态: 与 graph.Expansion 同构 (给能直接产 GraphSpec 的调用方)
	Nodes []graph.NodeSpec `json:"nodes,omitempty"`
	Edges []graph.EdgeSpec `json:"edges,omitempty"`
	// 友好形态: 子任务列表 (三套分解器的共同形状)
	Subtasks []stageSubtask `json:"subtasks,omitempty"`
	Tasks    []stageSubtask `json:"tasks,omitempty"` // subtasks 的别名
}

// parseStageExpansion 从阶段产出里抽取要追加的子图; 抽不到返回 nil。
//
// 只认**平衡的 JSON 对象**候选 (复用 orchestrator.go 的字符串感知扫描器,
// 它能正确跳过字符串里的括号 —— 这正是 REPORT.md 里带 ```json 围栏时踩过的坑),
// 逐个尝试解析, 取第一个能产出节点的。
func parseStageExpansion(parentID, parentRole, output string) *graph.Expansion {
	for _, cand := range extractJSONObjectCandidates(output) {
		var p stageExpansionPayload
		if json.Unmarshal([]byte(cand), &p) != nil {
			continue
		}
		if len(p.Nodes) > 0 {
			return &graph.Expansion{Nodes: p.Nodes, Edges: p.Edges}
		}
		subs := p.Subtasks
		if len(subs) == 0 {
			subs = p.Tasks
		}
		if len(subs) == 0 {
			continue
		}
		ex := &graph.Expansion{}
		ids := map[string]bool{}
		for _, s := range subs {
			id := strings.TrimSpace(string(s.ID))
			if id == "" || ids[id] {
				continue
			}
			ids[id] = true
			role := strings.TrimSpace(s.Role)
			if role == "" {
				role = parentRole // 未给角色: 沿用父节点角色 (最保守的选择)
			}
			ex.Nodes = append(ex.Nodes, graph.NodeSpec{
				ID:    id,
				Kind:  graph.NodeKindAgent,
				Agent: graph.AgentSpec{Role: role, Prompt: s.promptText()},
			})
		}
		if len(ex.Nodes) == 0 {
			continue
		}
		for _, s := range subs {
			id := strings.TrimSpace(string(s.ID))
			if !ids[id] {
				continue
			}
			deps := 0
			for _, raw := range s.deps() {
				d := strings.TrimSpace(string(raw))
				if d == "" || d == id || !ids[d] {
					continue // 只认兄弟依赖: 指向图外节点的依赖会被引擎的边界闸整段拒掉
				}
				ex.Edges = append(ex.Edges, graph.EdgeSpec{From: d, To: id})
				deps++
			}
			if deps == 0 {
				// 无依赖的子任务直接挂在父节点下 —— 必须有这条边, 否则它是入度 0 的
				// 悬空节点, 会脱离父节点抢先执行 (引擎的可达性闸也会整段拒绝)。
				ex.Edges = append(ex.Edges, graph.EdgeSpec{From: parentID, To: id})
			}
		}
		return ex
	}
	return nil
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
	gateStart := time.Now()
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
		out := fmt.Sprintf(`{"gate":"deterministic","score":%.0f}`, score)
		r.writeGateSpan(ctx, node, in, "deterministic", score, score >= 75, deliverable, out, gateStart)
		return graph.NodeResult{
			Status: graph.NodeStatusCompleted,
			Score:  score,
			Output: out,
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
			r.writeGateSpan(ctx, node, in, "content", float64(v.Score), v.Score >= 75, deliverable, string(out), gateStart)
			return graph.NodeResult{
				Status: graph.NodeStatusCompleted,
				Score:  float64(v.Score),
				Output: string(out),
			}
		}
	}
	// 评审不可用: 给中性分, 不阻断 (fail-open)。
	// **这一支也要留轨迹**: "评审器不可用所以放行"与"评审通过"在事后必须可区分,
	// 否则一段时间的 LLM 故障会在学习数据里表现为"这些产出质量都还行"。
	r.writeGateSpan(ctx, node, in, "unavailable", 60, true, deliverable, `{"gate":"unavailable","score":60}`, gateStart)
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
	// internalHits 节点 → 内置 hook 名 → 干预次数 (design/01 §4.5 turn|tool 作用域)。
	// 只计数不存明细, 理由见 graph_internal_bridge.go 的 noteInternalHook。
	internalHits map[string]map[string]int
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

// seedFromJournal 用 journal 投影给 flush() 的快照打底 (design/01 §六 team.json
// 投影化的**运行期**接线点; 默认关, 见 team_projection.go)。
//
// 修的是一处**永久性漂移**: resume 时 Replay 命中缓存的节点不会被派发 ⇒ 一条 hook 都
// 不发 ⇒ 不进 h.byID ⇒ flush() 用"这一轮真跑了的那几个"整体覆盖 team.Stages。
// 正常收尾时 run_interceptors.go:331 会用引擎返回值 (含缓存命中) 覆盖回来, 但这一轮
// **又失败**时走的是 failTeam —— 它不碰 Stages, 截断后的 Stages 就永久落盘了。
//
// 只打底不覆盖: 本轮真跑的节点在 nodeStarted/nodeFinished 里会把同名条目改写掉,
// 于是"实测耗时/产出"永远来自本轮 hook, journal 投影只负责补上没跑的那些。
//
// **只认还在图里的节点**: 与 engine.go 重建展开记录时那道闸同一口径 ("展开记录的父节点
// 已不在图里 ⇒ 整段丢弃")。工作流改过之后, journal 里的老节点不会再被 Replay 命中,
// 把它们打进快照等于在 dashboard 上显示一个这张图里根本没有的阶段。
func (h *teamGraphHooks) seedFromJournal(dataDir string) int {
	if h == nil || h.team == nil || !teamProjectionEnabled() {
		return 0
	}
	p, ok := projectTeamProgressFromJournal(dataDir)
	if !ok {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	seeded := 0
	for _, s := range p.Stages {
		if _, declared := h.roles[baseNodeID(s.Name)]; !declared {
			continue
		}
		if _, live := h.byID[s.Name]; live {
			continue
		}
		// Role 不在 journal 里 (它是声明, 见 team_projection.go 文件头二): 按声明侧补。
		if s.Role == "" {
			s.Role = h.roles[baseNodeID(s.Name)]
		}
		h.byID[s.Name] = s
		h.trackDynamic(s.Name)
		seeded++
	}
	return seeded
}

// Emit 实现 graph.HookBus。恒返回放行决策。
func (h *teamGraphHooks) Emit(ctx context.Context, ev graph.HookEvent) graph.HookDecision {
	switch ev.Scope {
	case graph.ScopeGraph:
		if ev.Phase == "stalled" {
			// 图级 watchdog 判定停滞 (design/01 §4.3, graph/watchdog.go)。
			// 单独一支是因为载荷键完全不同: 走上面那行会打出一条四个字段全空的
			// `graph.stalled` —— 那种日志比没有更糟 (看着有事件, 什么都没说)。
			// **只打日志不改任何执行**: 干预 (action=fail) 由引擎在取消路径上做,
			// 这个观测桥一如既往恒放行。
			logging.For("graph").Warn("图级停滞", "team", h.teamName(),
				"graph", payloadStr(ev.Payload, "graph"),
				"stall_ms", payloadStr(ev.Payload, "stall_ms"),
				"threshold_ms", payloadStr(ev.Payload, "threshold_ms"),
				"action", payloadStr(ev.Payload, "action"),
				"repeat", payloadStr(ev.Payload, "repeat"),
				"events", payloadStr(ev.Payload, "events"))
			break
		}
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
	case graph.ScopeTurn, graph.ScopeTool, graph.ScopeSession:
		// design/01 §4.5: internal_hook 保留在引擎内、外部 hook 保留在 pkg/hooks,
		// 但两者的事件都注册进同一总线。这里**只聚合计数**, 不落盘不打日志 ——
		// 一次团队运行会有成千上万条, 逐条处理会把最热路径与日志双双淹掉。
		// 见 graph_internal_bridge.go / graph_external_bridge.go。
		//
		// session 与 turn|tool 走同一支而不是单开一支: 图层对这三类的用法完全相同
		// (按节点 × hook 名计数)。单开一支就要再写一份计数表与一个日志字段, 而
		// "这个阶段一共被干预了多少次"就得靠调用方相加 —— 相加一定有人漏做。
		h.noteInternalHook(ev)
	}
	// 恒放行。turn|tool|session 事件更是纯观测: 桥接方也已把决策丢弃 (双保险)。
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
	h.trackDynamic(ev.NodeID)
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
	h.trackDynamic(ev.NodeID)
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
	kv := []string{"team", h.teamName(), "node", ev.NodeID,
		"role", sr.Role, "status", string(sr.Status), "attempts", fmt.Sprintf("%d", attempts),
		"iterations", fmt.Sprintf("%d", iterations), "duration_ms", fmt.Sprintf("%d", durMs),
		"output_len", fmt.Sprintf("%d", len(sr.Output)), "error", sr.Error}
	// 内置 hook 干预摘要 (§4.5 turn|tool)。**为空时一个字段都不加**:
	// 没有干预的运行, 这行日志逐字节与改造前一致 (下游有按日志比对的验收)。
	if s := h.internalHookSummary(ev.NodeID); s != "" {
		kv = append(kv, "internal_hooks", s)
	}
	logging.Event(ctx, "graph.node."+ev.Phase, toAnySlice(kv)...)
}

// toAnySlice 把 k/v 串切片转成 logging.Event 的可变参形态。
// 单独提出来是因为 logging.Event 收 ...any 而这里需要先条件追加再一次性传入。
func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// trackDynamic 把运行期才出现的节点 ID 追加进刷盘序 (调用方须持 h.mu)。
//
// 声明期拿不到这些 ID: map 分片 (<阶段>#<序号>)、loop-group 组内成员
// (<组>#it<轮次>/<成员>)、动态展开产物 (<父>/<子>) 都是引擎在运行中合成的。
// 不追踪的后果是 flush 按 h.order 过滤时把它们整个丢掉 —— dashboard 上一个 8 分片
// 的扇出会显示成"什么都没跑", 与图侧 hook 已通电的初衷相违。
func (h *teamGraphHooks) trackDynamic(id string) {
	if _, known := h.roles[id]; known {
		return
	}
	for _, existing := range h.order {
		if existing == id {
			return
		}
	}
	h.order = append(h.order, id)
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
			recordTeamRun(mc, "team", metrics.MTeamStageDurationSec, d.Seconds(), team, labels)
		}
	}
	recordTeamRun(mc, "team", metrics.MTeamStageCount, 1, team, labels)
	if sr.Status == TaskCompleted {
		recordTeamRun(mc, "team", metrics.MTeamStageSuccessCount, 1, team, labels)
	} else {
		recordTeamRun(mc, "team", metrics.MTeamStageFailCount, 1, team, labels)
	}
	if retries > 0 {
		recordTeamRun(mc, "team", metrics.MTeamStageRetryCount, float64(retries), team, labels)
	}
	if sr.Output != "" {
		recordTeamRun(mc, "team", metrics.MTeamStageOutputLen, float64(len(sr.Output)), team, labels)
	}
}

// ---------------------------------------------------------------------------
// 图引擎执行入口
// ---------------------------------------------------------------------------

// executeGraph 图引擎执行入口 (wf.Mode=="graph" 或灰度开关命中时由 Execute/executePipeline 转入)。
// Journal 落 <team.dataDir>/graph-journal.jsonl; Resume 恒开 —— 重放即恢复,
// 取代 pipeline 路径的 checkpoints.json (design/01 §4.3)。
func (we *WorkflowExecutor) executeGraph(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	// 图的来源单一入口 (design/01 §五): 该 mode 有等价图模板就用模板展开的阶段序列,
	// 否则直译 wf.Stages。模板库本身在 graph_templates.go —— 加一个 mode 模板不必碰这里。
	spec, err := graphSpecForWorkflow(wf)
	if err != nil {
		return nil, err
	}
	return we.runGraphSpec(ctx, wf, spec, objective, team, func(s graph.GraphSpec) graph.NodeRunner {
		return &stageNodeRunner{we: we, team: team, objective: objective, deps: graphNodeDeps(s)}
	})
}

// runGraphSpec 图引擎执行的公共骨架: journal + hook 桥 + 拦截器链 + 结果回译。
//
// 为什么把 runner 做成参数而不是写死 stageNodeRunner: design/01 M4 退役 pkg/orchestrator
// 后, orchestrated 模式要在**同一套**调度/journal/hook/预算之上跑
// 一个不同的执行内核 (裸 LLM completion, 无工具, 见 workflow_orchestrated.go)。
// 若为它另抄一份引擎装配, journal 目录、hook 桥、拦截器链、结果回译四处都会各自
// 漂移 —— 那正是这轮归一要消除的形态 (两套调度系统并存)。
//
// newRunner 收**最终 spec** 而不是在外面先造好: stageNodeRunner 需要从 spec 抽依赖
// 声明序 (graphNodeDeps), 而 spec 的来源 (模板/直译/mode 专属构造) 由调用方决定。
func (we *WorkflowExecutor) runGraphSpec(
	ctx context.Context, wf *WorkflowDef, spec graph.GraphSpec, objective string,
	team *ProductionTeam, newRunner func(graph.GraphSpec) graph.NodeRunner,
) ([]StageResult, error) {
	// 图级 watchdog (design/01 §4.3): 默认关, 环境开启后才装进 Policies。
	// 必须在 eng.Run (内部 Validate) 之前、且在三条 spec 来源 (模板/覆盖表/动态注册)
	// 汇合之后 —— 这里正是那个汇合点。见 graph_watchdog.go。
	applyGraphWatchdog(&spec)

	// NewFileJournal 收目录名, 内部落 <dir>/journal.jsonl
	journalDir := filepath.Join(team.dataDir, graphJournalDirName)
	journal, err := graph.NewFileJournal(journalDir)
	if err != nil {
		return nil, fmt.Errorf("graph_adapter: 打开 journal 失败: %w", err)
	}
	defer journal.Close() // 不关会每跑一次团队泄一个 fd

	hooks := newTeamGraphHooks(we, team, spec)
	// team.json 的运行进度以 journal 为准 (design/01 §六 投影化; 默认关)。
	// 必须在 eng.Run **之前**: 引擎一旦开始跑, 第一个 nodeStarted 就会 flush 一次,
	// 那时快照里若还没有缓存命中的历史阶段, dashboard 上就已经闪过一次"阶段变少"。
	if n := hooks.seedFromJournal(team.dataDir); n > 0 {
		logging.Event(ctx, "graph.projection.seed", "team", team.Name,
			"stages", fmt.Sprintf("%d", n), "source", "graph-journal")
	}
	eng := &graph.Engine{
		Runner:  newRunner(spec),
		Journal: journal,
		Hooks:   hooks, // 生产此前恒 NopBus: 图模式没有阶段级刷盘/指标/心跳
		// 并发上限与 pipeline 侧对齐: effectiveParallel 会按 API 流控状态动态收敛
		// (图引擎自己的默认是硬编码 4)。覆盖表声明的 Policies.MaxParallel 优先于此。
		MaxParallel: we.effectiveParallel(),
		// 拦截器链 (design/01 §4.10)。此前生产恒空链 = 切面建成未通电。
		Interceptors: graphInterceptors(we, team, spec),
	}
	runID := trace.From(ctx).RunID
	if runID == "" {
		runID = trace.NewRunID(team.Name)
	}
	// design/01 §4.5: 让引擎内 internal_hook 的 turn|tool 事件进**同一条**总线。
	// 挂在 ctx 上而不是各 runner 里 —— ctx 从这里一路流到 sessionAgentRunner →
	// engine.Query → queryLoop 的 HookChain, 中间各层无需知情。
	ctx = withInternalHookBridge(ctx, hooks, runID)
	// design/01 §4.5 归宿表第 1 行: 外部 shell/HTTP/gRPC/OPA hook 的事件也进**同一条**
	// 总线 (ExternalHook 适配器, 见 graph_external_bridge.go)。同一个 ctx 通道两个
	// 观测者互不干扰: 各自用自己的 ctx 键。
	ctx = withExternalHookBridge(ctx, hooks, runID)
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
		Params:    graphRunParams(team, wf, runID),
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
		// 黑板回写与 pipeline 路径对齐 (下游 harvest/handoff 依赖 <stage>-result 键)。
		// -status 这条此前图路径漏写: pipeline 的 executeStage (workflow.go) 与旧
		// orchestrated 的 convertResults 都写, 只有图路径不写 —— 同一团队在灰度开关
		// 两侧黑板内容不同形。补上, 键名/分类/作者与 pipeline 侧逐字一致。
		if team.Blackboard != nil {
			if nr.Status == graph.NodeStatusCompleted {
				team.Blackboard.Write(n.ID+"-result", nr.Output, n.Agent.Role, "result")
				team.Blackboard.Write(n.ID+"-status", "completed", "system", "progress")
			} else {
				team.Blackboard.Write(n.ID+"-status", "failed: "+sr.Error, "system", "progress")
			}
		}
	}
	if runErr != nil {
		return results, runErr
	}
	// 两层状态机对齐走**显式映射表** (design/01 §4.3, graph_run_status.go):
	// 改造前这里只认 failed 一个字面量, 其余取值落到隐式 else = "继续交付" ——
	// 于是 suspended (还在等人答复) 会被当成一次成功交付, 而日后新增的图层终态
	// 也会默认放行。未知取值在表里是 fail-closed 的报错, 不是退档。
	verdict, vErr := graphRunStatusVerdict(rr.Status)
	if vErr != nil {
		return results, vErr
	}
	if !verdict.Deliver {
		return results, fmt.Errorf("%s", verdict.Reason)
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

// graphRunParams 图运行参数 (design/01 §4.2 MapPolicy.Source="param:<键>" 的来源)。
//
// 改造前 executeGraph **不传 Params**, 于是 `param:` 来源在生产恒取空 —— 表现为
// "map 节点拿到空集合 → 整节点 skipped", 图层支持了但没通电。
//
// 这里只把**已存在的团队/工作流字段**暴露成参数, 不新造用户可见概念:
// 团队上没有通用 params map, 凭空发明一个会让"参数从哪来"变成一个需要贯穿
// 飞书/dashboard/CLI 三条入口的新设计, 那属另一件事。
//
// 键名带 team./wf./run. 前缀是刻意的: 将来真加了用户参数, 可以直接并进同一个 map
// 而不会与这些派生量撞名。
func graphRunParams(team *ProductionTeam, wf *WorkflowDef, runID string) map[string]string {
	p := map[string]string{"run.id": runID}
	if team != nil {
		p["team.name"] = team.Name
		p["team.objective"] = team.Objective
		if team.Cwd != "" {
			p["team.cwd"] = team.Cwd
		}
		if team.Language != "" {
			p["team.language"] = team.Language
		}
		if team.PendingFeedback != "" {
			// 精修反馈: 让模板能把它当集合切分 (逐条反馈扇出) 而不只是拼进 prompt。
			p["team.feedback"] = team.PendingFeedback
		}
	}
	if wf != nil {
		p["wf.name"] = wf.Name
		p["wf.mode"] = wf.Mode
	}
	return p
}

// invalidateGraphJournal 给图 journal 追加节点失效事件 (design/01 §4.3 InvalidateFrom)。
//
// 只在 journal 目录已存在时动手: 目录不存在说明这个团队没走过图引擎, 凭空建一个
// 空 journal 会让下一次 Run 的 Resume 读到一个"有文件但无事件"的基线 —— 无害但
// 会让排查时误以为跑过图。
//
// 失效对象用**阶段名**: TranslateWorkflow 把阶段名原样用作节点 ID, 所以
// stagesFromTarget 给出的名字就是节点 ID (派生 ID 带前缀, 由 Replay 侧一并摘掉)。
//
// 失败只记日志不阻断精修: 精修本身还有 checkpoints.json 与 PendingFeedback 两条腿,
// 因为写不进一条事件就把用户的精修请求整个拒掉是过度反应。但**必须留痕** ——
// 静默失败正是这个 bug 原本的形态。
func invalidateGraphJournal(ctx context.Context, team *ProductionTeam, stages []string, reason string) {
	if team == nil || team.dataDir == "" || len(stages) == 0 {
		return
	}
	dir := filepath.Join(team.dataDir, graphJournalDirName)
	if _, err := os.Stat(dir); err != nil {
		return // 没走过图引擎
	}
	runID := team.LastRunID
	if runID == "" {
		// 没有 RunID 就写不出能被 Replay 认领的事件 (它按最近一次 run 过滤)。
		// 退回到"整段清掉"这个粗粒度但确定生效的做法, 而不是写一条注定被过滤的事件。
		logging.Event(ctx, "graph.invalidate.fallback", "team", team.Name, "reason", reason,
			"detail", "无 LastRunID 无法定位 run, 改为清空 graph-journal")
		if err := os.RemoveAll(dir); err != nil {
			logging.Event(ctx, "graph.invalidate.error", "team", team.Name, "err", err.Error())
		}
		return
	}
	j, err := graph.NewFileJournal(dir)
	if err != nil {
		logging.Event(ctx, "graph.invalidate.error", "team", team.Name, "err", err.Error(),
			"detail", "打开 graph-journal 失败, 按阶段失效未生效")
		return
	}
	defer j.Close()
	if err := graph.InvalidateFrom(j, runID, stages, reason); err != nil {
		logging.Event(ctx, "graph.invalidate.error", "team", team.Name, "err", err.Error(),
			"detail", "写入节点失效事件失败")
		return
	}
	logging.Event(ctx, "graph.invalidate", "team", team.Name, "run", runID, "reason", reason,
		"nodes", fmt.Sprintf("%d", len(stages)), "stages", strings.Join(stages, ","))
}

// writeNodeSpan 采集 node 轨迹 (design/03 §4.1 KindNode)。
//
// 为什么必须有它: `tracestore.KindNode` 常量早就在, 但**全仓零产生方** —— 于是
// design/03 的轨迹底座只有 llm_call 与 gate 两类。缺了 node 这一层, 奖励就无法
// 归因到具体节点 (只能归到整个 run 或某次 LLM 调用), 而"哪个阶段该改"正是结构
// 学习器 (§4.3 d/e) 要回答的问题。加一个常量不等于采集了那类轨迹。
//
// 挂在 stageNodeRunner 而非引擎侧: 引擎 (pkg/graph) 不认识 tracestore, 且这里才
// 同时拿得到节点声明与 StageResult (角色/状态/产出/重试)。
//
// fail-open: traceStore 为 nil 或写失败都不影响节点执行 —— 采集是观测, 不是治理。
func (r *stageNodeRunner) writeNodeSpan(ctx context.Context, node graph.NodeSpec, in graph.NodeInput, sr StageResult, start time.Time) {
	if r == nil || r.we == nil || r.we.traceStore == nil {
		return
	}
	ids := trace.From(ctx)
	attrs := map[string]any{
		"kind":   string(node.Kind),
		"role":   node.Agent.Role,
		"status": string(sr.Status),
	}
	if r.team != nil {
		attrs["team"] = r.team.Name
	}
	// 轮次/分片进 attrs: 同一节点在 loop/map 下会产多条 span, 不带轮次无法区分,
	// 也就无法按"第几轮才收敛"做结构学习。
	if in.Iteration > 0 {
		attrs["iteration"] = in.Iteration
	}
	if in.GroupIteration > 0 {
		attrs["group_iteration"] = in.GroupIteration
	}
	if in.Shard != nil {
		attrs["shard_index"] = in.Shard.Index
		attrs["shard_total"] = in.Shard.Total
	}
	// NodeID 用**限定 ID**(NodeRef): 分片与组内成员的 span 必须能各自归因,
	// 用声明名会把 N 个分片的 span 全挂在同一个节点上。
	nodeRef := orNodeID(in.NodeRef, node.ID)
	r.we.traceStore.Write(tracestore.Span{
		TraceID:   ids.RunID,
		SpanID:    newSpanID(),
		ParentID:  ids.RunID,
		Kind:      tracestore.KindNode,
		Name:      node.ID,
		NodeID:    nodeRef,
		InputRef:  r.we.traceStore.MakeRef(stageSpanInput(node, in)),
		OutputRef: r.we.traceStore.MakeRef(sr.Output),
		Attrs:     attrs,
		TS:        start.UnixMilli(),
		DurMS:     time.Since(start).Milliseconds(),
	})
}

// stageSpanInput 节点输入的可读摘要 (进 InputRef, 长文本由 MakeRef 内容寻址)。
// 只取"这个节点看到了什么", 不重复整图 objective —— 后者在 run 级已有。
func stageSpanInput(node graph.NodeSpec, in graph.NodeInput) string {
	var b strings.Builder
	b.WriteString("role=" + node.Agent.Role + "\n")
	if in.Feedback != "" {
		b.WriteString("feedback=" + in.Feedback + "\n")
	}
	if in.Shard != nil {
		b.WriteString("shard=" + in.Shard.Value + "\n")
	}
	for _, dep := range sortedKeys(in.PrevOutputs) {
		b.WriteString("--- from " + dep + " ---\n" + in.PrevOutputs[dep] + "\n")
	}
	return b.String()
}

// sortedKeys 确定性遍历 (span 内容需可比对)。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeGateSpan 图 gate 节点的 gate 轨迹 (design/03 §4.1 KindGate)。
//
// pipeline 侧的门禁 (teams.go / content_gate.go) 已经产 gate Span, 图侧此前没有 ——
// 于是同一个团队在灰度开关两侧会产出**不同形状**的轨迹, 学习管线拿到的数据取决于
// 开关状态。这不是"少一点观测", 是数据地基不一致。
func (r *stageNodeRunner) writeGateSpan(ctx context.Context, node graph.NodeSpec, in graph.NodeInput,
	gateKind string, score float64, pass bool, input, detail string, start time.Time) {
	if r == nil || r.we == nil || r.we.traceStore == nil {
		return
	}
	ids := trace.From(ctx)
	attrs := map[string]any{
		"gate":   gateKind,
		"pass":   pass,
		"score":  score,
		"status": gateSpanStatus(gateKind, pass),
		"path":   "graph", // 与 pipeline 侧同字段但标明来源, 便于比对两条路径
	}
	if r.team != nil {
		attrs["team"] = r.team.Name
	}
	r.we.traceStore.Write(tracestore.Span{
		TraceID:   ids.RunID,
		SpanID:    newSpanID(),
		ParentID:  ids.RunID,
		Kind:      tracestore.KindGate,
		Name:      node.ID,
		NodeID:    orNodeID(in.NodeRef, node.ID),
		InputRef:  r.we.traceStore.MakeRef(input),
		OutputRef: r.we.traceStore.MakeRef(detail),
		Attrs:     attrs,
		TS:        start.UnixMilli(),
		DurMS:     time.Since(start).Milliseconds(),
	})
}

// gateSpanStatus 与 pipeline 侧 gateStatusLabel 口径一致, 外加 unavailable 一档 ——
// "评审器不可用所以放行"必须与"评审通过"可区分。
func gateSpanStatus(gateKind string, pass bool) string {
	if gateKind == "unavailable" {
		return "unavailable"
	}
	if pass {
		return "pass"
	}
	return "fail"
}
