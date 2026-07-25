package modelconfig

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
)

// LoadFromJSON 从 JSON 文件加载完整配置并构建注册表 + 解析器。
func LoadFromJSON(path string) (*ProviderRegistry, *ConfigResolver, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg ConfigJSON
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("解析 JSON 失败: %w", err)
	}

	return LoadFromConfig(cfg)
}

// LoadFromConfig 从 ConfigJSON 构建注册表 + 解析器。
func LoadFromConfig(cfg ConfigJSON) (*ProviderRegistry, *ConfigResolver, error) {
	registry := NewProviderRegistry()

	for _, p := range cfg.Providers {
		registry.RegisterProvider(applyProviderKeyFromEnv(p))
	}

	resolver := NewConfigResolver(registry, cfg.AI.GlobalConfig, cfg.AI.Plans)
	return registry, resolver, nil
}

// LoadFromBytes 从 JSON 字节加载。
func LoadFromBytes(data []byte) (*ProviderRegistry, *ConfigResolver, error) {
	var cfg ConfigJSON
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("解析 JSON 失败: %w", err)
	}
	return LoadFromConfig(cfg)
}

// ReloadFromJSON 热更新: 重新加载 JSON 并更新注册表和解析器。
// 调用方需要持有 registry/resolver 引用。
func ReloadFromJSON(path string, registry *ProviderRegistry, resolver *ConfigResolver) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg ConfigJSON
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("解析 JSON 失败: %w", err)
	}

	// 构建 provider 列表用于 Reload
	providers := make([]ProviderConfig, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		providers = append(providers, p)
	}
	registry.Reload(providers)
	resolver.SetGlobal(cfg.AI.GlobalConfig)
	resolver.SetPlans(cfg.AI.Plans)

	return nil
}

// Validate 验证配置完整性。
func Validate(registry *ProviderRegistry, resolver *ConfigResolver) []string {
	var errs []string

	// 检查全局默认 model alias 是否有效
	if resolver.global.DefaultModelAlias != "" {
		if registry.GetModel(resolver.global.DefaultModelAlias) == nil {
			errs = append(errs, fmt.Sprintf("全局默认模型别名 '%s' 未在 providers 中注册", resolver.global.DefaultModelAlias))
		}
		if !strings.Contains(resolver.global.DefaultModelAlias, ":") {
			errs = append(errs, fmt.Sprintf("全局默认模型别名 '%s' 格式非法，应为 'provider:modelName'", resolver.global.DefaultModelAlias))
		}
	}

	// 检查全局 fallback aliases
	for _, alias := range resolver.global.DefaultFallbackAliases {
		if registry.GetModel(alias) == nil {
			errs = append(errs, fmt.Sprintf("全局备用模型别名 '%s' 未在 providers 中注册", alias))
		}
		if !strings.Contains(alias, ":") {
			errs = append(errs, fmt.Sprintf("全局备用模型别名 '%s' 格式非法，应为 'provider:modelName'", alias))
		}
	}

	// 检查每个 plan
	for planName, plan := range resolver.plans {
		if plan.ModelAlias != "" {
			if registry.GetModel(plan.ModelAlias) == nil {
				errs = append(errs, fmt.Sprintf("plan '%s' 的主模型别名 '%s' 未注册", planName, plan.ModelAlias))
			}
			if !strings.Contains(plan.ModelAlias, ":") {
				errs = append(errs, fmt.Sprintf("plan '%s' 的主模型别名 '%s' 格式非法，应为 'provider:modelName'", planName, plan.ModelAlias))
			}
		}
		for _, alias := range plan.FallbackAliases {
			if registry.GetModel(alias) == nil {
				errs = append(errs, fmt.Sprintf("plan '%s' 的备用模型别名 '%s' 未注册", planName, alias))
			}
			if !strings.Contains(alias, ":") {
				errs = append(errs, fmt.Sprintf("plan '%s' 的备用模型别名 '%s' 格式非法，应为 'provider:modelName'", planName, alias))
			}
		}
		for role, rc := range plan.Roles {
			if rc.ModelAlias != "" {
				if registry.GetModel(rc.ModelAlias) == nil {
					errs = append(errs, fmt.Sprintf("plan '%s' role '%s' 的模型别名 '%s' 未注册", planName, role, rc.ModelAlias))
				}
				if !strings.Contains(rc.ModelAlias, ":") {
					errs = append(errs, fmt.Sprintf("plan '%s' role '%s' 的模型别名 '%s' 格式非法，应为 'provider:modelName'", planName, role, rc.ModelAlias))
				}
			}
			for _, alias := range rc.FallbackAliases {
				if registry.GetModel(alias) == nil {
					errs = append(errs, fmt.Sprintf("plan '%s' role '%s' 的备用模型别名 '%s' 未注册", planName, role, alias))
				}
				if !strings.Contains(alias, ":") {
					errs = append(errs, fmt.Sprintf("plan '%s' role '%s' 的备用模型别名 '%s' 格式非法，应为 'provider:modelName'", planName, role, alias))
				}
			}
		}
	}

	return errs
}

// providerKeyEnv 返回某 provider 的 API key 环境变量名, 形如 kimi → KIMI_API_KEY。
// 非字母数字一律转下划线, 以容纳 "azure-openai" 这类带连字符的 provider 名。
func providerKeyEnv(providerName string) string {
	var b strings.Builder
	for _, r := range providerName {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String() + "_API_KEY"
}

// applyProviderKeyFromEnv 用 <PROVIDER>_API_KEY 环境变量覆盖配置里的 apiKey。
//
// 为什么需要它: 容器部署的标准做法是 ConfigMap 只放非敏感结构、密钥从 Secret
// 经环境变量注入。deploy/k8s 的清单**已经这么做了**——ConfigMap 里 apiKey 写的是
// "PLACEHOLDER_OVERRIDE_VIA_ENV", Deployment 从 Secret 注入 KIMI_API_KEY——
// 但此前**全仓没有任何代码读 <PROVIDER>_API_KEY**, 于是占位符原样进了 provider
// 配置, Pod 拿着假 key 去调 LLM。这与 CLAUDE_GO_LLM_GATEWAY 是同一类"清单设了
// 环境变量而代码不读"的缺陷。
//
// 语义: 环境变量非空则覆盖(12-factor 惯例, 容器里 Secret 应当赢)。启动时打一行
// 日志说明哪个 provider 的 key 来自环境变量, 但**绝不打印 key 本身**。
func applyProviderKeyFromEnv(p ProviderConfig) ProviderConfig {
	if p.Name == "" {
		return p
	}
	env := providerKeyEnv(p.Name)
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		p.APIKey = v
		log.Printf("[modelconfig] provider %q 的 apiKey 取自环境变量 %s", p.Name, env)
	}
	return p
}
