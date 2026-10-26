package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitTestRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitTestRun(t, dir, "init", "-q")
	gitTestRun(t, dir, "config", "user.email", "test@example.com")
	gitTestRun(t, dir, "config", "user.name", "test")
	return dir
}

func writeScope(t *testing.T, root string, paths []string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "scope.json")
	raw, _ := json.Marshal(gitInspectScope{Root: root, Paths: paths})
	if err := os.WriteFile(f, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func callGitInspect(t *testing.T, input any) *toolResult {
	t.Helper()
	raw, _ := json.Marshal(input)
	res, err := NewGitInspectTool().Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return &toolResult{Content: res.Content, IsError: res.IsError}
}

type toolResult struct {
	Content string
	IsError bool
}

func TestGitInspectRequiresScope(t *testing.T) {
	t.Setenv(GitInspectScopeEnv, "")
	res := callGitInspect(t, map[string]string{"operation": "status"})
	if !res.IsError || !strings.Contains(res.Content, GitInspectScopeEnv) {
		t.Fatalf("无 scope 时应报错并指出变量名, got %+v", res)
	}
}

func TestGitInspectStatusAndDiffStayInScope(t *testing.T) {
	repo := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, repo, "add", ".")
	gitTestRun(t, repo, "commit", "-qm", "init")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(GitInspectScopeEnv, writeScope(t, repo, []string{"a.txt"}))

	status := callGitInspect(t, map[string]string{"operation": "status"})
	if status.IsError || !strings.Contains(status.Content, "a.txt") {
		t.Fatalf("status 应包含授权文件: %+v", status)
	}
	if strings.Contains(status.Content, "b.txt") {
		t.Fatalf("status 不应泄露范围外文件名: %+v", status)
	}

	diff := callGitInspect(t, map[string]string{"operation": "diff"})
	if diff.IsError || !strings.Contains(diff.Content, "a2") {
		t.Fatalf("diff 应包含授权文件内容: %+v", diff)
	}
	if strings.Contains(diff.Content, "b2") {
		t.Fatalf("diff 不应包含范围外文件内容: %+v", diff)
	}

	escape := callGitInspect(t, map[string]any{"operation": "diff", "path": "../outside"})
	if !escape.IsError {
		t.Fatalf("越界 path 应被拒绝: %+v", escape)
	}
	other := callGitInspect(t, map[string]any{"operation": "diff", "path": "b.txt"})
	if !other.IsError {
		t.Fatalf("范围外 path 应被拒绝: %+v", other)
	}
}

