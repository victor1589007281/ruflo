// Coordinator — 编排协调器, 提供心跳检查、检查点持久化和失败断点续作。
//
// 设计参考:
//   - LangGraph node-level checkpointing (每阶段存档)
//   - Temporal activity-level replay (确定性重放)
//   - Kimi K2.5 async_with_checkpoints (异步 + 检查点)
//   - 业界 Graceful Degradation (41-86.7% 生产故障率)
//
// 核心能力:
//
//  1. 检查点: 每个 stage 执行前后保存快照, 可从中恢复
//
//  2. 失败重试: 指数退避 + 最大重试次数
//
//  3. 心跳检测: 后台 goroutine 定期检查活跃 agent 状态
//
//  4. 断点续作: 重启后从最后成功的检查点恢复
//
//     ┌──────────────────────────────────────────────────┐
//     │ Coordinator                                      │
//     │  RunWithRecovery() — 带恢复的工作流执行          │
//     │    ├─ loadCheckpoints() — 加载已保存的检查点      │
//     │    ├─ executeStageWithRetry() — 带重试的阶段执行  │
//     │    │   ├─ checkpoint("running") — 执行前存档     │
//     │    │   ├─ execute agent                          │
//     │    │   ├─ checkpoint("completed") — 成功后存档   │
//     │    │   └─ retry with backoff — 失败时重试        │
//     │    └─ heartbeatLoop() — 后台心跳检测             │
//     └──────────────────────────────────────────────────┘
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/metrics"
)

// Checkpoint 阶段执行检查点。
type Checkpoint struct {
	StageName string    `json:"stageName"`
	Status    string    `json:"status"` // pending, running, completed, failed
	Attempt   int       `json:"attempt"`
	Output    string    `json:"output,omitempty"`
	Error     string    `json:"error,omitempty"`
	SavedAt   time.Time `json:"savedAt"`
}

// Coordinator 编排协调器。
type Coordinator struct {
	pool        *AgentPool
	taskTracker TaskTracker
	notify      NotifyFunc
	chatID      string

	checkpoints map[string]*Checkpoint
	mu          sync.Mutex
	dataDir     string

	maxRetries    int
	heartbeatFreq time.Duration

	// watchdog 状态
	lastActivity   time.Time
	lastActivityMu sync.Mutex

	// 进展型心跳: 跟踪任务的实际进度, 而非仅心跳
	// 心跳只证明 "进程活着", 进展证明 "任务在前进"
	progress   ProgressState
	progressMu sync.RWMutex

	// autoResumeCallback 自动恢复回调: 当 watchdog 检测到团队因瞬态错误失败时,
	// 自动触发恢复 (避免用户手动 /team resume)。
	// 由 ProductionTeamManager 在创建 Coordinator 时注入。
	autoResumeCallback func(teamName string)
}

// ProgressState 任务进展状态 (用于 watchdog 区分 "活着" 和 "在前进")
type ProgressState struct {
	Phase        string    `json:"phase"`        // 当前阶段: "LLM生成", "编译", "测试", "等待"
	Iteration    int       `json:"iteration"`    // 当前轮次/尝试次数
	UpdatedAt    time.Time `json:"updatedAt"`    // 上次更新进展时间
	BytesWritten int64     `json:"bytesWritten"` // 累计产出大小 (代码行数/文件字节数)
	TaskID       string    `json:"taskId"`       // 当前执行的任务 ID
}

// ReportProgress 上报进展 (由执行器在关键节点调用)
func (c *Coordinator) ReportProgress(phase string, iteration int, bytesWritten int64, taskID string) {
	c.progressMu.Lock()
	c.progress.Phase = phase
	c.progress.Iteration = iteration
	c.progress.BytesWritten = bytesWritten
	c.progress.TaskID = taskID
	c.progress.UpdatedAt = time.Now()
	c.progressMu.Unlock()
}

// ProgressAge 距上次真正进展的时间
func (c *Coordinator) ProgressAge() time.Duration {
	c.progressMu.RLock()
	defer c.progressMu.RUnlock()
	if c.progress.UpdatedAt.IsZero() {
		return time.Since(c.lastActivity) // 降级到 activity 心跳
	}
	return time.Since(c.progress.UpdatedAt)
}

