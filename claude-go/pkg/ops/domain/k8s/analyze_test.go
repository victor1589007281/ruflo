package k8s

import (
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

var testNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.Local)

func testCollector() *Collector {
	return &Collector{
		cfg: Config{Context: "kind-test", Window: 30 * time.Minute, Now: func() time.Time { return testNow }},
		now: func() time.Time { return testNow },
	}
}

func runningPod(ns, name, node string) podInfo {
	return podInfo{
		Namespace: ns, Name: name, Node: node, Phase: "Running", Created: testNow.Add(-time.Hour),
		Containers: []containerInfo{{
			Name: "app", Image: "example/app:1", Ready: true,
			State: containerState{Running: &struct {
				StartedAt time.Time `json:"startedAt"`
			}{StartedAt: testNow.Add(-time.Hour)}},
		}},
	}
}

func baseSnapshot() *snapshot {
	s := &snapshot{
		context: "kind-test", now: testNow, window: 30 * time.Minute,
		nodes: []nodeInfo{{
			Name: "node-1", Ready: true, Conditions: map[string]string{"Ready": "True"},
			Kubelet: "v1.36.1", Capacity: map[string]string{"cpu": "8"}, Alloc: map[string]string{"cpu": "8"},
		}},
		nodesValid:  true,
		pods:        []podInfo{runningPod("default", "web-1", "node-1")},
		podsValid:   true,
		eventsValid: true,
		pvcsValid:   true,
		workloads: []workloadInfo{{
			Kind: "Deployment", Namespace: "default", Name: "web", Desired: 1, Ready: 1, Available: 1,
		}},
		workloadsValid: true,
	}
	return s
}

func build(s *snapshot) *evidence.Bundle {
	b := testCollector().buildBundle(s)
	analyze(b, s)
	return b
}

func findHypothesis(b *evidence.Bundle, id string) *evidence.Hypothesis {
	for i := range b.Hypotheses {
		if b.Hypotheses[i].ID == id {
			return &b.Hypotheses[i]
		}
	}
	return nil
}

func TestAnalyze_HealthyClusterHasNoRootCause(t *testing.T) {
	b := build(baseSnapshot())
	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	if len(b.Hypotheses) != 0 {
		t.Fatalf("healthy cluster should not trigger hypotheses: %+v", b.Hypotheses)
	}
	if !strings.Contains(b.Summary, "未发现满足 E3 门控的根因") {
		t.Fatalf("unexpected summary: %s", b.Summary)
	}
	md := RenderMarkdown(b)
	if !strings.Contains(md, "没有假设通过根因门控") {
		t.Fatal("report must explicitly state the gate was not passed")
	}
}

func TestCollectionCapabilitiesAndRestartTimestampCaliber(t *testing.T) {
	s := baseSnapshot()
	finished := testNow.Add(-2 * time.Hour)
	s.pods[0].Containers[0].RestartCount = 4
	s.pods[0].Containers[0].LastState = containerState{
		Terminated: &struct {
			ExitCode   int       `json:"exitCode"`
			Reason     string    `json:"reason"`
			Message    string    `json:"message"`
			FinishedAt time.Time `json:"finishedAt"`
		}{ExitCode: 1, Reason: "Error", FinishedAt: finished},
	}
	b := build(s)
	e := b.EvidenceByID("ev_k8s_restarts")
	if e == nil || !strings.Contains(e.Summary, "last_finished="+finished.Format(time.RFC3339)) ||
		!strings.Contains(e.Summary, "不是逐次重启时间线") {
		t.Fatalf("restart summary must expose the last known timestamp without inventing a timeline: %+v", e)
	}
	caps := map[string]string{}
	for _, c := range b.Collection.Capabilities {
		caps[c.Name] = c.Availability
	}
	if caps["fixed_kubectl_get"] != "tool" || caps["pod_logs"] != "manual" || caps["metrics_and_traffic"] != "manual" {
		t.Fatalf("K8s tool/manual boundaries are unclear: %+v", b.Collection.Capabilities)
	}
}

