// OpenAI Responses API (POST /v1/responses) 协议翻译层。
//
// 为什么需要它: opencode 官方对 muse-spark-1.2-contributor (Meta 模型, 大陆不可达)
// 推荐的接入方式是 OpenAI Responses API —— 模型清单表里该行 SDK 列是 @ai-sdk/openai、
// 端点是 https://opencode.ai/zen/go/v1/responses, 与 chat/completions 不同协议。
// 引擎 (pkg/engine) 只认 Anthropic Messages 事件序列, 因此这里做双向翻译:
//
//   - 出站: Anthropic APIRequest → Responses API body (input items 数组)。
//     input 是异构 item 列表: 用户文本 = {role:"user",content:[{type:"input_text",text}]},
//     助手文本 = {type:"message",role:"assistant",content:[{type:"output_text",text}]},
//     助手工具调用 = {type:"function_call",call_id,name,arguments},
//     工具结果 = {type:"function_call_output",call_id,output}。
//   - 入站非流式: response.output items (reasoning/message/function_call) → APIResponse。
//   - 入站流式: Responses SSE 事件 → StreamDelta (串行块约束同 openai.go 头注)。
//
// 实测 (2026-08-21, 经东京 tailnet 代理) 确认的 SSE 事件序列:
//   response.created → ... → response.output_item.added (reasoning|message|function_call)
//   → response.content_part.added → response.output_text.delta/done
//   → response.function_call_arguments.delta/done → response.output_item.done
//   → response.completed (带 usage)。期间混有 ping keepalive 帧。
package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// ProtocolOpenAIResponses 客户端出站协议 (OpenAI Responses API)。
const ProtocolOpenAIResponses = "openai-responses"

// ── 出站请求 wire 类型 ──────────────────────────────────────────────────────

// responsesInputItem 是所有 input item 的公共形态 (用 RawMessage 保持异构)。
type responsesInputItem struct {
	Raw json.RawMessage
}

func (i responsesInputItem) MarshalJSON() ([]byte, error) { return i.Raw, nil }

