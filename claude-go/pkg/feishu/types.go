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
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/sandbox"
	"github.com/anthropic/claude-go/pkg/sync"
)

// BotConfig 飞书机器人配置
type BotConfig struct {
	// Headless 无飞书模式 (design/02 §四 单体部署形态):
	// 完整装配 :18080 wiki+dashboard+teams+cron+sync 全栈, 但不连接飞书 WS,
	// 出站飞书消息降级为日志。K8s 单体 Pod / 下游平台专用实例用此模式,
	// 避免与生产 bot 重复消费同一飞书应用的消息。
	Headless bool

	// AppID 飞书应用 App ID (Headless 模式下可空)
	// 在飞书开发者后台 > 应用凭证中获取
	AppID string

	// AppSecret 飞书应用 App Secret (Headless 模式下可空)
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

	// ExecutionModel 自动相位路由的快速执行模型 (Path B, 工具消费回合用)。
	// 取 alias 后半段 (短名, 如 "lfm2.5:2.6b-q4_k_m"); 留空 = 关闭自动路由。
	// 规划/推理回合仍走 ModelAlias。
	ExecutionModel string
	// PlanModel 规划期独立模型别名 (格式 "provider:modelName"); 留空 = 规划期沿用主模型。
	// 类似 advisor 独立客户端: 会话 plan flag ON (规划期) 时引擎全程路由到独立 plan 客户端。
	PlanModel string

	// AutoPlanMode 引擎级复杂度自判断 → 自动 Plan Mode + 自动 Advisor。
	// 复杂任务在首轮自动开启会话级只读规划并注入 system-reminder 引导,
	// 由模型自行调用 ExitPlanMode 进入实施。默认 true。
	AutoPlanMode bool
	// ComplexityMode 复杂度判定模式: "heuristic"(默认, 零 LLM 开销)/"llm"/"hybrid"。
	ComplexityMode string
	// AutoAdvisor 复杂任务自动咨询 advisor (需 Advisor 已装配)。
	AutoAdvisor bool
	// PlanFileDir 计划文件目录: ExitPlanMode 将计划落盘, 实施阶段模型读取。
	// 空 = 由装配方推导为 <StateDir>/plans。
	PlanFileDir string

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

	// EnableStopSignal CaRT 停止信号开关 (实验性, 不在上面的一键包里)。
	// 指针: 缺省 = 按各路径原行为 (飞书主会话不开 / 团队 agent 开 / CLI 不开),
	// 显式 true/false 才覆盖。见 EngineSection.EnableStopSignal。
	EnableStopSignal *bool

	// PromptDebug 开启后将完整 LLM 请求/响应落盘到 PromptDebugDir。
	PromptDebug bool
	// PromptDebugDir 提示词调试文件目录。为空时使用 stateDir/prompt-debug。
	PromptDebugDir        string
	PromptDebugMaxFiles   int
	PromptDebugMaxBytes   int64
	PromptDebugSampleRate float64
	PromptDebugRedact     bool

	// EnvironmentDate 是否在系统提示的 <environment> 块里写当前日期。默认 false ——
	// 该行靠前且每天变一次, 会让它之后的整段前缀天天作废。时间无关的业务不需要它。
	EnvironmentDate bool

	// MemoryIndexMode 记忆注入形态: false(默认) = 注入正文, true = 只注入索引
	// (id + 标题 + 预览), 正文由模型按需调 memory_read 取回。
	//
	// 默认关的理由是缓存: 正文形态实测单次注入 ~18.5K 字符 (7 条 × 截断到 4096),
	// 而记忆库每次 run 结尾都被写一次, 于是下一次 run 的 BM25 top-K 必然不同 ——
	// 这块自己就是漂移源。索引形态把它压到 ~1K, 漂移的绝对代价随之降一个数量级。
	//
	// 代价是模型得自己发现该去取: 打开后 memory_read 的实际调用率是个未知数,
	// 先量一段再决定要不要默认开 (见 builtin.MemoryIndexModeEnabled 的注释)。
	//
	// 指针而非 bool: 这个值要**原样**转交给 engine.Config.MemoryIndexMode, 而后者
	// 靠 nil 区分「没配」与「显式配了 false」(见 QueryEngine.resolvedMemoryIndexMode)。
	// 用值类型的话这一层就把 nil 压成了 false, 于是「没配」被当成「显式关」——
	// CLAUDE_GO_MEMORY_INDEX_MODE 这条兜底在飞书路径上会彻底失效。
	MemoryIndexMode *bool

	// Plans 按工作流名称覆盖模型配置 (供 Agent Teams 使用)
	Plans map[string]PlanModelConfig

	// Providers 厂商配置 (新模式): 每个 provider 包含 baseURL/apiKey + 模型列表。
	Providers ProvidersSection

	// Sandbox agent 代码执行沙盒配置。
	Sandbox *sandbox.Config

	// Sync 外部数据源同步配置。
	Sync sync.Config

	// Advisor 顾问工具配置。
	Advisor *AdvisorSection
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
	// APISecret API 鉴权密钥 (Bearer token)。
	// 为空 = 不鉴权 (fail-open, 兼容 8+ 下游平台的历史调用), 详见 pkg/httpauth。
	APISecret string
	// APIHost HTTP API 监听地址的 host 部分。空 = 绑全部网卡 (历史行为,
	// 局域网与 tailscale 均可访问)。想收回本机请设 "127.0.0.1"。
	// 注意本端口不只承载 /wiki/*, dashboard 的 /api/* 与 cluster 的
	// /cluster/* 也挂在同一 mux 上, 故此项决定整个 :18080 面的暴露程度。
	APIHost string
	// APIExtensions 当 APIPort > 0 时在 wikiAPIServer.Start 前被调用,
	// 允许外部模块 (dashboard 等) 把自己的路由挂到同一 HTTP 端口,
	// 避免开多个监听端口。调用方负责保证路由 pattern 与 /wiki/* 不冲突。
	APIExtensions []func(mux *http.ServeMux) `json:"-"`
	// SyncTaskSubmitter 非 nil 时，IMA/微信读书同步的**执行体**改成提交
	// TaskService（design/02 §3.5 通道表 sync 那一行），两条触发路径
	// （/sync/* 端点 与 进程内 cron tick）都走它，进程内直跑 RunSync 一并摘掉。
	//
	// 为什么走配置字段而不是 NewBot 之后 setter 注入：wikiAPI 在 NewBot 内构造
	// **并立即 Start**，返回后再 set 会留一个"服务已在听、执行体还没换"的窗口，
	// 那期间的请求静默走老路——正是"一半动作走新路一半走老路"那类现场无法归因
	// 的故障。类型是 pkg/sync 里的窄接口，本文件已 import 该包。
	SyncTaskSubmitter sync.TaskSubmitter `json:"-"`
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
		AutoPlanMode:                true,
		ComplexityMode:              "heuristic",
		PromptDebugMaxFiles:         500,
		PromptDebugMaxBytes:         200 * 1024 * 1024,
		PromptDebugRedact:           true,
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

// SyncSection 外部数据源同步配置段。
type SyncSection struct {
	// KnowledgeRepo 知识库根目录。
	KnowledgeRepo string `json:"knowledgeRepo,omitempty"`
	// IMA IMA 笔记同步配置。
	IMA sync.IMAConfig `json:"ima,omitempty"`
	// WeRead 微信读书同步配置。
	WeRead sync.WeReadConfig `json:"weread,omitempty"`
}

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

	// AllowedTools 工具白名单。与 CLI --allowed-tools 同义, 优先级低于 flag:
	// flag 非空时以 flag 为准。
	//
	// 存在的理由是**托管 Agent 的部署态**: 平台用配置文件下发岗位的工具面,
	// 而此前白名单只能拼命令行 —— 命令行参数不可回读、不可版本化、不可对账。
	// 语义与 engine.Config.AllowedTools 逐字一致: 缺省 = 不设白名单 (全部可见);
	// 显式给出即白名单, 且**空表是 fail-closed**(拒绝一切工具), 不是"不设限" ——
	// 调用方的岗位模板若渲染成空, 宁可拒绝启动也不能静默放开全部工具。
	// 两种写法等价: "a,b,c" 或是 ["a","b","c"]。
	AllowedTools StringList `json:"allowedTools,omitempty"`

	// TicketID / InstanceID 运维工单与实例的索引维度 (可选)。
	//
	// 平台侧用它们回答"这个实例上周发生了什么""这张工单当时怎么判的", 而
	// 会话 ID 回答不了这两个问题。设置后写进 trace IDs, 随 span 落盘。
	TicketID   string `json:"ticketId,omitempty"`
	InstanceID string `json:"instanceId,omitempty"`

	// AssemblyProfile 装配描述符 (方案 20.6.3): 平台认为它下发了一份怎样的装配。
	//
	// 存在的理由是把"一个数字员工是什么"从三处散落 (平台侧 ManagedAgent 字段、
	// 拼出来的命令行、这份 config 的模型与预算段) 收敛成一个**平台生成、harness
	// 装载、可版本化、可回读**的单一对象。命令行参数做不到这件事: 不可回读 (进程
	// 起来后没人知道当时传了什么)、不可版本化 (改一次没法 diff / 回滚 / 回答"上周
	// 那次事故时的装配是什么")、不可对账 (只有下发没有生效的回程)。
	//
	// 本字段是**声明侧的唯一来源**。对账 (pkg/assembly.Reconcile) 拿它跟运行期
	// 采集的装配自报逐项比。刻意不让 harness 从别的配置字段"推断"出声明: 推断出来
	// 的是一份**期望**不是一份**声明** —— 期望由 harness 自己写, 拿它去对账等于
	// 自己判自己, 报出来的差异谁也修不了。声明必须能指回一份平台签发的版本。
	AssemblyProfile *AssemblyProfile `json:"assemblyProfile,omitempty"`

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

	// Sandbox agent 代码执行沙盒配置。
	Sandbox *sandbox.Config `json:"sandbox,omitempty"`

	// CodeIntel Code Intelligence 配置 (MCP Server 与内部工具共用)
	CodeIntel *CodeIntelSection `json:"codeIntel,omitempty"`

	// Sync 外部数据源同步配置 (IMA / 微信读书)
	Sync *SyncSection `json:"sync,omitempty"`

	// Advisor 顾问工具配置 (设计文档: docs/advisor-tool-design.md)
	Advisor *AdvisorSection `json:"advisor,omitempty"`
}

