package skillaudit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, name, status string) string {
	t.Helper()
	sd := filepath.Join(dir, name)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sd, "SKILL.md")
	content := "---\nname: " + name + "\nstatus: " + status + "\ncreated_at: 2020-01-01T00:00:00Z\n---\n技能正文"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeRewards(t *testing.T, path string, values ...float64) {
	t.Helper()
	var b strings.Builder
	for _, v := range values {
		fmt.Fprintf(&b, "{\"ts\":1700000000000,\"value\":%g}\n", v)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAuditPromote(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "good-skill", "shadow")
	rewards := filepath.Join(dir, "rewards.jsonl")
	writeRewards(t, rewards, 1.0, 1.0, 1.0, 0.5) // 均值 0.875 ≥ 0.3

	res, err := Audit(dir, rewards, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Promoted) != 1 || res.Promoted[0] != "good-skill" {
		t.Fatalf("应晋升 good-skill: %+v", res)
	}
	// 验证 SKILL.md 被改为 active + audit 留痕
	data, _ := os.ReadFile(filepath.Join(dir, "good-skill", "SKILL.md"))
	if !strings.Contains(string(data), "status: active") {
		t.Fatal("SKILL.md 应改为 active")
	}
	if !strings.Contains(string(data), "audit:") {
		t.Fatal("应写入 audit 谱系")
	}
}

func TestAuditRetire(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "bad-skill", "shadow")
	rewards := filepath.Join(dir, "rewards.jsonl")
	writeRewards(t, rewards, -1.0, -1.0, -1.0) // 均值 -1 ≤ -0.2

	res, _ := Audit(dir, rewards, true)
	if len(res.Retired) != 1 {
		t.Fatalf("应退役 bad-skill: %+v", res)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "bad-skill", "SKILL.md"))
	if !strings.Contains(string(data), "status: archived") {
		t.Fatal("应改为 archived")
	}
}

func TestAuditHoldInsufficientSamples(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "new-skill", "shadow")
	rewards := filepath.Join(dir, "rewards.jsonl")
	writeRewards(t, rewards, 1.0) // 仅 1 样本 < minSamples(3)

	res, _ := Audit(dir, rewards, true)
	if len(res.Held) != 1 {
		t.Fatalf("样本不足应 hold: %+v", res)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "new-skill", "SKILL.md"))
	if !strings.Contains(string(data), "status: shadow") {
		t.Fatal("hold 不应改变 status")
	}
}

func TestAuditSkipsNonShadow(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "active-skill", "active")
	rewards := filepath.Join(dir, "rewards.jsonl")
	writeRewards(t, rewards, 1.0, 1.0, 1.0)
	res, _ := Audit(dir, rewards, true)
	if res.Evaluated != 0 {
		t.Fatal("非 shadow 技能不应被审计")
	}
}

func TestSetStatus(t *testing.T) {
	dir := t.TempDir()
	p := writeSkill(t, dir, "s", "shadow")
	if err := SetStatus(p, "active"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "status: active") {
		t.Fatal("SetStatus 应改为 active")
	}
	if err := SetStatus(p, "bogus"); err == nil {
		t.Fatal("非法状态应报错")
	}
}
