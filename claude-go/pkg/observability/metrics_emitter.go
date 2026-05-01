package observability

import (
	"log"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/metrics"
)

// MetricsEmitter 将 observability 事件桥接到现有 metrics.Collector + Prometheus。
// 实现所有 6 个 Hook 接口, 作为 Bus subscriber 的统一出口。
type MetricsEmitter struct {
	collector *metrics.Collector
}

// NewMetricsEmitter 创建指标发射器, 若 collector 为 nil 则尝试使用全局 LLM collector。
func NewMetricsEmitter(c *metrics.Collector) *MetricsEmitter {
	if c == nil {
		c = metrics.GlobalLLMCollector()
	}
	return &MetricsEmitter{collector: c}
}

var _ LLMHook = (*MetricsEmitter)(nil)
var _ StageHook = (*MetricsEmitter)(nil)
var _ TaskHook = (*MetricsEmitter)(nil)
var _ TeamHook = (*MetricsEmitter)(nil)
var _ CollaborationHook = (*MetricsEmitter)(nil)
var _ PromptHook = (*MetricsEmitter)(nil)

// ─── LLMHook ────────────────────────────────────────────────────────────────

func (m *MetricsEmitter) OnLLMCallStart(ev LLMCallEvent) {
	// start 事件不上报 metrics, 只留 trace
}

func (m *MetricsEmitter) OnLLMCallComplete(ev LLMCallEvent) {
	if m.collector == nil {
		return
	}
	rec := ev.Record
	labels := m.llmLabels(ev)
	m.collector.RecordWithLabels("llm", metrics.MLLMCallCount, 1, labels)
	m.collector.RecordWithLabels("llm", metrics.MLLMSuccessCount, 1, labels)
	m.collector.RecordWithLabels("llm", metrics.MLLMDurationSec, rec.DurationSec, labels)
	if rec.InputTokens > 0 {
		m.collector.RecordWithLabels("llm", metrics.MLLMInputTokens, float64(rec.InputTokens), labels)
	}
	if rec.OutputTokens > 0 {
		m.collector.RecordWithLabels("llm", metrics.MLLMOutputTokens, float64(rec.OutputTokens), labels)
	}
	if rec.CacheReadTokens > 0 {
		m.collector.RecordWithLabels("llm", metrics.MLLMCacheReadTokens, float64(rec.CacheReadTokens), labels)
	}
	if rec.CacheCreationTokens > 0 {
		m.collector.RecordWithLabels("llm", metrics.MLLMCacheCreateTokens, float64(rec.CacheCreationTokens), labels)
	}
	if rec.TotalTokens > 0 {
		m.collector.RecordWithLabels("llm", metrics.MLLMTotalTokens, float64(rec.TotalTokens), labels)
	}
	if rec.Retries > 0 {
		m.collector.RecordWithLabels("llm", metrics.MLLMRetryCount, float64(rec.Retries), labels)
	}
}

func (m *MetricsEmitter) OnLLMCallError(ev LLMCallEvent) {
	if m.collector == nil {
		return
	}
	rec := ev.Record
	labels := m.llmLabels(ev)
	m.collector.RecordWithLabels("llm", metrics.MLLMCallCount, 1, labels)
	m.collector.RecordWithLabels("llm", metrics.MLLMErrorCount, 1, labels)
	m.collector.RecordWithLabels("llm", metrics.MLLMDurationSec, rec.DurationSec, labels)
	switch rec.ErrorKind {
	case "rate_limit":
		m.collector.RecordWithLabels("llm", metrics.MLLMRateLimitCount, 1, labels)
	case "overloaded":
		m.collector.RecordWithLabels("llm", metrics.MLLMOverloadCount, 1, labels)
	case "timeout":
		m.collector.RecordWithLabels("llm", metrics.MLLMTimeoutCount, 1, labels)
	case "refusal":
		m.collector.RecordWithLabels("llm", metrics.MLLMRefusalCount, 1, labels)
	case "prompt_too_long":
		m.collector.RecordWithLabels("llm", metrics.MLLMPromptTooLong, 1, labels)
	}
	if rec.CircuitOpened {
		m.collector.RecordWithLabels("llm", metrics.MLLMCircuitTrips, 1, labels)
	}
}

