// paired_test.go —— 13.8.6 P1 skillaudit 配对归因的语义锁定 (SkillAudit, arXiv:2606.14239)。
//
// M2 验收判据 (docforge planning-evo-track §13.8.6): 配对归因上线后, skillaudit 对
// 同一批 shadow 技能给出**不同**的裁决 —— 配对证据真实参与裁决, 而非装饰字段。
//
// 场景: 时序判据下两个技能的"创建后奖励均值"完全相同 (都达标), 但配对视角下 ——
//   - good-paired: 带 vs 不带的 run 差值 ≥ +0.3 → 晋升
//   - bad-paired:  带 vs 不带的 run 差值 ≤ −0.2 → 退役
//
// 时序均值相同 (同期整体偏好的干扰), 配对差值把它们分开: 这就是论文要的归因。
//
// 夹具: rewards 行带 run_id (RewardEvent json:"run_id" 同 tag); trace 行为
// tracestore.Span 的 json 序列化 (trace_id/kind/ts/attrs.skills), 与
// FileStore Log 落盘格式逐字段一致。
package skillaudit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkillsWithReward 写一个 shadow 技能; created_at 用远期, 让时序判据把全部奖励
// 都算成"创建后" —— 两技能的时序均值刻意完全相同, 裁决差异只能来自配对证据。
func writeSkillsWithReward(t *testing.T, dir, name string) {
	t.Helper()
	p := filepath.Join(dir, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\nstatus: shadow\ncreated_at: 2000-01-01T00:00:00Z\n---\n正文"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRunRewards 写 rewards.jsonl: 每个 run 两条同值奖励 (gate.test 1.0 + episode 0.3)。
func writeRunRewards(t *testing.T, path string, runs map[string]float64) {
	t.Helper()
	var b strings.Builder
	for runID, v := range runs {
		fmt.Fprintf(&b, "{\"ts\":1700000000000,\"value\":%g,\"source\":\"gate.test\",\"weight\":1.0,\"run_id\":%q}\n", v, runID)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// spanLine 构造一条 policy_decision Span 的 JSON (与 tracestore.Span 的落盘一致)。
func spanLine(traceID string, skills []string) string {
	return fmt.Sprintf(`{"trace_id":%q,"span_id":"s","kind":"policy_decision","name":"apply","ts":1700000000000,"attrs":{"stage":"impl","role":"coder","skills":[%s]}}`,
		traceID, `"`+strings.Join(skills, `","`)+`"`)
}

// M2 主场景: 时序均值相同的两个 shadow 技能, 配对差值给出相反裁决。
func TestPaired_同时序均值配对差值分裁决(t *testing.T) {
	dir := t.TempDir()
	writeSkillsWithReward(t, dir, "good-paired")
	writeSkillsWithReward(t, dir, "bad-paired")
	rewards := filepath.Join(dir, "rewards.jsonl")

	// run 奖励: 带 good 的 run 好 (0.9), 带 bad 的 run 差 (-0.8), 其余中等 (0.1)。
	// 两个技能都"创建于"全部奖励之前 → 各自时序均值同为 0.1 上下 (夹在 promote 与
	// retire 阈值之间, 时序判据只会 hold)。
	runs := map[string]float64{
		"run-g1": 0.9, "run-g2": 0.9, "run-g3": 0.9, "run-g4": 0.9, // 带 good
		"run-b1": -0.8, "run-b2": -0.8, "run-b3": -0.8, "run-b4": -0.8, // 带 bad
		"run-n1": 0.1, "run-n2": 0.1, "run-n3": 0.1, "run-n4": 0.1, // 两者都不带
	}
	writeRunRewards(t, rewards, runs)

	traceDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 每个 run 一条 policy_decision Span。run-g* 注入 good, run-b* 注入 bad,
	// run-n* 无注入 —— 这就是 13.7.4 选择留痕提供的 per-skill 证据。
	var lines []string
	for _, r := range []struct {
		id string
		sk []string
	}{{"run-g1", []string{"good-paired"}}, {"run-g2", []string{"good-paired"}},
		{"run-g3", []string{"good-paired"}}, {"run-g4", []string{"good-paired"}},
		{"run-b1", []string{"bad-paired"}}, {"run-b2", []string{"bad-paired"}},
		{"run-b3", []string{"bad-paired"}}, {"run-b4", []string{"bad-paired"}},
		{"run-n1", nil}, {"run-n2", nil}, {"run-n3", nil}, {"run-n4", nil}} {
		lines = append(lines, spanLine(r.id, r.sk))
	}
	if err := os.WriteFile(filepath.Join(traceDir, "trace-runs.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := AuditWithTraces(dir, rewards, traceDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Evaluated != 2 {
		t.Fatalf("应评估两个 shadow 技能: %+v", res)
	}
	// M2 判据: 同一批技能, 配对证据给出**不同**裁决。
	if len(res.Promoted) != 1 || res.Promoted[0] != "good-paired" {
		t.Fatalf("配对差值应晋升 good-paired (带它 run 显著更好): %+v", res)
	}
	if len(res.Retired) != 1 || res.Retired[0] != "bad-paired" {
		t.Fatalf("配对差值应退役 bad-paired (带它 run 显著更差): %+v", res)
	}
}

// 无配对证据 (trace 目录不存在) → 回退 v1 时序判据: 同批技能同均值, 全部 hold。
func TestPaired_无留痕回退时序判据(t *testing.T) {
	dir := t.TempDir()
	writeSkillsWithReward(t, dir, "a-skill")
	writeSkillsWithReward(t, dir, "b-skill")
	rewards := filepath.Join(dir, "rewards.jsonl")
	writeRunRewards(t, rewards, map[string]float64{
		"r1": 0.9, "r2": -0.8, "r3": 0.1, "r4": 0.1, "r5": 0.1, "r6": 0.1,
	})

	res, err := AuditWithTraces(dir, rewards, filepath.Join(dir, "不存在的log"), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Evaluated != 2 || len(res.Held) != 2 {
		t.Fatalf("无留痕应回退时序判据且同均值全 hold: %+v", res)
	}
	if len(res.Promoted) != 0 || len(res.Retired) != 0 {
		t.Fatalf("时序均值夹在阈值之间不应裁决: %+v", res)
	}
}

// 有留痕但配对证据不足 (单侧 run 数 < minSamples) → 回退时序判据。
// 夹具刻意让两条判据分道: 配对差值 1.0 (够晋升), 时序均值 0.17 (hold 区) ——
// 断言 hold 即证明裁决确实没用配对证据。
func TestPaired_配对样本不足回退(t *testing.T) {
	dir := t.TempDir()
	writeSkillsWithReward(t, dir, "lone-skill")
	rewards := filepath.Join(dir, "rewards.jsonl")
	// 4 个带技能 run (0.5) + 只有 2 个不带 (-0.5) → withoutN=2 < minSamples=3。
	// 时序均值 = (0.5*4 − 0.5*2)/6 ≈ 0.17 (hold); 配对差值 = 1.0 (够晋升)。
	writeRunRewards(t, rewards, map[string]float64{
		"w1": 0.5, "w2": 0.5, "w3": 0.5, "w4": 0.5,
		"o1": -0.5, "o2": -0.5,
	})

	traceDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range []struct {
		id string
		sk []string
	}{{"w1", []string{"lone-skill"}}, {"w2", []string{"lone-skill"}},
		{"w3", []string{"lone-skill"}}, {"w4", []string{"lone-skill"}},
		{"o1", nil}, {"o2", nil}} {
		lines = append(lines, spanLine(r.id, r.sk))
	}
	if err := os.WriteFile(filepath.Join(traceDir, "trace-lone.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := AuditWithTraces(dir, rewards, traceDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Held) != 1 {
		t.Fatalf("配对证据不足应回退时序判据并 hold: %+v", res)
	}
	if len(res.Promoted) != 0 {
		t.Fatalf("单侧样本不足不得晋升: %+v", res)
	}
}

// 单测钉住 join 契约: rewards 行 run_id 与 Span trace_id 用同一键, attrs.skills 数组
// 直接来自 policy_decision attrs。两侧字段名漂移时这里先炸。
func TestPaired_join契约钉住(t *testing.T) {
	dir := t.TempDir()
	traceDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	one := spanLine("run-x", []string{"sk-a", "sk-b"})
	if err := os.WriteFile(filepath.Join(traceDir, "trace-x.jsonl"), []byte(one+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	traces := readPolicySkills(traceDir)
	ev, ok := traces["run-x"]
	if !ok {
		t.Fatal("trace_id 应成为 run 键")
	}
	if !ev.Skills["sk-a"] || !ev.Skills["sk-b"] || len(ev.Skills) != 2 {
		t.Fatalf("attrs.skills 应反解为技能集合: %+v", ev.Skills)
	}
	if ev.Stage != "impl" || ev.Role != "coder" {
		t.Fatalf("stage/role 应被读出: %+v", ev)
	}
	if ev.TS != 1700000000000 {
		t.Fatalf("ts (unix-milli) 应被读出: %d", ev.TS)
	}
}
