package sandbox

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutputLimiterStopsUnboundedOutput(t *testing.T) {
	dir := t.TempDir()
	triggered := false
	limiter, err := NewOutputLimiter(OutputOptions{
		ID:              "test-output-limit",
		LogRoot:         dir,
		MaxTotalBytes:   12,
		MaxPreviewBytes: 8,
		MaxLogBytes:     10,
		OnLimit: func() {
			triggered = true
		},
	})
	if err != nil {
		t.Fatalf("NewOutputLimiter: %v", err)
	}
	defer limiter.Close()

	_, _ = limiter.Stdout().Write([]byte("hello"))
	_, _ = limiter.Stderr().Write([]byte("0123456789"))

	got := limiter.Result()
	if !triggered {
		t.Fatalf("expected output limit callback")
	}
	if !got.Truncated {
		t.Fatalf("expected truncated result")
	}
	if got.TotalBytes != 15 {
		t.Fatalf("total bytes=%d, want 15", got.TotalBytes)
	}
	if !strings.HasPrefix(got.LogDir, filepath.Join(dir, "test-output-limit")) {
		t.Fatalf("unexpected log dir: %s", got.LogDir)
	}
}

func TestProcessRunnerReturnsOutputLimitFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := (&ProcessRunner{}).Run(ctx, CommandSpec{
		Cwd:  t.TempDir(),
		Args: []string{"sh", "-c", "yes x"},
		Limits: ResourceLimits{
			Timeout:         2 * time.Second,
			OutputMaxBytes:  32 * 1024,
			PreviewMaxBytes: 1024,
			LogMaxBytes:     2048,
		},
	})
	if err == nil {
		t.Fatalf("expected output limit error")
	}
	if result == nil || result.FailureKind != FailureOutputLimit {
		t.Fatalf("failureKind=%v err=%v", result, err)
	}
	if bytes.Count([]byte(result.CombinedPreview), []byte("x")) == 0 {
		t.Fatalf("expected output preview, got %q", result.CombinedPreview)
	}
}
