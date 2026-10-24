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
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
)

// ReasoningPlayback 出站回传上一轮推理所用的 OpenAI 字段名 (见 Client.ReasoningPlayback)。
// 两家上游收在不同字段上, 不能互相替代:
//
//	ReasoningPlaybackReasoning        = "reasoning"          // opencode 系 (step-5 等)
//	ReasoningPlaybackReasoningContent = "reasoning_content"  // DeepSeek / R1 系
//
// 空字符串 = 不回传, 是默认值。**不做自动嗅探**: DeepSeek 官方文档明确拒绝在请求里
// 回传 reasoning_content (传了直接报错), 而 opencode:step-5 实测接受; 两家要求相反,
// 猜错一边就是"修一个模型打死另一个模型"。所以这是模型级配置项, 由 config.json
// 逐台决定, 不在代码里推断。
const (
	ReasoningPlaybackReasoning        = "reasoning"
	ReasoningPlaybackReasoningContent = "reasoning_content"
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
	Role       string           `json:"role"`
	Content    interface{}      `json:"content,omitempty"` // string | []openAIContentPart | null
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	// Reasoning / ReasoningContent 出站回传上一轮的思维链 (仅 assistant 消息)。
	//
	// 两家上游收在不同字段名上, 所以两个都留, 由 ReasoningPlayback 决定填哪个:
	//   - reasoning:          opencode 系的 step-5 等
	//   - reasoning_content:  DeepSeek / R1 系
	//
	// 两个都是 omitempty: 不开回传时这两个字段**不出现在线上报文里**, 与改造前
	// 逐字节相同 —— 不能因为"结构体上加了字段"就改变默认请求的形状, 那会让所有
	// 没配这个开关的模型白白多背一份请求体, 也可能撞上不认该字段的上游。
	//
	// 必要性: 网关是无状态的 (openAIRequest 里没有 previous_response_id 之类的
	// 服务端会话键), 每轮都全量重发 messages[]。上游"知道"自己上一轮推理的唯一
	// 前提是那段推理在这次的 body 里。此前它被 assistantBlocksToOpenAI 丢弃,
	// 于是多轮工具循环里模型看不到自己上一轮的推理。
	Reasoning        string `json:"reasoning,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
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
	oreq, err := anthropicToOpenAI(req, stream, c.ReasoningPlayback)
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
//   - assistant 消息的 tool_use 块 → tool_calls 数组; thinking 块按 reasoningPlayback
//     回传 (见 ReasoningPlayback), 为空则丢弃
//   - tools input_schema → OpenAI function.parameters
//   - tool_choice any/tool → "required"
//
// reasoningPlayback 一路显式传参而不是读 Client 字段: 翻译层是纯函数, 调用方从
// 哪来 (Client / 探针 / 测试) 与它无关。读到全局字段上的话, 单测就得先构造一个
// Client 才能验一个字符串该落在哪个字段里。
func anthropicToOpenAI(req types.APIRequest, stream bool, reasoningPlayback string) (*openAIRequest, error) {
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
		ms, err := anthropicMessageToOpenAI(m, reasoningPlayback)
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
func anthropicMessageToOpenAI(msg types.APIMessage, reasoningPlayback string) ([]openAIMessage, error) {
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
		return assistantBlocksToOpenAI(blocks, reasoningPlayback)
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

// assistantBlocksToOpenAI 把 assistant 消息的 text 块拼成 content, tool_use 块转
// tool_calls, thinking 块按 reasoningPlayback 回传。
//
// reasoningPlayback 为空时 thinking 块依旧丢弃 —— 与改造前逐字节相同。这一点是硬要求:
// 默认请求的形状不能因为结构体多了两个字段就改变, 否则没配这个开关的模型会平白多背
// 一份请求体, 还可能撞上不认该字段的上游。
func assistantBlocksToOpenAI(blocks []types.ContentBlock, reasoningPlayback string) ([]openAIMessage, error) {
	var sb strings.Builder
	var calls []openAIToolCall
	var reasoning strings.Builder
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
			// 上一轮的思维链。回传是**整段重发**, 不是增量: 上游要在这一轮看到
			// 完整推理, 只发新的一段会让它对着半截推理"接上下文", 比不发更容易出
			// 矛盾结论。多个 thinking 块按出现顺序拼接 (同一条 assistant 消息里
			// 正常只有一个, 但压缩/重试路径上可能出现多个分段)。
			if reasoningPlayback != "" && b.Thinking != "" {
				if reasoning.Len() > 0 {
					reasoning.WriteString("\n")
				}
				reasoning.WriteString(b.Thinking)
			}
		}
	}
	var content interface{}
	if sb.Len() > 0 {
		content = sb.String()
	}
	msg := openAIMessage{Role: "assistant", Content: content}
	if reasoning.Len() > 0 {
		switch reasoningPlayback {
		case ReasoningPlaybackReasoningContent:
			msg.ReasoningContent = reasoning.String()
		default:
			// "reasoning" 以及任何未登记的值都走这个字段 —— 保守的一侧:
			// 空值已经在上面被挡住 (不进这个分支), 而写错字段名最多是上游忽略
			// 一个它不认识的键, 不会把字段内容漏给另一个语义不同的位。
			msg.Reasoning = reasoning.String()
		}
	}
	if len(calls) == 0 {
		return []openAIMessage{msg}, nil
	}
	msg.ToolCalls = calls
	return []openAIMessage{msg}, nil
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
			Role      string           `json:"role"`
			Content   json.RawMessage  `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
			// Reasoning / ReasoningContent: 隐藏推理模型的思维链 (非流式同样给)。
			// 同帧三个字段内容重复, parseOpenAIResponse 取最长的一个。
			Reasoning        string                `json:"reasoning"`
			ReasoningContent string                `json:"reasoning_content"`
			ReasoningDetails []openAIReasoningItem `json:"reasoning_details"`
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
		resp.Content = openAIContentToBlocks(ch.Message.Content, ch.Message.ToolCalls, openAIReasoningText(ch.Message.Reasoning, ch.Message.ReasoningContent, ch.Message.ReasoningDetails))
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

// openAIContentToBlocks 把 OpenAI message.content (string | 块数组) + tool_calls →
// Anthropic 块。reasoning 非空时以 thinking 块**排在正文之前** —— 上游的处理顺序
// 就是先推理后作答, 引擎/轨迹也按这个顺序读 (ExtractPlan 取首个 thinking 块)。
func openAIContentToBlocks(content json.RawMessage, calls []openAIToolCall, reasoning string) []types.ContentBlock {
	var out []types.ContentBlock
	if reasoning != "" {
		out = append(out, types.ContentBlock{Type: types.ContentBlockThinking, Thinking: reasoning})
	}
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
	finishPending  bool   // finish_reason 已到但 usage 未到账(独立尾帧), message_delta 挂起待补发
	finishReason   string // 挂起中的 stop_reason (Anthropic 枚举)
	id             string
	nextBlockIndex int

	textOpen       bool
	textBlockIndex int
	trailing       strings.Builder // 工具块打开后到来的文本 (极少数交错), finish 时补发

	// thinking 块状态。与 text/tool 一样遵循"同一时刻只开一个块": reasoning
	// 到达时若 text 开着就先关 text, 反之亦然。
	thinkingOpen       bool
	thinkingBlockIndex int
	// reasoningSeen / reasoningLen 记录已发出过的思维链前缀。不同上游对
	// reasoning 字段的语义不同 (增量串 vs 累计快照), reasoningDelta 靠这对值判别。
	reasoningSeen string
	reasoningLen  int

	toolBlocks map[int]*openAIToolState
	// openTool 当前**开着**的工具块 (OpenAI tool_call index); -1 = 没有。
	// 引擎同一时刻只跟踪一个当前块, 所以它是"该关谁"的唯一依据 ——
	// 不能靠遍历全部工具块去关, 那正是并行开块的成因 (见包注释)。
	openTool int
	usage    *types.Usage
}

type openAIToolState struct {
	blockIndex int
	id         string
	name       string
	args       strings.Builder
	started    bool
}

// newOpenAIStreamTranslator 构造流式翻译器; openTool 初始为 -1, 表示当前没有打开的工具块。
func newOpenAIStreamTranslator(model string) *openAIStreamTranslator {
	return &openAIStreamTranslator{model: model, toolBlocks: map[int]*openAIToolState{}, openTool: -1}
}

// openAIUsage 流式 usage (chat.completion.chunk 尾帧 / 非流式 usage)。opencode 等上游
// 在 finish_reason 之后单独发一个 {choices:[], usage:{...}} 帧, 而非与 finish 同帧。
type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// captureUsage 把上游 usage 存入翻译器, 供 message_delta 记账。
func (t *openAIStreamTranslator) captureUsage(u *openAIUsage) {
	if u == nil {
		return
	}
	t.usage = &types.Usage{
		InputTokens:          u.PromptTokens,
		OutputTokens:         u.CompletionTokens,
		CacheReadInputTokens: u.PromptTokensDetails.CachedTokens,
	}
}

// reasoningText 非流式思维的归一: 三个候选字段取最长的非空值。
// 为什么取最长而不是固定某一个: 同一个 reasoning 概念在不同上游的字段名不同
// (opencode/GLM 用 reasoning, DeepSeek 用 reasoning_content), 而三个字段同帧出现时
// 内容是**同一段文字**, 取任意一个都等于全部内容 —— 取最长只是为了让"某个上游
// 把推理切片到 reasoning_details 而主字段为空"的情形也不丢内容。
func reasoningText(fields ...string) string {
	longest := ""
	for _, f := range fields {
		if len([]rune(f)) > len([]rune(longest)) {
			longest = f
		}
	}
	return longest
}

// openAIReasoningText 把非流式 message 的三个推理字段归一为一个字符串。
// 签名收 []openAIReasoningItem 而不是 string: reasoning_details 是**数组**, 收 string
// 会让调用方为了凑类型把整段拼好的文字再塞进来 —— 那种"先拼后拆"的签名是给重构挖坑。
func openAIReasoningText(reasoning, reasoningContent string, details []openAIReasoningItem) string {
	return reasoningText(reasoning, reasoningContent, reasoningDetailText(details))
}

// openAIReasoningItem 是上游 reasoning_details[] 的一个分片 (opencode/OpenRouter
// 风格: 每片只有文本, 正文之间是同一段思维链的连续部分)。
type openAIReasoningItem struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Index *int   `json:"index"`
}

