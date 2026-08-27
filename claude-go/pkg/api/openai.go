// OpenAI chat/completions 协议翻译层。
//
// 为什么需要它: opencode 建议 mimo-v2.5 走 OpenAI 协议 (/v1/chat/completions),
// 因为它的 Anthropic→OpenAI 转换层会丢 tool_use 的 name 字段导致 400
// (见 opencode-go-anthropic-compat 能力矩阵)。引擎 (pkg/engine) 只认 Anthropic
// Messages 事件序列, 因此这里做双向翻译:
//
//   - 出站: Anthropic APIRequest (块数组 content / tool_result / tool_use / thinking)
//     → OpenAI chat/completions body (role 分离的 user/tool/assistant.tool_calls)。
//   - 入站非流式: OpenAI choices[0].message → types.APIResponse。
//   - 入站流式: OpenAI SSE delta → types.StreamDelta 事件序列。
//
// 关键约束 (引擎消费假设): engine.go 同一时刻只跟踪**一个**当前块 (content_block_start
// 覆盖 currentBlock 并 reset 文本/输入缓冲, content_block_stop 才落盘)。因此流式翻译
// 必须**串行发出**每个块: 文本块在首个 tool_calls 到达时先关闭, 工具块再逐个
// start→delta→stop, 交错的尾部文本缓冲到 finish_reason 后补发 —— 绝不并行开两个块。
package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// ProtocolOpenAI / ProtocolAnthropic 客户端出站协议。
const (
	ProtocolOpenAI     = "openai"
	ProtocolAnthropic  = "anthropic"
)

// ── 出站请求 wire 类型 ──────────────────────────────────────────────────────

type openAIToolCall struct {
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function openAIToolFunc `json:"function"`
}

