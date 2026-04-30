package modelconfig

import (
	"encoding/json"
	"fmt"
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
		registry.RegisterProvider(p)
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