// CurrentProgress 返回当前进展快照
func (c *Coordinator) CurrentProgress() ProgressState {
	c.progressMu.RLock()
	defer c.progressMu.RUnlock()
	return c.progress
}

// CoordinatorConfig 协调器配置。
type CoordinatorConfig struct {
	MaxRetries    int
	HeartbeatFreq time.Duration
	DataDir       string
	ChatID        string
}

// NewCoordinator 创建编排协调器。
func NewCoordinator(pool *AgentPool, taskTracker TaskTracker, notify NotifyFunc, cfg CoordinatorConfig) *Coordinator {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}
	if cfg.HeartbeatFreq <= 0 {
		cfg.HeartbeatFreq = 30 * time.Second
	}
	c := &Coordinator{
		pool:          pool,
		taskTracker:   taskTracker,
		notify:        notify,
		chatID:        cfg.ChatID,
		checkpoints:   make(map[string]*Checkpoint),
		dataDir:       cfg.DataDir,
		maxRetries:    cfg.MaxRetries,
		heartbeatFreq: cfg.HeartbeatFreq,
		lastActivity:  time.Now(),
	}
	c.loadCheckpoints()
	return c
}

// watchdogStaleThreshold 团队 watchdog: 超过此时间无进展则告警
const watchdogStaleThreshold = 5 * time.Minute

// watchdogCriticalThreshold 超过此时间无进展则尝试恢复
const watchdogCriticalThreshold = 15 * time.Minute

// watchdogCheckInterval watchdog 巡检间隔
const watchdogCheckInterval = 60 * time.Second

// TouchActivity 标记活动 (供 WorkflowExecutor 回调)
func (c *Coordinator) TouchActivity() {
	c.lastActivityMu.Lock()
	c.lastActivity = time.Now()
	c.lastActivityMu.Unlock()
}

// LastActivityAge 距离上次活动的时间
func (c *Coordinator) LastActivityAge() time.Duration {
	c.lastActivityMu.Lock()
	defer c.lastActivityMu.Unlock()
	return time.Since(c.lastActivity)
}

// RunWithRecovery 带恢复能力的工作流执行。
// 根据已保存的 checkpoint 跳过已完成阶段, 对失败阶段重试。
// 改进: 所有模式均受 watchdog 保护, adversarial_dev 支持检查点恢复。
// 参考: Temporal Workflow replay + K8s liveness probe 思路。
func (c *Coordinator) RunWithRecovery(
	ctx context.Context,
	wf *WorkflowDef,
	objective string,
	team *ProductionTeam,
	executor *WorkflowExecutor,
) ([]StageResult, error) {
	c.TouchActivity()

	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go c.heartbeatLoop(hbCtx, team)
	go c.teamWatchdog(hbCtx, team)

	switch wf.Mode {
	case "adversarial":
		return c.executeWithWatchdog(ctx, wf, objective, team, executor)
	case "adversarial_dev":
		return c.executeWithWatchdog(ctx, wf, objective, team, executor)
	case "orchestrated":
		return c.executeWithWatchdog(ctx, wf, objective, team, executor)
	case "trading_debate":
		return c.executeWithWatchdog(ctx, wf, objective, team, executor)
	case "creative_media":
		return c.executeWithWatchdog(ctx, wf, objective, team, executor)
	case "novel_writing":
		return c.executeWithWatchdog(ctx, wf, objective, team, executor)
	case "swarm_novel":
		return c.executeWithWatchdog(ctx, wf, objective, team, executor)
	default:
		return c.runPipelineWithRecovery(ctx, wf, objective, team, executor)
	}
}

// executeWithWatchdog 带超时保护的工作流执行 (adversarial 等非 pipeline 模式)。
// 关键改进: 之前 adversarial_dev 直接调 executor.Execute, 无 Coordinator 级恢复;
// 现在套一层超时 + 通知, 失败后尝试续作。
func (c *Coordinator) executeWithWatchdog(
	ctx context.Context,
	wf *WorkflowDef,
	objective string,
	team *ProductionTeam,
	executor *WorkflowExecutor,
) ([]StageResult, error) {
	executor.activityCallback = c.TouchActivity
	executor.progressCallback = c.ReportProgress

	results, err := executor.Execute(ctx, wf, objective, team)
	if err != nil {
		age := c.LastActivityAge()
		if age > watchdogStaleThreshold {
			c.notify(c.chatID, fmt.Sprintf(
				"⚠️ 工作流 **%s** 执行失败且已 %s 无活动, 检查是否需要手动恢复\n错误: %s",
				wf.Name, age.Round(time.Second), err.Error()))
		}

	}
	return results, err
}

