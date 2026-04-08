// Package feishu 实现飞书(Feishu/Lark)长连接集成。
// 通过飞书开放平台的 WebSocket 长连接接收消息事件，
// 桥接到 claude-go 的 QueryEngine 进行处理，
// 然后将结果通过飞书 IM API 回复给用户。
//
// 架构:
//
//	┌───────────┐  WebSocket  ┌──────────────┐
//	│ 飞书服务器  │◄───────────►│ claude-go    │
//	│ (事件推送)  │  长连接      │ feishu daemon│
//	└───────────┘             └──────┬───────┘
//	                                 │
//	                          ┌──────▼──────┐
//	                          │SessionManager│
//	                          │(per chat_id) │
//	                          └──────┬──────┘
//	                                 │
//	                          ┌──────▼──────┐
//	                          │ QueryEngine  │
//	                          │ (ReAct Loop) │
//	                          └─────────────┘
//
// 关键约束:
//   - 飞书要求事件回调在 3 秒内返回，否则触发超时重推
//   - 因此 AI 处理必须异步执行: 收到事件 → 立即 goroutine → 返回 nil
//   - 每个 chat_id 维护独立的 QueryEngine 会话
//   - 支持群聊(@机器人)和私聊两种模式
package feishu

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// BotConfig 飞书机器人配置
type BotConfig struct {
	// AppID 飞书应用 App ID (必填)
	// 在飞书开发者后台 > 应用凭证中获取
	AppID string

	// AppSecret 飞书应用 App Secret (必填)
	AppSecret string

	// Domain 飞书API域名
	// "feishu" → https://open.feishu.cn (国内飞书)
	// "lark"   → https://open.larksuite.com (国际 Lark)
	// 默认为 "feishu"
	Domain string

	// Model AI 模型名称
	Model string

	// APIKey AI API Key
	APIKey string

	// BaseURL AI API Base URL
	BaseURL string

	// Cwd 工作目录 (工具执行的根目录)
	Cwd string

	// MaxTokens 最大输出 token 数
	MaxTokens int

	// MaxTurns queryLoop 最大迭代次数
	MaxTurns int

	// SystemPrompt 自定义系统提示词
	SystemPrompt string

	// SessionTimeout 会话超时时间 (超过此时间未活动的会话被清理)
	// 默认 30 分钟
	SessionTimeout time.Duration

	// MaxSessions 最大并发会话数
	// 默认 100
	MaxSessions int

	// Debug 调试模式
	Debug bool

	// PermissionMode 权限模式
	PermissionMode string

	// MentionOnly 群聊中是否仅响应 @机器人 的消息
	// 默认 true
	MentionOnly bool

	// WelcomeMessage 首次对话的欢迎消息
	WelcomeMessage string

	// ThinkingMessage 处理中的提示消息
	ThinkingMessage string

	// MCPConfigPath MCP 配置文件路径 (mcpServers JSON 格式)
	MCPConfigPath string

	// MCPServers MCP 服务器配置 (从 JSON config 加载)
	MCPServers map[string]MCPServerEntry

	// Hooks Hook 配置 (从 JSON config 加载)
	Hooks []HookEntry

	// SkillDirs 额外的 Skills 目录 (除默认的 .claude/skills/)
	SkillDirs []string

	// DreamEnabled 是否启用 Dreaming 记忆整理
	DreamEnabled bool

	// DreamMinHours 距上次整理的最小间隔 (小时, 默认 24)
	DreamMinHours int

	// DreamMinSessions 触发整理所需的最小会话数 (默认 5)
	DreamMinSessions int
}

// DefaultBotConfig 返回默认配置
func DefaultBotConfig() *BotConfig {
	return &BotConfig{
		Domain:          "feishu",
		Model:           "qwen3.5-plus",
		MaxTokens:       16384,
		SessionTimeout:  30 * time.Minute,
		MaxSessions:     100,
		PermissionMode:  "bypass",
		MentionOnly:     true,
		WelcomeMessage:  "你好！我是 Claude Code (Go) 机器人。发送消息与我对话，我可以帮你编程、分析代码、执行命令等。",
		ThinkingMessage: "正在思考中...",
	}
}

// FeishuTextContent 飞书文本消息内容格式
type FeishuTextContent struct {
	Text string `json:"text"`
}

// FeishuCardContent 飞书卡片消息内容格式 (用于富文本回复)
type FeishuCardContent struct {
	Config   *CardConfig    `json:"config,omitempty"`
	Header   *CardHeader    `json:"header,omitempty"`
	Elements []CardElement  `json:"elements"`
}

