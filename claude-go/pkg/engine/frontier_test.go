// frontier_test.go — 单元测试: QueryEngine 前沿优化组件。
// 放在 package engine 内, 避免 tests/unit 下 pre-existing 构建错误干扰。
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/types"
)

// ==================== internal_hook.PromptCacheBuilder ====================

func TestPromptCache_StablePrefixHit(t *testing.T) {
	b := internal_hook.NewPromptCacheBuilder()
	static := []string{"sys prompt", "tool defs"}

	_, hit1 := b.Build(static, []string{"turn1 user"})
	if hit1 {
		t.Fatalf("首次应该是 miss, 实际 hit")
	}

	_, hit2 := b.Build(static, []string{"turn2 user"})
	if !hit2 {
		t.Fatalf("相同前缀 + 不同动态, 应 hit, 实际 miss")
	}

	_, hit3 := b.Build([]string{"sys prompt changed", "tool defs"}, nil)
	if hit3 {
		t.Fatalf("前缀变更应 miss, 实际 hit")
	}

	hits, miss := b.Stats()
	if hits != 1 || miss != 2 {
		t.Fatalf("hits/miss = %d/%d, 期望 1/2", hits, miss)
	}
}

func TestPromptCache_SplitStaticDynamic(t *testing.T) {
	static, dynamic := internal_hook.SplitStaticDynamic([]string{
		"system role prompt",
		"<recent_memory> some mem </recent_memory>",
		"tool defs list",
	})
	if len(static) != 2 || len(dynamic) != 1 {
		t.Fatalf("split 预期 2 static + 1 dynamic, 实际 %d/%d", len(static), len(dynamic))
	}
	if !strings.Contains(dynamic[0], "recent_memory") {
		t.Fatalf("dynamic 未识别记忆标记")
	}
}

func TestPromptCache_NilSafe(t *testing.T) {
	var b *internal_hook.PromptCacheBuilder
	merged, hit := b.Build([]string{"a"}, []string{"b"})
	if hit {
		t.Fatalf("nil builder 不应报告 hit")
	}
	if len(merged) != 2 {
		t.Fatalf("nil builder 仍应拼装, 实际 len=%d", len(merged))
	}
}

// ==================== TokenBudgetManager ====================

func TestBudget_LevelDetection(t *testing.T) {
	m := internal_hook.NewTokenBudgetManager(1000) // 1000 token 预算
	// 生成 ~650 token (≈ 2600 字符) 进入 Yellow (>=60%)
	msgs := []types.Message{
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: strings.Repeat("x", 2600)}}},
	}
	level := m.Level(msgs)
	if level != internal_hook.BudgetYellow {
		t.Fatalf("期望 Yellow, 实际 %s", level)
	}

	// Red: >= 90% → >= 3600 字符
	msgs[0].Content[0].Text = strings.Repeat("x", 3700)
	level = m.Level(msgs)
	if level != internal_hook.BudgetRed {
		t.Fatalf("期望 Red, 实际 %s", level)
	}

	// Critical: >= 95% → >= 3800 字符
	msgs[0].Content[0].Text = strings.Repeat("x", 3900)
	level = m.Level(msgs)
	if level != internal_hook.BudgetCritical {
		t.Fatalf("期望 Critical, 实际 %s", level)
	}
}

func TestBudget_RedDegradeToolResults(t *testing.T) {
	m := internal_hook.NewTokenBudgetManager(100000)
	m.KeepRecentToolTurns = 1
	m.RedSummaryMaxChars = 50

	long := strings.Repeat("very long output ", 200) // > 50 chars
	msgs := []types.Message{
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockToolResult, Content: long, ToolUseID: "t1"}}},
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "thinking"}}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockToolResult, Content: long, ToolUseID: "t2"}}},
	}

	degraded := m.Degrade(msgs, internal_hook.BudgetRed)
	if len(degraded) != 3 {
		t.Fatalf("degrade 不应改变消息数量")
	}
	// 第 0 条 tool_result 应被摘要, 第 2 条保留
	if len(degraded[0].Content[0].Content) >= len(long) {
		t.Fatalf("早期 tool_result 应被摘要, 实际长度 %d 仍 >= %d", len(degraded[0].Content[0].Content), len(long))
	}
	if degraded[2].Content[0].Content != long {
		t.Fatalf("最近的 tool_result 应保留原文")
	}
}

