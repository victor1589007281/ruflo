package dashboard

// 统一的 LLM 客户端工厂。
//
// 目标: dashboard 里所有需要调用模型的场景 (insights LLM 诊断、
// dreaming 评估、evolution 评估、未来的自动修复等) 共享同一个
// *api.Client, 且优先复用飞书机器人/主进程所用的配置 (claude-go.json 中
// 的 "ai" 段), 避免每个功能各自维护 ANTHROPIC_API_KEY/BASE_URL 的散列。
//
// 加载优先级 (高 → 低):
//  1. 环境变量 DASHBOARD_LLM_API_KEY / _BASE_URL / _MODEL (dashboard 独占覆盖)
//  2. 环境变量 ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN + ANTHROPIC_BASE_URL + DASHBOARD_LLM_MODEL
//  3. feishu.LoadJSONConfig -> providers + alias 解析
//  4. 若上述均缺失, 返回 ErrLLMNotConfigured
//
// 客户端选择:
//  - 若 baseURL 非空: api.NewClient(baseURL, key, model)
//  - 否则: api.NewDashScopeClient(key, model)  (DashScope 默认)
//
// L1 层归属 (design/02 §3.1): 本文件是 dashboard 侧唯一的 LLM 出口, 因此也是
// LLMGateway 接口的接线点 —— 见 SharedGateway 与 LLMComplete。上面第 1/2 条
// env 直通路径此前不经 ConfigResolver, 于是 CLAUDE_GO_LLM_GATEWAY 对它们无效
// (设了网关, dashboard 的诊断调用照旧直连 provider); resolveLLMClient 末尾现在
// 显式复用 modelconfig.ApplyGatewayOverride 补上这一跳。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/llmgw"
)

// ErrLLMNotConfigured 表示无法从 env/配置文件中解析出可用的模型凭据。
var ErrLLMNotConfigured = errors.New("LLM 未配置: 请在 claude-go.json providers 中设置 apiKey 和 alias，或设置 ANTHROPIC_API_KEY 环境变量")

// LLMProfile 代表一次成功解析出的模型端点, 用于 /api/llm/status 展示。
type LLMProfile struct {
	Source    string `json:"source"`    // env | claude-go.json | dashscope-default
	Provider  string `json:"provider"`  // anthropic | dashscope | compatible
	BaseURL   string `json:"baseUrl,omitempty"`
	Model     string `json:"model"`
	HasAPIKey bool   `json:"hasApiKey"`
	// ConfigPath 若使用的是文件配置, 这里记录实际命中的文件路径。
	ConfigPath string `json:"configPath,omitempty"`
	// Gateway 非空表示出站这一跳实际打的是 L1 LLM 网关 (CLAUDE_GO_LLM_GATEWAY),
	// 而不是 BaseURL 里那个 provider 端点。新增字段, 只增不改, /api/llm/status
	// 的既有字段语义不动。
	Gateway string `json:"gateway,omitempty"`
}

var (
	llmOnce       sync.Once
	llmClient     *api.Client
	llmProfile    LLMProfile
	llmInitErr    error
	llmClientMu   sync.Mutex // 保护 Reload 场景
	llmCacheUntil time.Time
	llmInjected   bool // 标记是否由外部注入 (如飞书 bot)

	// llmGW 是 llmClient 的 LLMGateway 视图 (design/02 §3.1)。
	// llmGWFor 记录它包的是哪个 client, 客户端被重解析/重注入后随之重建。
	llmGW    llmgw.LLMGateway
	llmGWFor *api.Client
)

// SetSharedLLMClient 由外部注入已配置好的 LLM 客户端。
// 当 dashboard 跟随飞书 bot 一起启动时, bot 应调用此函数注入其 aiClient,
// 确保 dashboard 诊断使用与 bot 完全相同的模型配置, 而非自行猜测。
// 注入后 GetSharedLLMClient 将始终返回该客户端, 不再自行解析。
func SetSharedLLMClient(client *api.Client) {
	if client == nil {
		return
	}
	llmClientMu.Lock()
	defer llmClientMu.Unlock()
	llmClient = client
	llmProfile = buildProfileFromClient(client)
	llmInitErr = nil
	llmInjected = true
	// 网关视图随之作废, 下次 SharedGateway 会照新 client 重建。
	llmGW, llmGWFor = nil, nil
	llmCacheUntil = time.Now().Add(24 * time.Hour) // 注入的客户端长期有效
}