// SetAutoResumeCallback 设置自动恢复回调 (由 ProductionTeamManager 注入)。
func (c *Coordinator) SetAutoResumeCallback(cb func(teamName string)) {
	c.autoResumeCallback = cb
}

// teamWatchdog 团队级 watchdog: 区分 "进程活着" 和 "任务在前进"。
//

// 两层检测:
//
//	L1 (activity 心跳): 5min 无 activity → 警告 (进程可能挂了)
//	L2 (progress 进展): 10min 进展不变 → 疑似卡住, 30min → 强制终止
//
// 进展不变的定义: Phase 和 Iteration 都没变化, 说明卡在同一个状态。
// 参考: K8s liveness probe (进程存活) + readiness probe (服务可用) 分离设计。
func (c *Coordinator) teamWatchdog(ctx context.Context, team *ProductionTeam) {
	ticker := time.NewTicker(watchdogCheckInterval)
	defer ticker.Stop()

	warned := false
	var stagnantRounds int // 连续无进展轮数
	var autoResumeAttempted bool

	// 进展检测阈值: 超过此时间 phase/iteration 不变即视为停滞
	progressStaleThreshold := 10 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// L1: Activity 心跳检测 (进程是否活着)
			age := c.LastActivityAge()

			// L2: 进展检测 (任务是否在前进)
			pAge := c.ProgressAge()
			prog := c.CurrentProgress()

			isProgressStagnant := pAge > progressStaleThreshold && prog.Phase != ""
			if isProgressStagnant {
				stagnantRounds++
			} else {
				stagnantRounds = 0
			}

			if stagnantRounds >= 3 { // 30min 无进展
				c.notify(c.chatID, fmt.Sprintf(
					"🔴 团队 **%s** 已 %s 无实际进展 (阶段: %s, 轮次: %d)\n"+
						"可能陷入无效循环, 建议 /team resume 或手动干预",
					team.Name, pAge.Round(time.Minute), prog.Phase, prog.Iteration))
				warned = false
			} else if stagnantRounds >= 1 { // 10min 无进展, 首次告警
				c.notify(c.chatID, fmt.Sprintf(
					"⏳ 团队 **%s** 已 %s 停留在阶段 [%s] 轮次 %d, 疑似卡住",
					team.Name, pAge.Round(time.Minute), prog.Phase, prog.Iteration))
				warned = true
			} else if age > watchdogCriticalThreshold {
				remaining := c.countRemainingTasks(team)
				c.notify(c.chatID, fmt.Sprintf(
					"🔴 团队 **%s** 已 **%s** 无活动 (%d 个剩余任务), 可能已卡住\n"+
						"▸ 建议: 运行 `/team status %s` 查看详情, 或 `/team resume %s` 尝试恢复",
					team.Name, age.Round(time.Second), remaining, team.Name, team.Name))
				warned = false
			} else if age > watchdogStaleThreshold && !warned {
				c.notify(c.chatID, fmt.Sprintf(
					"⚠️ 团队 **%s** 已 %s 无新进展, 持续监控中...",
					team.Name, age.Round(time.Second)))
				warned = true
			} else if age < watchdogStaleThreshold && stagnantRounds == 0 {
				warned = false
			}

			// L3: 自动恢复检测 (团队失败后自动 resume)
			// 当团队状态为 failed 且错误为瞬态错误 (API 超时/限流) 时,
			// 自动触发 ResumeTeam, 无需用户手动干预。
			if !autoResumeAttempted && c.autoResumeCallback != nil && team.Status == TeamStatusFailed {
				lower := strings.ToLower(team.Error)
				isTransient := strings.Contains(lower, "timeout") ||
					strings.Contains(lower, "deadline exceeded") ||
					strings.Contains(lower, "429") ||
					strings.Contains(lower, "rate limit") ||
					strings.Contains(lower, "限流") ||
					strings.Contains(lower, "overloaded") ||
					strings.Contains(lower, "超过最大重试次数")
				if isTransient {
					autoResumeAttempted = true
					c.notify(c.chatID, fmt.Sprintf(
						"🔄 团队 **%s** 检测到瞬态错误失败, 正在自动恢复...", team.Name))
					go c.autoResumeCallback(team.Name)
				}
			}
		}
	}
}