func TestBudget_GreenNoChange(t *testing.T) {
	m := internal_hook.NewTokenBudgetManager(100000)
	msgs := []types.Message{
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "short"}}},
	}
	if got := m.Level(msgs); got != internal_hook.BudgetGreen {
		t.Fatalf("少量消息应为 Green, 实际 %s", got)
	}
	out := m.Degrade(msgs, internal_hook.BudgetGreen)
	if &out[0] == &msgs[0] {
		// ok, 共享底层, 不强制要求
	}
	if out[0].Content[0].Text != "short" {
		t.Fatalf("Green 不应改变内容")
	}
}

// ==================== LoopDetector ====================

func TestLoopDetector_TriggerOnRepeatedCall(t *testing.T) {
	d := internal_hook.NewLoopDetector()
	d.Threshold = 3

	input := json.RawMessage(`{"path":"a.go"}`)

	for i := 0; i < 2; i++ {
		looped, _ := d.Observe("Read", input)
		if looped {
			t.Fatalf("第 %d 次就触发, 期望 threshold=3", i+1)
		}
	}
	looped, sug := d.Observe("Read", input)
	if !looped {
		t.Fatalf("第 3 次应触发")
	}
	if !strings.Contains(sug, "Read") {
		t.Fatalf("建议文本未包含工具名")
	}
}

func TestLoopDetector_DifferentInputsNotLooped(t *testing.T) {
	d := internal_hook.NewLoopDetector()
	d.Threshold = 3

	for _, p := range []string{`{"path":"a.go"}`, `{"path":"b.go"}`, `{"path":"c.go"}`} {
		looped, _ := d.Observe("Read", json.RawMessage(p))
		if looped {
			t.Fatalf("不同参数不应触发: %s", p)
		}
	}
}

func TestLoopDetector_Reset(t *testing.T) {
	d := internal_hook.NewLoopDetector()
	d.Threshold = 2
	d.Observe("Read", json.RawMessage(`{"a":1}`))
	if d.Size() != 1 {
		t.Fatalf("期望 size=1")
	}
	d.Reset()
	if d.Size() != 0 {
		t.Fatalf("reset 后 size 应为 0")
	}
}

// ==================== ErrorClassifier ====================

func TestErrorClassifier_Classify(t *testing.T) {
	c := internal_hook.NewErrorClassifier()

	tests := []struct {
		name string
		err  error
		want internal_hook.ErrorFamily
	}{
		{"ptl typed", &api.PromptTooLongError{Message: "prompt too long"}, internal_hook.ErrFamilyPTL},
		{"overloaded typed", &api.OverloadedError{Message: "overloaded"}, internal_hook.ErrFamilyOverload},
		{"429 string", errors.New("HTTP 429 rate limit exceeded"), internal_hook.ErrFamilyRateLimit},
		{"503 string", errors.New("503 overloaded"), internal_hook.ErrFamilyOverload},
		{"500 string", errors.New("500 internal server error"), internal_hook.ErrFamilyServer},
		{"400 string", errors.New("400 bad request: invalid parameter"), internal_hook.ErrFamilyBadRequest},
		{"timeout string", errors.New("context deadline exceeded timeout"), internal_hook.ErrFamilyTimeout},
		{"network string", errors.New("connection reset by peer"), internal_hook.ErrFamilyNetwork},
		{"auth string", errors.New("401 unauthorized"), internal_hook.ErrFamilyAuth},
		{"ctx deadline", context.DeadlineExceeded, internal_hook.ErrFamilyTimeout},
		{"nil err", nil, internal_hook.ErrFamilyUnknown},
	}
	for _, tt := range tests {
		got := c.Classify(tt.err)
		if got != tt.want {
			t.Errorf("%s: Classify=%s, want=%s", tt.name, got, tt.want)
		}
	}
}

