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

// Name 返回运行时名 "docker"。
func (r *DockerRunner) Name() string { return "docker" }

// Probe 探测 docker CLI 与 daemon 是否可用 (docker info 取 ServerVersion, 3 秒超时);
// 可用时声明支持内存/PID/CPU 三类限制。
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

// Run 在一次性容器里执行命令, 并把结果归入统一的失败分类。
//
// 算法解读：
//
//	原理：先 Probe 确认 docker 可用, 再拼参数 docker run 起容器; 输出经 OutputLimiter
//	      边读边限流; 结束后 docker inspect 取 OOMKilled 与真实退出码, 最后按失败类型归类返回。
//	关键不变量：失败分类的判据**有优先级**: 输出超限 > 超时 > OOM > 进程失败 ——
//	      同一结果常同时满足多条 (超时被杀也非零退出), 顺序决定归因, 不能随意调换。
//	代价与边界：每个容器三次 docker CLI 往返 (run / inspect / rm); ExitCode 优先信 inspect 的值,
//	      因为 docker run 自身的退出码不含容器内进程的真实退出码。
//	设计权衡：容器内不留任何持久状态 (tmpfs + 只读工作区外挂载), 代价是拿不到镜像层缓存复用,
//	      换来的是"可丢弃、可重跑、不污染宿主"。
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

// dockerRunArgs 拼装 docker run 的参数表: 安全加固 (cap-drop / no-new-privileges) +
// 资源限制 (内存/CPU/PID) + 工作区挂载 + 环境变量。
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
		// 工作区以 rw 挂载: 容器内的构建产物必须落到宿主机, 这是"沙箱里跑真命令"的前提。
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

// dockerInspectState 读容器的 OOMKilled 标志与真实退出码;
// inspect 失败或输出字段不足时返回 (false, nil), 交回调用方按 wait 结果判定。
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