// buildProfileFromClient 从 api.Client 构建 LLMProfile 用于展示。
func buildProfileFromClient(client *api.Client) LLMProfile {
	provider := "compatible"
	if client.BaseURL != "" {
		lb := strings.ToLower(client.BaseURL)
		switch {
		case strings.Contains(lb, "anthropic.com"):
			provider = "anthropic"
		case strings.Contains(lb, "dashscope") || strings.Contains(lb, "aliyuncs.com"):
			provider = "dashscope"
		case strings.Contains(lb, "openai.com"):
			provider = "openai"
		case strings.Contains(lb, "moonshot"):
			provider = "moonshot"
		}
	} else {
		provider = "dashscope"
	}
	return LLMProfile{
		Source:   "injected:bot",
		Provider: provider,
		BaseURL:  client.BaseURL,
		Model:    client.Model,
		HasAPIKey: client.APIKey != "",
	}
}

// GetSharedLLMClient 返回全局共享的 LLM 客户端 (带 profile)。
// 若已通过 SetSharedLLMClient 注入, 直接返回注入的客户端。
// 否则缓存 5 分钟, 之后自动重载 (容忍用户在运行中修改 claude-go.json)。
func GetSharedLLMClient() (*api.Client, LLMProfile, error) {
	llmClientMu.Lock()
	defer llmClientMu.Unlock()
	if llmInjected && llmClient != nil {
		return llmClient, llmProfile, nil
	}
	if llmClient != nil && time.Now().Before(llmCacheUntil) {
		return llmClient, llmProfile, llmInitErr
	}
	llmClient, llmProfile, llmInitErr = resolveLLMClient()
	llmCacheUntil = time.Now().Add(5 * time.Minute)
	return llmClient, llmProfile, llmInitErr
}

// SharedGateway 返回共享 LLM 客户端的 LLMGateway 视图 (design/02 §3.1 L1)。
//
// 为什么它存在: design/02 要求"上层只依赖 LLMGateway 接口, 不直接触碰
// api.Client"。dashboard 的诊断/洞察/团队体检三条链路是 :18080 上真实的 LLM
// 消费方, 全部改经本函数, 接口因此有了生产调用方 (此前 llmgw.NewLocal 全仓只有
// 自己的测试在调)。
//
// 为什么不顺手把 GetSharedLLMClient 换成返回接口: 它是导出符号, /api/llm/status
// 与 /api/llm/rate 拿它读 BaseURL/Model 等具体字段, 换类型会破坏既有调用方。
// 两者共用同一份解析与缓存, 不存在第二个客户端。
func SharedGateway() (llmgw.LLMGateway, LLMProfile, error) {
	client, profile, err := GetSharedLLMClient()
	if err != nil {
		return nil, profile, err
	}
	llmClientMu.Lock()
	defer llmClientMu.Unlock()
	if llmGW == nil || llmGWFor != client {
		// 档位由 llmgw.NewFromEnv 统一决定 (默认 local), 不在这里自己读 env:
		// 两个装配点各判一次档位就会出现"飞书走 remote 而 dashboard 仍直连",
		// 而两边日志长得一模一样, 现场无法归因。
		gw, err := llmgw.NewFromEnv(client)
		if err != nil {
			// fail-closed: 档位配错时报错, 绝不退回 local 静默直连 provider。
			return nil, profile, err
		}
		llmGW, llmGWFor = gw, client
	}
	return llmGW, profile, nil
}

// ResetSharedLLMClient 强制下次调用时重新加载 (单元测试或配置热更新)。
func ResetSharedLLMClient() {
	llmClientMu.Lock()
	defer llmClientMu.Unlock()
	llmClient = nil
	llmInjected = false
	llmCacheUntil = time.Time{}
	llmGW, llmGWFor = nil, nil
}

