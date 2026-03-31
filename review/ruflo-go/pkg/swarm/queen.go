package swarm

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// QueenMode selects how the queen resolves collective decisions.
type QueenMode string

const (
	QueenOverride   QueenMode = "queen-override"
	QueenUnanimous  QueenMode = "unanimous"
	QueenSuperMajor QueenMode = "supermajority"
	QueenMajority   QueenMode = "majority"
	QueenWeighted   QueenMode = "weighted"
)

// OutcomeRecord is one queen-tracked task outcome for analytics.
type OutcomeRecord struct {
	Task       api.TaskDefinition
	PatternKey string
	Success    bool
	RecordedAt time.Time
}

// QueenCoordinator performs hierarchical task decomposition and delegation.
type QueenCoordinator struct {
	mu sync.RWMutex

	agents   map[string]*api.Agent
	history  []api.TaskDefinition
	patterns map[string]int

	outcomes         []OutcomeRecord
	lastHealthReport map[string]any
	initialized      bool

	consensusFn func(ctx context.Context, mode QueenMode, weights map[string]float64, votes map[string]bool) (bool, error)
}

// NewQueenCoordinator constructs a queen with optional consensus hook.
func NewQueenCoordinator(consensusFn func(ctx context.Context, mode QueenMode, weights map[string]float64, votes map[string]bool) (bool, error)) *QueenCoordinator {
	if consensusFn == nil {
		consensusFn = defaultQueenConsensus
	}
	return &QueenCoordinator{
		agents:           make(map[string]*api.Agent),
		history:          make([]api.TaskDefinition, 0),
		patterns:         make(map[string]int),
		outcomes:         make([]OutcomeRecord, 0),
		lastHealthReport: make(map[string]any),
		consensusFn:      consensusFn,
	}
}

// Initialize marks the queen ready for coordination (idempotent).
func (q *QueenCoordinator) Initialize(ctx context.Context) error {
	_ = ctx
	q.mu.Lock()
	defer q.mu.Unlock()
	q.initialized = true
	if q.lastHealthReport == nil {
		q.lastHealthReport = make(map[string]any)
	}
	return nil
}

// Shutdown clears runtime state; consensus hook is left intact.
func (q *QueenCoordinator) Shutdown() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.initialized = false
	q.agents = make(map[string]*api.Agent)
	return nil
}

// GetLastHealthReport returns the most recent bottleneck scan summary.
func (q *QueenCoordinator) GetLastHealthReport() map[string]any {
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make(map[string]any, len(q.lastHealthReport))
	for k, v := range q.lastHealthReport {
		out[k] = v
	}
	return out
}

// GetOutcomeHistory returns recorded outcomes (newest last).
func (q *QueenCoordinator) GetOutcomeHistory() []OutcomeRecord {
	q.mu.RLock()
	defer q.mu.RUnlock()
	cp := make([]OutcomeRecord, len(q.outcomes))
	copy(cp, q.outcomes)
	return cp
}

// GetPerformanceStats summarizes pattern usage and swarm size.
func (q *QueenCoordinator) GetPerformanceStats() map[string]any {
	q.mu.RLock()
	defer q.mu.RUnlock()
	stats := map[string]any{
		"agents":        len(q.agents),
		"history_len":   len(q.history),
		"outcomes_len":  len(q.outcomes),
		"patterns_keys": len(q.patterns),
	}
	return stats
}

// RegisterAgent adds an agent visible to the queen.
func (q *QueenCoordinator) RegisterAgent(a *api.Agent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.agents[a.ID] = a
}

