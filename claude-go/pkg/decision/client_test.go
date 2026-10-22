package decision

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newDecideServer 起一个返回固定 answers 的假 Ollaya 端点。
func newDecideServer(t *testing.T, answers map[string]Answer) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/decide" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req decideRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{Answers: answers, Model: req.Model})
	}))
}

func TestDecideChoiceParsesDistribution(t *testing.T) {
	srv := newDecideServer(t, map[string]Answer{
		"pick": {
			Choice:        "b",
			Confidence:    0.81,
			Probabilities: map[string]float64{"a": 0.19, "b": 0.81},
		},
	})
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	resp, err := c.Decide(context.Background(), "snap:2b", "some state", []Question{{
		ID: "pick", Type: TypeChoice, Options: map[string]string{"a": "甲", "b": "乙"},
	}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	ans := resp.Answers["pick"]
	if ans.Choice != "b" || ans.Confidence != 0.81 {
		t.Fatalf("unexpected answer: %+v", ans)
	}
	if ans.Probabilities["b"] != 0.81 {
		t.Fatalf("probabilities not parsed: %+v", ans.Probabilities)
	}
}

func TestDecideScoreParsesLegendAndScore(t *testing.T) {
	srv := newDecideServer(t, map[string]Answer{
		"q": {
			Score:         3.25,
			Confidence:    0.6,
			Legend:        map[string]string{"0": "差", "4": "优"},
			Probabilities: map[string]float64{"0": 0.0, "3": 0.75, "4": 0.25},
		},
	})
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	resp, err := c.Decide(context.Background(), "jeb:4b", "state", []Question{{
		ID: "q", Type: TypeScore, Legend: []string{"差", "弱", "中", "良", "优"},
	}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	ans := resp.Answers["q"]
	if ans.Score != 3.25 || ans.Legend["4"] != "优" {
		t.Fatalf("unexpected score answer: %+v", ans)
	}
}

func TestDecideRetriesOn503(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 { // 第一次 503 QUEUE_FULL，第二次成功
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(apiError{Err: "queue full", Code: "QUEUE_FULL"})
			return
		}
		_ = json.NewEncoder(w).Encode(Response{Answers: map[string]Answer{"pick": {Choice: "a", Confidence: 0.9}}})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, MaxRetries: 3}
	resp, err := c.Decide(context.Background(), "snap:2b", "s", []Question{{ID: "pick", Type: TypeChoice, Options: map[string]string{"a": "甲", "b": "乙"}}})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if resp.Answers["pick"].Choice != "a" {
		t.Fatalf("unexpected: %+v", resp.Answers)
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatalf("expected at least 2 calls (retry), got %d", calls)
	}
}

func TestDecideNoRetryOn422(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(apiError{Err: "state too long", Code: "INPUT_TOO_LONG"})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, MaxRetries: 5}
	_, err := c.Decide(context.Background(), "snap:2b", "s", []Question{{ID: "pick", Type: TypeChoice, Options: map[string]string{"a": "甲", "b": "乙"}}})
	if err == nil {
		t.Fatal("expected error on 422")
	}
	var de *Error
	if !errorAs(err, &de) || de.Retryable {
		t.Fatalf("422 should be non-retryable, got %+v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("422 must not be retried; calls=%d", got)
	}
}

func TestDecideNetworkErrorRetryable(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1", Timeout: 500 * time.Millisecond, MaxRetries: 1}
	_, err := c.Decide(context.Background(), "snap:2b", "s", []Question{{ID: "pick", Type: TypeChoice, Options: map[string]string{"a": "甲", "b": "乙"}}})
	if err == nil {
		t.Fatal("expected network error")
	}
	var de *Error
	if !errorAs(err, &de) || !de.Retryable {
		t.Fatalf("network error should be retryable, got %+v", err)
	}
}

func TestDecideValidation(t *testing.T) {
	c := &Client{}
	if _, err := c.Decide(context.Background(), "", "s", []Question{{ID: "x"}}); err == nil {
		t.Fatal("empty model should error")
	}
	if _, err := c.Decide(context.Background(), "m", "s", nil); err == nil {
		t.Fatal("empty questions should error")
	}
}

func TestLoggerDisabledNoPanic(t *testing.T) {
	var l *Logger // nil logger
	if err := l.Log(context.Background(), Record{Kind: "recall"}); err != nil {
		t.Fatalf("nil logger should be no-op, got %v", err)
	}
	l2 := &Logger{BaseURL: ""} // empty base
	if err := l2.Log(context.Background(), Record{Kind: "recall"}); err != nil {
		t.Fatalf("empty-base logger should be no-op, got %v", err)
	}
	l3 := &Logger{BaseURL: "http://x", Disabled: true}
	if err := l3.Log(context.Background(), Record{Kind: "recall"}); err != nil {
		t.Fatalf("disabled logger should be no-op, got %v", err)
	}
}

func TestLoggerAppendsToAgentDBShape(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	l := NewLogger(srv.URL)
	if err := l.Log(context.Background(), Record{Kind: "quality_gate", Model: "jeb:4b", Extra: map[string]any{"length_conf": 0.9}}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if gotPath != "/v1/log/decision/append" {
		t.Fatalf("wrong path: %s", gotPath)
	}
	var rec Record
	if err := json.Unmarshal(gotBody, &rec); err != nil {
		t.Fatalf("body not a Record: %v (%s)", err, gotBody)
	}
	if rec.Kind != "quality_gate" || rec.Model != "jeb:4b" || rec.At.IsZero() {
		t.Fatalf("unexpected record: %+v", rec)
	}
}
