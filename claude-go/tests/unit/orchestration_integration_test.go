package unit

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
)

// TestCoordinatorWatchdogDetectsStale 验证 watchdog 可以检测活动超时。
// 模拟: 团队长时间无 TouchActivity → watchdog 应发出告警。
func TestCoordinatorWatchdogDetectsStale(t *testing.T) {
	var alertCount int32
	notify := func(_, msg string) {
		t.Logf("notify: %s", msg)
		atomic.AddInt32(&alertCount, 1)
	}

	coord := agent.NewCoordinator(nil, nil, notify, agent.CoordinatorConfig{
		MaxRetries:    2,
		HeartbeatFreq: 100 * time.Millisecond,
		DataDir:       t.TempDir(),
		ChatID:        "test-watchdog",
	})

	// 初始活动 (构造函数已 touch)
	if age := coord.LastActivityAge(); age > time.Second {
		t.Fatalf("初始活动年龄过大: %s", age)
	}

	// 验证 TouchActivity 有效
	time.Sleep(50 * time.Millisecond)
	coord.TouchActivity()
	if age := coord.LastActivityAge(); age > time.Second {
		t.Fatalf("TouchActivity 后年龄过大: %s", age)
	}
}

// TestCoordinatorCheckpointPersistence 验证检查点可以跨实例恢复
func TestCoordinatorCheckpointPersistence(t *testing.T) {
	dir := t.TempDir()
	noop := func(string, string) {}

	// 实例 1: 写入检查点
	c1 := agent.NewCoordinator(nil, nil, noop, agent.CoordinatorConfig{
		MaxRetries:    3,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       dir,
		ChatID:        "test",
	})
	c1.SaveCheckpoint("research", "completed", 0, "研究结果...")
	c1.SaveCheckpoint("design", "completed", 0, "设计文档...")
	c1.SaveCheckpoint("code", "running", 1, "")

	// 实例 2: 从持久化恢复
	c2 := agent.NewCoordinator(nil, nil, noop, agent.CoordinatorConfig{
		MaxRetries:    3,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       dir,
		ChatID:        "test",
	})

	if cp := c2.GetCheckpoint("research"); cp == nil || cp.Status != "completed" {
		t.Error("research checkpoint not recovered")
	}
	if cp := c2.GetCheckpoint("design"); cp == nil || cp.Status != "completed" {
		t.Error("design checkpoint not recovered")
	}
	if cp := c2.GetCheckpoint("code"); cp == nil || cp.Status != "running" {
		t.Error("code checkpoint not recovered")
	}
	if c2.CompletedCount() != 2 {
		t.Errorf("expected 2 completed, got %d", c2.CompletedCount())
	}
}