// CardConfig 卡片配置
type CardConfig struct {
	WideScreenMode bool `json:"wide_screen_mode"`
}

// CardHeader 卡片标题
type CardHeader struct {
	Title    *CardText `json:"title"`
	Template string    `json:"template,omitempty"`
}

// CardText 卡片文本
type CardText struct {
	Content string `json:"content"`
	Tag     string `json:"tag"`
}

// CardElement 卡片元素
type CardElement struct {
	Tag     string    `json:"tag"`
	Content string    `json:"content,omitempty"`
	Text    *CardText `json:"text,omitempty"`
}

// ============================================================================
// JSON 配置文件支持
// 对应 TS: .claude/settings.json 中的配置结构
// ============================================================================

// JSONConfig JSON 配置文件的完整结构。
// 支持通过 --config 参数或 CLAUDE_GO_CONFIG 环境变量指定。
//
// 示例配置文件:
//
//	{
//	  "feishu": {
//	    "appId": "cli_xxx",
//	    "appSecret": "xxx",
//	    "domain": "feishu",
//	    "mentionOnly": true,
//	    "sessionTimeout": 30,
//	    "maxSessions": 100,
//	    "welcomeMessage": "你好！",
//	    "thinkingMessage": "思考中..."
//	  },
//	  "ai": {
//	    "model": "qwen3.5-plus",
//	    "apiKey": "sk-xxx",
//	    "baseUrl": "https://...",
//	    "maxTokens": 16384,
//	    "maxTurns": 0
//	  },
//	  "mcpServers": {
//	    "my-server": {
//	      "command": "npx",
//	      "args": ["-y", "some-mcp-server"],
//	      "env": {"KEY": "VALUE"}
//	    }
//	  },
//	  "hooks": [
//	    {"event": "PreToolUse", "command": "echo pre"}
//	  ],
//	  "systemPrompt": "你是一个编程助手",
//	  "permissionMode": "bypass",
//	  "cwd": "/path/to/work",
//	  "debug": false
//	}
type JSONConfig struct {
	// Feishu 飞书应用配置
	Feishu *FeishuSection `json:"feishu,omitempty"`

	// AI AI 模型配置
	AI *AISection `json:"ai,omitempty"`

	// MCPServers MCP 服务器配置 (key=名称, value=服务器配置)
	// 对应 TS: .claude/settings.json 中的 mcpServers
	MCPServers map[string]MCPServerEntry `json:"mcpServers,omitempty"`

	// Hooks Hook 配置列表
	Hooks []HookEntry `json:"hooks,omitempty"`

	// SystemPrompt 自定义系统提示词
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// PermissionMode 权限模式 (default/auto/plan/bypass)
	PermissionMode string `json:"permissionMode,omitempty"`

	// Cwd 工作目录
	Cwd string `json:"cwd,omitempty"`

	// Debug 调试模式
	Debug bool `json:"debug,omitempty"`

	// Skills 技能配置
	Skills *SkillsSection `json:"skills,omitempty"`

	// Dreaming 记忆整理配置
	Dreaming *DreamingSection `json:"dreaming,omitempty"`
}

// SkillsSection 技能配置段
type SkillsSection struct {
	Dirs []string `json:"dirs,omitempty"` // 额外技能目录
}

// DreamingSection Dreaming 配置段
type DreamingSection struct {
	Enabled     *bool `json:"enabled,omitempty"`
	MinHours    int   `json:"minHours,omitempty"`
	MinSessions int   `json:"minSessions,omitempty"`
}

// FeishuSection 飞书配置段
type FeishuSection struct {
	AppID           string `json:"appId"`
	AppSecret       string `json:"appSecret"`
	Domain          string `json:"domain,omitempty"`
	MentionOnly     *bool  `json:"mentionOnly,omitempty"`
	SessionTimeout  int    `json:"sessionTimeout,omitempty"`  // 分钟
	MaxSessions     int    `json:"maxSessions,omitempty"`
	WelcomeMessage  string `json:"welcomeMessage,omitempty"`
	ThinkingMessage string `json:"thinkingMessage,omitempty"`
}

// AISection AI 模型配置段
type AISection struct {
	Model     string `json:"model,omitempty"`
	APIKey    string `json:"apiKey,omitempty"`
	BaseURL   string `json:"baseUrl,omitempty"`
	MaxTokens int    `json:"maxTokens,omitempty"`
	MaxTurns  int    `json:"maxTurns,omitempty"`
}