func TestErrorClassifier_BudgetExhaustion(t *testing.T) {
	c := internal_hook.NewErrorClassifier()

	// BadRequest: 0 次重试预算, 立即 abort
	_, abort, _, _ := c.Observe(errors.New("400 bad request"))
	if !abort {
		t.Fatalf("BadRequest 应立即 abort")
	}

	// RateLimit: 5 次预算
	var lastAbort bool
	for i := 0; i < 6; i++ {
		_, lastAbort, _, _ = c.Observe(errors.New("429 rate limit"))
	}
	if !lastAbort {
		t.Fatalf("6 次 429 后应 abort (预算 5)")
	}
}

func TestErrorClassifier_ResetOnSuccess(t *testing.T) {
	c := internal_hook.NewErrorClassifier()
	_, _, _, _ = c.Observe(errors.New("429 rate limit"))
	c.ResetAll()
	cnt := c.Counters()
	if len(cnt) != 0 {
		t.Fatalf("ResetAll 后计数应清零, 实际 %v", cnt)
	}
}

// ==================== JSONRepair ====================

func TestJSONRepair_DirectValid(t *testing.T) {
	r := internal_hook.NewJSONRepair()
	out, res := r.Try([]byte(`{"a":1}`))
	if !res.OK || res.Method != "direct" {
		t.Fatalf("direct valid 应直接通过, got %+v", res)
	}
	if string(out) != `{"a":1}` {
		t.Fatalf("输出被改变: %s", out)
	}
}

func TestJSONRepair_TrailingComma(t *testing.T) {
	r := internal_hook.NewJSONRepair()
	out, res := r.Try([]byte(`{"a":1,}`))
	if !res.OK {
		t.Fatalf("尾逗号应被修复, got %+v", res)
	}
	if !json.Valid(out) {
		t.Fatalf("修复后仍非法: %s", out)
	}
}

func TestJSONRepair_MarkdownFence(t *testing.T) {
	r := internal_hook.NewJSONRepair()
	input := "```json\n{\"a\":1}\n```"
	out, res := r.Try([]byte(input))
	if !res.OK {
		t.Fatalf("markdown fence 应被剥离, got %+v", res)
	}
	if !json.Valid(out) {
		t.Fatalf("修复后仍非法: %s", out)
	}
}

func TestJSONRepair_TruncatedBracket(t *testing.T) {
	r := internal_hook.NewJSONRepair()
	out, res := r.Try([]byte(`{"a":1`))
	if !res.OK {
		t.Fatalf("截断应被修复, got %+v", res)
	}
	if !json.Valid(out) {
		t.Fatalf("修复后仍非法: %s", out)
	}
}

func TestJSONRepair_SingleQuotes(t *testing.T) {
	r := internal_hook.NewJSONRepair()
	out, res := r.Try([]byte(`{'key':'value'}`))
	if !res.OK {
		t.Fatalf("单引号应被替换, got %+v", res)
	}
	if !json.Valid(out) {
		t.Fatalf("修复后仍非法: %s", out)
	}
}

func TestJSONRepair_FailCase(t *testing.T) {
	r := internal_hook.NewJSONRepair()
	_, res := r.Try([]byte(`not json at all ###`))
	if res.OK {
		t.Fatalf("garbage 不应被报告为 OK")
	}
}

func TestJSONRepair_EmptyInput(t *testing.T) {
	r := internal_hook.NewJSONRepair()
	_, res := r.Try([]byte{})
	if !res.OK {
		t.Fatalf("空输入应视为 OK")
	}
}

