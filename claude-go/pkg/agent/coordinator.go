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
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
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

// watchdogProgressStaleThreshold L2 进展检测阈值: 超过此时间 phase/iteration 不变
// 即视为停滞。
//
// 原本是 teamWatchdog 内的局部变量。提成包级常量**不改任何数值**, 只是为了让
// design/01 §4.3 图级 watchdog 的生产接线 (graph_watchdog.go) 能引用同一个数字 ——
// 设计要求"参数沿用 coordinator.go", 而抄一份字面量就等于两处会各自漂移。
const watchdogProgressStaleThreshold = 10 * time.Minute

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

	// 路由单一真源: 与 WorkflowExecutor.Execute 的模式表共享 (dedicatedExecutorModes)。
	// 历史缺陷: 此处曾手抄一份 case 列表, 与 executor 的 switch 漂移, 导致
	// app_composite/game_composite 落 default 被静默降级为普通 pipeline 恢复,
	// 专用跨团队编排器永不触发 (design/01 §1.2-1)。
	if ModeHasDedicatedExecutor(wf.Mode) {
		return c.executeWithWatchdog(ctx, wf, objective, team, executor)
	}
	// 图引擎灰度 (design/01 M1): 开关命中时 pipeline/fanout 类走 executor.Execute
	// → executePipeline → executeGraph (ready-set 调度 + Journal 恢复), 取代
	// Coordinator 自带的 runPipelineWithRecovery。使灰度 flag 对真实团队生效
	// (此前 flag 只在直接调 executePipeline 时触发, 团队恒走本恢复路径故永不命中)。
	if graphEngineEnabled() {
		return c.executeWithWatchdog(ctx, wf, objective, team, executor)
	}
	return c.runPipelineWithRecovery(ctx, wf, objective, team, executor)
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

	// 进展检测阈值 (包级常量, 图级 watchdog 复用同一个数字, 见其定义处注释)
	progressStaleThreshold := watchdogProgressStaleThreshold

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

	// 恢复已完成的检查点。
	// 只恢复当前工作流定义中存在的阶段: 工作流定义变更(拆分/改名阶段)后, 残留旧阶段名的
	// 检查点会把 completed 数量虚增到 >= len(wf.Stages), 使下方 for 循环直接退出,
	// 尾部阶段被静默跳过且团队仍报 completed (曾致 qc 阶段两次未执行)。
	currentStages := make(map[string]bool, len(wf.Stages))
	for _, s := range wf.Stages {
		currentStages[s.Name] = true
	}
	for name, cp := range c.checkpoints {
		if !currentStages[name] {
			continue
		}
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
	// 委托给图侧那份实现: 两者的 4 个 label 与指标项逐一相同, 保留两份必然漂移。
	// 图侧额外记重试次数, pipeline 路径的重试由本 Coordinator 自己吞掉, 故传 0。
	recordGraphStageMetrics(team, sr, 0)
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
	// H0-4 重试诊断状态: 上一次失败的指纹, 以及本阶段是否已提升过超时。
	// 两者都是跨尝试状态 —— 没有它们, "重试"就退化成"原样重跑",
	// 实测代价是 mr-trading-v2 约 48 分钟的墙钟空转。
	prevSignature := ""
	escalated := false
	stageTimeoutForRun := stageTimeout

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if ctx.Err() != nil {
			return StageResult{Name: stage.Name, Status: TaskFailed, Error: "cancelled"}
		}

		c.saveCheckpoint(stage.Name, "running", attempt, "")

		// 每次重试也有独立超时保护。这里必须使用 executor 自己的角色级
		// 超时 (coder 25min / tester 15min / 等等), 而不是硬编码的 stageTimeout
		// (10min). 否则 WorkflowExecutor.executeStageWithRetry 内层即便把
		// timeout 设到 25min, 也会被 Coordinator 的这个 10min 包裹截断,
		// 导致大功能 (Phase1+Phase2) 阶段反复在 10min 处被杀.
		stageTimeoutForRole := stageTimeoutForRun
		if executor != nil {
			stageTimeoutForRole = executor.computeStageTimeout(stage.Agent, attempt)
			// 提升只在原值之上, 不被角色超时压回去 (提升的目的就是突破原值)。
			if stageTimeoutForRun > stageTimeoutForRole {
				stageTimeoutForRole = stageTimeoutForRun
			}
		}
		stageCtx, stageCancel := context.WithTimeout(ctx, stageTimeoutForRole)
		// 标记外层重试驱动: 内层 WorkflowExecutor.executeStageWithRetry 见此标记
		// 退化为单次尝试, 消除 Coordinator(maxRetries) × 内层(3+限流) 的嵌套放大
		// (design/01 §1.2-4)。
		sr := executor.ExecuteSingleStage(WithOuterRetryDriven(stageCtx), stage, objective, prevResults, team)
		stageCancel()

		if sr.Status == TaskCompleted {
			c.saveCheckpoint(stage.Name, "completed", attempt, sr.Output)
			return sr
		}

		// 先落检查点再判分流: 分流的三条出口 (预算/放弃/重试耗尽) 都会直接 return,
		// 漏掉这一句会让 checkpoint 永远停在上面那句 "running" —— 磁盘上留下的
		// 最后一条记录说"这个阶段正在跑", 而进程早就走了。
		c.saveCheckpoint(stage.Name, "failed", attempt, sr.Error)

		// P0-1 run 预算耗尽: 不进重试环。
		//
		// 为什么这条要**独立于**下面那个 classifyStageFailureWith (它也会对预算
		// 返回 stageRetryAbort): 那个调用点在 `if attempt < c.maxRetries` 里, 而
		// c.maxRetries 是可配的 —— 取 0 时分类器**一次都不会被调用**, 循环直接落到
		// 末尾返回一句合成的 "超过最大重试次数 (0)", 把 cause 整个丢掉。闸门刚判完
		// "这个 run 花超了", 台账上却记成"重试次数用完了", 这是两件不同的事。
		// 预算终止是**终态**, 它的判据不该依赖重试预算配了几次。
		if errors.Is(sr.cause, internal_hook.ErrRunBudgetExhausted) {
			c.notify(c.chatID, fmt.Sprintf("⛔ 阶段 **%s** 终止: run 预算已耗尽", stage.Name))
			return sr
		}

		if attempt < c.maxRetries {
			// H0-4: 重试前先诊断 —— 该不该重试、要不要改变状态。
			action, sig, why := classifyStageFailureWith(sr.Error, sr.cause, prevSignature, escalated)
			switch action {
			case stageRetryAbort:
				c.notify(c.chatID, fmt.Sprintf("⛔ 阶段 **%s** 不再重试: %s", stage.Name, why))
				// cause 必须原样带出去。上面那句 fmt.Sprintf 会把它变成纯文本,
				// 而文本不是类型 —— 调用方 (RunWithRecovery / 上层团队编排) 再想
				// 区分"预算耗尽"与"输出不合格"就只能去猜字符串。
				sr.Error = fmt.Sprintf("%s (已放弃重试: %s)", sr.Error, why)
				return sr
			case stageRetryEscalate:
				// 改变状态后再试: 同一失败原样重跑没有信息增益, 提升超时才是新尝试。
				stageTimeoutForRun = stageRetryEscalation(stageTimeoutForRun)
				escalated = true
				c.notify(c.chatID, fmt.Sprintf(
					"🔧 阶段 **%s** 同一失败复现, 超时提升至 %s 后重试",
					stage.Name, stageTimeoutForRun))
			}
			prevSignature = sig

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

// stageRetryAction 是阶段失败后的处置决定。
type stageRetryAction int

const (
	// stageRetryAsIs 原样重试 (失败原因与上次不同, 再试一次有意义)。
	stageRetryAsIs stageRetryAction = iota
	// stageRetryEscalate 提升超时后重试 —— 即"改变状态"。
	stageRetryEscalate
	// stageRetryAbort 不再重试, 直接判该阶段失败。
	stageRetryAbort
)

// stageNonRetryableMarkers 是不会因重试而改变的错误族。
//
// 判据来源是引擎侧 ErrorClassifier 的 DefaultBudgets: BadRequest 与 Auth 的
// MaxAttempts 都是 0 (即不重试)。这里保持一致 —— 同一个错误在 LLM 层不重试、
// 到了阶段层却重试 3 次, 是两套语义打架。
var stageNonRetryableMarkers = []string{
	"400", "401", "403", "422",
	"bad request", "invalid request", "unauthorized", "forbidden",
	"context canceled", "cancelled", "取消",
}

// stageFailureSignature 归一化错误文本, 作为"是不是同一个失败"的指纹。
//
// 归一化掉数字与时长: `agent execution timeout after 15m59.7s` 与
// `after 16m01.2s` 是**同一个**失败, 若按原文比较会被当成两次不同失败,
// 于是无限重试 —— 这正是实测中 ~48 分钟空转的成因。
func stageFailureSignature(errMsg string) string {
	lower := strings.ToLower(errMsg)
	var b strings.Builder
	b.Grow(len(lower))
	prevDigit := false
	for _, r := range lower {
		switch {
		case r >= '0' && r <= '9':
			// 连续数字折叠成单个 #, 使 15.7 / 16.2 / 3 归一到同一个指纹。
			if !prevDigit {
				b.WriteByte('#')
			}
			prevDigit = true
		case r == ' ' || r == '\t' || r == '\n':
			prevDigit = false
		default:
			b.WriteRune(r)
			prevDigit = false
		}
	}
	return strings.TrimSpace(b.String())
}

// classifyStageFailure 决定一个失败的阶段该重试、该换策略、还是该放弃。
//
// 返回 (处置, 本次失败指纹, 处置理由)。
//
// 三条判据, 按优先级:
//  1. 不可重试族 (400/401/403/422/cancelled) ⇒ 立即放弃。重试不会改变它们,
//     只会把一次确定性失败放大成 3 次。
//  2. 与上一次**同一指纹** ⇒ 提升超时后重试 (改变状态)。原样重试同一个失败
//     是纯浪费: 实测 mr-trading-v2 的三个阶段各自撞同一个 16 分钟硬超时、
//     各重试 3 次, 约 48 分钟墙钟花在重跑注定失败的事上。
//  3. 已提升过超时、且仍是同一指纹 ⇒ 放弃。继续加超时是在赌, 不是修。
//  4. 指纹不同 ⇒ 原样重试 (新原因, 再试一次有意义)。
//
// escalated 由调用方维护 (本阶段此前是否已提升过超时), 因为"是否已改变过状态"
// 是跨尝试的状态, 不是单次失败的属性。
func classifyStageFailure(errMsg, prevSignature string, escalated bool) (stageRetryAction, string, string) {
	return classifyStageFailureWith(errMsg, nil, prevSignature, escalated)
}

// classifyStageFailureWith 带原始 error 的版本, 用于识别类型化终态 (预算耗尽)。
//
// 为什么不能只看文本: "预算耗尽"的文案里带 limit 与 spent 两个数字, 而两次重试
// 之间的累计消耗必然不同 ⇒ 归一化后的指纹也不同 ⇒ 文本判据会把它当成"新原因"
// 而继续重试。那是这道闸门存在的反面: 闸门刚判完"这个 run 花超了", 重试环立刻
// 再花一笔。只有拿到 error 本身, errors.Is 才能给出一条不依赖文案的分流。
func classifyStageFailureWith(errMsg string, cause error, prevSignature string, escalated bool) (stageRetryAction, string, string) {
	sig := stageFailureSignature(errMsg)
	lower := strings.ToLower(errMsg)

	// 预算耗尽**先于**一切文本判据 —— 它是终态, 不是"又一个失败"。
	if cause != nil && errors.Is(cause, internal_hook.ErrRunBudgetExhausted) {
		return stageRetryAbort, sig, "run 预算已耗尽, 重试只会继续烧钱"
	}

	for _, marker := range stageNonRetryableMarkers {
		if strings.Contains(lower, marker) {
			// "cancelled" 是用户/上下文取消, 同样不该重试。
			return stageRetryAbort, sig, "不可重试的错误族 (" + marker + ")"
		}
	}

	if prevSignature != "" && sig == prevSignature {
		if escalated {
			return stageRetryAbort, sig, "同一失败重复出现且已提升过超时, 停止重试"
		}
		return stageRetryEscalate, sig, "同一失败重复出现, 提升超时后重试"
	}

	return stageRetryAsIs, sig, ""
}

// stageRetryEscalation 返回提升后的阶段超时 (翻倍, 封顶 2 倍原值)。
func stageRetryEscalation(cur time.Duration) time.Duration {
	next := cur * 2
	if cap := stageTimeout * 2; next > cap {
		next = cap
	}
	return next
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

	// 实时心跳落盘: 把当前进展快照写入 team.json, 让既有展示入口
	// (team status / dashboard) 能看到团队内部实时状态, 而非只有结束态。
	team.updateHeartbeat(c.CurrentProgress())
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
	_ = os.WriteFile(filepath.Join(c.dataDir, checkpointsFileName), data, 0644)
}

func (c *Coordinator) loadCheckpoints() {
	if c.dataDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(c.dataDir, checkpointsFileName))
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
		os.Remove(filepath.Join(c.dataDir, checkpointsFileName))
	}
}