// AdvisorSection advisor 顾问工具配置段。
// 主模型在关键时刻通过 advisor() 工具咨询更强的 reviewer 模型。
type AdvisorSection struct {
	// Enabled 是否启用 advisor 工具
	Enabled bool `json:"enabled,omitempty"`
	// ModelAlias advisor 模型别名 (格式 "provider:modelName"); 为空时禁用
	ModelAlias string `json:"modelAlias,omitempty"`
	// MaxCallsPerSession 单会话最大咨询次数 (默认 8)
	MaxCallsPerSession int `json:"maxCallsPerSession,omitempty"`
	// CooldownTurns 两次咨询间最少间隔的 assistant 轮数 (默认 2)
	CooldownTurns int `json:"cooldownTurns,omitempty"`
	// MaxTranscriptTokens transcript 序列化 token 预算 (默认 60000)
	MaxTranscriptTokens int `json:"maxTranscriptTokens,omitempty"`
	// MaxOutputTokens advisor 回复最大输出 token (默认 2000)
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	// CheckpointEveryTurns Phase 3 push 模式: 每 N 轮 harness 主动咨询一次 (0=关闭)
	CheckpointEveryTurns int `json:"checkpointEveryTurns,omitempty"`
	// CheckpointOnLoop Phase 3 push 模式: LoopDetector 检测到工具循环时主动咨询
	CheckpointOnLoop bool `json:"checkpointOnLoop,omitempty"`
}