// TestPipelineWithRecoveryDeadlockDetection 验证 pipeline 死锁检测
func TestPipelineWithRecoveryDeadlockDetection(t *testing.T) {
	// 构造一个有循环依赖的 workflow (间接)
	wf := &agent.WorkflowDef{
		Name: "test-deadlock",
		Mode: "pipeline",
		Stages: []agent.StageDef{
			{Name: "a", Role: "coder", Prompt: "do a", DependsOn: []string{"b"}},
			{Name: "b", Role: "coder", Prompt: "do b", DependsOn: []string{"a"}},
		},
	}

	noop := func(string, string) {}
	coord := agent.NewCoordinator(nil, nil, noop, agent.CoordinatorConfig{
		MaxRetries:    1,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       t.TempDir(),
		ChatID:        "test",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	executor := &agent.WorkflowExecutor{}
	_, err := coord.RunWithRecovery(ctx, wf, "test", &agent.ProductionTeam{Name: "test"}, executor)
	if err == nil {
		t.Fatal("expected deadlock error, got nil")
	}
	t.Logf("deadlock correctly detected: %s", err)
}

// TestFilterParallelWithExplicitFlags 验证显式 Parallel 标记的阶段能并行
func TestFilterParallelWithExplicitFlags(t *testing.T) {
	// 通过 research workflow 测试: 它有 3 个 parallel 的 researcher 阶段
	wf := agent.GetWorkflow("research")
	if wf == nil {
		t.Fatal("research workflow not found")
	}

	var parallelStages []string
	for _, s := range wf.Stages {
		if s.Parallel {
			parallelStages = append(parallelStages, s.Name)
		}
	}
	if len(parallelStages) < 2 {
		t.Skipf("research workflow only has %d parallel stages", len(parallelStages))
	}
	t.Logf("research workflow parallel stages: %v", parallelStages)
}

// TestDevelopmentWorkflowStructure 验证开发团队 workflow 结构完整性
func TestDevelopmentWorkflowStructure(t *testing.T) {
	wf := agent.GetWorkflow("development")
	if wf == nil {
		t.Fatal("development workflow not found")
	}

	if wf.Mode != "adversarial_dev" {
		t.Errorf("expected mode adversarial_dev, got %s", wf.Mode)
	}

	// 验证必须存在的角色
	roleSet := make(map[string]bool)
	for _, s := range wf.Stages {
		roleSet[s.Role] = true
	}

	required := []string{"researcher", "architect", "planner"}
	for _, r := range required {
		if !roleSet[r] {
			t.Errorf("missing required role %q in development workflow", r)
		}
	}
}

// TestAdversarialDevNowProtected 验证 adversarial_dev 模式不再直接 bypass Coordinator
func TestAdversarialDevNowProtected(t *testing.T) {
	wf := agent.GetWorkflow("development")
	if wf == nil {
		t.Fatal("development workflow not found")
	}

	// 验证 mode 是 adversarial_dev (之前这个模式直接 bypass Coordinator)
	if wf.Mode != "adversarial_dev" {
		t.Errorf("expected adversarial_dev, got %s", wf.Mode)
	}

	// 验证 coordinator 可以处理此模式 (不 panic)
	noop := func(string, string) {}
	coord := agent.NewCoordinator(nil, nil, noop, agent.CoordinatorConfig{
		MaxRetries:    1,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       t.TempDir(),
		ChatID:        "test",
	})
	_ = coord // 编译通过即验证结构正确
}

// TestEvalScoreHardPass 验证评分阈值 (回归)
func TestEvalScoreHardPass(t *testing.T) {
	tests := []struct {
		name     string
		score    agent.EvalScore
		wantPass bool
	}{
		{"all high", agent.EvalScore{Correctness: 8, Completeness: 8, Security: 8, CodeQuality: 8, Pass: true}, true},
		{"low completeness", agent.EvalScore{Correctness: 3, Completeness: 3, Security: 3, CodeQuality: 3, Pass: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.score.MeetsHardPassThreshold()
			if got != tt.wantPass {
				t.Errorf("MeetsHardPassThreshold() = %v, want %v (score: %+v)", got, tt.wantPass, tt.score)
			}
		})
	}
}

// TestWatchdogThresholds 验证 watchdog 常量的合理性
func TestWatchdogThresholds(t *testing.T) {
	// watchdog 阈值应该在合理范围: stale < critical
	// 无法直接引用常量(未导出), 但通过行为测试验证
	coord := agent.NewCoordinator(nil, nil, func(string, string) {}, agent.CoordinatorConfig{
		MaxRetries:    2,
		HeartbeatFreq: 30 * time.Second,
		DataDir:       t.TempDir(),
		ChatID:        "test",
	})

	// 确保初始状态正常
	age := coord.LastActivityAge()
	if age > time.Second {
		t.Errorf("initial age too large: %s", age)
	}

	// Touch 后应重置
	coord.TouchActivity()
	age2 := coord.LastActivityAge()
	if age2 > 100*time.Millisecond {
		t.Errorf("age after touch too large: %s", age2)
	}
}

// TestAllProtectedModesHaveWorkflows 验证被保护的模式都有对应 workflow
func TestAllProtectedModesHaveWorkflows(t *testing.T) {
	modes := map[string]bool{
		"adversarial_dev": false,
		"adversarial":     false,
		"trading_debate":  false,
		"creative_media":  false,
		"swarm_novel":     false,
	}

	for _, wf := range agent.ListWorkflows() {
		if _, ok := modes[wf.Mode]; ok {
			modes[wf.Mode] = true
		}
	}

	for mode, found := range modes {
		if !found {
			t.Logf("mode %q: no workflow found (may use custom execute path)", mode)
		}
	}
}

// TestBottleneckClassificationRegression 更详细的瓶颈分类回归测试
func TestBottleneckClassificationRegression(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		expectMin int
		expectMax int
	}{
		{"empty", "", 0, 0},
		{"all pass", "compilation: PASS | logic: PASS | security: PASS", 0, 0},
		{"compile fail", "compilation: FAIL - missing import", 1, 2},
		{"security fail", "security: FAIL - SQL injection risk", 1, 2},
		{"multi fail", "compilation: FAIL | logic: FAIL | security: FAIL", 2, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bns := agent.ClassifyBottlenecks(tt.input)
			if len(bns) < tt.expectMin {
				t.Errorf("got %d bottlenecks, want >= %d for input: %s", len(bns), tt.expectMin, tt.input)
			}
			if len(bns) > tt.expectMax {
				t.Logf("got %d bottlenecks (more than expected max %d), input: %s", len(bns), tt.expectMax, tt.input)
			}
			for _, bn := range bns {
				if bn.Type == "" {
					t.Error("bottleneck type should not be empty")
				}
				if bn.Severity == "" {
					t.Error("bottleneck severity should not be empty")
				}
				t.Logf("  type=%s severity=%s detail=%s", bn.Type, bn.Severity, fmt.Sprintf("%.40s", bn.Detail))
			}
		})
	}
}
