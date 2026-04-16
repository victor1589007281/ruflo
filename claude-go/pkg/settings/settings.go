package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings 项目配置。
// 兼容两种格式:
//  1. .claude-go/settings.json (纯 settings)
//  2. claude-go.json (飞书统一配置, 含 ai/feishu 段)
type Settings struct {
	Permissions  PermissionSettings       `json:"permissions,omitempty"`
	Env          map[string]string        `json:"env,omitempty"`
	Model        string                   `json:"model,omitempty"`
	McpServers   map[string]McpConfig     `json:"mcpServers,omitempty"`
	Hooks        HookSettings             `json:"hooks,omitempty"`
	SystemPrompt string                   `json:"systemPrompt,omitempty"`
	MaxTokens    int                      `json:"maxTokens,omitempty"`
	MaxTurns     int                      `json:"maxTurns,omitempty"`

	// AI 段 (兼容 claude-go.json 飞书统一配置格式)
	AI *AISection `json:"ai,omitempty"`
}

// AISection AI 模型配置段 (与飞书 claude-go.json 格式统一)。
type AISection struct {
	Model     string `json:"model,omitempty"`
	APIKey    string `json:"apiKey,omitempty"`
	BaseURL   string `json:"baseUrl,omitempty"`
	MaxTokens int    `json:"maxTokens,omitempty"`
}

// PermissionSettings 权限配置。
type PermissionSettings struct {
	Allow       []PermissionRule `json:"allow,omitempty"`
	Deny        []PermissionRule `json:"deny,omitempty"`
	DefaultMode string           `json:"defaultMode,omitempty"`
}

// PermissionRule 权限规则。
type PermissionRule struct {
	Tool    string `json:"tool,omitempty"`
	Pattern string `json:"pattern,omitempty"`
}

// McpConfig MCP 服务器配置。
type McpConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"`
}

// HookSettings hook 配置。
type HookSettings struct {
	PreToolUse  []HookEntry `json:"preToolUse,omitempty"`
	PostToolUse []HookEntry `json:"postToolUse,omitempty"`
}

// HookEntry 单条 hook。
type HookEntry struct {
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
}

// Load 从指定路径加载 settings.json。
func Load(path string) (*Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadProjectSettings 按优先级加载并合并设置。
// 加载顺序 (低优先级 → 高优先级):
//  1. ~/.claude-go/settings.json         (用户级)
//  2. .claude-go/settings.json           (项目级)
//  3. .claude-go/settings.local.json     (本地级, gitignored)
//  4. claude-go.json                     (飞书统一配置, 兼容)
func LoadProjectSettings(projectDir string) *Settings {
	merged := &Settings{
		Env: make(map[string]string),
	}

	homeDir, _ := os.UserHomeDir()
	paths := []string{
		filepath.Join(homeDir, ".claude-go", "settings.json"),
		filepath.Join(projectDir, ".claude-go", "settings.json"),
		filepath.Join(projectDir, ".claude-go", "settings.local.json"),
		filepath.Join(projectDir, "claude-go.json"),
	}

	for _, p := range paths {
		s, err := Load(p)
		if err != nil {
			continue
		}
		mergeSettings(merged, s)
	}

	return merged
}

// mergeSettings 将 src 合并到 dst (src 覆盖 dst)。
func mergeSettings(dst, src *Settings) {
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.SystemPrompt != "" {
		dst.SystemPrompt = src.SystemPrompt
	}
	if src.MaxTokens > 0 {
		dst.MaxTokens = src.MaxTokens
	}
	if src.MaxTurns > 0 {
		dst.MaxTurns = src.MaxTurns
	}

	for k, v := range src.Env {
		dst.Env[k] = v
	}

	if src.Permissions.DefaultMode != "" {
		dst.Permissions.DefaultMode = src.Permissions.DefaultMode
	}
	dst.Permissions.Allow = append(dst.Permissions.Allow, src.Permissions.Allow...)
	dst.Permissions.Deny = append(dst.Permissions.Deny, src.Permissions.Deny...)

	if len(src.McpServers) > 0 {
		if dst.McpServers == nil {
			dst.McpServers = make(map[string]McpConfig)
		}
		for k, v := range src.McpServers {
			dst.McpServers[k] = v
		}
	}

	dst.Hooks.PreToolUse = append(dst.Hooks.PreToolUse, src.Hooks.PreToolUse...)
	dst.Hooks.PostToolUse = append(dst.Hooks.PostToolUse, src.Hooks.PostToolUse...)

	if src.AI != nil {
		if src.AI.Model != "" && dst.Model == "" {
			dst.Model = src.AI.Model
		}
		if src.AI.MaxTokens > 0 && dst.MaxTokens == 0 {
			dst.MaxTokens = src.AI.MaxTokens
		}
		if dst.AI == nil {
			dst.AI = &AISection{}
		}
		if src.AI.APIKey != "" {
			dst.AI.APIKey = src.AI.APIKey
		}
		if src.AI.BaseURL != "" {
			dst.AI.BaseURL = src.AI.BaseURL
		}
		if src.AI.Model != "" {
			dst.AI.Model = src.AI.Model
		}
	}
}

// ApplyEnv 将 settings 中的 env 变量设到进程环境中。
func (s *Settings) ApplyEnv() {
	for k, v := range s.Env {
		os.Setenv(k, v)
	}
}