// countRemainingTasks 统计团队中未完成的任务数
func (c *Coordinator) countRemainingTasks(team *ProductionTeam) int {
	if team == nil {
		return 0
	}
	c.mu.Lock()
	total := 0
	completed := 0
	for _, cp := range c.checkpoints {
		total++
		if cp.Status == "completed" {
			completed++
		}
	}
	c.mu.Unlock()
	if total == 0 {
		return 0
	}
	return total - completed
}

func (c *Coordinator) runPipelineWithRecovery(
	ctx context.Context,
	wf *WorkflowDef,
	objective string,
	team *ProductionTeam,
	executor *WorkflowExecutor,
) ([]StageResult, error) {
	completed := make(map[string]bool)
	results := make(map[string]string)
	var allResults []StageResult

	// 恢复已完成的检查点
	for name, cp := range c.checkpoints {
		if cp.Status == "completed" && cp.Output != "" {
			completed[name] = true
			results[name] = cp.Output
			allResults = append(allResults, StageResult{
				Name:   name,
				Status: TaskCompleted,
				Output: cp.Output,
			})
			c.notify(c.chatID, fmt.Sprintf("♻️ 阶段 **%s** 从检查点恢复 (跳过)", name))
		}
	}

	for len(completed) < len(wf.Stages) {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

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
			return allResults, fmt.Errorf("工作流死锁: 无可执行阶段")
		}

		// 自动扩缩池
		if c.pool != nil {
			c.pool.AutoScale(len(ready))
		}

		parallelGroup := filterParallel(ready)
		if len(parallelGroup) > 1 {
			stageResults := c.executeParallelWithRetry(ctx, parallelGroup, objective, results, team, executor)
			for _, sr := range stageResults {
				allResults = append(allResults, sr)
				// 阶段一完成就增量刷盘 stages + 记录 stage 指标, dashboard 可实时看到进度
				c.flushTeamStages(team, allResults)
				c.recordStageMetrics(team, sr)
				if sr.Status == TaskCompleted {
					completed[sr.Name] = true
					results[sr.Name] = sr.Output
				} else {
					return allResults, fmt.Errorf("阶段 %s 失败 (已重试 %d 次): %s", sr.Name, c.maxRetries, sr.Error)
				}
			}
		} else {
			stage := ready[0]
			sr := c.executeStageWithRetry(ctx, stage, objective, results, team, executor)
			allResults = append(allResults, sr)
			c.flushTeamStages(team, allResults)
			c.recordStageMetrics(team, sr)
			if sr.Status == TaskCompleted {
				completed[stage.Name] = true
				results[stage.Name] = sr.Output
			} else {
				return allResults, fmt.Errorf("阶段 %s 失败 (已重试 %d 次): %s", stage.Name, c.maxRetries, sr.Error)
			}
		}
	}

	return allResults, nil
}

// flushTeamStages 把当前已产生的 stage 结果增量写回 team, 供 dashboard 实时查看。
// 注意: 调用方不持有 team.mu, 这里会短暂加锁拷贝 + 触发 persist。
func (c *Coordinator) flushTeamStages(team *ProductionTeam, snapshot []StageResult) {
	if team == nil {
		return
	}
	// 防御: 拷贝一份给 team, 避免后续 append 改动产生 data race。
	cp := make([]StageResult, len(snapshot))
	copy(cp, snapshot)
	team.mu.Lock()
	team.Stages = cp
	team.mu.Unlock()
	team.persist()
}

