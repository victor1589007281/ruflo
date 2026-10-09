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
	// Core file tools. 写工具走 registerFileMutationTools 而不是各写一行 ——
	// 回滚出口 (H3-1) 必须与写工具同进同出, 抄成两份迟早会分叉成
	// "有的档位能改不能撤", 那种分叉没有任何一处会报错。
	reg.Register(NewFileReadTool())
	registerFileMutationTools(reg)

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
	//
	// TeamMailbox 已移除 (2026-09-25)：它把消息写进一个**零读者**的 map 却回"已投递"，
	// 是**对模型撒谎**而非未接线 —— 模型会因此不再用别的方式传达。全仓信箱收敛为
	// pkg/agent.PersistentMailbox 一份 (见 pkg/agent/mailbox.go 与 21.8.9-D6)。
	ts := NewTeamStore()
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

	// 进化操作台 (design/03 §4.7): agent 自助编排进化实验的 evo_* 七件套。
	// 放在 admin/base 集里而不是每个档位: 做进化实验是运维/自省动作, 不该出现在
	// 每个团队角色的工具 schema 里 (那是纯粹的 token 浪费)。
	RegisterEvolutionTools(reg)

	// Computer Use (截屏/鼠标/键盘控制)
	computeruse.RegisterAll(reg, nil)

	return todoTool, store
}
