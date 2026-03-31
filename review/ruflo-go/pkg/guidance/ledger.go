package guidance

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// RunLedger records run lifecycle events for the control plane.
type RunLedger interface {
	StartRun(runID string, meta map[string]any)
	FinalizeRun(runID string, success bool, meta map[string]any)
}

// RunRecord captures a single orchestration run for auditing and metrics.
type RunRecord struct {
	RunID         string           `json:"run_id"`
	ToolsUsed     []string         `json:"tools_used,omitempty"`
	FilesModified []string         `json:"files_modified,omitempty"`
	DiffSummary   string           `json:"diff_summary,omitempty"`
	TestResults   string           `json:"test_results,omitempty"`
	Violations    []Violation      `json:"violations,omitempty"`
	Intent        TaskIntent       `json:"intent,omitempty"`
	Duration      time.Duration    `json:"duration,omitempty"`
	Success       bool             `json:"success"`
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    time.Time        `json:"finished_at,omitempty"`
	Extra         map[string]any   `json:"extra,omitempty"`
}

// StructuredRunLedger stores run records and a timeline of events per run.
type StructuredRunLedger struct {
	mu     sync.Mutex
	runs   map[string]*RunRecord
	events map[string][]RunEvent
	order  []string
}

// NewStructuredRunLedger creates an in-memory ledger used by the control plane.
func NewStructuredRunLedger() *StructuredRunLedger {
	return &StructuredRunLedger{
		runs:   make(map[string]*RunRecord),
		events: make(map[string][]RunEvent),
	}
}

// StartRun opens or continues a run record.
func (l *StructuredRunLedger) StartRun(runID string, meta map[string]any) {
	if l == nil || runID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.runs[runID]
	if !ok {
		rec = &RunRecord{RunID: runID, StartedAt: time.Now().UTC()}
		l.runs[runID] = rec
		l.order = append(l.order, runID)
	}
	mergeMetaIntoRecord(rec, meta)
	l.events[runID] = append(l.events[runID], RunEvent{Type: "start", Timestamp: time.Now().UTC(), Payload: meta})
}

// FinalizeRun closes a run and merges completion metadata.
func (l *StructuredRunLedger) FinalizeRun(runID string, success bool, meta map[string]any) {
	if l == nil || runID == "" {
		return
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["success"] = success
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := l.runs[runID]
	if rec == nil {
		rec = &RunRecord{RunID: runID, StartedAt: time.Now().UTC()}
		l.runs[runID] = rec
	}
	rec.Success = success
	rec.FinishedAt = time.Now().UTC()
	if !rec.StartedAt.IsZero() {
		rec.Duration = rec.FinishedAt.Sub(rec.StartedAt)
	}
	mergeMetaIntoRecord(rec, meta)
	l.events[runID] = append(l.events[runID], RunEvent{Type: "finalize", Timestamp: rec.FinishedAt, Payload: meta})
}

// GetRecord returns a snapshot of the run (nil if unknown).
func (l *StructuredRunLedger) GetRecord(runID string) *RunRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.runs[runID]
	if r == nil {
		return nil
	}
	cp := *r
	cp.ToolsUsed = append([]string(nil), r.ToolsUsed...)
	cp.FilesModified = append([]string(nil), r.FilesModified...)
	cp.Violations = append([]Violation(nil), r.Violations...)
	if r.Extra != nil {
		cp.Extra = make(map[string]any, len(r.Extra))
		for k, v := range r.Extra {
			cp.Extra[k] = v
		}
	}
	return &cp
}

// Events returns timeline entries for a run id.
func (l *StructuredRunLedger) Events(runID string) []RunEvent {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]RunEvent(nil), l.events[runID]...)
}