// responsesTool Responses API 的工具定义 (平铺字段, 不同于 chat/completions 的
// {type:"function",function:{...}} 嵌套)。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type responsesRequest struct {
	Model           string              `json:"model"`
	Instructions    string              `json:"instructions,omitempty"`
	Input           []responsesInputItem `json:"input"`
	Tools           []responsesTool     `json:"tools,omitempty"`
	ToolChoice      interface{}         `json:"tool_choice,omitempty"`
	MaxOutputTokens int                 `json:"max_output_tokens,omitempty"`
	Stream          bool                `json:"stream,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
}

// input item 构造辅助 (直接产出 RawMessage)。
func responsesItem(v interface{}) responsesInputItem {
	b, _ := json.Marshal(v)
	return responsesInputItem{Raw: b}
}

// responsesUserText 用户文本消息 item。
func responsesUserText(text string) responsesInputItem {
	return responsesItem(map[string]interface{}{
		"role": "user",
		"content": []map[string]interface{}{{"type": "input_text", "text": text}},
	})
}

// responsesAssistantText 助手文本消息 item (多轮回填)。
func responsesAssistantText(text string) responsesInputItem {
	return responsesItem(map[string]interface{}{
		"type": "message", "role": "assistant",
		"content": []map[string]interface{}{{"type": "output_text", "text": text}},
	})
}

// responsesFunctionCall 助手工具调用 item (多轮回填)。
func responsesFunctionCall(id, name, arguments string) responsesInputItem {
	return responsesItem(map[string]interface{}{
		"type": "function_call", "call_id": id, "name": name, "arguments": arguments,
	})
}

// responsesFunctionCallOutput 工具结果 item。
func responsesFunctionCallOutput(callID, output string) responsesInputItem {
	return responsesItem(map[string]interface{}{
		"type": "function_call_output", "call_id": callID, "output": output,
	})
}

// buildOpenAIResponsesRequest 把 Anthropic 请求翻译为 Responses API body。
func (c *Client) buildOpenAIResponsesRequest(req types.APIRequest, stream bool) (*responsesRequest, []byte, error) {
	rreq, err := anthropicToResponses(req, stream)
	if err != nil {
		return nil, nil, err
	}
	body, err := json.Marshal(rreq)
	if err != nil {
		return nil, nil, fmt.Errorf("序列化 Responses 请求失败: %w", err)
	}
	return rreq, body, nil
}

// anthropicToResponses 把 engine 装配好的 Anthropic 请求翻译为 Responses API input。
func anthropicToResponses(req types.APIRequest, stream bool) (*responsesRequest, error) {
	rreq := &responsesRequest{
		Model:           req.Model,
		Instructions:    systemToOpenAIString(req.System),
		Input:           []responsesInputItem{},
		MaxOutputTokens: req.MaxTokens,
		Stream:          stream,
		Temperature:     req.Temperature,
	}
	for _, m := range req.Messages {
		items, err := anthropicMessageToResponses(m)
		if err != nil {
			return nil, err
		}
		rreq.Input = append(rreq.Input, items...)
	}
	if len(req.Tools) > 0 {
		rreq.Tools = make([]responsesTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := json.RawMessage(`{}`)
			if len(t.InputSchema) > 0 {
				params = t.InputSchema
			}
			rreq.Tools = append(rreq.Tools, responsesTool{
				Type: "function", Name: t.Name, Description: t.Description, Parameters: params,
			})
		}
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "none":
			// opencode 上游 (Console Go) 只支持 tool_choice:"auto":
			// "none" 用不传 tools 实现"禁用工具"语义 (直接发 none 会 400)。
			rreq.Tools = nil
		default: // "any" / "tool" / "auto" / 空
			// 上游不支持 required/命名函数 (仅 auto), 无法强制工具调用;
			// any/tool 落到 auto, 引擎仍会按输出中的 function_call 执行工具。
			rreq.ToolChoice = "auto"
		}
	}
	return rreq, nil
}

// anthropicMessageToResponses 翻译单条 Anthropic 消息为 0..N 个 input items。
func anthropicMessageToResponses(msg types.APIMessage) ([]responsesInputItem, error) {
	var blocks []types.ContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		// 纯字符串 content (部分调用方直接传字符串)
		var s string
		if err2 := json.Unmarshal(msg.Content, &s); err2 == nil {
			return []responsesInputItem{responsesUserText(s)}, nil
		}
		return nil, fmt.Errorf("responses: 无法解析 content: %w", err)
	}
	var out []responsesInputItem
	var parts []string
	var toolCalls []responsesInputItem
	for _, b := range blocks {
		switch b.Type {
		case types.ContentBlockToolResult:
			// 工具结果 → function_call_output item (立即 append, 它在对话中排助手工具调用之后)
			out = append(out, responsesFunctionCallOutput(b.ToolUseID, b.Content))
		case types.ContentBlockText:
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case types.ContentBlockToolUse:
			// 助手工具调用 → function_call item (多轮回填); 先攒着, 排在文本之后
			toolCalls = append(toolCalls, responsesFunctionCall(b.ID, b.Name, normalizeJSONArgs(string(b.Input))))
		case types.ContentBlockThinking:
			// Responses 无 thinking 块, 丢弃
		}
	}
	// 同一条 assistant 消息内保持对话时序: 文本先于工具调用。
	// (Responses API 的 input 按时间序解释, 倒序会破坏多轮工具循环的因果链。)
	if len(parts) > 0 {
		switch msg.Role {
		case "assistant":
			out = append(out, responsesAssistantText(strings.Join(parts, "\n")))
		case "user":
			out = append(out, responsesUserText(strings.Join(parts, "\n")))
		default: // tool / system / 其它一律按 user 文本
			out = append(out, responsesUserText(strings.Join(parts, "\n")))
		}
	}
	out = append(out, toolCalls...)
	return out, nil
}

// ── 入站非流式 wire 类型 ─────────────────────────────────────────────────────

type responsesOutputItem struct {
	Type      string `json:"type"` // reasoning | message | function_call
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // function_call 的参数 JSON 字符串
	Content   []struct {
		Type string `json:"type"` // output_text
		Text string `json:"text"`
	} `json:"content"`
}

type openAIResponsesResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Status  string `json:"status"` // completed | incomplete | failed | ...
	Output  []responsesOutputItem `json:"output"`
	Usage   struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// parseOpenAIResponsesResponse 解析 Responses API 非流式响应 → types.APIResponse。
func parseOpenAIResponsesResponse(model string, raw []byte) (types.APIResponse, error) {
	var r openAIResponsesResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return types.APIResponse{}, fmt.Errorf("responses 响应解析失败: %w", err)
	}
	if r.Error != nil {
		return types.APIResponse{}, fmt.Errorf("responses 错误(%s): %s", r.Error.Code, r.Error.Message)
	}
	var resp types.APIResponse
	resp.ID = r.ID
	resp.Type = "message"
	resp.Role = "assistant"
	resp.Model = model
	if r.Model != "" {
		resp.Model = r.Model
	}
	// Responses API 的 status="completed" 不区分"文本收尾"和"工具调用收尾"——
	// 工具调用由 output 里的 function_call item 体现。Anthropic 引擎靠 stop_reason
	// 决定是否执行工具, 若 output 含 function_call 必须报 tool_use, 否则引擎会当
	// 最终回复处理、工具永不执行。
	hasToolUse := false
	for _, it := range r.Output {
		switch it.Type {
		case "reasoning":
			// Meta 加密推理, 不透传
		case "message":
			var sb strings.Builder
			for _, p := range it.Content {
				if p.Type == "output_text" && p.Text != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(p.Text)
				}
			}
			if sb.Len() > 0 {
				resp.Content = append(resp.Content, types.ContentBlock{Type: types.ContentBlockText, Text: sb.String()})
			}
		case "function_call":
			hasToolUse = true
			id := it.CallID
			if id == "" {
				id = it.ID
			}
			resp.Content = append(resp.Content, types.ContentBlock{
				Type:  types.ContentBlockToolUse,
				ID:    id,
				Name:  it.Name,
				Input: json.RawMessage(normalizeJSONArgs(it.Arguments)),
			})
		}
	}
	resp.StopReason = responsesStatusToStop(r.Status)
	if hasToolUse {
		resp.StopReason = string(types.StopReasonToolUse)
	}
	resp.Usage = &types.Usage{
		InputTokens:          r.Usage.InputTokens,
		OutputTokens:         r.Usage.OutputTokens,
		CacheReadInputTokens: r.Usage.InputTokensDetails.CachedTokens,
	}
	if resp.Content == nil {
		resp.Content = []types.ContentBlock{}
	}
	return resp, nil
}

// responsesStatusToStop 把 Responses API status 映射为 Anthropic stop_reason。
func responsesStatusToStop(status string) string {
	switch status {
	case "completed":
		return string(types.StopReasonEndTurn)
	case "incomplete":
		return string(types.StopReasonMaxTokens)
	// Anthropic stop_reason 无 "error" 枚举: failed/cancelled 也归 end_turn,
	// 真实失败经由 response.error 字段在 parse 层已显式报错, 到不了这里。
	case "failed", "cancelled":
		return string(types.StopReasonEndTurn)
	default:
		return string(types.StopReasonEndTurn)
	}
}

// ── 入站流式翻译器 ────────────────────────────────────────────────────────────

// openAIResponsesStreamTranslator 把 Responses API SSE 事件翻译为 Anthropic StreamDelta。
// 持有所需的跨事件状态: 当前文本块、正在流的 function_call (按 item_id 索引)、
// 已发出的块序号。串行块约束: 文本块在其 message item 结束 (output_item.done) 时关闭,
// function_call item 出现时若文本块还开着先强制关闭; 工具块在其 item.done 时关闭。
type openAIResponsesStreamTranslator struct {
	model    string
	started  bool
	finished bool
	id       string
	status   string

	nextBlockIndex int

	// 当前文本块 (每个 message item 一个)
	textOpen       bool
	textBlockIndex int
	textItemID     string

	// 进行中的 function_call 工具块, 按 item_id 索引
	toolItems map[string]*responsesToolState
	toolOrder []string // 关闭顺序
	curToolID string   // 最近一个收到 arguments.delta 的 item

	usage *types.Usage
}

type responsesToolState struct {
	blockIndex int
	id         string
	name       string
	args       strings.Builder
	started    bool
	stopped    bool // 该工具块是否已在其 output_item.done 处关闭 (防 completed 重复 stop)
}

func newOpenAIResponsesStreamTranslator(model string) *openAIResponsesStreamTranslator {
	return &openAIResponsesStreamTranslator{model: model, toolItems: map[string]*responsesToolState{}}
}

// translate 处理一个 Responses SSE data 行, 返回 0..N 个 Anthropic 事件。
func (t *openAIResponsesStreamTranslator) translate(data []byte) ([]types.StreamDelta, error) {
	var ev struct {
		Type string `json:"type"`
		// output_item.added / done 时携带 item
		Item *struct {
			ID      string `json:"id"`
			Type    string `json:"type"` // reasoning | message | function_call
			Name    string `json:"name"`
			CallID  string `json:"call_id"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
		// output_text.delta / function_call_arguments.delta
		Delta   string `json:"delta"`
		ItemID  string `json:"item_id"`
		// response.completed 时携带完整 response
		Response *struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Usage  *struct {
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				InputTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, err
	}
	if t.finished {
		return nil, nil
	}
	var out []types.StreamDelta

	if !t.started {
		t.started = true
		out = append(out, types.StreamDelta{
			Type: "message_start",
			Message: &types.APIResponse{
				ID: t.id, Type: "message", Role: "assistant",
				Model: t.model, Usage: &types.Usage{},
			},
		})
	}

	switch ev.Type {
	case "response.created":
		if t.id == "" {
			t.id = ev.Response.ID
		}
		return out, nil
	case "response.output_item.added":
		if ev.Item == nil {
			return out, nil
		}
		switch ev.Item.Type {
		case "function_call":
			// 引擎只跟踪一个当前块: 若文本块还开着先关闭
			if t.textOpen {
				t.textOpen = false
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.textBlockIndex})
			}
			st := t.toolItems[ev.Item.ID]
			if st == nil {
				st = &responsesToolState{id: ev.Item.CallID, name: ev.Item.Name}
				t.toolItems[ev.Item.ID] = st
				t.toolOrder = append(t.toolOrder, ev.Item.ID)
			}
			if ev.Item.CallID != "" {
				st.id = ev.Item.CallID
			}
			if ev.Item.Name != "" {
				st.name = ev.Item.Name
			}
		}
		return out, nil
	case "response.content_part.added", "response.output_text.done":
		return out, nil
	case "response.output_text.delta":
		if !t.textOpen {
			t.textOpen = true
			t.textBlockIndex = t.nextBlockIndex
			t.nextBlockIndex++
			out = append(out, types.StreamDelta{
				Type: "content_block_start", Index: t.textBlockIndex,
				ContentBlock: &types.ContentBlock{Type: types.ContentBlockText},
			})
		}
		if ev.Delta != "" {
			out = append(out, types.StreamDelta{
				Type: "content_block_delta", Index: t.textBlockIndex,
				Delta: &types.DeltaContent{Type: "text_delta", Text: ev.Delta},
			})
		}
		return out, nil
	case "response.function_call_arguments.delta":
		st := t.toolItems[ev.ItemID]
		if st == nil {
			// 防御: 没收到 item.added 就先有 arguments (罕见) —— 现造一个
			st = &responsesToolState{}
			t.toolItems[ev.ItemID] = st
			t.toolOrder = append(t.toolOrder, ev.ItemID)
		}
		t.curToolID = ev.ItemID
		if !st.started {
			st.started = true
			st.blockIndex = t.nextBlockIndex
			t.nextBlockIndex++
			id := st.id
			if id == "" {
				id = ev.ItemID
			}
			name := st.name
			if name == "" {
				name = "tool" // 防御: name 未随 item.added 到达
			}
			out = append(out, types.StreamDelta{
				Type: "content_block_start", Index: st.blockIndex,
				ContentBlock: &types.ContentBlock{
					Type: types.ContentBlockToolUse, ID: id, Name: name,
					Input: json.RawMessage("{}"),
				},
			})
		}
		if ev.Delta != "" {
			out = append(out, types.StreamDelta{
				Type: "content_block_delta", Index: st.blockIndex,
				Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: ev.Delta},
			})
		}
		return out, nil
	case "response.function_call_arguments.done":
		return out, nil
	case "response.output_item.done":
		if ev.Item == nil {
			return out, nil
		}
		switch ev.Item.Type {
		case "message":
			if t.textOpen {
				t.textOpen = false
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.textBlockIndex})
			}
		case "function_call":
			if st := t.toolItems[ev.Item.ID]; st != nil {
				if !st.started {
					// 无任何 arguments delta 就到 done: 补一个空工具块
					st.started = true
					st.blockIndex = t.nextBlockIndex
					t.nextBlockIndex++
					id := st.id
					if id == "" {
						id = ev.Item.ID
					}
					name := st.name
					if name == "" {
						name = ev.Item.Name
					}
					if name == "" {
						name = "tool"
					}
					out = append(out, types.StreamDelta{
						Type: "content_block_start", Index: st.blockIndex,
						ContentBlock: &types.ContentBlock{
							Type: types.ContentBlockToolUse, ID: id, Name: name,
							Input: json.RawMessage("{}"),
						},
					})
				}
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: st.blockIndex})
				st.stopped = true
			}
		}
		return out, nil
	case "response.completed":
		if ev.Response != nil {
			t.status = ev.Response.Status
			if ev.Response.ID != "" {
				t.id = ev.Response.ID
			}
			if ev.Response.Usage != nil {
				t.usage = &types.Usage{
					InputTokens:          ev.Response.Usage.InputTokens,
					OutputTokens:         ev.Response.Usage.OutputTokens,
					CacheReadInputTokens: ev.Response.Usage.InputTokensDetails.CachedTokens,
				}
			}
		}
		// 收尾: 若有未关闭块先关闭 (防御), 再发 message_delta
		if t.textOpen {
			t.textOpen = false
			out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.textBlockIndex})
		}
		for _, id := range t.toolOrder {
			// 已在其 item.done 处关闭的块不重复 stop
			if st := t.toolItems[id]; st != nil && st.started && !st.stopped {
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: st.blockIndex})
			}
		}
		// 与 parseOpenAIResponsesResponse 同理: 出现过工具块就报 tool_use,
		// 否则引擎会把工具调用当最终回复。
		stopReason := responsesStatusToStop(t.status)
		for _, id := range t.toolOrder {
			if st := t.toolItems[id]; st != nil && st.started {
				stopReason = string(types.StopReasonToolUse)
				break
			}
		}
		out = append(out, types.StreamDelta{
			Type: "message_delta",
			Delta: &types.DeltaContent{StopReason: stopReason},
			Usage: t.usage,
		})
		t.finished = true
		return out, nil
	}
	// ping / response.in_progress / 其它 → 忽略
	return out, nil
}
