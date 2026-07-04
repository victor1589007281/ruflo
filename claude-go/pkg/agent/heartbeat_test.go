package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestHeartbeatDoesNotClobberAgentPhase 守护 pipeline 模式(默认路径)的真实性:
// 该模式无 ReportProgress → CurrentProgress() 为空; 此时心跳周期必须只刷新
// LastBeat(证明存活), 而不能用空 Phase 覆盖 runAgent 已置的"执行中"。
// (防止 per-agent 心跳在最常见路径上变成空壳。)
func TestHeartbeatDoesNotClobberAgentPhase(t *testing.T) {
	dir := t.TempDir()
	team := &ProductionTeam{
		Name:    "hb2",
		Status:  TeamStatusRunning,
		dataDir: dir,
		Agents: map[string]*BGAgent{
			// 模拟 runAgent 在阶段起始已置的状态
			"coder": {Name: "coder", Role: "coder", Status: AgentStatusRunning, Phase: "执行中"},
		},
	}
	c := &Coordinator{} // 未 ReportProgress → 空进展 (pipeline 模式)
	c.checkTeamHealth(team)

	if got := team.Agents["coder"].Phase; got != "执行中" {
		t.Fatalf("空进展不应覆盖 runAgent 已置的阶段, got %q", got)
	}
	if team.Agents["coder"].LastBeat.IsZero() {
		t.Fatal("LastBeat 应被心跳刷新以证明存活")
	}
}

// TestHeartbeatConcurrentPersistNoRace 用 -race 验证 2b 修复:
// persist() 的 marshal 现在 t.mu 下做一致性快照, 与并发写者(模拟 runAgent 的
// 加锁改动)不再产生撕裂快照/数据竞争; 且 persist 内部自锁、updateHeartbeat
// 调用前已解锁, 不会重入死锁。
func TestHeartbeatConcurrentPersistNoRace(t *testing.T) {
	dir := t.TempDir()
	team := &ProductionTeam{
		Name:    "hbrace",
		Status:  TeamStatusRunning,
		dataDir: dir,
		Agents: map[string]*BGAgent{
			"coder": {Name: "coder", Role: "coder", Status: AgentStatusRunning, Phase: "执行中"},
		},
	}
	c := &Coordinator{}
	c.ReportProgress("编译", 1, 10, "t")

	var wg sync.WaitGroup
	wg.Add(2)
	// 写者: 模拟 runAgent 在 t.mu 下改 agent/team 字段 + 落盘
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			team.mu.Lock()
			team.Agents["coder"].Result = "partial"
			team.Status = TeamStatusRunning
			team.mu.Unlock()
			team.persist()
		}
	}()
	// 心跳: 并发 updateHeartbeat(含 persist 快照)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			team.updateHeartbeat(c.CurrentProgress())
		}
	}()
	wg.Wait()
}
