// Package modelconfig 实现 providers + aliases 配置管理。
//
// 核心概念:
//   - Provider: 一个厂商的 API 连接参数 (baseURL, apiKey)
//   - ModelConfig: 模型元数据 (maxTokens, contextWindow 等)
//   - PlanConfig: 团队级别的模型别名 + 备用模型别名列表 + 角色覆盖
//   - ResolvedConfig: 按 角色>团队>全局 解析后的最终配置
//
// 别名格式: "provider:actualModelName" (如 "dashscope:qwen3.6-plus")
// 通过别名可直接在内存中查找到所有 LLM 连接参数。
//
// 优先级: role alias > plan modelAlias > global defaultModelAlias
package modelconfig

import (
	"strings"
	"sync"
)

// ProviderConfig 单个厂商的 API 连接参数。
type ProviderConfig struct {
	Name    string            `json:"name"`
	BaseURL string            `json:"baseUrl"`
	APIKey  string            `json:"apiKey"`
	Models  map[string]ModelConfig `json:"models"` // key = alias ("provider:modelName")
}

// ModelConfig 单个模型的元数据。
// 注意: 别名本身已经编码了 provider 和实际模型名 (格式 "provider:modelName")，
// 因此 ModelConfig 不再包含 Alias 和 ProviderName 字段。
type ModelConfig struct {
	MaxTokens       int    `json:"maxTokens,omitempty"`       // 默认 max_tokens
	MaxTurns        int    `json:"maxTurns,omitempty"`        // 默认 max_turns
	PromptCacheMode string `json:"promptCacheMode,omitempty"` // "auto" / "on" / "off"
	ContextWindow   int    `json:"contextWindow,omitempty"`   // 上下文窗口长度
	// 模型专属限流参数 (0 = 使用全局默认值)
	RPM         int `json:"rpm,omitempty"`         // 每分钟最大请求数
	MaxParallel int `json:"maxParallel,omitempty"` // 最大并发请求数
	MinParallel int `json:"minParallel,omitempty"` // 最小并发请求数 (AIMD 下限)
	// 超时与退避参数 (0 = 使用全局默认值)
	FirstTokenTimeoutSec int `json:"firstTokenTimeoutSec,omitempty"` // 首token等待时间(秒)
	CallTimeoutSec       int `json:"callTimeoutSec,omitempty"`       // 单次agent调用总时间(秒)
	DeadlineRetryBaseSec int `json:"deadlineRetryBaseSec,omitempty"` // deadline exceeded退避基数(秒)
}

// GlobalConfig 全局默认配置。
type GlobalConfig struct {
	DefaultModelAlias      string   `json:"defaultModelAlias"`
	DefaultFallbackAliases []string `json:"defaultFallbackAliases"`
	DefaultPromptCacheMode string   `json:"defaultPromptCacheMode"`
}

// RoleConfig 角色级别的模型别名配置。
type RoleConfig struct {
	ModelAlias      string   `json:"modelAlias"`
	FallbackAliases []string `json:"fallbackAliases"`
}

// PlanConfig 团队(plan)级别的配置。
type PlanConfig struct {
	ModelAlias      string                `json:"modelAlias"`
	FallbackAliases []string              `json:"fallbackAliases"`
	Roles           map[string]RoleConfig `json:"roles"` // role -> 角色配置
}

// ResolvedConfig 解析后的最终配置，供 api.Client / engine.Config 使用。
type ResolvedConfig struct {
	Alias           string   // 模型别名 (如 "dashscope:qwen3.6-plus")
	ProviderName    string   // 厂商实际模型名 (如 "qwen3.6-plus")
	Provider        string   // provider 名称 (如 "dashscope")
	BaseURL         string   // API endpoint
	APIKey          string   // API key
	FallbackModels  []string // 备用模型 providerName 列表 (已解析为实际模型名)
	FallbackBaseURL string   // 备用端点 (若与主端点不同)
	FallbackAPIKey  string   // 备用 API key
	MaxTokens       int
	MaxTurns        int
	PromptCacheMode string
	ContextWindow   int
	// 模型专属限流参数 (0 = 使用全局默认值)
	RPM         int
	MaxParallel int
	MinParallel int
	// 超时与退避参数 (0 = 使用全局默认值)
	FirstTokenTimeoutSec int // 首token等待时间(秒)
	CallTimeoutSec       int // 单次agent调用总时间(秒)
	DeadlineRetryBaseSec int // deadline exceeded退避基数(秒)
}

// ConfigJSON 顶层 JSON 配置结构 (对应 config.json 中的 ai 段 + providers 段)。
type ConfigJSON struct {
	Providers map[string]ProviderConfig `json:"providers"`
	AI        struct {
		GlobalConfig
		Plans map[string]PlanConfig `json:"plans"`
	} `json:"ai"`
}

