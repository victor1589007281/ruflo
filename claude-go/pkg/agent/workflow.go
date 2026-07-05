// Workflow — 多 Agent 协作工作流模式。
//
// 三种核心模式 (参考 CrewAI + LangGraph + AutoGen):
//
//  1. Pipeline (开发): Architect → Coder → Reviewer → Tester (串行依赖)
//  2. Fan-Out (调研): Researcher₁ ∥ Researcher₂ → Synthesizer (并行汇聚)
//  3. Adversarial (辩论): Proposer ↔ Opponent × N轮 → Judge (对抗决策)
//
// 工作流执行器按 stage 依赖拓扑排序, 自动传递上下文。
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/observability"
	"github.com/anthropic/claude-go/pkg/sandbox"
)

// stageTimeout 单阶段执行超时 (防止 agent 无限循环)
const stageTimeout = 10 * time.Minute

// maxWorkflowParallelDefault 默认 workflow 层并行上限
const maxWorkflowParallelDefault = 6

// stageRetryMaxRetries 阶段级重试次数 (API 瞬态错误自动恢复)
const stageRetryMaxRetries = 3

// stageRetryBaseDelay 重试基础退避时间
const stageRetryBaseDelay = 3 * time.Second

// stageRetryMaxDelay 重试最大退避时间
const stageRetryMaxDelay = 120 * time.Second

// antiLoopDirective 防死循环指令，注入到所有 agent prompt 中
const antiLoopDirective = `
## CRITICAL — 防死循环规则
- 你正在参与一个自动化工作流，每个阶段只执行一次。
- 如果上一轮输出已被接受，直接返回上一轮内容，不要重新生成。
- 不要反复解释、道歉或自我修正已通过的步骤。
`

// codeIntelDirective 代码分析铁律: 强制"先索引后精读", 注入到代码类 agent prompt,
// 取代对大仓库的 Read/Grep/Glob 地毯式扫描, 大幅节省 token。
const codeIntelDirective = `
## CRITICAL — 代码分析铁律 (必须遵守, 否则浪费大量 token)
分析任何代码仓库 (尤其大型仓库) 时:
1. **先索引后精读**: 先用 code_intel_status 确认索引就绪; 未就绪则用 code_intel_init 建一次索引 (由外部 CLI 完成, 不消耗 LLM token)。
2. **用知识图谱精准定位**: 用 code_intel_query 以自然语言查询相关执行流/符号/调用关系/影响面, 拿到精确文件与位置。
3. **只精读命中文件**: 仅对 code_intel_query 指向的少量文件做 Read (尽量带行号范围)。
4. **严禁地毯式扫描**: 不要用 Glob/Grep/Read 遍历整个仓库; code_intel_query 已能回答时不要再翻文件。
`

// isCodeAnalysisRole 判定角色是否为"代码类"(需要读/分析源码), 用于决定是否注入 codeIntelDirective。
func isCodeAnalysisRole(role *RoleDef) bool {
	if role == nil {
		return false
	}
	hay := strings.ToLower(role.Name + " " + role.Description + " " + strings.Join(role.Tags, " "))
	for _, kw := range []string{"source", "code", "源码", "coder", "architect", "review", "tester", "implement", "debug", "refactor", "analyst"} {
		if strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

// maxDepOutputLen 每个依赖阶段输出注入 prompt 的最大字符数, 防止上下文膨胀。
const maxDepOutputLen = 1500

// mysqlBuildState MySQL 编译状态跟踪 (避免重复 cmake configure)。
var mysqlBuildState = struct {
	sync.Mutex
	configured map[string]bool
}{
	configured: make(map[string]bool),
}

// mysqlEssentialTargets MySQL 首次编译必须构建的最小核心目标。
var mysqlEssentialTargets = []string{
	"mysys",     // 底层系统库
	"clientlib", // MySQL 客户端库
	"heap",      // MEMORY 存储引擎
	"csv",       // CSV 存储引擎
	"innobase",  // InnoDB 存储引擎
	"myisam",    // MyISAM 存储引擎
}

// workflowRegistry 工作流名 → 构造函数. 注意: 工作流定义本身不带状态,
// 每次按需 new 一份, 避免不同 team 之间共享同一 WorkflowDef 引用.
var workflowRegistry = map[string]func() *WorkflowDef{
	"development":  developmentWorkflow,
	"app":          appCompositeWorkflow,
	"game":         gameCompositeWorkflow,
	"code-review":  codeReviewWorkflow,
	"testing":      testingWorkflow,
	"creative-v2":  creativeV2Workflow,
	"trading-v2":   tradingV2Workflow,
	"sector-scan":  sectorScanWorkflow,
	"industry-map": industryMapWorkflow,
	"novel-v2":     novelV2Workflow,
	"novel-v3":     novelV3Workflow,
	"ml-training":  mlTrainingWorkflow,
	"hiring":       hiringWorkflow,
	"parenting":    parentingWorkflow,
	"research":     researchWorkflow,
	"finance":      financeWorkflow,
	"mr-worker":        mrWorkerWorkflow,        // market-radar 手脚(gemma), 单 agent
	"quant-strategist": quantStrategistWorkflow, // market-radar 大脑(kimi), 单 agent
	"mr-chain":         mrChainWorkflow,         // market-radar 产业链建模(kimi), 单 agent
	"techblog":     techBlogWorkflow,
	"creative":                       creativeWorkflow,
	"manager-lab-rehearsal-npc":      managerLabRehearsalNPCWorkflow,
	"manager-lab-rehearsal-critic":   managerLabRehearsalCriticWorkflow,
	"manager-lab-simulation-v2":      managerLabSimulationV2Workflow,
}

// GetWorkflow 获取工作流: 先查内置, 再查运行时注册的动态工作流. 都没有返回 nil.
func GetWorkflow(name string) *WorkflowDef {
	if factory, ok := workflowRegistry[name]; ok {
		return factory()
	}
	return getCustomWorkflow(name)
}

// ListWorkflows 列出所有可用工作流 (内置 + 动态, 按名称排序, 便于 CLI/API 输出稳定).
func ListWorkflows() []WorkflowDef {
	return mergedWorkflowList()
}

// executeFanOut 并行扇出 → 汇聚 (简化版: 直接复用 pipeline 执行)。
func (we *WorkflowExecutor) executeFanOut(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	return we.executePipeline(ctx, wf, objective, team)
}

// executeAdversarial 对抗辩论执行 (简化版: 直接复用 pipeline 执行)。
func (we *WorkflowExecutor) executeAdversarial(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	return we.executePipeline(ctx, wf, objective, team)
}

type PromptCache struct {
	mu           sync.RWMutex
	staticPrefix string // 不变内容: system prompt + tool defs + repo context
	prefixHash   string // SHA256 用于缓存追踪
	cacheHits    int64  // 命中次数 (同一 prefix 复用)
	cacheMisses  int64  // 未命中 (prefix 变化)
}

// UpdatePrefix 更新静态前缀（对应测试中的 PromptCache API）。
func (p *PromptCache) UpdatePrefix(prefix string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.staticPrefix = prefix
	p.prefixHash = fmt.Sprintf("%x", sha256.Sum256([]byte(prefix)))[:16]
	p.cacheMisses++
}

// BuildPrompt 组合 static + dynamic 内容（对应测试中的 PromptCache API）。
func (p *PromptCache) BuildPrompt(dynamic string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.staticPrefix + "\n" + dynamic
}

// HitRate 返回缓存命中率（供测试/观测使用）。
func (p *PromptCache) HitRate() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	total := p.cacheHits + p.cacheMisses
	if total == 0 {
		return 0
	}
	return float64(p.cacheHits) / float64(total)
}

// maxCodeDepOutputLen 代码/HTML 类依赖件注入下游(审查/渲染)stage 时的上限。
// 远大于 maxDepOutputLen: 评审/渲染必须看到完整产物。1500 字符会把 17KB 的 HTML 砍到只剩
// <head>+场景1, 导致 art-director 把"看不到的后续场景"误判成"内容缺失", 对抗循环永不通过。
const maxCodeDepOutputLen = 60000

// looksLikeCodeArtifact 判断依赖输出是否为需完整传递的代码/HTML 产物 (而非可摘要的散文)。
func looksLikeCodeArtifact(s string) bool {
	h := strings.ToLower(s)
	for _, m := range []string{"<!doctype", "<html", "<section", "<svg", "<style", "<script", "```"} {
		if strings.Contains(h, m) {
			return true
		}
	}
	return false
}

// summarizeDependency 为依赖注入选择策略: 代码/HTML 产物保留原文(大上限, 供完整审查/渲染),
// 普通文本走关键行摘要(小上限, 防上下文膨胀)。
func summarizeDependency(output string) string {
	if looksLikeCodeArtifact(output) {
		if len(output) > maxCodeDepOutputLen {
			return output[:maxCodeDepOutputLen] + "\n...(truncated)"
		}
		return output
	}
	return SummarizeOldOutput(output, maxDepOutputLen)
}

func SummarizeOldOutput(output string, maxLen int) string {
	if len(output) <= maxLen {
		return output
	}
	lines := strings.Split(output, "\n")
	var summary []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		isKey := strings.HasPrefix(line, "#") || strings.HasPrefix(line, "func ") ||
			strings.HasPrefix(line, "type ") || strings.Contains(line, "决策") ||
			strings.Contains(line, "结论") || strings.Contains(line, "推荐") ||
			strings.Contains(line, "错误") || strings.Contains(line, "FAIL") ||
			strings.HasPrefix(line, "- [") || strings.HasPrefix(line, "│")
		if isKey {
			summary = append(summary, line)
		}
	}
	result := strings.Join(summary, "\n")
	if len(result) > maxLen {
		result = result[:maxLen]
	}
	if result == "" {
		result = output[:maxLen]
	}
	return "[摘要] " + result
}

type WorkflowDef struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Mode        string     `json:"mode"`             // pipeline, fanout, adversarial, adversarial_dev, orchestrated, ...
	Stages      []StageDef `json:"stages,omitempty"` // pipeline/fanout 模式
	Rounds      int        `json:"rounds,omitempty"` // adversarial 模式的对抗轮数

	// 动态工作流(运行时定义)用的声明式元数据。内置工作流通过名字白名单判定门禁(见 teams.go /
	// content_gate.go), 自定义工作流则读下列字段——否则按名字判定会让自定义工作流静默丢门禁。
	ProducesCode bool   `json:"producesCode,omitempty"` // 是否跑编译/测试门禁
	QualityGate  string `json:"qualityGate,omitempty"`  // ""|"content"|"none": 内容质量门禁策略
	Custom       bool   `json:"custom,omitempty"`       // 运行时注册的动态工作流标记
}

