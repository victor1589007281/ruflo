package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestAnthropicToOpenAI(t *testing.T) {
	req := types.APIRequest{
		Model:     "mimo-v2.5",
		MaxTokens: 1024,
		Stream:    true,
		System:    "你是助手",
		Messages: []types.APIMessage{
			// assistant: tool_use
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_1","name":"calc","input":{"a":2,"b":2}}]`)},
			// user: tool_result + text
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"4"},{"type":"text","text":"and now?"}]`)},
		},
		Tools: []types.APITool{
			{Name: "calc", Description: "add two numbers", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"}}}`)},
		},
		ToolChoice: &types.APIToolChoice{Type: "any"},
	}
	oreq, err := anthropicToOpenAI(req, true, "") // 空 reasoningPlayback = 不回传 (默认行为)
	if err != nil {
		t.Fatalf("anthropicToOpenAI: %v", err)
	}
	if len(oreq.Messages) != 4 {
		t.Fatalf("want 4 messages (system/user-tool/user-tool/assistant), got %d: %+v", len(oreq.Messages), oreq.Messages)
	}
	if oreq.Messages[0].Role != "system" || oreq.Messages[0].Content != "你是助手" {
		t.Fatalf("system message wrong: %+v", oreq.Messages[0])
	}
	if oreq.Messages[1].Role != "assistant" {
		t.Fatalf("msg1 want assistant, got %+v", oreq.Messages[1])
	}
	if len(oreq.Messages[1].ToolCalls) != 1 || oreq.Messages[1].ToolCalls[0].ID != "toolu_1" || oreq.Messages[1].ToolCalls[0].Function.Name != "calc" {
		t.Fatalf("assistant tool_calls wrong: %+v", oreq.Messages[1].ToolCalls)
	}
	if oreq.Messages[2].Role != "tool" || oreq.Messages[2].ToolCallID != "toolu_1" {
		t.Fatalf("tool message wrong: %+v", oreq.Messages[2])
	}
	if oreq.Messages[3].Role != "user" {
		t.Fatalf("msg3 want user, got %+v", oreq.Messages[3])
	}
	if oreq.Tools[0].Type != "function" || oreq.Tools[0].Function.Name != "calc" {
		t.Fatalf("tools wrong: %+v", oreq.Tools)
	}
	if oreq.ToolChoice != "required" {
		t.Fatalf("tool_choice any -> required, got %v", oreq.ToolChoice)
	}
	if oreq.StreamOptions == nil || !oreq.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options.include_usage not set")
	}
	b, _ := json.Marshal(oreq)
	if !strings.Contains(string(b), "chat") && !strings.Contains(string(b), "stream_options") {
		t.Fatalf("marshaled body missing fields: %s", b)
	}
	t.Logf("BODY: %s", b)
}

