package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseStatusFrontmatter 守护 status 解析 + 向后兼容:
// 存量 SKILL.md 绝大多数没有 status 行, 必须视为 active(可用), 否则一次上线
// 就会静默禁用全部既有技能。
func TestParseStatusFrontmatter(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		wantStatus string
		wantActive bool
	}{
		{
			name:       "无 status 行 → 视为 active",
			content:    "---\nname: legacy\ndescription: 存量技能\n---\n正文",
			wantStatus: "",
			wantActive: true,
		},
		{
			name:       "无 frontmatter → 视为 active",
			content:    "# 纯 Markdown 技能\n正文",
			wantStatus: "",
			wantActive: true,
		},
		{
			name:       "status: shadow → 停用",
			content:    "---\nname: auto\nstatus: shadow\n---\n正文",
			wantStatus: StatusShadow,
			wantActive: false,
		},
		{
			name:       "status: archived → 停用",
			content:    "---\nname: old\nstatus: archived\n---\n正文",
			wantStatus: StatusArchived,
			wantActive: false,
		},
		{
			name:       "status: active → 可用",
			content:    "---\nname: promoted\nstatus: active\n---\n正文",
			wantStatus: StatusActive,
			wantActive: true,
		},
		{
			name:       "大小写/空格不敏感",
			content:    "---\nname: mixed\nstatus:  Shadow \n---\n正文",
			wantStatus: "Shadow",
			wantActive: false,
		},
		{
			name:       "未知 status 值 fail-open (第三方把 status 当自由文本用)",
			content:    "---\nname: beta-skill\nstatus: beta\n---\n正文",
			wantStatus: "beta",
			wantActive: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sk, err := ParseSkillContent(c.content, filepath.Join("x", "s", "SKILL.md"), "test")
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if strings.TrimSpace(sk.Status) != strings.TrimSpace(c.wantStatus) {
				t.Errorf("Status = %q, want %q", sk.Status, c.wantStatus)
			}
			if got := sk.IsActive(); got != c.wantActive {
				t.Errorf("IsActive() = %v, want %v (status=%q)", got, c.wantActive, sk.Status)
			}
		})
	}
}

// TestRegistryShadowIsolation 守护 shadow 技能"无运行期效力":
// 清单 (FormatListing/FormatShortListing) 与加载路径 (Get) 排除 shadow,
// 但管理视图 (All/GetAny/WithStatus) 仍能看到它 —— 否则进化门禁无法裁决。
func TestRegistryShadowIsolation(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Skill{Name: "legacy-no-status", Description: "存量技能无 status"})
	reg.Register(&Skill{Name: "explicit-active", Description: "显式 active", Status: StatusActive})
	reg.Register(&Skill{Name: "auto-shadow", Description: "自动提炼待裁决", Status: StatusShadow})
	reg.Register(&Skill{Name: "retired-one", Description: "已退役", Status: StatusArchived})

	// 1. 运行期清单只含 2 个 active
	active := reg.Active()
	if len(active) != 2 || active[0].Name != "explicit-active" || active[1].Name != "legacy-no-status" {
		var names []string
		for _, s := range active {
			names = append(names, s.Name)
		}
		t.Fatalf("Active() = %v, want [explicit-active legacy-no-status]", names)
	}

	// 2. 管理视图仍看到全部 4 个
	if got := len(reg.All()); got != 4 {
		t.Errorf("All() 应含全部技能, got %d want 4", got)
	}
	if got := reg.Count(); got != 4 {
		t.Errorf("Count() 应含全部技能, got %d want 4", got)
	}

	// 3. 清单文本: 不得出现 shadow/archived 技能名
	for _, listing := range []string{reg.FormatListing(), reg.FormatShortListing(0)} {
		if strings.Contains(listing, "auto-shadow") {
			t.Errorf("清单泄漏 shadow 技能:\n%s", listing)
		}
		if strings.Contains(listing, "retired-one") {
			t.Errorf("清单泄漏 archived 技能:\n%s", listing)
		}
		if !strings.Contains(listing, "legacy-no-status") {
			t.Errorf("无 status 的存量技能必须仍在清单里:\n%s", listing)
		}
	}

	// 4. 加载路径: Get 排除 shadow, GetAny 仍可取 (审计/自改进依赖)
	if _, ok := reg.Get("auto-shadow"); ok {
		t.Error("Get 不应返回 shadow 技能 (运行期视图)")
	}
	if _, ok := reg.GetAny("auto-shadow"); !ok {
		t.Error("GetAny 必须仍能取到 shadow 技能, 否则进化门禁/自改进无法工作")
	}
	if _, ok := reg.Get("legacy-no-status"); !ok {
		t.Error("无 status 的存量技能必须可被 Get 加载 (向后兼容硬要求)")
	}

	// 5. 按状态盘点
	if got := reg.WithStatus(StatusShadow); len(got) != 1 || got[0].Name != "auto-shadow" {
		t.Errorf("WithStatus(shadow) 应只含 auto-shadow, got %v", got)
	}
	if got := reg.WithStatus(StatusActive); len(got) != 2 {
		t.Errorf("WithStatus(active) 应含 2 个 (含无 status 的存量技能), got %d", len(got))
	}
}