func TestGitInspectDiffPagesRejoinWithoutLoss(t *testing.T) {
	repo := initGitRepo(t)
	file := filepath.Join(repo, "a.txt")
	var old, changed strings.Builder
	for i := 0; i < 240; i++ {
		old.WriteString(fmt.Sprintf("old-%03d-%s\n", i, strings.Repeat("x", 180)))
		changed.WriteString(fmt.Sprintf("new-%03d-%s\n", i, strings.Repeat("y", 180)))
	}
	if err := os.WriteFile(file, []byte(old.String()), 0644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, repo, "add", "a.txt")
	gitTestRun(t, repo, "commit", "-qm", "init")
	if err := os.WriteFile(file, []byte(changed.String()), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(GitInspectScopeEnv, writeScope(t, repo, []string{"a.txt"}))

	// 省略 path 的既有调用保留完整 patch，作为逐页重组的对照；scope 里只有
	// a.txt，所以两次调用读取的授权范围相同。
	full := callGitInspect(t, map[string]string{"operation": "diff"})
	if full.IsError {
		t.Fatalf("full diff: %s", full.Content)
	}
	offset, pages := 0, 0
	var joined strings.Builder
	for {
		pages++
		if pages > 20 {
			t.Fatal("next_offset 没有收敛")
		}
		res := callGitInspect(t, map[string]any{
			"operation": "diff", "path": "a.txt", "offset": offset, "limit": 180,
		})
		if res.IsError {
			t.Fatalf("page %d: %s", pages, res.Content)
		}
		if len(res.Content) >= 24000 {
			t.Fatalf("page %d 仍会触发通用 24KB artifact 折叠: %d bytes", pages, len(res.Content))
		}
		parts := strings.SplitN(res.Content, "\n", 3)
		if len(parts) != 3 {
			t.Fatalf("分页响应缺少头/body: %q", res.Content)
		}
		var gotOffset, limit, total, next int
		if _, err := fmt.Sscanf(parts[0], "GIT_INSPECT_DIFF_PAGE offset=%d limit=%d total_lines=%d next_offset=%d",
			&gotOffset, &limit, &total, &next); err != nil {
			t.Fatalf("分页头无法解析: %q: %v", parts[0], err)
		}
		if gotOffset != offset || limit != 180 || total != len(strings.Split(full.Content, "\n")) {
			t.Fatalf("分页坐标错误: header=%q want offset=%d total=%d", parts[0], offset, len(strings.Split(full.Content, "\n")))
		}
		joined.WriteString(parts[2])
		if next == -1 {
			break
		}
		if next <= offset {
			t.Fatalf("next_offset 必须前进: %d -> %d", offset, next)
		}
		offset = next
	}
	if pages < 2 {
		t.Fatal("前置数据没有覆盖多页")
	}
	if joined.String() != full.Content {
		t.Fatalf("逐页重组与完整 patch 不一致: pages=%d joined=%d full=%d", pages, joined.Len(), len(full.Content))
	}

	bad := callGitInspect(t, map[string]any{"operation": "diff", "path": "a.txt", "offset": -1})
	if !bad.IsError {
		t.Fatal("负 offset 必须显式拒绝")
	}
}

func TestGitInspectRejectsRootThatIsNotTopLevel(t *testing.T) {
	repo := initGitRepo(t)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(GitInspectScopeEnv, writeScope(t, sub, []string{"x.txt"}))
	res := callGitInspect(t, map[string]string{"operation": "status"})
	if !res.IsError || !strings.Contains(res.Content, "顶层") {
		t.Fatalf("子目录 root 应被拒绝: %+v", res)
	}
}

func TestReadGuardBlocksOutsideAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(inside, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}

	t.Setenv(ReadRootEnv, root)
	if err := guardReadPath(inside); err != nil {
		t.Fatalf("根内路径应放行: %v", err)
	}
	if err := guardReadPath(outside); err == nil {
		t.Fatal("根外路径应被拒绝")
	}
	if err := guardReadPath(link); err == nil {
		t.Fatal("指向根外的符号链接应被拒绝")
	}
	if err := guardReadPath(filepath.Join(root, "..", "escape.txt")); err == nil {
		t.Fatal(".. 逃逸应被拒绝")
	}
	// 未创建的文件只要父目录在根内就放行, 让 FileRead 走正常的 not-found 分支。
	if err := guardReadPath(filepath.Join(root, "not-yet.txt")); err != nil {
		t.Fatalf("根内不存在的文件不应被误判为越界: %v", err)
	}
}

func TestGitInspectRedactsSecretsInDiff(t *testing.T) {
	repo := initGitRepo(t)
	file := filepath.Join(repo, "config.txt")
	if err := os.WriteFile(file, []byte("token=AKIA1234567890ABCDEF\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, repo, "add", "config.txt")
	gitTestRun(t, repo, "commit", "-qm", "add secret")
	if err := os.WriteFile(file, []byte("token=\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(GitInspectScopeEnv, writeScope(t, repo, []string{"config.txt"}))

	res := callGitInspect(t, map[string]string{"operation": "diff"})
	if res.IsError {
		t.Fatalf("diff 失败: %+v", res)
	}
	if strings.Contains(res.Content, "AKIA1234567890ABCDEF") {
		t.Fatalf("删除行里的密钥必须被脱敏: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[REDACTED:SECRET]") {
		t.Fatalf("应出现脱敏占位符: %s", res.Content)
	}
}