func (m *MetricsEmitter) OnLLMCallFallback(primary, fallback string, ev LLMCallEvent) {
	if m.collector == nil {
		return
	}
	labels := m.llmLabels(ev)
	labels["fallback_from"] = primary
	labels["fallback_to"] = fallback
	m.collector.RecordWithLabels("llm", metrics.MLLMRetryCount, 1, labels)
}

func (m *MetricsEmitter) OnLLMCacheHit(model, cacheType string, tokens int) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"model": model, "cache_type": cacheType}
	m.collector.RecordWithLabels("llm", metrics.MLLMCacheReadTokens, float64(tokens), labels)
}

func (m *MetricsEmitter) OnLLMCacheMiss(model, cacheType string, tokens int) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"model": model, "cache_type": cacheType}
	m.collector.RecordWithLabels("llm", metrics.MLLMCacheCreateTokens, float64(tokens), labels)
}

func (m *MetricsEmitter) llmLabels(ev LLMCallEvent) map[string]string {
	rec := ev.Record
	labels := map[string]string{
		"model":       firstNonEmpty(rec.Model, "unknown"),
		"model_alias": firstNonEmpty(rec.Model, "unknown"),
		"status":      rec.Status,
		"source":      firstNonEmpty(rec.Source, "unknown"),
		"purpose":     firstNonEmpty(rec.Purpose, ""),
		"stop_reason": firstNonEmpty(rec.StopReason, ""),
		"error_kind":  firstNonEmpty(rec.ErrorKind, ""),
	}
	if ev.TeamID != "" {
		labels["team_id"] = ev.TeamID
	}
	if ev.StageName != "" {
		labels["stage"] = ev.StageName
	}
	if ev.AgentRole != "" {
		labels["role"] = ev.AgentRole
	}
	return labels
}

// ─── StageHook ──────────────────────────────────────────────────────────────

func (m *MetricsEmitter) OnStageStart(ev StageEvent) {}

func (m *MetricsEmitter) OnStageComplete(ev StageEvent) {
	if m.collector == nil {
		return
	}
	labels := m.teamLabels(ev.TeamID, ev.Workflow, ev.StageName, ev.AgentRole)
	m.collector.RecordWithLabels("team", metrics.MTeamStageCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamStageSuccessCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamStageDurationSec, ev.DurationSec, labels)
	if ev.OutputLen > 0 {
		m.collector.RecordWithLabels("team", metrics.MTeamStageOutputLen, float64(ev.OutputLen), labels)
	}
	if ev.ToolCallCount > 0 {
		m.collector.RecordWithLabels("team", metrics.MTeamStageToolCalls, float64(ev.ToolCallCount), labels)
	}
}

func (m *MetricsEmitter) OnStageFail(ev StageEvent) {
	if m.collector == nil {
		return
	}
	labels := m.teamLabels(ev.TeamID, ev.Workflow, ev.StageName, ev.AgentRole)
	m.collector.RecordWithLabels("team", metrics.MTeamStageCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamStageFailCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamStageDurationSec, ev.DurationSec, labels)
	if ev.RetryCount > 0 {
		m.collector.RecordWithLabels("team", metrics.MTeamStageRetryCount, float64(ev.RetryCount), labels)
	}
}

func (m *MetricsEmitter) OnStageRetry(ev StageEvent, attempt int, delay time.Duration) {
	if m.collector == nil {
		return
	}
	labels := m.teamLabels(ev.TeamID, ev.Workflow, ev.StageName, ev.AgentRole)
	m.collector.RecordWithLabels("team", metrics.MTeamStageRetryCount, float64(attempt), labels)
}

// ─── TaskHook ───────────────────────────────────────────────────────────────

func (m *MetricsEmitter) OnTaskReady(ev TaskEvent)  {}
func (m *MetricsEmitter) OnTaskStart(ev TaskEvent)  {}
func (m *MetricsEmitter) OnTaskComplete(ev TaskEvent) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"team": ev.TeamID, "task_name": ev.TaskName}
	m.collector.RecordWithLabels("team", metrics.MTeamAgentRunCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamAgentDurationSec, ev.DurationSec, labels)
}

