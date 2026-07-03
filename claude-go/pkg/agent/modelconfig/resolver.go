package modelconfig

import "sync"

// ConfigResolver 按 角色 > 团队 > 全局 优先级解析最终配置。
type ConfigResolver struct {
	registry *ProviderRegistry
	global   GlobalConfig
	plans    map[string]PlanConfig
	mu       sync.RWMutex
}

// NewConfigResolver 创建配置解析器。
func NewConfigResolver(registry *ProviderRegistry, global GlobalConfig, plans map[string]PlanConfig) *ConfigResolver {
	if plans == nil {
		plans = make(map[string]PlanConfig)
	}
	return &ConfigResolver{
		registry: registry,
		global:   global,
		plans:    plans,
	}
}

// SetGlobal 更新全局默认配置 (动态更新用)。
func (r *ConfigResolver) SetGlobal(g GlobalConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.global = g
}

// SetPlans 更新 plan 配置 (动态更新用)。
func (r *ConfigResolver) SetPlans(plans map[string]PlanConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if plans == nil {
		plans = make(map[string]PlanConfig)
	}
	r.plans = plans
}

// Resolve 按 plan + role 解析最终配置。
// 优先级: role alias > plan modelAlias > global defaultModelAlias
func (r *ConfigResolver) Resolve(planName, role string) ResolvedConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 1. 从全局默认开始
	modelAlias := r.global.DefaultModelAlias
	fallbackAliases := make([]string, len(r.global.DefaultFallbackAliases))
	copy(fallbackAliases, r.global.DefaultFallbackAliases)
	promptCacheMode := r.global.DefaultPromptCacheMode

	// 2. 应用 plan 级覆盖
	plan, hasPlan := r.plans[planName]
	if hasPlan {
		if plan.ModelAlias != "" {
			modelAlias = plan.ModelAlias
		}
		if len(plan.FallbackAliases) > 0 {
			fallbackAliases = make([]string, len(plan.FallbackAliases))
			copy(fallbackAliases, plan.FallbackAliases)
		}
	}

	// 3. 应用 role 级覆盖 (最高优先级)
	if hasPlan && role != "" {
		if rc, ok := plan.Roles[role]; ok {
			if rc.ModelAlias != "" {
				modelAlias = rc.ModelAlias
			}
			if len(rc.FallbackAliases) > 0 {
				fallbackAliases = make([]string, len(rc.FallbackAliases))
				copy(fallbackAliases, rc.FallbackAliases)
			}
		}
	}

	// 4. 解析 modelAlias -> 完整配置
	resolved := r.resolveAlias(modelAlias)

	// 5. 解析 fallbackAliases -> 实际模型名列表 + 自动计算 fallback 端点
	resolved.FallbackModels, resolved.FallbackBaseURL, resolved.FallbackAPIKey = r.resolveFallbacks(fallbackAliases, resolved.Provider)

	// 6. promptCacheMode 继承全局默认 (若模型未指定)
	if resolved.PromptCacheMode == "" {
		resolved.PromptCacheMode = promptCacheMode
	}

	// 7. MaxTurns/MaxTokens 兜底: 模型未指定 (0) 时套用全局默认上限,
	//    防止 maxTurns=0 (无限) 导致 agent 无界循环烧 token。
	//    需要更多回合/输出的模型可在 model 级显式调大, 会覆盖此兜底。
	if resolved.MaxTurns == 0 && r.global.DefaultMaxTurns > 0 {
		resolved.MaxTurns = r.global.DefaultMaxTurns
	}
	if resolved.MaxTokens == 0 && r.global.DefaultMaxTokens > 0 {
		resolved.MaxTokens = r.global.DefaultMaxTokens
	}

	return resolved
}

// ResolveAlias 将任意单个 alias 解析为 ResolvedConfig (不含 fallback)。
// 供 advisor 等需要在 plan/role 体系之外解析独立模型的场景使用。
func (r *ConfigResolver) ResolveAlias(alias string) ResolvedConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveAlias(alias)
}

// resolveAlias 将单个 alias 解析为 ResolvedConfig (不含 fallback)。
func (r *ConfigResolver) resolveAlias(alias string) ResolvedConfig {
	cfg := ResolvedConfig{Alias: alias}

	entry := r.registry.GetModel(alias)
	if entry == nil {
		// 未知 alias: 兜底尝试从 alias 直接解析 provider 和 modelName
		provider, modelName := splitAlias(alias)
		if provider != "" && modelName != "" {
			cfg.Provider = provider
			cfg.ProviderName = modelName
			// 尝试查找 provider 获取 baseURL/apiKey
			if p := r.registry.GetProvider(provider); p != nil {
				cfg.BaseURL = p.BaseURL
				cfg.APIKey = p.APIKey
			}
		} else {
			cfg.ProviderName = alias
		}
		return cfg
	}

	cfg.ProviderName = entry.ProviderName
	cfg.Provider = entry.Provider.Name
	cfg.BaseURL = entry.Provider.BaseURL
	cfg.APIKey = entry.Provider.APIKey
	cfg.MaxTokens = entry.Params.MaxTokens
	cfg.MaxTurns = entry.Params.MaxTurns
	cfg.PromptCacheMode = entry.Params.PromptCacheMode
	cfg.ContextWindow = entry.Params.ContextWindow
	cfg.RPM = entry.Params.RPM
	cfg.MaxParallel = entry.Params.MaxParallel
	cfg.MinParallel = entry.Params.MinParallel
	cfg.FirstTokenTimeoutSec = entry.Params.FirstTokenTimeoutSec
	cfg.CallTimeoutSec = entry.Params.CallTimeoutSec
	cfg.DeadlineRetryBaseSec = entry.Params.DeadlineRetryBaseSec

	return cfg
}

// resolveFallbacks 将 fallback alias 列表解析为实际模型名列表，
// 并自动计算 fallback 端点: 若存在与主模型不同 provider 的 fallback alias，
// 则返回该 provider 的 baseURL/apiKey (仅取第一个不同 provider)。
func (r *ConfigResolver) resolveFallbacks(aliases []string, primaryProvider string) ([]string, string, string) {
	if len(aliases) == 0 {
		return nil, "", ""
	}

	result := make([]string, 0, len(aliases))
	var fallbackBaseURL, fallbackAPIKey string

	for _, alias := range aliases {
		entry := r.registry.GetModel(alias)
		if entry == nil {
			// 未知 alias: 尝试从 alias 解析后半段作为实际模型名
			_, modelName := splitAlias(alias)
			if modelName != "" {
				result = append(result, modelName)
			} else {
				result = append(result, alias)
			}
			continue
		}
		result = append(result, entry.ProviderName)
		// 自动计算 fallback 端点: 取第一个与主模型不同 provider 的 fallback
		if fallbackBaseURL == "" && entry.Provider != nil && entry.Provider.Name != primaryProvider {
			fallbackBaseURL = entry.Provider.BaseURL
			fallbackAPIKey = entry.Provider.APIKey
		}
	}

	return result, fallbackBaseURL, fallbackAPIKey
}
