package sandbox

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

type Manager struct {
	native  Runner
	docker  Runner
	k8s     Runner
	process Runner

	mu     sync.Mutex
	probes map[string]ProbeResult
}

var (
	defaultManagerOnce sync.Once
	defaultManager     *Manager
)

// DefaultManager 返回进程级单例 Manager, 四个 runner (native/docker/k8s/process) 一次装配到位。
func DefaultManager() *Manager {
	defaultManagerOnce.Do(func() {
		cfg := CurrentConfig()
		defaultManager = &Manager{
			native:  &NativeCgroupV2Runner{BasePath: cfg.Native.BasePath},
			docker:  &DockerRunner{},
			k8s:     &K8SRunner{},
			process: &ProcessRunner{},
			probes:  make(map[string]ProbeResult),
		}
	})
	return defaultManager
}

// Run 按解析出的运行时模式把命令分派给对应 runner, 并统一记账。
//
// 算法解读：
//
//	原理：这是一个"先补齐 spec 上的隔离语义、再按 mode 分派"的两段式。第一段把配置
//	      与三个环境变量 (CLAUDE_GO_SANDBOX_MODE / _REQUIRED / _ALLOW_UNSAFE_FALLBACK)
//	      折叠进 spec 的三个字段 (Runtime/RequireIsolation/AllowUnsafeFallback),
//	      第二段是一张 mode → runner 的表。
//	关键不变量：**隔离要求只能被加强, 不能被削弱**。cfg.Enabled 为假时把
//	      RequireIsolation 置 false 并打开 unsafe 回退, 这是唯一一处"降级"的授权点,
//	      它由配置显式给出; 其余分支 (team-verification、环境变量) 都只做单向提升。
//	      破坏它意味着一个声明了隔离的任务会被静默地放到裸进程里跑。
//	代价与边界：每次 Run 都要读一次 CurrentConfig() 与最多三个环境变量, 无缓存 ——
//	      换来的是配置热改能立即生效。"auto" 分支会额外做若干次 probe (有缓存),
//	      首次调用有探测开销。
//	设计权衡：把 process runner 也做成一个平级 runner (而不是"没有隔离就直接跑"),
//	      使"降级"必须经过 RequireIsolation/AllowUnsafeFallback 这一对显式判据 ——
//	      否则任何一次 mode 拼错都会变成静默的无隔离执行。
func (m *Manager) Run(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	spec = spec.normalized()
	cfg := CurrentConfig()
	if !cfg.Enabled {
		spec.RequireIsolation = false
		spec.AllowUnsafeFallback = true
		spec.Runtime = "process"
	}
	if spec.Purpose == "team-verification" && cfg.RequiredForTeam {
		spec.RequireIsolation = true
	}
	if cfg.AllowUnsafeFallback {
		spec.AllowUnsafeFallback = true
	}
	mode := strings.ToLower(strings.TrimSpace(spec.Runtime))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(firstNonEmpty(os.Getenv("CLAUDE_GO_SANDBOX_MODE"), cfg.Mode)))
	}
	if mode == "" {
		mode = "auto"
	}
	if boolEnv("CLAUDE_GO_SANDBOX_REQUIRED") {
		spec.RequireIsolation = true
	}
	if boolEnv("CLAUDE_GO_SANDBOX_ALLOW_UNSAFE_FALLBACK") {
		spec.AllowUnsafeFallback = true
	}

	switch mode {
	case "native", "native-cgroupv2", "cgroupv2":
		return m.runAndRecord(ctx, m.native, spec)
	case "docker":
		return m.runAndRecord(ctx, m.docker, spec)
	case "k8s", "kubernetes":
		return m.runAndRecord(ctx, m.k8s, spec)
	case "process", "process-unsafe", "unsafe":
		if spec.RequireIsolation && !spec.AllowUnsafeFallback {
			result, err := unavailable(spec, "process runner requested but isolation is required")
			recordRunMetrics(spec, result)
			return result, err
		}
		return m.runAndRecord(ctx, m.process, spec)
	case "auto":
		recordActiveMetric(spec, "auto", 1)
		defer recordActiveMetric(spec, "auto", -1)
		result, err := m.runAuto(ctx, spec)
		recordRunMetrics(spec, result)
		return result, err
	default:
		result, err := unavailable(spec, "unknown sandbox runtime: "+mode)
		recordRunMetrics(spec, result)
		return result, err
	}
}

// ProbeAll 探测全部可用 runner; k8s 仅在配置里显式开启时才探测。
func (m *Manager) ProbeAll(ctx context.Context) []ProbeResult {
	probes := []ProbeResult{
		m.probe(ctx, m.native),
		m.probe(ctx, m.docker),
	}
	cfg := CurrentConfig()
	if cfg.K8S.Enabled != nil && *cfg.K8S.Enabled {
		probes = append(probes, m.probe(ctx, m.k8s))
	}
	probes = append(probes, m.probe(ctx, m.process))
	return probes
}