func (m *MetricsEmitter) OnTaskFail(ev TaskEvent) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"team": ev.TeamID, "task_name": ev.TaskName}
	m.collector.RecordWithLabels("team", metrics.MTeamAgentRunCount, 1, labels)
}

func (m *MetricsEmitter) OnTaskRetry(ev TaskEvent, attempt int, delay time.Duration) {}

// ─── TeamHook ───────────────────────────────────────────────────────────────

func (m *MetricsEmitter) OnTeamStart(ev TeamEvent) {}

func (m *MetricsEmitter) OnTeamComplete(ev TeamEvent) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"team": ev.TeamID, "workflow": ev.Workflow}
	m.collector.RecordWithLabels("team", metrics.MTeamRunCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamSuccessCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamDurationSec, ev.DurationSec, labels)
	if ev.StageCount > 0 {
		m.collector.RecordWithLabels("team", metrics.MTeamStageCount, float64(ev.StageCount), labels)
	}
	if ev.FilesProduced > 0 {
		m.collector.RecordWithLabels("team", metrics.MTeamFileCount, float64(ev.FilesProduced), labels)
	}
}

func (m *MetricsEmitter) OnTeamFail(ev TeamEvent) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"team": ev.TeamID, "workflow": ev.Workflow}
	m.collector.RecordWithLabels("team", metrics.MTeamRunCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamFailCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamDurationSec, ev.DurationSec, labels)
}

func (m *MetricsEmitter) OnTeamStageTransition(from, to string, ev TeamEvent) {}

// ─── CollaborationHook ──────────────────────────────────────────────────────

func (m *MetricsEmitter) OnCollabMessage(ev CollaborationEvent)    {}
func (m *MetricsEmitter) OnCollabReview(ev CollaborationEvent)     {}
func (m *MetricsEmitter) OnCollabConsensus(ev CollaborationEvent)  {}
func (m *MetricsEmitter) OnCollabHandoff(from, to, summary string, ev CollaborationEvent) {}

// ─── PromptHook ─────────────────────────────────────────────────────────────

func (m *MetricsEmitter) OnPromptRender(ev PromptEvent) {}

func (m *MetricsEmitter) OnPromptVersion(promptID, version, hash string, metadata map[string]string) {}

func (m *MetricsEmitter) OnPromptCompare(v1, v2 string, diffStats map[string]int) {}

func (m *MetricsEmitter) OnPromptABStart(testID, variantA, variantB string, ev PromptEvent) {}

func (m *MetricsEmitter) OnPromptABResult(res PromptABResult) {}

// ─── helpers ────────────────────────────────────────────────────────────────