func TestAnalyze_OOMKilledIsE4RootCause(t *testing.T) {
	s := baseSnapshot()
	s.pods[0].Containers[0].Ready = false
	s.pods[0].Containers[0].State = containerState{
		Terminated: &struct {
			ExitCode   int       `json:"exitCode"`
			Reason     string    `json:"reason"`
			Message    string    `json:"message"`
			FinishedAt time.Time `json:"finishedAt"`
		}{ExitCode: 137, Reason: "OOMKilled", FinishedAt: testNow.Add(-time.Minute)},
	}
	b := build(s)
	e := b.EvidenceByID("ev_k8s_pod_failures")
	if e == nil || e.Grade != evidence.GradeE4 {
		t.Fatalf("OOMKilled should be E4, got %+v", e)
	}
	h := findHypothesis(b, "K1")
	if h == nil || !h.RootCause {
		t.Fatalf("K1 should be root cause, got %+v", h)
	}
	if !strings.Contains(h.Statement, "default/web-1") {
		t.Fatalf("statement should name the pod: %s", h.Statement)
	}
}

func TestAnalyze_OldOOMInLastStateIsNotE4(t *testing.T) {
	s := baseSnapshot()
	s.pods[0].Containers[0].Ready = false
	s.pods[0].Containers[0].RestartCount = 3
	s.pods[0].Containers[0].State = containerState{
		Waiting: &struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}{Reason: "CrashLoopBackOff"},
	}
	s.pods[0].Containers[0].LastState = containerState{
		Terminated: &struct {
			ExitCode   int       `json:"exitCode"`
			Reason     string    `json:"reason"`
			Message    string    `json:"message"`
			FinishedAt time.Time `json:"finishedAt"`
		}{ExitCode: 137, Reason: "OOMKilled", FinishedAt: testNow.Add(-5 * time.Hour)},
	}
	b := build(s)
	e := b.EvidenceByID("ev_k8s_pod_failures")
	if e == nil || e.Grade != evidence.GradeE3 {
		t.Fatalf("old OOMKilled in lastState is not direct current cause; want E3, got %+v", e)
	}
}

func TestAnalyze_CrashLoopNeedsRestartsOrEvent(t *testing.T) {
	s := baseSnapshot()
	s.pods[0].Containers[0].Ready = false
	s.pods[0].Containers[0].RestartCount = 5
	s.pods[0].Containers[0].State = containerState{
		Waiting: &struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"},
	}
	s.pods[0].Containers[0].LastState = containerState{
		Terminated: &struct {
			ExitCode   int       `json:"exitCode"`
			Reason     string    `json:"reason"`
			Message    string    `json:"message"`
			FinishedAt time.Time `json:"finishedAt"`
		}{ExitCode: 1, Reason: "Error", FinishedAt: testNow.Add(-5 * time.Minute)},
	}
	b := build(s)
	e := b.EvidenceByID("ev_k8s_pod_failures")
	if e == nil || e.Grade != evidence.GradeE3 {
		t.Fatalf("crashloop with restarts should be E3, got %+v", e)
	}
	h := findHypothesis(b, "K1")
	if h == nil || !h.RootCause {
		t.Fatalf("K1 should be root cause, got %+v", h)
	}

	// 只有 waiting 状态、没有重启计数也没有事件：单源 E2，不能定因。
	s2 := baseSnapshot()
	s2.pods[0].Containers[0].Ready = false
	s2.pods[0].Containers[0].State = containerState{
		Waiting: &struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}{Reason: "CrashLoopBackOff"},
	}
	b2 := build(s2)
	h2 := findHypothesis(b2, "K1")
	if h2 == nil || h2.RootCause {
		t.Fatalf("single-source crashloop must not be a root cause, got %+v", h2)
	}
}

func TestAnalyze_ImagePullNeedsEventForE3(t *testing.T) {
	s := baseSnapshot()
	s.pods[0].Containers[0].Ready = false
	s.pods[0].Containers[0].State = containerState{
		Waiting: &struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}{Reason: "ImagePullBackOff", Message: "Back-off pulling image"},
	}
	s.events = []eventInfo{{
		Namespace: "default", Kind: "Pod", Name: "web-1", Reason: "Failed",
		Message: "Failed to pull image \"example/app:1\": not found", Type: "Warning",
		Last: testNow.Add(-time.Minute), Count: 3,
	}}
	b := build(s)
	e := b.EvidenceByID("ev_k8s_pod_failures")
	if e == nil || e.Grade != evidence.GradeE3 {
		t.Fatalf("image pull with event should be E3, got %+v", e)
	}
	h := findHypothesis(b, "K2")
	if h == nil || !h.RootCause {
		t.Fatalf("K2 should be root cause, got %+v", h)
	}
}