// SelectRuntime 按 native → docker → k8s → process 的固定优先级挑第一个可用 runner。
//
// requireIsolation 为真时**不**回退到 process: 宁可在没有隔离能力时报 "none", 也不给
// 调用方一个看起来成功、实际无隔离的结果。
func (m *Manager) SelectRuntime(ctx context.Context, requireIsolation bool) ProbeResult {
	if p := m.probe(ctx, m.native); p.OK {
		return p
	}
	if p := m.probe(ctx, m.docker); p.OK {
		return p
	}
	cfg := CurrentConfig()
	if cfg.K8S.Enabled != nil && *cfg.K8S.Enabled {
		if p := m.probe(ctx, m.k8s); p.OK {
			return p
		}
	}
	if requireIsolation {
		return ProbeResult{Runtime: "none", OK: false, Detail: "no isolated runtime available"}
	}
	return m.probe(ctx, m.process)
}

// runAuto 按优先级挑一个探测通过的 runner 执行; 全不可用且要求隔离时返回 unavailable。
func (m *Manager) runAuto(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	if m.probe(ctx, m.native).OK {
		return m.native.Run(ctx, spec)
	}
	if m.probe(ctx, m.docker).OK {
		return m.docker.Run(ctx, spec)
	}
	cfg := CurrentConfig()
	if cfg.K8S.Enabled != nil && *cfg.K8S.Enabled && m.probe(ctx, m.k8s).OK {
		return m.k8s.Run(ctx, spec)
	}
	if spec.RequireIsolation && !spec.AllowUnsafeFallback {
		native := m.probe(ctx, m.native)
		docker := m.probe(ctx, m.docker)
		return unavailable(spec, fmt.Sprintf("no isolated runtime available; native=%q docker=%q", native.Detail, docker.Detail))
	}
	return m.process.Run(ctx, spec)
}

// runAndRecord 执行并记账: 活跃数 +1/-1 包住整个执行, 结束时落一次运行指标。
func (m *Manager) runAndRecord(ctx context.Context, runner Runner, spec CommandSpec) (*CommandResult, error) {
	runtime := "unknown"
	if runner != nil {
		runtime = runner.Name()
	}
	recordActiveMetric(spec, runtime, 1)
	defer recordActiveMetric(spec, runtime, -1)
	result, err := m.runRequired(ctx, runner, spec)
	recordRunMetrics(spec, result)
	return result, err
}

// runRequired 探测通过才执行; 探测失败直接返回 unavailable, 不尝试其它 runner。
func (m *Manager) runRequired(ctx context.Context, runner Runner, spec CommandSpec) (*CommandResult, error) {
	if !m.probe(ctx, runner).OK {
		p := m.probe(ctx, runner)
		return unavailable(spec, runner.Name()+": "+p.Detail)
	}
	return runner.Run(ctx, spec)
}

// probe 探测 runner 并按名字缓存结果 —— 一次进程内只探测一次, 避免每个命令都付探测开销。
//
// 注意：缓存键是 runner.Name(), 无失效机制。同进程内环境变化 (例如中途装上 docker)
// 不会被重新探测到; 需要重新探测只能重启进程或调 resetDefaultManager。
func (m *Manager) probe(ctx context.Context, runner Runner) ProbeResult {
	if runner == nil {
		return ProbeResult{OK: false, Detail: "runner nil"}
	}
	m.mu.Lock()
	if p, ok := m.probes[runner.Name()]; ok {
		m.mu.Unlock()
		return p
	}
	m.mu.Unlock()

	p := runner.Probe(ctx)
	recordProbeMetrics(p)

	m.mu.Lock()
	m.probes[runner.Name()] = p
	m.mu.Unlock()
	return p
}

// resetDefaultManager 清空单例与探测缓存 (测试用; 下次 DefaultManager 会重新装配)。
func resetDefaultManager() {
	defaultManagerOnce = sync.Once{}
	defaultManager = nil
}

// unavailable 构造一条"运行时不可用"的失败结果 (FailureKind=FailureRuntime), 并让返回的 err 与 result.Err 同源。
func unavailable(spec CommandSpec, detail string) (*CommandResult, error) {
	result := &CommandResult{
		ID:            spec.ID,
		Runtime:       spec.Runtime,
		Purpose:       spec.Purpose,
		Cwd:           spec.Cwd,
		Args:          append([]string(nil), spec.Args...),
		FailureKind:   FailureRuntime,
		RuntimeDetail: detail,
	}
	result.Err = fmt.Errorf("%s: %s", FailureRuntime, detail)
	return result, result.Err
}

// firstNonEmpty 返回第一个去空白后非空的字符串; 全空时返回空串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// RunProcessGuarded 用裸进程 runner 执行一条命令 (无隔离, 只有超时/输出上限)。
//
// AllowUnsafeFallback 显式置真: 调用方主动选择了无隔离执行, 不该再被
// "process runner requested but isolation is required" 拦下。
func RunProcessGuarded(ctx context.Context, cwd string, args []string, timeoutOutput ResourceLimits) (*CommandResult, error) {
	spec := CommandSpec{
		Purpose:             "process-guarded",
		Cwd:                 cwd,
		Args:                args,
		Runtime:             "process",
		AllowUnsafeFallback: true,
		Limits:              timeoutOutput,
	}
	recordActiveMetric(spec, "process-unsafe", 1)
	defer recordActiveMetric(spec, "process-unsafe", -1)
	result, err := (&ProcessRunner{}).Run(ctx, spec)
	recordRunMetrics(spec, result)
	return result, err
}