// CodeIntelSection Code Intelligence 配置段
type CodeIntelSection struct {
	// RepoPath 默认仓库路径 (可选; 所有工具均支持通过参数传入 repo_path 实现多仓库管理)
	RepoPath string `json:"repoPath,omitempty"`
	// Transport MCP 传输方式: stdio | http | sse (默认 stdio)
	Transport string `json:"transport,omitempty"`
	// Addr HTTP/SSE 监听地址 (仅 transport=http/sse 时生效, 默认 127.0.0.1:9234)
	Addr string `json:"addr,omitempty"`
	// IndexBaseDir 集中索引根目录 (如 /mnt/data/codeintel)。为空时索引存放在各仓库本地。
	IndexBaseDir string `json:"indexBaseDir,omitempty"`
	// GitNexus GitNexus 运行时调优配置
	GitNexus *struct {
		DefaultHeapMB        int `json:"defaultHeapMB,omitempty"`
		MaxHeapMB            int `json:"maxHeapMB,omitempty"`
		PerThousandFilesMB   int `json:"perThousandFilesMB,omitempty"`
		PerHundredMBSourceMB int `json:"perHundredMBSourceMB,omitempty"`
		IndexTimeoutMin      int `json:"indexTimeoutMin,omitempty"`
	} `json:"gitnexus,omitempty"`
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
	PromptDebugDir        string  `json:"promptDebugDir,omitempty"`
	PromptDebugMaxFiles   int     `json:"promptDebugMaxFiles,omitempty"`
	PromptDebugMaxBytes   int64   `json:"promptDebugMaxBytes,omitempty"`
	PromptDebugSampleRate float64 `json:"promptDebugSampleRate,omitempty"`
	PromptDebugRedact     *bool   `json:"promptDebugRedact,omitempty"`

	// EnvironmentDate 是否在系统提示的 <environment> 块里写当前日期。默认 false。
	//
	// 默认关的理由是缓存而非偏好: 该行排在系统提示靠前处, 跨零点一变就让它之后的
	// 整段前缀失效。时间无关的业务 (面试平台的 JD 分析/题库生成等) 不需要它;
	// 需要时效的业务显式置 true。
	EnvironmentDate *bool `json:"environmentDate,omitempty"`

	// MemoryIndexMode 记忆注入形态开关: true = 只注入索引 (id + 标题 + 预览),
	// 正文由模型按需 memory_read 取回; false/缺省 = 注入正文 (旧行为)。
	//
	// 指针类型 + omitempty 与同段其它开关一致: 缺省与显式 false 必须能区分,
	// 否则将来改默认值时老配置会被静默反向覆盖。
	MemoryIndexMode *bool `json:"memoryIndexMode,omitempty"`

	// EnableStopSignal 启用 CaRT 停止信号检测 (实验性, 见 engine.Config.EnableStopSignal)。
	//
	// 这个字段存在的理由是**消除一条路径不对称**: 该组件此前只在飞书侧被打开
	// (pkg/feishu/session.go 团队 agent 路径), CLI headless —— 也就是平台实际走的
	// 那条 —— 永远拿不到它, 而两条路径读的是同一份配置文件。20.6.2 记的正是这一类
	// "配置面建好了、只在一半路径上通电"的缺陷, 顺着自报能被照出来 (装配自报里
	// components.stop_signal 会如实报 false), 但要能**修**, 得让平台有地方声明它。
	//
	// 缺省 nil = 保持各路径原有行为 (CLI 不开 / 飞书团队路径开), 所以加上这个字段
	// 本身不改变任何现网行为; 显式 true/false 才会覆盖。
	EnableStopSignal *bool `json:"enableStopSignal,omitempty"`
}

