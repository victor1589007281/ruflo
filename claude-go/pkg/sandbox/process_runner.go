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

func (r *ProcessRunner) Name() string { return "process-unsafe" }

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

func (r *ProcessRunner) Run(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	return runLocalProcess(ctx, spec.normalized(), r.Name(), nil, nil)
}

type localProcessHooks struct {
	AfterStart func(cmd *exec.Cmd) error
	OnKill     func(cmd *exec.Cmd)
	AfterWait  func(cmd *exec.Cmd, result *CommandResult)
}

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

func applyOutput(result *CommandResult, output OutputResult) {
	result.StdoutPreview = output.StdoutPreview
	result.StderrPreview = output.StderrPreview
	result.CombinedPreview = output.Combined
	result.LogDir = output.LogDir
	result.OutputTruncated = output.Truncated
}

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