type StageDef struct {
	Name      string   `json:"name"`                // 阶段名称
	Role      string   `json:"role"`                // agent 角色
	Prompt    string   `json:"prompt,omitempty"`    // 系统提示词模板 (支持 {objective}, {prev_result}, {user_feedback} 占位符)
	DependsOn []string `json:"dependsOn,omitempty"` // 依赖的前置阶段
	Parallel  bool     `json:"parallel,omitempty"`  // 是否可与同级并行
}

type WorkflowExecutor struct {
	factory          CreateAgentFunc
	planCfgResolver  *PlanConfigResolver // 模型/连接参数解析器 (可选, 按 plan+role 层级解析)
	notify           NotifyFunc
	chatID           string
	llm              LLMClient                                                            // LLM 客户端 (供 swarm_intel.Engine 等需要直接调用的场景)
	taskTracker      TaskTracker                                                          // 复用 V2 Task 系统 (可为 nil)
	dagTracker       DAGTaskTracker                                                       // V2 DAG 能力 (运行时从 taskTracker 检测)
	evolution        *EvolutionEngine                                                     // 自动进化引擎 (可为 nil)
	roles            *RoleRegistry                                                        // 角色注册表 (可为 nil, 降级用 StageDef.Prompt)
	metrics          *metrics.Collector                                                   // 持续观测指标 (可为 nil)
	pool             *AgentPool                                                           // Agent 池 (动态扩缩, 可为 nil)
	checkpoints      CheckpointStore                                                      // 检查点存取 (由 Coordinator 注入, 可为 nil)
	promptCache      *PromptCache                                                         // 提示词缓存 (参考 Anthropic Prompt Caching)
	concurrency      ConcurrencySuggestor                                                 // 动态并发建议 (基于 API 流控状态, 可为 nil)
	activityCallback func()                                                               // 活动回调: Coordinator watchdog 心跳 (可为 nil)
	progressCallback func(phase string, iteration int, bytesWritten int64, taskID string) // 进展上报 (可为 nil)
}

func (we *WorkflowExecutor) tryInitDAG() {
	if we.dagTracker != nil || we.taskTracker == nil {
		return
	}
	if dag, ok := we.taskTracker.(DAGTaskTracker); ok {
		we.dagTracker = dag
	}
}

func (we *WorkflowExecutor) Execute(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	start := time.Now()
	traceCtx := observability.NewRootTrace().WithBaggage("team_id", team.Name).WithBaggage("workflow", wf.Mode)
	ctx = observability.WithTrace(ctx, traceCtx)

	observability.Emit(observability.Event{
		Type:      observability.EvtTeamStart,
		Timestamp: start,
		TraceID:   traceCtx.TraceID,
		Module:    "workflow",
		Name:      wf.Mode,
		Payload: map[string]interface{}{
			"team_id":     team.Name,
			"workflow":    wf.Mode,
			"objective":   objective,
			"stage_count": len(wf.Stages),
		},
	})

	var results []StageResult
	var err error
	defer func() {
		dur := time.Since(start).Seconds()
		ev := observability.Event{
			Type:      observability.EvtTeamComplete,
			Timestamp: time.Now(),
			TraceID:   traceCtx.TraceID,
			Module:    "workflow",
			Name:      wf.Mode,
			Payload: map[string]interface{}{
				"team_id":      team.Name,
				"workflow":     wf.Mode,
				"duration_sec": dur,
				"stage_count":  len(results),
				"success":      err == nil,
				"error":        "",
			},
		}
		if err != nil {
			ev.Type = observability.EvtTeamFail
			ev.Payload["error"] = err.Error()
			ev.Payload["success"] = false
		}
		observability.Emit(ev)
	}()

	switch wf.Mode {
	case "pipeline":
		return we.executePipeline(ctx, wf, objective, team)
	case "fanout":
		return we.executeFanOut(ctx, wf, objective, team)
	case "adversarial":
		return we.executeAdversarial(ctx, wf, objective, team)
	case "adversarial_dev":
		return we.executeAdversarialDev(ctx, wf, objective, team)
	case "trading_debate":
		return we.executeTradingDebate(ctx, wf, objective, team)
	case "creative_media":
		return we.executeCreativeMedia(ctx, wf, objective, team)
	case "novel_writing":
		return we.executeNovelWriting(ctx, wf, objective, team)
	case "swarm_novel":
		return we.executeSwarmNovel(ctx, wf, objective, team)
	case "orchestrated":
		return we.executeOrchestrated(ctx, wf, objective, team)
	case "app_composite":
		return we.executeAppComposite(ctx, wf, objective, team)
	case "game_composite":
		return we.executeGameComposite(ctx, wf, objective, team)
	default:
		return we.executePipeline(ctx, wf, objective, team)
	}
}

func (we *WorkflowExecutor) savePhaseCheckpoints(results []StageResult) {
	if we.checkpoints == nil {
		return
	}
	for _, r := range results {
		if r.Name == "" {
			continue
		}
		if r.Status == TaskCompleted {
			we.checkpoints.SaveCheckpoint(r.Name, "completed", 0, r.Output)
		} else if r.Status == TaskFailed {
			we.checkpoints.SaveCheckpoint(r.Name, "failed", 0, r.Error)
		}
	}
}

func (we *WorkflowExecutor) flushStagesLive(team *ProductionTeam, results []StageResult) {
	if team == nil || len(results) == 0 {
		return
	}
	cp := make([]StageResult, len(results))
	copy(cp, results)
	team.mu.Lock()
	team.Stages = cp
	team.mu.Unlock()
	team.persist()

	// 把细粒度 stage 指标也一并上报, 与 Coordinator.recordStageMetrics 保持一致。
	if mc := team.metrics(); mc != nil {
		for _, sr := range results {
			labels := map[string]string{
				"workflow": team.Workflow,
				"stage":    sr.Name,
				"role":     sr.Role,
				"status":   string(sr.Status),
			}
			if sr.Duration != "" {
				if d, err := time.ParseDuration(sr.Duration); err == nil {
					mc.RecordRun("team", metrics.MTeamStageDurationSec, d.Seconds(), team.Name, labels)
				}
			}
			mc.RecordRun("team", metrics.MTeamStageCount, 1, team.Name, labels)
			if sr.Status == TaskCompleted {
				mc.RecordRun("team", metrics.MTeamStageSuccessCount, 1, team.Name, labels)
			} else if sr.Status == TaskFailed {
				mc.RecordRun("team", metrics.MTeamStageFailCount, 1, team.Name, labels)
			}
			if sr.Output != "" {
				mc.RecordRun("team", metrics.MTeamStageOutputLen, float64(len(sr.Output)), team.Name, labels)
			}
		}
	}
}

func (we *WorkflowExecutor) restoreCheckpoints(stages []StageDef, prevResults map[string]string, allResults *[]StageResult) {
	if we.checkpoints == nil {
		return
	}
	restored := 0
	for _, stage := range stages {
		cp := we.checkpoints.GetCheckpoint(stage.Name)
		if cp == nil || cp.Status != "completed" || cp.Output == "" {
			continue
		}
		prevResults[stage.Name] = cp.Output
		// 角色别名也注入 (design 阶段的 key 可能是 role name)
		if stage.Role != "" {
			prevResults[stage.Role] = cp.Output
		}
		*allResults = append(*allResults, StageResult{
			Name: stage.Name, Role: stage.Role, Status: TaskCompleted,
			Output: cp.Output, StartedAt: cp.SavedAt,
		})
		restored++
	}
	if restored > 0 {
		we.notify(we.chatID, fmt.Sprintf("♻️ 从检查点恢复 %d 个已完成阶段", restored))
	}
}


func (we *WorkflowExecutor) executePipeline(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	results := make(map[string]string)
	var allResults []StageResult

	// 按依赖拓扑执行 (检测可并行的阶段)
	completed := make(map[string]bool)

	for len(completed) < len(wf.Stages) {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		// 找到所有依赖已满足的阶段
		var ready []StageDef
		for _, stage := range wf.Stages {
			if completed[stage.Name] {
				continue
			}
			allDepsReady := true
			for _, dep := range stage.DependsOn {
				if !completed[dep] {
					allDepsReady = false
					break
				}
			}
			if allDepsReady {
				ready = append(ready, stage)
			}
		}

		if len(ready) == 0 {
			return allResults, fmt.Errorf("工作流死锁: 无法找到可执行的阶段")
		}

		// 检查是否有多个可并行的阶段
		parallelGroup := filterParallel(ready)
		if len(parallelGroup) > 1 {
			stageResults := we.executeParallel(ctx, parallelGroup, objective, results, team)
			for _, sr := range stageResults {
				allResults = append(allResults, sr)
				if sr.Status == TaskCompleted {
					completed[sr.Name] = true
					results[sr.Name] = sr.Output
				} else {
					return allResults, fmt.Errorf("阶段 %s 失败: %s", sr.Name, sr.Error)
				}
			}
		} else {
			stage := ready[0]
			sr := we.executeStage(ctx, stage, objective, results, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				completed[stage.Name] = true
				results[stage.Name] = sr.Output
			} else {
				return allResults, fmt.Errorf("阶段 %s 失败: %s", stage.Name, sr.Error)
			}
		}
	}
	return allResults, nil
}

func estimateDebateDivergence(proposerOutput, opponentOutput string) float64 {
	if proposerOutput == "" || opponentOutput == "" {
		return 0.5
	}

	agreementMarkers := []string{"agree", "同意", "确实", "indeed", "correct", "正确", "是的", "没错"}
	disagreementMarkers := []string{"disagree", "不同意", "反对", "however", "但是", "错误", "误导", "不正确", "fallacy"}

	pLower := strings.ToLower(proposerOutput)
	oLower := strings.ToLower(opponentOutput)
	combined := pLower + " " + oLower

	agreeCount, disagreeCount := 0, 0
	for _, m := range agreementMarkers {
		agreeCount += strings.Count(combined, m)
	}
	for _, m := range disagreementMarkers {
		disagreeCount += strings.Count(combined, m)
	}

	total := agreeCount + disagreeCount
	if total == 0 {
		return 0.5
	}

	divergence := float64(disagreeCount) / float64(total)

	// 文本长度差异也暗示分歧 (一方明显更长说明有更多反驳)
	lenRatio := float64(len(proposerOutput)) / float64(len(opponentOutput))
	if lenRatio < 1 {
		lenRatio = 1 / lenRatio
	}
	if lenRatio > 2 {
		divergence = divergence*0.7 + 0.3
	}

	if divergence > 1 {
		divergence = 1
	}
	return divergence
}

func (we *WorkflowExecutor) ExecuteSingleStage(ctx context.Context, stage StageDef, objective string, prevResults map[string]string, team *ProductionTeam) StageResult {
	return we.executeStage(ctx, stage, objective, prevResults, team)
}

