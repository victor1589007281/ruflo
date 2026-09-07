package metrics

// llm_surface_test.go —— F10 表面定价的兑现形态 (dsh token-meter 的 Go 侧适配)。
//
// 三条承诺各自钉一个测试:
//	1. 锚定总量守恒: Σ tokens == 真实 InputTokens (失配即静默错价, 必须在分配处收敛)。
//	2. 缺锚点/零组件不捏造: 网关没回 input 或没有组件信息时**一条都不发** ——
//	   dsh projection.ts 明文禁止拿启发式冒充总量 ("never as a total"), ledger 的
//	   "缺数据不落 0" 同源纪律。
//	3. logRevision 随事件走: 定价规则版本在 JSONL 事件上可辨, 但不进 Prometheus
//	   标签 (不在 llmComponentNeed 里, 无界基数纪律之外的序列身份一致性要求)。
//
// 同 llm_collector_test.go: 测试不碰包级全局 llmGlobal, 直接构造 Collector。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

// TestLLM采集_表面锚定总量守恒 —— Σ tokens == InputTokens, 逐组件可归因。
func TestLLM采集_表面锚定总量守恒(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(dir)
	ts := time.Now()

	// 真实 usage: InputTokens=4000, 组件构成里 skill 清单占启发式大头。
	recordLLMCall(c, api.LLMCallRecord{
		Model: "kimi-k3", Status: "success", Timestamp: ts,
		InputTokens: 4000, OutputTokens: 300,
		PromptComponents: api.PromptComponentMetrics{
			SystemChars: 8000, SkillListingChars: 4000, MessagesChars: 4000,
		},
	})

	surfaces := eventsNamed(readLLMEvents(t, dir), MLLMPromptSurfaceTokens)
	if len(surfaces) != 3 {
		t.Fatalf("3 个非零组件应各发 1 条表面样本, 实际 %d (%+v)", len(surfaces), surfaces)
	}
	sum := 0
	for _, e := range surfaces {
		sum += int(e.Value)
	}
	if sum != 4000 {
		t.Fatalf("Σ tokens 应恰等于真实 InputTokens (余数必须在分配处收敛), got %d want 4000", sum)
	}
	// 启发式占比 8000:4000:4000 = 2:1:1 → 锚定 2000:1000:1000
	for _, e := range surfaces {
		switch e.Labels["component"] {
		case "system_chars":
			if e.Value != 2000 {
				t.Fatalf("system 组件应分到 2000 (占比 2/4), got %v", e.Value)
			}
		case "skill_listing_chars", "messages_chars":
			if e.Value != 1000 {
				t.Fatalf("%s 应分到 1000, got %v", e.Labels["component"], e.Value)
			}
		default:
			t.Fatalf("意外组件 %q", e.Labels["component"])
		}
	}
}

// TestLLM采集_表面零组件与缺锚点不捏造 —— 两类缺失都不产生样本。
func TestLLM采集_表面零组件与缺锚点不捏造(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(dir)
	ts := time.Now()

	// 有锚点无组件: 没法分, 不发。
	recordLLMCall(c, api.LLMCallRecord{
		Model: "kimi-k3", Status: "success", Timestamp: ts, InputTokens: 1000,
	})
	// 有组件无锚点: 拿启发式冒充总量是被禁止的呈现, 不发。
	recordLLMCall(c, api.LLMCallRecord{
		Model: "kimi-k3", Status: "success", Timestamp: ts,
		PromptComponents: api.PromptComponentMetrics{SystemChars: 8000},
	})

	if got := len(eventsNamed(readLLMEvents(t, dir), MLLMPromptSurfaceTokens)); got != 0 {
		t.Fatalf("缺锚点/零组件都不应产出表面定价样本, 实际 %d", got)
	}
	// 对照: 启发式孪生照常发 (两条指标独立并存, 互不替代)。两次调用各发 9 条
	// (启发式恒全量记录含零值), 表面定价则一条不发。
	if got := len(eventsNamed(readLLMEvents(t, dir), MLLMPromptComponentTokens)); got != 18 {
		t.Fatalf("启发式分量应照常每调用 9 条全量记录 (含零值), 实际 %d", got)
	}
}

