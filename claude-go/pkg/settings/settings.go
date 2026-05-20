package settings

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/anthropic/claude-go/pkg/feishu"
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

	// CodeIntel 段 (代码智能配置)
	CodeIntel *CodeIntelSettings `json:"codeIntel,omitempty"`
}

// AISection AI 模型配置段 (与飞书 claude-go.json 格式统一)。
type AISection struct {
	Model     string                          `json:"model,omitempty"`
	APIKey    string                          `json:"apiKey,omitempty"`
	BaseURL   string                          `json:"baseUrl,omitempty"`
	MaxTokens int                             `json:"maxTokens,omitempty"`
	Plans     map[string]feishu.PlanModelConfig `json:"plans,omitempty"`
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

// CodeIntelSettings 代码智能配置。
type CodeIntelSettings struct {
	// 全局索引根目录（默认 ~/.claude-code-intel）
	GlobalIndexRoot string `json:"globalIndexRoot,omitempty"`

	// 缓存配置
	Cache *CodeIntelCacheSettings `json:"cache,omitempty"`

	// MCP Server 配置
	MCP *CodeIntelMCPSettings `json:"mcp,omitempty"`

	// Metrics 配置
	Metrics *CodeIntelMetricsSettings `json:"metrics,omitempty"`

	// 自动更新配置
	AutoUpdate *CodeIntelAutoUpdateSettings `json:"autoUpdate,omitempty"`

	// 工具路径覆盖（优先于自动探测）
	ToolPaths *CodeIntelToolPaths `json:"toolPaths,omitempty"`
}

// CodeIntelCacheSettings 缓存配置。
type CodeIntelCacheSettings struct {
	MaxNodes     int `json:"maxNodes,omitempty"`     // 节点缓存容量（默认 10000）
	MaxEdges     int `json:"maxEdges,omitempty"`     // 边列表缓存容量（默认 50000）
	QueryTTLSec  int `json:"queryTTLSec,omitempty"`  // 查询结果缓存 TTL（默认 60）
}

// CodeIntelMCPSettings MCP Server 配置。
type CodeIntelMCPSettings struct {
	Enabled   bool     `json:"enabled,omitempty"`   // 是否启用 MCP Server
	RepoPath  string   `json:"repoPath,omitempty"`  // 默认仓库路径
	Transport string   `json:"transport,omitempty"` // "stdio" | "http" | "sse"
}

// CodeIntelMetricsSettings Metrics 配置。
type CodeIntelMetricsSettings struct {
	Enabled    bool   `json:"enabled,omitempty"`    // 是否启用指标采集
	OutputPath string `json:"outputPath,omitempty"` // 指标输出路径（默认 .claude-code-intel/metrics.json）
	FlushIntervalSec int `json:"flushIntervalSec,omitempty"` // 刷盘间隔（默认 300）
}

// CodeIntelAutoUpdateSettings 自动更新配置。
type CodeIntelAutoUpdateSettings struct {
	Enabled       bool `json:"enabled,omitempty"`       // 是否启用自动更新
	IntervalSec   int  `json:"intervalSec,omitempty"`   // 检测间隔（默认 300 = 5分钟）
	FileThreshold int  `json:"fileThreshold,omitempty"` // 触发重建的文件变更阈值（默认 5）
	QuietHours    *CodeIntelQuietHours `json:"quietHours,omitempty"` // 静默时段
}

// CodeIntelQuietHours 静默时段配置。
type CodeIntelQuietHours struct {
	Start string `json:"start,omitempty"` // "HH:MM" 格式
	End   string `json:"end,omitempty"`   // "HH:MM" 格式
}

// CodeIntelToolPaths 工具路径覆盖。
type CodeIntelToolPaths struct {
	NodePath     string `json:"nodePath,omitempty"`     // Node.js 路径
	GitNexusPath string `json:"gitnexusPath,omitempty"` // gitnexus CLI 路径
	GraphifyPath string `json:"graphifyPath,omitempty"` // graphify CLI 路径
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
		if len(src.AI.Plans) > 0 {
			if dst.AI.Plans == nil {
				dst.AI.Plans = make(map[string]feishu.PlanModelConfig, len(src.AI.Plans))
			}
			for k, v := range src.AI.Plans {
				dst.AI.Plans[k] = v
			}
		}
	}

	// 合并 CodeIntel 配置
	if src.CodeIntel != nil {
		if dst.CodeIntel == nil {
			dst.CodeIntel = &CodeIntelSettings{}
		}
		mergeCodeIntelSettings(dst.CodeIntel, src.CodeIntel)
	}
}

