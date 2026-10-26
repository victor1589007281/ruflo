package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

func callOps(t *testing.T, input string) *toolResult {
	t.Helper()
	tool := NewOpsInvestigateTool()
	res, err := tool.Call(context.Background(), json.RawMessage(input), nil)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	return &toolResult{Content: res.Content, IsError: res.IsError}
}

func TestOpsInvestigate_RejectsUnknownDomain(t *testing.T) {
	r := callOps(t, `{"domain":"windows","instance":"x"}`)
	if !r.IsError || !strings.Contains(r.Content, "domain") {
		t.Fatalf("expected domain rejection, got %+v", r)
	}
}

func TestOpsInvestigate_MySQLRequiresInstance(t *testing.T) {
	r := callOps(t, `{"domain":"mysql"}`)
	if !r.IsError || !strings.Contains(r.Content, "instance is required") {
		t.Fatalf("expected instance rejection, got %+v", r)
	}
}

func TestOpsInvestigate_K8sRequiresContext(t *testing.T) {
	r := callOps(t, `{"domain":"k8s","context":"  "}`)
	if !r.IsError || !strings.Contains(r.Content, "context is required") {
		t.Fatalf("expected context rejection, got %+v", r)
	}
}

func TestOpsInvestigate_K8sRejectsIllegalNamespace(t *testing.T) {
	r := callOps(t, `{"domain":"k8s","context":"kind-hakit","namespace":"Bad_NS"}`)
	if !r.IsError || !strings.Contains(r.Content, "namespace") {
		t.Fatalf("expected namespace rejection, got %+v", r)
	}
}

func TestOpsInvestigate_K8sRejectsSQL(t *testing.T) {
	r := callOps(t, `{"domain":"k8s","context":"kind-hakit","sql":"select 1"}`)
	if !r.IsError || !strings.Contains(r.Content, "sql 只支持 domain=mysql") {
		t.Fatalf("expected sql rejection, got %+v", r)
	}
}

func TestOpsInvestigate_LinuxRejectsSQL(t *testing.T) {
	r := callOps(t, `{"domain":"linux","sql":"select 1"}`)
	if !r.IsError || !strings.Contains(r.Content, "sql 只支持 domain=mysql") {
		t.Fatalf("expected sql rejection, got %+v", r)
	}
}

func TestOpsFinishReturnsCompactViewAndWritesFullArtifact(t *testing.T) {
	artifactRoot := t.TempDir()
	t.Setenv("CLAUDE_GO_OPS_ARTIFACT_DIR", artifactRoot)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	b := &evidence.Bundle{
		IncidentID: "inc_finish", Instance: "host-a", Domain: "linux", GeneratedAt: now,
		Window: evidence.TimeWindow{Start: now.Add(-30 * time.Minute), End: now},
		Collection: evidence.CollectionInfo{Capabilities: []evidence.Capability{
			{Name: "unit_logs", Availability: "manual", Detail: "operator only"},
		}},
		Timeline: []evidence.TimelinePoint{{At: now, Metrics: map[string]float64{"timeline_only": 123}}},
		Evidences: []evidence.Evidence{{
			ID: "ev_finish", Type: evidence.TypeMetric, Source: "/proc/test", CollectedAt: now,
			Subject: evidence.Subject{Kind: "host", Name: "host-a"}, Grade: evidence.GradeE2,
			Reliability: 0.9, Summary: "summary visible to model", RawPreview: "raw_only_in_bundle",
			ProbeValid: true,
		}},
		Summary: "no eligible root cause",
	}
	tool := NewOpsInvestigateTool()
	res, err := tool.finish(b, "markdown")
	if err != nil || res.IsError {
		t.Fatalf("finish failed: err=%v result=%+v", err, res)
	}
	if !strings.Contains(res.Content, "view=compact") || !strings.Contains(res.Content, "ev_finish") {
		t.Fatalf("missing compact view or evidence: %s", res.Content)
	}
	if strings.Contains(res.Content, "raw_only_in_bundle") || strings.Contains(res.Content, "timeline_only") {
		t.Fatalf("raw/timeline should stay out of model view: %s", res.Content)
	}
	dir := filepath.Join(artifactRoot, b.IncidentID)
	report, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "## 证据") || !strings.Contains(string(report), "[需人工] unit_logs") {
		t.Fatalf("full report was not preserved: %s", report)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "raw_only_in_bundle") || !strings.Contains(string(raw), "timeline_only") {
		t.Fatalf("bundle lost raw/timeline data: %s", raw)
	}
}
