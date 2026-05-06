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

func (m *Manager) runRequired(ctx context.Context, runner Runner, spec CommandSpec) (*CommandResult, error) {
	if !m.probe(ctx, runner).OK {
		p := m.probe(ctx, runner)
		return unavailable(spec, runner.Name()+": "+p.Detail)
	}
	return runner.Run(ctx, spec)
}

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

func resetDefaultManager() {
	defaultManagerOnce = sync.Once{}
	defaultManager = nil
}

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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

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