// AnalyzeTask decomposes a task into subtasks by type.
func (q *QueenCoordinator) AnalyzeTask(parent *api.TaskDefinition) []*api.TaskDefinition {
	if parent == nil {
		return nil
	}
	base := parent.ID + "-sub"
	switch parent.Type {
	case api.TaskTypeCoding:
		return []*api.TaskDefinition{
			{ID: base + "-design", Type: api.TaskTypeResearch, Status: api.TaskStatusPending, Priority: parent.Priority, Labels: cloneLabels(parent.Labels), CreatedAt: time.Now(), UpdatedAt: time.Now()},
			{ID: base + "-impl", Type: api.TaskTypeCoding, Status: api.TaskStatusPending, Priority: parent.Priority, Labels: cloneLabels(parent.Labels), CreatedAt: time.Now(), UpdatedAt: time.Now()},
			{ID: base + "-test", Type: api.TaskTypeTesting, Status: api.TaskStatusPending, Priority: parent.Priority, Labels: cloneLabels(parent.Labels), CreatedAt: time.Now(), UpdatedAt: time.Now()},
		}
	case api.TaskTypeTesting, api.TaskTypeTest:
		return []*api.TaskDefinition{
			{ID: base + "-plan", Type: api.TaskTypeResearch, Status: api.TaskStatusPending, Priority: parent.Priority, Labels: cloneLabels(parent.Labels), CreatedAt: time.Now(), UpdatedAt: time.Now()},
			{ID: base + "-run", Type: api.TaskTypeTesting, Status: api.TaskStatusPending, Priority: parent.Priority, Labels: cloneLabels(parent.Labels), CreatedAt: time.Now(), UpdatedAt: time.Now()},
		}
	case api.TaskTypeSecurity:
		return []*api.TaskDefinition{
			{ID: base + "-threat", Type: api.TaskTypeResearch, Status: api.TaskStatusPending, Priority: parent.Priority, Labels: cloneLabels(parent.Labels), CreatedAt: time.Now(), UpdatedAt: time.Now()},
			{ID: base + "-scan", Type: api.TaskTypeSecurity, Status: api.TaskStatusPending, Priority: parent.Priority, Labels: cloneLabels(parent.Labels), CreatedAt: time.Now(), UpdatedAt: time.Now()},
		}
	default:
		return []*api.TaskDefinition{
			{ID: base + "-work", Type: parent.Type, Status: api.TaskStatusPending, Priority: parent.Priority, Labels: cloneLabels(parent.Labels), CreatedAt: time.Now(), UpdatedAt: time.Now()},
		}
	}
}

