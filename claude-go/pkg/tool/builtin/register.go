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

// RegisterBaseTools 注册全部内置工具, 返回 TodoWriteTool 供外部查询清单。
// 内部自建一份默认 TaskStore; 需要与外部共享同一份 store 时改用 RegisterBaseToolsWithStore。
//
// RegisterBaseTools registers all built-in tools.
// Returns TodoWriteTool for external use.
// Internally creates a default TaskStore; use RegisterBaseToolsWithStore to share one.
func RegisterBaseTools(reg *tool.Registry, searcher WebSearcher) *TodoWriteTool {
	todo, _ := RegisterBaseToolsWithStore(reg, nil, searcher)
	return todo
}

// RegisterBaseToolsWithStore 用共享的 TaskStore 注册全部内置工具, 并把 (可能新建的)
// store 回传给调用方复用。
//
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
	reg.Register(NewGitInspectTool())

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
	// 结构化行情数据面 (时点/复权/多源核价), 与 WebSearch 的叙述性检索分工。
	registerMarketDataTools(reg)

	// 排障只读取证 (MySQL 域)。放在 base 集是为了让 admin 档也拥有同一份工具目录，
	// 避免"analysis 档有、admin 档没有"的目录分叉。
	reg.Register(NewOpsInvestigateTool())

	// User interaction
	reg.Register(NewAskUserQuestionTool())

	// Plan mode
	reg.Register(NewEnterPlanModeTool())
	reg.Register(NewExitPlanModeTool())

	// V2 Task system (shared store)
	// store 为 nil 表示"由本函数自建一份"并回传 —— 四个 Task 工具必须拿到同一个 store,
	// 否则 TaskCreate 写进去的任务 TaskGet/TaskList 永远看不到。
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
