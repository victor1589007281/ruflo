package agent

// team_metrics_test.go —— 钉住 design/03 §1.3 的跨源对账口径：
// 团队级指标的 `run_id` 必须是**真 RunID**、团队名走独立的 `team` 字段、
// 两者都**不进 Prometheus 标签**。
//
// 为什么这三条都要断言而不只断言"有 run_id":
//   - 只断 run_id 非空 → 传 team.Name 进去也能过（那正是改造前的缺陷形态）;
//   - 不断 team → 一次"顺手把 team.Name 换成 RunID"的修改会把团队身份**弄丢**,
//     而指标看起来完全正常;
//   - 不断 labels → 把团队名加进 labels 是最直觉的修法, 而它会改变 Prometheus
//     序列身份（旧序列停更、counter 历史断开），这种代价不会在任何测试里自己现形。

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/metrics"
)

func readTeamEvents(t *testing.T, stateDir string) []metrics.MetricEvent {
	t.Helper()
	f, err := os.Open(filepath.Join(stateDir, "metrics", "team.jsonl"))
	if err != nil {
		t.Fatalf("打开 team.jsonl 失败: %v", err)
	}
	defer f.Close()
	var out []metrics.MetricEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e metrics.MetricEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("解析 team.jsonl 行失败: %v (%s)", err, sc.Text())
		}
		out = append(out, e)
	}
	return out
}

// TestTeam指标_runID是真RunID而非团队名 —— design/03 §1.3 的核心口径。
func TestTeam指标_runID是真RunID而非团队名(t *testing.T) {
	dir := t.TempDir()
	c := metrics.NewCollector(dir)
	team := &ProductionTeam{Name: "e2e-team", LastRunID: "run-e2e-team-abc123"}

	recordTeamRun(c, "team", "team_build_pass_rate", 1, team, map[string]string{"round": "2"})

	evts := readTeamEvents(t, dir)
	if len(evts) != 1 {
		t.Fatalf("应有 1 条事件, 实际 %d", len(evts))
	}
	e := evts[0]
	if e.RunID != "run-e2e-team-abc123" {
		t.Fatalf("run_id 应是真 RunID, got %q —— 传团队名进 run_id 正是本轮修的缺陷", e.RunID)
	}
	if e.RunID == team.Name {
		t.Fatalf("run_id 不该等于团队名")
	}
	if e.Team != "e2e-team" {
		t.Fatalf("team 字段应携带团队名, got %q —— 少了它, 换用 RunID 就把团队身份弄丢了", e.Team)
	}
	// labels 必须**一字未增**: 加标签 = 改 Prometheus 序列身份。
	if len(e.Labels) != 1 || e.Labels["round"] != "2" {
		t.Fatalf("labels 只应有调用方给的 round, got %v", e.Labels)
	}
	for _, k := range []string{"team", "run_id", "run", "team_name"} {
		if _, ok := e.Labels[k]; ok {
			t.Fatalf("%q 绝不能进 labels（会改 Prometheus 序列身份）, labels=%v", k, e.Labels)
		}
	}
}

// TestTeam指标_无RunID时留空而不是回填团队名 —— 空是真话, 回填是把缺陷改个位置。
func TestTeam指标_无RunID时留空而不是回填团队名(t *testing.T) {
	dir := t.TempDir()
	c := metrics.NewCollector(dir)
	team := &ProductionTeam{Name: "not-yet-run"} // 还没跑过 ⇒ LastRunID 为空

	recordTeamRun(c, "team", "team_stage_total", 1, team, nil)

	evts := readTeamEvents(t, dir)
	if len(evts) != 1 {
		t.Fatalf("应有 1 条事件, 实际 %d", len(evts))
	}
	if evts[0].RunID != "" {
		t.Fatalf("没有 LastRunID 时 run_id 应为空, got %q", evts[0].RunID)
	}
	if evts[0].Team != "not-yet-run" {
		t.Fatalf("team 字段仍应有团队名, got %q", evts[0].Team)
	}
	// omitempty 生效: 空 run_id 不该出现在 JSON 文本里。
	raw, err := os.ReadFile(filepath.Join(dir, "metrics", "team.jsonl"))
	if err != nil {
		t.Fatalf("读 team.jsonl: %v", err)
	}
	if bytesContains(raw, `"run_id"`) {
		t.Fatalf("run_id 为空时不该出现该字段, 实际: %s", raw)
	}
}

// TestTeam指标_nil采集器不panic —— 守卫必须对**带类型的 nil** 也成立。
//
// 这条不是防御性冗余: 第一版把参数写成接口, 于是 `(*metrics.Collector)(nil)` 装进接口后
// `m == nil` 为 false, 守卫在最需要它的场景下恰好失效 —— 指标路径 panic 会弄死交付。
func TestTeam指标_nil采集器不panic(t *testing.T) {
	var nilCollector *metrics.Collector
	recordTeamRun(nilCollector, "team", "x", 1, &ProductionTeam{Name: "t"}, nil)
	recordTeamRun(nil, "team", "x", 1, &ProductionTeam{Name: "t"}, nil)

	dir := t.TempDir()
	recordTeamRun(metrics.NewCollector(dir), "team", "x", 1, nil, nil) // team 为 nil
	if _, err := os.Stat(filepath.Join(dir, "metrics", "team.jsonl")); err == nil {
		t.Fatal("team 为 nil 时不该写出任何事件")
	}
}

// TestTeam指标_并发读LastRunID无竞争 —— 指标可能从心跳/看门狗 goroutine 发出,
// 而 LastRunID 由 executeWorkflow 在 team.mu 下写入。跑 -race 才有意义。
func TestTeam指标_并发读LastRunID无竞争(t *testing.T) {
	dir := t.TempDir()
	c := metrics.NewCollector(dir)
	team := &ProductionTeam{Name: "racy", LastRunID: "run-1"}

	done := make(chan struct{}, 2)
	go func() { // 模拟 executeWorkflow 覆写本轮 RunID
		for i := 0; i < 200; i++ {
			team.mu.Lock()
			team.LastRunID = "run-2"
			team.mu.Unlock()
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 200; i++ {
			recordTeamRun(c, "team", "team_stage_total", 1, team, nil)
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func bytesContains(hay []byte, needle string) bool {
	n := []byte(needle)
	for i := 0; i+len(n) <= len(hay); i++ {
		ok := true
		for j := range n {
			if hay[i+j] != n[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
