package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxMetricsJSONL(t *testing.T) {
	ResetConfigForTest()
	stateDir := t.TempDir()
	Configure(Config{StateDir: stateDir})
	defer ResetConfigForTest()

	_, _ = RunProcessGuarded(context.Background(), t.TempDir(), []string{"sh", "-c", "echo sandbox-ok"}, ResourceLimits{
		Timeout:         2 * time.Second,
		OutputMaxBytes:  1024,
		PreviewMaxBytes: 128,
		LogMaxBytes:     1024,
	})

	data, err := os.ReadFile(filepath.Join(stateDir, "metrics", "sandbox.jsonl"))
	if err != nil {
		t.Fatalf("expected sandbox metrics jsonl: %v", err)
	}
	body := string(data)
	for _, want := range []string{"sandbox_active_count", "sandbox_run_count", "sandbox_duration_ms"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in sandbox metrics: %s", want, body)
		}
	}
	if !strings.Contains(body, "\"value\":0") {
		t.Fatalf("missing sandbox metrics: %s", body)
	}
}