func TestParseOpenAIResponse(t *testing.T) {
	raw := `{"id":"chatcmpl-x","model":"mimo-v2.5","choices":[{"index":0,"message":{"role":"assistant","content":"Let me calculate","tool_calls":[{"id":"call_1","type":"function","function":{"name":"calc","arguments":"{\"a\":2,\"b\":2}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`
	resp, err := parseOpenAIResponse("mimo-v2.5", []byte(raw))
	if err != nil {
		t.Fatalf("parseOpenAIResponse: %v", err)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop_reason want tool_use, got %s", resp.StopReason)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("want 2 blocks (text + tool_use), got %d", len(resp.Content))
	}
	if resp.Content[0].Type != types.ContentBlockText || resp.Content[0].Text != "Let me calculate" {
		t.Fatalf("text block wrong: %+v", resp.Content[0])
	}
	if resp.Content[1].Type != types.ContentBlockToolUse || resp.Content[1].Name != "calc" || resp.Content[1].ID != "call_1" {
		t.Fatalf("tool_use block wrong: %+v", resp.Content[1])
	}
	if string(resp.Content[1].Input) != `{"a":2,"b":2}` {
		t.Fatalf("input wrong: %s", resp.Content[1].Input)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 20 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
}

func TestOpenAIStreamTranslator(t *testing.T) {
	tr := newOpenAIStreamTranslator("mimo-v2.5")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"Think"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"calc","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":2,\"b\":2}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":99,"completion_tokens":30}}`,
	}
	var all []types.StreamDelta
	for _, c := range chunks {
		evs, err := tr.translate([]byte(c))
		if err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
		all = append(all, evs...)
	}
	// 事件类型序列
	var seq []string
	for _, e := range all {
		seq = append(seq, e.Type)
		if e.Type == "content_block_start" && e.ContentBlock != nil {
			seq = append(seq, "["+string(e.ContentBlock.Type)+"]")
		}
	}
	want := "message_start, content_block_start, [text], content_block_delta, content_block_stop, content_block_start, [tool_use], content_block_delta, content_block_stop, message_delta"
	if got := strings.Join(seq, ", "); got != want {
		t.Fatalf("event sequence wrong:\n got: %s\nwant: %s", got, want)
	}
	last := all[len(all)-1]
	if last.Delta == nil || last.Delta.StopReason != "tool_use" {
		t.Fatalf("last event stop_reason wrong: %+v", last)
	}
	if last.Usage == nil || last.Usage.InputTokens != 99 {
		t.Fatalf("usage not propagated: %+v", last.Usage)
	}
	// 工具块 Input 累积校验: 通过模拟引擎消费重放
	var curInput strings.Builder
	for _, e := range all {
		if e.Delta != nil && e.Delta.Type == "input_json_delta" {
			curInput.WriteString(e.Delta.PartialJSON)
		}
	}
	if curInput.String() != `{"a":2,"b":2}` {
		t.Fatalf("accumulated tool input wrong: %q", curInput.String())
	}
}

// TestOpenAIStreamTranslatorUsageTailFrame 覆盖 opencode/omen/hy3 的上游行为:
// usage 不在 finish_reason 帧里, 而在其后的 {choices:[],usage:{...}} 独立尾帧。
// message_delta 必须挂起到尾帧吸收 usage 后补发, 且 stop_reason/usage 都正确。
func TestOpenAIStreamTranslatorUsageTailFrame(t *testing.T) {
	tr := newOpenAIStreamTranslator("omen-alpha")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,       // finish 帧无 usage
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":500,"completion_tokens":88}}`, // 独立 usage 尾帧
	}
	var all []types.StreamDelta
	for _, c := range chunks {
		evs, err := tr.translate([]byte(c))
		if err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
		all = append(all, evs...)
	}
	last := all[len(all)-1]
	if last.Type != "message_delta" {
		t.Fatalf("last event want message_delta, got %s", last.Type)
	}
	if last.Delta == nil || last.Delta.StopReason != "max_tokens" {
		t.Fatalf("stop_reason want max_tokens (finish length), got %+v", last.Delta)
	}
	if last.Usage == nil || last.Usage.InputTokens != 500 || last.Usage.OutputTokens != 88 {
		t.Fatalf("usage not propagated from tail frame: %+v", last.Usage)
	}
	// message_delta 发完即 finished: 后续帧被忽略
	if evs, _ := tr.translate([]byte(`{"id":"c1","usage":{"prompt_tokens":9}}`)); len(evs) != 0 {
		t.Fatalf("expected no events after finished, got %d", len(evs))
	}
	// Flush 此时应为 no-op (未挂起)
	if evs := tr.Flush(); len(evs) != 0 {
		t.Fatalf("Flush after tail-frame finish should be empty, got %d", len(evs))
	}
}

// TestOpenAIStreamTranslatorFlushNoUsageTail 覆盖上游忽略 include_usage、始终不送
// usage 尾帧的退化情形: Flush() 兜底补发 message_delta, 保证引擎收到 stop_reason。
func TestOpenAIStreamTranslatorFlushNoUsageTail(t *testing.T) {
	tr := newOpenAIStreamTranslator("omen-alpha")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		// 没有 usage-only 尾帧, 直接 EOF
	}
	for _, c := range chunks {
		if _, err := tr.translate([]byte(c)); err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
	}
	evs := tr.Flush()
	if len(evs) != 1 {
		t.Fatalf("Flush want 1 deferred message_delta, got %d", len(evs))
	}
	if evs[0].Type != "message_delta" || evs[0].Delta == nil || evs[0].Delta.StopReason != "end_turn" {
		t.Fatalf("flush delta wrong: %+v", evs[0])
	}
	// 第二次 Flush 应为 no-op
	if evs := tr.Flush(); len(evs) != 0 {
		t.Fatalf("second Flush should be empty, got %d", len(evs))
	}
}