// reasoningDetailText 把 reasoning_details[] 拼回整段思维链。
func reasoningDetailText(items []openAIReasoningItem) string {
	var sb strings.Builder
	for _, it := range items {
		if it.Text != "" {
			sb.WriteString(it.Text)
		}
	}
	return sb.String()
}

// reasoningCandidates 汇总一帧里所有可能出现推理的字段。三个字段内容重复
// (opencode 同帧给 reasoning 与 reasoning_details), 所以调用方按"取最长"归一,
// 而不是叠加 —— 叠加会让同一句话在轨迹里出现两遍。
func reasoningCandidates(reasoning, reasoningContent string, details []openAIReasoningItem) []string {
	return []string{reasoning, reasoningContent, reasoningDetailText(details)}
}

// reasoningDelta 从一帧的上游推理字段里取出"尚未发过的思维链增量"。
//
// 为什么需要归一: 同一个"思维链"概念在不同上游的名字和语义都不同 ——
//   - opencode step-5: delta.reasoning 是**增量**, 同帧还带一份内容完全相同的
//     reasoning_details[{text}] (不是两份内容, 别叠加);
//   - 智谱 GLM: delta.reasoning 是**累计快照** (chunk1="a", chunk2="ab");
//   - DeepSeek/R1: delta.reasoning_content, 增量。
//
// 判据不靠字段名 (名字会漂, 且同名字段在不同上游语义相反), 而靠**内容关系**:
// 若本帧最长的那个字段以上一帧的前缀开头, 它就是快照, 只发多出来的那一截;
// 否则当增量整段发。多候选字段并存时取最长的那一个作为"本帧新值", 这样
// reasoning 与 reasoning_details 的重复就不会让同一句话翻倍。
func (t *openAIStreamTranslator) reasoningDelta(fields []string) string {
	newest := ""
	for _, f := range fields {
		if f == "" {
			continue
		}
		if len([]rune(f)) > len([]rune(newest)) {
			newest = f
		}
	}
	if newest == "" {
		return ""
	}
	runes := []rune(newest)
	cur := t.reasoningLen
	if cur > len(runes) {
		// 上游缩短了 (重试/换 key 后的新一轮): 之前的积累全部作废, 按增量重开。
		t.reasoningSeen, t.reasoningLen = "", 0
		cur = 0
	}
	if cur == 0 || string(runes[:cur]) == t.reasoningSeen[:cur] {
		// 前缀吻合: 本帧是在已发内容之后追加 (快照形态, 或增量的首个分片)。
		delta := string(runes[cur:])
		t.reasoningSeen = newest
		t.reasoningLen = len(runes)
		return delta
	}
	// 前缀不吻合: 上游换了路子 (例如先发快照后改发增量)。此时不能再按偏移量裁,
	// 否则会把没发过的内容裁掉; 整段发, 并把已发记录重置为本帧 —— 重复的那部分
	// 由前缀判据在下一次自动吸收。
	t.reasoningSeen = newest
	t.reasoningLen = len(runes)
	return newest
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
				// Reasoning: 隐藏推理模型的思维链增量。opencode 的 step-5 系列
				// 每个 reasoning chunk 都同时给 `reasoning` (增量串) 与
				// `reasoning_details[0].text` (**同一段文字**, 不是两份), 所以只取
				// 最长的那一个, 否则同一句话会翻倍。智谱 GLM 同名字段是全量快照,
				// 由 reasoningDelta 的前缀判据分开。
				Reasoning string `json:"reasoning"`
				// ReasoningContent: DeepSeek / 智谱把推理放在这个字段 (同为增量)。
				ReasoningContent string `json:"reasoning_content"`
				// ReasoningDetails: OpenRouter 风格的分片数组, 每片只有 text/type。
				// 它与上面两个字段**内容重复**, 不独立取用。
				ReasoningDetails []openAIReasoningItem `json:"reasoning_details"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, err
	}
	if t.finished {
		return nil, nil
	}

	var out []types.StreamDelta

	// finish_reason 已见但 usage 未随帧 (finishPending): 等待紧随其后的 usage-only 尾帧。
	// opencode/omen/hy3 就是 finish 后单独发 {choices:[],usage:{...}} → 这里吸收并补发挂起的
	// message_delta, 否则引擎收不到 stop_reason 且 usage 永远落 0。
	if t.finishPending {
		if chunk.Usage != nil {
			t.captureUsage(chunk.Usage)
			t.finishPending = false
			t.finished = true
			out = append(out, types.StreamDelta{
				Type:  "message_delta",
				Delta: &types.DeltaContent{StopReason: t.finishReason},
				Usage: t.usage,
			})
		}
		return out, nil
	}

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
	t.captureUsage(chunk.Usage)

	for _, ch := range chunk.Choices {
		// 0) 思维链增量 (先于正文: 上游的处理顺序是 reasoning → content)
		//
		// 隐藏推理模型 (opencode:step-5-preview-free, GLM 等) 会把大半个
		// max_tokens 烧在 reasoning 上; 不透传时轨迹里只剩一个空的 text 块 ——
		// 看着像"模型什么都没说", 而 content 其实压根还没开始。这里把它翻成
		// Anthropic 的 thinking 块 (engine.go 已有 thinking_delta 消费路径,
		// pkg/engine/runner.go 会把它并进本轮文本)。
		if rt := t.reasoningDelta(reasoningCandidates(ch.Delta.Reasoning, ch.Delta.ReasoningContent, ch.Delta.ReasoningDetails)); rt != "" {
			// thinking 与 text 同属"当前块", 开 thinking 前必须先关 text。
			if t.textOpen {
				t.textOpen = false
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.textBlockIndex})
			}
			if t.openTool >= 0 {
				// 不会发生 (tool_calls 与 reasoning 不同帧), 但保持串行不变量:
				// 真出现就先关工具块, 否则两个块同时开着。
				if st := t.toolBlocks[t.openTool]; st != nil && st.started {
					out = append(out, types.StreamDelta{Type: "content_block_stop", Index: st.blockIndex})
				}
				t.openTool = -1
			}
			if !t.thinkingOpen {
				t.thinkingOpen = true
				t.thinkingBlockIndex = t.nextBlockIndex
				t.nextBlockIndex++
				out = append(out, types.StreamDelta{
					Type: "content_block_start", Index: t.thinkingBlockIndex,
					ContentBlock: &types.ContentBlock{Type: types.ContentBlockThinking},
				})
			}
			out = append(out, types.StreamDelta{
				Type: "content_block_delta", Index: t.thinkingBlockIndex,
				Delta: &types.DeltaContent{Type: "thinking_delta", Thinking: rt},
			})
		}

		// 1) 文本增量
		if text := openAIContentText(ch.Delta.Content); text != "" {
			// thinking 开着就先关 (引擎同一时刻只跟踪一个当前块)。
			if t.thinkingOpen {
				t.thinkingOpen = false
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.thinkingBlockIndex})
			}
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
			// 切到新的 tool_call index 之前, 先把上一个工具块关掉 —— 引擎同一时刻
			// 只跟踪一个当前块 (见包注释 "绝不并行开两个块")。若把关闭一路推迟到
			// finish_reason, ≥2 个工具调用就会同时开着: content_block_start 覆盖
			// currentBlock, 结果是**丢掉前一个、复制最后一个** (实测 opencode 双工具
			// 回合: Glob 被丢, Shell 出现两次), 两份 tool 结果因此带上同一个
			// tool_call_id, 上游直接判 400 invalid request。
			if t.openTool >= 0 && t.openTool != tc.Index {
				if prev := t.toolBlocks[t.openTool]; prev != nil && prev.started {
					out = append(out, types.StreamDelta{Type: "content_block_stop", Index: prev.blockIndex})
				}
				t.openTool = -1
			}
			st := t.toolBlocks[tc.Index]
			if st == nil {
				st = &openAIToolState{id: tc.ID}
				t.toolBlocks[tc.Index] = st
			}
			if tc.ID != "" {
				st.id = tc.ID
			}
			if tc.Function.Name != "" {
				st.name = tc.Function.Name
			}
			// 文本块 / thinking 块若开着, 先关闭 (引擎只跟踪一个当前块)
			if t.thinkingOpen {
				t.thinkingOpen = false
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.thinkingBlockIndex})
			}
			if t.textOpen {
				t.textOpen = false
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.textBlockIndex})
			}
			if !st.started {
				st.started = true
				t.openTool = tc.Index
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
			if t.thinkingOpen {
				t.thinkingOpen = false
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.thinkingBlockIndex})
			}
			if t.textOpen {
				t.textOpen = false
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: t.textBlockIndex})
			}
			// 关掉仍开着的那一个工具块。其余工具块在切换 index 时已经逐个关过 ——
			// 引擎只跟踪一个当前块, 这里不能遍历全部去关。
			if t.openTool >= 0 {
				if st := t.toolBlocks[t.openTool]; st != nil && st.started {
					out = append(out, types.StreamDelta{Type: "content_block_stop", Index: st.blockIndex})
				}
				t.openTool = -1
			}
			if t.trailing.Len() > 0 {
				bi := t.nextBlockIndex
				t.nextBlockIndex++
				out = append(out, types.StreamDelta{Type: "content_block_start", Index: bi, ContentBlock: &types.ContentBlock{Type: types.ContentBlockText}})
				out = append(out, types.StreamDelta{Type: "content_block_delta", Index: bi, Delta: &types.DeltaContent{Type: "text_delta", Text: t.trailing.String()}})
				out = append(out, types.StreamDelta{Type: "content_block_stop", Index: bi})
			}
			if t.usage != nil {
				// usage 与 finish_reason 同帧 (deepseek/glm 等 OpenAI 标准): 直接收尾记账。
				out = append(out, types.StreamDelta{
					Type:  "message_delta",
					Delta: &types.DeltaContent{StopReason: openAIFinishToStop(ch.FinishReason)},
					Usage: t.usage,
				})
				t.finished = true
			} else {
				// usage 在 finish 之后的独立尾帧 (opencode/omen/hy3): 块已关完,
				// 但 message_delta 挂起 —— 等 usage-only 尾帧吸收后补发 (见 finishPending 分支),
				// 流末仍没来则由 Flush() 兜底, 保证引擎总能收到 stop_reason。
				t.finishPending = true
				t.finishReason = openAIFinishToStop(ch.FinishReason)
			}
		}
	}
	return out, nil
}

// Flush 在流结束 (EOF / [DONE]) 调用: 若 finish 后 usage 尾帧始终未到 (上游忽略
// include_usage 的退化情形), 补发挂起的 message_delta (usage 可能仍为 nil), 防止引擎
// 因缺 stop_reason 把回复标记为中断。
func (t *openAIStreamTranslator) Flush() []types.StreamDelta {
	if !t.finishPending || t.finished {
		return nil
	}
	t.finishPending = false
	t.finished = true
	return []types.StreamDelta{{
		Type:  "message_delta",
		Delta: &types.DeltaContent{StopReason: t.finishReason},
		Usage: t.usage,
	}}
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