// ==================== Trajectory ====================

func TestTrajectory_InferVerdict(t *testing.T) {
	tests := []struct {
		name    string
		stop    string
		tools   []internal_hook.ToolSig
		aborted bool
		want    internal_hook.TrajVerdict
	}{
		{"aborted wins", "", nil, true, internal_hook.VerdictAborted},
		{"end_turn no tools", "end_turn", nil, false, internal_hook.VerdictSuccess},
		{"max_tokens", "max_tokens", nil, false, internal_hook.VerdictPartial},
		{"all tools ok", "tool_use", []internal_hook.ToolSig{{OK: true}, {OK: true}}, false, internal_hook.VerdictSuccess},
		{"partial tools", "tool_use", []internal_hook.ToolSig{{OK: true}, {OK: false}}, false, internal_hook.VerdictPartial},
		{"all tools fail", "tool_use", []internal_hook.ToolSig{{OK: false}, {OK: false}}, false, internal_hook.VerdictFail},
	}
	for _, tt := range tests {
		got := internal_hook.InferVerdict(tt.stop, tt.tools, tt.aborted)
		if got != tt.want {
			t.Errorf("%s: got %s want %s", tt.name, got, tt.want)
		}
	}
}

func TestTrajectory_Format(t *testing.T) {
	tr := &internal_hook.Trajectory{
		TurnID:     "t1",
		UserIntent: "帮我重构认证模块",
		Plan:       "先阅读现有代码, 然后提取接口",
		ToolCalls: []internal_hook.ToolSig{
			{Name: "Read", OK: true},
			{Name: "Edit", OK: true},
			{Name: "Read", OK: true},
		},
		Verdict:   internal_hook.VerdictSuccess,
		At:        time.Now(),
		LatencyMs: 1500,
	}
	s := tr.Format()
	if !strings.Contains(s, "success") || !strings.Contains(s, "重构") {
		t.Fatalf("Format 缺失必要字段: %s", s)
	}
	// tools 应去重
	if !strings.Contains(s, "Read,Edit") {
		t.Fatalf("去重后期望 Read,Edit, 实际: %s", s)
	}
}

func TestTrajectory_ExtractPlan(t *testing.T) {
	msgs := []types.Message{
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "user question"}}},
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockThinking, Thinking: "  let me plan  "},
			{Type: types.ContentBlockText, Text: "ok here is what we will do"},
		}},
	}
	plan := internal_hook.ExtractPlan(msgs)
	if plan != "let me plan" {
		t.Fatalf("期望从 thinking 提取, 实际: %q", plan)
	}
}

// ==================== internal_hook.EngineMetrics ====================

func TestEngineMetrics_Snapshot(t *testing.T) {
	m := internal_hook.NewEngineMetrics()
	m.TurnsTotal.Add(3)
	m.TurnsSuccess.Add(2)
	m.RecordCache(true)
	m.RecordCache(true)
	m.RecordCache(false)
	m.RecordBudgetDegrade(int(internal_hook.BudgetRed))
	m.RecordError(int(internal_hook.ErrFamilyRateLimit))
	m.RecordTurnLatency(500 * time.Millisecond)

	snap := m.Snapshot()

	if snap["turns_total"].(int64) != 3 {
		t.Fatalf("turns_total 错误: %v", snap["turns_total"])
	}
	if rate := snap["cache_hit_rate"].(float64); rate < 0.66 || rate > 0.67 {
		t.Fatalf("cache_hit_rate 错误: %v (期望 ~0.666)", rate)
	}
	budget := snap["budget_degradations"].(map[int]int64)
	if budget[int(internal_hook.BudgetRed)] != 1 {
		t.Fatalf("Red 降级计数错误: %v", budget)
	}
	errs := snap["errors_by_family"].(map[int]int64)
	if errs[int(internal_hook.ErrFamilyRateLimit)] != 1 {
		t.Fatalf("RateLimit 计数错误: %v", errs)
	}
	if avg := snap["avg_turn_latency_ms"].(int64); avg != 166 {
		t.Fatalf("avg_turn_latency_ms 错误: %v (期望 166 = 500/3)", avg)
	}
}

