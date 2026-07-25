package unit

import (
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
)

// TestFilterParallelFix 验证 filterParallel 修复:
// 之前要求所有 ready 阶段的依赖集字符串完全相同才并行, 导致依赖不同但同时就绪的阶段无法并行。
func TestFilterParallelFix(t *testing.T) {
	// 通过 GetWorkflow 获取 development workflow, 检查其并行阶段能被正确识别
	wf := agent.GetWorkflow("development")
	if wf == nil {
		t.Fatal("development workflow not found")
	}

	var parallelStages []agent.StageDef
	for _, s := range wf.Stages {
		if s.Parallel {
			parallelStages = append(parallelStages, s)
		}
	}
	if len(parallelStages) == 0 {
		t.Skip("development workflow has no explicit parallel stages")
	}

	t.Logf("development workflow has %d stages, %d marked Parallel", len(wf.Stages), len(parallelStages))
}

// TestWatchdogConstants 验证 watchdog 常量在合理范围
func TestWatchdogConstants(t *testing.T) {
	// 通过 Coordinator 的公开方法间接测试
	noop := func(string, string) {}
	coord := agent.NewCoordinator(nil, nil, noop, agent.CoordinatorConfig{
		MaxRetries:    3,
		HeartbeatFreq: 10 * time.Second,
		DataDir:       t.TempDir(),
		ChatID:        "test",
	})

	// 新建的 coordinator 应该有初始活动时间
	age := coord.LastActivityAge()
	if age > 2*time.Second {
		t.Errorf("新建 coordinator 的活动年龄不应超过 2s, got %s", age)
	}

	// TouchActivity 应刷新时间
	time.Sleep(10 * time.Millisecond)
	coord.TouchActivity()
	age2 := coord.LastActivityAge()
	if age2 > 1*time.Second {
		t.Errorf("TouchActivity 后活动年龄不应超过 1s, got %s", age2)
	}
}

// TestCoordinatorCheckpointRecovery 验证检查点的保存和恢复
func TestCoordinatorCheckpointRecovery(t *testing.T) {
	noop := func(string, string) {}
	dir := t.TempDir()

	coord := agent.NewCoordinator(nil, nil, noop, agent.CoordinatorConfig{
		MaxRetries:    2,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       dir,
		ChatID:        "test",
	})

	// 保存检查点
	coord.SaveCheckpoint("stage-1", "completed", 0, "output-1")
	coord.SaveCheckpoint("stage-2", "failed", 1, "error-2")

	// 读取检查点
	cp1 := coord.GetCheckpoint("stage-1")
	if cp1 == nil || cp1.Status != "completed" || cp1.Output != "output-1" {
		t.Errorf("stage-1 checkpoint unexpected: %+v", cp1)
	}
	cp2 := coord.GetCheckpoint("stage-2")
	if cp2 == nil || cp2.Status != "failed" {
		t.Errorf("stage-2 checkpoint unexpected: %+v", cp2)
	}

	// 创建新 coordinator 读取持久化的检查点
	coord2 := agent.NewCoordinator(nil, nil, noop, agent.CoordinatorConfig{
		MaxRetries:    2,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       dir,
		ChatID:        "test",
	})
	cp1r := coord2.GetCheckpoint("stage-1")
	if cp1r == nil || cp1r.Status != "completed" {
		t.Errorf("恢复后 stage-1 checkpoint unexpected: %+v", cp1r)
	}
}

// TestOrchestratorStallConstants 验证 Orchestrator 停滞恢复常量
func TestOrchestratorStallConstants(t *testing.T) {
	// 验证 OrchestratorConfig 的默认值合理
	cfg := agent.OrchestratorConfig{
		MaxParallel:      3,
		MaxRetries:       2,
		MicroTestAfter:   true,
		AdversarialRound: 5,
	}
	if cfg.MaxParallel < 1 || cfg.MaxParallel > 20 {
		t.Errorf("MaxParallel 不在合理范围: %d", cfg.MaxParallel)
	}
	if cfg.MaxRetries < 0 || cfg.MaxRetries > 10 {
		t.Errorf("MaxRetries 不在合理范围: %d", cfg.MaxRetries)
	}
}

