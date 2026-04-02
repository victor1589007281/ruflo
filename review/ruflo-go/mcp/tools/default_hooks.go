// default_hooks.go：在 init 时向 HookRegistry 注册默认 handler，使得
// hooks_pre-task / hooks_post-task / hooks_pre-command 等 MCP 工具调用 hookExec.Execute 时
// 能触发真实的业务逻辑，而不是空注册表空转。
//
// 对标 V3 TypeScript 中：
//   - TaskHooksManager  → PreTask（任务分析、Agent 推荐、复杂度评估、风险识别）
//   - TaskHooksManager  → PostTask（学习记录、活动统计）
//   - SessionHooksManager → SessionStart/End/Restore（活动重置、会话摘要）
//   - BashSafetyHook    → PreCommand（危险命令检测、密钥泄露检测、注入防护）
//   - FileOrganizationHook → PreEdit（根目录写入拦截、目录策略建议）
//   - SessionHooksManager → PostEdit/PostCommand（文件/命令活动追踪）
package tools

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/pkg/hooks"
	nlp "github.com/ruflo/ruflo-go/pkg/neural"
)

// sessionActivity 追踪当前会话内的活动指标（对标 V3 SessionHooksManager）。
type sessionActivity struct {
	mu              sync.Mutex
	sessionID       string
	startTime       time.Time
	tasksExecuted   int
	tasksSucceeded  int
	tasksFailed     int
	commandsExec    int
	filesModified   map[string]struct{}
	agentsSpawned   map[string]struct{}
}

var sessActivity = &sessionActivity{
	filesModified: make(map[string]struct{}),
	agentsSpawned: make(map[string]struct{}),
}

func (s *sessionActivity) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasksExecuted = 0
	s.tasksSucceeded = 0
	s.tasksFailed = 0
	s.commandsExec = 0
	s.filesModified = make(map[string]struct{})
	s.agentsSpawned = make(map[string]struct{})
}

// agentKeywords 用于 pre-task handler 分析任务描述，推荐合适的 Agent 类型。
// 对标 V3 TaskHooksManager.analyzeTask 中的关键词评分逻辑。
var agentKeywords = map[api.AgentType][]string{
	api.AgentTypeCoder:               {"implement", "code", "fix", "bug", "feature", "function", "class", "method", "refactor", "write"},
	api.AgentTypeTester:              {"test", "spec", "coverage", "qa", "assert", "expect", "mock", "stub"},
	api.AgentTypeReviewer:            {"review", "pr", "pull request", "check", "inspect", "audit code"},
	api.AgentTypeArchitect:           {"architect", "design", "structure", "pattern", "module", "system", "diagram"},
	api.AgentTypeResearcher:          {"research", "analyze", "investigate", "explore", "compare", "document", "readme"},
	api.AgentTypeSecurityArchitect:   {"security", "vulnerability", "cve", "exploit", "auth", "encrypt", "permission"},
	api.AgentTypePerformanceEngineer: {"performance", "optimize", "latency", "throughput", "benchmark", "profile", "cache"},
}

// complexityKeywords 用于复杂度评估。
var (
	highComplexityKW = []string{"complex", "large", "refactor", "migrate", "architecture", "distributed", "concurrent"}
	lowComplexityKW  = []string{"simple", "quick", "small", "trivial", "minor", "typo", "rename"}
)

// riskKeywords 用于风险识别。
var riskKeywords = map[string]string{
	"production":   "涉及生产环境变更",
	"delete":       "包含删除操作",
	"drop":         "包含 drop 操作",
	"database":     "涉及数据库变更",
	"migration":    "涉及数据迁移",
	"security":     "涉及安全相关变更",
	"credential":   "涉及凭证处理",
	"password":     "涉及密码处理",
	"deploy":       "涉及部署操作",
	"rollback":     "涉及回滚操作",
}

