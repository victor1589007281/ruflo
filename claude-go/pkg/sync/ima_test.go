package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIMAScriptCandidatesOrdering 钉住候选顺序: 显式覆盖 > 状态卷 > 宿主 > cwd。
// 顺序即优先级 —— 换顺序会让 pod 优先落到那个空 hostPath 上, 就是本次修的回归。
func TestIMAScriptCandidatesOrdering(t *testing.T) {
	t.Setenv("IMA_SCRIPT", "")
	t.Setenv("CLAUDE_GO_STATE_DIR", "/data/.claude-go")
	got := imaScriptCandidates()

	want := []string{
		"/data/.claude-go/skills/ima-skill/ima_api.cjs",
		"/mnt/data/claude-go/skills/ima-skill/ima_api.cjs",
		"/home/victor/.claude/skills/ima-skill/ima_api.cjs",
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Fatalf("候选列表缺少 %q; 实得 %v", w, got)
		}
	}
	// 顺序断言: 状态卷必须排在宿主 skills 目录之前 (pod 里宿主那个是空目录)。
	if idxOf(got, want[0]) >= idxOf(got, want[2]) {
		t.Fatalf("状态卷未排在宿主 skills 之前: %v", got)
	}
}

// TestIMAScriptExplicitEnvWins 环境变量覆盖必须是第一候选。
func TestIMAScriptExplicitEnvWins(t *testing.T) {
	t.Setenv("IMA_SCRIPT", "/opt/ima/ima_api.cjs")
	got := imaScriptCandidates()
	if len(got) == 0 || got[0] != "/opt/ima/ima_api.cjs" {
		t.Fatalf("IMA_SCRIPT 未成为第一候选: %v", got)
	}
}

// TestIMACandidatesDedup 同一路径只出现一次 (否则报错文案会重复列同一个路径)。
func TestIMACandidatesDedup(t *testing.T) {
	got := imaScriptCandidates()
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Fatalf("候选 %q 重复出现: %v", p, got)
		}
		seen[p] = true
	}
}

// TestFirstExistingFileSkipsEmptyDir 第一候选是**空目录**时必须落到下一个真实文件。
// 这是生产事故的精确形状: /home/victor/.claude/skills/ima-skill 在 pod 里只有 . 和 ..。
func TestFirstExistingFileSkipsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	emptyDir := filepath.Join(dir, "empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "real.cjs")
	if err := os.WriteFile(real, []byte("// node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := firstExistingFile([]string{filepath.Join(emptyDir, "ima_api.cjs"), real, "/nonexistent/x.cjs"})
	if got != real {
		t.Fatalf("未跳过空目录下的路径, got %q want %q", got, real)
	}
}

// TestFirstExistingFileAllMissing 全不存在返回空串 —— call() 据此报"试过的路径"。
func TestFirstExistingFileAllMissing(t *testing.T) {
	dir := t.TempDir()
	if got := firstExistingFile([]string{filepath.Join(dir, "a"), filepath.Join(dir, "b")}); got != "" {
		t.Fatalf("全不存在时应返回空串, got %q", got)
	}
}

// TestIMAAdapterNoScriptFailLoud 脚本找不到时必须**失败**, 且报错带上试过的路径
// (不能让 node 拿一个空字符串路径报一句无从下手的 MODULE_NOT_FOUND)。
//
// 直接造 adapter 而不走 NewIMAAdapter: 走构造函数的话本机候选列表里
// "/mnt/data/claude-go/skills/ima-api.cjs" 真实存在, 场景永远构造不出来 ——
// 那样这条断言就退化成一条永不执行的死测试 (比没有更坏)。
func TestIMAAdapterNoScriptFailLoud(t *testing.T) {
	a := &IMAAdapter{ClientID: "id", APIKey: "key", Script: ""}
	_, err := a.call("openapi/note/v1/list_note", "{}", "{}")
	if err == nil {
		t.Fatal("脚本不存在却成功了 —— 这是个静默失败")
	}
	if !strings.Contains(err.Error(), "找不到 ima_api.cjs") {
		t.Fatalf("报错没说清是脚本缺失: %v", err)
	}
	if !strings.Contains(err.Error(), "IMA_SCRIPT") {
		t.Fatalf("报错没给出 IMA_SCRIPT 逃生舱: %v", err)
	}
}

// TestIMAAdapterNoNodeFailLoud: node 缺失时的报错要能自己说清是 node 找不到,
// 而不是把 exec 的 "executable file not found in $PATH" 原样抛出来。
func TestIMAAdapterNoNodeFailLoud(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ima_api.cjs")
	if err := os.WriteFile(script, []byte("// node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// IMA_NODE_BIN 显式指到一个不存在的路径: 分支直接返回它, 由 exec 报错。
	a := &IMAAdapter{ClientID: "id", APIKey: "key", Script: script}
	t.Setenv("IMA_NODE_BIN", filepath.Join(dir, "no-such-node"))
	_, err := a.call("openapi/note/v1/list_note", "{}", "{}")
	if err == nil {
		t.Fatal("node 不存在却成功了")
	}
	if !strings.Contains(err.Error(), script) {
		t.Fatalf("报错没带上实际用的 script 路径, 无法定位: %v", err)
	}
}

// TestIMAAdapterScriptResolved 构造一个真实的候选路径, 确认探测能选中它。
func TestIMAAdapterScriptResolved(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ima_api.cjs")
	if err := os.WriteFile(script, []byte("// node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IMA_SCRIPT", script)
	t.Setenv("CLAUDE_GO_STATE_DIR", "")

	a := NewIMAAdapter("id", "key")
	if a.Script != script {
		t.Fatalf("Script = %q, want %q", a.Script, script)
	}
}

func contains(list []string, v string) bool { return idxOf(list, v) >= 0 }

func idxOf(list []string, v string) int {
	for i, e := range list {
		if e == v {
			return i
		}
	}
	return -1
}
