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
	"net/http"
	"os"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
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

	// Cwd 工作目录 (工具执行的根目录)
	Cwd string

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

	// StateDir 数据根目录，所有模块的数据/日志都存储在此下。
	// 默认为 Cwd/.claude-go。若为空且 Cwd 也为空，程序拒绝启动。
	StateDir string

	// ModelAlias 全局默认模型别名 (格式: "provider:modelName")
	ModelAlias string

	// FallbackAliases 全局默认备用模型别名列表
	FallbackAliases []string

	// Wiki LLM Wiki 知识库配置
	Wiki WikiConfig

	// Browser 浏览器自动化配置 (CDP)
	Browser BrowserConfig

	// PromptCacheMode 提示词缓存模式: "auto"(默认)/"on"/"off"
	PromptCacheMode string

	// EnableFrontierOptimizations QueryEngine 前沿优化特性开关 (默认 true)
	EnableFrontierOptimizations bool

	// PromptDebug 开启后将完整 LLM 请求/响应落盘到 PromptDebugDir。
	PromptDebug bool
	// PromptDebugDir 提示词调试文件目录。为空时使用 stateDir/prompt-debug。
	PromptDebugDir string

	// Plans 按工作流名称覆盖模型配置 (供 Agent Teams 使用)
	Plans map[string]PlanModelConfig

	// Providers 厂商配置 (新模式): 每个 provider 包含 baseURL/apiKey + 模型列表。
	Providers ProvidersSection
}

// BrowserConfig 浏览器抓取配置。
type BrowserConfig struct {
	// ChromePath Chrome/Chromium 可执行文件路径 (空=自动查找)
	ChromePath string `json:"chromePath,omitempty"`
	// ProxyURL 代理 URL
	ProxyURL string `json:"proxyUrl,omitempty"`
}

// WikiConfig LLM Wiki 知识库配置。
type WikiConfig struct {
	// Enabled 是否启用 Wiki 功能 (默认 true)
	Enabled bool
	// Repos Wiki 仓库目录列表 (支持多个知识库)，第一个为主仓库
	Repos []string
	// AutoIngestURL 是否自动从消息中提取 URL 并摄取到 wiki (默认 true)
	AutoIngestURL bool
	// AutoOrganize 是否自动在 Ingest 后触发增量整理 (默认 false)
	AutoOrganize bool
	// APIPort Wiki HTTP API 端口 (供 Obsidian 等外部客户端调用, 0=不启动)
	APIPort int
	// APISecret API 鉴权密钥 (Bearer token)
	APISecret string
	// APIExtensions 当 APIPort > 0 时在 wikiAPIServer.Start 前被调用,
	// 允许外部模块 (dashboard 等) 把自己的路由挂到同一 HTTP 端口,
	// 避免开多个监听端口。调用方负责保证路由 pattern 与 /wiki/* 不冲突。
	APIExtensions []func(mux *http.ServeMux) `json:"-"`
}

// DefaultBotConfig 返回默认配置
func DefaultBotConfig() *BotConfig {
	return &BotConfig{
		Domain:                      "feishu",
		SessionTimeout:              30 * time.Minute,
		MaxSessions:                 100,
		PermissionMode:              "bypass",
		MentionOnly:                 true,
		WelcomeMessage:              "你好！我是 Claude Code (Go) 机器人。发送消息与我对话，我可以帮你编程、分析代码、执行命令等。",
		ThinkingMessage:             "正在思考中...",
		EnableFrontierOptimizations: true,
		Wiki: WikiConfig{
			Enabled:       true,
			AutoIngestURL: true,
		},
	}
}

// FeishuTextContent 飞书文本消息内容格式
type FeishuTextContent struct {
	Text string `json:"text"`
}

// FeishuCardContent 飞书卡片消息内容格式 (用于富文本回复)
type FeishuCardContent struct {
	Config   *CardConfig   `json:"config,omitempty"`
	Header   *CardHeader   `json:"header,omitempty"`
	Elements []CardElement `json:"elements"`
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

	// Wiki LLM Wiki 知识库配置
	Wiki *WikiSection `json:"wiki,omitempty"`

	// Browser 浏览器自动化配置 (CDP)
	Browser *BrowserSection `json:"browser,omitempty"`

	// StateDir 数据根目录
	StateDir string `json:"stateDir,omitempty"`

	// Dashboard Dashboard 配置
	Dashboard *DashboardSection `json:"dashboard,omitempty"`

	// Engine QueryEngine 前沿优化特性配置
	Engine *EngineSection `json:"engine,omitempty"`

	// Providers 厂商配置 (新模式)
	Providers ProvidersSection `json:"providers,omitempty"`
}