// dangerousCommandPatterns 对标 V3 BashSafetyHook 的危险命令正则。
var dangerousCommandPatterns = []struct {
	re    *regexp.Regexp
	risk  string
	block bool
}{
	{regexp.MustCompile(`\brm\s+(-[rR]f?|--recursive)\b`), "recursive deletion", true},
	{regexp.MustCompile(`\brm\s+-[fF]?r?f?\s+/`), "root deletion", true},
	{regexp.MustCompile(`\b(mkfs|fdisk|dd\s+if=)\b`), "disk format/overwrite", true},
	{regexp.MustCompile(`>\s*/dev/sd[a-z]`), "device overwrite", true},
	{regexp.MustCompile(`\bchmod\s+-R\s+777\b`), "insecure permissions", false},
	{regexp.MustCompile(`\bcurl\b.*\|\s*\b(bash|sh|zsh)\b`), "pipe to shell", true},
	{regexp.MustCompile(`\bwget\b.*\|\s*\b(bash|sh|zsh)\b`), "pipe to shell", true},
	{regexp.MustCompile(`\b(shutdown|reboot|halt|poweroff)\b`), "system shutdown", true},
	{regexp.MustCompile(`\bgit\s+push\s+.*--force\b`), "force push", false},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "hard reset", false},
	{regexp.MustCompile(`\b(DROP\s+TABLE|DROP\s+DATABASE|TRUNCATE)\b`), "destructive SQL", true},
	{regexp.MustCompile(`\bsudo\s+rm\b`), "sudo remove", true},
	{regexp.MustCompile(`\b:>\s*\S+`), "file truncation", false},
}

// secretPatterns 检测命令中可能的密钥泄露。
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|credential)\s*=\s*['"]?\S{8,}`),
	regexp.MustCompile(`(?i)bearer\s+[a-zA-Z0-9._~+/=-]{20,}`),
	regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`),
	regexp.MustCompile(`ghp_[a-zA-Z0-9]{36}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
}

// rootForbiddenExts 不应在项目根目录创建的文件类型。
var rootForbiddenExts = map[string]string{
	".go":   "src",
	".ts":   "src",
	".js":   "src",
	".py":   "src",
	".rs":   "src",
	".java": "src",
	".test": "tests",
	".spec": "tests",
	".md":   "docs",
}

// registerDefaultHooks 向 globalState.hookReg 注册所有默认 handler。
// 在 init() 中调用，对标 V3 的 TaskHooksManager/SessionHooksManager/BashSafetyHook/FileOrganizationHook。
func registerDefaultHooks() {
	reg := globalState.hookReg

	// ── PreTask: 任务分析、Agent 推荐、复杂度评估、风险识别 ──
	_ = reg.Register(hooks.HookEventPreTask, defaultPreTaskHandler, 100, "builtin:pre-task-analysis")

	// ── PostTask: 学习记录、活动统计 ──
	_ = reg.Register(hooks.HookEventPostTask, defaultPostTaskHandler, 100, "builtin:post-task-learning")
	_ = reg.Register(hooks.HookEventPostTask, defaultPostTaskActivityTracker, 50, "builtin:post-task-activity")

	// ── PreCommand: 危险命令检测 + 密钥泄露检测（Critical 优先级） ──
	_ = reg.Register(hooks.HookEventPreCommand, defaultPreCommandSafetyHandler, 200, "builtin:bash-safety")

	// ── PostCommand: 活动计数 ──
	_ = reg.Register(hooks.HookEventPostCommand, defaultPostCommandTracker, 50, "builtin:post-command-activity")

	// ── PreEdit: 根目录写入拦截、目录策略建议 ──
	_ = reg.Register(hooks.HookEventPreEdit, defaultPreEditFileOrgHandler, 150, "builtin:file-organization")

	// ── PostEdit: 文件修改追踪 ──
	_ = reg.Register(hooks.HookEventPostEdit, defaultPostEditTracker, 50, "builtin:post-edit-activity")

	// ── SessionStart: 重置活动计数 ──
	_ = reg.Register(hooks.HookEventSessionStart, defaultSessionStartHandler, 100, "builtin:session-start")

	// ── SessionEnd: 生成会话摘要 ──
	_ = reg.Register(hooks.HookEventSessionEnd, defaultSessionEndHandler, 100, "builtin:session-end")

	// ── SessionRestore: 恢复会话状态 ──
	_ = reg.Register(hooks.HookEventSessionRestore, defaultSessionRestoreHandler, 100, "builtin:session-restore")

	// ── AgentSpawn: 追踪 Agent ──
	_ = reg.Register(hooks.HookEventAgentSpawn, defaultAgentSpawnTracker, 50, "builtin:agent-spawn-activity")

	// ── PreRoute/PostRoute: 路由上下文注入 ──
	_ = reg.Register(hooks.HookEventPreRoute, defaultPreRouteHandler, 100, "builtin:pre-route")
}

// ═══════════════════════════════════════════════════════════════════════
// PreTask Handler — 任务分析、Agent 推荐、复杂度评估
// 对标 V3 TaskHooksManager.handlePreTask + analyzeTask
// ═══════════════════════════════════════════════════════════════════════

