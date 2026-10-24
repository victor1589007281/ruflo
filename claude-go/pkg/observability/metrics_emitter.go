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

// OnLLMCallStart LLM 调用开始钩子: 刻意空实现 —— 开始事件只留 trace, 不产生指标。
func (m *MetricsEmitter) OnLLMCallStart(ev LLMCallEvent) {
	// start 事件不上报 metrics, 只留 trace
}

// OnLLMCallComplete LLM 调用成功钩子: 记一次调用 + 一次成功, 并累加耗时与各类 token。
//
// 所有 token/重试类指标都带 `> 0` 守卫 —— 零值不写: 这些在 Prometheus 侧是 counter,
// 写 0 不改变总和, 却会给每个 (model, source, purpose, ...) 组合凭空造出一条零值序列。
//
// 算法解读：
//
//	原理：把一条 LLMCallRecord 摊成最多 9 个 counter 的一次增量, 标签统一由 llmLabels 派生。
//	关键不变量：CallCount 与 SuccessCount 必须成对 +1 —— 二者分别是成功率的分子与分母,
//	          只记一个会让 dashboard 的成功率算出 >100% 或负值。
//	代价与边界：每次调用最多 9 次 collector 写入 (每次都要拿锁 + 追加一行 JSONL +
//	          自增 Prometheus 指标); collector 为 nil (只读进程) 时整体跳过。
//	设计权衡：不做本地聚合再批量落盘, 换来"进程被 kill 也不丢已记的增量"; 代价是
//	          JSONL 的追加次数与指标条数同阶。
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

// OnLLMCallError LLM 调用失败钩子: 记一次调用 + 一次错误, 并按 ErrorKind 细分错误计数器。
//
// 注意：这里同样累加 MLLMCallCount —— 调用总数把失败也算了进去, 因此"成功率"必须
// 用 SuccessCount/CallCount 而不是 1-ErrorCount/CallCount 之外的口径自行推导。
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

// OnLLMCallFallback 模型降级钩子: 用 fallback_from/fallback_to 两个标签把降级记进重试计数器。
//
// 注意：没有独立的"降级"指标, 复用的是 MLLMRetryCount —— 因此该计数器的语义是
// "重试 + 降级"之和, 单看它无法区分两者 (要靠 fallback_from 标签是否存在来分辨)。
func (m *MetricsEmitter) OnLLMCallFallback(primary, fallback string, ev LLMCallEvent) {
	if m.collector == nil {
		return
	}
	labels := m.llmLabels(ev)
	labels["fallback_from"] = primary
	labels["fallback_to"] = fallback
	m.collector.RecordWithLabels("llm", metrics.MLLMRetryCount, 1, labels)
}

// OnLLMCacheHit 缓存命中钩子: 命中的 token 记进 cache read 计数器。
func (m *MetricsEmitter) OnLLMCacheHit(model, cacheType string, tokens int) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"model": model, "cache_type": cacheType}
	m.collector.RecordWithLabels("llm", metrics.MLLMCacheReadTokens, float64(tokens), labels)
}

// OnLLMCacheMiss 缓存未命中钩子: 未命中的 token 记进 cache creation 计数器 (写缓存要付的钱)。
func (m *MetricsEmitter) OnLLMCacheMiss(model, cacheType string, tokens int) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"model": model, "cache_type": cacheType}
	m.collector.RecordWithLabels("llm", metrics.MLLMCacheCreateTokens, float64(tokens), labels)
}

// llmLabels 从事件派生 LLM 指标的标签集, 空值统一回落成 "unknown" 或 ""。
//
// 注意：model 与 model_alias 取的是**同一个** rec.Model —— 两个标签名一个值,
// 这是给历史面板兼容用的, 不要据此以为存在"别名到真名"的映射。
// team_id/stage/role 三个标签只在非空时附加, 保持只读场景 (无团队) 的标签集最小。
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

// OnStageStart 阶段开始钩子: 空实现 —— 阶段耗时由完成/失败事件携带, 开始事件不单独计时。
func (m *MetricsEmitter) OnStageStart(ev StageEvent) {}

// OnStageComplete 阶段成功钩子: 记一次阶段 + 一次成功, 并累加耗时、输出长度与工具调用数。
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

// OnStageFail 阶段失败钩子: 记一次阶段 + 一次失败, 并累加耗时与本次已发生的重试数。
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

