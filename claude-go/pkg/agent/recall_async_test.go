package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// slowDecisionServer returns 503 after d. It counts attempts so tests can
// prove the client no longer multiplies one timeout into three retries.
func slowDecisionServer(t *testing.T, d time.Duration) (*httptest.Server, *int32, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	var attempts int32
	hit := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		atomic.AddInt32(&attempts, 1)
		time.Sleep(d)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"busy"}`))
		select {
		case done <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &attempts, hit, done
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func seedRecallExperiences(ee *EvolutionEngine) {
	ee.mu.Lock()
	defer ee.mu.Unlock()
	ee.experiences = []*Experience{
		{
			ID: "exp-redo", Role: "coder", Category: "error", Lifecycle: "active",
			Quality: 0.9, Content: "MySQL redo crash recovery checkpoint replay",
			Symptom: "redo checkpoint replay after crash", Tags: []string{"mysql", "redo"},
			UpdatedAt: time.Now(),
		},
		{
			ID: "exp-build", Role: "coder", Category: "general", Lifecycle: "active",
			Quality: 0.8, Content: "Vite build dist stale frontend cache",
			Symptom: "frontend sees stale dist", Tags: []string{"webapp", "build"},
			UpdatedAt: time.Now(),
		},
		// 第三条是同一查询下的弱相关候选：决策服务的 Choice 至少需要 2 个选项，
		// 只有一条命中时调用会 422，测试也就测不到"是否发了决策请求"。
		// 措辞刻意与 exp-redo 保持低重合，避免被 Jaccard 去重合并。
		{
			ID: "exp-mysql-restart", Role: "coder", Category: "error", Lifecycle: "active",
			Quality: 0.6, Content: "MySQL crash recovery redo log replay on restart",
			Symptom: "redo log replay after restart", Tags: []string{"mysql", "crash"},
			UpdatedAt: time.Now(),
		},
	}
}

// TestDualRunRecallDoesNotBlock: 双跑观察必须立即返回 BM25 结果，慢决策调用在
// 后台完成；同时 MaxRetries=-1 保证 503 只尝试一次。
func TestDualRunRecallDoesNotBlock(t *testing.T) {
	ts, attempts, hit, done := slowDecisionServer(t, 800*time.Millisecond)
	t.Setenv("CLAUDE_GO_DECISION_RECALL", "")
	t.Setenv("CLAUDE_GO_AGENTDB_URL", "")
	t.Setenv("CLAUDE_GO_DECISION_URL", ts.URL)

	ee := NewEvolutionEngine(t.TempDir(), nil)
	seedRecallExperiences(ee)

	start := time.Now()
	got := ee.RetrieveFor("coder", "MySQL redo crash recovery", 2)
	elapsed := time.Since(start)
	if elapsed > 300*time.Millisecond {
		t.Fatalf("dual-run retrieval blocked for %v (want <300ms)", elapsed)
	}
	if len(got) == 0 || got[0].ID != "exp-redo" {
		t.Fatalf("BM25 topK changed by dual-run: %+v", got)
	}

	waitSignal(t, hit, "decision call")
	waitSignal(t, done, "decision response")
	if n := atomic.LoadInt32(attempts); n != 1 {
		t.Fatalf("decision attempts = %d, want 1 (retries disabled)", n)
	}
}

// TestEnabledRecallUsesSingleAttempt: 生效模式仍需等决策，但不能再做 2 次重试。
func TestEnabledRecallUsesSingleAttempt(t *testing.T) {
	ts, attempts, _, _ := slowDecisionServer(t, 300*time.Millisecond)
	t.Setenv("CLAUDE_GO_DECISION_RECALL", "1")
	t.Setenv("CLAUDE_GO_AGENTDB_URL", "")
	t.Setenv("CLAUDE_GO_DECISION_URL", ts.URL)

	ee := NewEvolutionEngine(t.TempDir(), nil)
	seedRecallExperiences(ee)

	start := time.Now()
	got := ee.RetrieveFor("coder", "MySQL redo crash recovery", 2)
	elapsed := time.Since(start)
	if elapsed > 900*time.Millisecond {
		t.Fatalf("enabled recall took %v (retry amplification returned?)", elapsed)
	}
	if len(got) == 0 || got[0].ID != "exp-redo" {
		t.Fatalf("fail-open must return BM25 topK: %+v", got)
	}
	if n := atomic.LoadInt32(attempts); n != 1 {
		t.Fatalf("decision attempts = %d, want 1 (retries disabled)", n)
	}
}

// TestSingleCandidateSkipsDecision: 决策服务的 Choice 问题要求选项字典至少 2 项，
// 单候选请求会被 422 拒绝。客户端应在发请求前回退，不产生一次必然失败的调用。
func TestSingleCandidateSkipsDecision(t *testing.T) {
	ts, attempts, _, _ := slowDecisionServer(t, 10*time.Millisecond)
	t.Setenv("CLAUDE_GO_DECISION_RECALL", "1")
	t.Setenv("CLAUDE_GO_AGENTDB_URL", "")
	t.Setenv("CLAUDE_GO_DECISION_URL", ts.URL)

	ee := NewEvolutionEngine(t.TempDir(), nil)
	cfg := recallConfigFromEnv()
	cands := []*Experience{{ID: "exp-only", Role: "coder", Content: "only candidate"}}
	picked, judged := ee.jevRefineRecall(context.Background(), "coder", "goal", cands, 3, cfg)
	if judged || picked != nil {
		t.Fatalf("single candidate should fall back, got judged=%v picked=%v", judged, picked)
	}
	if n := atomic.LoadInt32(attempts); n != 0 {
		t.Fatalf("decision attempts = %d, want 0 (request would be rejected with 422)", n)
	}
}

// TestQualityObserverDoesNotBlock: qualityGate 的双跑打分必须立即返回。
func TestQualityObserverDoesNotBlock(t *testing.T) {
	ts, attempts, hit, done := slowDecisionServer(t, 700*time.Millisecond)
	logSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(logSrv.Close)
	t.Setenv("CLAUDE_GO_DECISION_QUALITY", "1")
	t.Setenv("CLAUDE_GO_AGENTDB_URL", logSrv.URL)
	t.Setenv("CLAUDE_GO_DECISION_URL", ts.URL)

	s := &SwarmOrchestrator{}
	start := time.Now()
	s.jevScoreOutput(
		StageResult{Status: TaskCompleted, Output: "a sufficiently long sub-task output"},
		SubTask{ID: "task-1", Role: "researcher"},
		0.7,
	)
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("jevScoreOutput blocked for %v (want <300ms)", elapsed)
	}

	waitSignal(t, hit, "quality decision call")
	waitSignal(t, done, "quality decision response")
	if n := atomic.LoadInt32(attempts); n != 1 {
		t.Fatalf("quality decision attempts = %d, want 1 (retries disabled)", n)
	}
}