// TestOpenAIStreamTranslatorMultiToolCalls 一个回合里 ≥2 个工具调用必须串行开块。
//
// 这是实测事故的回归测试, 不是假想的边界: opencode:space-bunny-free 在"实现 Greet()"
// 这种普通任务的首个回合就同时返回 Glob(index=0) 与 Shell(index=1)。
//
// 修复前的失败形态 (逐字节复现过): 翻译器把两个 content_block_stop 都推迟到
// finish_reason, 于是两个工具块**同时开着**。engine.go:1516 的 content_block_start
// 会覆盖 currentBlock 并清空缓冲, 结果 assistant 消息里变成
//
//	Shell(id=call_2)  ← Glob 被丢掉
//	Shell(id=call_2)  ← 最后一个被复制
//
// 两条 tool 结果于是带上同一个 tool_call_id, 上游判 400 invalid request; 而引擎把
// 这个 400 吞掉之后以"模型说了句话"收尾 —— exit 0、is_error=false、实际一行没改。
//
// 因此断言必须落在**消费端形状**上 (引擎看到的块序列), 而不是"事件个数对得上":
// 个数在"丢一个+复制一个"的形态下完全可能还是对的。
func TestOpenAIStreamTranslatorMultiToolCalls(t *testing.T) {
	tr := newOpenAIStreamTranslator("space-bunny-free")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"先看看项目结构。"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Glob","arguments":"{\"glob_pattern\": \"**/*.go\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"Shell","arguments":"{\"command\": \"ls -la\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":99,"completion_tokens":30}}`,
	}
	var all []types.StreamDelta
	for _, c := range chunks {
		evs, err := tr.translate([]byte(c))
		if err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
		all = append(all, evs...)
	}

	// 按引擎的语义重放: 它只跟踪一个当前块, start 覆盖 currentBlock, stop 才落盘。
	type blk struct {
		kind string
		id   string
		name string
		text string
		args strings.Builder
	}
	var blocks []*blk
	var cur *blk
	openDepth := 0
	for _, e := range all {
		switch e.Type {
		case "content_block_start":
			if openDepth != 0 {
				t.Fatalf("前一个块还没关就又开了新块 (index=%d) —— 引擎的 currentBlock 会被覆盖, "+
					"前一个块整块丢失; 这正是多工具调用那个 400 的成因。事件序列: %s",
					e.Index, describeDeltas(all))
			}
			openDepth++
			cur = &blk{}
			if e.ContentBlock != nil {
				cur.kind = string(e.ContentBlock.Type)
				cur.id = e.ContentBlock.ID
				cur.name = e.ContentBlock.Name
			}
		case "content_block_delta":
			if openDepth == 0 {
				t.Fatalf("没有开着的块却收到 delta (index=%d)", e.Index)
			}
			if e.Delta != nil {
				switch e.Delta.Type {
				case "text_delta":
					cur.text += e.Delta.Text
				case "input_json_delta":
					cur.args.WriteString(e.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if openDepth == 0 {
				t.Fatalf("块已经关了却又收到 content_block_stop (index=%d)", e.Index)
			}
			openDepth--
			blocks = append(blocks, cur)
			cur = nil
		}
	}
	if openDepth != 0 {
		t.Fatalf("流结束时还有 %d 个块没关", openDepth)
	}

	// 必须恰好是: 文本块 + Glob + Shell, 顺序与内容都对。
	var toolIDs, toolNames []string
	for _, b := range blocks {
		if b.kind == "tool_use" {
			toolIDs = append(toolIDs, b.id)
			toolNames = append(toolNames, b.name)
		}
	}
	if len(toolNames) != 2 {
		t.Fatalf("工具块数 = %d (%v), 期望 2 —— 丢一个/复制一个都会让这个数看着"+
			"「差不多」, 所以这里同时核名字与 id 的**集合**", len(toolNames), toolNames)
	}
	if toolNames[0] != "Glob" || toolNames[1] != "Shell" {
		t.Fatalf("工具块名字/顺序 = %v, 期望 [Glob Shell]", toolNames)
	}
	if toolIDs[0] != "call_1" || toolIDs[1] != "call_2" {
		t.Fatalf("工具块 id = %v, 期望 [call_1 call_2] —— 两个 tool 结果必须能各自对上"+
			"一个 id, 重复 id 会被上游判 invalid request", toolIDs)
	}
	if toolIDs[0] == toolIDs[1] {
		t.Fatal("两个工具块的 id 相同 —— assistant 消息里会出现两条同 id 的 tool_calls")
	}

	// 每个工具块只拿到自己的参数 (互相串了会体现在这里)。
	if got := blocks[1].args.String(); got != `{"glob_pattern": "**/*.go"}` {
		t.Fatalf("Glob 的入参 = %q", got)
	}
	if got := blocks[2].args.String(); got != `{"command": "ls -la"}` {
		t.Fatalf("Shell 的入参 = %q", got)
	}
	// 交错期之前的正文不能被工具块吞掉。
	if blocks[0].kind != "text" || !strings.Contains(blocks[0].text, "先看看项目结构") {
		t.Fatalf("首个块应为正文, 实得 kind=%q text=%q", blocks[0].kind, blocks[0].text)
	}

	last := all[len(all)-1]
	if last.Delta == nil || last.Delta.StopReason != "tool_use" {
		t.Fatalf("最后一个事件的 stop_reason = %+v, 期望 tool_use", last)
	}
}

// replayAnthropicBlocks 按**引擎的语义**重放事件序列 (pkg/engine/engine.go):
// 只跟踪一个当前块, content_block_start 覆盖 currentBlock 并清空缓冲,
// content_block_stop 才落盘。翻译器若同时开两个块, 这里会当场报错 —— 断言落在
// 消费端形状上, 而不是"事件个数对得上" (丢一个+复制一个时个数可能还是对的)。
func replayAnthropicBlocks(t *testing.T, evs []types.StreamDelta) []*replayedBlock {
	t.Helper()
	var blocks []*replayedBlock
	var cur *replayedBlock
	openDepth := 0
	for _, e := range evs {
		switch e.Type {
		case "content_block_start":
			if openDepth != 0 {
				t.Fatalf("前一个块还没关就又开了新块 (index=%d) —— 引擎的 currentBlock 会被覆盖, "+
					"前一个块整块丢失。事件序列: %s", e.Index, describeDeltas(evs))
			}
			openDepth++
			cur = &replayedBlock{}
			if e.ContentBlock != nil {
				cur.kind = string(e.ContentBlock.Type)
				cur.id = e.ContentBlock.ID
				cur.name = e.ContentBlock.Name
			}
		case "content_block_delta":
			if openDepth == 0 {
				t.Fatalf("没有开着的块却收到 delta (index=%d)", e.Index)
			}
			if e.Delta != nil {
				switch e.Delta.Type {
				case "text_delta":
					cur.text += e.Delta.Text
				case "thinking_delta":
					cur.thinking += e.Delta.Thinking
				case "input_json_delta":
					cur.args.WriteString(e.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if openDepth == 0 {
				t.Fatalf("块已经关了却又收到 content_block_stop (index=%d)", e.Index)
			}
			openDepth--
			blocks = append(blocks, cur)
			cur = nil
		}
	}
	if openDepth != 0 {
		t.Fatalf("流结束时还有 %d 个块没关", openDepth)
	}
	return blocks
}

type replayedBlock struct {
	kind     string
	id       string
	name     string
	text     string
	thinking string
	args     strings.Builder
}

// TestOpenAIStreamTranslatorReasoningThinking 是本次修复的主回归: 上游把思维链放在
// delta.reasoning, 而 Anthropic 侧必须变成 thinking 块, 否则轨迹里只剩一个正文块 ——
// opencode:step-5-preview-free 这种把大半个 max_tokens 烧在推理上的模型, 看起来就像
// "什么都没说"。
//
// 第二个断言同样关键: opencode 每帧**同时**给 delta.reasoning (增量串) 和
// delta.reasoning_details[0].text (**同一段文字**, 不是两份内容)。两个字段都发就会
// 让思维链在轨迹里翻倍。
func TestOpenAIStreamTranslatorReasoningThinking(t *testing.T) {
	tr := newOpenAIStreamTranslator("step-5-preview-free")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning":"Let","reasoning_details":[{"type":"reasoning.text","text":"Let","format":"unknown","index":0}]},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning":" me think","reasoning_details":[{"type":"reasoning.text","text":" me think","format":"unknown","index":1}]},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"光合作用"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":40}}`,
	}
	var all []types.StreamDelta
	for _, c := range chunks {
		evs, err := tr.translate([]byte(c))
		if err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
		all = append(all, evs...)
	}
	blocks := replayAnthropicBlocks(t, all)

	if len(blocks) != 2 {
		t.Fatalf("块数 = %d, 期望 2 (thinking + text): %s", len(blocks), describeDeltas(all))
	}
	if blocks[0].kind != string(types.ContentBlockThinking) {
		t.Fatalf("首个块应为 thinking, 实得 %q —— 上游先推理后作答, 顺序必须保持 (轨迹的"+
			" ExtractPlan 取首个 thinking 块)", blocks[0].kind)
	}
	// thinking 与 reasoning_details 内容重复, 各分片只能发一次。
	if got := blocks[0].thinking; got != "Let me think" {
		t.Fatalf("thinking 内容 = %q, 期望 \"Let me think\" (reasoning 与 reasoning_details "+
			"同帧重复, 叠加会变成 %q)", got, "LetLet me thinkme think")
	}
	if blocks[1].kind != string(types.ContentBlockText) || blocks[1].text != "光合作用" {
		t.Fatalf("正文块 = %+v, 期望 text/光合作用", blocks[1])
	}
	// 不能合成 signature: 上游没有该字段, 造一个假签名只会让下游校验失败。
	for _, e := range all {
		if e.Delta != nil && e.Delta.Type == "signature_delta" {
			t.Fatal("合成了 signature_delta, 但上游响应里没有任何签名/加密推理字段")
		}
	}
}

