// Coordinator — 编排协调器, 提供心跳检查、检查点持久化和失败断点续作。
//
// 设计参考:
//   - LangGraph node-level checkpointing (每阶段存档)
//   - Temporal activity-level replay (确定性重放)
//   - Kimi K2.5 async_with_checkpoints (异步 + 检查点)
//   - 业界 Graceful Degradation (41-86.7% 生产故障率)
//
// 核心能力:
//   1. 检查点: 每个 stage 执行前后保存快照, 可从中恢复
//   2. 失败重试: 指数退避 + 最大重试次数
//   3. 心跳检测: 后台 goroutine 定期检查活跃 agent 状态
//   4. 断点续作: 重启后从最后成功的检查点恢复
//
//	┌──────────────────────────────────────────────────┐
//	│ Coordinator                                      │
//	│  RunWithRecovery() — 带恢复的工作流执行          │
//	│    ├─ loadCheckpoints() — 加载已保存的检查点      │
//	│    ├─ executeStageWithRetry() — 带重试的阶段执行  │
//	│    │   ├─ checkpoint("running") — 执行前存档     │
//	│    │   ├─ execute agent                          │
//	│    │   ├─ checkpoint("completed") — 成功后存档   │
//	│    │   └─ retry with backoff — 失败时重试        │
//	│    └─ heartbeatLoop() — 后台心跳检测             │
//	└──────────────────────────────────────────────────┘
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
	}
	c.loadCheckpoints()
	return c
}

// RunWithRecovery 带恢复能力的工作流执行。
// 根据已保存的 checkpoint 跳过已完成阶段, 对失败阶段重试。
// 改进: adversarial_dev 现在也支持检查点恢复 (设计阶段可跳过)。
// 参考: Temporal Workflow replay — 确定性重放已完成的活动。
func (c *Coordinator) RunWithRecovery(
	ctx context.Context,
	wf *WorkflowDef,
	objective string,
	team *ProductionTeam,
	executor *WorkflowExecutor,
) ([]StageResult, error) {
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go c.heartbeatLoop(hbCtx, team)

	switch wf.Mode {
	case "adversarial":
		return executor.Execute(ctx, wf, objective, team)
	case "adversarial_dev":
		// 检查点恢复和保存由 executor 内部细粒度处理 (每个 phase / 每个 task)
		return executor.Execute(ctx, wf, objective, team)
	default:
		return c.runPipelineWithRecovery(ctx, wf, objective, team, executor)
	}
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
			// 429/限流/熔断 → 更长退避 (基数 ×3)
			base := 2 * time.Second
			errLower := strings.ToLower(sr.Error)
			isRateLimit := strings.Contains(sr.Error, "429") ||
				strings.Contains(errLower, "rate limit") ||
				strings.Contains(errLower, "限流") ||
				strings.Contains(errLower, "熔断")
			if isRateLimit {
				base = 10 * time.Second
			}
			backoff := time.Duration(1<<uint(attempt)) * base
			if backoff > 120*time.Second {
				backoff = 120 * time.Second
			}
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
	team.mu.Lock()
	defer team.mu.Unlock()

	runningCount := 0
	for _, ag := range team.Agents {
		if ag.Status == AgentStatusRunning {
			runningCount++
		}
	}

	if c.pool != nil {
		stats := c.pool.Stats()
		if stats.ActiveCount > 0 || runningCount > 0 {
			log.Printf("[Coordinator] 心跳: pool活跃=%d, team运行中=%d",
				stats.ActiveCount, runningCount)
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
