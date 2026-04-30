package agent

import (
	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
)

// PlanConfigKey 是 context.Context 中保存解析后 plan 配置的 key。
// 已废弃: 请改用 ModelConfigKey。
type PlanConfigKey struct{}

// ModelConfigKey 是 context.Context 中保存 modelconfig.ResolvedConfig 的 key。
// 用于传递更丰富的模型配置 (alias, provider, maxTokens 等)。
type ModelConfigKey struct{}

// PlanConfigResolver 按 plan + role 层级解析最终 API 连接参数。
// 现在是 modelconfig.ConfigResolver 的轻量包装，保持向后兼容的接口。
type PlanConfigResolver struct {
	modelResolver *modelconfig.ConfigResolver
}

// NewPlanConfigResolver 从 modelconfig 模块创建解析器 (唯一构造方式)。
func NewPlanConfigResolver(resolver *modelconfig.ConfigResolver) *PlanConfigResolver {
	return &PlanConfigResolver{
		modelResolver: resolver,
	}
}

// Resolve 按 plan + role 层级解析最终配置。
func (r *PlanConfigResolver) Resolve(plan, role string) modelconfig.ResolvedConfig {
	if r.modelResolver != nil {
		return r.modelResolver.Resolve(plan, role)
	}
	return modelconfig.ResolvedConfig{}
}

// Registry 返回内部的 ProviderRegistry。
func (r *PlanConfigResolver) Registry() *modelconfig.ProviderRegistry {
	if r.modelResolver != nil {
		// ConfigResolver 没有直接暴露 registry 的方法，
		// 但 PlanConfigResolver 在初始化时通常同时持有 registry 引用。
		// 如需访问 registry，请通过外部持有的引用直接访问。
	}
	return nil
}
