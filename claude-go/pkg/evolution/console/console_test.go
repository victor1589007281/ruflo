package console

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectHealthyLoop(t *testing.T) {
	dir := t.TempDir()
	// 构造健康闭环制品: trace span + reward + experience
	mkdir(t, filepath.Join(dir, "statestore", "log"))
	writeFile(t, filepath.Join(dir, "statestore", "log", "trace-run1.jsonl"),
		`{"trace_id":"run1","kind":"turn"}`+"\n"+`{"trace_id":"run1","kind":"tool_call"}`+"\n")
	mkdir(t, filepath.Join(dir, "statestore", "blob", "ab"))
	writeFile(t, filepath.Join(dir, "statestore", "blob", "ab", "abcdef"), "blobdata")
	mkdir(t, filepath.Join(dir, "evolution"))
	writeFile(t, filepath.Join(dir, "evolution", "rewards.jsonl"),
		`{"source":"episode","value":1}`+"\n"+`{"source":"gate.content","value":0.6}`+"\n")
	writeFile(t, filepath.Join(dir, "evolution", "experiences.json"), `[{"id":"e1"},{"id":"e2"}]`)
	writeFile(t, filepath.Join(dir, "evolution", "trajectories.json"), `[{"id":"t1"}]`)

	r, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.LoopHealth != "healthy" {
		t.Errorf("三环齐备应 healthy, got %s", r.LoopHealth)
	}
	if r.TraceSpans != 2 || r.TraceBlobs != 1 {
		t.Errorf("轨迹统计错误: spans=%d blobs=%d", r.TraceSpans, r.TraceBlobs)
	}
	if r.Rewards != 2 || r.RewardBySource["episode"] != 1 || r.RewardBySource["gate.content"] != 1 {
		t.Errorf("奖励统计错误: %+v", r.RewardBySource)
	}
	if r.RewardMean != 0.8 {
		t.Errorf("奖励均值应 0.8, got %f", r.RewardMean)
	}
	if r.Experiences != 2 || r.Trajectories != 1 {
		t.Errorf("经验/轨迹统计错误: exp=%d traj=%d", r.Experiences, r.Trajectories)
	}
	if r.Format() == "" {
		t.Error("Format 不应为空")
	}
}

func TestInspectOpenLoop(t *testing.T) {
	dir := t.TempDir() // 空目录: 无任何制品
	r, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.LoopHealth != "open" {
		t.Errorf("空目录应 open, got %s", r.LoopHealth)
	}
}

func TestInspectShadowSkills(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "skills", "auto-skill"))
	writeFile(t, filepath.Join(dir, "skills", "auto-skill", "SKILL.md"),
		"---\nname: auto-skill\nstatus: shadow\n---\n内容")
	mkdir(t, filepath.Join(dir, "skills", "stable-skill"))
	writeFile(t, filepath.Join(dir, "skills", "stable-skill", "SKILL.md"),
		"---\nname: stable-skill\n---\n内容")
	r, _ := Inspect(dir)
	if r.ShadowSkills != 1 {
		t.Errorf("应识别 1 个 shadow 技能, got %d", r.ShadowSkills)
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