// WikiSection Wiki 配置段 (JSON)
type WikiSection struct {
	Enabled       *bool    `json:"enabled,omitempty"`
	Repos         []string `json:"repos,omitempty"`
	AutoIngestURL *bool    `json:"autoIngestUrl,omitempty"`
	AutoOrganize  *bool    `json:"autoOrganize,omitempty"`
	APIPort       int      `json:"apiPort,omitempty"`
	APISecret     string   `json:"apiSecret,omitempty"`
	APIHost       string   `json:"apiHost,omitempty"`
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
	// Protocol 出站协议: ""(默认 anthropic /messages) | "openai" (/chat/completions)
	// | "openai-responses" (/v1/responses, opencode 对 muse-spark 等 Meta 模型推荐)。
	Protocol string `json:"protocol,omitempty"`
	// ReasoningPlayback 出站回传上一轮推理的 OpenAI 字段名; 只对 protocol:"openai"
	// 有意义 (""=不回传, "reasoning", "reasoning_content")。
	// 与 Protocol 一样是模型级开关: DeepSeek 拒绝在请求里回传 reasoning_content,
	// opencode step-5 接受, 全球开会让同一份 provider 列表里的兜底模型整批报错。
	// 完整说明见 modelconfig.ModelConfig.ReasoningPlayback。
	ReasoningPlayback string `json:"reasoningPlayback,omitempty"`
	// Proxy 出站 HTTP 代理 URL (http://host:port)。模型级配置 —— 只有配置了 proxy 的
	// 模型才走代理 (如 muse-spark 大陆不可达, 须经东京 tailnet 代理出口), 其余模型
	// 保持直连, 不消耗代理出口流量额度。
	Proxy string `json:"proxy,omitempty"`
	// 模型专属限流参数 (0 = 使用全局默认值)
	RPM         int `json:"rpm,omitempty"`         // 每分钟最大请求数
	MaxParallel int `json:"maxParallel,omitempty"` // 最大并发请求数 (本地 Ollama 对应 OLLAMA_NUM_PARALLEL)
	MinParallel int `json:"minParallel,omitempty"` // 最小并发请求数 (AIMD 下限)
	// 超时参数 (0 = 使用全局默认值); 本地大模型解码慢, 建议调大
	FirstTokenTimeoutSec int `json:"firstTokenTimeoutSec,omitempty"`
	CallTimeoutSec       int `json:"callTimeoutSec,omitempty"`
	DeadlineRetryBaseSec int `json:"deadlineRetryBaseSec,omitempty"`
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
	ModelAlias      string   `json:"modelAlias,omitempty"`      // 全局默认模型别名
	ExecutionModel  string   `json:"executionModel,omitempty"`  // 自动相位路由的快速执行模型 (Path B, 工具消费回合); 空=关闭
	PlanModel       string   `json:"planModel,omitempty"`       // 规划期独立模型 (plan mode 专用, 类似 advisor 独立客户端); 空=关闭
	FallbackAliases []string `json:"fallbackAliases,omitempty"` // 全局默认备用模型别名
	PromptCacheMode string   `json:"promptCacheMode,omitempty"` // "auto"(默认)/"on"/"off"
	MaxTurns        int      `json:"maxTurns,omitempty"`        // 全局默认最大回合数 (0=不限); 防 agent 无界循环
	MaxTokens       int      `json:"maxTokens,omitempty"`       // 全局默认最大输出 token (0=用引擎默认)
	// LongHorizonMaxTurns 长程任务档的回合上限 (H4-3)。产码/测试/修复这类没有自然
	// 下界的任务取这个值; 常规任务仍取 maxTurns。0 = 不开分层 (长程档回落 maxTurns)。
	LongHorizonMaxTurns int                        `json:"longHorizonMaxTurns,omitempty"`
	Plans               map[string]PlanModelConfig `json:"plans,omitempty"`    // 按 plan 名称覆盖模型配置
	AutoPlan            *AutoPlanSection           `json:"autoPlan,omitempty"` // AutoMode: 复杂度自判断 → 自动 Plan + Advisor
}

