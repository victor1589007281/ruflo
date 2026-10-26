package builtin

import "github.com/anthropic/claude-go/pkg/tool"

// ToolProfile controls how much of the built-in tool surface is exposed to an
// LLM call. Keeping narrow profiles is the first line of defense against prompt
// schema bloat in chat-style entrypoints.
type ToolProfile string

const (
	ToolProfileChat     ToolProfile = "chat"
	ToolProfileResearch ToolProfile = "research"
	ToolProfileCoding   ToolProfile = "coding"
	ToolProfileTeam     ToolProfile = "team"
	ToolProfileAnalysis ToolProfile = "analysis" // 只读分析/调研角色: 最精简工具集, 降低每轮工具 schema 体积
	ToolProfileAdmin    ToolProfile = "admin"
)

// NormalizeToolProfile 把未知档位归一为 chat, 保证返回值总是合法档位。
// NormalizeToolProfile returns a safe profile default.
func NormalizeToolProfile(profile ToolProfile) ToolProfile {
	switch profile {
	case ToolProfileChat, ToolProfileResearch, ToolProfileCoding, ToolProfileTeam, ToolProfileAnalysis, ToolProfileAdmin:
		return profile
	default:
		return ToolProfileChat
	}
}

// ValidToolProfile reports whether the value is one of the profiles defined here.
//
// 存在的理由只有一个: NormalizeToolProfile 对**拼错的**档位静默返回 chat, 且不留痕。
// 调用方在"声明来自外部 (资产 frontmatter / 平台下发)"的那条路上必须先问这一句 ——
// `tool-profile: codign` 会得到一个连 Read 都没有的子代理, 而日志上一个字都没有,
// 从外面看与"这个角色本来就不需要文件工具"完全一样。
//
// 与 Normalize 的分工: 内部枚举/常量传参用 Normalize (它不会错), 外来字符串先
// 过 Valid 再决定"作废并留痕"还是"接受并归一"。
func ValidToolProfile(profile ToolProfile) bool {
	switch profile {
	case ToolProfileChat, ToolProfileResearch, ToolProfileCoding, ToolProfileTeam, ToolProfileAnalysis, ToolProfileAdmin:
		return true
	default:
		return false
	}
}

// RegisterProfileToolsWithStore 按档位注册受限的内置工具集; admin 刻意保留历史的全量行为。
//
// 算法解读：
//
//	原理：档位 → 工具集的映射。窄档位是对 prompt schema 膨胀的第一道防线: 每轮都要重发全部
//	      工具 schema, 工具越多越贵, 也越容易让模型选错。
//	关键不变量：未识别的档位经 NormalizeToolProfile 归一为 chat, 而 chat 是**很窄**的档位;
//	      因此"拼错的档位"不会意外放开写/执行工具 (fail-closed 方向)。
//	代价与边界：admin 是唯一不做裁剪的档位, 它保留全量工具 (含写与 Bash)。
//	设计权衡：各档位手写枚举而不是用"基础集 + 差集", 牺牲一点重复, 换来"这一档到底有哪些
//	      工具"可以逐行读出来。
//
// RegisterProfileToolsWithStore registers a bounded built-in tool set for the
// given profile. Admin intentionally preserves the historical all-tools behavior.
func RegisterProfileToolsWithStore(reg *tool.Registry, store *TaskStore, searcher WebSearcher, profile ToolProfile) (*TodoWriteTool, *TaskStore) {
	switch NormalizeToolProfile(profile) {
	case ToolProfileAdmin:
		return RegisterBaseToolsWithStore(reg, store, searcher)
	case ToolProfileChat:
		reg.Register(NewAskUserQuestionTool())
		registerCodeIntelReadTools(reg) // 精准代码定位 (GitNexus/Graphify), 零 LLM token 建索引
		reg.Register(NewToolSearchTool(reg))
		return nil, store
	case ToolProfileResearch:
		registerReadSearchTools(reg)
		registerCodeIntelTools(reg)
		todoTool := NewTodoWriteTool()
		reg.Register(todoTool)
		registerWebAndInteractionTools(reg, searcher)
		registerMarketDataTools(reg)
		reg.Register(NewEnterPlanModeTool())
		reg.Register(NewExitPlanModeTool())
		reg.Register(NewToolSearchTool(reg))
		return todoTool, store
	case ToolProfileTeam:
		registerReadSearchTools(reg)
		registerCodeIntelTools(reg)
		todoTool := NewTodoWriteTool()
		reg.Register(todoTool)
		reg.Register(NewAskUserQuestionTool())
		reg.Register(NewStructuredOutputTool())
		reg.Register(NewToolSearchTool(reg))
		return todoTool, store
	case ToolProfileAnalysis:
		// 只读分析/调研角色 (source-analyst / tech-investigator / reviewer / critic / fact-checker):
		// 只给"定位+读取+联网调研"工具, 去掉 写/编辑/Bash/建索引/TodoWrite/Cron/Worktree 等,
		// 显著缩小每轮重发的工具 schema。
		registerReadSearchTools(reg)    // Read/Glob/Grep
		registerCodeIntelReadTools(reg) // CodeIntelQuery/Status (只读, 精准定位代替地毯式扫描)
		reg.Register(NewWebFetchTool())
		reg.Register(NewWebSearchTool(searcher))
		registerMarketDataTools(reg)
		// 排障取数: 只读、固定采集 SQL、DSN 由运维白名单提供。放在 analysis 档
		// 是因为它本身就是"只读取证"动作, 不引入写/执行能力。
		reg.Register(NewOpsInvestigateTool())
		reg.Register(NewStructuredOutputTool())
		reg.Register(NewToolSearchTool(reg))
		return nil, store
	case ToolProfileCoding:
		fallthrough
	default:
		registerFileMutationTools(reg)
		registerReadSearchTools(reg)
		registerCodeIntelTools(reg)
		reg.Register(NewBashTool())
		todoTool := NewTodoWriteTool()
		reg.Register(todoTool)
		reg.Register(NewAskUserQuestionTool())
		reg.Register(NewEnterPlanModeTool())
		reg.Register(NewExitPlanModeTool())
		taskReg := NewBackgroundTaskRegistry()
		reg.Register(NewTaskOutputTool(taskReg))
		reg.Register(NewTaskStopTool(taskReg))
		reg.Register(NewStructuredOutputTool())
		reg.Register(NewToolSearchTool(reg))
		return todoTool, store
	}
}