func buildLocalReferenceContextForStage(stage StageDef, objective string) string {
	role := strings.ToLower(strings.TrimSpace(stage.Role))
	name := strings.ToLower(strings.TrimSpace(stage.Name))
	if role != "researcher" && role != "architect" && name != "research" && name != "design" {
		return ""
	}
	paths := extractLocalReferencePaths(objective)
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 本地参考资料摘录 (仅供调研/架构阶段使用)\n")
	b.WriteString("说明: 这些资料用于架构师形成接口、约束、目录和验证策略; Planner 不会直接读取这些原始资料。\n")
	total := 0
	for _, p := range paths {
		chunk := readLocalReferenceExcerpt(p, 24000-total)
		if chunk == "" {
			continue
		}
		b.WriteString("\n### 来源: " + p + "\n")
		b.WriteString(chunk)
		if !strings.HasSuffix(chunk, "\n") {
			b.WriteString("\n")
		}
		total = b.Len()
		if total >= 24000 {
			b.WriteString("\n[reference truncated]\n")
			break
		}
	}
	if total == 0 {
		return ""
	}
	return b.String()
}

func extractLocalReferencePaths(objective string) []string {
	re := regexp.MustCompile(`/[^\s，,。；;：:)）]+`)
	matches := re.FindAllString(objective, -1)
	var paths []string
	for _, match := range matches {
		match = strings.TrimRight(match, "。；;，,、)）]")
		if match == "" {
			continue
		}
		if _, err := os.Stat(match); err == nil {
			paths = append(paths, match)
		}
	}
	return uniqueStrings(paths)
}