// OnStageRetry 阶段重试钩子: 把 attempt 作为增量记进阶段重试计数器 (delay 不进指标)。
//
// 注意：这里加的是 attempt (绝对值), 而 OnStageFail 加的是 ev.RetryCount —— 同一事件
// 若既触发重试又触发失败, 该计数器会重复计量; 两者不是互补口径, 不要相加。
func (m *MetricsEmitter) OnStageRetry(ev StageEvent, attempt int, delay time.Duration) {
	if m.collector == nil {
		return
	}
	labels := m.teamLabels(ev.TeamID, ev.Workflow, ev.StageName, ev.AgentRole)
	m.collector.RecordWithLabels("team", metrics.MTeamStageRetryCount, float64(attempt), labels)
}

// ─── TaskHook ───────────────────────────────────────────────────────────────

// OnTaskReady 任务就绪钩子: 空实现 —— "就绪"是调度器内部状态, 没有对应的对外指标。
func (m *MetricsEmitter) OnTaskReady(ev TaskEvent)  {}

// OnTaskStart 任务开始钩子: 空实现 —— 任务耗时由完成/失败事件携带。
func (m *MetricsEmitter) OnTaskStart(ev TaskEvent)  {}

// OnTaskComplete 任务成功钩子: 记一次 Agent 运行 + 一次耗时。
func (m *MetricsEmitter) OnTaskComplete(ev TaskEvent) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"team": ev.TeamID, "task_name": ev.TaskName}
	m.collector.RecordWithLabels("team", metrics.MTeamAgentRunCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamAgentDurationSec, ev.DurationSec, labels)
}

// OnTaskFail 任务失败钩子: 只记一次 Agent 运行 (失败与成功共用同一个运行计数器)。
//
// 注意：这里**不**记耗时 —— 与 OnTaskComplete 不对称, 因此"平均 Agent 耗时"实际
// 只统计成功的那些任务, 失败的耗时在面板上不可见。
func (m *MetricsEmitter) OnTaskFail(ev TaskEvent) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"team": ev.TeamID, "task_name": ev.TaskName}
	m.collector.RecordWithLabels("team", metrics.MTeamAgentRunCount, 1, labels)
}

// OnTaskRetry 任务重试钩子: 空实现 —— 任务级重试暂不单独计数 (阶段级重试才进指标)。
func (m *MetricsEmitter) OnTaskRetry(ev TaskEvent, attempt int, delay time.Duration) {}

// ─── TeamHook ───────────────────────────────────────────────────────────────

// OnTeamStart 团队开始钩子: 空实现 —— 团队耗时由完成/失败事件携带。
func (m *MetricsEmitter) OnTeamStart(ev TeamEvent) {}

// OnTeamComplete 团队成功钩子: 记一次团队运行 + 一次成功 + 耗时, 并累加阶段数与产出文件数。
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

// OnTeamFail 团队失败钩子: 记一次团队运行 + 一次失败 + 耗时 (与成功路径共用运行计数器)。
func (m *MetricsEmitter) OnTeamFail(ev TeamEvent) {
	if m.collector == nil {
		return
	}
	labels := map[string]string{"team": ev.TeamID, "workflow": ev.Workflow}
	m.collector.RecordWithLabels("team", metrics.MTeamRunCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamFailCount, 1, labels)
	m.collector.RecordWithLabels("team", metrics.MTeamDurationSec, ev.DurationSec, labels)
}

// OnTeamStageTransition 团队阶段迁移钩子: 空实现 —— 阶段本身的起止已由 StageHook 覆盖。
func (m *MetricsEmitter) OnTeamStageTransition(from, to string, ev TeamEvent) {}

// ─── CollaborationHook ──────────────────────────────────────────────────────

// OnCollabMessage Agent 间消息钩子: 空实现 (协作指标目前只留事件, 未落 metrics)。
func (m *MetricsEmitter) OnCollabMessage(ev CollaborationEvent)    {}

// OnCollabReview 协作评审钩子: 空实现。
func (m *MetricsEmitter) OnCollabReview(ev CollaborationEvent)     {}

// OnCollabConsensus 协作共识钩子: 空实现。
func (m *MetricsEmitter) OnCollabConsensus(ev CollaborationEvent)  {}

// OnCollabHandoff 协作交接钩子: 空实现。
func (m *MetricsEmitter) OnCollabHandoff(from, to, summary string, ev CollaborationEvent) {}