// AutoPlanSection AutoMode 配置段 (引擎级复杂度自判断)。
type AutoPlanSection struct {
	// Enabled 总开关 (默认 true)。
	Enabled *bool `json:"enabled,omitempty"`
	// Mode 复杂度判定模式: "heuristic"(默认, 零 LLM 开销)/"llm"/"hybrid"。
	Mode string `json:"mode,omitempty"`
	// AutoAdvisor 复杂任务自动咨询 advisor (需 advisor 已装配)。
	AutoAdvisor *bool `json:"autoAdvisor,omitempty"`
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
	Event    string `json:"event"`
	HookType string `json:"hook_type,omitempty"`
	Command  string `json:"command"`
	Timeout  int    `json:"timeout,omitempty"`
	If       string `json:"if,omitempty"`
	URL      string `json:"url,omitempty"`

	// MCP 类型专用
	MCPServer string `json:"mcp_server,omitempty"`
	MCPTool   string `json:"mcp_tool,omitempty"`

	// Plugin 类型专用
	PluginPath   string `json:"plugin_path,omitempty"`
	PluginSymbol string `json:"plugin_symbol,omitempty"`

	// OPA 类型专用
	OPAPolicy string `json:"opa_policy,omitempty"`
	OPAQuery  string `json:"opa_query,omitempty"`

	// Function 类型专用
	FunctionName string `json:"function_name,omitempty"`

	// gRPC 类型专用
	GRPCService string `json:"grpc_service,omitempty"`
	GRPCMethod  string `json:"grpc_method,omitempty"`
}