func readLocalReferenceExcerpt(path string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if !info.IsDir() {
		return readReferenceFileExcerpt(path, maxChars)
	}
	var files []string
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != path {
				switch d.Name() {
				case ".git", ".claude-go", "node_modules", "vendor", "dist", "build":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if isReferenceDocFile(p) {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	var b strings.Builder
	for _, file := range files {
		if b.Len() >= maxChars {
			break
		}
		excerpt := readReferenceFileExcerpt(file, min(4000, maxChars-b.Len()))
		if excerpt == "" {
			continue
		}
		rel, _ := filepath.Rel(path, file)
		b.WriteString("\n#### " + filepath.ToSlash(rel) + "\n")
		b.WriteString(excerpt)
		if !strings.HasSuffix(excerpt, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func isReferenceDocFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".txt", ".rst", ".json", ".yaml", ".yml", ".toml":
		return true
	default:
		return false
	}
}

func readReferenceFileExcerpt(path string, maxChars int) string {
	if maxChars <= 0 || !isReferenceDocFile(path) {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	text := string(data)
	if len(text) > maxChars {
		text = text[:maxChars] + "\n[truncated]\n"
	}
	return text
}

func (we *WorkflowExecutor) executeStage(ctx context.Context, stage StageDef, objective string, prevResults map[string]string, team *ProductionTeam) StageResult {
	stageStart := time.Now()
	traceCtx := observability.TraceFromContext(ctx)

	observability.Emit(observability.Event{
		Type:      observability.EvtStageStart,
		Timestamp: stageStart,
		TraceID:   traceCtx.TraceID,
		SpanID:    traceCtx.SpanID,
		Module:    "workflow",
		Name:      stage.Name,
		Payload: map[string]interface{}{
			"team_id":    team.Name,
			"workflow":   team.Workflow,
			"stage_name": stage.Name,
			"agent_role": stage.Role,
		},
	})

	ctx, endSpan := logging.WithSpan(ctx, "stage."+stage.Name)
	defer endSpan()
	logging.Event(ctx, "stage.start", "stage", stage.Name, "role", stage.Role, "team", team.Name)

	// watchdog 心跳: 标记有活动
	if we.activityCallback != nil {
		we.activityCallback()
	}

	// 1. 构建 prompt: 原有模板 + Blackboard 上下文 + Handoff 信息
	bbContext := ""
	if team.Blackboard != nil {
		var completedStages []string
		for name := range prevResults {
			completedStages = append(completedStages, name)
		}
		bbContext = team.Blackboard.HandoffContext(completedStages, stage.Role)
	}
	prompt := buildStagePromptWithRoles(stage, objective, prevResults, we.roles)
	if referenceContext := buildLocalReferenceContextForStage(stage, objective); referenceContext != "" {
		prompt += "\n\n---\n\n" + referenceContext
	}
	if bbContext != "" {
		prompt = bbContext + "\n\n---\n\n" + prompt
	}

	// 1a2. 注入用户反馈 (RefineTeam: 运行后用户反馈, 本轮重做的所有阶段都必须针对性处理)。
	// 支持角色模板里的 {user_feedback} 占位; 无占位则前置整段强约束。
	if team != nil {
		if fb := strings.TrimSpace(team.PendingFeedback); fb != "" {
			block := "### ⚠️ 用户反馈 (上一轮产出后收到, 本次必须针对性修正, 不得忽略):\n" + fb + "\n"
			if strings.Contains(prompt, "{user_feedback}") {
				prompt = strings.ReplaceAll(prompt, "{user_feedback}", block)
			} else {
				prompt = block + "\n---\n\n" + prompt
			}
		}
	}

	// 1b. 注入进化经验 (RETRIEVE: 执行前检索相关经验)
	var injectedExpIDs []string
	if we.evolution != nil {
		exps := we.evolution.RetrieveFor(stage.Role, objective, 3)
		if len(exps) > 0 {
			prompt = FormatExperiencesForPrompt(exps) + "\n" + prompt
			for _, e := range exps {
				injectedExpIDs = append(injectedExpIDs, e.ID)
			}
		}
	}

	// 2. 创建 V2 Task (LLM 可通过 TaskList 看到团队进度)
	var v2TaskID string
	if we.taskTracker != nil {
		taskSubject := fmt.Sprintf("[%s] %s", team.Name, stage.Name)
		id, err := we.taskTracker.AddTask(taskSubject, objective, stage.Role)
		if err == nil {
			v2TaskID = id
			_ = we.taskTracker.SetTaskStatus(id, "in_progress")
		}
	}

	we.notify(we.chatID, fmt.Sprintf("🔄 阶段 **%s** (%s) 开始执行...", stage.Name, stage.Role))

	// 3. 执行 Agent (带智能重试)
	sr := we.executeStageWithRetry(ctx, stage, prompt, objective, team, injectedExpIDs)
	sr.Name = stage.Name
	sr.Role = stage.Role
	sr.V2TaskID = v2TaskID

	// 4. 将结果写入 Blackboard (bMAS 核心: Agent 执行后写回黑板)
	if team.Blackboard != nil {
		if sr.Status == TaskCompleted {
			team.Blackboard.Write(stage.Name+"-result", sr.Output, stage.Role, "result")
			team.Blackboard.Write(stage.Name+"-status", "completed", "system", "progress")
			// 修复 handoff key 不匹配: 对抗轮次阶段 (如 implement-round3) 同时写入
			// 基础名 (如 implement-result), 确保 HandoffContext 能正确查找。
			if baseName := stripRoundSuffix(stage.Name); baseName != stage.Name {
				team.Blackboard.Write(baseName+"-result", sr.Output, stage.Role, "result")
			}
		} else {
			team.Blackboard.Write(stage.Name+"-status", "failed: "+sr.Error, "system", "progress")
		}
	}

	// 5. 更新 V2 Task 状态
	if we.taskTracker != nil && v2TaskID != "" {
		if sr.Status == TaskCompleted {
			_ = we.taskTracker.SetTaskStatus(v2TaskID, "completed")
		} else {
			_ = we.taskTracker.SetTaskStatus(v2TaskID, "failed")
		}
	}

	// 6. 记录执行轨迹 (RECORD) + 经验反馈 (EVOLVE) + 增量学习
	if we.evolution != nil {
		traj := Trajectory{
			TeamName:  team.Name,
			StageName: stage.Name,
			Role:      stage.Role,
			Objective: objective,
			Input:     prompt,
			Output:    sr.Output,
			Error:     sr.Error,
			Success:   sr.Status == TaskCompleted,
			Duration:  sr.Duration,
			Timestamp: time.Now(),
		}
		we.evolution.RecordTrajectory(traj)
		// V2双向学习: 成功+失败都提炼经验 (参考 ExpeL/MiniMax)
		we.evolution.LearnFromStage(traj)
		// V2反事实学习: 失败时额外生成假设性策略
		if sr.Status == TaskFailed {
			we.evolution.LearnCounterfactual(traj)
		}
		if len(injectedExpIDs) > 0 {
			we.evolution.RecordBatchFeedback(injectedExpIDs, sr.Status == TaskCompleted)
			// V2注入效果追踪
			we.evolution.RecordInjection(injectedExpIDs, stage.Name, team.Name, stage.Role, sr.Status == TaskCompleted)
		} else {
			// 无注入: 更新基线成功率 (用于 Uplift 计算)
			we.evolution.UpdateBaseline(sr.Status == TaskCompleted)
		}
	}

	// 发射 observability stage 完成/失败事件
	stageDur := time.Since(stageStart).Seconds()
	if sr.Status == TaskCompleted {
		observability.Emit(observability.Event{
			Type:      observability.EvtStageComplete,
			Timestamp: time.Now(),
			TraceID:   traceCtx.TraceID,
			SpanID:    traceCtx.SpanID,
			Module:    "workflow",
			Name:      stage.Name,
			Payload: map[string]interface{}{
				"team_id":      team.Name,
				"workflow":     team.Workflow,
				"stage_name":   stage.Name,
				"agent_role":   stage.Role,
				"duration_sec": stageDur,
				"success":      true,
				"output_len":   len(sr.Output),
			},
		})
	} else {
		observability.Emit(observability.Event{
			Type:      observability.EvtStageFail,
			Timestamp: time.Now(),
			TraceID:   traceCtx.TraceID,
			SpanID:    traceCtx.SpanID,
			Module:    "workflow",
			Name:      stage.Name,
			Payload: map[string]interface{}{
				"team_id":      team.Name,
				"workflow":     team.Workflow,
				"stage_name":   stage.Name,
				"agent_role":   stage.Role,
				"duration_sec": stageDur,
				"success":      false,
				"error":        sr.Error,
			},
		})
	}

	return sr
}

func (we *WorkflowExecutor) executeStageWithRetry(ctx context.Context, stage StageDef, prompt, objective string, team *ProductionTeam, injectedExpIDs []string) StageResult {
	var lastErr StageResult
	var rateLimitAttempt int // 限流专用重试计数器
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return StageResult{Role: stage.Role, Status: TaskFailed, Error: "cancelled"}
		}

		// 动态超时: 根据角色和尝试次数调整
		// 限流时增加超时预算, 给 RateLimitGuard 足够的等待时间
		timeout := we.computeStageTimeout(stage.Role, attempt)
		if rateLimitAttempt > 0 {
			// 限流时增加 50% 超时预算, 让 API client 的 RateLimitGuard 有机会等待
			timeout = time.Duration(float64(timeout) * 1.5)
			if timeout > 45*time.Minute {
				timeout = 45 * time.Minute
			}
		}
		stageCtx, stageCancel := context.WithTimeout(ctx, timeout)

		attemptLabel := fmt.Sprintf("%d", attempt+1)
		if rateLimitAttempt > 0 {
			attemptLabel = fmt.Sprintf("%d (限流第 %d 次)", attempt+1, rateLimitAttempt)
		}
		we.notify(we.chatID, fmt.Sprintf("🔄 阶段 **%s** (%s) 第 %s 次尝试...",
			stage.Name, stage.Role, attemptLabel))

		sr := we.runAgent(stageCtx, stage.Role, prompt, team)
		stageCancel()

		if sr.Status == TaskCompleted {
			if validationErr := validateStageOutputForRetry(stage, objective, sr.Output); validationErr != "" {
				lastErr = StageResult{Role: stage.Role, Status: TaskFailed, Error: validationErr, Output: sr.Output}
				if fallback := deterministicStageValidationFallback(stage, objective, validationErr); fallback.Status == TaskCompleted && shouldUseStageValidationFallback(stage, objective, validationErr, attempt) {
					we.notify(we.chatID, fmt.Sprintf("🧭 阶段 **%s** 输出越界, 使用确定性 fallback: %s", stage.Name, validationErr))
					return fallback
				}
				if attempt >= stageRetryMaxRetries {
					if fallback := deterministicPlannerFallbackStageResult(stage, objective, validationErr); fallback.Status == TaskCompleted {
						we.notify(we.chatID, fmt.Sprintf("🧭 阶段 **%s** 多次越界, 使用通用 V1 纵切 fallback WBS", stage.Name))
						return fallback
					}
					we.notify(we.chatID, fmt.Sprintf("❌ 阶段 **%s** 输出校验失败: %s", stage.Name, validationErr))
					return lastErr
				}
				we.notify(we.chatID, fmt.Sprintf("⚠️ 阶段 **%s** 输出校验失败, 将重试: %s", stage.Name, validationErr))
				prompt = prompt + "\n\n## 上次输出无效, 必须修正\n" + validationErr + "\n只输出该阶段要求的最终内容; 不要输出工具调用、bash 命令或元任务。\n"
				continue
			}
			if attempt > 0 {
				we.notify(we.chatID, fmt.Sprintf("✅ 阶段 **%s** 第 %d 次尝试成功", stage.Name, attempt+1))
			}
			return sr
		}

		lastErr = sr

		if isStageRetryableOutputError(sr.Error) {
			if fallback := deterministicStageValidationFallback(stage, objective, sr.Error); fallback.Status == TaskCompleted && shouldUseStageValidationFallback(stage, objective, sr.Error, attempt) {
				we.notify(we.chatID, fmt.Sprintf("🧭 阶段 **%s** 输出形态错误, 使用确定性 fallback: %s", stage.Name, sr.Error))
				return fallback
			}
			if attempt >= stageRetryMaxRetries {
				we.notify(we.chatID, fmt.Sprintf("❌ 阶段 **%s** 输出形态错误重试耗尽: %s", stage.Name, sr.Error))
				return sr
			}
			we.notify(we.chatID, fmt.Sprintf("⚠️ 阶段 **%s** 输出形态错误, 将重试: %s", stage.Name, sr.Error))
			prompt = buildStageOutputRetryPrompt(prompt, stage, sr.Error, sr.Output)
			continue
		}

		// 错误分类: 瞬态错误 vs 永久错误
		if !isStageTransientError(sr.Error) {
			// 永久错误 (产出验证失败、编译错误等): 不重试, 直接失败
			we.notify(we.chatID, fmt.Sprintf("❌ 阶段 **%s** 永久错误, 不重试: %s", stage.Name, sr.Error))
			return sr
		}

		// 检测是否为 429 限流
		isRateLimit := strings.Contains(strings.ToLower(sr.Error), "429") ||
			strings.Contains(strings.ToLower(sr.Error), "rate limit") ||
			strings.Contains(strings.ToLower(sr.Error), "限流") ||
			strings.Contains(strings.ToLower(sr.Error), "throttl")

		if isRateLimit {
			rateLimitAttempt++
			// 429 限流: 无限重试, 直到成功或 context 被取消
			// 利用 RateLimitGuard 的全局退避机制自动等待
			delay := computeRetryDelay(min(rateLimitAttempt-1, 5), true) // 最多用第 5 档退避
			// 限流时增加固定等待, 让 RateLimitGuard 冷却
			if delay < 30*time.Second {
				delay = 30 * time.Second
			}

			we.notify(we.chatID, fmt.Sprintf(
				"⏳ 阶段 **%s** 遇到 LLM 限流 (429), 等待 %.0f 秒后无限重试...\n"+
					"▸ 已等待限流解除 %d 次 | 错误: %s",
				stage.Name, delay.Seconds(), rateLimitAttempt, sr.Error))

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return StageResult{Role: stage.Role, Status: TaskFailed, Error: "cancelled during retry"}
			}
			continue // 无限循环, 不限次数
		}

		// 非限流瞬态错误: 有限重试
		if attempt >= stageRetryMaxRetries {
			break
		}

		delay := computeRetryDelay(attempt, false)
		we.notify(we.chatID, fmt.Sprintf("⚠️ 阶段 **%s** 第 %d 次尝试失败, %.0f秒后重试...\n错误: %s",
			stage.Name, attempt+1, delay.Seconds(), sr.Error))

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return StageResult{Role: stage.Role, Status: TaskFailed, Error: "cancelled during retry"}
		}
	}

	// 非限流瞬态错误重试耗尽
	return StageResult{
		Role:   stage.Role,
		Status: TaskFailed,
		Error:  fmt.Sprintf("超过最大重试次数 (%d): %s", stageRetryMaxRetries, lastErr.Error),
	}
}

func isStageRetryableOutputError(errStr string) bool {
	if errStr == "" {
		return false
	}
	lower := strings.ToLower(errStr)
	patterns := []string{
		"产出验证失败", "输出校验失败", "产出过短", "无结构化内容", "角色扮演空转",
		"伪工具调用", "pseudo tool", "tool call",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

func deterministicPlannerFallbackStageResult(stage StageDef, objective, reason string) StageResult {
	role := strings.ToLower(strings.TrimSpace(stage.Role))
	name := strings.ToLower(strings.TrimSpace(stage.Name))
	if role != "planner" && name != "plan" {
		return StageResult{Status: TaskFailed}
	}
	if !objectiveAllowsDeterministicV1Fallback(objective) {
		return StageResult{Status: TaskFailed}
	}
	if !strings.Contains(reason, "Planner WBS 不可执行") && !strings.Contains(reason, "Planner 输出不是可解析") {
		return StageResult{Status: TaskFailed}
	}
	tasks := synthesizeObjectiveWBS(objective)
	if len(tasks) == 0 {
		return StageResult{Status: TaskFailed}
	}
	out, err := marshalRawTasksAsWBSJSON(tasks)
	if err != nil {
		return StageResult{Status: TaskFailed, Error: err.Error()}
	}
	return StageResult{
		Name:   stage.Name,
		Role:   stage.Role,
		Status: TaskCompleted,
		Output: out,
	}
}

func deterministicStageValidationFallback(stage StageDef, objective, reason string) StageResult {
	if fallback := deterministicPlannerFallbackStageResult(stage, objective, reason); fallback.Status == TaskCompleted {
		return fallback
	}
	role := strings.ToLower(strings.TrimSpace(stage.Role))
	name := strings.ToLower(strings.TrimSpace(stage.Name))
	switch {
	case role == "researcher" || name == "research":
		return deterministicResearchFallbackStageResult(stage, objective, reason)
	case role == "architect" || name == "design":
		return deterministicArchitectFallbackStageResult(stage, objective, reason)
	default:
		return StageResult{Status: TaskFailed}
	}
}

func shouldUseStageValidationFallback(stage StageDef, objective, reason string, attempt int) bool {
	role := strings.ToLower(strings.TrimSpace(stage.Role))
	name := strings.ToLower(strings.TrimSpace(stage.Name))
	if role == "researcher" || role == "architect" || name == "research" || name == "design" {
		if objectiveRequiresDesignCompleteMode(objective) && attempt == 0 {
			return false
		}
		return strings.Contains(reason, "伪工具调用") || strings.Contains(strings.ToLower(reason), "pseudo tool")
	}
	if role == "planner" || name == "plan" {
		if !objectiveAllowsDeterministicV1Fallback(objective) {
			return false
		}
		return attempt >= 1 && strings.Contains(reason, "Planner WBS 不可执行")
	}
	return false
}

func deterministicResearchFallbackStageResult(stage StageDef, objective, reason string) StageResult {
	if objectiveRequiresDesignCompleteMode(objective) {
		out := fmt.Sprintf(`## HEV 调研报告 (deterministic fallback)

### 假设
- 用户明确要求严格按照参考设计文档完整实现; 参考设计就是本次验收范围, 不允许降级为 V1 纵切、接口占位或仅 happy path。
- 当前回退仅用于替代无效的伪工具输出; 它不能缩小范围, 只能把设计文档中的强约束转译为研发团队可执行约束。
- 高风险内核能力必须 contract-first 分阶段落地: storage/事务/索引/查询/协议入口依赖方向必须清晰, 先核心契约和本地引擎, 后 HTTP/gRPC/MCP/CLI 适配。

### 证据与约束
- 目标: %s
- 失败原因: %s
- 必须覆盖目标中列出的高级能力, 包括 WAL/LSM/SST/Compaction/MVCC、HNSW/DiskANN/PQ、CSR/Cypher、FTS/symbol、Embedding、SQL、Memory/session/retrieve/format、MCP/HTTP/gRPC/CLI、安全与备份恢复。

### 推荐结论
- 采用 design-complete execution: contract/base layer -> core engines -> query/adapters -> protocol/CLI -> global verification。
- 每个 leaf 仍必须小于 2-4 分钟、只改 1-3 个文件、scoped build/test 通过; 但禁止用 V1 fallback 替代设计覆盖。
`, objective, truncateResult(reason, 600))
		return StageResult{Name: stage.Name, Role: stage.Role, Status: TaskCompleted, Output: out}
	}
	out := fmt.Sprintf(`## HEV 调研报告 (deterministic fallback)

### 假设
- 用户目标要求交付一个本地可编译、可测试的 V1 纵切实现, 而不是一次性实现完整企业级/分布式/高阶存储内核。
- 参考设计只作为产品方向输入; 当前实现应优先提供稳定接口、最小本地存储、基础检索和回归测试。
- 未明确要求外部服务面时, 不引入 HTTP/gRPC/RPC server/client、API Key 或远程依赖。

### 证据与约束
- 目标: %s
- 失败原因: %s
- 研发团队应把高级能力作为未来扩展点, 包括 WAL/LSM/SSTable/HNSW/MVCC/分布式复制等, 当前 V1 不直接实现这些核心引擎。

### 推荐结论
- 采用本地库/CLI 优先的 V1 vertical slice: manifest、入口、核心 API/types、最小本地 store、最小查询/search、测试与本地验证。
- 所有 leaf 必须小于 2-4 分钟, scoped build/test 通过后再进入下游。
`, objective, truncateResult(reason, 600))
	return StageResult{Name: stage.Name, Role: stage.Role, Status: TaskCompleted, Output: out}
}

func deterministicArchitectFallbackStageResult(stage StageDef, objective, reason string) StageResult {
	targetRoot := inferObjectiveTargetRoot(objective)
	if targetRoot == "" {
		targetRoot = "app"
	}
	adapter := planningLanguageAdapterFor(inferPlanningLanguageID(objective, ""))
	if objectiveRequiresDesignCompleteMode(objective) {
		out := fmt.Sprintf(`## 架构设计 (deterministic fallback)

### 目标
%s

### 设计边界
- 输出目录: %s
- 语言适配器: %s
- 本次是 design-complete 任务: 参考设计文档是验收范围, 不允许降级成 V1 纵切。
- 所有高级能力必须进入模块边界和 WBS: storage/WAL/LSM/SST/compaction/MVCC、vector/HNSW/DiskANN/PQ、graph/CSR/Cypher、file/chunk/FTS/symbol、embedding、SQL、memory/session/retrieve/format、protocol/MCP/HTTP/gRPC/CLI、安全/备份恢复。

### 模块边界
- contracts: Config、Engine、Transaction、KV、Vector、Graph、File、SQL、Memory、Protocol 的公共接口和错误类型。
- storage core: WAL、SST、MemTable、Manifest、Compaction、MVCC、Page/File layout。
- model engines: vector、graph、file、embedding、memory/retrieve/format。
- query layer: SQL parser/planner/executor、Cypher subset、semantic retrieval。
- adapters: MCP、HTTP、gRPC、CLI 只依赖稳定 contracts/query facade, 不反向阻塞核心引擎实现。
- verification: 每个核心 capability 有本地单测, 最终执行 go test ./...。

### 验证策略
- Planner 必须按 contract/base -> core engines -> query/adapters -> protocol/CLI -> global verification 分层。
- Protocol/API 入口不得作为核心 engine 的上游依赖; API 失败不能阻塞 storage/vector/graph/file/sql 核心 leaf。
- 每个 leaf 只修改目标文件并执行 scoped build/test。

fallback reason: %s
`, objective, targetRoot, adapter.ID, truncateResult(reason, 600))
		return StageResult{Name: stage.Name, Role: stage.Role, Status: TaskCompleted, Output: out}
	}
	out := fmt.Sprintf(`## 架构设计 (deterministic fallback)

### 目标
%s

### 设计边界
- 输出目录: %s
- 语言适配器: %s
- 当前版本只交付 V1 纵切: 可编译项目骨架、核心 API/types、最小本地存储、最小检索/查询、测试和本地验证。
- 不实现未明确要求的外部服务面或高级存储内核: HTTP/gRPC/RPC server/client、API Key、WAL/LSM/SSTable/HNSW/MVCC/分布式复制等。

### 模块边界
- manifest/entry: 项目声明与最小入口。
- core: Config、Record/Document、Store/Search 接口、错误约定。
- storage: 内存或简单本地实现, 负责 Put/Get/Delete/List。
- search: 基础关键词或线性检索, 作为未来向量/倒排/图谱能力扩展点。
- tests: 覆盖 create/open、write/read/update/delete/list/search 的 happy path 和基础错误路径。

### 验证策略
- 每个 leaf 只修改目标文件并执行 scoped build/test。
- 最终 verification 只跑本地 build/test/TODO scan, 不调用 tester LLM。

fallback reason: %s
`, objective, targetRoot, adapter.ID, truncateResult(reason, 600))
	return StageResult{Name: stage.Name, Role: stage.Role, Status: TaskCompleted, Output: out}
}

func marshalRawTasksAsWBSJSON(tasks []rawTask) (string, error) {
	type taskJSON struct {
		ID              string   `json:"id"`
		Title           string   `json:"title"`
		Role            string   `json:"role"`
		TaskType        string   `json:"taskType"`
		WorkUnitType    string   `json:"workUnitType,omitempty"`
		ParentID        string   `json:"parentId,omitempty"`
		DependsOn       []string `json:"dependsOn,omitempty"`
		EstimatedMin    int      `json:"estimatedMinutes,omitempty"`
		RiskLevel       string   `json:"riskLevel,omitempty"`
		ParallelGroup   string   `json:"parallelGroup,omitempty"`
		BlockingPolicy  string   `json:"blockingPolicy,omitempty"`
		TargetFiles     []string `json:"targetFiles,omitempty"`
		WriteFiles      []string `json:"writeFiles,omitempty"`
		ReadFiles       []string `json:"readFiles,omitempty"`
		ConflictKeys    []string `json:"conflictKeys,omitempty"`
		TargetPackages  []string `json:"targetPackages,omitempty"`
		Acceptance      string   `json:"acceptance,omitempty"`
		VerifyCommand   string   `json:"verifyCommand,omitempty"`
		DesignRef       string   `json:"designRef,omitempty"`
		SplitReason     string   `json:"splitReason,omitempty"`
		CapabilityID    string   `json:"capabilityId,omitempty"`
		ContractRefs    []string `json:"contractRefs,omitempty"`
		Provides        []string `json:"provides,omitempty"`
		Requires        []string `json:"requires,omitempty"`
		ConstraintRefs  []string `json:"constraintRefs,omitempty"`
		EstimatedChange int      `json:"estimatedChangedLOC,omitempty"`
	}
	out := struct {
		Tasks []taskJSON `json:"tasks"`
	}{Tasks: make([]taskJSON, 0, len(tasks))}
	for _, task := range tasks {
		id := strings.TrimSpace(task.num)
		if id == "" {
			id = strings.TrimSpace(task.title)
		}
		out.Tasks = append(out.Tasks, taskJSON{
			ID:              id,
			Title:           task.title,
			Role:            task.role,
			TaskType:        task.taskType,
			WorkUnitType:    task.workUnitType,
			ParentID:        task.parentID,
			DependsOn:       task.depNums,
			EstimatedMin:    task.estimatedMin,
			RiskLevel:       task.riskLevel,
			ParallelGroup:   task.parallelGroup,
			BlockingPolicy:  task.blockingPolicy,
			TargetFiles:     task.targetFiles,
			WriteFiles:      task.writeFiles,
			ReadFiles:       task.readFiles,
			ConflictKeys:    task.conflictKeys,
			TargetPackages:  task.targetPackages,
			Acceptance:      task.accept,
			VerifyCommand:   task.verifyCommand,
			DesignRef:       task.designRef,
			SplitReason:     task.splitReason,
			CapabilityID:    task.capabilityID,
			ContractRefs:    task.contractRefs,
			Provides:        task.provides,
			Requires:        task.requires,
			ConstraintRefs:  task.constraintRefs,
			EstimatedChange: task.estimatedLOC,
		})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func buildStageOutputRetryPrompt(base string, stage StageDef, reason, previousOutput string) string {
	role := strings.ToLower(strings.TrimSpace(stage.Role))
	var required string
	switch role {
	case "researcher":
		required = "必须直接输出 HEV 调研报告，包含假设表、证据/反例、技术选型对比、关键难点方案、推荐结论。"
	case "architect":
		required = "必须直接输出架构设计文档，包含模块边界、接口契约、目录结构、依赖关系、验证策略。"
	case "planner":
		required = "必须直接输出严格 JSON WBS，不要 Markdown、不要解释、不要工具调用。"
	default:
		required = "必须直接输出该阶段要求的最终交付内容。"
	}
	return base + "\n\n## 上次输出无效, 请立即修正\n" +
		"失败原因: " + truncateResult(reason, 800) + "\n" +
		"硬性规则: 不要输出 `<minimax:tool_call>`、`<invoke>`、bash、Read、Search、Cat 或任何伪工具调用；本地参考资料已经注入 prompt，请基于已给内容直接完成。\n" +
		required + "\n" +
		"上次无效输出摘录:\n" + truncateResult(previousOutput, 1200) + "\n"
}

func validateStageOutputForRetry(stage StageDef, objective, output string) string {
	role := strings.ToLower(strings.TrimSpace(stage.Role))
	name := strings.ToLower(strings.TrimSpace(stage.Name))
	if role == "planner" || name == "plan" {
		if containsPseudoToolCall(output) {
			return "Planner 输出包含伪工具调用; Planner 只能输出严格 JSON WBS, 不能读取文件或执行命令"
		}
		tasks := parseWBSFromJSON(output)
		if len(tasks) == 0 {
			return "Planner 输出不是可解析的 JSON WBS"
		}
		if err := validateParsedWBSForObjective(tasks, objective); err != nil {
			return "Planner WBS 不可执行: " + err.Error()
		}
		return ""
	}
	if role == "researcher" || role == "architect" || name == "research" || name == "design" {
		if containsPseudoToolCall(output) {
			return "阶段输出包含伪工具调用; 本地参考资料已注入 prompt, 请直接产出最终内容"
		}
	}
	return ""
}

func containsPseudoToolCall(output string) bool {
	lower := strings.ToLower(output)
	patterns := []string{
		"<minimax:tool_call",
		"<invoke name=\"read\"",
		"<invoke name=\"bash\"",
		"```bash",
		"cat /",
		"ls -la /",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (we *WorkflowExecutor) computeStageTimeout(role string, attempt int) time.Duration {
	base := stageTimeout // 默认 10 分钟

	// 根据角色调整基础超时.
	// 注意: 做事型角色 (coder/tester) 在写 Phase1+Phase2 这种大功能时需要 20+ 分钟,
	// 之前 8 min 直接被 RunIsolated 撞墙. 这里大幅放宽; 简单 bug 修复阶段会有更短的
	// 总流水线截止时间兜底.
	switch role {
	case "coder":
		base = 25 * time.Minute
	case "researcher":
		base = 6 * time.Minute
	case "architect", "planner":
		base = 6 * time.Minute
	case "tester":
		base = 15 * time.Minute
	case "reviewer":
		base = 10 * time.Minute
	}

	// 每次重试增加 20% 超时预算 (给 LLM 更多时间)
	if attempt > 0 {
		multiplier := 1.0 + float64(attempt)*0.2
		base = time.Duration(float64(base) * multiplier)
	}

	// 硬上限: 给 coder 这种大动作放到 40 分钟; 简单角色配置远低于此, 不会受影响.
	if base > 40*time.Minute {
		base = 40 * time.Minute
	}

	return base
}

func (we *WorkflowExecutor) effectiveParallel() int {
	if we.concurrency != nil {
		suggested := we.concurrency.SuggestConcurrency()
		if suggested > 0 && suggested < maxWorkflowParallelDefault {
			return suggested
		}
		if suggested > maxWorkflowParallelDefault {
			return maxWorkflowParallelDefault
		}
	}
	return maxWorkflowParallelDefault
}

func (we *WorkflowExecutor) executeParallel(ctx context.Context, stages []StageDef, objective string, prevResults map[string]string, team *ProductionTeam) []StageResult {
	results := make([]StageResult, len(stages))
	var wg sync.WaitGroup
	para := we.effectiveParallel()
	sem := make(chan struct{}, para)

	for i, stage := range stages {
		wg.Add(1)
		go func(idx int, s StageDef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = we.executeStage(ctx, s, objective, prevResults, team)
		}(i, stage)
	}

	wg.Wait()
	return results
}

func isStageTransientError(errStr string) bool {
	if errStr == "" {
		return false
	}
	lower := strings.ToLower(errStr)
	transientPatterns := []string{
		"context deadline exceeded", "timeout", "deadline",
		"429", "rate limit", "rate_limit", "throttl", "限流", "频率",
		"connection refused", "connection reset", "network",
		"503", "529", "overloaded", "过载",
		"burstrate", "allocationquota", "ratequota",
		"temporary", "transient", "retry",
	}
	for _, pat := range transientPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

func computeRetryDelay(attempt int, isRateLimit bool) time.Duration {
	base := stageRetryBaseDelay
	if isRateLimit {
		base = 15 * time.Second
	}
	// 指数退避: base * 2^attempt
	delay := time.Duration(1<<uint(attempt)) * base
	if delay > stageRetryMaxDelay {
		delay = stageRetryMaxDelay
	}
	// 全抖动: random(0, delay)
	jitter := time.Duration(rand.Float64() * float64(delay))
	return jitter
}

func (we *WorkflowExecutor) runAgent(ctx context.Context, role, prompt string, team *ProductionTeam) StageResult {
	start := time.Now()

	if we.factory == nil {
		return StageResult{Role: role, Status: TaskFailed, Error: "Agent 工厂未配置"}
	}

	// 更新 agent 状态 (含实时心跳: 进入运行态, 供 team status / dashboard 观测团队内部)
	team.mu.Lock()
	if ag, ok := team.Agents[role]; ok {
		ag.Status = AgentStatusRunning
		ag.Phase = "执行中"
		ag.LastBeat = time.Now()
	}
	team.mu.Unlock()
	team.persist()

	stageCtx := ctx
	stageCancel := func() {}
	if _, ok := stageCtx.Deadline(); !ok {
		// 单阶段超时保护: 防止 agent 陷入死循环。若上层已经设置了更精确的
		// 角色级 timeout, 这里不再覆盖。
		stageCtx, stageCancel = context.WithTimeout(ctx, stageTimeout)
	}
	defer stageCancel()

	// 模型层级解析: role > plan > 全局默认
	if we.planCfgResolver != nil {
		stageCtx = context.WithValue(stageCtx, ModelConfigKey{}, we.planCfgResolver.Resolve(team.Workflow, role))
	}
	stageCtx = WithRunMetadata(stageCtx, RunMetadata{
		Source:   "team_stage",
		Purpose:  team.Name,
		Workflow: team.Workflow,
		Role:     role,
		Team:     team.Name,
	})

	runner, err := we.factory(stageCtx, role, "")
	if err != nil {
		return StageResult{Role: role, Status: TaskFailed, Error: err.Error(), StartedAt: start, Duration: time.Since(start).String()}
	}

	timeout := remainingContextTimeout(stageCtx, stageTimeout)
	if timeout <= 0 {
		return StageResult{Role: role, Status: TaskFailed, Error: "stage timeout before agent execution", StartedAt: start, Duration: time.Since(start).Round(time.Second).String()}
	}
	result, err := executeRunnerBounded(stageCtx, runner, prompt, timeout)
	duration := time.Since(start)

	// 更新 agent 状态 (结束: 清空实时阶段, 刷新心跳)
	team.mu.Lock()
	if ag, ok := team.Agents[role]; ok {
		ag.Phase = ""
		ag.LastBeat = time.Now()
		if err != nil {
			ag.Status = AgentStatusFailed
			ag.Error = err.Error()
		} else {
			ag.Status = AgentStatusCompleted
			ag.Result = truncateResult(result, 1000)
		}
	}
	team.mu.Unlock()
	team.persist()

	if err != nil {
		return StageResult{Role: role, Status: TaskFailed, Error: err.Error(), StartedAt: start, Duration: duration.Round(time.Second).String()}
	}

	// 产出验证: 防止 Agent "角色扮演空转"（仅声明就绪但无实际产出）
	if reason := validateAgentOutput(result, role); reason != "" {
		// V2 关键修复: 如果产出是 API 错误文本 (限流/超时/熔断), 将其转换为 API 错误,
		// 使 executeStageWithRetry 能识别为瞬态错误并自动重试。
		if reason == "__API_ERROR__" {
			return StageResult{
				Role: role, Status: TaskFailed,
				Error:     fmt.Sprintf("API 错误 (限流/超时/熔断): %s", truncateResult(result, 200)),
				Output:    result,
				StartedAt: start, Duration: duration.Round(time.Second).String(),
			}
		}
		we.notify(we.chatID, fmt.Sprintf("⚠️ Agent **%s** 产出不合格: %s — 标记为失败并重试", role, reason))
		return StageResult{
			Role: role, Status: TaskFailed,
			Error:     fmt.Sprintf("产出验证失败: %s", reason),
			Output:    result,
			StartedAt: start, Duration: duration.Round(time.Second).String(),
		}
	}

	return StageResult{
		Role: role, Status: TaskCompleted,
		Output: result, StartedAt: start,
		Duration: duration.Round(time.Second).String(),
	}
}

func remainingContextTimeout(ctx context.Context, fallback time.Duration) time.Duration {
	if ctx == nil {
		return fallback
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < 0 {
			return 0
		}
		return remaining
	}
	return fallback
}

func ValidateAgentOutput(output, role string) string {
	return validateAgentOutput(output, role)
}

func validateAgentOutput(output, role string) string {
	trimmed := strings.TrimSpace(output)

	// V2 关键修复: 只把"明显是 API 错误包装文本"的输出转成瞬态错误。
	// 不能全文匹配 "限流/timeout/429" 等词, 否则正常技术报告讨论限流、超时设计也会被误判。
	if looksLikeAPIErrorOutput(trimmed) {
		return "__API_ERROR__"
	}

	// 1. 基本长度检查 (有效产出通常 > 100 字符)
	if len(trimmed) < 50 {
		return "产出过短 (< 50 字符)，可能未实际执行任务"
	}
	lower := strings.ToLower(trimmed)

	// 思考型角色 (architect/researcher/planner) 在 DisableTools 模式下经常吐
	// "我将先读文档" 之类 1-2 句承诺型 stub. 这里识别两种典型 stub 形态:
	//   1. 输出非常短 (< 300 字符), 同时只包含 "执行/调用/读取" 这类动词 ➜ 标记为 stub
	//   2. 开头明确说 "我将先读 X / 首先读取 Y" 这类承诺型措辞 ➜ 标记为 stub
	// 对 1500+ 字符的正常设计稿不影响, 对 200-300 字符的合法简报也保留 (没有承诺措辞).
	roleLower := strings.ToLower(strings.TrimSpace(role))
	if roleLower == "architect" || roleLower == "researcher" || roleLower == "planner" {
		head := trimmed
		if len(head) > 600 {
			head = head[:600]
		}
		promiseTriggers := []string{
			"我将先读", "我将先看", "我先读", "我先看",
			"首先读取", "首先查看", "首先了解", "首先阅读", "首先获取",
			"我将按照", // "我将按照 X 流水线执行..." 这种描述性开场
			"i will first read", "i'll first read", "let me first read",
			"first, i will read", "first, let me read",
		}
		matchedPromise := false
		for _, p := range promiseTriggers {
			if strings.Contains(head, p) {
				matchedPromise = true
				break
			}
		}
		// 极短输出 + 承诺措辞 ➜ 几乎必然是 stub. 此时拒绝.
		if matchedPromise && len(trimmed) < 800 {
			return "思考型角色产出仅是 \"先读文档再做事\" 的承诺型 stub. 必须直接基于已注入的参考资料给出完整产出, 不要先承诺再行动"
		}
	}

	// 2. 空转模式检测: 仅声明角色就绪、未提供实质内容
	idlePatterns := []string{
		"i am ready", "i'm ready", "已就位", "已准备", "准备就绪",
		"i understand my role", "i have been assigned",
		"please provide", "please tell me", "请告诉我",
		"waiting for", "等待指令", "等待进一步",
		"now i have full understanding", "let me write",
	}
	idleCount := 0
	for _, pat := range idlePatterns {
		if strings.Contains(lower, pat) {
			idleCount++
		}
	}

	// 产出中 >50% 是角色声明/等待指令 → 空转
	hasSubstantiveContent := false
	substantiveMarkers := []string{
		"```", "##", "func ", "class ", "def ", "import ", "const ", "var ",
		"<svg", "<html", "<div", "export ", "package ", "module ",
		"CREATE TABLE", "SELECT ", "INSERT ",
		"步骤", "方案", "分析", "结论", "建议", "设计", "实现",
	}
	for _, marker := range substantiveMarkers {
		if strings.Contains(trimmed, marker) {
			hasSubstantiveContent = true
			break
		}
	}

	if idleCount >= 2 && !hasSubstantiveContent {
		return "检测到角色扮演空转 (仅声明就绪/等待指令，无实质产出)"
	}

	// 3. 过短且无代码/结构化内容
	if len(trimmed) < 200 && !hasSubstantiveContent {
		return "产出过短且无结构化内容 (代码、文档、分析等)"
	}

	return ""
}

func looksLikeAPIErrorOutput(output string) bool {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return false
	}
	lines := strings.Split(trimmed, "\n")
	head := strings.ToLower(strings.TrimSpace(strings.Join(lines[:min(len(lines), 3)], "\n")))
	if len(head) > 800 {
		head = head[:800]
	}
	apiErrorPrefixes := []string{
		"api error", "api 错误", "error:", "错误:", "request failed",
		"http 429", "429:", "status 429", "rate limit exceeded",
		"rate_limit", "context deadline exceeded", "deadline exceeded",
		"circuit breaker", "断路器触发", "family exhausted",
	}
	for _, prefix := range apiErrorPrefixes {
		if strings.HasPrefix(head, prefix) {
			return true
		}
	}
	apiErrorMarkers := []string{
		"api 返回 429", "api返回429", "api returned 429",
		"api 错误 (限流", "api 错误(限流",
		"api 错误 (超时", "api 错误(超时",
		"api 错误 (限流/超时/熔断)",
	}
	for _, marker := range apiErrorMarkers {
		if strings.Contains(head, marker) {
			return true
		}
	}
	return false
}

func buildStagePromptWithRoles(stage StageDef, objective string, prevResults map[string]string, roles *RoleRegistry) string {
	var prevOutput strings.Builder
	for _, dep := range stage.DependsOn {
		if r, ok := prevResults[dep]; ok {
			// 代码/HTML 产物完整传递 (供审查/渲染); 散文走关键行摘要。
			summary := summarizeDependency(r)
			prevOutput.WriteString(fmt.Sprintf("### Dependency output from %s:\n%s\n\nFull artifact/ref: blackboard key `%s-result`.\n\n", dep, summary, dep))
		}
	}

	// 代码类角色额外注入"先索引后精读"铁律
	extra := antiLoopDirective
	if roles != nil && isCodeAnalysisRole(roles.Get(stage.Role)) {
		extra += codeIntelDirective
	}

	// 优先从角色注册表获取 (包含专属 Skills)
	if roles != nil {
		if merged := roles.MergedPrompt(stage.Role, objective, prevOutput.String()); merged != "" {
			// 角色模板已承载任务目标 (SystemPrompt 含 {objective} 占位并已替换) → 保持原行为,
			// 不重复注入, 避免 70+ 个正常角色出现任务双写。
			if strings.TrimSpace(objective) == "" || strings.Contains(merged, objective) {
				return merged + extra
			}
			// 角色模板是纯人格 (persona-only, 无 {objective} 占位, 如 creative-v2 的
			// creative-planner / html-developer / art-director / media-producer):
			// 必须补上 stage 的具体任务, 否则 agent 收不到目标, 只会自我介绍并反向索要需求 (历史 bug)。
			task := substituteStagePlaceholders(stage.Prompt, objective, prevOutput.String(), prevResults)
			if strings.TrimSpace(task) == "" {
				// stage 自身也没有任务模板 (如 final-delivery): 注入显式任务块。
				task = buildExplicitTaskBlock(objective, prevOutput.String())
			}
			return merged + "\n\n---\n\n" + task + extra
		}
	}

	// 降级: 使用 StageDef 中的内联 Prompt
	prompt := substituteStagePlaceholders(stage.Prompt, objective, prevOutput.String(), prevResults)
	return prompt + extra
}

// optionalStagePlaceholders 是"对应阶段可能尚未产出"的可选占位符。
// 当其对应键还不在 prevResults 中时 (如对抗第 1 轮还没有 visual-feedback),
// 替换为空字符串, 避免字面 {visual-feedback} 泄漏进提示词。
// 注意: {user_feedback} 不在此列 — 它由 executeStage 在更晚阶段按 team.PendingFeedback 处理。
var optionalStagePlaceholders = []string{"visual-feedback"}

// substituteStagePlaceholders 替换 stage 模板里的占位符:
//   - {objective}    → 任务目标
//   - {prev_result}  → 依赖阶段产出摘要
//   - {<阶段名>}      → 对应阶段产出 (如 {html-develop} {media-render} {visual-feedback})
//
// 刻意不删除未知的 {…}: taskDecompose 等模板含字面 JSON 大括号, 必须原样保留。
func substituteStagePlaceholders(tmpl, objective, prevResult string, prevResults map[string]string) string {
	s := strings.ReplaceAll(tmpl, "{objective}", objective)
	s = strings.ReplaceAll(s, "{prev_result}", prevResult)
	for k, v := range prevResults {
		s = strings.ReplaceAll(s, "{"+k+"}", summarizeDependency(v))
	}
	for _, opt := range optionalStagePlaceholders {
		if _, done := prevResults[opt]; !done {
			s = strings.ReplaceAll(s, "{"+opt+"}", "")
		}
	}
	return s
}

// buildExplicitTaskBlock 为"纯人格角色 + 无 stage 模板"的阶段 (如 final-delivery)
// 兜底注入明确任务, 直接对治"只自我介绍、反向索要需求"的失败模式。
func buildExplicitTaskBlock(objective, prevResult string) string {
	var b strings.Builder
	b.WriteString("## 当前任务\n")
	b.WriteString(objective)
	b.WriteString("\n")
	if strings.TrimSpace(prevResult) != "" {
		b.WriteString("\n## 已完成阶段产出 (基于此继续, 勿重复索要需求)\n")
		b.WriteString(prevResult)
		b.WriteString("\n")
	}
	b.WriteString("\n请直接产出本阶段要求的最终成果, 不要自我介绍, 也不要反过来索要需求。")
	return b.String()
}

func stripRoundSuffix(name string) string {
	for i := 1; i <= 20; i++ {
		suffix := fmt.Sprintf("-round%d", i)
		if strings.HasSuffix(name, suffix) {
			return name[:len(name)-len(suffix)]
		}
	}
	return name
}

type LanguageToolchain struct {
	Language        string     // "go", "cpp", "rust", "python"
	BuildCmds       [][]string // 编译命令序列
	LintCmds        [][]string // 静态分析命令
	TestCmds        [][]string // 测试命令
	InitCmds        [][]string // 项目初始化命令
	FileExt         string     // ".go", ".cpp"/".h", ".rs", ".py"
	ProjectFile     string     // "go.mod", "CMakeLists.txt", "Cargo.toml", "pyproject.toml"
	Timeout         time.Duration
	MemoryMaxMB     int // 子进程内存上限 (MB), 0=不限制
	CPUQuotaPercent int // CPU 配额百分比, 0=不限制
}

func GetToolchain(lang string) *LanguageToolchain {
	switch lang {
	case "cpp", "c++":
		return &LanguageToolchain{
			Language: "cpp",
			// 默认: 最小化构建 (仅编译依赖当前源码的目标, 不构建 mysqld 全量)
			// 通过 make <file>.o 验证语法, 避免每次修改都触发全量构建
			BuildCmds:       [][]string{{"cmake", "-B", "build", "-DCMAKE_EXPORT_COMPILE_COMMANDS=ON"}, {"cmake", "--build", "build", "--parallel"}},
			LintCmds:        [][]string{{"cmake", "--build", "build", "--target", "all"}},
			TestCmds:        [][]string{{"ctest", "--test-dir", "build", "--output-on-failure"}},
			InitCmds:        [][]string{},
			FileExt:         ".cpp",
			ProjectFile:     "CMakeLists.txt",
			Timeout:         120 * time.Second,
			MemoryMaxMB:     16384,
			CPUQuotaPercent: 200,
			// MySQL/Percona 特殊处理: 增量编译时仅编译修改过的 .o
			// 在 BuildCmds 执行前, buildScript 会检测项目类型并动态调整策略
		}
	case "rust", "rs":
		return &LanguageToolchain{
			Language:        "rust",
			BuildCmds:       [][]string{{"cargo", "build"}},
			LintCmds:        [][]string{{"cargo", "clippy", "--", "-D", "warnings"}},
			TestCmds:        [][]string{{"cargo", "test"}},
			InitCmds:        [][]string{{"cargo", "init", "--name", "agentdb"}},
			FileExt:         ".rs",
			ProjectFile:     "Cargo.toml",
			Timeout:         120 * time.Second,
			MemoryMaxMB:     8192,
			CPUQuotaPercent: 200,
		}
	case "python", "py":
		return &LanguageToolchain{
			Language:        "python",
			BuildCmds:       [][]string{{"python", "-m", "compileall", "-q", "."}},
			LintCmds:        [][]string{{"python", "-m", "flake8", "."}},
			TestCmds:        [][]string{{"python", "-m", "pytest"}},
			InitCmds:        [][]string{},
			FileExt:         ".py",
			ProjectFile:     "pyproject.toml",
			Timeout:         60 * time.Second,
			MemoryMaxMB:     4096,
			CPUQuotaPercent: 200,
		}
	case "typescript", "ts":
		return &LanguageToolchain{
			Language:        "typescript",
			BuildCmds:       [][]string{},
			LintCmds:        [][]string{},
			TestCmds:        [][]string{},
			InitCmds:        [][]string{},
			FileExt:         ".ts",
			ProjectFile:     "package.json",
			Timeout:         60 * time.Second,
			MemoryMaxMB:     4096,
			CPUQuotaPercent: 200,
		}
	case "javascript", "js", "node":
		return &LanguageToolchain{
			Language:        "javascript",
			BuildCmds:       [][]string{},
			LintCmds:        [][]string{},
			TestCmds:        [][]string{{"npm", "test", "--", "--test-reporter=spec"}},
			InitCmds:        [][]string{},
			FileExt:         ".js",
			ProjectFile:     "package.json",
			Timeout:         60 * time.Second,
			MemoryMaxMB:     4096,
			CPUQuotaPercent: 200,
		}
	default: // "go" or empty
		return &LanguageToolchain{
			Language:        "go",
			BuildCmds:       [][]string{{"go", "build", "./..."}, {"go", "vet", "./..."}},
			LintCmds:        [][]string{},
			TestCmds:        [][]string{{"go", "test", "-count=1", "-timeout=30s", "-parallel=4", "./..."}},
			InitCmds:        [][]string{},
			FileExt:         ".go",
			ProjectFile:     "go.mod",
			Timeout:         30 * time.Second,
			MemoryMaxMB:     8192,
			CPUQuotaPercent: 200,
		}
	}
}

func (tc *LanguageToolchain) BuildCheckLabel() string {
	switch tc.Language {
	case "cpp":
		return "cmake --build build 通过"
	case "rust":
		return "cargo build 通过"
	case "python":
		return "python -m py_compile 通过"
	default:
		return "go build 通过"
	}
}

func (tc *LanguageToolchain) TestCheckLabel() string {
	switch tc.Language {
	case "cpp":
		return "ctest --output-on-failure 通过"
	case "rust":
		return "cargo test 通过"
	case "python":
		return "pytest 通过"
	default:
		return "go test ./... 通过"
	}
}

func runBuildCheck(cwd string) string {
	return runBuildCheckLang(cwd, "go")
}

func mysqlCMakeConfigureArgs() []string {
	return []string{
		"-B", "build",
		"-DWITH_UNIT_TESTS=OFF", // 节省 30-40% 编译时间
		"-DWITH_DEBUG=OFF",      // Release 模式
		"-DCMAKE_BUILD_TYPE=Release",
		"-DWITH_PROTOBUF=bundled", // 使用预编译 protobuf
		"-DWITH_SSL=system",       // 使用系统 OpenSSL
	}
}

func runMySQLBuildCheck(cwd string) string {
	jobs := CalcSafeMakeJobs()

	// 检测是否已配置 (避免每次 check 都重复 configure)
	mysqlBuildState.Lock()
	alreadyConfigured := mysqlBuildState.configured[cwd]
	mysqlBuildState.Unlock()

	buildCache := filepath.Join(cwd, "build", "CMakeCache.txt")
	_, statErr := os.Stat(buildCache)
	cacheOK := statErr == nil
	if cacheOK && !alreadyConfigured {
		mysqlBuildState.Lock()
		mysqlBuildState.configured[cwd] = true
		mysqlBuildState.Unlock()
		alreadyConfigured = true
	}

	// Phase 1: cmake configure (仅首次)
	if !alreadyConfigured {
		configureArgs := mysqlCMakeConfigureArgs()
		ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel1()
		out, err := runLimitedCommand(ctx1, cwd, append([]string{"cmake"}, configureArgs...), 16384, 200)
		if err != nil {
			return fmt.Sprintf("cmake configure 失败:\n%.*s", 2000, string(out))
		}
		mysqlBuildState.Lock()
		mysqlBuildState.configured[cwd] = true
		mysqlBuildState.Unlock()
	}

	// Phase 2: 构建基础库 (首次 + 增量都执行, CMake 增量编译)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel2()
	// 使用 ninja 或 make 的并行模式, 仅构建核心依赖
	baseArgs := []string{"--build", "build", "--parallel", fmt.Sprintf("%d", jobs)}
	for _, t := range mysqlEssentialTargets {
		baseArgs = append(baseArgs, "--target", t)
	}
	out2, err2 := runLimitedCommand(ctx2, cwd, append([]string{"cmake"}, baseArgs...), 16384, 200)
	if err2 != nil {
		return fmt.Sprintf("基础库编译失败:\n%.*s", 2000, string(out2))
	}

	// Phase 3: 构建 mysqld 主程序 (验证核心链接)
	ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel3()
	out3, err3 := runLimitedCommand(ctx3, cwd, []string{"cmake", "--build", "build", "--parallel", fmt.Sprintf("%d", jobs), "--target", "mysqld"}, 16384, 200)
	if err3 != nil {
		return fmt.Sprintf("mysqld 编译失败:\n%.*s", 2000, string(out3))
	}

	return ""
}

func runLimitedCommand(ctx context.Context, cwd string, args []string, memMaxMB, cpuQuotaPercent int) ([]byte, error) {
	return runLimitedCommandWithNetwork(ctx, cwd, args, memMaxMB, cpuQuotaPercent, true, nil)
}

func runLimitedCommandWithNetwork(ctx context.Context, cwd string, args []string, memMaxMB, cpuQuotaPercent int, networkDisabled bool, env map[string]string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	spec := sandbox.CommandSpec{
		Purpose:             "team-verification",
		Cwd:                 cwd,
		Args:                args,
		AllowUnsafeFallback: os.Getenv("CLAUDE_GO_SANDBOX_ALLOW_UNSAFE_FALLBACK") != "",
		NetworkDisabled:     networkDisabled,
		Env:                 env,
		Limits: sandbox.ResourceLimits{
			MemoryMaxMB:     memMaxMB,
			CPUQuotaPercent: cpuQuotaPercent,
			PidsMax:         256,
			OutputMaxBytes:  4 * 1024 * 1024,
			PreviewMaxBytes: 192 * 1024,
			LogMaxBytes:     16 * 1024 * 1024,
		},
	}
	result, err := sandbox.DefaultManager().Run(ctx, spec)
	if result == nil {
		if err != nil {
			return []byte(fmt.Sprintf("[runLimitedCommand] sandbox 调度失败 (无 result): %v", err)), err
		}
		return nil, err
	}
	if result.Runtime == "process-unsafe" && memMaxMB > 0 {
		log.Printf("[workflow] sandbox isolated runtime unavailable, using output-limited process guard for %s", strings.Join(args, " "))
	}
	out := []byte(result.CombinedPreview)
	if err != nil {
		// 关键修复: out 为空时输出 err / FailureKind / RuntimeDetail / ExitCode, 让上层 build/test 检查能拿到可诊断信息。
		if len(out) == 0 {
			diag := buildLimitedCommandDiagnostics(result, err)
			out = []byte(diag)
		}
		if result.FailureKind != sandbox.FailureNone {
			return out, fmt.Errorf("%s: %w", result.FailureKind, err)
		}
		return out, err
	}
	return out, nil
}

// buildLimitedCommandDiagnostics 构造 sandbox 失败时的诊断信息, 用于 build/test 错误消息.
func buildLimitedCommandDiagnostics(result *sandbox.CommandResult, err error) string {
	if result == nil {
		if err != nil {
			return fmt.Sprintf("[runLimitedCommand] sandbox 无 result, err=%v", err)
		}
		return "[runLimitedCommand] sandbox 无 result 且无 err"
	}
	parts := []string{fmt.Sprintf("[runLimitedCommand] 命令未产生输出, sandbox 报告失败 (runtime=%s exit=%d)", result.Runtime, result.ExitCode)}
	if result.FailureKind != sandbox.FailureNone {
		parts = append(parts, fmt.Sprintf("failureKind=%s", result.FailureKind))
	}
	if result.RuntimeDetail != "" {
		parts = append(parts, fmt.Sprintf("runtimeDetail=%s", result.RuntimeDetail))
	}
	if result.LogDir != "" {
		parts = append(parts, fmt.Sprintf("logDir=%s", result.LogDir))
	}
	if err != nil {
		parts = append(parts, fmt.Sprintf("err=%v", err))
	}
	parts = append(parts, fmt.Sprintf("args=%s", strings.Join(result.Args, " ")))
	return strings.Join(parts, "\n")
}

func runBuildCheckLang(cwd, lang string) string {
	if cwd == "" {
		return ""
	}
	tc := GetToolchain(lang)
	if _, err := os.Stat(filepath.Join(cwd, tc.ProjectFile)); err != nil {
		if hasPrimarySourceFiles(cwd, tc) {
			return fmt.Sprintf("%s 未找到: 已发现源码文件，请在项目根目录初始化 %s 后再编译", tc.ProjectFile, tc.ProjectFile)
		}
		return ""
	}
	// MySQL/Percona 专用: 分阶段最小编译 (禁用测试, 仅构建核心目标)
	if lang == "cpp" && isMySQLProject(cwd) {
		return runMySQLBuildCheck(cwd)
	}
	if errText := ensureGoModuleDependencies(cwd, lang, tc); errText != "" {
		return errText
	}
	if tc.Language != "go" {
		return runBuildCheckLang(cwd, lang)
	}

	ctx, cancel := context.WithTimeout(context.Background(), tc.Timeout)
	defer cancel()

	var errors []string
	for _, args := range tc.BuildCmds {
		var env map[string]string
		if tc.Language == "go" {
			env = map[string]string{"GOFLAGS": "-mod=readonly"}
		}
		out, err := runLimitedCommandWithNetwork(ctx, cwd, args, tc.MemoryMaxMB, tc.CPUQuotaPercent, true, env)
		if err != nil {
			if tc.Language == "go" && isGoNoPackagesOutput(string(out)) {
				continue
			}
			errors = append(errors, fmt.Sprintf("%s 失败:\n%s", strings.Join(args, " "), string(out)))
		}
	}
	if len(errors) == 0 {
		return ""
	}
	result := strings.Join(errors, "\n\n")
	if len(result) > 3000 {
		result = result[:3000] + "\n...(截断)"
	}
	return result
}

func runBuildCheckScoped(cwd, lang string, targetPackages []string) string {
	if cwd == "" {
		return ""
	}
	if len(targetPackages) == 0 {
		return runBuildCheckLang(cwd, lang)
	}

	tc := GetToolchain(lang)
	if _, err := os.Stat(filepath.Join(cwd, tc.ProjectFile)); err != nil {
		if hasPrimarySourceFiles(cwd, tc) {
			return fmt.Sprintf("%s 未找到: 已发现源码文件，请在项目根目录初始化 %s 后再编译", tc.ProjectFile, tc.ProjectFile)
		}
		return ""
	}
	if errText := ensureGoModuleDependencies(cwd, lang, tc); errText != "" {
		return errText
	}
	if tc.Language != "go" {
		return runBuildCheckLang(cwd, lang)
	}
	targetPackages = existingGoTargetPackages(cwd, targetPackages)
	if len(targetPackages) == 0 {
		return runBuildCheckLang(cwd, lang)
	}

	ctx, cancel := context.WithTimeout(context.Background(), tc.Timeout)
	defer cancel()

	var errors []string
	tmpOutDir, _ := os.MkdirTemp("", "claude-go-buildcheck-*")
	if tmpOutDir != "" {
		defer os.RemoveAll(tmpOutDir)
	}
	for _, pkg := range targetPackages {
		args := []string{"go", "build", pkg}
		if tmpOutDir != "" && !strings.Contains(pkg, "...") {
			outName := strings.NewReplacer("/", "_", ".", "_").Replace(strings.Trim(pkg, "./"))
			if outName == "" {
				outName = "pkg"
			}
			args = []string{"go", "build", "-o", filepath.Join(tmpOutDir, outName), pkg}
		}
		var env map[string]string
		if tc.Language == "go" {
			env = map[string]string{"GOFLAGS": "-mod=readonly"}
		}
		out, err := runLimitedCommandWithNetwork(ctx, cwd, args, tc.MemoryMaxMB, tc.CPUQuotaPercent, true, env)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s 失败:\n%s", strings.Join(args, " "), string(out)))
		}
	}
	if len(errors) == 0 {
		return ""
	}
	result := strings.Join(errors, "\n\n")
	if len(result) > 3000 {
		result = result[:3000] + "\n...(截断)"
	}
	return result
}

func existingGoTargetPackages(cwd string, targetPackages []string) []string {
	if cwd == "" || len(targetPackages) == 0 {
		return nil
	}
	modulePath, _ := readGoModModuleAndRequires(filepath.Join(cwd, "go.mod"))
	modulePath = strings.TrimSpace(modulePath)
	var out []string
	seen := make(map[string]bool)
	for _, pkg := range targetPackages {
		pkg = strings.TrimSpace(filepath.ToSlash(pkg))
		if pkg == "" {
			continue
		}
		if modulePath != "" && strings.HasPrefix(pkg, modulePath+"/") {
			pkg = "./" + strings.TrimPrefix(pkg, modulePath+"/")
		}
		if strings.Contains(pkg, "...") {
			if !strings.HasPrefix(pkg, "./") && !strings.HasPrefix(pkg, ".") && !filepath.IsAbs(pkg) {
				pkg = "./" + pkg
			}
			if seen[pkg] {
				continue
			}
			out = append(out, pkg)
			seen[pkg] = true
			continue
		}
		if pkg == "." || pkg == "./" {
			if seen["."] {
				continue
			}
			out = append(out, ".")
			seen["."] = true
			continue
		}
		if !strings.HasPrefix(pkg, "./") {
			localDir := filepath.Join(cwd, filepath.FromSlash(pkg))
			if directoryHasGoFiles(localDir) {
				pkg = "./" + pkg
			} else {
				continue
			}
		}
		dir := filepath.Join(cwd, filepath.FromSlash(strings.TrimPrefix(pkg, "./")))
		if directoryHasGoFiles(dir) {
			if seen[pkg] {
				continue
			}
			out = append(out, pkg)
			seen[pkg] = true
		}
	}
	return out
}

func directoryHasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".go") {
			return true
		}
	}
	return false
}