// ─── PromptHook ─────────────────────────────────────────────────────────────

// OnPromptRender prompt 渲染钩子: 空实现 (渲染观测走 prompt_observatory.go 那条独立链路)。
func (m *MetricsEmitter) OnPromptRender(ev PromptEvent) {}

// OnPromptVersion prompt 版本变更钩子: 空实现。
func (m *MetricsEmitter) OnPromptVersion(promptID, version, hash string, metadata map[string]string) {}

// OnPromptCompare prompt 版本对比钩子: 空实现。
func (m *MetricsEmitter) OnPromptCompare(v1, v2 string, diffStats map[string]int) {}

// OnPromptABStart A/B 实验开始钩子: 空实现 (只有结果事件进了 BusSubscriber 的分发)。
func (m *MetricsEmitter) OnPromptABStart(testID, variantA, variantB string, ev PromptEvent) {}

// OnPromptABResult A/B 实验结果钩子: 空实现。
func (m *MetricsEmitter) OnPromptABResult(res PromptABResult) {}

// ─── helpers ────────────────────────────────────────────────────────────────

// teamLabels 构造团队类指标的固定四标签 (team/workflow/stage/role), 空值一律回落 "unknown"。
//
// 与 llmLabels 的差别: 这里**不**省略空标签 —— 团队面板按这四维分组, 缺维会让
// Prometheus 把不同来源的序列合并, 用 "unknown" 占位才能保住分组边界。
func (m *MetricsEmitter) teamLabels(team, workflow, stage, role string) map[string]string {
	return map[string]string{
		"team":     firstNonEmpty(team, "unknown"),
		"workflow": firstNonEmpty(workflow, "unknown"),
		"stage":    firstNonEmpty(stage, "unknown"),
		"role":     firstNonEmpty(role, "unknown"),
	}
}

// firstNonEmpty 返回第一个非空字符串; 全空时返回 ""。
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
	// 未列出 case 的 EventType (EvtTaskReady / EvtLLMCacheHit / EvtCollabConsensus /
	// EvtPromptVersion / trace:* 等) 会掉出 switch 被**静默丢弃** —— 发射方不报错,
	// 订阅方也收不到; 新增事件类型时必须同时在这里补 case, 否则等于没接。
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
		// delay 以毫秒整数过 payload (map[string]interface{} 里放 time.Duration 会被
		// 断言成 int64 而不是 Duration), 这里再折回 Duration 只是为对齐 hook 签名。
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

// toLLMEvent 把通用 Event 的 payload 还原成 LLMCallEvent。
//
// record 字段按 api.LLMCallRecord **值类型**断言: payload 里放的是指针或 map 时
// 断言失败, ev.Record 会留成零值 (指标照发, 但 token/耗时全是 0)。
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

// toStageEvent 把通用 Event 的 payload 还原成 StageEvent (缺失字段留零值)。
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

// toTaskEvent 把通用 Event 的 payload 还原成 TaskEvent。
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

// toTeamEvent 把通用 Event 的 payload 还原成 TeamEvent。
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

// toCollabEvent 把通用 Event 的 payload 还原成 CollaborationEvent。
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

// toPromptEvent 把通用 Event 的 payload 还原成 PromptEvent。
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

// toPromptABResult 把通用 Event 的 payload 还原成 PromptABResult。
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

// strPayload 取 payload 里的字符串字段; 缺失或类型不符时返回 ""。
func strPayload(e Event, k string) string {
	if v, ok := e.Payload[k].(string); ok {
		return v
	}
	return ""
}

// intPayload 取 payload 里的整型字段, 兼容 int/int64/float64 (JSON 解出来是 float64) 三种载体。
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

// floatPayload 取 payload 里的浮点字段, 兼容 float64/float32/int/int64 四种载体。
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

// boolPayload 取 payload 里的布尔字段; 缺失或类型不符时返回 false。
func boolPayload(e Event, k string) bool {
	if v, ok := e.Payload[k].(bool); ok {
		return v
	}
	return false
}

// safeCall 执行 hook 并吞掉 panic: 单个订阅者崩溃不能让整条事件分发链断掉。
//
// 注意：panic 只记一行日志, 没有栈、没有计数器 —— 一个持续 panic 的 hook 在指标上
// 完全不可见, 表现为"某类指标安静地不再增长"。
func safeCall(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[observability] hook panic recovered: %v", r)
		}
	}()
	fn()
}
