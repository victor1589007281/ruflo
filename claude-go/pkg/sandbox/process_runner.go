package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type ProcessRunner struct{}

// Name 返回后端名 "process-unsafe" —— 名字里的 unsafe 是**如实声明**: 这个后端没有内存隔离。
func (r *ProcessRunner) Name() string { return "process-unsafe" }

// Probe 汇报本后端的隔离能力: 只做超时与输出限流, 无 memory/pids/cpu cgroup 限制。
func (r *ProcessRunner) Probe(ctx context.Context) ProbeResult {
	return ProbeResult{
		Runtime:   r.Name(),
		OK:        true,
		Detail:    "host process runner with timeout/output limiter; no memory cgroup isolation",
		CanMemory: false,
		CanPids:   false,
		CanCPU:    false,
	}
}

// Run 直接在宿主上执行命令 (无 hook、无额外环境变量), 是各受限后端的兜底实现。
func (r *ProcessRunner) Run(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	return runLocalProcess(ctx, spec.normalized(), r.Name(), nil, nil)
}

type localProcessHooks struct {
	AfterStart func(cmd *exec.Cmd) error
	OnKill     func(cmd *exec.Cmd)
	AfterWait  func(cmd *exec.Cmd, result *CommandResult)
}

// runLocalProcess 是各后端的公共执行内核: 起进程、限时限流、回收整棵进程树、归类失败原因。
//
// 算法解读：
//
//	原理：exec.CommandContext 只能杀**直接子进程**, 而 shell 起的命令会派生出一整棵树
//		(grandchildren), 所以这里配 setProcessGroup + killProcessTree 做整组回收;
//		另有一个 goroutine 盯 runCtx.Done() 承担超时/取消时的补刀。
//	关键不变量：**先 Wait 再 close(done), 最后才排空 copyDone**。顺序反了会有两种错:
//		提前 close(done) 会让杀进程 goroutine 退出而漏杀; 不等 copyDone 直接读
//		limiter.Result() 会读到还没被 io.Copy 灌完的输出。processStarted 这个标志
//		也是必需的 —— OutputLimiter 的 OnLimit 回调可能在 Start 之前就被触发,
//		那时 cmd.Process 还是 nil。
//	代价与边界：输出经 OutputLimiter 中转 (每字节过一次判断), 超出上限即杀整组进程
//		并把结果标成 FailureOutputLimit; 失败归类只看错误文本与输出里的关键词
//		(classifyProcessFailure), 所以 OOM 判定是**启发式**的, 可能漏判或误判。
//	设计权衡：用管道 + 自己的限流器, 而不是 cmd.Output() —— 后者会把全部输出读进内存,
//		恰恰是"输出炸弹"要防的情形。代价是多两个 goroutine 与显式的排空同步。
func runLocalProcess(ctx context.Context, spec CommandSpec, runtimeName string, hooks *localProcessHooks, extraEnv map[string]string) (*CommandResult, error) {
	start := time.Now()
	result := &CommandResult{
		ID:      spec.ID,
		Runtime: runtimeName,
		Purpose: spec.Purpose,
		Cwd:     spec.Cwd,
		Args:    append([]string(nil), spec.Args...),
	}
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

	cmd := exec.CommandContext(runCtx, spec.Args[0], spec.Args[1:]...)
	cmd.Dir = spec.Cwd
	setProcessGroup(cmd)
	cmd.Env = append(os.Environ(), envPairs(extraEnv)...)
	cmd.Env = append(cmd.Env, envPairs(spec.Env)...)
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}

	logRoot := filepath.Join(spec.Cwd, ".claude-go", "sandboxes")
	var limiter *OutputLimiter
	var processStarted bool
	limiter, err := NewOutputLimiter(OutputOptions{
		ID:              spec.ID,
		LogRoot:         logRoot,
		MaxTotalBytes:   spec.Limits.OutputMaxBytes,
		MaxPreviewBytes: spec.Limits.PreviewMaxBytes,
		MaxLogBytes:     spec.Limits.LogMaxBytes,
		OnLimit: func() {
			if processStarted {
				if hooks != nil && hooks.OnKill != nil {
					hooks.OnKill(cmd)
				}
				killProcessTree(cmd.Process)
			}
		},
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
	processStarted = true
	if hooks != nil && hooks.AfterStart != nil {
		if err := hooks.AfterStart(cmd); err != nil {
			if hooks.OnKill != nil {
				hooks.OnKill(cmd)
			}
			killProcessTree(cmd.Process)
			_ = cmd.Wait()
			output := limiter.Result()
			applyOutput(result, output)
			result.Duration = time.Since(start)
			result.FailureKind = FailureSetup
			result.Err = err
			return result, err
		}
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			if hooks != nil && hooks.OnKill != nil {
				hooks.OnKill(cmd)
			}
			killProcessTree(cmd.Process)
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

	// 顺序有依赖: Wait 之后才 close(done) (否则杀进程 goroutine 提前退出而漏杀),
	// 且必须等两个 io.Copy 都结束才读 limiter.Result() (否则读到未灌完的输出)。
	waitErr := cmd.Wait()
	close(done)
	<-copyDone
	<-copyDone

	output := limiter.Result()
	applyOutput(result, output)
	result.Duration = time.Since(start)
	result.ExitCode = exitCode(waitErr)
	if hooks != nil && hooks.AfterWait != nil {
		hooks.AfterWait(cmd, result)
	}

	// 归类顺序也是刻意的: 输出超限排在超时之前 —— 限流触发的杀进程同样会让 ctx 到期,
	// 先判超时就会把"输出炸了"误报成"跑太久"。
	if output.Truncated {
		result.FailureKind = FailureOutputLimit
		result.Err = fmt.Errorf("%s: output exceeded %d bytes", FailureOutputLimit, spec.Limits.OutputMaxBytes)
		return result, result.Err
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.FailureKind = FailureTimeout
		result.Err = fmt.Errorf("%s after %s", FailureTimeout, spec.Limits.Timeout)
		return result, result.Err
	}
	if waitErr != nil {
		if result.FailureKind == FailureNone {
			result.FailureKind = classifyProcessFailure(waitErr, output.Combined)
		}
		result.Err = waitErr
		return result, waitErr
	}
	return result, nil
}

// applyOutput 把限流器产出的预览与截断标志回填到命令结果。
func applyOutput(result *CommandResult, output OutputResult) {
	result.StdoutPreview = output.StdoutPreview
	result.StderrPreview = output.StderrPreview
	result.CombinedPreview = output.Combined
	result.LogDir = output.LogDir
	result.OutputTruncated = output.Truncated
}

// envPairs 把环境变量映射摊平成 "k=v" 切片; 空映射返回 nil。
//
// 注意：map 遍历无序, 同一份 map 两次调用产出的顺序可能不同 (对 exec 无影响)。
func envPairs(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// exitCode 从 Wait 错误里取退出码; 无错误为 0, 非 ExitError (如被信号杀) 返回 -1。
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// classifyProcessFailure 按错误与输出的关键词把失败归类为 OOM 或一般进程失败。
//
// 注意：这是**文本启发式**而非可靠判据 —— "killed" 也可能来自超时补刀或外部 kill,
// 都会被归进 FailureOOM; 且仅识别英文关键词, 本地化的错误文本会落到默认分支。
func classifyProcessFailure(err error, out string) FailureKind {
	lower := strings.ToLower(out + " " + err.Error())
	switch {
	case strings.Contains(lower, "out of memory"),
		strings.Contains(lower, "cannot allocate memory"),
		strings.Contains(lower, "signal: killed"),
		strings.Contains(lower, "killed"):
		return FailureOOM
	default:
		return FailureProcessFailed
	}
}
