package modelconfig

import (
	"os"
	"strings"
	"sync"
)

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

	return ApplyGatewayOverride(resolved)
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
		return ApplyGatewayOverride(cfg)
	}

	cfg.ProviderName = entry.ProviderName
	cfg.Provider = entry.Provider.Name
	cfg.BaseURL = entry.Provider.BaseURL
	cfg.APIKey = entry.Provider.APIKey
	cfg.MaxTokens = entry.Params.MaxTokens
	cfg.MaxTurns = entry.Params.MaxTurns
	cfg.PromptCacheMode = entry.Params.PromptCacheMode
	cfg.Protocol = entry.Params.Protocol
	cfg.ContextWindow = entry.Params.ContextWindow
	cfg.Proxy = entry.Params.Proxy
	cfg.RPM = entry.Params.RPM
	cfg.MaxParallel = entry.Params.MaxParallel
	cfg.MinParallel = entry.Params.MinParallel
	cfg.FirstTokenTimeoutSec = entry.Params.FirstTokenTimeoutSec
	cfg.CallTimeoutSec = entry.Params.CallTimeoutSec
	cfg.DeadlineRetryBaseSec = entry.Params.DeadlineRetryBaseSec

	return ApplyGatewayOverride(cfg)
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

// gatewayEnv 是 LLM 网关地址的环境变量名 (design/02 §3.1 L1)。
const gatewayEnv = "CLAUDE_GO_LLM_GATEWAY"

// GatewayBaseURL 返回 CLAUDE_GO_LLM_GATEWAY 的规范化值 (去空白、去尾斜杠);
// 未设置时返回空串。
//
// 导出的理由: 有些出站端点不经 ConfigResolver 解析 (例如 dashboard 的
// DASHBOARD_LLM_BASE_URL / ANTHROPIC_BASE_URL 两条 env 直通路径), 它们既要
// 复用 ApplyGatewayOverride 的覆盖规则, 又要如实报告"这一跳是否经网关"。
// 让它们各自再 os.Getenv 一遍会把环境变量名散成多份, 迟早漂移 ——
// 变量名只在本文件出现一次。
func GatewayBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv(gatewayEnv)), "/")
}

// GatewayEnvName 返回网关地址环境变量的名字, 供报错信息引用。
//
// 为什么不让调用方写字面量: "设 CLAUDE_GO_LLM_GATEWAY" 这句话出现在报错里最有用,
// 而报错里的字面量是不会跟着改名的那一份 —— 变量名仍只在本文件定义一次。
func GatewayEnvName() string { return gatewayEnv }

// ApplyGatewayOverride 在设置了 CLAUDE_GO_LLM_GATEWAY 时把出站 BaseURL 改指网关。
//
// 为什么需要它: deploy/k8s/distributed.yaml 给 control 与 worker 都注入了
// CLAUDE_GO_LLM_GATEWAY, 但此前**全仓 Go 代码零读取**——于是分布式拓扑里那个
// gateway pod 是装饰品, 各副本仍各自直连 provider, design/02 §3.1 承诺的
// "多副本共享同一网关 = 共享配额观测 + 集中记账"一项都拿不到。
//
// 覆盖点选在 ConfigResolver 的出口而非各 api.NewClient 调用点: 后者散落在
// feishu/CLI/advisor/worker 多处, 逐个改必然漏 (这正是历史上鉴权逐路由包装
// 漏掉两组的同一类错误)。
//
// 路径约定: api.Client 会给 BaseURL 拼 "/messages", 而网关正好在 /messages 与
// /v1/messages 上服务, 故直接用网关根地址即可, 无需带路径。
//
// 注意只改 BaseURL 不改 APIKey: 网关负责向真实 provider 注入凭据, 但客户端
// 到网关这一跳仍可能需要鉴权头, 保持原样透传由网关决定是否校验。
func ApplyGatewayOverride(cfg ResolvedConfig) ResolvedConfig {
	gw := GatewayBaseURL()
	if gw == "" || cfg.BaseURL == "" {
		return cfg
	}
	cfg.BaseURL = gw
	// fallback 端点一并改指网关: 否则主端点走网关而降级路径直连 provider,
	// 集中记账与配额观测在最需要的时候(主端点故障)恰好失效。
	if cfg.FallbackBaseURL != "" {
		cfg.FallbackBaseURL = cfg.BaseURL
	}
	return cfg
}
