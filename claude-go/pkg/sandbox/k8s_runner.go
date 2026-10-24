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

// Name 返回 runner 标识, 恒为 "k8s"。
func (r *K8SRunner) Name() string { return "k8s" }

// Probe 探测 k8s 后端是否可用: 依次检查运行时开关、kubectl 可执行文件、客户端版本与 workspacePVC 配置,
// 任一不满足即 OK=false 并在 Detail 里带上原因; 版本查询自带 5 秒超时。
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
	// CanPids=false: Job 未声明 pids 限制, 后端不承诺 pids 隔离, 所以不谎报这一项。
	return ProbeResult{Runtime: r.Name(), OK: true, Detail: cfg.K8S.Namespace, Latency: time.Since(start), CanMemory: true, CanPids: false, CanCPU: true}
}

// Run 在 k8s 上执行一条命令: apply Job → 等完成 → 收日志 → 回收 Job。
//
// 算法解读：
//
//	原理：把 CommandSpec 翻成一次性 Job (restartPolicy=Never, backoffLimit=0) 后 apply;
//	      kubectl wait 等 complete, 再把 logs --all-containers 的两个流灌进 OutputLimiter,
//	      最后按 输出超限 > 超时 > 进程失败 的优先级归类失败原因。
//	关键不变量：Job 必须被回收 —— defer 里的 kubectl delete --ignore-not-found 是唯一的
//	      清理点, 少了它每次执行都会在集群里留下 Job 与已结束的 Pod; 且失败归因顺序不能
//	      颠倒, 先判 waitErr 会把"输出超限"误报成"进程失败"。
//	代价与边界：每个 spec 至少四次 kubectl 往返 (apply/wait/logs/delete), 冷启动 kubectl
//	      在百毫秒量级; waitTimeout 缺省仅 30s, 长任务必须显式给 Limits.Timeout, 否则
//	      一定被判失败。日志读取用 context.Background(), 不受 runCtx 超时约束。
//	设计权衡：不复用 DockerRunner 那套 inspect 轮询, 而是把生命周期交给 k8s (wait + TTL) ——
//	      换来集群侧自动回收, 代价是强依赖 kubectl 可执行文件, 且拿不到容器退出码
//	      (失败时只能填 ExitCode=1)。
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
	// 唯一的回收点: 无论成败都删 Job; --ignore-not-found 让 apply 没落地时也不报错。
	// 注意：这里用的是裸 CurrentConfig().K8S.Namespace, 而 manifest 侧用 firstNonEmpty 兜底成
	// "claude-go-sandbox"; 若未配置 namespace, 两条路径会指向不同的命名空间 (wait/logs/delete 落空)。
	defer exec.Command("kubectl", "delete", "job", jobName, "-n", CurrentConfig().K8S.Namespace, "--ignore-not-found=true").Run()

	// 30s 只是兜底而非推荐值: 没显式给 Limits.Timeout 的 spec 在 k8s 后端上更容易被判超时。
	waitTimeout := spec.Limits.Timeout
	if waitTimeout <= 0 {
		waitTimeout = 30 * time.Second
	}
	wait := exec.CommandContext(runCtx, "kubectl", "wait", "job/"+jobName, "-n", CurrentConfig().K8S.Namespace, "--for=condition=complete", "--timeout="+waitTimeout.String())
	waitErr := wait.Run()

	// 限流器按 spec 的三档字节上限构造: 总量 / 预览 / 落盘日志各管一层, 超总量即置 Truncated。
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
	// 故意用 context.Background() 而不是 runCtx: 超时/取消后仍要把已产生的日志读完,
	// 已到手的输出比一次干净的取消更值钱。
	logs := exec.CommandContext(context.Background(), "kubectl", "logs", "job/"+jobName, "-n", CurrentConfig().K8S.Namespace, "--all-containers=true")
	stdout, _ := logs.StdoutPipe()
	stderr, _ := logs.StderrPipe()
	if err := logs.Start(); err == nil {
		// 两个流必须各自排空后再 Close: 只等 stdout 会让 stderr 的字节计数漏掉一半。
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
	// 超时判定看 runCtx 而不是 wait 的返回: kubectl wait 自身超时与节点卡死都归为 FailureTimeout。
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.FailureKind = FailureTimeout
		result.Err = fmt.Errorf("%s after %s", FailureTimeout, spec.Limits.Timeout)
		return result, result.Err
	}
	// 剩下的 wait 失败统一归进程失败; k8s 不回报容器退出码, 只能填 1。
	if waitErr != nil {
		result.ExitCode = 1
		result.FailureKind = FailureProcessFailed
		result.Err = waitErr
		return result, waitErr
	}
	return result, nil
}

// k8sJobName 由命令 ID 派生合法的 Job 名: 转小写、'_' 换成 '-'、截到 63 字符再去首尾 '-' (k8s 对象名上限 63)。
func k8sJobName(id string) string {
	name := "claude-go-" + strings.ToLower(strings.ReplaceAll(id, "_", "-"))
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-")
}

// buildK8SJobManifest 把 CommandSpec 渲染成 batch/v1 Job 的 JSON manifest —— 资源限制在这里
// 落到 container.resources, 容器固定以非 root + drop ALL capabilities + 不自动重启的姿态运行。
//
// 算法解读：
//
//	原理：按 spec > k8s 配置 > 常量的优先级逐项兜底, 把内存/CPU/PVC/TTL 映射成 Job 字段;
//	      未配 WorkspacePVC 直接报错 (没有工作区, 建 Job 无意义)。
//	关键不变量：cpu 的换算是 CPUQuotaPercent×10 毫核 (cgroup 的 100% = 1 核), 且 limits 与
//	      requests 必须取同一份 map —— 只设 limits 会让调度器按 request=0 超卖节点。
//	代价与边界：每次调用都重新 json.Marshal 一棵 map 树, 无缓存 (每个 spec 都不同);
//	      workspaceSubPath / serviceAccount 只在配置非空时才出现, 空值即整字段省略。
//	设计权衡：用 map[string]any 手搭而不是 typed struct —— 换来对可选字段的按需增删,
//	      代价是字段名拼错只能等到 kubectl apply 才暴露。
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
	// CPUQuotaPercent 是 cgroup 口径的百分比 (100 = 1 核), 换成 k8s 毫核要 ×10。
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
	// limits 与 requests 共用同一份 map: 只设 limits 会让调度器按 request=0 超卖节点。
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
			// backoffLimit=0 且 restartPolicy=Never: 失败不自动重试 —— 重试是引擎 retry 环的职责,
			// 两处都重会把 LLM 类命令的 token 消耗放大数倍。
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