func mergeCodeIntelSettings(dst, src *CodeIntelSettings) {
	if src.GlobalIndexRoot != "" {
		dst.GlobalIndexRoot = src.GlobalIndexRoot
	}
	if src.Cache != nil {
		if dst.Cache == nil {
			dst.Cache = &CodeIntelCacheSettings{}
		}
		if src.Cache.MaxNodes > 0 {
			dst.Cache.MaxNodes = src.Cache.MaxNodes
		}
		if src.Cache.MaxEdges > 0 {
			dst.Cache.MaxEdges = src.Cache.MaxEdges
		}
		if src.Cache.QueryTTLSec > 0 {
			dst.Cache.QueryTTLSec = src.Cache.QueryTTLSec
		}
	}
	if src.MCP != nil {
		if dst.MCP == nil {
			dst.MCP = &CodeIntelMCPSettings{}
		}
		if src.MCP.Enabled {
			dst.MCP.Enabled = true
		}
		if src.MCP.RepoPath != "" {
			dst.MCP.RepoPath = src.MCP.RepoPath
		}
		if src.MCP.Transport != "" {
			dst.MCP.Transport = src.MCP.Transport
		}
	}
	if src.Metrics != nil {
		if dst.Metrics == nil {
			dst.Metrics = &CodeIntelMetricsSettings{}
		}
		if src.Metrics.Enabled {
			dst.Metrics.Enabled = true
		}
		if src.Metrics.OutputPath != "" {
			dst.Metrics.OutputPath = src.Metrics.OutputPath
		}
		if src.Metrics.FlushIntervalSec > 0 {
			dst.Metrics.FlushIntervalSec = src.Metrics.FlushIntervalSec
		}
	}
	if src.AutoUpdate != nil {
		if dst.AutoUpdate == nil {
			dst.AutoUpdate = &CodeIntelAutoUpdateSettings{}
		}
		if src.AutoUpdate.Enabled {
			dst.AutoUpdate.Enabled = true
		}
		if src.AutoUpdate.IntervalSec > 0 {
			dst.AutoUpdate.IntervalSec = src.AutoUpdate.IntervalSec
		}
		if src.AutoUpdate.FileThreshold > 0 {
			dst.AutoUpdate.FileThreshold = src.AutoUpdate.FileThreshold
		}
		if src.AutoUpdate.QuietHours != nil {
			dst.AutoUpdate.QuietHours = src.AutoUpdate.QuietHours
		}
	}
	if src.ToolPaths != nil {
		if dst.ToolPaths == nil {
			dst.ToolPaths = &CodeIntelToolPaths{}
		}
		if src.ToolPaths.NodePath != "" {
			dst.ToolPaths.NodePath = src.ToolPaths.NodePath
		}
		if src.ToolPaths.GitNexusPath != "" {
			dst.ToolPaths.GitNexusPath = src.ToolPaths.GitNexusPath
		}
		if src.ToolPaths.GraphifyPath != "" {
			dst.ToolPaths.GraphifyPath = src.ToolPaths.GraphifyPath
		}
	}
}

// ApplyEnv 将 settings 中的 env 变量设到进程环境中。
func (s *Settings) ApplyEnv() {
	for k, v := range s.Env {
		os.Setenv(k, v)
	}
}
