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
	ToolProfileAdmin    ToolProfile = "admin"
)

// NormalizeToolProfile returns a safe profile default.
func NormalizeToolProfile(profile ToolProfile) ToolProfile {
	switch profile {
	case ToolProfileChat, ToolProfileResearch, ToolProfileCoding, ToolProfileTeam, ToolProfileAdmin:
		return profile
	default:
		return ToolProfileChat
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
		reg.Register(NewToolSearchTool(reg))
		return nil, store
	case ToolProfileResearch:
		registerReadSearchTools(reg)
		todoTool := NewTodoWriteTool()
		reg.Register(todoTool)
		registerWebAndInteractionTools(reg, searcher)
		reg.Register(NewEnterPlanModeTool())
		reg.Register(NewExitPlanModeTool())
		reg.Register(NewToolSearchTool(reg))
		return todoTool, store
	case ToolProfileTeam:
		registerReadSearchTools(reg)
		todoTool := NewTodoWriteTool()
		reg.Register(todoTool)
		reg.Register(NewAskUserQuestionTool())
		reg.Register(NewStructuredOutputTool())
		reg.Register(NewToolSearchTool(reg))
		return todoTool, store
	case ToolProfileCoding:
		fallthrough
	default:
		registerFileMutationTools(reg)
		registerReadSearchTools(reg)
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
}

func registerReadSearchTools(reg *tool.Registry) {
	reg.Register(NewFileReadTool())
	reg.Register(NewGlobTool())
	reg.Register(NewGrepTool())
}

func registerWebAndInteractionTools(reg *tool.Registry, searcher WebSearcher) {
	reg.Register(NewWebFetchTool())
	reg.Register(NewWebSearchTool(searcher))
	reg.Register(NewAskUserQuestionTool())
}
