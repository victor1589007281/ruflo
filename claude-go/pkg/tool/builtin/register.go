package builtin

import (
	"context"

	"github.com/anthropic/claude-go/pkg/computeruse"
	"github.com/anthropic/claude-go/pkg/tool"
)

// WebSearcher 网页搜索接口，与 browser.Client.Search 兼容。
type WebSearcher interface {
	Search(ctx context.Context, query string) (title, text, html string, err error)
}

// RegisterBaseTools registers all built-in tools.
// Returns TodoWriteTool for external use.
// Internally creates a default TaskStore; use RegisterBaseToolsWithStore to share one.
func RegisterBaseTools(reg *tool.Registry, searcher WebSearcher) *TodoWriteTool {
	todo, _ := RegisterBaseToolsWithStore(reg, nil, searcher)
	return todo
}

// RegisterBaseToolsWithStore registers all built-in tools with a shared TaskStore.
// If store is nil, creates a new one with default path.
// Returns the TodoWriteTool and the (possibly newly created) TaskStore for reuse.
func RegisterBaseToolsWithStore(reg *tool.Registry, store *TaskStore, searcher WebSearcher) (*TodoWriteTool, *TaskStore) {
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

	// Media generation (闭环输出: SVG/HTML→PNG, HTML→GIF, Python→mp4, slides→PPTX, 文本→WAV)
	reg.Register(NewGenerateImageTool())
	reg.Register(NewGenerateGIFTool())
	reg.Register(NewGenerateVideoTool())
	reg.Register(NewGeneratePPTXTool())
	reg.Register(NewGenerateChartTool())
	reg.Register(NewGenerateSpeechTool())

	// Web tools
	reg.Register(NewWebFetchTool())
	reg.Register(NewWebSearchTool(searcher))
	reg.Register(NewKLineTool())
	reg.Register(NewQuoteTool())

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

	// Code Intelligence (大型代码库知识图谱)
	reg.Register(NewCodeIntelInitTool())
	reg.Register(NewCodeIntelUpdateTool())
	reg.Register(NewCodeIntelStatusTool())
	reg.Register(NewCodeIntelQueryTool())
	reg.Register(NewCodeIntelBranchTool())

	// Computer Use (截屏/鼠标/键盘控制)
	computeruse.RegisterAll(reg, nil)

	return todoTool, store
}