// TestSkillToolRefusesShadow 守护 Skill 工具不加载 shadow 技能正文,
// 且给出可排障的明确错误 (而不是"未找到", 让人以为名字打错了)。
func TestSkillToolRefusesShadow(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Skill{Name: "shadow-one", Body: "机密的 shadow 正文", Status: StatusShadow})
	reg.Register(&Skill{Name: "ok-one", Body: "可用正文"})
	tl := NewSkillTool(reg)

	res, err := tl.Call(context.Background(), json.RawMessage(`{"name":"shadow-one"}`), nil)
	if err != nil {
		t.Fatalf("Call 报错: %v", err)
	}
	if !res.IsError {
		t.Error("加载 shadow 技能应返回错误结果")
	}
	if strings.Contains(res.Content, "机密的 shadow 正文") {
		t.Errorf("shadow 技能正文泄漏到工具结果: %s", res.Content)
	}
	if !strings.Contains(res.Content, StatusShadow) {
		t.Errorf("错误信息应说明处于 shadow 态, got %q", res.Content)
	}

	// 可用技能仍照常加载
	res2, err := tl.Call(context.Background(), json.RawMessage(`{"name":"ok-one"}`), nil)
	if err != nil || res2.IsError {
		t.Fatalf("active 技能应能加载: err=%v res=%+v", err, res2)
	}
	if !strings.Contains(res2.Content, "可用正文") {
		t.Errorf("active 技能正文缺失: %s", res2.Content)
	}

	// 未找到时的可用清单不应包含 shadow 技能名
	res3, _ := tl.Call(context.Background(), json.RawMessage(`{"name":"nope"}`), nil)
	if strings.Contains(res3.Content, "shadow-one") {
		t.Errorf("未找到提示泄漏 shadow 技能名: %s", res3.Content)
	}
}

// TestLoadFromDirsPreservesStatus 端到端: 从磁盘加载的 shadow 技能 (AutoCreator 的产物格式)
// 不进运行期清单, 无 status 的同目录技能不受影响。
func TestLoadFromDirsPreservesStatus(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// AutoCreator.MaybeCreate 的真实产物格式
	write("auto-refactor", "---\nname: auto-refactor\ndescription: 自动提炼\nwhen_to_use: X\ncreated_at: 2026-07-25T00:00:00Z\nauto_generated: true\nstatus: shadow\n---\n\nbody\n")
	write("hand-written", "---\nname: hand-written\ndescription: 人写的\n---\n\nbody\n")

	reg := NewRegistry()
	if n := reg.LoadFromDirs([]string{dir}, "project"); n != 2 {
		t.Fatalf("应加载 2 个技能, got %d", n)
	}
	if got := len(reg.Active()); got != 1 {
		t.Fatalf("Active() 应只含 1 个, got %d", got)
	}
	if reg.Active()[0].Name != "hand-written" {
		t.Errorf("Active() 应是 hand-written, got %s", reg.Active()[0].Name)
	}
	sk, ok := reg.GetAny("auto-refactor")
	if !ok || sk.Status != StatusShadow {
		t.Fatalf("shadow 技能应被解析出 status=shadow, got ok=%v skill=%+v", ok, sk)
	}
}
