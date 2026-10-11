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
	Name    string                 `json:"name"`
	BaseURL string                 `json:"baseUrl"`
	APIKey  string                 `json:"apiKey"`
	Models  map[string]ModelConfig `json:"models"` // key = alias ("provider:modelName")
}

// ModelConfig 单个模型的元数据。
// 注意: 别名本身已经编码了 provider 和实际模型名 (格式 "provider:modelName")，
// 因此 ModelConfig 不再包含 Alias 和 ProviderName 字段。
type ModelConfig struct {
	MaxTokens       int    `json:"maxTokens,omitempty"`       // 默认 max_tokens
	MaxTurns        int    `json:"maxTurns,omitempty"`        // 默认 max_turns
	PromptCacheMode string `json:"promptCacheMode,omitempty"` // "auto" / "on" / "off"
	// Protocol 出站协议: "anthropic"(默认, /messages)、"openai"(/chat/completions)
	// 或 "openai-responses"(/v1/responses)。模型级配置 —— 如 opencode:mimo-v2.5 的
	// tool_use 必须走 OpenAI 协议 (opencode Anthropic→OpenAI 转换层丢 name 字段返回 400),
	// opencode:muse-spark-1.2-contributor 走 OpenAI Responses API, 其余 opencode 模型走 Anthropic。
	Protocol      string `json:"protocol,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"` // 上下文窗口长度
	// ReasoningPlayback 出站回传上一轮推理的字段名 —— 只对 protocol:"openai" 有意义。
	//
	//    ""                    = 不回传 (默认, 与改造前逐字节相同)
	//    "reasoning"           = 写 messages[].reasoning          (opencode step-5 等)
	//    "reasoning_content"   = 写 messages[].reasoning_content  (DeepSeek/R1 系)
	//
	// 为什么必须是模型级开关而不是全局打开: 各家对"请求里带回传推理"的接受度
	// 相反。DeepSeek 官方文档明确**拒绝**在请求中回传 reasoning_content, 传了直接
	// 报错; 而 opencode:step-5-preview-free 实测接受。全局打开会让默认兜底模型
	// (deepseek-v4.1-flash 就在同一个 provider 列表里) 整批 400 —— 这正是"修一个
	// 模型而打死另一个模型"的典型形状, 所以闸门放在模型上, 由配置逐台决定。
	//
	// 现状: 上一轮推理此前**已被网关收到并存进本地 messages[]** (runner.go 把带
	// thinking 块的 assistant 消息 append 进历史), 只是翻译成 OpenAI 格式时被丢弃
	// (openai.go assistantBlocksToOpenAI 的 case ContentBlockThinking)。这个开关
	// 只是让那段已经存在的数据继续往下游走, 不产生新的推理。
	ReasoningPlayback string `json:"reasoningPlayback,omitempty"`
	// Proxy 出站 HTTP 代理 URL (http://host:port)。模型级配置 —— 只有配置了 proxy 的
	// 模型才走代理 (如 muse-spark 大陆不可达, 须经东京 tailnet 代理出口), 其余模型
	// 保持直连, 不消耗代理出口流量额度。
	Proxy string `json:"proxy,omitempty"`
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
	// DefaultMaxTurns / DefaultMaxTokens 全局兜底上限 (0=不限)。
	// 当模型/plan/role 未显式指定时生效; 需要更多回合/输出的场景可在 model 级单独调大覆盖。
	DefaultMaxTurns  int `json:"defaultMaxTurns,omitempty"`
	DefaultMaxTokens int `json:"defaultMaxTokens,omitempty"`

	// LongHorizonMaxTurns 长程任务档的回合上限 (H4-3, 见 taskclass.go)。
	//
	// 与 DefaultMaxTurns 是**两个不同的量**: 后者是常规任务的止损线, 前者是
	// 产码/测试/修复这类没有自然下界的任务的止损线。用一个数兼顾两者必然顾此失彼
	// (调大则冒烟任务跑飞, 调小则长程任务跑不完)。
	//
	// 0 = **未开启分层**, 长程档回落到 DefaultMaxTurns —— 与改造前逐字节相同。
	// 刻意的: 分层的默认行为必须等于改造前的行为, 否则这次改动会在没人配置的
	// 情况下改掉每一个任务的止损线。
	LongHorizonMaxTurns int `json:"longHorizonMaxTurns,omitempty"`
}

// PreferredModelName 返回应发给上游的 model 值。
//
// 优先 Alias (完整别名, 如 "opencode:deepseek-v4.1-flash") 而不是
// ProviderName (仅后半段的厂商模型名, 如 "deepseek-v4.1-flash")。
//
// 为什么: provider 前缀是路由信息的一部分 —— 网关/上游靠它决定把请求发给
// 哪个后端池。只发裸模型名时, 同一个模型名在不同 provider 下会撞车
// (deepseek:deepseek-v4.1-flash 与 opencode:deepseek-v4.1-flash 的裸名完全
// 相同), 请求会被路由到默认池而非配置指定的池; 上游按模型名做的提示缓存
// 分片键也跟着变, 跨轮命中率随之抖动。
//
// ProviderName 的语义 (厂商实际模型名) 保留不动 —— metrics / dashboard 的
// LookupAliasByModelName 反查等 20+ 处消费点依赖它。这里只改"出站发什么"。
func (c ResolvedConfig) PreferredModelName() string {
	if c.Alias != "" {
		return c.Alias
	}
	return c.ProviderName
}

