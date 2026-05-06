package sandbox

import (
	"strconv"
	"sync"

	"github.com/anthropic/claude-go/pkg/metrics"
)

var activeMetrics = struct {
	sync.Mutex
	values map[string]float64
}{values: make(map[string]float64)}

func resetActiveMetrics() {
	activeMetrics.Lock()
	activeMetrics.values = make(map[string]float64)
	activeMetrics.Unlock()
}

func recordProbeMetrics(p ProbeResult) {
	c := MetricsCollector()
	if c == nil {
		return
	}
	status := "unavailable"
	if p.OK {
		status = "available"
	}
	labels := map[string]string{
		"runtime": p.Runtime,
		"status":  status,
	}
	c.RecordWithLabels("sandbox", metrics.MSandboxProbeLatencyMs, float64(p.Latency.Milliseconds()), labels)
	if p.OK {
		c.RecordWithLabels("sandbox", metrics.MSandboxRunnerSelected, 1, labels)
	}
}

func recordActiveMetric(spec CommandSpec, runtime string, delta float64) {
	c := MetricsCollector()
	if c == nil {
		return
	}
	runtime = firstNonEmpty(runtime, spec.Runtime, "unknown")
	purpose := firstNonEmpty(spec.Purpose, "command")
	key := runtime + "\x00" + purpose

	activeMetrics.Lock()
	next := activeMetrics.values[key] + delta
	if next < 0 {
		next = 0
	}
	activeMetrics.values[key] = next
	activeMetrics.Unlock()

	c.RecordWithLabels("sandbox", metrics.MSandboxActiveCount, next, map[string]string{
		"runtime": runtime,
		"purpose": purpose,
	})
}

func recordRunMetrics(spec CommandSpec, result *CommandResult) {
	c := MetricsCollector()
	if c == nil || result == nil {
		return
	}
	status := "success"
	if result.Failed() {
		status = "failed"
	}
	failure := string(result.FailureKind)
	labels := map[string]string{
		"runtime":      firstNonEmpty(result.Runtime, spec.Runtime, "unknown"),
		"purpose":      firstNonEmpty(spec.Purpose, result.Purpose, "command"),
		"status":       status,
		"failure_kind": failure,
		"exit_code":    strconv.Itoa(result.ExitCode),
	}
	c.RecordWithLabels("sandbox", metrics.MSandboxRunCount, 1, labels)
	c.RecordWithLabels("sandbox", metrics.MSandboxDurationMs, float64(result.Duration.Milliseconds()), labels)
	c.RecordWithLabels("sandbox", metrics.MSandboxExitCode, float64(result.ExitCode), labels)
	if spec.Limits.MemoryMaxMB > 0 {
		c.RecordWithLabels("sandbox", metrics.MSandboxMemoryLimitBytes, float64(spec.Limits.MemoryMaxMB)*1024*1024, labels)
	}
	if spec.Limits.OutputMaxBytes > 0 {
		c.RecordWithLabels("sandbox", metrics.MSandboxOutputLimitBytes, float64(spec.Limits.OutputMaxBytes), labels)
	}
	switch result.FailureKind {
	case FailureOOM:
		c.RecordWithLabels("sandbox", metrics.MSandboxOOMCount, 1, labels)
	case FailureTimeout:
		c.RecordWithLabels("sandbox", metrics.MSandboxTimeoutCount, 1, labels)
	case FailureOutputLimit:
		c.RecordWithLabels("sandbox", metrics.MSandboxOutputLimitCount, 1, labels)
	case FailurePidsLimit:
		c.RecordWithLabels("sandbox", metrics.MSandboxPidsLimitCount, 1, labels)
	case FailureRuntime, FailureSetup:
		c.RecordWithLabels("sandbox", metrics.MSandboxRuntimeUnavailableCount, 1, labels)
	}
}