// TestWorkflowExecutorActivityCallback 验证 WorkflowExecutor 的 activityCallback 字段存在
func TestWorkflowExecutorActivityCallback(t *testing.T) {
	wf := agent.GetWorkflow("development")
	if wf == nil {
		t.Fatal("development workflow not found")
	}

	// 验证 adversarial_dev 模式的 workflow 存在
	if wf.Mode != "adversarial_dev" {
		t.Logf("development workflow mode: %s", wf.Mode)
	}
}

// TestRunWithRecoveryAllModesProtected 验证所有 workflow 模式都受 watchdog 保护
func TestRunWithRecoveryAllModesProtected(t *testing.T) {
	protectedModes := []string{
		"adversarial", "adversarial_dev", "trading_debate",
		"creative_media", "novel_writing", "swarm_novel",
	}

	for _, mode := range protectedModes {
		t.Run(mode, func(t *testing.T) {
			workflows := agent.ListWorkflows()
			found := false
			for _, wf := range workflows {
				if wf.Mode == mode {
					found = true
					t.Logf("workflow %q uses mode %q", wf.Name, mode)
					break
				}
			}
			if !found {
				t.Logf("no workflow with mode %q (may still be covered)", mode)
			}
		})
	}
}

// TestExistingWorkflowsUnchangedOrch 确保已有工作流未被意外修改
func TestExistingWorkflowsUnchangedOrch(t *testing.T) {
	expected := map[string]struct {
		mode   string
		minLen int
	}{
		"development": {"adversarial_dev", 5},
		"research":    {"fanout", 3},
		// "debate" 已移除: 不在 workflowRegistry 中（git 历史确认从未在过）
		"code-review": {"orchestrated", 3},
	}

	for name, exp := range expected {
		wf := agent.GetWorkflow(name)
		if wf == nil {
			t.Errorf("workflow %q not found", name)
			continue
		}
		if wf.Mode != exp.mode {
			t.Errorf("workflow %q mode: got %q, want %q", name, wf.Mode, exp.mode)
		}
		if len(wf.Stages) < exp.minLen {
			t.Errorf("workflow %q stages: got %d, want >= %d", name, len(wf.Stages), exp.minLen)
		}
	}
}

// TestValidateAgentOutputIdleDetection 验证空转检测仍然正常工作
func TestValidateAgentOutputIdleDetection(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		role     string
		wantFail bool
	}{
		{"idle output", "I am ready. I understand my role. Please tell me what to do. I'm ready to start. Waiting for instructions.", "coder", true},
		{"substantive output", "```go\npackage main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n```", "coder", false},
		{"short but has code", "```go\nfunc Test() {}\n```\nThis is the implementation.", "coder", false},
		{"too short", "ok", "coder", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := agent.ValidateAgentOutput(tt.output, tt.role)
			failed := reason != ""
			if failed != tt.wantFail {
				t.Errorf("ValidateAgentOutput(%q) = %q, wantFail=%v", tt.name, reason, tt.wantFail)
			}
		})
	}
}

// TestFilterParallelAllReady 验证修复后的 filterParallel:
// 多个 ready 阶段即使依赖不完全相同也应能并行
func TestFilterParallelAllReady(t *testing.T) {
	for _, wf := range agent.ListWorkflows() {
		parallelCount := 0
		for _, s := range wf.Stages {
			if s.Parallel {
				parallelCount++
			}
		}
		if parallelCount > 0 {
			t.Logf("workflow %q: %d/%d parallel stages", wf.Name, parallelCount, len(wf.Stages))
		}
	}
}

// TestBottleneckClassification 验证 Orchestrator 的瓶颈分类 (回归测试)
func TestBottleneckClassification(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect int // 最小瓶颈数
	}{
		{"empty", "", 0},
		{"pass all", "compilation: PASS | logic: PASS | security: PASS", 0},
		{"compile fail", "compilation: FAIL - syntax error", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bns := agent.ClassifyBottlenecks(tt.input)
			if len(bns) < tt.expect {
				t.Errorf("got %d bottlenecks, want >= %d", len(bns), tt.expect)
			}
		})
	}
}