// resolveLLMClient 是无缓存的解析实现。
func resolveLLMClient() (*api.Client, LLMProfile, error) {
	profile := LLMProfile{}
	apiKey, baseURL, model := "", "", ""

	// 1) dashboard 独占覆盖
	if v := os.Getenv("DASHBOARD_LLM_API_KEY"); v != "" {
		apiKey = v
		profile.Source = "env:DASHBOARD_LLM_*"
	}
	if v := os.Getenv("DASHBOARD_LLM_BASE_URL"); v != "" {
		baseURL = v
	}
	if v := os.Getenv("DASHBOARD_LLM_MODEL"); v != "" {
		model = v
	}

	// 2) Anthropic 标准环境变量
	if apiKey == "" {
		apiKey = firstNonEmpty(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("ANTHROPIC_AUTH_TOKEN"))
		if apiKey != "" && profile.Source == "" {
			profile.Source = "env:ANTHROPIC_*"
		}
	}
	if baseURL == "" {
		baseURL = os.Getenv("ANTHROPIC_BASE_URL")
	}

	// 3) 复用飞书 bot 所用 claude-go.json (providers + alias 新模式)
	//    先尝试显式 CLAUDE_GO_CONFIG, 再回退到 dashboard 所在 stateDir 的上级目录 claude-go.json,
	//    最后让 feishu.LoadJSONConfig 自己按 ./claude-go.json / ~/.claude-go/config.json 搜索。
	if apiKey == "" || baseURL == "" || model == "" {
		cfg, path, err := loadFeishuJSONConfig()
		if err == nil && cfg != nil && len(cfg.Providers) > 0 {
			registry, resolver, err := modelconfig.LoadFromConfig(cfg.ToModelConfigJSON())
			if err == nil && resolver != nil {
				if errs := modelconfig.Validate(registry, resolver); len(errs) == 0 {
					resolved := resolver.Resolve("", "")
					if apiKey == "" && resolved.APIKey != "" {
						apiKey = resolved.APIKey
						profile.Source = "claude-go.json:providers"
						profile.ConfigPath = path
					}
					if baseURL == "" && resolved.BaseURL != "" {
						baseURL = resolved.BaseURL
					}
					if model == "" && resolved.ProviderName != "" {
						model = resolved.ProviderName
					}
				}
			}
		}
	}

	if apiKey == "" {
		return nil, profile, ErrLLMNotConfigured
	}

	// ---------------------------------------------------------------
	// Provider-aware model default.
	//
	// 以前 bug: 当 claude-go.json 只配 ai.apiKey + ai.baseUrl 而没有 ai.model,
	// 会固定 fallback 到 "claude-haiku-4-5", 直接打到 DashScope/Qwen 网关会得到
	// 400 invalid_parameter_error "model claude-haiku-4-5 is not supported".
	//
	// 现在根据 baseURL 推断 provider, 选择该 provider 实际支持的模型:
	//   anthropic.com                        -> claude-haiku-4-5
	//   dashscope (aliyuncs.com / dashscope) -> qwen3.5-plus  (与 feishu bot 默认一致)
	//   其他 OpenAI 兼容                      -> 若 cfg 仍未给, 退回 qwen3.5-plus (最广泛可用)
	//   空 baseURL (走 DashScope SDK)         -> qwen3.5-plus
	// ---------------------------------------------------------------
	provider := "compatible"
	if baseURL != "" {
		lb := strings.ToLower(baseURL)
		switch {
		case strings.Contains(lb, "anthropic.com"):
			provider = "anthropic"
		case strings.Contains(lb, "dashscope") || strings.Contains(lb, "aliyuncs.com"):
			provider = "dashscope"
		case strings.Contains(lb, "openai.com"):
			provider = "openai"
		case strings.Contains(lb, "moonshot"):
			provider = "moonshot"
		default:
			provider = "compatible"
		}
	} else {
		provider = "dashscope"
	}
	if model == "" {
		switch provider {
		case "anthropic":
			model = "claude-haiku-4-5"
		case "openai":
			model = "gpt-4o-mini"
		case "moonshot":
			// Kimi K2 系列 (Anthropic 兼容端点)，取代已过时的 moonshot-v1-auto
			model = "kimi-k2.6"
		default:
			// dashscope / compatible: qwen3.5-plus 与飞书 bot 默认一致
			model = "qwen3.5-plus"
		}
	}

	profile.Model = model
	profile.Provider = provider
	profile.HasAPIKey = true
	profile.BaseURL = baseURL

	// 客户端构造
	var client *api.Client
	if baseURL != "" {
		trimmed := strings.TrimRight(baseURL, "/")
		// 常见 DashScope 端点尾部修正
		if strings.HasSuffix(trimmed, "/anthropic") || strings.HasSuffix(trimmed, "/compatible-mode") {
			trimmed += "/v1"
		}
		// L1 网关最后一跳 (design/02 §3.1)。顺序很重要: 先做 provider 端点的尾部
		// 修正, 再整体改指网关 —— 反过来会把 /v1 补到网关地址上。
		// 复用 modelconfig.ApplyGatewayOverride 而非自己读 env: 覆盖规则只有一份,
		// 上面第 3 条 (ConfigResolver) 路径已被它覆盖过, 这里对已是网关地址的输入
		// 是幂等的。
		if ov := modelconfig.ApplyGatewayOverride(modelconfig.ResolvedConfig{BaseURL: trimmed}); ov.BaseURL != "" {
			trimmed = ov.BaseURL
		}
		if gw := modelconfig.GatewayBaseURL(); gw != "" && trimmed == gw {
			profile.Gateway = gw
		}
		client = api.NewClient(trimmed, apiKey, model)
	} else {
		client = api.NewDashScopeClient(apiKey, model)
		if profile.Source == "" {
			profile.Source = "dashscope-default"
		}
	}
	// 打上业务标签: dashboard 自身发起的诊断/总结调用 source=dashboard
	client.Tag = "dashboard"
	return client, profile, nil
}