func defaultPreTaskHandler(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	desc := hc.Command
	if desc == "" {
		if v, ok := hc.Args["description"].(string); ok {
			desc = v
		}
	}
	if desc == "" {
		return hooks.HookResult{
			Success: true,
			Message: "no task description provided",
			Data:    map[string]any{"analyzed": false},
		}
	}

	lower := strings.ToLower(desc)

	// Agent 推荐：关键词评分
	type agentScore struct {
		agent api.AgentType
		score float64
	}
	var candidates []agentScore
	for agentType, keywords := range agentKeywords {
		hits := 0
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				hits++
			}
		}
		if hits > 0 {
			conf := 0.3 + float64(hits)*0.2
			if conf > 0.95 {
				conf = 0.95
			}
			candidates = append(candidates, agentScore{agentType, conf})
		}
	}
	// 按分数降序排序
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	suggested := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		suggested = append(suggested, map[string]any{
			"agent": string(c.agent), "confidence": c.score,
		})
	}
	if len(suggested) == 0 {
		suggested = append(suggested, map[string]any{
			"agent": string(api.AgentTypeCoder), "confidence": 0.5,
		})
	}

	// 复杂度评估
	complexity := 0.5
	for _, kw := range highComplexityKW {
		if strings.Contains(lower, kw) {
			complexity += 0.15
		}
	}
	for _, kw := range lowComplexityKW {
		if strings.Contains(lower, kw) {
			complexity -= 0.15
		}
	}
	if len(desc) > 200 {
		complexity += 0.1
	}
	if complexity < 0 {
		complexity = 0.1
	}
	if complexity > 1 {
		complexity = 1.0
	}

	// 时长估计
	estimatedMinutes := 30
	if complexity < 0.3 {
		estimatedMinutes = 5
	} else if complexity > 0.7 {
		estimatedMinutes = 120
	}

	// 风险识别
	var risks []map[string]any
	for kw, riskDesc := range riskKeywords {
		if strings.Contains(lower, kw) {
			risks = append(risks, map[string]any{"keyword": kw, "description": riskDesc})
		}
	}

	// ReasoningBank 模式检索
	var patterns []map[string]any
	if globalState.reasoningBank != nil {
		rbPatterns := globalState.reasoningBank.SearchPatterns(nil, 3)
		for _, p := range rbPatterns {
			patterns = append(patterns, map[string]any{
				"id": p.ID, "strategy": p.Strategy, "domain": p.Domain, "quality": p.Quality,
			})
		}
	}

	// ADR-026 风格模型路由：按复杂度分档
	var modelRouting map[string]any
	switch {
	case complexity > 0.7:
		modelRouting = map[string]any{"tier": 3, "model": "sonnet/opus"}
	case complexity >= 0.2:
		modelRouting = map[string]any{"tier": 2, "model": "haiku"}
	default:
		modelRouting = map[string]any{"tier": 1, "model": "agent_booster"}
	}

	return hooks.HookResult{
		Success: true,
		Message: "task analyzed",
		Data: map[string]any{
			"analyzed":          true,
			"suggested_agents":  suggested,
			"complexity":        complexity,
			"estimated_minutes": estimatedMinutes,
			"risks":             risks,
			"risk_count":        len(risks),
			"patterns":          patterns,
			"description":       desc,
			"model_routing":     modelRouting,
		},
	}
}

// ═══════════════════════════════════════════════════════════════════════
// PostTask Handler — 学习记录
// 对标 V3 TaskHooksManager.handlePostTask + recordLearning
// ═══════════════════════════════════════════════════════════════════════

func defaultPostTaskHandler(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	success, _ := hc.Args["success"].(bool)
	taskID, _ := hc.Args["task_id"].(string)

	learning := map[string]any{
		"patterns_updated":      0,
		"new_patterns":          0,
		"confidence_adjusted":   0,
		"trajectories_recorded": 1,
	}
	if success {
		learning["new_patterns"] = 1
	}

	// SONA 信号记录
	if globalState.sona != nil {
		kind := "post_task_success"
		if !success {
			kind = "post_task_failure"
		}
		globalState.sona.RecordSignal(nlp.Signal{
			Kind:      kind,
			Payload:   map[string]any{"task_id": taskID, "success": success},
			Timestamp: time.Now().UTC(),
		})
		learning["sona_signal"] = kind
	}

	return hooks.HookResult{
		Success: true,
		Message: "learning recorded",
		Data: map[string]any{
			"learning":  learning,
			"task_id":   taskID,
			"success":   success,
		},
	}
}

