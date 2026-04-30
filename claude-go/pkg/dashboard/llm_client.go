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
}

var (
	llmOnce       sync.Once
	llmClient     *api.Client
	llmProfile    LLMProfile
	llmInitErr    error
	llmClientMu   sync.Mutex // 保护 Reload 场景
	llmCacheUntil time.Time
	llmInjected   bool // 标记是否由外部注入 (如飞书 bot)
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

// ResetSharedLLMClient 强制下次调用时重新加载 (单元测试或配置热更新)。
func ResetSharedLLMClient() {
	llmClientMu.Lock()
	defer llmClientMu.Unlock()
	llmClient = nil
	llmInjected = false
	llmCacheUntil = time.Time{}
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
			model = "moonshot-v1-auto"
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

// LLMComplete 封装一次 "system + user" 的对话 (非流式), 自动使用共享 client。
// 供 dashboard 的各个诊断 handler 使用。
//
// 错误信息里携带 profile.Provider + profile.Model 的原因:
// 一旦出现 "model X is not supported" / "insufficient quota" 一类的错误,
// 用户能直接从结果页看出发生在哪个 provider + 哪个模型, 不用再翻日志。
func LLMComplete(ctx context.Context, system, user string, timeout time.Duration) (string, LLMProfile, error) {
	client, profile, err := GetSharedLLMClient()
	if err != nil {
		return "", profile, err
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := client.SimpleComplete(cctx, system, user)
	if err != nil {
		return "", profile, fmt.Errorf("llm complete (provider=%s, model=%s): %w",
			profile.Provider, profile.Model, err)
	}
	return strings.TrimSpace(out), profile, nil
}