// OutboundModelName 返回**出站请求**该带的 model 值 —— 与请求实际发往哪里有关:
//
//	经网关 (ViaGateway) → 完整别名。别名是网关的查找键, 网关据它选出 provider 的
//	端点/凭据/协议, 再把 model 剥成裸名转发上游 (见 cmd/anthropic-gateway 的
//	proxyPassthrough)。发裸名也能解析, 但发别名让网关日志与指标里的 model 字段
//	带得上 provider, 归因不丢。
//
//	直连 provider → 裸名。providers 的 baseUrl 已经指向具体厂商, 厂商端点只认
//	自己的模型 ID: 把 "opencode:deepseek-v4.1-flash" 直发给 opencode 会被拒
//	(401 ModelError), kimi 同理; 本机 Ollama 的 /v1/messages 也会 400
//	invalid model name。
//
// 所以"发什么"不能是配置层的静态属性 —— 它是**这条链路形态**的函数。用错方向的
// 代价是不对称的: 直连发别名是硬失败 (请求被拒), 经网关发裸名只是少了个标签。
// 因此判据取肯定性证据 (ViaGateway / LocalEndpoint), 而不是"看着像网关"的猜测。
//
// **判据顺序要紧, 两条都不能省。** 本机端点那条是踩过的坑: 它此前按"非网关即裸名"
// 的推论**隐式**成立, 只因为当时没有任何一处覆盖过客户端上的 Model 值; 一旦改为
// 显式赋值 (或将来让网关把 BaseURL 改指一个本机代理 —— 那时两条同时为真), 推论就
// 断了, 请求会带着 "ollama:xxx" 打到一个只认裸名的端点。显式判据写出来之后, 与
// ViaGateway 谁先谁后也是一句话说得清的事, 而不再是某处赋值顺序的副作用。
func (c ResolvedConfig) OutboundModelName() string {
	if c.ViaGateway {
		return c.PreferredModelName()
	}
	if c.LocalEndpoint {
		return c.ProviderName
	}
	return c.ProviderName
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
	Protocol        string // "anthropic"(默认) | "openai" | "openai-responses"; 见 ModelConfig.Protocol
	// ReasoningPlayback 出站回传推理用的 OpenAI 字段名; 见 ModelConfig.ReasoningPlayback。
	// "" = 不回传。指向 api.Client.ReasoningPlayback。
	ReasoningPlayback string
	ContextWindow     int
	Proxy             string // 出站 HTTP 代理 URL, 仅配置了 proxy 的模型走代理 (见 ModelConfig.Proxy)
	// 模型专属限流参数 (0 = 使用全局默认值)
	RPM         int
	MaxParallel int
	MinParallel int
	// 超时与退避参数 (0 = 使用全局默认值)
	FirstTokenTimeoutSec int // 首token等待时间(秒)
	CallTimeoutSec       int // 单次agent调用总时间(秒)
	DeadlineRetryBaseSec int // deadline exceeded退避基数(秒)

	// ViaGateway 表示 BaseURL 已被 ApplyGatewayOverride 改指 LLM 网关
	// (即进程设了 CLAUDE_GO_LLM_GATEWAY)。它决定 OutboundModelName 发别名还是裸名。
	//
	// 做成字段而不是各调用点再读一次环境变量: 覆盖动作发生在 ApplyGatewayOverride
	// 一处, "这一跳是否经网关" 的结论就只该在那处产生 —— 下游各自 os.Getenv 会在
	// "env 设了但 BaseURL 为空没被覆盖" 这类边界上给出与事实相反的答案。
	ViaGateway bool

	// LocalEndpoint 表示 BaseURL 指向本机端点 (Ollama 等, 见 api.IsLocalEndpoint)。
	//
	// 与 ViaGateway 取同一种做法: 在解析出口把结论定下来, 下游不再各自 os/字符串比对。
	// 这里必须一致 —— OutboundModelName 是个纯函数, 而判据散在两处的话, 迟早出现
	// "网关把 BaseURL 改指本机代理 (经网关) 但某处仍按本机裸名发" 这类两面都自认正确的错。
	//
	// 为什么本机端点要发裸名: Ollama 的 /v1/messages 兼容层把 model 字段当**真实模型
	// 名**解析 (不是别名), 收到 "ollama:gemma4:..." 会直接 400 invalid model name。
	// 这与厂商端点拒前缀是同一个原因的两个实例。
	LocalEndpoint bool
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
	ProviderName string // 实际模型名（alias 后半段）
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