// ResolveJSONConfigPath 解析 JSON 配置文件路径。
// 如果 path 为空，尝试以下路径:
//  1. CLAUDE_GO_CONFIG 环境变量
//  2. ./claude-go.json (当前目录)
//  3. ./config/claude-go.json
//  4. ~/.claude-go/config/config.json (新版用户目录)
//  5. ~/.claude-go/config.json (旧版用户目录)
//  6. /etc/claude-go/config.json (容器约定路径, K8s ConfigMap 挂载点)
//
// 第 6 条是容器部署的关键: deploy/k8s 的清单把 ConfigMap 挂到 /etc/claude-go,
// 但 control/worker 的 args 里并没有传 --config (只有 gateway 传了)。此前发现
// 列表不含该路径, 于是**挂载的 ConfigMap 从未被读取**, Pod 一路落到"占位
// provider"分支——那样跑出来的 K8s 冒烟不接触 LLM, 不构成功能验证。
func ResolveJSONConfigPath(path string) (string, error) {
	p, _, err := ResolveJSONConfigPathWithOrigin(path)
	return p, err
}

// ConfigOrigin* 是配置命中的查找链环节, 供装配自报 (pkg/assembly.ConfigSource) 回填。
//
// 为什么要报"哪一环"而不只是路径: 现场最常见的浪费是"你改的不是它读的那一份" ——
// 宿主上改了 /etc/claude-go/config.json, 实际生效的却是 $HOME/.claude-go/config.json,
// 而两者只给出路径时看不出谁赢了 (改的那份也是合法路径, 也真实存在)。
// 环节名让人一眼看到优先级链上是谁截胡了。
const (
	ConfigOriginFlag      = "flag --config"
	ConfigOriginEnv       = "env CLAUDE_GO_CONFIG"
	ConfigOriginCwd       = "cwd ./claude-go.json"
	ConfigOriginCwdConfig = "cwd ./config/claude-go.json"
	ConfigOriginHomeNew   = "home ~/.claude-go/config/config.json"
	ConfigOriginHomeOld   = "home ~/.claude-go/config.json"
	ConfigOriginEtc       = "container /etc/claude-go/config.json"
	// ConfigOriginNone 六环都没命中 —— 走的是纯默认值, 不是"读到了空配置"。
	ConfigOriginNone = "default"
)

// ResolveJSONConfigPathWithOrigin 与 ResolveJSONConfigPath 同源, 额外返回命中的环节。
//
// 返回的 path 语义与老函数逐字节一致 (仅加了一个来源标注), 老函数改为调用本函数,
// 保证两条入口不可能在"选哪一份"这件事上分叉 —— 分叉过一次的查找链, 自报就会
// 报出一份与生效配置不同的来源, 那比不报还坏。
func ResolveJSONConfigPathWithOrigin(path string) (string, string, error) {
	if path != "" {
		return path, ConfigOriginFlag, nil
	}
	if env := os.Getenv("CLAUDE_GO_CONFIG"); env != "" {
		return env, ConfigOriginEnv, nil
	}
	type candidate struct{ path, origin string }
	candidates := []candidate{
		{"claude-go.json", ConfigOriginCwd},
		{"config/claude-go.json", ConfigOriginCwdConfig},
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			candidate{home + "/.claude-go/config/config.json", ConfigOriginHomeNew},
			candidate{home + "/.claude-go/config.json", ConfigOriginHomeOld},
		)
	}
	// 容器约定路径放最后: 本机开发时家目录配置优先, 容器里家目录通常没有
	// 配置, 自然落到这一条。
	candidates = append(candidates, candidate{"/etc/claude-go/config.json", ConfigOriginEtc})
	for _, c := range candidates {
		if _, err := os.Stat(c.path); err == nil {
			return c.path, c.origin, nil
		}
	}
	return "", ConfigOriginNone, nil
}