// MCPServerEntry MCP 服务器条目
type MCPServerEntry struct {
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Transport string            `json:"transport,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// HookEntry Hook 配置条目
type HookEntry struct {
	Event   string `json:"event"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
	If      string `json:"if,omitempty"`
}

// LoadJSONConfig 从文件加载 JSON 配置。
// 如果 path 为空，尝试以下路径:
//  1. CLAUDE_GO_CONFIG 环境变量
//  2. ./claude-go.json (当前目录)
//  3. ~/.claude-go/config.json (用户目录)
func LoadJSONConfig(path string) (*JSONConfig, error) {
	if path == "" {
		path = os.Getenv("CLAUDE_GO_CONFIG")
	}
	if path == "" {
		candidates := []string{
			"claude-go.json",
			"config/claude-go.json",
		}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, home+"/.claude-go/config.json")
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				path = c
				break
			}
		}
	}
	if path == "" {
		return nil, nil // 无配置文件，使用默认值
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	var cfg JSONConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}

	return &cfg, nil
}

// ApplyToBot 将 JSON 配置应用到 BotConfig (合并, 不覆盖已有非零值)
func (jc *JSONConfig) ApplyToBot(bc *BotConfig) {
	if jc == nil {
		return
	}

	if jc.Feishu != nil {
		if bc.AppID == "" {
			bc.AppID = jc.Feishu.AppID
		}
		if bc.AppSecret == "" {
			bc.AppSecret = jc.Feishu.AppSecret
		}
		if bc.Domain == "" || bc.Domain == "feishu" {
			if jc.Feishu.Domain != "" {
				bc.Domain = jc.Feishu.Domain
			}
		}
		if jc.Feishu.MentionOnly != nil {
			bc.MentionOnly = *jc.Feishu.MentionOnly
		}
		if jc.Feishu.SessionTimeout > 0 && bc.SessionTimeout == 30*time.Minute {
			bc.SessionTimeout = time.Duration(jc.Feishu.SessionTimeout) * time.Minute
		}
		if jc.Feishu.MaxSessions > 0 && bc.MaxSessions == 100 {
			bc.MaxSessions = jc.Feishu.MaxSessions
		}
		if jc.Feishu.WelcomeMessage != "" && bc.WelcomeMessage == DefaultBotConfig().WelcomeMessage {
			bc.WelcomeMessage = jc.Feishu.WelcomeMessage
		}
		if jc.Feishu.ThinkingMessage != "" && bc.ThinkingMessage == DefaultBotConfig().ThinkingMessage {
			bc.ThinkingMessage = jc.Feishu.ThinkingMessage
		}
	}

	if jc.AI != nil {
		if bc.Model == "" || bc.Model == "qwen3.5-plus" {
			if jc.AI.Model != "" {
				bc.Model = jc.AI.Model
			}
		}
		if bc.APIKey == "" {
			bc.APIKey = jc.AI.APIKey
		}
		if bc.BaseURL == "" {
			bc.BaseURL = jc.AI.BaseURL
		}
		if jc.AI.MaxTokens > 0 && bc.MaxTokens == 16384 {
			bc.MaxTokens = jc.AI.MaxTokens
		}
		if jc.AI.MaxTurns > 0 && bc.MaxTurns == 0 {
			bc.MaxTurns = jc.AI.MaxTurns
		}
	}

	if jc.SystemPrompt != "" && bc.SystemPrompt == "" {
		bc.SystemPrompt = jc.SystemPrompt
	}
	if jc.PermissionMode != "" && bc.PermissionMode == "bypass" {
		bc.PermissionMode = jc.PermissionMode
	}
	if jc.Cwd != "" && bc.Cwd == "" {
		bc.Cwd = jc.Cwd
	}
	if jc.Debug {
		bc.Debug = true
	}

	if jc.Skills != nil {
		if len(jc.Skills.Dirs) > 0 {
			bc.SkillDirs = append(bc.SkillDirs, jc.Skills.Dirs...)
		}
	}

	if jc.Dreaming != nil {
		if jc.Dreaming.Enabled != nil {
			bc.DreamEnabled = *jc.Dreaming.Enabled
		}
		if jc.Dreaming.MinHours > 0 {
			bc.DreamMinHours = jc.Dreaming.MinHours
		}
		if jc.Dreaming.MinSessions > 0 {
			bc.DreamMinSessions = jc.Dreaming.MinSessions
		}
	}
}