// registerFileMutationTools 注册写/编辑/回退三个文件改动工具。
func registerFileMutationTools(reg *tool.Registry) {
	reg.Register(NewFileWriteTool())
	reg.Register(NewFileEditTool())
	// 回滚出口与写工具同档: 能改文件的地方才需要能撤 —— 只读档位 (research /
	// analysis / team) 注册它只会白白多出一份 schema, 而它们没有任何东西可退。
	reg.Register(NewRewindTool())
}

// registerReadSearchTools 注册 Read/Glob/Grep 三个只读检索工具, 外加受 scope
// 约束的 GitInspect。GitInspect 放在这一组而不是 admin 档: "看未提交内容"
// 是分析类角色的正常需求, 而它固定的三个只读动词不会把 Bash 的能力带回来。
func registerReadSearchTools(reg *tool.Registry) {
	reg.Register(NewFileReadTool())
	reg.Register(NewGlobTool())
	reg.Register(NewGrepTool())
	reg.Register(NewGitInspectTool())
}

// registerCodeIntelReadTools 暴露只读的代码智能工具 (查询 + 状态),
// 让 agent 用知识图谱精准定位代码 (执行流/符号/调用关系/impact),
// 取代对大仓库的 Read/Grep/Glob 地毯式扫描。建索引由外部 CLI 完成, 不耗 LLM token。
func registerCodeIntelReadTools(reg *tool.Registry) {
	reg.Register(NewCodeIntelQueryTool())
	reg.Register(NewCodeIntelStatusTool())
}

// registerCodeIntelTools 在只读工具基础上额外暴露建/更新索引工具,
// 供具备代码读写能力的角色 (research/team/coding) 在索引缺失时自助构建。
func registerCodeIntelTools(reg *tool.Registry) {
	registerCodeIntelReadTools(reg)
	reg.Register(NewCodeIntelInitTool())
	reg.Register(NewCodeIntelUpdateTool())
}

// registerWebAndInteractionTools 注册联网抓取/搜索与提问交互工具。
func registerWebAndInteractionTools(reg *tool.Registry, searcher WebSearcher) {
	reg.Register(NewWebFetchTool())
	reg.Register(NewWebSearchTool(searcher))
	reg.Register(NewAskUserQuestionTool())
}