func cloneLabels(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// DelegateToAgents ranks agents: capability 0.30, load 0.20, performance 0.25, health 0.15, availability 0.10.
func (q *QueenCoordinator) DelegateToAgents(task *api.TaskDefinition, candidates []string) ([]string, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if task == nil {
		return nil, fmt.Errorf("queen: nil task")
	}
	type scored struct {
		id    string
		score float64
	}
	var out []scored
	for _, id := range candidates {
		ag, ok := q.agents[id]
		if !ok {
			continue
		}
		capScore := matchCapabilityScore(ag, task)
		loadScore := 1.0 - math.Min(1.0, agentLoad(ag))
		perf := performanceScore(ag)
		health := agentHealth(ag)
		avail := availabilityScore(ag)
		s := capScore*0.30 + loadScore*0.20 + perf*0.25 + health*0.15 + avail*0.10
		out = append(out, scored{id: id, score: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })
	ids := make([]string, 0, len(out))
	for _, s := range out {
		ids = append(ids, s.id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("queen: no candidates")
	}
	return ids, nil
}

func matchCapabilityScore(ag *api.Agent, task *api.TaskDefinition) float64 {
	ts := string(task.Type)
	for _, c := range agentCapabilities(ag) {
		if c == ts {
			return 1.0
		}
	}
	if ag.Domain == api.AgentDomainCore && (task.Type == api.TaskTypeCoding || task.Type == api.TaskTypeTesting || task.Type == api.TaskTypeTest) {
		return 0.6
	}
	return 0.2
}

func performanceScore(ag *api.Agent) float64 {
	t := float64(ag.Metrics.TasksCompleted + ag.Metrics.TasksFailed)
	if t == 0 {
		return 0.5
	}
	return float64(ag.Metrics.TasksCompleted) / t
}

func availabilityScore(ag *api.Agent) float64 {
	switch ag.State {
	case api.AgentStateIdle:
		return 1.0
	case api.AgentStateBusy, api.AgentStateRunning:
		return 0.5
	default:
		return 0.1
	}
}

// MonitorHealth runs on interval; returns alerts for bottlenecks (blocking until ctx done).
func (q *QueenCoordinator) MonitorHealth(ctx context.Context, interval time.Duration) []string {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var alerts []string
	for {
		select {
		case <-ctx.Done():
			return alerts
		case <-ticker.C:
			alerts = q.scanBottlenecks()
		}
	}
}

func (q *QueenCoordinator) scanBottlenecks() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var alerts []string
	busy := 0
	for _, ag := range q.agents {
		if ag.State == api.AgentStateBusy || ag.State == api.AgentStateRunning {
			busy++
		}
		if agentLoad(ag) > 0.85 {
			alerts = append(alerts, fmt.Sprintf("high-load agent %s load=%.2f", ag.ID, agentLoad(ag)))
		}
	}
	if len(q.agents) > 0 && float64(busy)/float64(len(q.agents)) > 0.85 {
		alerts = append(alerts, "swarm bottleneck: >85% agents busy")
	}
	q.lastHealthReport["alerts"] = append([]string(nil), alerts...)
	q.lastHealthReport["alert_count"] = len(alerts)
	q.lastHealthReport["busy_agents"] = busy
	q.lastHealthReport["total_agents"] = len(q.agents)
	q.lastHealthReport["ts"] = time.Now().UTC()
	return alerts
}

// CoordinateConsensus applies queen-selected aggregation mode.
func (q *QueenCoordinator) CoordinateConsensus(ctx context.Context, mode QueenMode, weights map[string]float64, votes map[string]bool) (bool, error) {
	return q.consensusFn(ctx, mode, weights, votes)
}

func defaultQueenConsensus(_ context.Context, mode QueenMode, weights map[string]float64, votes map[string]bool) (bool, error) {
	switch mode {
	case QueenOverride:
		return true, nil
	case QueenUnanimous:
		for _, v := range votes {
			if !v {
				return false, nil
			}
		}
		return len(votes) > 0, nil
	case QueenSuperMajor:
		return ratioApproved(votes) >= 0.66, nil
	case QueenMajority:
		return ratioApproved(votes) > 0.5, nil
	case QueenWeighted:
		var num, den float64
		for id, w := range weights {
			den += w
			if votes[id] {
				num += w
			}
		}
		if den == 0 {
			return false, nil
		}
		return num/den > 0.5, nil
	default:
		return ratioApproved(votes) > 0.5, nil
	}
}

func ratioApproved(votes map[string]bool) float64 {
	if len(votes) == 0 {
		return 0
	}
	n := 0
	for _, v := range votes {
		if v {
			n++
		}
	}
	return float64(n) / float64(len(votes))
}

// RecordOutcome appends history and increments pattern key frequency.
func (q *QueenCoordinator) RecordOutcome(task api.TaskDefinition, patternKey string, success bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.history = append(q.history, task)
	q.outcomes = append(q.outcomes, OutcomeRecord{
		Task:       task,
		PatternKey: patternKey,
		Success:    success,
		RecordedAt: time.Now().UTC(),
	})
	if patternKey != "" {
		q.patterns[patternKey]++
	}
	if success {
		q.patterns[patternKey+":ok"]++
	}
}

// HistorySnapshot returns recent tasks up to n.
func (q *QueenCoordinator) HistorySnapshot(n int) []api.TaskDefinition {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if n <= 0 || n > len(q.history) {
		n = len(q.history)
	}
	start := len(q.history) - n
	if start < 0 {
		start = 0
	}
	slice := q.history[start:]
	out := make([]api.TaskDefinition, len(slice))
	copy(out, slice)
	return out
}
