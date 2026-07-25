package skillaudit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkSkill 写一份 shadow 技能。
func mkSkill(t *testing.T, root, name, extraFM string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fm := "name: " + name + "\ndescription: 测试技能\nstatus: shadow\ncreated_at: " +
		time.Now().Add(-time.Hour).Format(time.RFC3339) + "\n" + extraFM
	p := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(p, []byte("---\n"+fm+"---\n\n正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// mkRewards 写足量高分奖励, 让奖励判据必然达标 (于是唯一的变量是不越权闸)。
func mkRewards(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < 5; i++ {
		row := map[string]any{"ts": time.Now().UnixMilli(), "value": 1.0, "team": "t"}
		data, _ := json.Marshal(row)
		b.Write(data)
		b.WriteByte('\n')
	}
	p := filepath.Join(dir, "rewards.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 奖励达标但声明了 Bash 的自动技能: 必须落 Rejected 而不是 Promoted。
func TestAudit_奖励达标但越权则拒绝晋升(t *testing.T) {
	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")
	p := mkSkill(t, skillsDir, "auto-bad", "auto_generated: true\nallowed-tools: Read, Bash\n")
	rewards := mkRewards(t, root)

	res, err := Audit(skillsDir, rewards, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Promoted) != 0 {
		t.Fatalf("越权技能不该被晋升, got promoted=%v", res.Promoted)
	}
	if len(res.Rejected) != 1 || res.Rejected[0] != "auto-bad" {
		t.Fatalf("应落 Rejected 档, got %+v", res)
	}
	if len(res.RejectReasons["auto-bad"]) == 0 {
		t.Error("拒绝必须带理由 (人要知道该改什么)")
	}
	// apply=true 也不许改文件 —— 拒绝路径不留半成品。
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "status: shadow") {
		t.Errorf("被拒后 status 必须仍是 shadow: %s", data)
	}
}

// 奖励达标且只读声明: 正常晋升 (证明闸不是一刀切拦死)。
func TestAudit_只读自动技能正常晋升(t *testing.T) {
	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")
	p := mkSkill(t, skillsDir, "auto-ok", "auto_generated: true\nallowed-tools: Read, Grep\n")
	rewards := mkRewards(t, root)

	res, err := Audit(skillsDir, rewards, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Promoted) != 1 || len(res.Rejected) != 0 {
		t.Fatalf("只读技能应晋升: %+v", res)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "status: active") {
		t.Errorf("晋升后 status 应为 active: %s", data)
	}
}

// 手动通道 (evo promote / evo_promote 工具都走它) 同样过闸。
func TestSetStatus_晋升方向过不越权闸(t *testing.T) {
	root := t.TempDir()
	p := mkSkill(t, filepath.Join(root, "skills"), "auto-bad", "auto_generated: true\nallowed-tools: Bash\n")

	if err := SetStatus(p, "active"); err == nil {
		t.Fatal("手动晋升越权技能必须被拒 —— 否则机械强制只是给自动通道加的装饰")
	}
	// 收紧方向不检查: 把权限面变小永远不是提权。
	if err := SetStatus(p, "archived"); err != nil {
		t.Errorf("退役方向不该被闸拦: %v", err)
	}
	if err := SetStatus(p, "shadow"); err != nil {
		t.Errorf("回退 shadow 不该被闸拦: %v", err)
	}
	if err := SetStatus(p, "什么状态"); err == nil {
		t.Error("非法状态应报错")
	}
}

// 解析不了的 SKILL.md 不得因"当作没声明"而被放行。
func TestSetStatus_解析失败拒绝晋升(t *testing.T) {
	if err := SetStatus(filepath.Join(t.TempDir(), "无此文件", "SKILL.md"), "active"); err == nil {
		t.Fatal("读不到文件必须拒绝晋升 (fail-closed)")
	}
}
