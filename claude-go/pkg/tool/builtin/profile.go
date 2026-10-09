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

func registerFileMutationTools(reg *tool.Registry) {
	reg.Register(NewFileWriteTool())
	reg.Register(NewFileEditTool())
	// 回滚出口与写工具同档: 能改文件的地方才需要能撤 —— 只读档位 (research /
	// analysis / team) 注册它只会白白多出一份 schema, 而它们没有任何东西可退。
	reg.Register(NewRewindTool())
}

func registerReadSearchTools(reg *tool.Registry) {
	reg.Register(NewFileReadTool())
	reg.Register(NewGlobTool())
	reg.Register(NewGrepTool())
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

func registerWebAndInteractionTools(reg *tool.Registry, searcher WebSearcher) {
	reg.Register(NewWebFetchTool())
	reg.Register(NewWebSearchTool(searcher))
	reg.Register(NewAskUserQuestionTool())
}
