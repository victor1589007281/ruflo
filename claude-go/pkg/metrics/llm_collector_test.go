package metrics

// llm_collector_test.go —— 钉住 design/02 §1.4 与 design/03 §1.3 两条承诺的**兑现形态**。
//
// 这个包改造前**一个测试文件都没有**, 而本轮改的两件事都属于"看着有数据其实半边是死的"
// 那一类, 光靠读代码判不出来:
//
//	§1.4  input token 的 `>0` 守卫 —— 注释写着"不再用守卫", 守卫却还在 (真实存在过的
//	      注释/代码相反), 表现是 llm_input_tokens 的样本数**少于**调用数, 而按样本数
//	      算的均值被系统性抬高。
//	§1.3  run_id 要进 llm.jsonl 但**绝不能**进 Prometheus 标签 (无界基数)。这两个方向
//	      的错误症状完全不同: 少了前者是"跨源对不上账", 多了后者是"指标炸成几千条序列"。
//
// 测试**不碰包级全局** `llmGlobal`(以及它的后台 restore goroutine): 直接构造
// Collector 并调 recordLLMCall。本仓吃过三次"非密闭测试"的亏 —— 组件活到测试边界
// 之外, 症状是偶发变红。

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

// readLLMEvents 读出 <stateDir>/metrics/llm.jsonl 的全部事件。
func readLLMEvents(t *testing.T, stateDir string) []MetricEvent {
	t.Helper()
	f, err := os.Open(filepath.Join(stateDir, "metrics", "llm.jsonl"))
	if err != nil {
		t.Fatalf("打开 llm.jsonl 失败: %v", err)
	}
	defer f.Close()
	var out []MetricEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var evt MetricEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			t.Fatalf("解析 llm.jsonl 行失败: %v (行: %s)", err, line)
		}
		out = append(out, evt)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("扫描 llm.jsonl 失败: %v", err)
	}
	return out
}

