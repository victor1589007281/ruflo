package pluginrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/types"
)

// writeFile 在临时目录里造一个插件包文件 (自动建父目录)。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// loadPluginSet 把 <root>/plugins 下的插件资产装进 Set —— 与 LoadStandard 收
// KindPlugin 的是同一个 LoadDir, 测试不另造一条装载路径。
func loadPluginSet(t *testing.T, root string) *asset.Set {
	t.Helper()
	set := asset.NewSet()
	asset.LoadDir(filepath.Join(root, "plugins"), "test", asset.KindPlugin, set)
	return set
}

// assetNames 取出成员资产的名字切片 (断言用)。
func assetNames(in []*asset.Asset) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, a.Name)
	}
	return out
}

// TestExpandBundleDirAllMembers 目录形式 (PLUGIN.md + 五个标准子目录) 全成员展开。
func TestExpandBundleDirAllMembers(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "demo", "PLUGIN.md"),
		"---\nkind: plugin\nname: demo\ndescription: 演示插件\nversion: 0.1.0\n---\n插件正文\n")
	writeFile(t, filepath.Join(root, "plugins", "demo", "agents", "coder.md"),
		"---\nname: coder\ndescription: 写代码\n---\n你是 coder\n")
	// 目录形式的 agent (agents/<n>/AGENT.md) 也要认 —— LoadDir 的 docNames 约定。
	writeFile(t, filepath.Join(root, "plugins", "demo", "agents", "reviewer", "AGENT.md"),
		"---\ndescription: 审代码\n---\n你是 reviewer\n")
	writeFile(t, filepath.Join(root, "plugins", "demo", "skills", "commit", "SKILL.md"),
		"---\nname: commit\ndescription: 写提交信息\n---\n技能正文\n")
	writeFile(t, filepath.Join(root, "plugins", "demo", "hooks", "guard.md"),
		"---\nname: guard\nhooks:\n  - PreToolUse:command:./guard.sh:Bash*\n---\n")
	writeFile(t, filepath.Join(root, "plugins", "demo", "tools", "awesome.md"),
		"---\nname: awesome\n---\n工具说明\n")
	writeFile(t, filepath.Join(root, "plugins", "demo", "workflows", "release.md"),
		"---\nname: release\n---\n工作流正文\n")

	set := loadPluginSet(t, root)
	b, err := Expand(set, "demo")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if b.Name != "demo" {
		t.Errorf("Name=%q, want demo", b.Name)
	}
	wantDir := filepath.Join(root, "plugins", "demo")
	if b.Dir != wantDir {
		t.Errorf("Dir=%q, want %q", b.Dir, wantDir)
	}
	if got := assetNames(b.Agents); strings.Join(got, ",") != "coder,reviewer" {
		t.Errorf("Agents=%v, want [coder reviewer] (按名字排序)", got)
	}
	if got := assetNames(b.Skills); strings.Join(got, ",") != "commit" {
		t.Errorf("Skills=%v, want [commit]", got)
	}
	if got := assetNames(b.Hooks); strings.Join(got, ",") != "guard" {
		t.Errorf("Hooks=%v, want [guard]", got)
	}
	if got := assetNames(b.Tools); strings.Join(got, ",") != "awesome" {
		t.Errorf("Tools=%v, want [awesome]", got)
	}
	if got := assetNames(b.Workflows); strings.Join(got, ",") != "release" {
		t.Errorf("Workflows=%v, want [release]", got)
	}
	// 成员资产的来源必须标成 plugin:<name> —— 归属只能靠 Source 留住。
	for _, a := range b.Agents {
		if a.Source != "plugin:demo" {
			t.Errorf("agent %q Source=%q, want plugin:demo", a.Name, a.Source)
		}
	}
	// 干净的包不该有任何 Problem (没声明 commands/mcp, 子文件全合法)。
	if len(b.Problems) != 0 {
		t.Errorf("Problems=%v, want 空", b.Problems)
	}
	c := b.Counts()
	want := map[string]int{"agents": 2, "skills": 1, "hooks": 1, "tools": 1, "workflows": 1}
	for k, w := range want {
		if c[k] != w {
			t.Errorf("Counts()[%q]=%d, want %d (全表 %v)", k, c[k], w, c)
		}
	}
	// 五键必须齐全 (零成员也是 0, 不缺键)。
	for _, k := range []string{"agents", "skills", "hooks", "tools", "workflows"} {
		if _, ok := c[k]; !ok {
			t.Errorf("Counts() 缺键 %q", k)
		}
	}
	if b.SkillsDir() != filepath.Join(wantDir, "skills") {
		t.Errorf("SkillsDir=%q", b.SkillsDir())
	}
	if b.AgentsDir() != filepath.Join(wantDir, "agents") {
		t.Errorf("AgentsDir=%q", b.AgentsDir())
	}
	if b.HooksDir() != filepath.Join(wantDir, "hooks") {
		t.Errorf("HooksDir=%q", b.HooksDir())
	}
}

