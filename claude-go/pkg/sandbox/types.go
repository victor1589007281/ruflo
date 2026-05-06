// Package sandbox provides guarded command execution for agent-generated code.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type FailureKind string

const (
	FailureNone          FailureKind = ""
	FailureOOM           FailureKind = "sandbox_oom"
	FailureTimeout       FailureKind = "sandbox_timeout"
	FailureOutputLimit   FailureKind = "sandbox_output_limit"
	FailurePidsLimit     FailureKind = "sandbox_pids_limit"
	FailureSetup         FailureKind = "sandbox_setup_failed"
	FailureRuntime       FailureKind = "sandbox_runtime_unavailable"
	FailureProcessFailed FailureKind = "process_failed"
)

type ResourceLimits struct {
	Timeout         time.Duration
	MemoryMaxMB     int
	CPUQuotaPercent int
	PidsMax         int
	OutputMaxBytes  int64
	PreviewMaxBytes int
	LogMaxBytes     int64
}

type CommandSpec struct {
	ID                    string
	Purpose               string
	Cwd                   string
	Args                  []string
	Env                   map[string]string
	Stdin                 []byte
	Image                 string
	Runtime               string
	RequireIsolation      bool
	AllowUnsafeFallback   bool
	NetworkDisabled       bool
	KeepSandboxOnFailure  bool
	DisableDockerAutoPull bool
	Limits                ResourceLimits
}

type CommandResult struct {
	ID              string        `json:"id"`
	Runtime         string        `json:"runtime"`
	Purpose         string        `json:"purpose,omitempty"`
	Cwd             string        `json:"cwd,omitempty"`
	Args            []string      `json:"args,omitempty"`
	ExitCode        int           `json:"exitCode"`
	FailureKind     FailureKind   `json:"failureKind,omitempty"`
	Duration        time.Duration `json:"duration"`
	StdoutPreview   string        `json:"stdoutPreview,omitempty"`
	StderrPreview   string        `json:"stderrPreview,omitempty"`
	CombinedPreview string        `json:"combinedPreview,omitempty"`
	LogDir          string        `json:"logDir,omitempty"`
	OutputTruncated bool          `json:"outputTruncated,omitempty"`
	RuntimeDetail   string        `json:"runtimeDetail,omitempty"`
	Err             error         `json:"-"`
}

func (r *CommandResult) Failed() bool {
	if r == nil {
		return false
	}
	return r.Err != nil || r.ExitCode != 0 || r.FailureKind != FailureNone
}

func (r *CommandResult) Error() error {
	if r == nil || !r.Failed() {
		return nil
	}
	if r.Err != nil {
		return r.Err
	}
	if r.FailureKind != FailureNone {
		return fmt.Errorf("%s", r.FailureKind)
	}
	return fmt.Errorf("exit code %d", r.ExitCode)
}

type ProbeResult struct {
	Runtime   string
	OK        bool
	Detail    string
	Latency   time.Duration
	CanMemory bool
	CanPids   bool
	CanCPU    bool
}

type Runner interface {
	Name() string
	Probe(ctx context.Context) ProbeResult
	Run(ctx context.Context, spec CommandSpec) (*CommandResult, error)
}

func (s CommandSpec) normalized() CommandSpec {
	cfg := CurrentConfig()
	if s.ID == "" {
		s.ID = newID()
	}
	if s.Purpose == "" {
		s.Purpose = "command"
	}
	if s.Limits.Timeout <= 0 {
		s.Limits.Timeout = 30 * time.Second
	}
	if s.Limits.PidsMax <= 0 {
		s.Limits.PidsMax = cfg.PidsMax
	}
	if s.Limits.OutputMaxBytes <= 0 {
		s.Limits.OutputMaxBytes = envInt64("CLAUDE_GO_SANDBOX_OUTPUT_BYTES", cfg.OutputMaxBytes)
	}
	if s.Limits.PreviewMaxBytes <= 0 {
		s.Limits.PreviewMaxBytes = int(envInt64("CLAUDE_GO_SANDBOX_PREVIEW_BYTES", int64(cfg.PreviewMaxBytes)))
	}
	if s.Limits.LogMaxBytes <= 0 {
		s.Limits.LogMaxBytes = envInt64("CLAUDE_GO_SANDBOX_LOG_BYTES", cfg.LogMaxBytes)
	}
	if s.Image == "" {
		s.Image = defaultImageForArgs(s.Args)
	}
	if !s.NetworkDisabled {
		s.NetworkDisabled = cfg.NetworkDisabled
	}
	if !s.DisableDockerAutoPull {
		s.DisableDockerAutoPull = cfg.Docker.DisableAutoPull
	}
	if !s.KeepSandboxOnFailure {
		s.KeepSandboxOnFailure = cfg.Docker.KeepSandboxOnFailure
	}
	if s.Cwd == "" {
		if cwd, err := os.Getwd(); err == nil {
			s.Cwd = cwd
		}
	}
	return s
}

func defaultImageForArgs(args []string) string {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_SANDBOX_IMAGE")); v != "" {
		return v
	}
	cfg := CurrentConfig()
	if cfg.DefaultImage != "" {
		return cfg.DefaultImage
	}
	if cfg.Docker.Image != "" {
		return cfg.Docker.Image
	}
	if len(args) > 0 {
		switch args[0] {
		case "go", "gofmt":
			return "golang:1.24"
		case "python", "python3", "pytest":
			return "python:3.12-slim"
		case "cargo", "rustc":
			return "rust:1.84"
		}
	}
	return "ubuntu:24.04"
}

func envInt64(key string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func boolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
