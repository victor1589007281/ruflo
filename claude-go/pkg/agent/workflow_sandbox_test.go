package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunLimitedCommandWithDockerSandbox(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon unavailable")
	}
	if err := exec.Command("docker", "image", "inspect", "golang:1.24").Run(); err != nil {
		t.Skip("golang:1.24 image unavailable")
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("home directory unavailable")
	}
	dir, err := os.MkdirTemp(home, ".claude-go-sandbox-test-*")
	if err != nil {
		t.Fatalf("create mounted temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module sandboxcheck\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testSrc := "package sandboxcheck\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_GO_SANDBOX_MODE", "docker")
	t.Setenv("CLAUDE_GO_SANDBOX_ALLOW_UNSAFE_FALLBACK", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := runLimitedCommand(ctx, dir, []string{"go", "test", "-count=1", "./..."}, 512, 100)
	if err != nil {
		t.Fatalf("runLimitedCommand docker failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("expected go test output, got %q", string(out))
	}
}
