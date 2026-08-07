package agent

// delegate_tool.go —— 子代理分解工具 (Path B)。
//
// 背景: Path B 把工具消费回合路由到快速执行模型, 但**最终综合回合也在执行模型上**,
// 工具循环越长的场景, 回答质量被小模型拉低。子代理分解补上这块:
//
//	规划方 (主模型) 用 delegate_task 把工具密集/可并行的子任务拆给独立子代理,
//	在快速执行模型上跑 (全新上下文 + 自带工具循环, 看不到本对话历史),
//	子代理报告回合经路由例外 (engine.isDelegationResultTurn) 回主模型综合。
//
// 与 AgentTool 的区别:
//   - **模型固定**: 从 tctx.ExecutionModel 取执行模型 (回退 MainLoopModel),
//     不依赖 LLM 手传 model 参数 —— 小模型易漏传, 漏了就跑回主模型, 违背"跑量用快模型"。
//   - **并行委派**: tasks 数组一次委派多个独立子任务, Call 内部并发执行 (信号量≤4)。
//   - **回合兜底**: max_turns 钳制每个子代理的循环轮数, 防跑量任务失控。
//
// 工具层 IsConcurrencySafe=false (与 AgentTool 一致): 并行在 Call 内部做,
// 避免 RunTools 跨调用并发共享状态 (runAgent/trace/hookRunner) 的竞态。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	// maxSubagentDepth 委派递归深度上限 (防失控递归)。
	maxSubagentDepth = 5
	// maxDelegateConcurrency 单次 delegate_task 调用内并行子代理上限。
	maxDelegateConcurrency = 4
	// defaultDelegateMaxTurns 每个子代理默认最大回合数。
	defaultDelegateMaxTurns = 40
)

// delegateInput delegate_task 工具的输入。task 与 tasks 二选一 (Call 里校验)。
type delegateInput struct {
	Task     string   `json:"task,omitempty"`
	Tasks    []string `json:"tasks,omitempty"`
	MaxTurns int      `json:"max_turns,omitempty"`
	ReadOnly bool     `json:"readonly,omitempty"`
}

// DelegateTool 子代理分解工具。
type DelegateTool struct {
	runAgent RunAgentFunc
	trace    *tracestore.Store
	// maxTurnsCap 每个子代理回合数兜底; <=0 时用 defaultDelegateMaxTurns。
	maxTurnsCap int
}

// NewDelegateTool 创建 delegate_task 工具。
func NewDelegateTool(runFn RunAgentFunc, cap int) *DelegateTool {
	return &DelegateTool{runAgent: runFn, maxTurnsCap: cap}
}

// NewDelegateToolWithTrace 带轨迹采集的 delegate_task 工具。
func NewDelegateToolWithTrace(runFn RunAgentFunc, ts *tracestore.Store, cap int) *DelegateTool {
	return &DelegateTool{runAgent: runFn, trace: ts, maxTurnsCap: cap}
}

// SetTraceStore 注入轨迹底座 (装配期调用; nil = 不采集, 行为不变)。
func (t *DelegateTool) SetTraceStore(ts *tracestore.Store) {
	if t != nil {
		t.trace = ts
	}
}

var _ tool.Tool = (*DelegateTool)(nil)

func (t *DelegateTool) Name() string { return tool.DelegateToolName }

func (t *DelegateTool) Description() string {
	return `Delegate tool-heavy or parallelizable sub-tasks to isolated sub-agents running on the fast execution model. Each sub-agent gets a fresh context and its own tool loop, but cannot see this conversation. You decompose the task and synthesize the results yourself. Pass multiple independent sub-tasks in "tasks" to run them in parallel.`
}

func (t *DelegateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task": {"type": "string", "description": "单个子任务描述 (与 tasks 二选一)。必须自包含: 子代理看不到本对话历史, 描述需含目标+约束+验收标准。"},
			"tasks": {"type": "array", "items": {"type": "string"}, "description": "多个相互独立的子任务, 每个由独立子代理并行执行 (与 task 二选一)。每个子任务同样必须自包含。"},
			"max_turns": {"type": "integer", "description": "每个子代理最大回合数 (默认 40)。"},
			"readonly": {"type": "boolean", "description": "子代理是否只读 (默认 false)。"}
		},
		"required": []
	}`)
}

// IsConcurrencySafe 返回 false: 并行在 Call 内部以信号量控制, 工具层串行。
func (t *DelegateTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *DelegateTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *DelegateTool) IsReadOnly(input json.RawMessage) bool {
	var in delegateInput
	if json.Unmarshal(input, &in) == nil {
		return in.ReadOnly
	}
	return false
}

// Call 执行委派: 把 task/tasks 交给独立子代理 (执行模型) 运行, 结果按序拼接。
func (t *DelegateTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in delegateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if t.runAgent == nil {
		return &tool.ToolResult{Content: "delegate_task 运行函数未配置", IsError: true}, nil
	}

	// 执行模型: 优先 tctx.ExecutionModel (Path B), 回退当前回合主循环模型。
	executorModel := tctx.ExecutionModel
	if executorModel == "" {
		executorModel = tctx.MainLoopModel
	}
	if executorModel == "" {
		return &tool.ToolResult{Content: "delegate_task 无法确定执行模型: 未配置 ExecutionModel 且主循环模型为空", IsError: true}, nil
	}

	maxTurns := in.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultDelegateMaxTurns
	}
	turnCap := t.maxTurnsCap
	if turnCap <= 0 {
		turnCap = defaultDelegateMaxTurns
	}
	if maxTurns > turnCap {
		maxTurns = turnCap
	}

	// 深度守卫: 防委派失控递归 (与 AgentTool.Call 同口径)。
	depth := SubagentDepth(ctx) + 1
	if depth > maxSubagentDepth {
		return &tool.ToolResult{Content: fmt.Sprintf("delegate_task 已达最大递归深度 %d", maxSubagentDepth), IsError: true}, nil
	}
	ctx = withSubagentDepth(ctx, depth)

	tasks := in.Tasks
	if in.Task != "" {
		tasks = append([]string{in.Task}, tasks...)
	}
	if len(tasks) == 0 {
		return &tool.ToolResult{Content: "delegate_task 需要 task 或 tasks (至少一个子任务)", IsError: true}, nil
	}

	results := make([]string, len(tasks))
	sem := make(chan struct{}, maxDelegateConcurrency)
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(i int, task string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			tokensBefore := subagentTokensBefore(ctx)
			out, err := t.runAgent(ctx, task, RunOptions{
				Model:    executorModel,
				ReadOnly: in.ReadOnly,
				ParentID: tctx.AgentID,
				MaxTurns: maxTurns,
			})
			observeSubagent(t.trace, ctx, agentInput{Prompt: task, Model: executorModel, ReadOnly: in.ReadOnly}, depth, out, err, start, tokensBefore)
			if err != nil {
				results[i] = fmt.Sprintf("### 子任务 %d/%d 执行失败: %v", i+1, len(tasks), err)
				return
			}
			results[i] = fmt.Sprintf("### 子任务 %d/%d\n%s", i+1, len(tasks), out)
		}(i, task)
	}
	wg.Wait()

	return &tool.ToolResult{Content: strings.Join(results, "\n\n")}, nil
}
