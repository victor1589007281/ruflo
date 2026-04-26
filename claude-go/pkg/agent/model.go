package agent

// PlanConfigKey 是 context.Context 中保存解析后 plan 配置的 key。
// WorkflowExecutor.runAgent 将解析后的配置存入 context,
// 工厂函数 (CreateAgentFunc) 通过 ctx.Value(agent.PlanConfigKey{}) 读取。
type PlanConfigKey struct{}

// ResolvedPlanConfig 最终解析出的 plan 连接参数。
type ResolvedPlanConfig struct {
	Model          string
	BaseURL        string
	APIKey         string
	FallbackModels []string
}

// PlanModels 单个 plan 的模型配置，由 Caller 从 feishu.PlanModelConfig 转换后传入。
type PlanModels struct {
	Model          string
	BaseURL        string
	APIKey         string
	FallbackModels []string
	RoleModels     map[string]string
}

// PlanConfigResolver 按 plan + role 层级解析最终 API 连接参数。
// 模型优先级: role 覆盖 > plan 覆盖 > 全局 role 默认 > 全局默认。
// baseURL/apiKey 优先级: plan 覆盖 > 全局默认。
type PlanConfigResolver struct {
	defaultBaseURL      string
	defaultAPIKey       string
	defaultModel        string
	defaultFallback     []string
	plans               map[string]PlanModels
	globalRoleDefaults  map[string]string // 跨 plan 的 role 模型默认
}

// NewPlanConfigResolver 创建配置解析器。
func NewPlanConfigResolver(
	defaultBaseURL, defaultAPIKey, defaultModel string,
	defaultFallback []string,
	configPlans map[string]PlanModels,
	builtinDefaults map[string]PlanModels,
	globalRoleDefaults map[string]string,
) *PlanConfigResolver {
	plans := make(map[string]PlanModels, len(configPlans))
	// 先写内置默认
	for k, v := range builtinDefaults {
		plans[k] = v
	}
	// 配置覆盖/合并内置默认
	for k, v := range configPlans {
		existing, ok := plans[k]
		if !ok {
			plans[k] = v
			continue
		}
		if v.Model != "" {
			existing.Model = v.Model
		}
		if v.BaseURL != "" {
			existing.BaseURL = v.BaseURL
		}
		if v.APIKey != "" {
			existing.APIKey = v.APIKey
		}
		if len(v.FallbackModels) > 0 {
			existing.FallbackModels = v.FallbackModels
		}
		if len(v.RoleModels) > 0 {
			if existing.RoleModels == nil {
				existing.RoleModels = make(map[string]string, len(v.RoleModels))
			}
			for role, m := range v.RoleModels {
				existing.RoleModels[role] = m
			}
		}
		plans[k] = existing
	}
	if globalRoleDefaults == nil {
		globalRoleDefaults = make(map[string]string)
	}
	return &PlanConfigResolver{
		defaultBaseURL:     defaultBaseURL,
		defaultAPIKey:      defaultAPIKey,
		defaultModel:       defaultModel,
		defaultFallback:    defaultFallback,
		plans:              plans,
		globalRoleDefaults: globalRoleDefaults,
	}
}

// Resolve 按 plan + role 层级解析最终配置。
func (r *PlanConfigResolver) Resolve(plan, role string) ResolvedPlanConfig {
	cfg := ResolvedPlanConfig{
		Model:          r.defaultModel,
		BaseURL:        r.defaultBaseURL,
		APIKey:         r.defaultAPIKey,
		FallbackModels: r.defaultFallback,
	}

	if p, ok := r.plans[plan]; ok {
		// Plan 级覆盖
		if p.BaseURL != "" {
			cfg.BaseURL = p.BaseURL
		}
		if p.APIKey != "" {
			cfg.APIKey = p.APIKey
		}
		if p.Model != "" {
			cfg.Model = p.Model
		}
		if len(p.FallbackModels) > 0 {
			cfg.FallbackModels = p.FallbackModels
		}

		// Role 覆盖 (plan 内, 最高优先级)
		if role != "" {
			if m, ok := p.RoleModels[role]; ok && m != "" {
				cfg.Model = m
				return cfg
			}
		}
		if p.Model != "" {
			return cfg
		}
	}

	// 全局 role 默认 (跨所有 plan)
	if role != "" {
		if m, ok := r.globalRoleDefaults[role]; ok && m != "" {
			cfg.Model = m
		}
	}
	return cfg
}
