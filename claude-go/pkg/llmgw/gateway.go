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

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// ChatRequest 一次对话补全请求 (Anthropic Messages 语义)。
type ChatRequest struct {
	Messages  []types.APIMessage // 对话消息列表
	System    []string           // 系统提示词段 (按序拼接)
	Tools     []types.APITool    // 工具定义
	MaxTokens int                // 最大输出 token 数
}

// LLMGateway LLM 引擎层唯一入口 (design/02 L1)。
type LLMGateway interface {
	// Stream 流式补全: 返回 SSE 增量事件通道与错误通道 (语义同 api.Client.StreamMessage)。
	Stream(ctx context.Context, req ChatRequest) (<-chan types.StreamDelta, <-chan error)
	// Complete 非流式补全 (语义同 api.Client.SendMessage)。
	Complete(ctx context.Context, req ChatRequest) (*types.APIResponse, error)
	// Simple 单轮文本补全: system + user 进, 拼接后的纯文本出 (语义同 api.Client.SimpleComplete)。
	Simple(ctx context.Context, system, user string) (string, error)
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
	return l.Client.StreamMessage(ctx, req.Messages, req.System, req.Tools, req.MaxTokens)
}

// Complete 转调 api.Client.SendMessage。
func (l *Local) Complete(ctx context.Context, req ChatRequest) (*types.APIResponse, error) {
	return l.Client.SendMessage(ctx, req.Messages, req.System, req.Tools, req.MaxTokens)
}

// Simple 转调 api.Client.SimpleComplete。
func (l *Local) Simple(ctx context.Context, system, user string) (string, error) {
	return l.Client.SimpleComplete(ctx, system, user)
}