// TestOpenAIStreamTranslatorReasoningSnapshot 覆盖智谱 GLM 一类上游: delta.reasoning
// 是**累计快照** (chunk1=" Think", chunk2=" Think harder"), 不是增量。
// 按增量整段发会让思维链呈平方级重复。判据是内容前缀关系, 不是字段名。
func TestOpenAIStreamTranslatorReasoningSnapshot(t *testing.T) {
	tr := newOpenAIStreamTranslator("glm-5.1")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning":" Think"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning":" Think harder"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning":" Think harder! done"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
	}
	var all []types.StreamDelta
	for _, c := range chunks {
		evs, err := tr.translate([]byte(c))
		if err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
		all = append(all, evs...)
	}
	blocks := replayAnthropicBlocks(t, all)
	if len(blocks) != 2 || blocks[0].kind != string(types.ContentBlockThinking) {
		t.Fatalf("块形状不对: %s", describeDeltas(all))
	}
	if got := blocks[0].thinking; got != " Think harder! done" {
		t.Fatalf("thinking = %q, 期望 \" Think harder! done\" —— 累计快照只该发多出来的那一截, "+
			"整段重发会得到 %q", got, " Think Think harder Think harder! done")
	}
}

// TestOpenAIStreamTranslatorReasoningThenTool 思维链与工具调用同回合: thinking 块必须
// 在工具块之前关掉。这条走的是与多工具调用同一个引擎不变量 (只跟踪一个当前块),
// 但触发路径不同 —— 之前 thinking 压根不开, 所以没人验证过它关不关。
func TestOpenAIStreamTranslatorReasoningThenTool(t *testing.T) {
	tr := newOpenAIStreamTranslator("step-5-preview-free")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning":"先看目录"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Glob","arguments":"{\"glob_pattern\":\"**/*.go\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":9}}`,
	}
	var all []types.StreamDelta
	for _, c := range chunks {
		evs, err := tr.translate([]byte(c))
		if err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
		all = append(all, evs...)
	}
	blocks := replayAnthropicBlocks(t, all)
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d, 期望 2 (thinking + tool_use): %s", len(blocks), describeDeltas(all))
	}
	if blocks[0].kind != string(types.ContentBlockThinking) || blocks[0].thinking != "先看目录" {
		t.Fatalf("thinking 块 = %+v", blocks[0])
	}
	if blocks[1].kind != string(types.ContentBlockToolUse) || blocks[1].name != "Glob" {
		t.Fatalf("工具块 = %+v", blocks[1])
	}
	if got := blocks[1].args.String(); got != `{"glob_pattern":"**/*.go"}` {
		t.Fatalf("工具入参 = %q", got)
	}
	// thinking 块不能被工具块覆盖: 引擎的 currentBlock 语义下, 忘了关 thinking 会让
	// 推理文字落进工具块的 input 里 (或整块消失)。
	if all[len(all)-1].Delta == nil || all[len(all)-1].Delta.StopReason != "tool_use" {
		t.Fatalf("stop_reason 不对: %+v", all[len(all)-1])
	}
}

// TestOpenAIStreamTranslatorReasoningAfterText 上游先发正文再发推理 (非标准但存在:
// 某些代理会把 reasoning 重排到尾部)。此时 thinking 块必须关掉先前的正文块, 且
// 两个块都得留住。
func TestOpenAIStreamTranslatorReasoningAfterText(t *testing.T) {
	tr := newOpenAIStreamTranslator("glm-5.1")
	chunks := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"正文"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning":"事后补的推理"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
	}
	var all []types.StreamDelta
	for _, c := range chunks {
		evs, err := tr.translate([]byte(c))
		if err != nil {
			t.Fatalf("translate %q: %v", c, err)
		}
		all = append(all, evs...)
	}
	blocks := replayAnthropicBlocks(t, all)
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d, 期望 2: %s", len(blocks), describeDeltas(all))
	}
	if blocks[0].kind != string(types.ContentBlockText) || blocks[0].text != "正文" {
		t.Fatalf("文本块 = %+v", blocks[0])
	}
	if blocks[1].kind != string(types.ContentBlockThinking) || blocks[1].thinking != "事后补的推理" {
		t.Fatalf("thinking 块 = %+v", blocks[1])
	}
}

// TestParseOpenAIResponseReasoningThinking 非流式路径: thinking 块必须排在正文前,
// 否则 pkg/engine/runner.go 的 ExtractPlan (取首个 thinking 块) 拿不到东西, 且
// thinking-only 回合会被 runner.go:428 的"空回复"判据判死。
func TestParseOpenAIResponseReasoningThinking(t *testing.T) {
	raw := `{"id":"chatcmpl-r","model":"step-5-preview-free","choices":[{"index":0,"message":{"role":"assistant","content":"9","reasoning":"设 x 为所求, x 的三倍是 ","reasoning_details":[{"type":"reasoning.text","text":"27, 所以 x=9","index":0}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":60}}`
	resp, err := parseOpenAIResponse("step-5-preview-free", []byte(raw))
	if err != nil {
		t.Fatalf("parseOpenAIResponse: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("块数 = %d, 期望 2 (thinking + text): %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Type != types.ContentBlockThinking {
		t.Fatalf("首块应为 thinking, 实得 %q", resp.Content[0].Type)
	}
	// reasoning 是完整思维链, reasoning_details[0] 只是它的一片 —— 取最长的那一个,
	// 不能把两段拼起来 (拼出来是 "设 x 为所求, x 的三倍是 27, 所以 x=927, 所以 x=9")。
	if resp.Content[0].Thinking != "设 x 为所求, x 的三倍是 " {
		t.Fatalf("thinking 内容 = %q, 期望 \"设 x 为所求, x 的三倍是 \" (details 是主字段的**子串**, "+
			"不是独立内容)", resp.Content[0].Thinking)
	}
	if resp.Content[1].Type != types.ContentBlockText || resp.Content[1].Text != "9" {
		t.Fatalf("正文块 = %+v", resp.Content[1])
	}
}

// TestOpenAIContentToBlocksReasoningOnly 只有推理没有正文的回合: thinking 块必须独立
// 存在。此前它会被整段丢掉, assistant 内容为 nil, runner 报 "assistant returned
// empty content" —— 而那正是一个"模型想了很多但还没开口"的正常中间态。
func TestOpenAIContentToBlocksReasoningOnly(t *testing.T) {
	blocks := openAIContentToBlocks(json.RawMessage(`""`), nil, "只推理不答话")
	if len(blocks) != 1 || blocks[0].Type != types.ContentBlockThinking || blocks[0].Thinking != "只推理不答话" {
		t.Fatalf("reasoning-only 应产出单个 thinking 块, 实得 %+v", blocks)
	}
	// 没有推理时行为不能变 (旧路径的回归)。
	plain := openAIContentToBlocks(json.RawMessage(`"hi"`), nil, "")
	if len(plain) != 1 || plain[0].Type != types.ContentBlockText || plain[0].Text != "hi" {
		t.Fatalf("无推理时行为回归: %+v", plain)
	}
}

// describeDeltas 把事件序列压成一行, 失败时能直接看出形状。
func describeDeltas(evs []types.StreamDelta) string {
	var parts []string
	for _, e := range evs {
		switch e.Type {
		case "content_block_start":
			if e.ContentBlock != nil {
				parts = append(parts, "start:"+string(e.ContentBlock.Type)+"/"+e.ContentBlock.Name)
			} else {
				parts = append(parts, "start:?")
			}
		case "content_block_delta":
			if e.Delta != nil {
				parts = append(parts, "delta:"+e.Delta.Type)
			}
		case "content_block_stop":
			parts = append(parts, "stop")
		default:
			parts = append(parts, e.Type)
		}
	}
	return strings.Join(parts, " ")
}

// --- 出站回传上一轮推理 (ReasoningPlayback) -------------------------------
//
// 背景: 网关是无状态的, 每一轮都把完整 messages[] 重新 POST 给上游
// (runner.go:310), 所以上游从来没"见过"上一轮的思维链 —— 不是忘了, 是根本没收到。
// 回传开关让那段已被网关收到并存进本地历史的数据继续往下游走。
//
// 这一组用例钉住三件事:
//   1. 空值 = 一个字节都不多发 (与改造前逐字节相同);
//   2. "reasoning" / "reasoning_content" 各写各的字段, 互不串台;
//   3. 整段重发而非只发新片段, 且与 tool_calls / text 共存。

// thinkingBlocks 造一条"上一轮真的会出现在 messages[] 里"的 assistant 消息:
// thinking + text + tool_use 三块齐全。
func thinkingBlocks() []types.ContentBlock {
	return []types.ContentBlock{
		{Type: types.ContentBlockThinking, Thinking: "先看 A 再决定 B"},
		{Type: types.ContentBlockText, Text: "调 calc"},
		{Type: types.ContentBlockToolUse, ID: "toolu_1", Name: "calc", Input: json.RawMessage(`{"a":2,"b":2}`)},
	}
}

// TestAssistantBlocksThinkingDroppedByDefault 空开关: thinking 块必须一个字节都不外流。
//
// 这是"默认行为不变"的硬要求。变异靶: 把 `reasoningPlayback != ""` 从条件里去掉,
// 或者把两个字段的 omitempty 去掉 —— 两者都会让本用例失败。
func TestAssistantBlocksThinkingDroppedByDefault(t *testing.T) {
	msgs, err := assistantBlocksToOpenAI(thinkingBlocks(), "")
	if err != nil {
		t.Fatalf("assistantBlocksToOpenAI: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Reasoning != "" || m.ReasoningContent != "" {
		t.Fatalf("空开关必须两个字段都为空, got reasoning=%q reasoning_content=%q", m.Reasoning, m.ReasoningContent)
	}
	if len(m.ToolCalls) != 1 {
		t.Fatalf("tool_calls 应保持 1 个 (thinking 丢弃不该影响它), got %d", len(m.ToolCalls))
	}
	if m.Content != "调 calc" {
		t.Fatalf("content 应保持 text 原值, got %v", m.Content)
	}
	// 走完整 JSON: 字节里连字段名都不该出现 (omitempty 的验收)。
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "reasoning") {
		t.Fatalf("默认路径的 wire body 出现 reasoning 字段: %s", b)
	}
}

// TestAssistantBlocksPlaybackReasoning 填 messages[].reasoning (opencode step-5 那侧)。
func TestAssistantBlocksPlaybackReasoning(t *testing.T) {
	msgs, err := assistantBlocksToOpenAI(thinkingBlocks(), ReasoningPlaybackReasoning)
	if err != nil {
		t.Fatalf("assistantBlocksToOpenAI: %v", err)
	}
	m := msgs[0]
	if m.Reasoning != "先看 A 再决定 B" {
		t.Fatalf("reasoning 字段错: %q", m.Reasoning)
	}
	if m.ReasoningContent != "" {
		t.Fatalf("填 reasoning 时 reasoning_content 必须留空, got %q", m.ReasoningContent)
	}
	// 回传不该挤掉原有字段: text 与 tool_calls 都还得在。
	if m.Content != "调 calc" || len(m.ToolCalls) != 1 {
		t.Fatalf("回传推理时 text/tool_calls 丢了: content=%v calls=%d", m.Content, len(m.ToolCalls))
	}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"reasoning":"先看 A 再决定 B"`) {
		t.Fatalf("wire body 缺 reasoning 字段: %s", b)
	}
}

// TestAssistantBlocksPlaybackReasoningContent 填 messages[].reasoning_content
// (DeepSeek/R1 那侧)。两个字段是互斥的: 同时填会让上游按错的那份推理接上下文。
func TestAssistantBlocksPlaybackReasoningContent(t *testing.T) {
	msgs, err := assistantBlocksToOpenAI(thinkingBlocks(), ReasoningPlaybackReasoningContent)
	if err != nil {
		t.Fatalf("assistantBlocksToOpenAI: %v", err)
	}
	m := msgs[0]
	if m.ReasoningContent != "先看 A 再决定 B" {
		t.Fatalf("reasoning_content 字段错: %q", m.ReasoningContent)
	}
	if m.Reasoning != "" {
		t.Fatalf("填 reasoning_content 时 reasoning 必须留空, got %q", m.Reasoning)
	}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"reasoning_content":"先看 A 再决定 B"`) {
		t.Fatalf("wire body 缺 reasoning_content 字段: %s", b)
	}
}

// TestAssistantBlocksPlaybackMultipleThinkingConcatenated 多个 thinking 块要整段
// 重发, 不是只发最后一段。
//
// 变异靶: 把 `reasoning.Len() > 0 { reasoning.WriteString("\n") }` 去掉, 或把
// `reasoning.WriteString` 换成覆盖赋值。
func TestAssistantBlocksPlaybackMultipleThinkingConcatenated(t *testing.T) {
	blocks := []types.ContentBlock{
		{Type: types.ContentBlockThinking, Thinking: "第一段"},
		{Type: types.ContentBlockThinking, Thinking: "第二段"},
	}
	msgs, err := assistantBlocksToOpenAI(blocks, ReasoningPlaybackReasoning)
	if err != nil {
		t.Fatalf("assistantBlocksToOpenAI: %v", err)
	}
	want := "第一段\n第二段"
	if msgs[0].Reasoning != want {
		t.Fatalf("多段应整段拼接, want %q got %q", want, msgs[0].Reasoning)
	}
}

// TestAssistantBlocksPlaybackEmptyThinkingNoField 空 thinking 块不该凭空产出一个
// 空字符串字段 (omitempty 的验收: wire 上不留 `"reasoning":""`)。
func TestAssistantBlocksPlaybackEmptyThinkingNoField(t *testing.T) {
	blocks := []types.ContentBlock{
		{Type: types.ContentBlockThinking}, // Thinking == ""
		{Type: types.ContentBlockText, Text: "只有文字"},
	}
	msgs, err := assistantBlocksToOpenAI(blocks, ReasoningPlaybackReasoning)
	if err != nil {
		t.Fatalf("assistantBlocksToOpenAI: %v", err)
	}
	b, _ := json.Marshal(msgs[0])
	if strings.Contains(string(b), "reasoning") {
		t.Fatalf("空 thinking 不该产生 reasoning 字段: %s", b)
	}
}

// TestAnthropicToOpenAIThreadsPlayback 端到端: 回传开关要能从 anthropicToOpenAI
// 一路传到 messages[] 里那条历史 assistant 消息上。
//
// 这条盯的是"参数有没有真的串起来" —— 逐层传参漏一层, 只测
// assistantBlocksToOpenAI 是发现不了的。
func TestAnthropicToOpenAIThreadsPlayback(t *testing.T) {
	req := types.APIRequest{
		Model:     "step-5-preview-free",
		MaxTokens: 1024,
		Messages: []types.APIMessage{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"上一轮的推理"},{"type":"text","text":"好的"}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"继续"}]`)},
		},
	}
	oreq, err := anthropicToOpenAI(req, false, ReasoningPlaybackReasoning)
	if err != nil {
		t.Fatalf("anthropicToOpenAI: %v", err)
	}
	var found bool
	for _, m := range oreq.Messages {
		if m.Role == "assistant" && m.Reasoning == "上一轮的推理" {
			found = true
		}
	}
	if !found {
		t.Fatalf("回传没串到 messages[].reasoning: %+v", oreq.Messages)
	}

	// 同一条请求喂空开关, assistant 消息上一个相关字节都不该有。
	oreq2, err := anthropicToOpenAI(req, false, "")
	if err != nil {
		t.Fatalf("anthropicToOpenAI(默认): %v", err)
	}
	for _, m := range oreq2.Messages {
		if m.Role == "assistant" && (m.Reasoning != "" || m.ReasoningContent != "") {
			t.Fatalf("默认路径漏出推理: %+v", m)
		}
	}
}

// TestWithModelKeepsPlayback WithModel 副本必须带走回传开关。
//
// plan/role 走的就是这条链: 主 client 按模型配置了 reasoningPlayback, 换模型名的
// 副本若不带, 开关在副本上静默失效 —— 而失效的表现是"上游没收到推理", 不报错。
func TestWithModelKeepsPlayback(t *testing.T) {
	c := &Client{ReasoningPlayback: ReasoningPlaybackReasoning}
	if got := c.WithModel("opencode:step-5-preview-free").ReasoningPlayback; got != ReasoningPlaybackReasoning {
		t.Fatalf("WithModel 丢了 ReasoningPlayback: %q", got)
	}
	if got := c.ConfiguredCloneFull("http://x", "k", "m", nil, "", "").ReasoningPlayback; got != ReasoningPlaybackReasoning {
		t.Fatalf("ConfiguredCloneFull 丢了 ReasoningPlayback: %q", got)
	}
}

// TestBuildOpenAIRequestReadsClientField 从 Client 字段出发的收口测试。
//
// 上面几条都从 assistantBlocksToOpenAI / anthropicToOpenAI 喂字符串常量, 有一条
// 链它们盖不住: buildOpenAIRequest 有没有真的把 c.ReasoningPlayback 读出来往下传。
// 那段一旦写成常量 "" (或读错字段), 单测全绿而线上一个字节都不回传 —— 因为
// 所有上游可见的现象都"没有", 而不是"出错"。所以必须有一例从 Client 起跳。
func TestBuildOpenAIRequestReadsClientField(t *testing.T) {
	req := types.APIRequest{
		Model:     "step-5-preview-free",
		MaxTokens: 1024,
		Messages: []types.APIMessage{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"推理"},{"type":"text","text":"ok"}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"go"}]`)},
		},
	}
	c := &Client{ReasoningPlayback: ReasoningPlaybackReasoning}
	oreq, body, err := c.buildOpenAIRequest(req, false)
	if err != nil {
		t.Fatalf("buildOpenAIRequest: %v", err)
	}
	if !strings.Contains(string(body), `"reasoning":"推理"`) {
		t.Fatalf("Client.ReasoningPlayback 没传到 wire body: %s", body)
	}
	_ = oreq

	// 反向: 同一个 client 结构的空值字段, wire 上什么都不能有。
	c2 := &Client{}
	_, body2, err := c2.buildOpenAIRequest(req, false)
	if err != nil {
		t.Fatalf("buildOpenAIRequest(默认): %v", err)
	}
	if strings.Contains(string(body2), "reasoning") {
		t.Fatalf("默认 client 漏出推理: %s", body2)
	}
}