func TestAnalyze_SchedulingFailureNeedsEventForE3(t *testing.T) {
	s := baseSnapshot()
	s.pods[0] = podInfo{Namespace: "default", Name: "web-1", Phase: "Pending", Reason: "Unschedulable", Created: testNow.Add(-time.Minute)}
	s.workloads[0].Ready = 0
	s.workloads[0].Available = 0
	s.events = []eventInfo{{
		Namespace: "default", Kind: "Pod", Name: "web-1", Reason: "FailedScheduling",
		Message: "0/1 nodes are available: 1 Insufficient memory", Type: "Warning",
		Last: testNow.Add(-time.Minute), Count: 12,
	}}
	b := build(s)
	e := b.EvidenceByID("ev_k8s_scheduling")
	if e == nil || e.Grade != evidence.GradeE3 {
		t.Fatalf("FailedScheduling + Pending should be E3, got %+v", e)
	}
	h := findHypothesis(b, "K3")
	if h == nil || !h.RootCause {
		t.Fatalf("K3 should be root cause, got %+v", h)
	}
}

func TestAnalyze_NodePressureNeedsImpact(t *testing.T) {
	s := baseSnapshot()
	s.nodes[0].Pressure = []string{"MemoryPressure"}
	s.pods[0].Containers[0].Ready = false // Pod 受影响，但没有进入失败态
	b := build(s)
	e := b.EvidenceByID("ev_k8s_nodes")
	if e == nil || e.Grade != evidence.GradeE3 {
		t.Fatalf("node pressure with degraded pod should be E3, got %+v", e)
	}
	h := findHypothesis(b, "K4")
	if h == nil || !h.RootCause {
		t.Fatalf("K4 should be root cause, got %+v", h)
	}

	// 压力条件为 True 但没有任何 Pod 受影响：单源 E2 + 未解释矛盾。
	s2 := baseSnapshot()
	s2.nodes[0].Pressure = []string{"MemoryPressure"}
	b2 := build(s2)
	e2 := b2.EvidenceByID("ev_k8s_nodes")
	if e2 == nil || e2.Grade != evidence.GradeE2 {
		t.Fatalf("isolated node pressure should stay E2, got %+v", e2)
	}
	h2 := findHypothesis(b2, "K4")
	if h2 == nil || h2.RootCause {
		t.Fatalf("isolated node pressure must not be a root cause, got %+v", h2)
	}
	var kc3 bool
	for _, c := range b2.Contradictions {
		if c.ID == "KC3" {
			kc3 = true
		}
	}
	if !kc3 {
		t.Fatalf("expected KC3 contradiction, got %+v", b2.Contradictions)
	}
}

func TestAnalyze_PendingPVCWithProvisioningFailure(t *testing.T) {
	s := baseSnapshot()
	s.pvcs = []pvcInfo{{Namespace: "default", Name: "data", Phase: "Pending", StorageClass: "fast"}}
	s.events = []eventInfo{{
		Namespace: "default", Kind: "PersistentVolumeClaim", Name: "data",
		Reason: "ProvisioningFailed", Message: "no storage class", Type: "Warning",
		Last: testNow.Add(-5 * time.Minute), Count: 4,
	}}
	b := build(s)
	e := b.EvidenceByID("ev_k8s_pvc")
	if e == nil || e.Grade != evidence.GradeE3 {
		t.Fatalf("ProvisioningFailed + Pending PVC should be E3, got %+v", e)
	}
	h := findHypothesis(b, "K5")
	if h == nil || !h.RootCause {
		t.Fatalf("K5 should be root cause, got %+v", h)
	}
}