// StringList 是一个"字符串数组或逗号分隔字符串"都能解析的配置类型。
//
// 之所以不直接用 []string: 配置文件是人手写的, "a,b,c" 比 ["a","b","c"] 好写,
// 而平台生成的又是数组 —— 两种写法都要能读。不直接用 string: 数组写法会直接
// 解析失败, 而不是退化成一个叫 `["a"` 的工具名。
//
// **DoesNotExist 时是 nil 切片**: nil 在这里是语义的一部分 (区别于空切片),
// 调用方靠它区分"没配白名单"与"配了空白名单" —— 后者是 fail-closed 的拒绝一切。
// AssemblyProfile 装配描述符 (方案 20.6.3)。
//
// 每一段都是**声明**: "平台签发时认为这个员工应当装配成这样"。它不描述"配置里
// 写了什么", 而是描述"平台要的是什么" —— 两者的差距正是对账要照出来的东西。
//
// 字段刻意都是可选的: 未声明的段**不参与对账** (而不是"声明为空")。一份只声明
// 工具面的描述符, 不该在模型面上报出一条"声明了但没生效"。
type AssemblyProfile struct {
	// Version 描述符版本 (平台签发, 如 "2026-09-20.3")。空 = 平台未盖章。
	// 它是对账结论的坐标系: 差异要能指回"哪一版描述符", 否则修完无从复核。
	Version string `json:"version,omitempty"`

	// Tools 声明的工具白名单。nil = 未声明 (不参与对账);
	// 非 nil (含空切片) = 声明了白名单, 空切片即"应当拒绝一切"。
	Tools StringList `json:"tools,omitempty"`

	// Hooks 声明的外部 hook。序号 Index 由数组下标决定 (与运行期自报的编号口径
	// 一致), 不接受手填 —— 两个编号口径一旦分家, 对账会报出一堆"没装上"的幻觉。
	Hooks []AssemblyProfileHook `json:"hooks,omitempty"`

	// Components 声明的前沿优化组件名 (metrics / prompt_cache / budget_degrade /
	// loop_detector / error_classifier / json_repair / trajectory / stop_signal)。
	// 这是 20.6.2 那个缺陷 (CLI 路径六个组件为 nil) 能被照出来的地方。
	Components StringList `json:"components,omitempty"`

	// Model 声明的主模型; 空 = 未声明。
	Model string `json:"model,omitempty"`

	// PermissionMode 声明的权限档; 空 = 未声明。
	PermissionMode string `json:"permissionMode,omitempty"`
}

// AssemblyProfileHook 描述符里的一条 hook 声明。
type AssemblyProfileHook struct {
	Event string `json:"event"`
	Type  string `json:"type,omitempty"`
}

type StringList []string

// UnmarshalJSON 接受三种形态: 字符串 (按逗号切)、数组、null。
func (s *StringList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*s = nil
		return nil
	}
	if trimmed[0] == '[' {
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		*s = cleanStringList(arr)
		return nil
	}
	var str string
	if err := json.Unmarshal(trimmed, &str); err != nil {
		return fmt.Errorf("allowedTools 取值既不是数组也不是字符串: %w", err)
	}
	*s = splitStringList(str)
	return nil
}

// MarshalJSON 写回数组形态 (回读一致性: 写出去的一定能被自己解析回来)。
func (s StringList) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	return json.Marshal([]string(s))
}

// splitStringList 按逗号切分并丢掉空项。
func splitStringList(csv string) StringList {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return cleanStringList(out)
}