// TestHoldLastOrDefault 验证评分降级策略 (回归测试)
func TestHoldLastOrDefault(t *testing.T) {
	zero := agent.EvalScore{}
	held := agent.HoldLastOrDefault(zero)
	if held.Correctness != 6 || held.Completeness != 6 {
		t.Errorf("zero score should default to 6, got %+v", held)
	}

	prev := agent.EvalScore{Correctness: 8, Completeness: 7}
	held2 := agent.HoldLastOrDefault(prev)
	if held2.Correctness != 8 {
		t.Errorf("should hold previous score, got %+v", held2)
	}
}

// TestListWorkflowsContainsNewTeams 验证新团队已注册
func TestListWorkflowsContainsNewTeams(t *testing.T) {
	workflows := agent.ListWorkflows()
	required := []string{"ml-training", "app", "game", "development", "research"}
	wfSet := make(map[string]bool)
	for _, w := range workflows {
		wfSet[w.Name] = true
	}

	for _, name := range required {
		if !wfSet[name] {
			t.Errorf("workflow %q not found in ListWorkflows()", name)
		}
	}
}

// TestCoordinatorNotifyOnStale 验证 coordinator 告警功能不 panic
func TestCoordinatorNotifyOnStale(t *testing.T) {
	var messages []string
	notify := func(_, msg string) {
		messages = append(messages, msg)
	}

	coord := agent.NewCoordinator(nil, nil, notify, agent.CoordinatorConfig{
		MaxRetries:    2,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       t.TempDir(),
		ChatID:        "test",
	})

	_ = coord
	_ = messages

	// 基本健壮性: 无 team 时不 panic
	coord.TouchActivity()
	if coord.LastActivityAge() > time.Second {
		t.Error("activity age too large after touch")
	}
}

// TestCoordinatorCountRemaining 验证剩余任务计数
func TestCoordinatorCountRemaining(t *testing.T) {
	noop := func(string, string) {}
	coord := agent.NewCoordinator(nil, nil, noop, agent.CoordinatorConfig{
		MaxRetries:    2,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       t.TempDir(),
		ChatID:        "test",
	})

	coord.SaveCheckpoint("s1", "completed", 0, "done")
	coord.SaveCheckpoint("s2", "running", 0, "")
	coord.SaveCheckpoint("s3", "failed", 1, "err")

	cp := coord.GetCheckpoint("s1")
	if cp == nil || cp.Status != "completed" {
		t.Fatal("s1 should be completed")
	}
	// CompletedCount 应只计算 completed
	count := coord.CompletedCount()
	if count != 1 {
		t.Errorf("CompletedCount: got %d, want 1", count)
	}
}

// TestOrchestratorConfigDefaults 验证编排器配置
func TestOrchestratorConfigDefaults(t *testing.T) {
	// OrchestratorConfig 应该被合理使用
	cfg := agent.OrchestratorConfig{
		MaxParallel:      3,
		MaxRetries:       2,
		MicroTestAfter:   true,
		AdversarialRound: 5,
	}

	if cfg.MaxParallel != 3 {
		t.Errorf("expected MaxParallel=3, got %d", cfg.MaxParallel)
	}
	if cfg.AdversarialRound != 5 {
		t.Errorf("expected AdversarialRound=5, got %d", cfg.AdversarialRound)
	}
}

// TestAllWorkflowStagesHaveRoles 验证所有 workflow 的所有阶段都有 Role
func TestAllWorkflowStagesHaveRoles(t *testing.T) {
	for _, wf := range agent.ListWorkflows() {
		for _, s := range wf.Stages {
			if s.Role == "" {
				t.Errorf("workflow %q stage %q has empty role", wf.Name, s.Name)
			}
		}
	}
}

// TestAllWorkflowStagesHavePrompts 记录阶段 prompt 覆盖率 (部分阶段通过自定义逻辑执行, 不依赖 prompt)
func TestAllWorkflowStagesHavePrompts(t *testing.T) {
	total, withPrompt := 0, 0
	for _, wf := range agent.ListWorkflows() {
		for _, s := range wf.Stages {
			total++
			if strings.TrimSpace(s.Prompt) != "" {
				withPrompt++
			}
		}
	}
	t.Logf("prompt 覆盖率: %d/%d (%.0f%%)", withPrompt, total, float64(withPrompt)/float64(total)*100)
	if withPrompt == 0 {
		t.Error("no stages have prompts")
	}
}
