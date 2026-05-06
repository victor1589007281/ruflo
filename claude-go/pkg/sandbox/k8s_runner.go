package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type K8SRunner struct{}

func (r *K8SRunner) Name() string { return "k8s" }

func (r *K8SRunner) Probe(ctx context.Context) ProbeResult {
	start := time.Now()
	cfg := CurrentConfig()
	if cfg.K8S.Enabled == nil || !*cfg.K8S.Enabled {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "k8s runtime disabled", Latency: time.Since(start)}
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "kubectl not found", Latency: time.Since(start)}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "kubectl", "version", "--client=true", "-o", "json")
	if out, err := cmd.CombinedOutput(); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: strings.TrimSpace(string(out)), Latency: time.Since(start)}
	}
	if cfg.K8S.WorkspacePVC == "" {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "k8s workspacePVC not configured", Latency: time.Since(start)}
	}
	return ProbeResult{Runtime: r.Name(), OK: true, Detail: cfg.K8S.Namespace, Latency: time.Since(start), CanMemory: true, CanPids: false, CanCPU: true}
}

func (r *K8SRunner) Run(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	spec = spec.normalized()
	probe := r.Probe(ctx)
	if !probe.OK {
		result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args, FailureKind: FailureRuntime, RuntimeDetail: probe.Detail}
		result.Err = fmt.Errorf("%s: %s", FailureRuntime, probe.Detail)
		return result, result.Err
	}
	if len(spec.Args) == 0 {
		result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args, FailureKind: FailureSetup}
		result.Err = fmt.Errorf("empty command")
		return result, result.Err
	}

	start := time.Now()
	result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: append([]string(nil), spec.Args...)}
	jobName := k8sJobName(spec.ID)
	manifest, err := buildK8SJobManifest(jobName, spec)
	if err != nil {
		result.FailureKind = FailureSetup
		result.Err = err
		return result, err
	}

	runCtx := ctx
	cancel := func() {}
	if spec.Limits.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Limits.Timeout)
	}
	defer cancel()

	apply := exec.CommandContext(runCtx, "kubectl", "apply", "-f", "-")
	apply.Stdin = bytes.NewReader(manifest)
	if out, err := apply.CombinedOutput(); err != nil {
		result.Duration = time.Since(start)
		result.CombinedPreview = string(out)
		result.FailureKind = FailureSetup
		result.Err = err
		return result, err
	}
	defer exec.Command("kubectl", "delete", "job", jobName, "-n", CurrentConfig().K8S.Namespace, "--ignore-not-found=true").Run()

	waitTimeout := spec.Limits.Timeout
	if waitTimeout <= 0 {
		waitTimeout = 30 * time.Second
	}
	wait := exec.CommandContext(runCtx, "kubectl", "wait", "job/"+jobName, "-n", CurrentConfig().K8S.Namespace, "--for=condition=complete", "--timeout="+waitTimeout.String())
	waitErr := wait.Run()

	limiter, err := NewOutputLimiter(OutputOptions{
		ID:              spec.ID,
		LogRoot:         filepath.Join(spec.Cwd, ".claude-go", "sandboxes"),
		MaxTotalBytes:   spec.Limits.OutputMaxBytes,
		MaxPreviewBytes: spec.Limits.PreviewMaxBytes,
		MaxLogBytes:     spec.Limits.LogMaxBytes,
	})
	if err != nil {
		result.FailureKind = FailureSetup
		result.Err = err
		return result, err
	}
	logs := exec.CommandContext(context.Background(), "kubectl", "logs", "job/"+jobName, "-n", CurrentConfig().K8S.Namespace, "--all-containers=true")
	stdout, _ := logs.StdoutPipe()
	stderr, _ := logs.StderrPipe()
	if err := logs.Start(); err == nil {
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(limiter.Stdout(), stdout); done <- struct{}{} }()
		go func() { _, _ = io.Copy(limiter.Stderr(), stderr); done <- struct{}{} }()
		_ = logs.Wait()
		<-done
		<-done
	}
	_ = limiter.Close()

	output := limiter.Result()
	applyOutput(result, output)
	result.Duration = time.Since(start)
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
		result.ExitCode = 1
		result.FailureKind = FailureProcessFailed
		result.Err = waitErr
		return result, waitErr
	}
	return result, nil
}

func k8sJobName(id string) string {
	name := "claude-go-" + strings.ToLower(strings.ReplaceAll(id, "_", "-"))
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-")
}

func buildK8SJobManifest(jobName string, spec CommandSpec) ([]byte, error) {
	cfg := CurrentConfig()
	kcfg := cfg.K8S
	if kcfg.WorkspacePVC == "" {
		return nil, fmt.Errorf("k8s workspacePVC not configured")
	}
	namespace := firstNonEmpty(kcfg.Namespace, "claude-go-sandbox")
	mountPath := firstNonEmpty(kcfg.WorkspaceMountPath, "/workspace")
	image := firstNonEmpty(kcfg.Image, spec.Image, cfg.DefaultImage, cfg.Docker.Image, "golang:1.24")
	ttl := kcfg.TTLSecondsAfterFinished
	if ttl <= 0 {
		ttl = 3600
	}
	limits := map[string]string{}
	if spec.Limits.MemoryMaxMB > 0 {
		limits["memory"] = fmt.Sprintf("%dMi", spec.Limits.MemoryMaxMB)
	}
	if spec.Limits.CPUQuotaPercent > 0 {
		limits["cpu"] = fmt.Sprintf("%dm", spec.Limits.CPUQuotaPercent*10)
	}
	pvc := map[string]any{"claimName": kcfg.WorkspacePVC}
	volumeMount := map[string]any{"name": "workspace", "mountPath": mountPath}
	if kcfg.WorkspaceSubPath != "" {
		volumeMount["subPath"] = kcfg.WorkspaceSubPath
	}
	container := map[string]any{
		"name":            "runner",
		"image":           image,
		"command":         spec.Args,
		"workingDir":      mountPath,
		"imagePullPolicy": "IfNotPresent",
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"readOnlyRootFilesystem":   false,
			"runAsNonRoot":             true,
			"capabilities":             map[string]any{"drop": []string{"ALL"}},
		},
		"volumeMounts": []any{volumeMount},
	}
	if len(limits) > 0 {
		container["resources"] = map[string]any{"limits": limits, "requests": limits}
	}
	podSpec := map[string]any{
		"restartPolicy": "Never",
		"containers":    []any{container},
		"volumes":       []any{map[string]any{"name": "workspace", "persistentVolumeClaim": pvc}},
	}
	if kcfg.ServiceAccount != "" {
		podSpec["serviceAccountName"] = kcfg.ServiceAccount
	}
	manifest := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      jobName,
			"namespace": namespace,
			"labels": map[string]string{
				"claude-go.sandbox":    "true",
				"claude-go.sandbox.id": spec.ID,
			},
		},
		"spec": map[string]any{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": ttl,
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"claude-go.sandbox": "true", "claude-go.sandbox.id": spec.ID}},
				"spec":     podSpec,
			},
		},
	}
	return json.Marshal(manifest)
}
