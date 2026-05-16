package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type DockerRunner struct{}

func (r *DockerRunner) Name() string { return "docker" }

func (r *DockerRunner) Probe(ctx context.Context) ProbeResult {
	start := time.Now()
	if _, err := exec.LookPath("docker"); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "docker CLI not found", Latency: time.Since(start)}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "docker", "info", "--format", "{{.ServerVersion}}")
	out, err := cmd.Output()
	if err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: strings.TrimSpace(err.Error()), Latency: time.Since(start)}
	}
	return ProbeResult{Runtime: r.Name(), OK: true, Detail: strings.TrimSpace(string(out)), Latency: time.Since(start), CanMemory: true, CanPids: true, CanCPU: true}
}

func (r *DockerRunner) Run(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	spec = spec.normalized()
	if probe := r.Probe(ctx); !probe.OK {
		result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args, FailureKind: FailureRuntime, RuntimeDetail: probe.Detail}
		result.Err = fmt.Errorf("%s: %s", FailureRuntime, probe.Detail)
		return result, result.Err
	}

	start := time.Now()
	result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: append([]string(nil), spec.Args...)}
	if len(spec.Args) == 0 {
		result.FailureKind = FailureSetup
		result.Err = fmt.Errorf("empty command")
		return result, result.Err
	}

	runCtx := ctx
	cancel := func() {}
	if spec.Limits.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Limits.Timeout)
	}
	defer cancel()

	name := "claude-go-" + strings.ReplaceAll(spec.ID, "_", "-")
	dockerArgs := dockerRunArgs(name, spec)
	cmd := exec.CommandContext(runCtx, "docker", dockerArgs...)
	setProcessGroup(cmd)

	logRoot := filepath.Join(spec.Cwd, ".claude-go", "sandboxes")
	var limiter *OutputLimiter
	var started bool
	killContainer := func() {
		if started {
			_ = exec.Command("docker", "kill", name).Run()
		}
		killProcessTree(cmd.Process)
	}
	limiter, err := NewOutputLimiter(OutputOptions{
		ID:              spec.ID,
		LogRoot:         logRoot,
		MaxTotalBytes:   spec.Limits.OutputMaxBytes,
		MaxPreviewBytes: spec.Limits.PreviewMaxBytes,
		MaxLogBytes:     spec.Limits.LogMaxBytes,
		OnLimit:         killContainer,
	})
	if err != nil {
		result.FailureKind = FailureSetup
		result.Err = err
		return result, err
	}
	defer limiter.Close()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		result.FailureKind = FailureSetup
		result.Err = err
		return result, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		result.FailureKind = FailureSetup
		result.Err = err
		return result, err
	}
	if err := cmd.Start(); err != nil {
		output := limiter.Result()
		applyOutput(result, output)
		result.Duration = time.Since(start)
		result.FailureKind = FailureSetup
		result.Err = err
		return result, err
	}
	started = true

	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			killContainer()
		case <-done:
		}
	}()
	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(limiter.Stdout(), stdout)
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(limiter.Stderr(), stderr)
		copyDone <- struct{}{}
	}()

	waitErr := cmd.Wait()
	close(done)
	<-copyDone
	<-copyDone

	oomKilled, inspectedExit := dockerInspectState(name)
	if !spec.KeepSandboxOnFailure || waitErr == nil {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	}

	output := limiter.Result()
	applyOutput(result, output)
	result.Duration = time.Since(start)
	result.ExitCode = exitCode(waitErr)
	if inspectedExit != nil {
		result.ExitCode = *inspectedExit
	}

	switch {
	case output.Truncated:
		result.FailureKind = FailureOutputLimit
		result.Err = fmt.Errorf("%s: output exceeded %d bytes", FailureOutputLimit, spec.Limits.OutputMaxBytes)
		return result, result.Err
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		result.FailureKind = FailureTimeout
		result.Err = fmt.Errorf("%s after %s", FailureTimeout, spec.Limits.Timeout)
		return result, result.Err
	case oomKilled || result.ExitCode == 137:
		result.FailureKind = FailureOOM
		result.Err = fmt.Errorf("%s", FailureOOM)
		return result, result.Err
	case waitErr != nil:
		if result.ExitCode == 125 || result.ExitCode == 126 || result.ExitCode == 127 {
			result.FailureKind = FailureSetup
		} else {
			result.FailureKind = FailureProcessFailed
		}
		result.Err = waitErr
		return result, waitErr
	default:
		return result, nil
	}
}

func dockerRunArgs(name string, spec CommandSpec) []string {
	cfg := CurrentConfig()
	tempSize := cfg.Docker.TempSize
	if tempSize == "" {
		tempSize = "512m"
	}
	// 当宿主机启用了 userns-remap (如 /etc/subuid 配置) 时, 容器内 root 会被映射到
	// 一个非宿主机属主的 UID, 导致对 bind mount 的宿主机目录没有写权限。
	// 显式指定 --user 为当前宿主机用户, 使容器内 UID/GID 与宿主机文件属主一致,
	// 从而保证 go build / npm install 等需要写工作区的命令能正常执行。
	uid := os.Getuid()
	gid := os.Getgid()
	userSpec := ""
	if uid >= 0 && gid >= 0 {
		userSpec = fmt.Sprintf("%d:%d", uid, gid)
	}

	args := []string{
		"run",
		"--name", name,
		"--label", "claude-go.sandbox=true",
		"--label", "claude-go.sandbox.id=" + spec.ID,
		"--workdir", "/workspace",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,nosuid,nodev,exec,size=" + tempSize,
		"-e", "HOME=/tmp",
		"-e", "GOCACHE=/tmp/go-cache",
		"-e", "GOMODCACHE=/tmp/go-mod",
		"-v", filepath.Clean(spec.Cwd) + ":/workspace:rw",
	}
	if userSpec != "" {
		args = append(args, "--user", userSpec)
	}
	if spec.NetworkDisabled {
		args = append(args, "--network", "none")
	}
	if spec.Limits.MemoryMaxMB > 0 {
		mem := fmt.Sprintf("%dm", spec.Limits.MemoryMaxMB)
		args = append(args, "--memory", mem, "--memory-swap", mem)
	}
	if spec.Limits.CPUQuotaPercent > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%.2f", float64(spec.Limits.CPUQuotaPercent)/100.0))
	}
	if spec.Limits.PidsMax > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(spec.Limits.PidsMax))
	}
	if spec.DisableDockerAutoPull {
		args = append(args, "--pull", "never")
	}
	for k, v := range spec.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Args...)
	return args
}

func dockerInspectState(name string) (bool, *int) {
	cmd := exec.Command("docker", "inspect", "--format", "{{.State.OOMKilled}} {{.State.ExitCode}}", name)
	out, err := cmd.Output()
	if err != nil {
		return false, nil
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return false, nil
	}
	oom := fields[0] == "true"
	exit, err := strconv.Atoi(fields[1])
	if err != nil {
		return oom, nil
	}
	return oom, &exit
}