func TestEngineMetrics_NilSafe(t *testing.T) {
	var m *internal_hook.EngineMetrics
	m.RecordCache(true)
	m.RecordError(0)
	m.RecordBudgetDegrade(0)
	m.RecordTurnLatency(time.Second)
	snap := m.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("nil metrics Snapshot 应返回空 map")
	}
}

// ==================== StopSignalDetector ====================

func TestStopSignal_RepeatedReads(t *testing.T) {
	d := internal_hook.NewStopSignalDetector()
	d.MinToolCallsBeforeHint = 1
	d.MaxFileReadsSamePath = 3
	d.DiminishingWindow = 1 // 一次即可发射

	path := `{"path":"foo.go"}`
	// 前两次不应触发 (fileReads < 3)
	for i := 0; i < 2; i++ {
		suggest, _ := d.Observe("Read", path, "file content")
		if suggest {
			t.Fatalf("第 %d 次就建议, 期望 3 次后", i+1)
		}
	}
	suggest, reason := d.Observe("Read", path, "file content")
	if !suggest {
		t.Fatalf("3 次读同文件应触发建议")
	}
	if !strings.Contains(reason, "foo.go") {
		t.Fatalf("理由缺 path: %s", reason)
	}
}

func TestStopSignal_BelowMinCalls(t *testing.T) {
	d := internal_hook.NewStopSignalDetector()
	d.MinToolCallsBeforeHint = 100 // 高阈值
	suggest, _ := d.Observe("Read", `{"path":"x"}`, "")
	if suggest {
		t.Fatalf("未达 min calls 不应建议")
	}
}

func TestStopSignal_BuildHintMessage(t *testing.T) {
	msg := internal_hook.BuildHintMessage("test reason")
	if !strings.Contains(msg, "soft_stop_hint") || !strings.Contains(msg, "test reason") {
		t.Fatalf("hint 格式不对: %s", msg)
	}
}

// ==================== 集成: EnableFrontierOptimizations ====================

func TestEnableFrontierOptimizations_Idempotent(t *testing.T) {
	e := &QueryEngine{
		Config: &Config{},
	}
	e.EnableFrontierOptimizations()
	if e.Metrics == nil || e.ErrClassifier == nil || e.JSONRepair == nil || e.LoopDet == nil {
		t.Fatalf("Enable 后 P0 组件应全部初始化")
	}
	if e.PromptCache == nil || e.Budget == nil {
		t.Fatalf("Enable 后 P1 组件应初始化")
	}
	if !e.Config.EnableMetrics || !e.Config.EnableErrorClassifier {
		t.Fatalf("Config flags 应被开启")
	}
	// 幂等
	metricsPtr := e.Metrics
	e.EnableFrontierOptimizations()
	if e.Metrics != metricsPtr {
		t.Fatalf("幂等性: 二次调用不应替换已存在的组件")
	}
}

func TestGetEngineMetrics_Disabled(t *testing.T) {
	e := &QueryEngine{Config: &Config{}}
	snap := e.GetEngineMetrics()
	if len(snap) != 0 {
		t.Fatalf("未启用 Metrics 时应返回空 map")
	}
}

func TestGetEngineMetrics_Enabled(t *testing.T) {
	e := &QueryEngine{Config: &Config{EnableMetrics: true}}
	e.applyFeatureFlags()
	e.Metrics.TurnsTotal.Add(5)
	snap := e.GetEngineMetrics()
	if snap["turns_total"].(int64) != 5 {
		t.Fatalf("Snapshot 未透传指标")
	}
}