// recordStageMetrics 为单个 stage 结果上报细粒度指标: duration / retry / status。
// 这些数据供 dashboard 绘制"每个阶段耗时分布 / 重试次数"等图表。
func (c *Coordinator) recordStageMetrics(team *ProductionTeam, sr StageResult) {
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
	durSec := 0.0
	if sr.Duration != "" {
		if d, err := time.ParseDuration(sr.Duration); err == nil {
			durSec = d.Seconds()
		}
	}
	if durSec > 0 {
		mc.RecordRun("team", metrics.MTeamStageDurationSec, durSec, team.Name, labels)
	}
	mc.RecordRun("team", metrics.MTeamStageCount, 1, team.Name, labels)
	if sr.Status == TaskCompleted {
		mc.RecordRun("team", metrics.MTeamStageSuccessCount, 1, team.Name, labels)
	} else {
		mc.RecordRun("team", metrics.MTeamStageFailCount, 1, team.Name, labels)
	}
	if sr.Output != "" {
		mc.RecordRun("team", metrics.MTeamStageOutputLen, float64(len(sr.Output)), team.Name, labels)
	}
}

// executeStageWithRetry 带重试和检查点的阶段执行。
func (c *Coordinator) executeStageWithRetry(
	ctx context.Context,
	stage StageDef,
	objective string,
	prevResults map[string]string,
	team *ProductionTeam,
	executor *WorkflowExecutor,
) StageResult {
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if ctx.Err() != nil {
			return StageResult{Name: stage.Name, Status: TaskFailed, Error: "cancelled"}
		}

		c.saveCheckpoint(stage.Name, "running", attempt, "")

		// 每次重试也有独立超时保护
		stageCtx, stageCancel := context.WithTimeout(ctx, stageTimeout)
		sr := executor.ExecuteSingleStage(stageCtx, stage, objective, prevResults, team)
		stageCancel()

		if sr.Status == TaskCompleted {
			c.saveCheckpoint(stage.Name, "completed", attempt, sr.Output)
			return sr
		}

		c.saveCheckpoint(stage.Name, "failed", attempt, sr.Error)

		if attempt < c.maxRetries {
			base := 2 * time.Second
			errLower := strings.ToLower(sr.Error)
			isRateLimit := strings.Contains(sr.Error, "429") ||
				strings.Contains(errLower, "rate limit") ||
				strings.Contains(errLower, "rate_limit") ||
				strings.Contains(errLower, "throttl") ||
				strings.Contains(errLower, "限流") ||
				strings.Contains(errLower, "熔断") ||
				strings.Contains(errLower, "overloaded") ||
				strings.Contains(errLower, "过载") ||
				strings.Contains(errLower, "503") ||
				strings.Contains(errLower, "529") ||
				strings.Contains(errLower, "burstrate") ||
				strings.Contains(errLower, "allocationquota") ||
				strings.Contains(errLower, "ratequota")
			if isRateLimit {
				base = 15 * time.Second
			}
			backoff := time.Duration(1<<uint(attempt)) * base
			if backoff > 120*time.Second {
				backoff = 120 * time.Second
			}
			// 全抖动 (full jitter) 防止雷群效应
			jitter := time.Duration(rand.Float64() * float64(backoff))
			backoff = backoff/2 + jitter

			retryHint := ""
			if isRateLimit {
				retryHint = " (LLM 限流中, 延长等待)"
			}
			c.notify(c.chatID, fmt.Sprintf("⚠️ 阶段 **%s** 第 %d 次尝试失败%s, %.0f秒后重试...\n错误: %s",
				stage.Name, attempt+1, retryHint, backoff.Seconds(), sr.Error))

			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return StageResult{Name: stage.Name, Status: TaskFailed, Error: "cancelled during retry"}
			}
		}
	}

	return StageResult{
		Name:   stage.Name,
		Status: TaskFailed,
		Error:  fmt.Sprintf("超过最大重试次数 (%d)", c.maxRetries),
	}
}

// executeParallelWithRetry 并行执行多个阶段 (每个阶段带重试)。
func (c *Coordinator) executeParallelWithRetry(
	ctx context.Context,
	stages []StageDef,
	objective string,
	prevResults map[string]string,
	team *ProductionTeam,
	executor *WorkflowExecutor,
) []StageResult {
	results := make([]StageResult, len(stages))
	var wg sync.WaitGroup

	for i, stage := range stages {
		wg.Add(1)
		go func(idx int, s StageDef) {
			defer wg.Done()
			results[idx] = c.executeStageWithRetry(ctx, s, objective, prevResults, team, executor)
		}(i, stage)
	}

	wg.Wait()
	return results
}