func eventsNamed(evts []MetricEvent, name string) []MetricEvent {
	var out []MetricEvent
	for _, e := range evts {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// TestLLM采集_input为0也记账 —— design/02 §1.4 的 `>0` 守卫已去掉。
//
// 断言的是**样本数与调用数相等**, 而不是"有一条 value=0 的事件": 后者用
// `if rec.InputTokens >= 0` 这种假修复也能过, 而真正的缺陷是样本数对不上调用数。
func TestLLM采集_input为0也记账(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(dir)
	ts := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	// 两次调用: 一次网关回了 input, 一次没回 (Kimi 类网关的常态)。
	recordLLMCall(c, api.LLMCallRecord{
		Model: "kimi-k3", Status: "success", Timestamp: ts,
		InputTokens: 1200, OutputTokens: 300,
	})
	recordLLMCall(c, api.LLMCallRecord{
		Model: "kimi-k3", Status: "success", Timestamp: ts,
		InputTokens: 0, OutputTokens: 300,
	})

	evts := readLLMEvents(t, dir)
	calls := eventsNamed(evts, MLLMCallCount)
	inputs := eventsNamed(evts, MLLMInputTokens)
	if len(calls) != 2 {
		t.Fatalf("应有 2 条调用样本, 实际 %d", len(calls))
	}
	// 核心断言: input 样本数 == 调用数。守卫在时这里是 1 != 2。
	if len(inputs) != len(calls) {
		t.Fatalf("input token 样本数应与调用数相等 (否则按样本数算的均值被系统性抬高), got %d want %d",
			len(inputs), len(calls))
	}
	var zeros int
	for _, e := range inputs {
		if e.Value == 0 {
			zeros++
		}
	}
	if zeros != 1 {
		t.Fatalf("应恰有 1 条 input=0 的显式记账, 实际 %d", zeros)
	}

	// 对照: output 为 0 时**仍然跳过**(为 0 是"确实没有该项", 不是信息缺失)。
	// 这一条防的是"顺手把四个守卫一起删了"——那会给每次调用凭空多出几条恒零样本。
	recordLLMCall(c, api.LLMCallRecord{
		Model: "kimi-k3", Status: "error", Timestamp: ts,
		InputTokens: 500, OutputTokens: 0,
	})
	evts = readLLMEvents(t, dir)
	if got := len(eventsNamed(evts, MLLMOutputTokens)); got != 2 {
		t.Fatalf("output=0 的那次不该产生样本 (应仍为 2 条), 实际 %d", got)
	}
}

// TestLLM采集_估算值可区分 —— InputEstimated 必须在标签上可见。
//
// 不可见的后果是具体的: 有人拿 llm_input_tokens 去和账单对账, 而其中一部分是
// 按字符数估出来的。
func TestLLM采集_估算值可区分(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(dir)
	ts := time.Now()

	recordLLMCall(c, api.LLMCallRecord{
		Model: "kimi-k3", Status: "success", Timestamp: ts,
		InputTokens: 800, InputEstimated: true,
	})
	recordLLMCall(c, api.LLMCallRecord{
		Model: "kimi-k3", Status: "success", Timestamp: ts,
		InputTokens: 800, InputEstimated: false,
	})

	inputs := eventsNamed(readLLMEvents(t, dir), MLLMInputTokens)
	if len(inputs) != 2 {
		t.Fatalf("应有 2 条 input 样本, 实际 %d", len(inputs))
	}
	if got := inputs[0].Labels["input_estimated"]; got != "1" {
		t.Fatalf("估算的那条应带 input_estimated=1, got %q", got)
	}
	// 未估算时**一个字段都不加**: 加了 "0" 会让所有既有部署的标签集变形,
	// 而既有 Grafana 查询是按精确标签集匹配的。
	if _, ok := inputs[1].Labels["input_estimated"]; ok {
		t.Fatalf("未估算时不应出现 input_estimated 标签, 实际 labels=%v", inputs[1].Labels)
	}
}

// TestLLM采集_run_id进事件但不进标签 —— design/03 §1.3 E0 验收项。
//
// 两个方向都要钉: 少了 run_id ⇒ llm.jsonl 与团队轨迹只能按时间戳粗对齐 (E0 不达成);
// run_id 进了标签 ⇒ Prometheus 每次运行多一条时间序列 (无界基数), 且既有查询的聚合
// 口径静默改变。
func TestLLM采集_runID进事件但不进标签(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(dir)
	ts := time.Now()

	recordLLMCall(c, api.LLMCallRecord{
		Model: "kimi-k3", Status: "success", Timestamp: ts,
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
		Retries: 1, GuardWaitSec: 0.5, CircuitBlocked: true,
		RunID: "run-abc", NodeID: "writer", TurnID: "t1", CallID: "c1",
		PromptComponents: api.PromptComponentMetrics{SystemChars: 40},
	})

	evts := readLLMEvents(t, dir)
	if len(evts) == 0 {
		t.Fatal("一条事件都没写出来")
	}
	for _, e := range evts {
		if e.RunID != "run-abc" {
			// 逐条检查而不是只看第一条: 本轮真实存在过"有的带有的不带"的中间状态
			// (熔断那几条与提示词构成那几条用的是另一份 labels), 而按 run_id 过滤时
			// 漏掉的恰好是最该被查到的几条。
			t.Fatalf("事件 %s 的 run_id 应为 run-abc, got %q", e.Name, e.RunID)
		}
		if v, ok := e.Labels["run_id"]; ok {
			t.Fatalf("run_id 绝不能进标签 (无界基数), 事件 %s 上出现了 run_id=%q", e.Name, v)
		}
		// node/turn/call 同理: 它们比 run_id 更细, 进标签更糟。
		for _, k := range []string{"node_id", "turn_id", "call_id"} {
			if _, ok := e.Labels[k]; ok {
				t.Fatalf("%s 不该出现在标签里, 事件 %s labels=%v", k, e.Name, e.Labels)
			}
		}
	}

	// run_id 为空时 omitempty 生效 —— 非团队路径的 llm.jsonl 逐字节与改造前一致。
	dir2 := t.TempDir()
	c2 := NewCollector(dir2)
	recordLLMCall(c2, api.LLMCallRecord{Model: "kimi-k3", Status: "success", Timestamp: ts})
	raw, err := os.ReadFile(filepath.Join(dir2, "metrics", "llm.jsonl"))
	if err != nil {
		t.Fatalf("读 llm.jsonl 失败: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("llm.jsonl 为空")
	}
	if idx := indexOf(raw, "run_id"); idx >= 0 {
		t.Fatalf("run_id 为空时不该出现 run_id 字段 (omitempty), 实际: %s", raw)
	}
}

func indexOf(hay []byte, needle string) int {
	n := []byte(needle)
	for i := 0; i+len(n) <= len(hay); i++ {
		match := true
		for j := range n {
			if hay[i+j] != n[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
