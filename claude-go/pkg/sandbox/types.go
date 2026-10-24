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

// Failed 判定结果是否算失败: Err 非空、退出码非零、FailureKind 非 None 三者任一成立; nil 接收者视为成功。
func (r *CommandResult) Failed() bool {
	if r == nil {
		return false
	}
	return r.Err != nil || r.ExitCode != 0 || r.FailureKind != FailureNone
}

// Error 把失败结果转成 error: 优先返回原始 Err, 其次按 FailureKind, 最后按退出码兜底;
// 成功结果或 nil 接收者返回 nil。
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

// normalized 把 CommandSpec 的零值字段补成自洽的默认值 (ID/Purpose/超时/各类限额/镜像/网络开关/工作目录),
// 使各 runner 拿到 spec 后不必再各自兜底。
func (s CommandSpec) normalized() CommandSpec {
	cfg := CurrentConfig()
	if s.ID == "" {
		s.ID = newID()
	}
	if s.Purpose == "" {
		s.Purpose = "command"
	}
	// 限额只在零值时才填默认: 显式传 0 与"没传"无法区分, 所以这里没法用 0 表达"不限"。
	if s.Limits.Timeout <= 0 {
		s.Limits.Timeout = 30 * time.Second
	}
	if s.Limits.PidsMax <= 0 {
		s.Limits.PidsMax = cfg.PidsMax
	}
	// 输出三档限额可由环境变量覆盖配置默认值, 便于在部署侧调参而不改配置文件。
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
	// 注意：这一组是单向守卫 —— 只能把 false 补成 cfg 的值, 调用方无法用显式 false
	// 覆盖 cfg 里的 true (即"配置禁网就禁网", 这多半是刻意的安全默认, 但表达力是单向的)。
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

// defaultImageForArgs 按命令首词挑默认镜像 (go/gofmt→golang, python→python, cargo/rustc→rust),
// 未命中回退 ubuntu:24.04。
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
	// 优先级: 环境变量 > 配置的 DefaultImage/Docker.Image > 按命令猜 > ubuntu。
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

// envInt64 读环境变量并解析为 int64; 缺失、非法或非正数一律回退 fallback。
func envInt64(key string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	// n <= 0 也算非法: 限额类配置里 0 无意义, 一律当作"没配"。
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// boolEnv 把环境变量按宽松布尔解析 (1/true/yes/on, 大小写不敏感), 其余取值一律为假。
func boolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