// heartbeatLoop 后台心跳检测。
// 定期检查活跃 Agent 状态, 发现异常时通知。
func (c *Coordinator) heartbeatLoop(ctx context.Context, team *ProductionTeam) {
	ticker := time.NewTicker(c.heartbeatFreq)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkTeamHealth(team)
		}
	}
}

func (c *Coordinator) checkTeamHealth(team *ProductionTeam) {
	if team == nil {
		return
	}
	// 统计 running/idle agent (pipeline 模式)
	runningCount := 0
	idleCount := 0
	for _, ag := range team.Agents {
		switch ag.Status {
		case AgentStatusRunning:
			runningCount++
		case AgentStatusIdle:
			idleCount++
		}
	}
	// orchestrated 模式下 engine 使用独立 goroutine, team.Agents 均为 idle。
	// 通过 team.EngineRunning 显式标记判断引擎是否活跃 (而非依赖 team.Stages 启发式)。
	engineRunning := 0
	team.mu.Lock()
	isEngineRunning := team.EngineRunning
	team.mu.Unlock()
	if isEngineRunning {
		engineRunning = 1
	}

	if c.pool != nil {
		stats := c.pool.Stats()
		log.Printf("[Coordinator] 心跳: pool活跃=%d, team运行=%d idle=%d, 引擎=%d, 最后活动=%s前",
			stats.ActiveCount, runningCount, idleCount, engineRunning, c.LastActivityAge().Round(time.Second))
	}

	// 所有 agent 都 idle 但团队仍在 running → 可能卡住
	// orchestrated 模式下 engineRunning=1, 跳过此检查 (引擎用独立 goroutine)
	if engineRunning == 0 && runningCount == 0 && idleCount > 0 {
		c.mu.Lock()
		hasRemaining := false
		for _, cp := range c.checkpoints {
			if cp.Status != "completed" {
				hasRemaining = true
				break
			}
		}
		c.mu.Unlock()
		if hasRemaining {
			log.Printf("[Coordinator] 警告: 所有 agent idle 但存在未完成任务")
		}
	}
}

// --- 检查点持久化 ---

// CheckpointStore 检查点存取接口, 供 WorkflowExecutor / Orchestrator 在执行过程中细粒度存取。
// Coordinator 隐式实现此接口。
type CheckpointStore interface {
	SaveCheckpoint(stageName, status string, attempt int, output string)
	GetCheckpoint(stageName string) *Checkpoint
}

// SaveCheckpoint 保存阶段检查点 (导出, 供 WorkflowExecutor 调用)。
func (c *Coordinator) SaveCheckpoint(stageName, status string, attempt int, output string) {
	c.saveCheckpoint(stageName, status, attempt, output)
}

// GetCheckpoint 获取指定阶段的检查点 (导出, 供恢复逻辑使用)。
func (c *Coordinator) GetCheckpoint(stageName string) *Checkpoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkpoints[stageName]
}

func (c *Coordinator) saveCheckpoint(stageName, status string, attempt int, output string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cp := &Checkpoint{
		StageName: stageName,
		Status:    status,
		Attempt:   attempt,
		Output:    output,
		SavedAt:   time.Now(),
	}
	if status == "failed" {
		cp.Error = output
		cp.Output = ""
	}
	c.checkpoints[stageName] = cp
	c.persistCheckpoints()
}

func (c *Coordinator) persistCheckpoints() {
	if c.dataDir == "" {
		return
	}
	os.MkdirAll(c.dataDir, 0755)
	data, err := json.MarshalIndent(c.checkpoints, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.dataDir, "checkpoints.json"), data, 0644)
}

func (c *Coordinator) loadCheckpoints() {
	if c.dataDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(c.dataDir, "checkpoints.json"))
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &c.checkpoints)
}

// CompletedCount 返回已完成阶段的数量。
func (c *Coordinator) CompletedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, cp := range c.checkpoints {
		if cp.Status == "completed" && cp.Output != "" {
			count++
		}
	}
	return count
}

// ClearCheckpoints 清除所有检查点 (新工作流开始时调用)。
func (c *Coordinator) ClearCheckpoints() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkpoints = make(map[string]*Checkpoint)
	if c.dataDir != "" {
		os.Remove(filepath.Join(c.dataDir, "checkpoints.json"))
	}
}