// DashboardSection Dashboard 配置段
type DashboardSection struct {
	Port   int    `json:"port,omitempty"`   // 监听端口 (默认 7777)
	Addr   string `json:"addr,omitempty"`   // 完整监听地址 (覆盖 port)
	NoOpen bool   `json:"noOpen,omitempty"` // 不自动打开浏览器

	// StateDir 已废弃: Dashboard 现在统一复用顶层 stateDir / cwd 的解析规则
	// (见 basedir.ResolveDefault), 与 feishu bot 共享同一数据目录。
	// 仍然保留该字段仅为向后兼容: 若设置, 启动时会打 deprecation 警告,
	// 并作为最低优先级 fallback (低于 CLI --state-dir 和顶层 stateDir)。
	//
	// Deprecated: 请改用顶层 stateDir 或 cwd。
	StateDir string `json:"stateDir,omitempty"`
}

// EngineSection QueryEngine 前沿优化特性配置段
type EngineSection struct {
	// EnableFrontierOptimizations 一键启用推荐的 P0/P1 前沿优化组件 (默认 true)
	// 包括: Metrics, ErrorClassifier, JSONRepair, LoopDetector, PromptCache, Budget, Trajectory
	EnableFrontierOptimizations *bool `json:"enableFrontierOptimizations,omitempty"`
	// PromptDebug 开启后保存每次发给 LLM 的完整请求与响应。仅建议排查成本/提示词问题时短期开启。
	PromptDebug *bool `json:"promptDebug,omitempty"`
	// PromptDebugDir 自定义调试输出目录。为空时使用 stateDir/prompt-debug。
	PromptDebugDir string `json:"promptDebugDir,omitempty"`
}

