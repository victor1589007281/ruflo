// 蜂后协调器（swarm 包）：类比蜂群中的 Queen，负责任务类型驱动的子任务拆解、候选 Agent 多因子加权排序、
// 周期性瓶颈扫描与共识模式（全票/多数/加权等）聚合；可注入 consensusFn 自定义投票结算逻辑。
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

// QueenMode 蜂后聚合投票时的策略枚举。
type QueenMode string

const (
	QueenOverride   QueenMode = "queen-override" // 强制通过（忽略票型）
	QueenUnanimous  QueenMode = "unanimous"      // 全票赞成
	QueenSuperMajor QueenMode = "supermajority"  // ≥66% 赞成
	QueenMajority   QueenMode = "majority"       // >50% 赞成
	QueenWeighted   QueenMode = "weighted"       // 按权重比例
)

// OutcomeRecord 单次任务结果审计：关联模式键与成功标记。
type OutcomeRecord struct {
	Task       api.TaskDefinition // 任务快照
	PatternKey string             // 学习/模式索引键
	Success    bool               // 是否成功
	RecordedAt time.Time          // UTC 记录时间
}

// QueenCoordinator 维护 agents 视图、历史任务、模式计数与健康摘要。
type QueenCoordinator struct {
	mu sync.RWMutex

	agents   map[string]*api.Agent // id -> Agent
	history  []api.TaskDefinition  // 时间序任务历史
	patterns map[string]int        // 模式键出现频次

	outcomes         []OutcomeRecord // 结构化结果轨迹
	lastHealthReport map[string]any  // 最近一次瓶颈扫描摘要
	initialized      bool            // Initialize 幂等标记

	consensusFn func(ctx context.Context, mode QueenMode, weights map[string]float64, votes map[string]bool) (bool, error) // 可插拔结算
}

// NewQueenCoordinator consensusFn 为空时使用 defaultQueenConsensus。
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

// Initialize 置 initialized 并确保 lastHealthReport 非 nil。
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

// Shutdown 清空 agents 与 initialized；不替换 consensusFn。
func (q *QueenCoordinator) Shutdown() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.initialized = false
	q.agents = make(map[string]*api.Agent)
	return nil
}

// GetLastHealthReport 浅拷贝 map 供外部只读遍历。
func (q *QueenCoordinator) GetLastHealthReport() map[string]any {
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make(map[string]any, len(q.lastHealthReport))
	for k, v := range q.lastHealthReport {
		out[k] = v
	}
	return out
}

// GetOutcomeHistory 拷贝 outcomes 切片。
func (q *QueenCoordinator) GetOutcomeHistory() []OutcomeRecord {
	q.mu.RLock()
	defer q.mu.RUnlock()
	cp := make([]OutcomeRecord, len(q.outcomes))
	copy(cp, q.outcomes)
	return cp
}

// GetPerformanceStats 返回规模与 pattern 键数量等粗粒度指标。
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

// RegisterAgent 以 Agent.ID 为主键覆盖写入。
func (q *QueenCoordinator) RegisterAgent(a *api.Agent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.agents[a.ID] = a
}

// AnalyzeTask 按父任务类型生成子任务 DAG 的扁平列表（固定模板：编码/测试/安全等分支）。
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

// cloneLabels 深拷贝标签 map。
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

// DelegateToAgents 对 candidates 计算加权分：能力0.30、负载0.20、绩效0.25、健康0.15、可用性0.10，降序返回 id。
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

// matchCapabilityScore 精确技能匹配 1.0，域级弱匹配 0.6，否则 0.2。
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

// performanceScore 用完成任务占比估计成功率，无历史返回 0.5。
func performanceScore(ag *api.Agent) float64 {
	t := float64(ag.Metrics.TasksCompleted + ag.Metrics.TasksFailed)
	if t == 0 {
		return 0.5
	}
	return float64(ag.Metrics.TasksCompleted) / t
}

// availabilityScore Idle=1，Busy/Running=0.5，其余 0.1。
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

// MonitorHealth 定时调用 scanBottlenecks，ctx 取消时返回累积 alerts（阻塞型循环）。
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

// scanBottlenecks 统计高负载 Agent 与全局忙碌比例，写入 lastHealthReport。
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

// CoordinateConsensus 转调注入的 consensusFn。
func (q *QueenCoordinator) CoordinateConsensus(ctx context.Context, mode QueenMode, weights map[string]float64, votes map[string]bool) (bool, error) {
	return q.consensusFn(ctx, mode, weights, votes)
}

// defaultQueenConsensus 内置 QueenOverride/全票/超多数/简单多数/加权多数逻辑。
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

// ratioApproved 赞成票占比。
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

// RecordOutcome 追加 history/outcomes，并 patterns[patternKey]++，成功额外 patterns[patternKey+":ok"]++。
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

// HistorySnapshot 取 history 尾部 n 条（n 非法时取全长）。
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
