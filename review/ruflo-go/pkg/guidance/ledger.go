package guidance

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// 本文件：运行账本（结构化审计日志）。StructuredRunLedger 内存存储 RunRecord 与按 runID 时间线事件；
// mergeMetaIntoRecord 将控制面 meta 映射到记录字段；RankViolations 按频次×严重度权重排序违规；ComputeMetrics 聚合成功率等。

// RunLedger 运行生命周期记录接口（可由 StructuredRunLedger 实现）。
type RunLedger interface {
	StartRun(runID string, meta map[string]any)                  // 开始或继续一次 run
	FinalizeRun(runID string, success bool, meta map[string]any) // 结束 run 并写入结果
}

// RunRecord 单次编排运行的审计快照，供合规检查与指标计算使用。
type RunRecord struct {
	RunID         string         `json:"run_id"`                   // 运行唯一 ID
	ToolsUsed     []string       `json:"tools_used,omitempty"`     // 使用过的工具列表
	FilesModified []string       `json:"files_modified,omitempty"` // 修改过的文件
	DiffSummary   string         `json:"diff_summary,omitempty"`   // 变更摘要
	TestResults   string         `json:"test_results,omitempty"`   // 测试结果文本
	Violations    []Violation    `json:"violations,omitempty"`     // 关联违规
	Intent        TaskIntent     `json:"intent,omitempty"`         // 任务意图
	Duration      time.Duration  `json:"duration,omitempty"`       // 持续时间
	Success       bool           `json:"success"`                  // 是否成功
	StartedAt     time.Time      `json:"started_at"`               // 开始时间
	FinishedAt    time.Time      `json:"finished_at,omitempty"`    // 结束时间
	Extra         map[string]any `json:"extra,omitempty"`          // 其它元数据
}

// StructuredRunLedger 内存账本：runs 主表、events 时间线、order 插入顺序用于指标遍历。
type StructuredRunLedger struct {
	mu     sync.Mutex            // 保护 runs、events、order
	runs   map[string]*RunRecord // runID -> 记录
	events map[string][]RunEvent // runID -> 事件切片
	order  []string              // runID 首次出现顺序
}

// NewStructuredRunLedger 创建空内存账本。
func NewStructuredRunLedger() *StructuredRunLedger {
	return &StructuredRunLedger{
		runs:   make(map[string]*RunRecord),
		events: make(map[string][]RunEvent),
	}
}

// StartRun 若 run 不存在则创建并记录 order；合并 meta；追加 type=start 事件。
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

// FinalizeRun 设置 Success、FinishedAt、Duration，合并 meta（含 success 键），追加 finalize 事件。
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

// GetRecord 返回 run 的深拷贝快照（切片与 Extra map 拷贝）；未知返回 nil。
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

// Events 返回指定 run 的事件时间线副本。
func (l *StructuredRunLedger) Events(runID string) []RunEvent {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]RunEvent(nil), l.events[runID]...)
}

// mergeMetaIntoRecord 将控制面 meta 键映射到 RunRecord 已知字段，其余落入 Extra。
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

// stringifyAny 将任意值转为字符串（当前仅 string 有值）。
func stringifyAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

// TestsPassEvaluator 根据 TestResults 文本解释测试是否通过。
type TestsPassEvaluator struct{}

// Evaluate：无 TestResults 时退回 rec.Success；否则要求 Success 且文本含 pass/ok/success 且不含 fail/error。
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

// containsAny 判断 s 是否包含 subs 中任一字串。
func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// RankViolations 按 RuleID（空则用 Message）分组，score = 出现次数 × severityWeight(Severity)，按 score 降序输出每组代表 Violation。
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

// severityWeight 将风险等级映射为数值权重（用于 RankViolations）。
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

// LedgerMetrics 账本聚合指标。
type LedgerMetrics struct {
	TotalRuns          int     `json:"total_runs"`          // 运行总数
	SuccessRate        float64 `json:"success_rate"`        // 成功占比
	AvgDuration        float64 `json:"avg_duration_sec"`    // 平均耗时（秒）
	ViolationFrequency float64 `json:"violation_frequency"` // 每条 run 平均违规条数
}

// ComputeMetrics 按 order 遍历 runs：统计成功率、平均 Duration、违规总数/运行数。
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
