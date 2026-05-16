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
	// Enable subtree_control so child cgroups can use memory/pids/cpu controllers.
	// Without this, writes to child's memory.max etc fail with EACCES.
	canMem, canPids, canCPU := ensureSubtreeControl(base)
	subTest := filepath.Join(base, ".claude-go-probe-sub")
	if err := os.MkdirAll(subTest, 0o755); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cannot create sub-cgroup: " + err.Error(), Latency: time.Since(start)}
	}
	defer os.Remove(subTest)
	// Verify we can actually configure resource limits on the sub-cgroup.
	// This catches the case where subtree_control delegation is incomplete.
	if canMem {
		if err := os.WriteFile(filepath.Join(subTest, "memory.max"), []byte("max"), 0o644); err != nil {
			canMem = false
		}
	}
	if canPids {
		if err := os.WriteFile(filepath.Join(subTest, "pids.max"), []byte("max"), 0o644); err != nil {
			canPids = false
		}
	}
	// If neither memory nor pids can be enforced, this runtime offers no isolation
	// benefit over process-unsafe — fail the probe so a stronger runtime is picked.
	if !canMem && !canPids {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "no resource controllers (memory/pids) writable in sub-cgroup at " + base, Latency: time.Since(start)}
	}
	// Verify that we can migrate processes into the sub-cgroup. systemd cgroup
	// delegation requires the writer to have write permission on cgroup.procs of
	// the common ancestor of the source (our own cgroup) and destination cgroups.
	// In rootless setups where claude-go was launched from a user session scope
	// (e.g. session-N.scope), the common ancestor with app.slice/claude-go-sandbox
	// is user-1000.slice, which is owned by root. The earlier controller writes
	// (memory.max, pids.max) succeed because they don't migrate processes, but
	// the AfterStart hook's cgroup.procs write fails with EACCES at runtime —
	// every command then errors as "sandbox setup failed". Detect this here and
	// fail the probe so the manager falls back to docker.
	procsWriteOK := true
	procsDetail := ""
	testCmd := exec.CommandContext(ctx, "sleep", "1")
	if err := testCmd.Start(); err == nil {
		childPid := testCmd.Process.Pid
		if werr := os.WriteFile(filepath.Join(subTest, "cgroup.procs"), []byte(strconv.Itoa(childPid)), 0o644); werr != nil {
			procsWriteOK = false
			procsDetail = werr.Error()
		}
		_ = testCmd.Process.Kill()
		_, _ = testCmd.Process.Wait()
	}
	if !procsWriteOK {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cgroup.procs migration not permitted (systemd delegation gap): " + procsDetail, Latency: time.Since(start)}
	}
	return ProbeResult{Runtime: r.Name(), OK: true, Detail: base, Latency: time.Since(start), CanMemory: canMem, CanPids: canPids, CanCPU: canCPU}
}

// ensureSubtreeControl enables memory/pids/cpu controllers in the given cgroup's
// cgroup.subtree_control file, so that child cgroups inherit those controllers
// and can configure resource limits (memory.max, pids.max, cpu.max).
//
// Returns booleans indicating which controllers are available after the operation.
// Best-effort: failures are silent (caller verifies via the probe step).
func ensureSubtreeControl(base string) (canMemory, canPids, canCPU bool) {
	// Read controllers available in this cgroup (inherited from parent's subtree_control).
	availData, err := os.ReadFile(filepath.Join(base, "cgroup.controllers"))
	if err != nil {
		return false, false, false
	}
	avail := map[string]bool{}
	for _, c := range strings.Fields(string(availData)) {
		avail[c] = true
	}
	// Read what's currently enabled in subtree_control.
	curData, _ := os.ReadFile(filepath.Join(base, "cgroup.subtree_control"))
	enabled := map[string]bool{}
	for _, c := range strings.Fields(string(curData)) {
		enabled[c] = true
	}
	// Enable available controllers that aren't yet enabled.
	wants := []string{"memory", "pids", "cpu"}
	var toAdd []string
	for _, w := range wants {
		if avail[w] && !enabled[w] {
			toAdd = append(toAdd, "+"+w)
		}
	}
	if len(toAdd) > 0 {
		if err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte(strings.Join(toAdd, " ")), 0o644); err == nil {
			for _, w := range wants {
				if avail[w] {
					enabled[w] = true
				}
			}
		}
	}
	return enabled["memory"], enabled["pids"], enabled["cpu"]
}

func (r *NativeCgroupV2Runner) Run(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	spec = spec.normalized()
	if probe := r.Probe(ctx); !probe.OK {
		result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args, FailureKind: FailureRuntime, RuntimeDetail: probe.Detail}
		result.Err = fmt.Errorf("%s: %s", FailureRuntime, probe.Detail)
		return result, result.Err
	}

	// Defensive: re-ensure subtree_control before creating child cgroup, in case
	// another process or restart reset it. Idempotent and cheap.
	ensureSubtreeControl(r.basePath())

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