// cleanStringList 去空白、去空项、去重 (保序)。
func cleanStringList(in []string) StringList {
	seen := make(map[string]bool, len(in))
	out := make(StringList, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// LoadJSONConfig 从文件加载 JSON 配置。
func LoadJSONConfig(path string) (*JSONConfig, error) {
	path, err := ResolveJSONConfigPath(path)
	if err != nil {
		return nil, err
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
					MaxTokens:            mc.MaxTokens,
					MaxTurns:             mc.MaxTurns,
					PromptCacheMode:      mc.PromptCacheMode,
					ContextWindow:        mc.ContextWindow,
					Protocol:             mc.Protocol,
					ReasoningPlayback:    mc.ReasoningPlayback,
					Proxy:                mc.Proxy,
					RPM:                  mc.RPM,
					MaxParallel:          mc.MaxParallel,
					MinParallel:          mc.MinParallel,
					FirstTokenTimeoutSec: mc.FirstTokenTimeoutSec,
					CallTimeoutSec:       mc.CallTimeoutSec,
					DeadlineRetryBaseSec: mc.DeadlineRetryBaseSec,
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
			DefaultMaxTurns:        jc.AI.MaxTurns,
			DefaultMaxTokens:       jc.AI.MaxTokens,
			// H4-3 长程档。不填 (0) 时 GlobalConfig.MaxTurnsFor 会回落 Standard,
			// 行为与改造前逐字相同 —— 这个映射漏了不会报错, 只会静默退回未分层,
			// 所以配了 longHorizonMaxTurns 而读不到时, 判据是**执行侧解析出的
			// maxTurns 仍是 standard 值**, 不是"字段有没有被赋值"。
			LongHorizonMaxTurns: jc.AI.LongHorizonMaxTurns,
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
		if bc.ExecutionModel == "" && jc.AI.ExecutionModel != "" {
			bc.ExecutionModel = jc.AI.ExecutionModel
		}
		if bc.PlanModel == "" && jc.AI.PlanModel != "" {
			bc.PlanModel = jc.AI.PlanModel
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
		if ap := jc.AI.AutoPlan; ap != nil {
			if ap.Enabled != nil {
				bc.AutoPlanMode = *ap.Enabled
			}
			if ap.Mode != "" {
				bc.ComplexityMode = ap.Mode
			}
			if ap.AutoAdvisor != nil {
				bc.AutoAdvisor = *ap.AutoAdvisor
			}
		}
	}

	if len(jc.Providers) > 0 {
		bc.Providers = make(map[string]ProviderConfig, len(jc.Providers))
		for name, p := range jc.Providers {
			bc.Providers[name] = p
		}
	}
	if jc.Sandbox != nil {
		cp := *jc.Sandbox
		bc.Sandbox = &cp
	}
	if jc.Advisor != nil && bc.Advisor == nil {
		cp := *jc.Advisor
		bc.Advisor = &cp
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
		if jc.Wiki.APIHost != "" {
			bc.Wiki.APIHost = jc.Wiki.APIHost
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
	// CaRT 停止信号: 原样透传指针 (nil = 不表态, 由各路径按自己的原行为决定)。
	// 这里刻意不给默认值 —— 给一个就把"缺省"与"显式设置"压成了同一个值,
	// 而那正是这个字段要消除的那类静默不对称。
	if jc.Engine != nil {
		bc.EnableStopSignal = jc.Engine.EnableStopSignal
	}
	if jc.Engine != nil {
		if jc.Engine.PromptDebug != nil {
			bc.PromptDebug = *jc.Engine.PromptDebug
		}
		if jc.Engine.PromptDebugDir != "" {
			bc.PromptDebugDir = jc.Engine.PromptDebugDir
		}
		if jc.Engine.PromptDebugMaxFiles > 0 {
			bc.PromptDebugMaxFiles = jc.Engine.PromptDebugMaxFiles
		}
		if jc.Engine.PromptDebugMaxBytes > 0 {
			bc.PromptDebugMaxBytes = jc.Engine.PromptDebugMaxBytes
		}
		if jc.Engine.PromptDebugSampleRate > 0 {
			bc.PromptDebugSampleRate = jc.Engine.PromptDebugSampleRate
		}
		if jc.Engine.PromptDebugRedact != nil {
			bc.PromptDebugRedact = *jc.Engine.PromptDebugRedact
		}
		if jc.Engine.EnvironmentDate != nil {
			bc.EnvironmentDate = *jc.Engine.EnvironmentDate
		}
		// 直接转交指针而不是解引用: 这里唯一的作用是搬运, 解引用会把「没配」压成
		// 「显式 false」, 而两者的区别要一路传到 engine (见字段注释)。
		bc.MemoryIndexMode = jc.Engine.MemoryIndexMode
	}

	if jc.Sync != nil {
		if jc.Sync.KnowledgeRepo != "" {
			bc.Sync.KnowledgeRepo = jc.Sync.KnowledgeRepo
		}
		bc.Sync.IMA = jc.Sync.IMA
		bc.Sync.WeRead = jc.Sync.WeRead
	}
}
