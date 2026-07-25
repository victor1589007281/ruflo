// Package llmgw 实现 design/02 §3.1 的 L1 LLM 引擎层抽象 (R0 接口抽取)。
//
// LLMGateway 是 LLM 调用的唯一入口: 上层 (编排/运行时/用户层) 只依赖本接口,
// 不直接触碰 api.Client。实现两态:
//   - local:  本包 Local, 薄包 api.Client (默认, 零网络开销);
//   - remote: 独立 llm-gateway 进程 (供应商路由/fallback/熔断/配额集中化),
//     属 R1 范畴, 本包先钉接口。
package llmgw

import (
	"context"
	"encoding/json"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// Trace 是 design/02 §3.1 要求随请求携带的 trace 四元组
// {RunID, NodeID, TurnID, CallID}, 也是 design/03 轨迹底座的关联键。
// 类型别名到 pkg/api: 出站落头这一步只有 api.Client 做得到, 两处各定义一份
// 结构体迟早漂移。
type Trace = api.Trace

// WithTrace 把 trace 四元组放进 ctx。
//
// 为什么 Simple 不像 Stream/Complete 那样从请求结构体里读 trace: 它的签名
// (system, user string) 是十几个包共用的最小 LLM 端口形态, 加参数会波及全部
// 实现方。上层用 WithTrace 包一下 ctx 即可, 三个方法一致生效。
func WithTrace(ctx context.Context, t Trace) context.Context { return api.WithTrace(ctx, t) }

// ChatRequest 一次对话补全请求 (Anthropic Messages 语义)。
type ChatRequest struct {
	Messages  []types.APIMessage // 对话消息列表
	System    []string           // 系统提示词段 (按序拼接)
	Tools     []types.APITool    // 工具定义
	MaxTokens int                // 最大输出 token 数
	// Trace 四元组 (design/02 §3.1 "ChatRequest 必带")。零值时沿用 ctx 里已有的
	// trace, 不会把它抹掉 —— 否则中途构造 ChatRequest 的适配层会静默切断链路。
	Trace Trace
}

// LLMGateway LLM 引擎层唯一入口 (design/02 L1)。
type LLMGateway interface {
	// Stream 流式补全: 返回 SSE 增量事件通道与错误通道 (语义同 api.Client.StreamMessage)。
	Stream(ctx context.Context, req ChatRequest) (<-chan types.StreamDelta, <-chan error)
	// Complete 非流式补全 (语义同 api.Client.SendMessage)。
	Complete(ctx context.Context, req ChatRequest) (*types.APIResponse, error)
	// Simple 单轮文本补全: system + user 进, 拼接后的纯文本出 (语义同 api.Client.SimpleComplete)。
	Simple(ctx context.Context, system, user string) (string, error)
	// Raw 以 Anthropic content 数组直接发一次请求 (图文混排的多模态入口),
	// 语义同 api.Client.RawComplete。
	//
	// 为什么接口里必须有它 (design/02 §3.1 只列了 Stream/Complete/Models):
	// 视觉相关的生产能力 (pkg/vision 的截图理解、pkg/wiki 的图文体检、
	// pkg/agent 的 vision_critic) 都走 content 数组而不是 messages+system。
	// 网关若不提供这条, 上层就必须同时持有一个 *api.Client —— "L1 是唯一 LLM
	// 出口"当场破功。宁可把接口做全, 也不留一条绕过口。
	Raw(ctx context.Context, contentJSON json.RawMessage, maxTokens int) (string, error)
}

// Local 本地实现: 直接转调进程内 api.Client (重试/熔断/限流/指标均由 Client 承担)。
type Local struct {
	Client *api.Client
}

var _ LLMGateway = (*Local)(nil)

// NewLocal 用给定的 api.Client 创建本地网关。
func NewLocal(c *api.Client) *Local {
	return &Local{Client: c}
}

// Stream 转调 api.Client.StreamMessage。
func (l *Local) Stream(ctx context.Context, req ChatRequest) (<-chan types.StreamDelta, <-chan error) {
	return l.Client.StreamMessage(withReqTrace(ctx, req), req.Messages, req.System, req.Tools, req.MaxTokens)
}

// Complete 转调 api.Client.SendMessage。
func (l *Local) Complete(ctx context.Context, req ChatRequest) (*types.APIResponse, error) {
	return l.Client.SendMessage(withReqTrace(ctx, req), req.Messages, req.System, req.Tools, req.MaxTokens)
}

// Simple 转调 api.Client.SimpleComplete。
func (l *Local) Simple(ctx context.Context, system, user string) (string, error) {
	return l.Client.SimpleComplete(ctx, system, user)
}

// Raw 转调 api.Client.RawComplete。
func (l *Local) Raw(ctx context.Context, contentJSON json.RawMessage, maxTokens int) (string, error) {
	return l.Client.RawComplete(ctx, contentJSON, maxTokens)
}

// withReqTrace 请求里带了 trace 就以它为准, 没带则保留 ctx 里已有的。
func withReqTrace(ctx context.Context, req ChatRequest) context.Context {
	if req.Trace.IsZero() {
		return ctx
	}
	return api.WithTrace(ctx, req.Trace)
}

// SimpleClient 把 LLMGateway 适配成"只有 SimpleComplete"的最小 LLM 端口。
//
// 为什么需要它: pkg/wiki.LLMClient、pkg/vision.LLMClient、pkg/skills.LLMClient、
// pkg/dreaming、pkg/swarm_intel 等十余个包各自声明了同名同签名的
// SimpleComplete(ctx, system, user) 端口, 历史上一律直接注入 *api.Client。
// 想让这些包改依赖 L1 网关, 要么逐包改接口定义 (十余处, 且是别人的包),
// 要么在注入点套一层。选后者: 注入点只有几处, 改动面小, 且各包的端口定义
// 一个字都不用动。
type SimpleClient struct {
	GW LLMGateway
}

// SimpleComplete 转调 LLMGateway.Simple。
func (s SimpleClient) SimpleComplete(ctx context.Context, system, user string) (string, error) {
	return s.GW.Simple(ctx, system, user)
}

// RawComplete 转调 LLMGateway.Raw。
// 有它才够格当 pkg/wiki.LLMClient / pkg/vision.LLMClient —— 那两个端口都是
// "文本 + 多模态"两件套。
func (s SimpleClient) RawComplete(ctx context.Context, contentJSON json.RawMessage, maxTokens int) (string, error) {
	return s.GW.Raw(ctx, contentJSON, maxTokens)
}