type openAIToolFunc struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Arguments   string      `json:"arguments,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

type openAIContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type openAIMessage struct {
	Role       string             `json:"role"`
	Content    interface{}        `json:"content,omitempty"` // string | []openAIContentPart | null
	ToolCallID string             `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall   `json:"tool_calls,omitempty"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIToolFunc `json:"function"`
}

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	Tools       []openAITool    `json:"tools,omitempty"`
	ToolChoice  interface{}     `json:"tool_choice,omitempty"`
	// StreamOptions 让流式末帧携带 usage (网关/客户端记账; 标准 OpenAI 字段)。
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

// buildOpenAIRequest 把 Anthropic 请求翻译为 OpenAI body。
// 返回可重序列化的请求体 (重试换模型时需要改 Model 再 marshal)。
func (c *Client) buildOpenAIRequest(req types.APIRequest, stream bool) (*openAIRequest, []byte, error) {
	oreq, err := anthropicToOpenAI(req, stream)
	if err != nil {
		return nil, nil, err
	}
	body, err := json.Marshal(oreq)
	if err != nil {
		return nil, nil, fmt.Errorf("序列化 OpenAI 请求失败: %w", err)
	}
	return oreq, body, nil
}

// anthropicToOpenAI 把 engine 已装配好的 Anthropic 请求翻译为 OpenAI chat/completions。
//
// 转换要点:
//   - system → messages[0] role=system (拼成单一字符串)
//   - user 消息里的 tool_result 块 → 独立的 role=tool 消息 (保持紧跟 assistant),
//     其余 text/image 块 → 一个 role=user 消息 (排在工具消息之后)
//   - assistant 消息的 tool_use 块 → tool_calls 数组; thinking 块丢弃
//   - tools input_schema → OpenAI function.parameters
//   - tool_choice any/tool → "required"
func anthropicToOpenAI(req types.APIRequest, stream bool) (*openAIRequest, error) {
	oreq := &openAIRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
		Temperature: req.Temperature,
	}
	if stream {
		oreq.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	if sys := systemToOpenAIString(req.System); sys != "" {
		oreq.Messages = append(oreq.Messages, openAIMessage{Role: "system", Content: sys})
	}
	for _, m := range req.Messages {
		ms, err := anthropicMessageToOpenAI(m)
		if err != nil {
			return nil, err
		}
		oreq.Messages = append(oreq.Messages, ms...)
	}
	if len(req.Tools) > 0 {
		oreq.Tools = make([]openAITool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var params interface{}
			if len(t.InputSchema) > 0 {
				_ = json.Unmarshal(t.InputSchema, &params)
			}
			if params == nil {
				params = map[string]interface{}{}
			}
			oreq.Tools = append(oreq.Tools, openAITool{
				Type:     "function",
				Function: openAIToolFunc{Name: t.Name, Description: t.Description, Parameters: params},
			})
		}
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "any", "tool":
			oreq.ToolChoice = "required"
		case "none":
			oreq.ToolChoice = "none"
		default: // "auto" / 空
			oreq.ToolChoice = "auto"
		}
	}
	return oreq, nil
}

// anthropicMessageToOpenAI 翻译单条 Anthropic 消息为 0..N 条 OpenAI 消息。
func anthropicMessageToOpenAI(msg types.APIMessage) ([]openAIMessage, error) {
	var blocks []types.ContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		// 纯字符串 content (部分调用方直接传字符串)
		var s string
		if err2 := json.Unmarshal(msg.Content, &s); err2 == nil {
			return []openAIMessage{{Role: msg.Role, Content: s}}, nil
		}
		return nil, fmt.Errorf("openai: 无法解析 content: %w", err)
	}
	switch msg.Role {
	case "assistant":
		return assistantBlocksToOpenAI(blocks)
	case "user":
		return userBlocksToOpenAI(blocks)
	case "system":
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == types.ContentBlockText {
				sb.WriteString(b.Text)
			}
		}
		return []openAIMessage{{Role: "system", Content: sb.String()}}, nil
	default: // tool / 其它一律按 user 处理
		return userBlocksToOpenAI(blocks)
	}
}

// userBlocksToOpenAI 把 user 消息拆成 tool 消息 + 一条 user 消息。
// 工具消息统一排前, 保证紧跟 assistant 的 tool_calls (OpenAI 校验要求)。
func userBlocksToOpenAI(blocks []types.ContentBlock) ([]openAIMessage, error) {
	var out []openAIMessage
	var parts []openAIContentPart
	for _, b := range blocks {
		switch b.Type {
		case types.ContentBlockToolResult:
			out = append(out, openAIMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: b.Content})
		case types.ContentBlockText:
			if b.Text != "" {
				parts = append(parts, openAIContentPart{Type: "text", Text: b.Text})
			}
		case types.ContentBlockImage:
			if b.Source != nil {
				url := b.Source.URL
				if b.Source.Type == "base64" && b.Source.Data != "" {
					url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
				}
				if url != "" {
					parts = append(parts, openAIContentPart{Type: "image_url", ImageURL: &struct {
						URL string `json:"url"`
					}{URL: url}})
				}
			}
		case types.ContentBlockThinking:
			// OpenAI 无 thinking 块, 丢弃
		}
	}
	if len(parts) > 0 {
		out = append(out, openAIMessage{Role: "user", Content: parts})
	}
	return out, nil
}

// assistantBlocksToOpenAI 把 assistant 消息的 text 块拼成 content, tool_use 块转 tool_calls。
func assistantBlocksToOpenAI(blocks []types.ContentBlock) ([]openAIMessage, error) {
	var sb strings.Builder
	var calls []openAIToolCall
	for _, b := range blocks {
		switch b.Type {
		case types.ContentBlockText:
			if b.Text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(b.Text)
			}
		case types.ContentBlockToolUse:
			args := normalizeJSONArgs(string(b.Input))
			calls = append(calls, openAIToolCall{
				ID:   b.ID,
				Type: "function",
				Function: openAIToolFunc{
					Name:      b.Name,
					Arguments: args,
				},
			})
		case types.ContentBlockThinking:
			// 丢弃
		}
	}
	var content interface{}
	if sb.Len() > 0 {
		content = sb.String()
	}
	if len(calls) == 0 {
		return []openAIMessage{{Role: "assistant", Content: content}}, nil
	}
	return []openAIMessage{{Role: "assistant", Content: content, ToolCalls: calls}}, nil
}

// systemToOpenAIString 把 Anthropic system 字段 (string | []map 块 | []string) 摊平为纯文本。
func systemToOpenAIString(sys interface{}) string {
	switch v := sys.(type) {
	case nil:
		return ""
	case string:
		return v
	case []string:
		return strings.Join(v, "\n\n")
	}
	// []map[string]interface{} 块形态: 逐个取 "text"
	b, err := json.Marshal(sys)
	if err != nil {
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(b, &blocks) == nil {
		var sb strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" && bl.Text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n\n")
				}
				sb.WriteString(bl.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// normalizeJSONArgs 保证 tool_use 的 input 是合法 JSON 对象。
func normalizeJSONArgs(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "{}"
	}
	var v interface{}
	if json.Unmarshal([]byte(args), &v) != nil {
		// 畸形参数 → 包成 {"raw": ...} 避免引擎后续 Unmarshal 失败
		b, _ := json.Marshal(map[string]interface{}{"raw": args})
		return string(b)
	}
	return args
}

// ── 入站非流式 wire 类型 ─────────────────────────────────────────────────────

type openAIResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// parseOpenAIResponse 解析 OpenAI 非流式响应 → types.APIResponse。
func parseOpenAIResponse(model string, raw []byte) (types.APIResponse, error) {
	var r openAIResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return types.APIResponse{}, fmt.Errorf("openai 响应解析失败: %w", err)
	}
	if r.Error != nil {
		return types.APIResponse{}, fmt.Errorf("openai 错误: %s", r.Error.Message)
	}
	var resp types.APIResponse
	resp.ID = r.ID
	resp.Type = "message"
	resp.Role = "assistant"
	resp.Model = model
	if r.Model != "" {
		resp.Model = r.Model
	}
	if len(r.Choices) > 0 {
		ch := r.Choices[0]
		resp.Content = openAIContentToBlocks(ch.Message.Content, ch.Message.ToolCalls)
		resp.StopReason = openAIFinishToStop(ch.FinishReason)
	}
	resp.Usage = &types.Usage{
		InputTokens:          r.Usage.PromptTokens,
		OutputTokens:         r.Usage.CompletionTokens,
		CacheReadInputTokens: r.Usage.PromptTokensDetails.CachedTokens,
	}
	if resp.Content == nil {
		resp.Content = []types.ContentBlock{}
	}
	return resp, nil
}

// openAIContentToBlocks 把 OpenAI message.content (string | 块数组) + tool_calls → Anthropic 块。
func openAIContentToBlocks(content json.RawMessage, calls []openAIToolCall) []types.ContentBlock {
	var out []types.ContentBlock
	if txt := openAIContentText(content); txt != "" {
		out = append(out, types.ContentBlock{Type: types.ContentBlockText, Text: txt})
	}
	for i, tc := range calls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("toolu_%d", i)
		}
		out = append(out, types.ContentBlock{
			Type:  types.ContentBlockToolUse,
			ID:    id,
			Name:  tc.Function.Name,
			Input: json.RawMessage(normalizeJSONArgs(tc.Function.Arguments)),
		})
	}
	return out
}

// ── 入站流式翻译器 ────────────────────────────────────────────────────────────

// openAIStreamTranslator 把 OpenAI SSE chunk 翻译为 Anthropic StreamDelta 事件。
// 持有所需的跨 chunk 状态: 已发出的块序号、工具块按 OpenAI tool_call index 索引、
// 交错期缓冲的尾部文本。引擎只认串行块, 因此文本块在首个工具块前关闭。
type openAIStreamTranslator struct {
	model          string
	started        bool
	finished       bool
	id             string
	nextBlockIndex int

	textOpen       bool
	textBlockIndex int
	trailing       strings.Builder // 工具块打开后到来的文本 (极少数交错), finish 时补发

	toolBlocks map[int]*openAIToolState
	toolOrder  []int // 出现顺序, 用于关闭
	usage      *types.Usage
}

type openAIToolState struct {
	blockIndex int
	id         string
	name       string
	args       strings.Builder
	started    bool
}

func newOpenAIStreamTranslator(model string) *openAIStreamTranslator {
	return &openAIStreamTranslator{model: model, toolBlocks: map[int]*openAIToolState{}}
}

// translate 处理一个 OpenAI SSE data 行, 返回 0..N 个 Anthropic 事件。
func (t *openAIStreamTranslator) translate(data []byte) ([]types.StreamDelta, error) {
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, err
	}
	if t.finished {
		return nil, nil
	}

	var out []types.StreamDelta

	if !t.started {
		t.started = true
		t.id = chunk.ID
		out = append(out, types.StreamDelta{
			Type: "message_start",
			Message: &types.APIResponse{
				ID: chunk.ID, Type: "message", Role: "assistant",
				Model: firstNonEmpty(chunk.Model, t.model),
				Usage: &types.Usage{},
			},
		})
	}
	if chunk.Usage != nil {
		t.usage = &types.Usage{
			InputTokens:          chunk.Usage.PromptTokens,
			OutputTokens:         chunk.Usage.CompletionTokens,
			CacheReadInputTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
		}
	}

	for _, ch := range chunk.Choices {
		// 1) 文本增量
		if text := openAIContentText(ch.Delta.Content); text != "" {
			if len(t.toolBlocks) == 0 && !t.textOpen {
				t.textOpen = true
				t.textBlockIndex = t.nextBlockIndex
				t.nextBlockIndex++
				out = append(out, types.StreamDelta{
					Type: "content_block_start", Index: t.textBlockIndex,
					ContentBlock: &types.ContentBlock{Type: types.ContentBlockText},
				})
			}
			if t.textOpen {
				out = append(out, types.StreamDelta{
					Type: "content_block_delta", Index: t.textBlockIndex,
					Delta: &types.DeltaContent{Type: "text_delta", Text: text},
				})
			} else {
				// 工具块已打开 (交错) → 缓冲, finish 时补发
				t.trailing.WriteString(text)
			}
		}

		// 2) 工具调用增量
		for _, tc := range ch.Delta.ToolCalls {
			st := t.toolBlocks[tc.Index]
			if st == nil {
				st = &openAIToolState{id: tc.ID}
				t.toolBlocks[tc.Index] = st
				t.toolOrder = append(t.toolOrder, tc.Index)
			}
			if tc.ID != "" {
				st.id = tc.ID
			}
			if tc.Function.Name != "" {
				st.name = tc.Function.Name
			}
			// 文本块若开着, 先关闭 (引擎只跟踪一个当前块)
			if t.textOpen {
				t.textOpen = false
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.textBlockIndex})
			}
			if !st.started {
				st.started = true
				st.blockIndex = t.nextBlockIndex
				t.nextBlockIndex++
				id := st.id
				if id == "" {
					id = fmt.Sprintf("toolu_%d", tc.Index)
				}
				out = append(out, types.StreamDelta{
					Type: "content_block_start", Index: st.blockIndex,
					ContentBlock: &types.ContentBlock{
						Type: types.ContentBlockToolUse, ID: id, Name: st.name,
						Input: json.RawMessage("{}"),
					},
				})
				// 若 name 晚到 (name 前已有攒下的 arguments), 一并补发
				if st.args.Len() > 0 {
					out = append(out, types.StreamDelta{
						Type: "content_block_delta", Index: st.blockIndex,
						Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: st.args.String()},
					})
					st.args.Reset()
				}
			}
			if tc.Function.Arguments != "" {
				if st.started {
					out = append(out, types.StreamDelta{
						Type: "content_block_delta", Index: st.blockIndex,
						Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: tc.Function.Arguments},
					})
				} else {
					st.args.WriteString(tc.Function.Arguments)
				}
			}
		}

		// 3) finish_reason → 逐个关闭块, 补发交错文本, 收尾 message_delta
		if ch.FinishReason != "" {
			if t.textOpen {
				t.textOpen = false
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.textBlockIndex})
			}
			for _, idx := range t.toolOrder {
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.toolBlocks[idx].blockIndex})
			}
			if t.trailing.Len() > 0 {
				bi := t.nextBlockIndex
				t.nextBlockIndex++
				out = append(out, types.StreamDelta{Type: "content_block_start", Index: bi, ContentBlock: &types.ContentBlock{Type: types.ContentBlockText}})
				out = append(out, types.StreamDelta{Type: "content_block_delta", Index: bi, Delta: &types.DeltaContent{Type: "text_delta", Text: t.trailing.String()}})
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: bi})
			}
			out = append(out, types.StreamDelta{
				Type: "message_delta",
				Delta: &types.DeltaContent{StopReason: openAIFinishToStop(ch.FinishReason)},
				Usage: t.usage,
			})
			t.finished = true
		}
	}
	return out, nil
}

// openAIContentText 从 OpenAI delta/message.content (string | 块数组) 提取纯文本。
func openAIContentText(content json.RawMessage) string {
	if len(content) == 0 || string(content) == "null" || string(content) == `""` {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// openAIFinishToStop 把 OpenAI finish_reason 映射为 Anthropic stop_reason。
func openAIFinishToStop(f string) string {
	switch f {
	case "tool_calls":
		return string(types.StopReasonToolUse)
	case "length":
		return string(types.StopReasonMaxTokens)
	case "content_filter":
		return string(types.StopReasonRefusal)
	case "stop":
		return string(types.StopReasonEndTurn)
	case "":
		return string(types.StopReasonEndTurn)
	default:
		return f
	}
}