// loadFeishuJSONConfig 尝试多级回退找到 claude-go.json 并解析。
// 返回 (cfg, path, err)。path 为实际加载的文件绝对路径 (便于 /api/llm/status 诊断)。
func loadFeishuJSONConfig() (*feishu.JSONConfig, string, error) {
	// 1) 显式 env
	if p := os.Getenv("CLAUDE_GO_CONFIG"); p != "" {
		cfg, err := feishu.LoadJSONConfig(p)
		return cfg, p, err
	}
	// 2) dashboard stateDir 的父目录下找 claude-go.json
	//    StateDir 通常是 .../<repo>/.claude-go
	if wd, err := os.Getwd(); err == nil {
		for _, p := range []string{
			filepath.Join(wd, "claude-go.json"),
			filepath.Join(wd, "config", "claude-go.json"),
			filepath.Join(wd, "..", "claude-go.json"),
		} {
			if _, err := os.Stat(p); err == nil {
				cfg, err := feishu.LoadJSONConfig(p)
				return cfg, p, err
			}
		}
	}
	// 3) feishu.LoadJSONConfig 内部会继续搜索 ~/.claude-go/config.json
	cfg, err := feishu.LoadJSONConfig("")
	return cfg, "", err
}

// LLMComplete 封装一次 "system + user" 的对话 (非流式), 经 L1 网关接口发出。
// 供 dashboard 的各个诊断 handler 使用。
//
// 错误信息里携带 profile.Provider + profile.Model 的原因:
// 一旦出现 "model X is not supported" / "insufficient quota" 一类的错误,
// 用户能直接从结果页看出发生在哪个 provider + 哪个模型, 不用再翻日志。
func LLMComplete(ctx context.Context, system, user string, timeout time.Duration) (string, LLMProfile, error) {
	gw, profile, err := SharedGateway()
	if err != nil {
		return "", profile, err
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := gw.Simple(cctx, system, user)
	if err != nil {
		return "", profile, fmt.Errorf("llm complete (provider=%s, model=%s): %w",
			profile.Provider, profile.Model, err)
	}
	return strings.TrimSpace(out), profile, nil
}
