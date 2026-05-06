package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	name string
	ok   bool
}

func (r fakeRunner) Name() string { return r.name }

func (r fakeRunner) Probe(ctx context.Context) ProbeResult {
	return ProbeResult{Runtime: r.name, OK: r.ok, Detail: r.name, Latency: time.Millisecond, CanMemory: r.ok, CanPids: r.ok, CanCPU: r.ok}
}

func (r fakeRunner) Run(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	result := &CommandResult{ID: spec.ID, Runtime: r.name, Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args}
	return result, nil
}

func TestManagerAutoSelectionPrefersNativeThenDocker(t *testing.T) {
	ResetConfigForTest()
	defer ResetConfigForTest()
	mgr := &Manager{
		native:  fakeRunner{name: "native-cgroupv2", ok: true},
		docker:  fakeRunner{name: "docker", ok: true},
		k8s:     fakeRunner{name: "k8s", ok: true},
		process: fakeRunner{name: "process-unsafe", ok: true},
		probes:  map[string]ProbeResult{},
	}
	result, err := mgr.Run(context.Background(), CommandSpec{Purpose: "team-verification", Args: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Runtime != "native-cgroupv2" {
		t.Fatalf("expected native runtime, got %s", result.Runtime)
	}
}

func TestManagerRequiresIsolationBeforeProcessFallback(t *testing.T) {
	ResetConfigForTest()
	defer ResetConfigForTest()
	mgr := &Manager{
		native:  fakeRunner{name: "native-cgroupv2", ok: false},
		docker:  fakeRunner{name: "docker", ok: false},
		k8s:     fakeRunner{name: "k8s", ok: false},
		process: fakeRunner{name: "process-unsafe", ok: true},
		probes:  map[string]ProbeResult{},
	}
	result, err := mgr.Run(context.Background(), CommandSpec{Purpose: "team-verification", Args: []string{"true"}})
	if err == nil {
		t.Fatal("expected runtime unavailable error")
	}
	if result == nil || result.FailureKind != FailureRuntime {
		t.Fatalf("unexpected result: %+v err=%v", result, err)
	}
}

func TestManagerAutoSelectionUsesK8SWhenEnabled(t *testing.T) {
	enabled := true
	Configure(Config{K8S: K8SConfig{Enabled: &enabled, WorkspacePVC: "claude-go-workspace"}})
	defer ResetConfigForTest()

	mgr := &Manager{
		native:  fakeRunner{name: "native-cgroupv2", ok: false},
		docker:  fakeRunner{name: "docker", ok: false},
		k8s:     fakeRunner{name: "k8s", ok: true},
		process: fakeRunner{name: "process-unsafe", ok: true},
		probes:  map[string]ProbeResult{},
	}
	result, err := mgr.Run(context.Background(), CommandSpec{Purpose: "team-verification", Args: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Runtime != "k8s" {
		t.Fatalf("expected k8s runtime, got %s", result.Runtime)
	}
}

func TestConfigureCgroupWritesLimits(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"memory.max", "memory.swap.max", "pids.max", "cpu.max"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := configureCgroup(dir, ResourceLimits{MemoryMaxMB: 128, PidsMax: 64, CPUQuotaPercent: 150}); err != nil {
		t.Fatal(err)
	}
	assertFile := func(name, want string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(data)); got != want {
			t.Fatalf("%s=%q, want %q", name, got, want)
		}
	}
	assertFile("memory.max", "134217728")
	assertFile("memory.swap.max", "134217728")
	assertFile("pids.max", "64")
	assertFile("cpu.max", "150000 100000")
}

func TestCgroupEventCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.events")
	if err := os.WriteFile(path, []byte("low 0\noom 2\noom_kill 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cgroupEventCount(path, "oom_kill"); got != 1 {
		t.Fatalf("oom_kill=%d, want 1", got)
	}
}

func TestK8SProbeDisabledWithoutConfig(t *testing.T) {
	ResetConfigForTest()
	defer ResetConfigForTest()

	p := (&K8SRunner{}).Probe(context.Background())
	if p.OK || !strings.Contains(p.Detail, "disabled") {
		t.Fatalf("expected disabled k8s probe, got %+v", p)
	}
}

func TestK8SManifestIncludesResourceLimitsAndPVC(t *testing.T) {
	enabled := true
	Configure(Config{
		K8S: K8SConfig{
			Enabled:            &enabled,
			Namespace:          "agent-sandbox",
			Image:              "golang:1.24",
			WorkspacePVC:       "claude-go-workspace",
			WorkspaceSubPath:   "team/run",
			WorkspaceMountPath: "/workspace",
		},
	})
	defer ResetConfigForTest()

	spec := CommandSpec{
		ID:   "sbx_test",
		Args: []string{"go", "test", "./..."},
		Limits: ResourceLimits{
			MemoryMaxMB:     512,
			CPUQuotaPercent: 200,
		},
	}
	manifest, err := buildK8SJobManifest("claude-go-test", spec.normalized())
	if err != nil {
		t.Fatal(err)
	}
	body := string(manifest)
	for _, want := range []string{"agent-sandbox", "claude-go-workspace", "team/run", "512Mi", "2000m", "allowPrivilegeEscalation"} {
		if !strings.Contains(body, want) {
			t.Fatalf("manifest missing %q: %s", want, body)
		}
	}
}
