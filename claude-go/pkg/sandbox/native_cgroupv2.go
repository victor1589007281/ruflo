package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type NativeCgroupV2Runner struct {
	BasePath string
}

func (r *NativeCgroupV2Runner) Name() string { return "native-cgroupv2" }

func (r *NativeCgroupV2Runner) Probe(ctx context.Context) ProbeResult {
	start := time.Now()
	if runtime.GOOS != "linux" {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "native cgroup v2 is linux-only", Latency: time.Since(start)}
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cgroup v2 unified hierarchy not detected", Latency: time.Since(start)}
	}
	base := r.basePath()
	if err := os.MkdirAll(base, 0o755); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cgroup base not writable: " + err.Error(), Latency: time.Since(start)}
	}
	// cgroup v2 is a special filesystem — cannot write arbitrary files.
	// Test by writing to actual control files and creating a sub-cgroup.
	if err := os.WriteFile(filepath.Join(base, "cgroup.freeze"), []byte("1"), 0o644); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cgroup base not delegated/writable: " + err.Error(), Latency: time.Since(start)}
	}
	_ = os.WriteFile(filepath.Join(base, "cgroup.freeze"), []byte("0"), 0o644)
	subTest := filepath.Join(base, ".claude-go-probe-sub")
	if err := os.MkdirAll(subTest, 0o755); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cannot create sub-cgroup: " + err.Error(), Latency: time.Since(start)}
	}
	_ = os.Remove(subTest)
	return ProbeResult{Runtime: r.Name(), OK: true, Detail: base, Latency: time.Since(start), CanMemory: true, CanPids: true, CanCPU: true}
}

func (r *NativeCgroupV2Runner) Run(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	spec = spec.normalized()
	if probe := r.Probe(ctx); !probe.OK {
		result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args, FailureKind: FailureRuntime, RuntimeDetail: probe.Detail}
		result.Err = fmt.Errorf("%s: %s", FailureRuntime, probe.Detail)
		return result, result.Err
	}

	cgPath := filepath.Join(r.basePath(), spec.ID)
	if err := os.MkdirAll(cgPath, 0o755); err != nil {
		result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args, FailureKind: FailureSetup}
		result.Err = err
		return result, err
	}
	defer os.Remove(cgPath)

	if err := configureCgroup(cgPath, spec.Limits); err != nil {
		result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args, FailureKind: FailureSetup}
		result.Err = err
		return result, err
	}

	hooks := &localProcessHooks{
		AfterStart: func(cmd *exec.Cmd) error {
			return os.WriteFile(filepath.Join(cgPath, "cgroup.procs"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
		},
		OnKill: func(cmd *exec.Cmd) {
			_ = os.WriteFile(filepath.Join(cgPath, "cgroup.kill"), []byte("1"), 0o644)
		},
		AfterWait: func(cmd *exec.Cmd, result *CommandResult) {
			if cgroupEventCount(filepath.Join(cgPath, "memory.events"), "oom_kill") > 0 || cgroupEventCount(filepath.Join(cgPath, "memory.events"), "oom") > 0 {
				result.FailureKind = FailureOOM
				result.Err = fmt.Errorf("%s", FailureOOM)
			}
			if cgroupEventCount(filepath.Join(cgPath, "pids.events"), "max") > 0 {
				result.FailureKind = FailurePidsLimit
				result.Err = fmt.Errorf("%s", FailurePidsLimit)
			}
		},
	}
	return runLocalProcess(ctx, spec, r.Name(), hooks, nil)
}

func (r *NativeCgroupV2Runner) basePath() string {
	if r != nil && r.BasePath != "" {
		return r.BasePath
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_CGROUP_BASE")); v != "" {
		return v
	}
	return filepath.Join("/sys/fs/cgroup", "claude-go-"+strconv.Itoa(os.Getuid()))
}

func configureCgroup(path string, limits ResourceLimits) error {
	if limits.MemoryMaxMB > 0 {
		bytes := int64(limits.MemoryMaxMB) * 1024 * 1024
		if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte(strconv.FormatInt(bytes, 10)), 0o644); err != nil {
			return fmt.Errorf("write memory.max: %w", err)
		}
		_ = os.WriteFile(filepath.Join(path, "memory.swap.max"), []byte(strconv.FormatInt(bytes, 10)), 0o644)
	}
	if limits.PidsMax > 0 {
		if err := os.WriteFile(filepath.Join(path, "pids.max"), []byte(strconv.Itoa(limits.PidsMax)), 0o644); err != nil {
			return fmt.Errorf("write pids.max: %w", err)
		}
	}
	if limits.CPUQuotaPercent > 0 {
		periodUS := 100000
		quotaUS := limits.CPUQuotaPercent * periodUS / 100
		if quotaUS < 1000 {
			quotaUS = 1000
		}
		value := fmt.Sprintf("%d %d", quotaUS, periodUS)
		if err := os.WriteFile(filepath.Join(path, "cpu.max"), []byte(value), 0o644); err != nil {
			return fmt.Errorf("write cpu.max: %w", err)
		}
	}
	return nil
}

func cgroupEventCount(path, key string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			return n
		}
	}
	return 0
}