// ProviderRegistry 管理所有 provider 和模型的注册表。
type ProviderRegistry struct {
	providers map[string]*ProviderConfig // key=provider name
	models    map[string]*ModelEntry     // key=alias ("provider:modelName")
	mu        sync.RWMutex
}

// ModelEntry 内部索引结构，保存 alias -> (ProviderName + Provider + Params)。
type ModelEntry struct {
	Alias        string
	ProviderName string         // 实际模型名（alias 后半段）
	Provider     *ProviderConfig
	Params       ModelConfig
}

// NewProviderRegistry 创建空注册表。
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
		providers: make(map[string]*ProviderConfig),
		models:    make(map[string]*ModelEntry),
	}
}

// RegisterProvider 注册一个 provider 及其模型。
func (r *ProviderRegistry) RegisterProvider(p ProviderConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()

	pcopy := p
	r.providers[p.Name] = &pcopy

	for alias, mc := range p.Models {
		provider, modelName := splitAlias(alias)
		if provider == "" || modelName == "" {
			continue // 格式非法，跳过
		}
		r.models[alias] = &ModelEntry{
			Alias:        alias,
			ProviderName: modelName,
			Provider:     r.providers[p.Name],
			Params:       mc,
		}
	}
}

// GetProvider 按名称获取 provider。
func (r *ProviderRegistry) GetProvider(name string) *ProviderConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[name]
}

// GetModel 按 alias 获取模型及其 provider。
func (r *ProviderRegistry) GetModel(alias string) *ModelEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.models[alias]
}

// LookupProviderName 将 alias 解析为厂商实际模型名（即 alias 后半段）。
func (r *ProviderRegistry) LookupProviderName(alias string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e := r.models[alias]; e != nil {
		return e.ProviderName
	}
	// 兜底: 尝试从 alias 直接解析后半段
	_, modelName := splitAlias(alias)
	if modelName != "" {
		return modelName
	}
	return alias
}

// LookupBaseURL 将 alias 解析为对应 provider 的 baseURL。
func (r *ProviderRegistry) LookupBaseURL(alias string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e := r.models[alias]; e != nil && e.Provider != nil {
		return e.Provider.BaseURL
	}
	return ""
}

// LookupAPIKey 将 alias 解析为对应 provider 的 apiKey。
func (r *ProviderRegistry) LookupAPIKey(alias string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e := r.models[alias]; e != nil && e.Provider != nil {
		return e.Provider.APIKey
	}
	return ""
}

// LookupModelParams 按 alias 获取 ModelConfig 参数。
func (r *ProviderRegistry) LookupModelParams(alias string) *ModelConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e := r.models[alias]; e != nil {
		return &e.Params
	}
	return nil
}

// LookupProviderByAlias 按 alias 获取 provider 名称（即 alias 前半段）。
func (r *ProviderRegistry) LookupProviderByAlias(alias string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e := r.models[alias]; e != nil && e.Provider != nil {
		return e.Provider.Name
	}
	provider, _ := splitAlias(alias)
	return provider
}

// LookupAliasByModelName 按原始模型名反查完整 alias。
// 若 rawModel 本身就是完整 alias 且已注册，直接返回；
// 否则遍历所有 alias 的 ProviderName（后半段）进行匹配，返回第一个命中的完整 alias；
// 若均未命中，返回 rawModel 原值。
func (r *ProviderRegistry) LookupAliasByModelName(rawModel string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e := r.models[rawModel]; e != nil {
		return rawModel
	}
	for alias, entry := range r.models {
		if entry.ProviderName == rawModel {
			return alias
		}
	}
	return rawModel
}

// AllAliases 返回所有已注册的 alias 列表。
func (r *ProviderRegistry) AllAliases() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.models))
	for alias := range r.models {
		out = append(out, alias)
	}
	return out
}

// Reload 热更新注册表内容 (先清空再重新注册)。
func (r *ProviderRegistry) Reload(providers []ProviderConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = make(map[string]*ProviderConfig, len(providers))
	r.models = make(map[string]*ModelEntry)
	for _, p := range providers {
		pcopy := p
		r.providers[p.Name] = &pcopy
		for alias, mc := range p.Models {
			provider, modelName := splitAlias(alias)
			if provider == "" || modelName == "" {
				continue
			}
			r.models[alias] = &ModelEntry{
				Alias:        alias,
				ProviderName: modelName,
				Provider:     r.providers[p.Name],
				Params:       mc,
			}
		}
	}
}

// splitAlias 将 "provider:modelName" 拆分为 provider 和 modelName。
// 若格式非法返回 ("", "").
func splitAlias(alias string) (provider, modelName string) {
	parts := strings.SplitN(alias, ":", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