// InvalidateCheckpoints 从 dataDir 的 checkpoints.json 中删除指定阶段的检查点, 使其在下次
// resume 时重新执行 (保留其余已完成阶段)。供 RefineTeam 做"从目标阶段起增量重跑"。
// 不依赖 Coordinator 实例; 直接操作磁盘上的检查点文件。
func InvalidateCheckpoints(dataDir string, stageNames []string) error {
	if dataDir == "" || len(stageNames) == 0 {
		return nil
	}
	path := filepath.Join(dataDir, checkpointsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // 无检查点文件 → 无需失效
	}
	var cps map[string]*Checkpoint
	if err := json.Unmarshal(data, &cps); err != nil {
		return err
	}
	for _, n := range stageNames {
		delete(cps, n)
	}
	out, err := json.MarshalIndent(cps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

// checkpointsFileName / graphJournalDirName 两个进度真源在团队 dataDir 下的落点。
//
// 提成常量不是洁癖: 这两个名字此前是散在 coordinator.go / teams.go / graph_adapter.go
// 里的字面量, 而"清进度"这件事必须同时覆盖两者 —— 少写一处的后果见 clearRunProgress。
const (
	checkpointsFileName = "checkpoints.json"
	graphJournalDirName = "graph-journal"
)

// clearRunProgress 清空一个团队 dataDir 下**全部磁盘进度真源**, 使下一次运行从零开始。
//
// ---------------------------------------------------------------------------
// 为什么必须一次清两个源, 而不是"清当前生效的那一个"
// ---------------------------------------------------------------------------
//
// design/01 §4.3 要 journal 作唯一进度真源, 但归一是渐进的: 眼下
// checkpoints.json (旧 pipeline 恢复 + 各 mode 专用执行器) 与 graph-journal
// (图引擎; **orchestrated 无条件走它, 不看灰度开关**) 并存, 哪个生效由
// graphEngineEnabled() 与 mode 表联合决定。
//
// 在这里复算一遍"谁生效"等于把那张模式表抄第二份 —— 那正是 §1.2 记的
// "双 switch 漂移"缺陷的形状 (app_composite/game_composite 曾因此被静默降级)。
// 清一个本轮用不到的源是无害的 (它本就只代表"上一轮"); 漏清一个是数据正确性事故:
//
//	用户带**新目标**重跑一个跑挂过的团队 → 旧代码只清 checkpoints.json,
//	graph-journal 原样留着 → Engine.Run(Resume:true) 重放上一轮全部
//	node.completed → 本轮**零个节点执行**, 直接把旧目标的产出当成新目标的交付
//	返回, 且团队报 completed。用户换的那个目标静默消失。
//
// 这与 refine 静默失效是同一族缺陷 (一个生命周期动作只认识两个源里的一个),
// 只是方向相反: 那次是"该失效的没失效", 这次是"该清空的没清空"。
//
// 等价性由 progress_source_equiv_test.go 的 S1/S2/S3/S4/S5/S5b 六场景钉住
// (两条路径逐项比对 + 变异反证)。
func clearRunProgress(dataDir string) {
	if dataDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dataDir, checkpointsFileName))
	_ = os.RemoveAll(filepath.Join(dataDir, graphJournalDirName))
}
