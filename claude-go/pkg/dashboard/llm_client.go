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
//  3. feishu.LoadJSONConfig -> ai.{apiKey, baseUrl, model}
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

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/feishu"
)

// ErrLLMNotConfigured 表示无法从 env/配置文件中解析出可用的模型凭据。
var ErrLLMNotConfigured = errors.New("LLM 未配置: 请在 claude-go.json (ai.apiKey) 或 ANTHROPIC_API_KEY 环境变量中设置")

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
)

// GetSharedLLMClient 返回全局共享的 LLM 客户端 (带 profile)。
// 缓存 5 分钟, 之后自动重载 (容忍用户在运行中修改 claude-go.json)。
func GetSharedLLMClient() (*api.Client, LLMProfile, error) {
	llmClientMu.Lock()
	defer llmClientMu.Unlock()
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

	// 3) 复用飞书 bot 所用 claude-go.json
	//    先尝试显式 CLAUDE_GO_CONFIG, 再回退到 dashboard 所在 stateDir 的上级目录 claude-go.json,
	//    最后让 feishu.LoadJSONConfig 自己按 ./claude-go.json / ~/.claude-go/config.json 搜索。
	if apiKey == "" || model == "" {
		cfg, path, err := loadFeishuJSONConfig()
		if err == nil && cfg != nil && cfg.AI != nil {
			if apiKey == "" && cfg.AI.APIKey != "" {
				apiKey = cfg.AI.APIKey
				profile.Source = "claude-go.json:ai.apiKey"
				profile.ConfigPath = path
			}
			if baseURL == "" && cfg.AI.BaseURL != "" {
				baseURL = cfg.AI.BaseURL
			}
			if model == "" && cfg.AI.Model != "" {
				model = cfg.AI.Model
			}
		}
	}

	if apiKey == "" {
		return nil, profile, ErrLLMNotConfigured
	}
	if model == "" {
		model = "claude-haiku-4-5"
	}

	profile.Model = model
	profile.HasAPIKey = true
	profile.BaseURL = baseURL

	// 客户端选择
	var client *api.Client
	if baseURL != "" {
		trimmed := strings.TrimRight(baseURL, "/")
		if strings.HasSuffix(trimmed, "/anthropic") || strings.HasSuffix(trimmed, "/compatible-mode") {
			trimmed += "/v1"
		}
		client = api.NewClient(trimmed, apiKey, model)
		switch {
		case strings.Contains(trimmed, "anthropic.com"):
			profile.Provider = "anthropic"
		case strings.Contains(trimmed, "dashscope"):
			profile.Provider = "dashscope"
		default:
			profile.Provider = "compatible"
		}
	} else {
		client = api.NewDashScopeClient(apiKey, model)
		profile.Provider = "dashscope"
		if profile.Source == "" {
			profile.Source = "dashscope-default"
		}
	}
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
		return "", profile, fmt.Errorf("llm complete: %w", err)
	}
	return strings.TrimSpace(out), profile, nil
}
