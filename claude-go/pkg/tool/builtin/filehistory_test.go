// filehistory_test.go —— 文件级检查点测试。
//
// 锁定的语义均来自对 Claude Code file-history 的磁盘实测 (见 filehistory.go 头注):
//  1. 版本命名 = sha256(绝对路径)[:16]@v<N> (按路径寻址, 不是内容寻址);
//  2. v<N> 是该文件第 N 个修订状态, 最高版本内容 == 磁盘当前内容;
//  3. 淘汰只动本路径的版本, 不误删别人 (版本号跨路径不可比)。
//
// 另覆盖 fail-soft (存档失败不阻断写入) 与回退失败必须报错 (不静默成功)。
package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

func newTestHistory(t *testing.T) *FileHistory {
	t.Helper()
	h := NewFileHistory(t.TempDir(), t.TempDir())
	SetFileHistory(h)
	t.Cleanup(func() { SetFileHistory(nil) })
	return h
}

func writeViaTool(t *testing.T, path, contents string) string {
	t.Helper()
	res, err := (&FileWriteTool{}).Call(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"path":%q,"contents":%q}`, path, contents)),
		&tool.ToolContext{Cwd: "/"})
	if err != nil {
		t.Fatalf("Write 调用出错: %v", err)
	}
	if res.IsError {
		t.Fatalf("Write 失败: %s", res.Content)
	}
	return res.Content
}

// TestRefNameMatchesClaudeCode 版本文件名必须是 sha256(绝对路径)[:16]。
// 锚定实测: 某会话 manifest.json 的 ref 为 21b2927e60d9342c, 恰等于其绝对路径的
// sha256 前 8 字节。命名规则错了, 两边目录就无法对读。
func TestRefNameMatchesClaudeCode(t *testing.T) {
	h := newTestHistory(t)
	p := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(p, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeViaTool(t, p, "two")

	abs, _ := filepath.Abs(p)
	sum := sha256.Sum256([]byte(abs))
	wantBefore := hex.EncodeToString(sum[:8]) + "@v"

	for _, ref := range h.Versions(p, "") {
		if !strings.HasPrefix(ref, wantBefore) {
			t.Fatalf("ref %q 不符 <sha256(absPath)[:16]>@v<N> (期望前缀 %q)", ref, wantBefore)
		}
	}
	if h.RefFor(p, "") == "" {
		t.Fatal("写入后应有最新版本引用")
	}
}

// TestLatestVersionEqualsDisk 核心不变式: 最高版本内容 == 磁盘当前内容。
// 记反成"只存写前内容"会让最高版本落后于磁盘, 回退语义随之错位。
func TestLatestVersionEqualsDisk(t *testing.T) {
	h := newTestHistory(t)
	p := filepath.Join(t.TempDir(), "b.txt")
	if err := os.WriteFile(p, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(h.root, h.sess)

	// 预置快照尚未落定会话目录, 先触发一次让 h.sess 定下来。
	writeViaTool(t, p, "v2")
	dir = filepath.Join(h.root, h.sess)

	for i, want := range []string{"v2", "v3", "v4"} {
		if i > 0 {
			writeViaTool(t, p, want)
		}
		ref := h.RefFor(p, "")
		if got := string(mustRead(t, filepath.Join(dir, ref))); got != want {
			t.Fatalf("第 %d 步: 最新版本应为磁盘内容 %q, got %q", i, want, got)
		}
	}
}

// TestPreviousVersionRestoresOneStep 回退一步 = 恢复上一个修订。
func TestPreviousVersionRestoresOneStep(t *testing.T) {
	h := newTestHistory(t)
	p := filepath.Join(t.TempDir(), "c.txt")
	if err := os.WriteFile(p, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeViaTool(t, p, "v2")
	writeViaTool(t, p, "v3")

	if got := string(mustRead(t, p)); got != "v3" {
		t.Fatalf("前置: 磁盘应为 v3, got %q", got)
	}
	prev := h.PreviousVersion(p, "")
	if prev == "" {
		t.Fatal("应有可回退的上一版本")
	}
	if err := h.Restore(p, prev, ""); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := string(mustRead(t, p)); got != "v2" {
		t.Fatalf("回退一步后应为 v2, got %q", got)
	}
	// 再退一步 → v1。
	prev2 := h.PreviousVersion(p, "")
	if err := h.Restore(p, prev2, ""); err != nil {
		t.Fatalf("Restore 第二次: %v", err)
	}
	if got := string(mustRead(t, p)); got != "v1" {
		t.Fatalf("回退两步后应为 v1, got %q", got)
	}
}

// TestRepeatWriteDoesNotGrowVersions 每次写入只新增一个版本 (写前状态与上一轮
// 记过的状态相同 → 去重复用), 而不是两个。
func TestRepeatWriteDoesNotGrowVersions(t *testing.T) {
	h := newTestHistory(t)
	p := filepath.Join(t.TempDir(), "d.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 三次写入三个不同内容 → 版本应为 x, y, z 共 3 个 (首次写入前记 x)。
	writeViaTool(t, p, "y")
	writeViaTool(t, p, "z")
	writeViaTool(t, p, "w")
	if n := len(h.Versions(p, "")); n != 4 {
		t.Fatalf("4 个不同状态应恰有 4 个版本, got %d", n)
	}
	if v := h.VersionFor(p, ""); v != 4 {
		t.Fatalf("版本号应为 4, got %d", v)
	}
	// 重复写同一内容: 不新增版本。
	writeViaTool(t, p, "w")
	if n := len(h.Versions(p, "")); n != 4 {
		t.Fatalf("重复写同一内容不应新增版本, got %d", n)
	}
}

// TestPruneDoesNotTouchOtherPaths 淘汰只动本路径的版本。
//
// 实现里最易错的一处: 目录里混着多个文件的版本, 且版本号按路径分配、跨路径不可比。
// 若按版本号全局淘汰, A 的 v1 会被当成"比 B 的 v50 旧"删掉 —— 而它可能正是
// B 的回退目标。
func TestPruneDoesNotTouchOtherPaths(t *testing.T) {
	h := newTestHistory(t)
	dir := t.TempDir()
	pA := filepath.Join(dir, "a.txt")
	pB := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(pA, []byte("A0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pB, []byte("B0"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		writeViaTool(t, pB, fmt.Sprintf("B%d", i+1))
	}
	refB := h.RefFor(pB, "")
	bVersions := len(h.Versions(pB, ""))

	for i := 0; i < FileHistoryMaxVersionsPerFile+5; i++ {
		writeViaTool(t, pA, fmt.Sprintf("A%d", i+1))
	}

	if got := string(mustRead(t, filepath.Join(h.root, h.sess, refB))); got != "B5" {
		t.Fatalf("B 的最新版本被误删或串档: got %q", got)
	}
	if n := len(h.Versions(pB, "")); n != bVersions {
		t.Fatalf("B 的版本数不应随 A 的写入变化: was %d, now %d", bVersions, n)
	}
	if n := len(h.Versions(pA, "")); n > FileHistoryMaxVersionsPerFile {
		t.Fatalf("A 的版本数应被压到上限内, got %d", n)
	}
	if PrunedRefs() == 0 {
		t.Error("发生淘汰时应留下观测计数")
	}
}

// TestRestoreMissingRefErrors 回退失败必须报错, 不能静默成功。
func TestRestoreMissingRefErrors(t *testing.T) {
	h := newTestHistory(t)
	p := filepath.Join(t.TempDir(), "e.txt")
	for _, ref := range []string{"", "deadbeefdeadbeef@v9", "../escape"} {
		if err := h.Restore(p, ref, ""); err == nil {
			t.Fatalf("ref=%q 应报错而非静默成功", ref)
		}
	}
}

// TestFailOpenWithoutInjection 未注入实例时写入照常, 且不报告版本。
func TestFailOpenWithoutInjection(t *testing.T) {
	SetFileHistory(nil)
	p := filepath.Join(t.TempDir(), "f.txt")
	msg := writeViaTool(t, p, "hello")
	if got := string(mustRead(t, p)); got != "hello" {
		t.Fatalf("未启用检查点时写入应照常, got %q", got)
	}
	if strings.Contains(msg, "[file-history]") {
		t.Errorf("未启用检查点不应报告版本: %q", msg)
	}
}

// TestEditAlsoCheckpoints StrReplace 走同一条检查点路径, 且版本覆盖替换前后。
func TestEditAlsoCheckpoints(t *testing.T) {
	h := newTestHistory(t)
	p := filepath.Join(t.TempDir(), "g.txt")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := (&FileEditTool{}).Call(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"world","new_string":"there"}`, p)),
		&tool.ToolContext{Cwd: "/"})
	if err != nil || res.IsError {
		t.Fatalf("StrReplace 失败: %v %v", err, res)
	}
	if got := string(mustRead(t, p)); got != "hello there" {
		t.Fatalf("替换未生效: %q", got)
	}
	dir := filepath.Join(h.root, h.sess)
	if got := string(mustRead(t, filepath.Join(dir, h.RefFor(p, "")))); got != "hello there" {
		t.Fatalf("最新版本应为替换后内容, got %q", got)
	}
	prev := h.PreviousVersion(p, "")
	if prev == "" {
		t.Fatal("应有替换前的版本")
	}
	if got := string(mustRead(t, filepath.Join(dir, prev))); got != "hello world" {
		t.Fatalf("上一版本应为替换前内容, got %q", got)
	}
}

// TestRejectedWriteLeavesNoSnapshot 被拒的写入不产生新修订。
func TestRejectedWriteLeavesNoSnapshot(t *testing.T) {
	h := newTestHistory(t)
	p := filepath.Join(t.TempDir(), "h.txt")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := (&FileEditTool{}).Call(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"l","new_string":"L"}`, p)),
		&tool.ToolContext{Cwd: "/"})
	if !res.IsError {
		t.Fatal("多处匹配应被拒")
	}
	if ref := h.RefFor(p, ""); ref != "" {
		t.Errorf("被拒的写入不应产生版本, got %q", ref)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return b
}