// defaultPostTaskActivityTracker 追踪任务完成统计（对标 V3 SessionHooksManager.trackTaskExecution）。
func defaultPostTaskActivityTracker(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	sessActivity.mu.Lock()
	sessActivity.tasksExecuted++
	success, _ := hc.Args["success"].(bool)
	if success {
		sessActivity.tasksSucceeded++
	} else {
		sessActivity.tasksFailed++
	}
	sessActivity.mu.Unlock()
	return hooks.HookResult{Success: true}
}

// ═══════════════════════════════════════════════════════════════════════
// PreCommand Handler — Bash 安全：危险命令检测 + 密钥泄露
// 对标 V3 BashSafetyHook.analyzeCommand
// ═══════════════════════════════════════════════════════════════════════

func defaultPreCommandSafetyHandler(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	cmd := hc.Command
	if cmd == "" {
		return hooks.HookResult{
			Success: true,
			Data:    map[string]any{"risk_level": "low", "checked": false},
		}
	}

	var risks []map[string]any
	blocked := false
	var blockReason string

	for _, dp := range dangerousCommandPatterns {
		if dp.re.MatchString(cmd) {
			risk := map[string]any{"pattern": dp.risk, "severity": "high"}
			if dp.block {
				risk["severity"] = "critical"
				risk["action"] = "blocked"
				blocked = true
				blockReason = dp.risk
			}
			risks = append(risks, risk)
		}
	}

	// 密钥泄露检测
	var secretWarnings []string
	for _, sp := range secretPatterns {
		if sp.MatchString(cmd) {
			secretWarnings = append(secretWarnings, "potential secret/credential detected in command")
			risks = append(risks, map[string]any{
				"pattern": "secret_exposure", "severity": "critical", "action": "warning",
			})
			break
		}
	}

	// 风险等级汇总
	riskLevel := "low"
	if len(risks) > 0 {
		riskLevel = "medium"
		for _, r := range risks {
			if r["severity"] == "critical" {
				riskLevel = "critical"
				break
			} else if r["severity"] == "high" {
				riskLevel = "high"
			}
		}
	}

	// 温和改写：rm 无 -i 时建议添加
	modifiedCmd := cmd
	isDestructive := false
	if strings.Contains(cmd, "rm ") && !strings.Contains(cmd, "-i") {
		modifiedCmd = strings.Replace(cmd, "rm ", "rm -i ", 1)
		isDestructive = true
	}

	result := hooks.HookResult{
		Success:  true,
		Warnings: secretWarnings,
		Data: map[string]any{
			"risk_level":     riskLevel,
			"risks":          risks,
			"risk_count":     len(risks),
			"blocked":        blocked,
			"is_destructive": isDestructive,
			"checked":        true,
			"command":        modifiedCmd,
		},
	}
	if blocked {
		result.Abort = true
		result.Message = "command blocked: " + blockReason
		result.Data["block_reason"] = blockReason
	}
	return result
}

// ═══════════════════════════════════════════════════════════════════════
// PostCommand Tracker
// ═══════════════════════════════════════════════════════════════════════

func defaultPostCommandTracker(_ context.Context, _ hooks.HookContext) hooks.HookResult {
	sessActivity.mu.Lock()
	sessActivity.commandsExec++
	sessActivity.mu.Unlock()
	return hooks.HookResult{Success: true}
}

// ═══════════════════════════════════════════════════════════════════════
// PreEdit Handler — 文件组织策略
// 对标 V3 FileOrganizationHook.analyzeFileOperation
// ═══════════════════════════════════════════════════════════════════════

func defaultPreEditFileOrgHandler(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	file := hc.File
	if file == "" {
		return hooks.HookResult{Success: true, Data: map[string]any{"checked": false}}
	}

	dir := filepath.Dir(file)
	ext := filepath.Ext(file)
	base := filepath.Base(file)
	isRoot := dir == "." || dir == "/" || dir == ""

	var issues []map[string]any
	var warnings []string
	suggestedDir := ""

	if isRoot {
		if recommended, ok := rootForbiddenExts[ext]; ok {
			issues = append(issues, map[string]any{
				"type":          "root-directory-violation",
				"file":          base,
				"recommended":   recommended,
				"message":       "should not be created in project root",
			})
			suggestedDir = recommended
			warnings = append(warnings, base+" should be in "+recommended+"/ directory, not project root")
		}
	}

	blocked := false
	for _, iss := range issues {
		if iss["type"] == "root-directory-violation" {
			blocked = true
			break
		}
	}

	result := hooks.HookResult{
		Success:  true,
		Warnings: warnings,
		Data: map[string]any{
			"checked":       true,
			"file":          file,
			"issues":        issues,
			"issue_count":   len(issues),
			"suggested_dir": suggestedDir,
			"blocked":       blocked,
		},
	}
	if blocked {
		result.Abort = true
		result.Message = "file should not be created in project root"
	}
	return result
}