// TestExpandFlatPluginHasNoBundleDir 扁平形式 (plugins/foo.md) 没有 bundle 目录:
// 成员恒空, 且 Problems 里必须有一句如实说明 (不是静默的空)。
func TestExpandFlatPluginHasNoBundleDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "foo.md"),
		"---\nkind: plugin\nname: foo\ndescription: 扁平插件\n---\n正文\n")

	set := loadPluginSet(t, root)
	b, err := Expand(set, "foo")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if b.Dir != "" {
		t.Errorf("Dir=%q, want 空 (扁平形式没有 bundle 目录)", b.Dir)
	}
	if len(b.Agents)+len(b.Skills)+len(b.Hooks)+len(b.Tools)+len(b.Workflows) != 0 {
		t.Errorf("扁平形式不该展开出任何成员: %v", b.Counts())
	}
	if b.SkillsDir() != "" || b.AgentsDir() != "" || b.HooksDir() != "" {
		t.Errorf("扁平形式的 *Dir 都必须为空: skills=%q agents=%q hooks=%q", b.SkillsDir(), b.AgentsDir(), b.HooksDir())
	}
	found := false
	for _, p := range b.Problems {
		if strings.Contains(p, "扁平形式") {
			found = true
		}
	}
	if !found {
		t.Errorf("扁平形式必须在 Problems 里有一句说明, got %v", b.Problems)
	}
}

// TestExpandMissingPlugin 找不到的插件名必须报错 —— 不静默跳过。
func TestExpandMissingPlugin(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "foo.md"), "---\nname: foo\n---\n")
	set := loadPluginSet(t, root)

	if _, err := Expand(set, "nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("Expand(nope) 必须报错且带上名字, got %v", err)
	}
	if _, err := Expand(set, "  "); err == nil {
		t.Errorf("空名字必须报错")
	}
	if _, err := Expand(nil, "foo"); err == nil {
		t.Errorf("nil Set 必须报错")
	}
}

// TestHookConfigs 插件 hooks 成员经 hooks.ParseAssetHooks 解析:
// 合法的生效, 拼错的进 problems (带成员名前缀), 两条路同时走。
func TestHookConfigs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "demo", "PLUGIN.md"), "---\nname: demo\n---\n")
	writeFile(t, filepath.Join(root, "plugins", "demo", "hooks", "guard.md"),
		"---\nname: guard\nhooks:\n  - PreToolUse:command:./guard.sh:Bash*\n  - Bogus:command:x\n---\n")

	set := loadPluginSet(t, root)
	b, err := Expand(set, "demo")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	cfgs, probs := b.HookConfigs()
	if len(cfgs) != 1 {
		t.Fatalf("HookConfigs 配置数=%d, want 1 (%v)", len(cfgs), cfgs)
	}
	h := cfgs[0]
	if h.Event != types.HookEvent("PreToolUse") || h.HookType != types.HookTypeCommand ||
		h.Command != "./guard.sh" || h.If != "Bash*" {
		t.Errorf("hook 解析结果不符: %+v", h)
	}
	if len(probs) != 1 || !strings.Contains(probs[0], "Bogus") || !strings.HasPrefix(probs[0], "hooks/guard:") {
		t.Errorf("拼错的 hook 必须进 problems 且带成员名前缀, got %v", probs)
	}
}

// TestCommandsMCPDeclaredProblems PLUGIN.md 声明 commands:/mcp: 键时,
// Problems 必须如实写出"本版本无运行时消费方" —— 否则作者以为声明了就会生效。
func TestCommandsMCPDeclaredProblems(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "full", "PLUGIN.md"),
		"---\nname: full\ncommands: [ship-it]\nmcp: ./mcp.json\n---\n")

	set := loadPluginSet(t, root)
	b, err := Expand(set, "full")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	var sawCommands, sawMCP bool
	for _, p := range b.Problems {
		if strings.Contains(p, "commands") && strings.Contains(p, "无运行时消费方") {
			sawCommands = true
		}
		if strings.Contains(p, "mcp") && strings.Contains(p, "无运行时消费方") {
			sawMCP = true
		}
	}
	if !sawCommands {
		t.Errorf("声明了 commands 键却没有对应 Problem: %v", b.Problems)
	}
	if !sawMCP {
		t.Errorf("声明了 mcp 键却没有对应 Problem: %v", b.Problems)
	}
}

// TestExpandAllSorted ExpandAll 展开全部插件, 按名字排序 (两次运行同一个序)。
func TestExpandAllSorted(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "b.md"), "---\nname: b\n---\n")
	writeFile(t, filepath.Join(root, "plugins", "a.md"), "---\nname: a\n---\n")

	set := loadPluginSet(t, root)
	all := ExpandAll(set)
	if len(all) != 2 || all[0].Name != "a" || all[1].Name != "b" {
		t.Errorf("ExpandAll=%v, want [a b]", func() []string {
			var names []string
			for _, b := range all {
				names = append(names, b.Name)
			}
			return names
		}())
	}
	if ExpandAll(nil) != nil {
		t.Errorf("nil Set 的 ExpandAll 必须是 nil")
	}
}
