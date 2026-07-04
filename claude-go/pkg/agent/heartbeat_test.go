package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHeartbeatSurfacesLiveProgress 验证 per-agent 心跳链路:
// Coordinator 的实时进展(ProgressState)经心跳周期回填进 team 与运行中 agent,
// 并落盘到 team.json —— 使既有展示入口(team status / dashboard, 均读该文件)
// 能看到团队内部"此刻在做什么", 解决"看不清团队内部运行情况"。全程无需 LLM。
func TestHeartbeatSurfacesLiveProgress(t *testing.T) {
	dir := t.TempDir()
	team := &ProductionTeam{
		Name:    "hb-test",
		Status:  TeamStatusRunning,
		dataDir: dir,
		Agents: map[string]*BGAgent{
			"coder": {Name: "coder", Role: "coder", Status: AgentStatusRunning},
			"idle":  {Name: "idle", Role: "reviewer", Status: AgentStatusIdle},
		},
	}

	c := &Coordinator{}
	c.ReportProgress("编译", 3, 4096, "task-x")

	// 模拟一次心跳周期 (heartbeatLoop → checkTeamHealth → updateHeartbeat)
	c.checkTeamHealth(team)

	// 1) team 级实时进展已回填
	if team.Progress == nil {
		t.Fatal("team.Progress 未回填")
	}
	if team.Progress.Phase != "编译" || team.Progress.Iteration != 3 || team.Progress.BytesWritten != 4096 {
		t.Fatalf("team.Progress 内容不符: %+v", team.Progress)
	}

	// 2) 运行中 agent 拿到心跳; idle agent 不应被回填
	if team.Agents["coder"].Phase != "编译" || team.Agents["coder"].LastBeat.IsZero() {
		t.Fatalf("running agent 心跳未回填: %+v", team.Agents["coder"])
	}
	if team.Agents["idle"].Phase != "" || !team.Agents["idle"].LastBeat.IsZero() {
		t.Fatalf("idle agent 不应被回填: %+v", team.Agents["idle"])
	}

	// 3) 已落盘到 team.json —— 既有展示入口读该文件即可见实时状态
	data, err := os.ReadFile(filepath.Join(dir, "team.json"))
	if err != nil {
		t.Fatalf("team.json 未落盘: %v", err)
	}
	for _, want := range []string{`"progress"`, `"编译"`, `"lastBeat"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("team.json 缺少 %q; got:\n%s", want, string(data))
		}
	}
}