// TestLLM采集_表面logRevision随事件不进标签 —— 规格里的 logRevision 字段钉在
// JSONL 事件 labels 上, 但绝不进 Prometheus 序列身份。
func TestLLM采集_表面logRevision随事件不进标签(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(dir)
	ts := time.Now()

	recordLLMCall(c, api.LLMCallRecord{
		Model: "kimi-k3", Status: "success", Timestamp: ts,
		InputTokens: 2000, RunID: "run-f10",
		PromptComponents: api.PromptComponentMetrics{SystemChars: 4000},
	})

	evts := readLLMEvents(t, dir)
	surfaces := eventsNamed(evts, MLLMPromptSurfaceTokens)
	if len(surfaces) != 1 {
		t.Fatalf("应 1 条表面样本, 实际 %d", len(surfaces))
	}
	if got := surfaces[0].Labels["pricing_rev"]; got != "1" {
		t.Fatalf("pricing_rev 应为 %q (TokenPricingRevision), got %q", "1", got)
	}
	if _, ok := surfaces[0].Labels["run_id"]; ok {
		t.Fatalf("run_id 不进标签 (同启发式孪生), labels=%v", surfaces[0].Labels)
	}
	if surfaces[0].RunID != "run-f10" {
		t.Fatalf("run_id 应进事件字段, got %q", surfaces[0].RunID)
	}
}

// TestLLM采集_分配函数余数修正 —— AllocatePromptSurface 的单元级不变式:
// 任意构成下 Σ tokens == anchorTokens, 且占比与启发式份额单调一致。
func TestLLM采集_分配函数余数修正(t *testing.T) {
	cases := []struct {
		name   string
		pc     api.PromptComponentMetrics
		anchor int
	}{
		{"整除", api.PromptComponentMetrics{SystemChars: 8000, MessagesChars: 4000}, 3000},
		{"难整除", api.PromptComponentMetrics{SystemChars: 3333, MessagesChars: 777, MemoryChars: 111}, 1000},
		{"单组件", api.PromptComponentMetrics{SystemChars: 4000}, 999},
		{"全组件", api.PromptComponentMetrics{
			SystemChars: 1000, ToolsSchemaChars: 900, MCPToolsChars: 800, SkillListingChars: 700,
			RoleSkillsChars: 600, MemoryChars: 500, BlackboardChars: 400, PrevResultChars: 300,
			MessagesChars: 200,
		}, 12345},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := api.AllocatePromptSurface(tc.pc, tc.anchor, false)
			if m.PricingRevision != api.TokenPricingRevision {
				t.Fatalf("PricingRevision 应为 %d, got %d", api.TokenPricingRevision, m.PricingRevision)
			}
			sum := 0
			for _, n := range m.Surfaces {
				sum += n.Tokens
			}
			if sum != tc.anchor {
				t.Fatalf("Σ tokens = %d, want %d (余数修正失效)", sum, tc.anchor)
			}
			// 确定性: 同输入两次分配结果逐字段一致。
			m2 := api.AllocatePromptSurface(tc.pc, tc.anchor, false)
			a, _ := json.Marshal(m.Surfaces)
			b, _ := json.Marshal(m2.Surfaces)
			if string(a) != string(b) {
				t.Fatalf("分配非确定性: %s vs %s", a, b)
			}
			// SharePct 一位小数; 逐组件独立截断, 误差上界 = 0.1×组件数 (锚定
			// tokens 本身已精确守恒, 这里只检呈现层不越界、不跑飞)。
			pctSum := 0.0
			for _, n := range m.Surfaces {
				if n.SharePct < 0 || n.SharePct > 100 {
					t.Fatalf("SharePct 越界: %v", n.SharePct)
				}
				pctSum += n.SharePct
			}
			if pctSum < 99.0 || pctSum > 100.1 {
				t.Fatalf("SharePct 总和应 ≈100 (±0.1×组件数), got %v", pctSum)
			}
			// 组件字典序 (确定性输出)。
			for i := 1; i < len(m.Surfaces); i++ {
				if strings.Compare(m.Surfaces[i-1].Component, m.Surfaces[i].Component) >= 0 {
					t.Fatalf("Surfaces 应按 Component 字典序: %+v", m.Surfaces)
				}
			}
		})
	}

	// 零组件与零锚点: 键缺失, Surfaces=nil 而非全员 0。
	if m := api.AllocatePromptSurface(api.PromptComponentMetrics{}, 500, false); m.Surfaces != nil {
		t.Fatalf("零组件应返回 nil Surfaces (不捏造), got %+v", m.Surfaces)
	}
	if m := api.AllocatePromptSurface(api.PromptComponentMetrics{SystemChars: 100}, 0, false); m.Surfaces != nil {
		t.Fatalf("零锚点应返回 nil Surfaces (不捏造), got %+v", m.Surfaces)
	}
}