func (m *MetricsEmitter) teamLabels(team, workflow, stage, role string) map[string]string {
	return map[string]string{
		"team":     firstNonEmpty(team, "unknown"),
		"workflow": firstNonEmpty(workflow, "unknown"),
		"stage":    firstNonEmpty(stage, "unknown"),
		"role":     firstNonEmpty(role, "unknown"),
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// BusSubscriber 将 EventBus 事件分发给 HookRegistry 中各 hook 实现。
// 作为统一适配层, 让业务代码只需调用 Emit, 无需直接操作 hook。
type BusSubscriber struct {
	registry *HookRegistry
}

// NewBusSubscriber 创建 Bus subscriber。
func NewBusSubscriber(r *HookRegistry) *BusSubscriber {
	return &BusSubscriber{registry: r}
}

// OnEvent 实现 Subscriber 接口, 将 Event 路由到对应 hook。
func (s *BusSubscriber) OnEvent(e Event) {
	switch e.Type {
	case EvtLLMCallStart:
		ev := s.toLLMEvent(e)
		for _, h := range s.registry.LLM {
			safeCall(func() { h.OnLLMCallStart(ev) })
		}
	case EvtLLMCallComplete:
		ev := s.toLLMEvent(e)
		for _, h := range s.registry.LLM {
			safeCall(func() { h.OnLLMCallComplete(ev) })
		}
	case EvtLLMCallError:
		ev := s.toLLMEvent(e)
		for _, h := range s.registry.LLM {
			safeCall(func() { h.OnLLMCallError(ev) })
		}
	case EvtLLMCallFallback:
		ev := s.toLLMEvent(e)
		primary := strPayload(e, "primary")
		fallback := strPayload(e, "fallback")
		for _, h := range s.registry.LLM {
			safeCall(func() { h.OnLLMCallFallback(primary, fallback, ev) })
		}
	case EvtStageStart:
		ev := s.toStageEvent(e)
		for _, h := range s.registry.Stage {
			safeCall(func() { h.OnStageStart(ev) })
		}
	case EvtStageComplete:
		ev := s.toStageEvent(e)
		for _, h := range s.registry.Stage {
			safeCall(func() { h.OnStageComplete(ev) })
		}
	case EvtStageFail:
		ev := s.toStageEvent(e)
		for _, h := range s.registry.Stage {
			safeCall(func() { h.OnStageFail(ev) })
		}
	case EvtStageRetry:
		ev := s.toStageEvent(e)
		attempt := intPayload(e, "attempt")
		delay := time.Duration(intPayload(e, "delay_ms")) * time.Millisecond
		for _, h := range s.registry.Stage {
			safeCall(func() { h.OnStageRetry(ev, attempt, delay) })
		}
	case EvtTaskStart:
		ev := s.toTaskEvent(e)
		for _, h := range s.registry.Task {
			safeCall(func() { h.OnTaskStart(ev) })
		}
	case EvtTaskComplete:
		ev := s.toTaskEvent(e)
		for _, h := range s.registry.Task {
			safeCall(func() { h.OnTaskComplete(ev) })
		}
	case EvtTaskFail:
		ev := s.toTaskEvent(e)
		for _, h := range s.registry.Task {
			safeCall(func() { h.OnTaskFail(ev) })
		}
	case EvtTaskRetry:
		ev := s.toTaskEvent(e)
		attempt := intPayload(e, "attempt")
		delay := time.Duration(intPayload(e, "delay_ms")) * time.Millisecond
		for _, h := range s.registry.Task {
			safeCall(func() { h.OnTaskRetry(ev, attempt, delay) })
		}
	case EvtTeamStart:
		ev := s.toTeamEvent(e)
		for _, h := range s.registry.Team {
			safeCall(func() { h.OnTeamStart(ev) })
		}
	case EvtTeamComplete:
		ev := s.toTeamEvent(e)
		for _, h := range s.registry.Team {
			safeCall(func() { h.OnTeamComplete(ev) })
		}
	case EvtTeamFail:
		ev := s.toTeamEvent(e)
		for _, h := range s.registry.Team {
			safeCall(func() { h.OnTeamFail(ev) })
		}
	case EvtCollabMessage:
		ev := s.toCollabEvent(e)
		for _, h := range s.registry.Collaboration {
			safeCall(func() { h.OnCollabMessage(ev) })
		}
	case EvtCollabReview:
		ev := s.toCollabEvent(e)
		for _, h := range s.registry.Collaboration {
			safeCall(func() { h.OnCollabReview(ev) })
		}
	case EvtPromptRender:
		ev := s.toPromptEvent(e)
		for _, h := range s.registry.Prompt {
			safeCall(func() { h.OnPromptRender(ev) })
		}
	case EvtPromptABResult:
		res := s.toPromptABResult(e)
		for _, h := range s.registry.Prompt {
			safeCall(func() { h.OnPromptABResult(res) })
		}
	}
}

func (s *BusSubscriber) toLLMEvent(e Event) LLMCallEvent {
	var ev LLMCallEvent
	if rec, ok := e.Payload["record"].(api.LLMCallRecord); ok {
		ev.Record = rec
	}
	ev.TraceID = e.TraceID
	ev.SpanID = e.SpanID
	ev.TeamID = strPayload(e, "team_id")
	ev.StageName = strPayload(e, "stage_name")
	ev.AgentRole = strPayload(e, "agent_role")
	ev.PromptHash = strPayload(e, "prompt_hash")
	ev.PromptLen = intPayload(e, "prompt_len")
	ev.ResponseLen = intPayload(e, "response_len")
	return ev
}

func (s *BusSubscriber) toStageEvent(e Event) StageEvent {
	return StageEvent{
		TraceID:       e.TraceID,
		TeamID:        strPayload(e, "team_id"),
		Workflow:      strPayload(e, "workflow"),
		StageName:     strPayload(e, "stage_name"),
		StageIndex:    intPayload(e, "stage_index"),
		AgentRole:     strPayload(e, "agent_role"),
		ModelAlias:    strPayload(e, "model_alias"),
		DurationSec:   floatPayload(e, "duration_sec"),
		Success:       boolPayload(e, "success"),
		Error:         strPayload(e, "error"),
		RetryCount:    intPayload(e, "retry_count"),
		OutputLen:     intPayload(e, "output_len"),
		ToolCallCount: intPayload(e, "tool_call_count"),
		BuildPassed:   boolPayload(e, "build_passed"),
		TestPassed:    boolPayload(e, "test_passed"),
	}
}

func (s *BusSubscriber) toTaskEvent(e Event) TaskEvent {
	return TaskEvent{
		TraceID:     e.TraceID,
		TeamID:      strPayload(e, "team_id"),
		TaskID:      strPayload(e, "task_id"),
		TaskName:    strPayload(e, "task_name"),
		DurationSec: floatPayload(e, "duration_sec"),
		Success:     boolPayload(e, "success"),
		Error:       strPayload(e, "error"),
		Attempt:     intPayload(e, "attempt"),
	}
}

func (s *BusSubscriber) toTeamEvent(e Event) TeamEvent {
	return TeamEvent{
		TraceID:       e.TraceID,
		TeamID:        strPayload(e, "team_id"),
		TeamName:      strPayload(e, "team_name"),
		Workflow:      strPayload(e, "workflow"),
		DurationSec:   floatPayload(e, "duration_sec"),
		Success:       boolPayload(e, "success"),
		Error:         strPayload(e, "error"),
		StageCount:    intPayload(e, "stage_count"),
		LLMCallCount:  intPayload(e, "llm_call_count"),
		TotalTokens:   intPayload(e, "total_tokens"),
		CostUSD:       floatPayload(e, "cost_usd"),
		FilesProduced: intPayload(e, "files_produced"),
	}
}

func (s *BusSubscriber) toCollabEvent(e Event) CollaborationEvent {
	return CollaborationEvent{
		TraceID:     e.TraceID,
		TeamID:      strPayload(e, "team_id"),
		FromRole:    strPayload(e, "from_role"),
		ToRole:      strPayload(e, "to_role"),
		MessageType: strPayload(e, "message_type"),
		ContentHash: strPayload(e, "content_hash"),
		ContentLen:  intPayload(e, "content_len"),
		TokensIn:    intPayload(e, "tokens_in"),
		TokensOut:   intPayload(e, "tokens_out"),
		LatencySec:  floatPayload(e, "latency_sec"),
		Accepted:    boolPayload(e, "accepted"),
		Score:       floatPayload(e, "score"),
	}
}

func (s *BusSubscriber) toPromptEvent(e Event) PromptEvent {
	return PromptEvent{
		TraceID:      e.TraceID,
		PromptID:     strPayload(e, "prompt_id"),
		Version:      strPayload(e, "version"),
		PromptHash:   strPayload(e, "prompt_hash"),
		TemplateName: strPayload(e, "template_name"),
		RenderedLen:  intPayload(e, "rendered_len"),
		RenderTimeMs: floatPayload(e, "render_time_ms"),
	}
}

func (s *BusSubscriber) toPromptABResult(e Event) PromptABResult {
	return PromptABResult{
		TraceID:     e.TraceID,
		TestID:      strPayload(e, "test_id"),
		VariantA:    strPayload(e, "variant_a"),
		VariantB:    strPayload(e, "variant_b"),
		MetricName:  strPayload(e, "metric_name"),
		Winner:      strPayload(e, "winner"),
		Improvement: floatPayload(e, "improvement"),
		SampleSize:  intPayload(e, "sample_size"),
	}
}

func strPayload(e Event, k string) string {
	if v, ok := e.Payload[k].(string); ok {
		return v
	}
	return ""
}

func intPayload(e Event, k string) int {
	switch v := e.Payload[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func floatPayload(e Event, k string) float64 {
	switch v := e.Payload[k].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

func boolPayload(e Event, k string) bool {
	if v, ok := e.Payload[k].(bool); ok {
		return v
	}
	return false
}

func safeCall(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[observability] hook panic recovered: %v", r)
		}
	}()
	fn()
}