// ═══════════════════════════════════════════════════════════════════════
// PostEdit Tracker
// ═══════════════════════════════════════════════════════════════════════

func defaultPostEditTracker(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	if hc.File != "" {
		sessActivity.mu.Lock()
		sessActivity.filesModified[hc.File] = struct{}{}
		sessActivity.mu.Unlock()
	}
	return hooks.HookResult{Success: true}
}

// ═══════════════════════════════════════════════════════════════════════
// Session Start/End/Restore Handlers
// 对标 V3 SessionHooksManager
// ═══════════════════════════════════════════════════════════════════════

func defaultSessionStartHandler(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	sid, _ := hc.Session["session_id"].(string)
	if sid == "" {
		sid = "session-" + time.Now().UTC().Format("20060102-150405")
	}
	sessActivity.mu.Lock()
	sessActivity.sessionID = sid
	sessActivity.startTime = time.Now().UTC()
	sessActivity.mu.Unlock()
	sessActivity.reset()

	return hooks.HookResult{
		Success: true,
		Message: "session started",
		Data: map[string]any{
			"session_id": sid,
			"start_time": sessActivity.startTime,
		},
	}
}

func defaultSessionEndHandler(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	sessActivity.mu.Lock()
	sid := sessActivity.sessionID
	start := sessActivity.startTime
	summary := map[string]any{
		"session_id":      sid,
		"duration_sec":    time.Since(start).Seconds(),
		"tasks_executed":  sessActivity.tasksExecuted,
		"tasks_succeeded": sessActivity.tasksSucceeded,
		"tasks_failed":    sessActivity.tasksFailed,
		"commands_run":    sessActivity.commandsExec,
		"files_modified":  len(sessActivity.filesModified),
		"agents_spawned":  len(sessActivity.agentsSpawned),
	}
	sessActivity.mu.Unlock()

	exportMetrics, _ := hc.Args["export_metrics"].(bool)

	return hooks.HookResult{
		Success: true,
		Message: "session ended",
		Data: map[string]any{
			"summary":        summary,
			"export_metrics": exportMetrics,
		},
	}
}

func defaultSessionRestoreHandler(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	sid, _ := hc.Session["session_id"].(string)
	if sid == "" {
		return hooks.HookResult{Success: true, Message: "no session_id to restore"}
	}

	// 从 globalState.sessions 查找历史会话
	globalState.mu.RLock()
	sess, ok := globalState.sessions[sid]
	globalState.mu.RUnlock()

	if !ok {
		return hooks.HookResult{
			Success:  true,
			Warnings: []string{"session " + sid + " not found in store"},
			Data:     map[string]any{"restored": false, "session_id": sid},
		}
	}

	newSID := "session-" + time.Now().UTC().Format("20060102-150405") + "-restored"
	sessActivity.mu.Lock()
	sessActivity.sessionID = newSID
	sessActivity.startTime = time.Now().UTC()
	sessActivity.mu.Unlock()
	sessActivity.reset()

	return hooks.HookResult{
		Success: true,
		Message: "session restored from " + sid,
		Data: map[string]any{
			"restored":     true,
			"restored_from": sid,
			"new_session_id": newSID,
			"original_data":  sess.Data,
		},
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Agent Spawn Tracker
// ═══════════════════════════════════════════════════════════════════════

func defaultAgentSpawnTracker(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	if hc.Agent != nil && hc.Agent.ID != "" {
		sessActivity.mu.Lock()
		sessActivity.agentsSpawned[hc.Agent.ID] = struct{}{}
		sessActivity.mu.Unlock()
	}
	return hooks.HookResult{Success: true}
}

// ═══════════════════════════════════════════════════════════════════════
// PreRoute Handler — 路由上下文增强
// ═══════════════════════════════════════════════════════════════════════

func defaultPreRouteHandler(_ context.Context, hc hooks.HookContext) hooks.HookResult {
	desc := hc.Command
	if desc == "" {
		if v, ok := hc.Args["description"].(string); ok {
			desc = v
		}
	}
	data := map[string]any{"routing_context": "enhanced"}
	if globalState.sona != nil {
		stats := globalState.sona.GetStats()
		data["sona_patterns_available"] = stats.TotalPatterns
		data["sona_avg_confidence"] = stats.AvgConfidence
	}
	return hooks.HookResult{Success: true, Data: data}
}

// init 注册所有默认 hooks（在 state.go init 之后执行，因 Go init 按依赖顺序调用）。
func init() {
	registerDefaultHooks()
}
