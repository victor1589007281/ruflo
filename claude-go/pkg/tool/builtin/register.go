package builtin

import "github.com/anthropic/claude-go/pkg/tool"

// RegisterBaseTools registers all built-in tools.
// Returns TodoWriteTool for external use.
// Internally creates a default TaskStore; use RegisterBaseToolsWithStore to share one.
func RegisterBaseTools(reg *tool.Registry) *TodoWriteTool {
	todo, _ := RegisterBaseToolsWithStore(reg, nil)
	return todo
}

// RegisterBaseToolsWithStore registers all built-in tools with a shared TaskStore.
// If store is nil, creates a new one with default path.
// Returns the TodoWriteTool and the (possibly newly created) TaskStore for reuse.
func RegisterBaseToolsWithStore(reg *tool.Registry, store *TaskStore) (*TodoWriteTool, *TaskStore) {
	// Core file tools
	reg.Register(NewFileReadTool())
	reg.Register(NewFileWriteTool())
	reg.Register(NewFileEditTool())

	// Search tools
	reg.Register(NewGlobTool())
	reg.Register(NewGrepTool())

	// Shell
	reg.Register(NewBashTool())

	// Todo (V1)
	todoTool := NewTodoWriteTool()
	reg.Register(todoTool)

	// Web tools
	reg.Register(NewWebFetchTool())
	reg.Register(NewWebSearchTool())

	// User interaction
	reg.Register(NewAskUserQuestionTool())

	// Plan mode
	reg.Register(NewEnterPlanModeTool())
	reg.Register(NewExitPlanModeTool())

	// V2 Task system (shared store)
	if store == nil {
		store = NewTaskStore("")
	}
	reg.Register(NewTaskCreateTool(store))
	reg.Register(NewTaskGetTool(store))
	reg.Register(NewTaskUpdateTool(store))
	reg.Register(NewTaskListTool(store))

	// Team tools
	ts := NewTeamStore()
	reg.Register(NewSendMessageTool(ts))
	reg.Register(NewTeamCreateTool(ts))
	reg.Register(NewTeamDeleteTool(ts))

	// Background task I/O
	taskReg := NewBackgroundTaskRegistry()
	reg.Register(NewTaskOutputTool(taskReg))
	reg.Register(NewTaskStopTool(taskReg))

	// Advanced tools
	reg.Register(NewLSPTool())
	reg.Register(NewConfigTool())
	reg.Register(NewSkillTool())
	reg.Register(NewStructuredOutputTool())
	reg.Register(NewToolSearchTool(reg))

	// Worktree
	reg.Register(NewEnterWorktreeTool())
	reg.Register(NewExitWorktreeTool())

	// Cron
	reg.Register(NewCronCreateTool())
	reg.Register(NewCronDeleteTool())
	reg.Register(NewCronListTool())

	return todoTool, store
}
