package tool

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkArtifacts(t *testing.T, dir string, n int, now time.Time) []string {
	t.Helper()
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, "Read-"+string(rune('a'+i%26))+string(rune('0'+i/26))+".txt")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// 每个文件比前一个早一小时, 便于断言"删最旧的"
		mod := now.Add(-time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

func TestSweepByAge(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mkArtifacts(t, dir, 5, now)
	// 把其中两个改成 40 天前
	old := []string{
		filepath.Join(dir, "ancient-1.txt"),
		filepath.Join(dir, "ancient-2.txt"),
	}
	for _, p := range old {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := now.AddDate(0, 0, -40)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	sweepToolArtifacts(dir, 0, 30, now)

	for _, p := range old {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("超过 30 天的 artifact 应被删除: %s", filepath.Base(p))
		}
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 5 {
		t.Errorf("未过期的应保留 5 个, 实得 %d", len(left))
	}
}

func TestSweepByCount(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	paths := mkArtifacts(t, dir, 10, now)

	// 上限 4 条: 应删掉最旧的 6 个 (paths[4:] 是较旧的, 按 mod 升序删)
	sweepToolArtifacts(dir, 4, 0, now)

	left, _ := os.ReadDir(dir)
	if len(left) != 4 {
		t.Fatalf("应保留 4 个, 实得 %d", len(left))
	}
	// 最新的 4 个 (i=0..3) 必须还在
	for i := 0; i < 4; i++ {
		if _, err := os.Stat(paths[i]); err != nil {
			t.Errorf("最新的第 %d 个不该被删: %v", i, err)
		}
	}
	// 最旧的必须没了
	if _, err := os.Stat(paths[9]); !os.IsNotExist(err) {
		t.Error("最旧的应被删除")
	}
}

func TestSweepDisabledByZero(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mkArtifacts(t, dir, 6, now)
	ts := now.AddDate(0, 0, -999)
	_ = os.Chtimes(filepath.Join(dir, "Read-a0.txt"), ts, ts)

	sweepToolArtifacts(dir, 0, 0, now) // 两个都关

	left, _ := os.ReadDir(dir)
	if len(left) != 6 {
		t.Errorf("关闭清理时不应删任何东西, 实得 %d 个", len(left))
	}
}

func TestSweepLeavesSubdirectories(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mkArtifacts(t, dir, 3, now)
	sub := filepath.Join(dir, "keepme")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}

	sweepToolArtifacts(dir, 1, 0, now)

	if _, err := os.Stat(sub); err != nil {
		t.Errorf("子目录不该被清理动到: %v", err)
	}
}

func TestArtifactLimitFromEnv(t *testing.T) {
	key := envArtifactMaxFiles
	t.Setenv(key, "7")
	if got := artifactLimitFromEnv(key, 1000); got != 7 {
		t.Errorf("应读到 7, 得 %d", got)
	}
	t.Setenv(key, "0")
	if got := artifactLimitFromEnv(key, 1000); got != 0 {
		t.Errorf("0 应被尊重 (表示不限), 得 %d", got)
	}
	// 配错时退回默认值, 而不是当成"不限" —— 拼错变量名不该静默关掉清理
	t.Setenv(key, "abc")
	if got := artifactLimitFromEnv(key, 1000); got != 1000 {
		t.Errorf("非法值应退回默认 1000, 得 %d", got)
	}
	t.Setenv(key, "")
	if got := artifactLimitFromEnv(key, 1000); got != 1000 {
		t.Errorf("空值应用默认 1000, 得 %d", got)
	}
}
