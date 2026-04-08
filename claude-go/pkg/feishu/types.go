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

	// MCPConfigPath MCP 配置文件路径
	MCPConfigPath string
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
