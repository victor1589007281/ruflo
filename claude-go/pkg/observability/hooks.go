package observability

import (
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

// ─── 1. LLMHook —— 大模型调用全生命周期 ──────────────────────────────────────

// LLMCallEvent LLM 调用事件 Payload。
type LLMCallEvent struct {
	Record      api.LLMCallRecord
	TraceID     string
	SpanID      string
	TeamID      string
	StageName   string
	AgentRole   string
	PromptHash  string // 提示词版本哈希
	PromptLen   int    // 提示词长度(字符)
	ResponseLen int    // 响应长度(字符)
}

// LLMHook 观测每次 LLM 调用的输入、输出、延迟、成败。
type LLMHook interface {
	OnLLMCallStart(ev LLMCallEvent)
	OnLLMCallComplete(ev LLMCallEvent)
	OnLLMCallError(ev LLMCallEvent)
	OnLLMCallFallback(primary, fallback string, ev LLMCallEvent)
	OnLLMCacheHit(model, cacheType string, tokens int)
	OnLLMCacheMiss(model, cacheType string, tokens int)
}

// NoopLLMHook 空实现, 可嵌入后选择性覆盖。
type NoopLLMHook struct{}

// OnLLMCallStart no-op, LLM 调用开始。
func (NoopLLMHook) OnLLMCallStart(ev LLMCallEvent)                     {}
// OnLLMCallComplete no-op, LLM 调用正常结束。
func (NoopLLMHook) OnLLMCallComplete(ev LLMCallEvent)                  {}
// OnLLMCallError no-op, LLM 调用失败。
func (NoopLLMHook) OnLLMCallError(ev LLMCallEvent)                     {}
// OnLLMCallFallback no-op, 主模型失败后切换到备用模型。
func (NoopLLMHook) OnLLMCallFallback(primary, fallback string, ev LLMCallEvent) {}
// OnLLMCacheHit no-op, 提示词缓存命中。
func (NoopLLMHook) OnLLMCacheHit(model, cacheType string, tokens int)  {}
// OnLLMCacheMiss no-op, 提示词缓存未命中。
func (NoopLLMHook) OnLLMCacheMiss(model, cacheType string, tokens int) {}

// ─── 2. StageHook —— 工作流 Stage 生命周期 ──────────────────────────────────

// StageEvent Stage 事件 Payload。
type StageEvent struct {
	TraceID       string
	TeamID        string
	Workflow      string
	StageName     string
	StageIndex    int
	AgentRole     string
	ModelAlias    string
	StartTime     time.Time
	EndTime       time.Time
	DurationSec   float64
	Success       bool
	Error         string
	RetryCount    int
	OutputLen     int
	ToolCallCount int
	BuildPassed   bool
	TestPassed    bool
}

// StageHook 观测工作流各阶段的执行。
type StageHook interface {
	OnStageStart(ev StageEvent)
	OnStageComplete(ev StageEvent)
	OnStageFail(ev StageEvent)
	OnStageRetry(ev StageEvent, attempt int, delay time.Duration)
}

type NoopStageHook struct{}

// OnStageStart no-op, Stage 开始。
func (NoopStageHook) OnStageStart(ev StageEvent)                      {}
// OnStageComplete no-op, Stage 成功结束。
func (NoopStageHook) OnStageComplete(ev StageEvent)                   {}
// OnStageFail no-op, Stage 失败。
func (NoopStageHook) OnStageFail(ev StageEvent)                       {}
// OnStageRetry no-op, Stage 重试 (携带第几次尝试与退避时长)。
func (NoopStageHook) OnStageRetry(ev StageEvent, attempt int, delay time.Duration) {}

// ─── 3. TaskHook —— Orchestrator 任务级生命周期 ─────────────────────────────

// TaskEvent 任务事件 Payload。
type TaskEvent struct {
	TraceID      string
	TeamID       string
	TaskID       string
	TaskName     string
	Dependencies []string
	StartTime    time.Time
	EndTime      time.Time
	DurationSec  float64
	Success      bool
	Error        string
	Attempt      int
	Output       string
	TargetFiles  []string
	TargetPackages []string
}

// TaskHook 观测 Orchestrator DAG 中每个任务的执行。
type TaskHook interface {
	OnTaskReady(ev TaskEvent)
	OnTaskStart(ev TaskEvent)
	OnTaskComplete(ev TaskEvent)
	OnTaskFail(ev TaskEvent)
	OnTaskRetry(ev TaskEvent, attempt int, delay time.Duration)
}

type NoopTaskHook struct{}

// OnTaskReady no-op, 任务依赖满足、进入就绪队列。
func (NoopTaskHook) OnTaskReady(ev TaskEvent)                         {}
// OnTaskStart no-op, 任务开始执行。
func (NoopTaskHook) OnTaskStart(ev TaskEvent)                         {}
// OnTaskComplete no-op, 任务成功结束。
func (NoopTaskHook) OnTaskComplete(ev TaskEvent)                      {}
// OnTaskFail no-op, 任务失败。
func (NoopTaskHook) OnTaskFail(ev TaskEvent)                          {}
// OnTaskRetry no-op, 任务重试。
func (NoopTaskHook) OnTaskRetry(ev TaskEvent, attempt int, delay time.Duration) {}

// ─── 4. TeamHook —— 团队运行全生命周期 ──────────────────────────────────────

// TeamEvent 团队事件 Payload。
type TeamEvent struct {
	TraceID       string
	TeamID        string
	TeamName      string
	Workflow      string
	StartTime     time.Time
	EndTime       time.Time
	DurationSec   float64
	Success       bool
	Error         string
	StageCount    int
	LLMCallCount  int
	TotalTokens   int
	CostUSD       float64
	FilesProduced int
}

// TeamHook 观测整个 Agent 团队的运行。
type TeamHook interface {
	OnTeamStart(ev TeamEvent)
	OnTeamComplete(ev TeamEvent)
	OnTeamFail(ev TeamEvent)
	OnTeamStageTransition(fromStage, toStage string, ev TeamEvent)
}

type NoopTeamHook struct{}

// OnTeamStart no-op, 团队启动。
func (NoopTeamHook) OnTeamStart(ev TeamEvent)                          {}
// OnTeamComplete no-op, 团队成功收尾。
func (NoopTeamHook) OnTeamComplete(ev TeamEvent)                       {}
// OnTeamFail no-op, 团队失败。
func (NoopTeamHook) OnTeamFail(ev TeamEvent)                           {}
// OnTeamStageTransition no-op, 团队内部阶段切换。
func (NoopTeamHook) OnTeamStageTransition(from, to string, ev TeamEvent) {}

// ─── 5. CollaborationHook —— 多 Agent 协作交互 ──────────────────────────────

// CollaborationEvent 协作事件 Payload。
type CollaborationEvent struct {
	TraceID     string
	TeamID      string
	FromRole    string
	ToRole      string
	MessageType string // "proposal" | "review" | "consensus" | "handoff"
	ContentHash string
	ContentLen  int
	TokensIn    int
	TokensOut   int
	LatencySec  float64
	Accepted    bool   // 评审是否通过
	Score       float64 // 评审分数 (0-1)
}

// CollaborationHook 观测 Agent 之间的消息传递和协作质量。
type CollaborationHook interface {
	OnCollabMessage(ev CollaborationEvent)
	OnCollabReview(ev CollaborationEvent)
	OnCollabConsensus(ev CollaborationEvent)
	OnCollabHandoff(fromRole, toRole string, contextSummary string, ev CollaborationEvent)
}

type NoopCollaborationHook struct{}

// OnCollabMessage no-op, Agent 间消息投递。
func (NoopCollaborationHook) OnCollabMessage(ev CollaborationEvent)    {}
// OnCollabReview no-op, 评审消息。
func (NoopCollaborationHook) OnCollabReview(ev CollaborationEvent)     {}
// OnCollabConsensus no-op, 达成共识。
func (NoopCollaborationHook) OnCollabConsensus(ev CollaborationEvent)  {}
// OnCollabHandoff no-op, 角色间交接 (附带上下文摘要)。
func (NoopCollaborationHook) OnCollabHandoff(from, to, summary string, ev CollaborationEvent) {}

// ─── 6. PromptHook —— 提示词版本管理与 A/B 测试 ─────────────────────────────

// PromptEvent 提示词事件 Payload。
type PromptEvent struct {
	TraceID      string
	PromptID     string
	Version      string
	PromptHash   string
	TemplateName string
	Variables    map[string]string
	RenderedLen  int
	RenderTimeMs float64
}

// PromptABResult A/B 测试结果。
type PromptABResult struct {
	TraceID     string
	TestID      string
	VariantA    string
	VariantB    string
	MetricName  string // "success_rate" | "latency" | "token_efficiency"
	Winner      string // "A" | "B" | "tie"
	Improvement float64 // 相对提升百分比
	SampleSize  int
}

// PromptHook 观测提示词的渲染、版本、A/B 测试。
type PromptHook interface {
	OnPromptRender(ev PromptEvent)
	OnPromptVersion(promptID, version, hash string, metadata map[string]string)
	OnPromptCompare(v1, v2 string, diffStats map[string]int)
	OnPromptABStart(testID, variantA, variantB string, ev PromptEvent)
	OnPromptABResult(res PromptABResult)
}

type NoopPromptHook struct{}

// OnPromptRender no-op, 提示词模板渲染完成。
func (NoopPromptHook) OnPromptRender(ev PromptEvent)                         {}
// OnPromptVersion no-op, 提示词产生新版本。
func (NoopPromptHook) OnPromptVersion(promptID, version, hash string, md map[string]string) {}
// OnPromptCompare no-op, 两个版本的差异统计。
func (NoopPromptHook) OnPromptCompare(v1, v2 string, diff map[string]int)    {}
// OnPromptABStart no-op, A/B 测试开跑。
func (NoopPromptHook) OnPromptABStart(testID, a, b string, ev PromptEvent)   {}
// OnPromptABResult no-op, A/B 测试出结果。
func (NoopPromptHook) OnPromptABResult(res PromptABResult)                   {}

// ─── HookRegistry —— 统一管理所有 Hook 实例 ─────────────────────────────────

// HookRegistry 持有所有观测 hook 的注册表, 作为 Bus subscriber 的统一分发入口。
type HookRegistry struct {
	LLM           []LLMHook
	Stage         []StageHook
	Task          []TaskHook
	Team          []TeamHook
	Collaboration []CollaborationHook
	Prompt        []PromptHook
}

// RegisterLLM 注册 LLMHook。
func (r *HookRegistry) RegisterLLM(h LLMHook)           { r.LLM = append(r.LLM, h) }
// RegisterStage 注册 StageHook。
func (r *HookRegistry) RegisterStage(h StageHook)       { r.Stage = append(r.Stage, h) }
// RegisterTask 注册 TaskHook。
func (r *HookRegistry) RegisterTask(h TaskHook)         { r.Task = append(r.Task, h) }
// RegisterTeam 注册 TeamHook。
func (r *HookRegistry) RegisterTeam(h TeamHook)         { r.Team = append(r.Team, h) }
// RegisterCollaboration 注册 CollaborationHook。
func (r *HookRegistry) RegisterCollaboration(h CollaborationHook) { r.Collaboration = append(r.Collaboration, h) }
// RegisterPrompt 注册 PromptHook。
func (r *HookRegistry) RegisterPrompt(h PromptHook)     { r.Prompt = append(r.Prompt, h) }

// globalRegistry 全局 hook 注册表。
var globalRegistry = &HookRegistry{}

// GlobalRegistry 返回全局 HookRegistry。
func GlobalRegistry() *HookRegistry { return globalRegistry }

// SetGlobalRegistry 替换全局注册表 (用于测试和初始化)。
func SetGlobalRegistry(r *HookRegistry) { globalRegistry = r }
