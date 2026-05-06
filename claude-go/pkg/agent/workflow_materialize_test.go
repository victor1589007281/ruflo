package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeCodeFromEmojiBacktickHeader(t *testing.T) {
	dir := t.TempDir()
	output := "### 📁 `agentDBV1/internal/domain/models.go`\n" +
		"```go\npackage domain\n\ntype Record struct{ ID string }\n```\n\n" +
		"### 📁 `agentDBV1/go.mod`\n" +
		"```mod\nmodule agentdbv1\n\ngo 1.22\n```\n"

	written := MaterializeCode(dir, output, "go")
	if len(written) != 2 {
		t.Fatalf("expected 2 materialized files, got %d: %#v", len(written), written)
	}
	if _, err := os.Stat(filepath.Join(dir, "agentDBV1", "internal", "domain", "models.go")); err != nil {
		t.Fatalf("expected models.go to be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agentDBV1", "go.mod")); err != nil {
		t.Fatalf("expected go.mod to be written: %v", err)
	}
}

func TestRunBuildCheckScopedFailsWhenGoModMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	errText := runBuildCheckScoped(dir, "go", []string{"./..."})
	if !strings.Contains(errText, "go.mod 未找到") {
		t.Fatalf("expected missing go.mod error, got %q", errText)
	}
}

func TestInferTaskBuildCwdForGeneratedProjectDir(t *testing.T) {
	cwd := filepath.Join(string(filepath.Separator), "tmp", "workspace")
	got := inferTaskBuildCwd(cwd, []string{
		"agentDBV1/internal/domain/models.go",
		"agentDBV1/go.mod",
	})
	want := filepath.Join(cwd, "agentDBV1")
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}