func mergeMetaIntoRecord(rec *RunRecord, meta map[string]any) {
	if rec == nil || len(meta) == 0 {
		return
	}
	if v, ok := meta["tools_used"].([]string); ok {
		rec.ToolsUsed = v
	} else if v, ok := meta["tools"].([]any); ok {
		for _, x := range v {
			rec.ToolsUsed = append(rec.ToolsUsed, stringifyAny(x))
		}
	}
	if v, ok := meta["files_modified"].([]string); ok {
		rec.FilesModified = v
	} else if v, ok := meta["files"].([]any); ok {
		for _, x := range v {
			rec.FilesModified = append(rec.FilesModified, stringifyAny(x))
		}
	}
	if s, ok := meta["diff_summary"].(string); ok {
		rec.DiffSummary = s
	}
	if s, ok := meta["test_results"].(string); ok {
		rec.TestResults = s
	}
	if s, ok := meta["intent"].(string); ok {
		rec.Intent = TaskIntent(s)
	}
	if vv, ok := meta["violations"].([]Violation); ok {
		rec.Violations = vv
	}
	if rec.Extra == nil {
		rec.Extra = make(map[string]any)
	}
	for k, v := range meta {
		switch k {
		case "tools_used", "files_modified", "diff_summary", "test_results", "intent", "violations", "tools", "files", "success":
			continue
		default:
			rec.Extra[k] = v
		}
	}
}

func stringifyAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

// TestsPassEvaluator interprets test result strings on run records.
type TestsPassEvaluator struct{}

// Evaluate returns true when the record indicates tests passed.
func (TestsPassEvaluator) Evaluate(rec *RunRecord) bool {
	if rec == nil {
		return false
	}
	s := rec.TestResults
	if s == "" {
		return rec.Success
	}
	low := strings.ToLower(s)
	return rec.Success && (containsAny(low, []string{"pass", "ok", "success"}) && !containsAny(low, []string{"fail", "error"}))
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// RankViolations groups by rule id, scores frequency×severity weight, and sorts descending.
func RankViolations(violations []Violation) []Violation {
	type agg struct {
		v     Violation
		count int
		score float64
	}
	byRule := map[string]*agg{}
	for _, v := range violations {
		key := v.RuleID
		if key == "" {
			key = v.Message
		}
		a := byRule[key]
		if a == nil {
			vv := v
			a = &agg{v: vv, count: 0}
			byRule[key] = a
		}
		a.count++
	}
	var keys []string
	for k := range byRule {
		keys = append(keys, k)
	}
	for _, k := range keys {
		a := byRule[k]
		a.score = float64(a.count) * severityWeight(a.v.Severity)
	}
	sort.Slice(keys, func(i, j int) bool {
		return byRule[keys[i]].score > byRule[keys[j]].score
	})
	out := make([]Violation, 0, len(keys))
	for _, k := range keys {
		v := byRule[k].v
		v.Message = strings.TrimSpace(v.Message)
		out = append(out, v)
	}
	return out
}

func severityWeight(r RiskClass) float64 {
	switch r {
	case RiskCritical:
		return 5
	case RiskHigh:
		return 4
	case RiskMedium:
		return 3
	case RiskLow:
		return 2
	case RiskInfo:
		return 1
	default:
		return 1
	}
}

// LedgerMetrics summarizes ledger contents.
type LedgerMetrics struct {
	TotalRuns          int     `json:"total_runs"`
	SuccessRate        float64 `json:"success_rate"`
	AvgDuration        float64 `json:"avg_duration_sec"`
	ViolationFrequency float64 `json:"violation_frequency"`
}

// ComputeMetrics aggregates success rate, average duration, and violation frequency across stored runs.
func (l *StructuredRunLedger) ComputeMetrics() LedgerMetrics {
	if l == nil {
		return LedgerMetrics{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var ok, n int
	var durSum time.Duration
	var violN int
	for _, id := range l.order {
		r := l.runs[id]
		if r == nil {
			continue
		}
		n++
		if r.Success {
			ok++
		}
		durSum += r.Duration
		violN += len(r.Violations)
	}
	m := LedgerMetrics{TotalRuns: n}
	if n > 0 {
		m.SuccessRate = float64(ok) / float64(n)
		m.AvgDuration = durSum.Seconds() / float64(n)
		m.ViolationFrequency = float64(violN) / float64(n)
	}
	return m
}