// WikiSection Wiki 配置段 (JSON)
type WikiSection struct {
	Enabled       *bool    `json:"enabled,omitempty"`
	Repos         []string `json:"repos,omitempty"`
	AutoIngestURL *bool    `json:"autoIngestUrl,omitempty"`
	AutoOrganize  *bool    `json:"autoOrganize,omitempty"`
	APIPort       int      `json:"apiPort,omitempty"`
	APISecret     string   `json:"apiSecret,omitempty"`
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

// BrowserSection 浏览器抓取配置段 (JSON)
type BrowserSection struct {
	ChromePath string `json:"chromePath,omitempty"`
	ProxyURL   string `json:"proxyUrl,omitempty"`
}

// FeishuSection 飞书配置段
type FeishuSection struct {
	AppID           string `json:"appId"`
	AppSecret       string `json:"appSecret"`
	Domain          string `json:"domain,omitempty"`
	MentionOnly     *bool  `json:"mentionOnly,omitempty"`
	SessionTimeout  int    `json:"sessionTimeout,omitempty"` // 分钟
	MaxSessions     int    `json:"maxSessions,omitempty"`
	WelcomeMessage  string `json:"welcomeMessage,omitempty"`
	ThinkingMessage string `json:"thinkingMessage,omitempty"`
}

// ProviderModelConfig 单个模型在 provider 中的配置。
// 注意: 别名本身编码了 provider 和实际模型名 (格式 "provider:modelName")，
// 因此 ProviderModelConfig 不再包含 Alias 和 ProviderName 字段。
type ProviderModelConfig struct {
	MaxTokens       int    `json:"maxTokens,omitempty"`
	MaxTurns        int    `json:"maxTurns,omitempty"`
	PromptCacheMode string `json:"promptCacheMode,omitempty"`
	ContextWindow   int    `json:"contextWindow,omitempty"`
}

// ProviderConfig 单个厂商的 API 连接参数。
type ProviderConfig struct {
	Name    string                         `json:"name"`
	BaseURL string                         `json:"baseUrl"`
	APIKey  string                         `json:"apiKey"`
	Models  map[string]ProviderModelConfig `json:"models"` // key=alias
}

// PlanModelConfig 单个 Plan (工作流) 的模型配置。
// 只配置别名，URL/KEY 通过别名自动解析。
type PlanModelConfig struct {
	ModelAlias      string            `json:"modelAlias,omitempty"`      // 该 plan 使用的模型别名
	FallbackAliases []string          `json:"fallbackAliases,omitempty"` // 该 plan 的备用模型别名
	RoleAliases     map[string]string `json:"roleAliases,omitempty"`     // 按 role 覆盖别名
}

// AISection AI 模型配置段 (只保留别名模式)。
type AISection struct {
	ModelAlias      string                     `json:"modelAlias,omitempty"`      // 全局默认模型别名
	FallbackAliases []string                   `json:"fallbackAliases,omitempty"` // 全局默认备用模型别名
	PromptCacheMode string                     `json:"promptCacheMode,omitempty"` // "auto"(默认)/"on"/"off"
	Plans           map[string]PlanModelConfig `json:"plans,omitempty"`           // 按 plan 名称覆盖模型配置
}

// ProvidersSection providers 配置段 (新模式)。
type ProvidersSection map[string]ProviderConfig

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

// ToModelConfigJSON 将 JSONConfig 转换为 modelconfig.ConfigJSON。
func (jc *JSONConfig) ToModelConfigJSON() modelconfig.ConfigJSON {
	var out modelconfig.ConfigJSON
	if len(jc.Providers) > 0 {
		out.Providers = make(map[string]modelconfig.ProviderConfig, len(jc.Providers))
		for name, p := range jc.Providers {
			models := make(map[string]modelconfig.ModelConfig, len(p.Models))
			for alias, mc := range p.Models {
				models[alias] = modelconfig.ModelConfig{
					MaxTokens:       mc.MaxTokens,
					MaxTurns:        mc.MaxTurns,
					PromptCacheMode: mc.PromptCacheMode,
					ContextWindow:   mc.ContextWindow,
				}
			}
			out.Providers[name] = modelconfig.ProviderConfig{
				Name:    p.Name,
				BaseURL: p.BaseURL,
				APIKey:  p.APIKey,
				Models:  models,
			}
		}
	}
	if jc.AI != nil {
		out.AI.GlobalConfig = modelconfig.GlobalConfig{
			DefaultModelAlias:      jc.AI.ModelAlias,
			DefaultFallbackAliases: jc.AI.FallbackAliases,
			DefaultPromptCacheMode: jc.AI.PromptCacheMode,
		}
		out.AI.Plans = make(map[string]modelconfig.PlanConfig, len(jc.AI.Plans))
		for planName, pc := range jc.AI.Plans {
			plan := modelconfig.PlanConfig{
				ModelAlias:      pc.ModelAlias,
				FallbackAliases: pc.FallbackAliases,
			}
			plan.Roles = make(map[string]modelconfig.RoleConfig, len(pc.RoleAliases))
			for role, alias := range pc.RoleAliases {
				plan.Roles[role] = modelconfig.RoleConfig{ModelAlias: alias}
			}
			out.AI.Plans[planName] = plan
		}
	}
	return out
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
		if bc.ModelAlias == "" {
			if jc.AI.ModelAlias != "" {
				bc.ModelAlias = jc.AI.ModelAlias
			}
		}
		if jc.AI.PromptCacheMode != "" && bc.PromptCacheMode == "" {
			bc.PromptCacheMode = jc.AI.PromptCacheMode
		}
		if len(jc.AI.FallbackAliases) > 0 && len(bc.FallbackAliases) == 0 {
			bc.FallbackAliases = jc.AI.FallbackAliases
		}
		if len(jc.AI.Plans) > 0 {
			bc.Plans = make(map[string]PlanModelConfig, len(jc.AI.Plans))
			for name, pc := range jc.AI.Plans {
				bc.Plans[name] = pc
			}
		}
	}

	if len(jc.Providers) > 0 {
		bc.Providers = make(map[string]ProviderConfig, len(jc.Providers))
		for name, p := range jc.Providers {
			bc.Providers[name] = p
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

	if jc.StateDir != "" && bc.StateDir == "" {
		bc.StateDir = jc.StateDir
	}

	if jc.Wiki != nil {
		if jc.Wiki.Enabled != nil {
			bc.Wiki.Enabled = *jc.Wiki.Enabled
		}
		if len(jc.Wiki.Repos) > 0 {
			bc.Wiki.Repos = jc.Wiki.Repos
		}
		if jc.Wiki.AutoIngestURL != nil {
			bc.Wiki.AutoIngestURL = *jc.Wiki.AutoIngestURL
		}
		if jc.Wiki.AutoOrganize != nil {
			bc.Wiki.AutoOrganize = *jc.Wiki.AutoOrganize
		}
		if jc.Wiki.APIPort > 0 {
			bc.Wiki.APIPort = jc.Wiki.APIPort
		}
		if jc.Wiki.APISecret != "" {
			bc.Wiki.APISecret = jc.Wiki.APISecret
		}
	}

	if jc.Browser != nil {
		if jc.Browser.ChromePath != "" {
			bc.Browser.ChromePath = jc.Browser.ChromePath
		}
		if jc.Browser.ProxyURL != "" {
			bc.Browser.ProxyURL = jc.Browser.ProxyURL
		}
	}

	// Engine: 默认开启前沿优化 (与之前硬编码行为一致), 除非显式设为 false
	if jc.Engine != nil && jc.Engine.EnableFrontierOptimizations != nil {
		bc.EnableFrontierOptimizations = *jc.Engine.EnableFrontierOptimizations
	} else {
		bc.EnableFrontierOptimizations = true
	}
	if jc.Engine != nil {
		if jc.Engine.PromptDebug != nil {
			bc.PromptDebug = *jc.Engine.PromptDebug
		}
		if jc.Engine.PromptDebugDir != "" {
			bc.PromptDebugDir = jc.Engine.PromptDebugDir
		}
	}
}
