// Package providers 实现多厂商 LLM 后端与统一路由。
//
// 整体架构：
//   - LLMProvider：策略模式中的「策略」接口，屏蔽各厂商 HTTP/JSON 差异，对外统一为 Complete / StreamComplete /
//     HealthCheck / EstimateCost。
//   - 各 *Provider 实现：将 api.LLMRequest（或为避免循环依赖而使用的本地别名类型）转换为厂商 REST 形态，
//     解析响应后再填回 api.LLMResponse。
//   - ProviderManager：集中注册、选型、fallback 与用量统计（见 manager.go）。
package providers

import (
	"context"
	"io"

	"github.com/ruflo/ruflo-go/api"
)

// LLMProvider 抽象单次补全、流式（占位）、健康检查与相对成本估计，供管理器做负载与降级决策。
type LLMProvider interface {
	Name() string
	Complete(ctx context.Context, req api.LLMRequest) (*api.LLMResponse, error)
	StreamComplete(ctx context.Context, req api.LLMRequest) (io.ReadCloser, error)
	HealthCheck(ctx context.Context) error
	EstimateCost(req api.LLMRequest) float64
}

// CostRecord 记录一次调用对应的厂商、模型与估算美元花费，用于可观测性汇总。
type CostRecord struct {
	Provider string  `json:"provider"` // 提供方标识
	Model    string  `json:"model"`    // 模型名
	USD      float64 `json:"usd"`      // 估算费用（美元）
}