func TestAnalyze_WorkloadUnavailableFusesWithPodFailure(t *testing.T) {
	s := baseSnapshot()
	s.workloads[0].Desired = 2
	s.workloads[0].Ready = 1
	s.workloads[0].Available = 1
	s.workloads[0].Unavailable = 1
	s.pods[0].Containers[0].Ready = false
	s.pods[0].Containers[0].RestartCount = 4
	s.pods[0].Containers[0].State = containerState{
		Waiting: &struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}{Reason: "CrashLoopBackOff"},
	}
	b := build(s)
	e := b.EvidenceByID("ev_k8s_workloads")
	if e == nil || e.Grade != evidence.GradeE3 {
		t.Fatalf("unavailable workload with pod failure should be E3, got %+v", e)
	}
	h := findHypothesis(b, "K6")
	if h == nil || !h.RootCause {
		t.Fatalf("K6 should be root cause, got %+v", h)
	}
}

func TestAnalyze_HistoricalRestartsAreNotCurrentFailure(t *testing.T) {
	s := baseSnapshot()
	s.pods[0].Containers[0].RestartCount = 3
	s.pods[0].Containers[0].LastState = containerState{
		Terminated: &struct {
			ExitCode   int       `json:"exitCode"`
			Reason     string    `json:"reason"`
			Message    string    `json:"message"`
			FinishedAt time.Time `json:"finishedAt"`
		}{ExitCode: 1, Reason: "Error", FinishedAt: testNow.Add(-5 * time.Hour)},
	}
	b := build(s)
	h := findHypothesis(b, "K7")
	if h == nil || h.Status != evidence.HypothesisInconcl || h.RootCause {
		t.Fatalf("historical restart should stay inconclusive, got %+v", h)
	}
	var kc1 bool
	for _, c := range b.Contradictions {
		if c.ID == "KC1" && c.Resolution != "" {
			kc1 = true
		}
	}
	if !kc1 {
		t.Fatalf("expected resolved KC1, got %+v", b.Contradictions)
	}
	if len(b.Summary) == 0 || strings.Contains(b.Summary, "满足门控的根因候选") {
		t.Fatalf("historical restarts must not yield a root cause: %s", b.Summary)
	}
}

func TestAnalyze_ProbeFailureIsNotZero(t *testing.T) {
	s := &snapshot{
		context: "kind-test", now: testNow, window: 30 * time.Minute,
		nodesNote: "forbidden", podsNote: "forbidden", eventsNote: "forbidden",
		pvcsNote: "forbidden", workloadsNote: "forbidden",
	}
	b := build(s)
	if err := b.Validate(); err != nil {
		t.Fatalf("bundle invalid: %v", err)
	}
	for _, id := range []string{"ev_k8s_nodes", "ev_k8s_pods", "ev_k8s_events", "ev_k8s_pvc", "ev_k8s_workloads"} {
		e := b.EvidenceByID(id)
		if e == nil || e.ProbeValid {
			t.Fatalf("%s must be probe_valid=false, got %+v", id, e)
		}
	}
	if !strings.Contains(b.Summary, "未发现满足 E3 门控的根因") {
		t.Fatalf("unexpected summary: %s", b.Summary)
	}
	var missingNodes, missingPods, missingEvents bool
	for _, m := range b.Missing {
		switch {
		case strings.Contains(m.What, "节点状态"):
			missingNodes = true
		case strings.Contains(m.What, "Pod 状态"):
			missingPods = true
		case strings.Contains(m.What, "Warning 事件"):
			missingEvents = true
		}
	}
	if !missingNodes || !missingPods || !missingEvents {
		t.Fatalf("probe failures must appear in missing: %+v", b.Missing)
	}
}

func TestBuildBundle_EvidenceIDsAreStable(t *testing.T) {
	s := baseSnapshot()
	b1 := build(s)
	b2 := build(baseSnapshot())
	if len(b1.Evidences) != len(b2.Evidences) {
		t.Fatalf("evidence count differs: %d vs %d", len(b1.Evidences), len(b2.Evidences))
	}
	for i := range b1.Evidences {
		if b1.Evidences[i].ID != b2.Evidences[i].ID {
			t.Fatalf("evidence id[%d] differs: %s vs %s", i, b1.Evidences[i].ID, b2.Evidences[i].ID)
		}
	}
}
