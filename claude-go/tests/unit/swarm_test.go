package unit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
)

func TestSwarmRoleRouter(t *testing.T) {
	tests := []struct {
		name      string
		objective string
		wantCode  bool
		wantRes   bool
		minRoles  int
		maxRoles  int
	}{
		{
			name:      "simple_research",
			objective: "调研 Redis 和 Memcached 的区别",
			wantRes:   true,
			minRoles:  2,
			maxRoles:  4,
		},
		{
			name:      "coding_task",
			objective: "实现一个 Go 语言的 HTTP 服务器",
			wantCode:  true,
			minRoles:  2,
			maxRoles:  5,
		},
		{
			name:      "complex_task",
			objective: "设计并实现一个分布式消息队列系统，包括调研现有方案、架构设计、Go代码实现、测试和安全审查",
			wantCode:  true,
			wantRes:   true,
			minRoles:  3,
			maxRoles:  7,
		},
		{
			name:      "short_task",
			objective: "分析日志",
			wantRes:   false,
			minRoles:  2,
			maxRoles:  4,
		},
	}

	orch := agent.NewSwarmOrchestrator(nil, nil, nil, func(_, _ string) {}, "", 8)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := orch.RouteRoles(tt.objective)

			if len(profile.Roles) < tt.minRoles || len(profile.Roles) > tt.maxRoles {
				t.Errorf("角色数=%d, 期望 [%d, %d], roles=%v",
					len(profile.Roles), tt.minRoles, tt.maxRoles, profile.Roles)
			}
			if profile.SuggestedMax <= 0 || profile.SuggestedMax > 8 {
				t.Errorf("SuggestedMax=%d 超出合理范围", profile.SuggestedMax)
			}
			if profile.Complexity < 1 || profile.Complexity > 3 {
				t.Errorf("Complexity=%d 超出范围", profile.Complexity)
			}

			if tt.wantCode && !profile.NeedsCoding {
				t.Error("预期 NeedsCoding=true")
			}
			if tt.wantRes && !profile.NeedsResearch {
				t.Error("预期 NeedsResearch=true")
			}
		})
	}
}

func TestSwarmQualityGate(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		role     string
		priority int
		wantDeg  bool
		minConf  float64
	}{
		{"empty_output", "", "coder", 0, true, 0},
		{"short_output", "hello", "coder", 0, false, 0.3},
		{"good_coder_output", "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"Hello World\")\n}\n" + makeString(300), "coder", 0, false, 0.5},
		{"structured_report", "## 报告标题\n\n### 1. 分析\n这是一个详细的分析报告...\n\n| 指标 | 值 |\n|---|---|\n| A | 1 |\n" + makeString(400), "researcher", 0, false, 0.6},
	}

	orch := agent.NewSwarmOrchestrator(nil, nil, nil, func(_, _ string) {}, "", 8)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr := agent.StageResult{
				Name:   "test-task",
				Role:   tt.role,
				Status: agent.TaskCompleted,
				Output: tt.output,
			}
			if tt.output == "" {
				sr.Status = agent.TaskFailed
			}
			task := agent.SubTask{ID: "test-task", Role: tt.role, Priority: tt.priority}

			sc := orch.QualityGate(sr, task)

			if sc.Degraded != tt.wantDeg {
				t.Errorf("Degraded=%v, 期望 %v (confidence=%.2f)", sc.Degraded, tt.wantDeg, sc.Confidence)
			}
			if sc.Confidence < tt.minConf {
				t.Errorf("Confidence=%.2f < minConf=%.2f", sc.Confidence, tt.minConf)
			}
		})
	}
}

func TestSwarmCheckpoint(t *testing.T) {
	dir := t.TempDir()
	orch := agent.NewSwarmOrchestrator(nil, nil, nil, func(_, _ string) {}, "", 8)
	orch.SetStateDir(dir)

	plan := &agent.DecompositionPlan{
		SubTasks: []agent.SubTask{
			{ID: "t1", Description: "task1", Role: "researcher"},
			{ID: "t2", Description: "task2", Role: "coder", DependsOn: []string{"t1"}},
		},
		Strategy: "hybrid",
	}
	resultMap := map[string]string{"t1": "result of t1"}

	orch.SaveCheckpoint("test-team", plan, resultMap, 1)

	cpPath := filepath.Join(dir, "swarm_test-team.json")
	if _, err := os.Stat(cpPath); os.IsNotExist(err) {
		t.Fatal("检查点文件未创建")
	}

	data, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatalf("读取检查点失败: %v", err)
	}

	var cp agent.SwarmCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatalf("解析检查点失败: %v", err)
	}

	if cp.CurrentLevel != 1 {
		t.Errorf("CurrentLevel=%d, 期望 1", cp.CurrentLevel)
	}
	if len(cp.Plan.SubTasks) != 2 {
		t.Errorf("Plan.SubTasks 数=%d, 期望 2", len(cp.Plan.SubTasks))
	}
	if cp.ResultMap["t1"] != "result of t1" {
		t.Errorf("ResultMap[t1]=%q, 期望 'result of t1'", cp.ResultMap["t1"])
	}

	orch.ClearCheckpoint("test-team")
	if _, err := os.Stat(cpPath); !os.IsNotExist(err) {
		t.Error("检查点文件未清理")
	}
}

func TestDebateDivergence(t *testing.T) {
	tests := []struct {
		name      string
		proposer  string
		opponent  string
		minDiv    float64
		maxDiv    float64
	}{
		{"agreement", "I agree that this is correct. Indeed, the data confirms it.", "Yes, I also agree. This is correct and well-founded.", 0.0, 0.3},
		{"disagreement", "This approach is superior however the opponent disagrees. 我不同意他的反对意见.", "I disagree completely. This is 错误 and 误导性. 我反对这个方案.", 0.4, 1.0},
		{"empty", "", "", 0.45, 0.55},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			div := agent.EstimateDebateDivergence(tt.proposer, tt.opponent)
			if div < tt.minDiv || div > tt.maxDiv {
				t.Errorf("divergence=%.2f, 期望在 [%.2f, %.2f] 范围内", div, tt.minDiv, tt.maxDiv)
			}
		})
	}
}

func TestComputeConfidence(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		role    string
		minConf float64
		maxConf float64
	}{
		{"empty", "", "coder", 0, 0},
		{"short", "hello", "coder", 0.3, 0.5},
		{"good_code", "package main\n\nimport \"fmt\"\n\nfunc main() {\n\t" + makeString(300) + "\n}", "coder", 0.5, 1.0},
		{"structured", "## Title\n\n```go\nfunc f() {}\n```\n\n| a | b |\n|---|---|\n" + makeString(500), "researcher", 0.7, 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := agent.ComputeConfidence(tt.output, tt.role)
			if conf < tt.minConf || conf > tt.maxConf {
				t.Errorf("confidence=%.2f, 期望在 [%.2f, %.2f]", conf, tt.minConf, tt.maxConf)
			}
		})
	}
}

func makeString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return string(b)
}
