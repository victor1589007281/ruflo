// Package agent 实现 Agent/Task 子代理系统。
// 对应 TS 源码: review/claude/src/tools/AgentTool/AgentTool.tsx
//
//	review/claude/src/tools/AgentTool/runAgent.ts
//
// Agent 工具是 Claude Code 最强大的功能之一:
//   - LLM 可以通过 "Agent" 工具派生子代理
//   - 子代理拥有独立的对话循环 (嵌套 queryLoop)
//   - 子代理可以拥有自己的工具集 (工具过滤)
//   - 子代理可以连接额外的 MCP 服务器
//   - 子代理执行完毕后，结果返回给父循环
//
// Agent Teams: 见 pkg/agent/teams.go (ProductionTeam) 与 pkg/agent/mailbox.go
// (PersistentMailbox)。本文件只负责子代理派生, 不再自带一套团队抽象。
//
// 旧名称: "Task" (现在 "Task" 是 LEGACY_AGENT_TOOL_NAME)
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	AgentToolName  = "Agent"
	LegacyToolName = "Task" // 向后兼容
)

// agentInput Agent 工具的输入
type agentInput struct {
	Prompt       string `json:"prompt"`
	Description  string `json:"description,omitempty"`
	SubagentType string `json:"subagent_type,omitempty"`
	Model        string `json:"model,omitempty"`
	ReadOnly     bool   `json:"readonly,omitempty"`
	Resume       string `json:"resume,omitempty"` // 恢复之前的代理
}

// RunAgentFunc 子代理执行函数的签名
// 外部注入，用于避免循环依赖 (engine → agent → engine)
type RunAgentFunc func(ctx context.Context, prompt string, opts RunOptions) (string, error)

// RunOptions 子代理运行选项
type RunOptions struct {
	SubagentType string
	Model        string
	ReadOnly     bool
	ParentID     types.AgentID
	// MaxTurns 子代理最大循环回合数; 0 = 不限制 (沿用主会话 Config.MaxTurns)。
	// delegate_task 用它给每个子代理兜底, 防跑量任务失控。
	MaxTurns int
}

// AgentTool Agent/Task 工具实现
type AgentTool struct {
	runAgent RunAgentFunc
	// trace 图外派生的轨迹底座 (design/01 §4.8, 见 subagent_span.go)。
	// 可空 —— 未注入时整套可见性采集是零成本 no-op, 行为与改造前一字不变。
	trace *tracestore.Store
}

// NewAgentTool 创建 Agent 工具
func NewAgentTool(runFn RunAgentFunc) *AgentTool {
	return &AgentTool{runAgent: runFn}
}

var _ tool.AliasedTool = (*AgentTool)(nil)

// Name 返回工具名 (Agent)。
func (t *AgentTool) Name() string { return AgentToolName }

// Aliases 返回 API 侧接受的旧工具名 (如 "Task"), 供别名解析做向后兼容。
// Aliases returns legacy tool names accepted by the API (e.g. "Task").
func (t *AgentTool) Aliases() []string { return []string{LegacyToolName} }

// Description Agent 工具的描述, **含当前可用的 agent 清单** (21.8.12 P5)。
//
// 为什么把清单拼进描述而不是另加一个工具: 描述是模型的唯一入口 —— 模型看得到什么
// 名字, 才可能填对 subagent_type。改造前这里是一段写死的英文, 一个名字都没有,
// 于是模型只能"猜一个像样的" (P5 记录的缺口 B)。
//
// 清单为空 (没装资产体系) 时逐字节等于改造前的那段 —— 空资产体系不该改变任何
// 既有请求。这是判据而不是口号: 大部分部署上 assets 目录是空的。
func (t *AgentTool) Description() string {
	const base = `Launch a new agent to handle complex, multi-step tasks autonomously.
The agent runs in a separate context and returns results when complete.`
	if m := AgentManifest(); m != "" {
		return base + m
	}
	return base
}

// InputSchema 返回 Agent 工具的输入 JSON Schema (只有 prompt 必填)。
func (t *AgentTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"prompt": {"type": "string", "description": "The task for the agent to perform."},
			"description": {"type": "string", "description": "A short description of the task."},
			"subagent_type": {"type": "string", "description": "Subagent type to use."},
			"model": {"type": "string", "description": "Optional model to use."},
			"readonly": {"type": "boolean", "description": "If true, agent runs in read-only mode."},
			"resume": {"type": "string", "description": "Agent ID to resume from."}
		},
		"required": ["prompt"]
	}`)
}

// IsReadOnly 报告本工具是否只读 —— 子代理会改文件, 恒为 false。
func (t *AgentTool) IsReadOnly(_ json.RawMessage) bool        { return false }
// IsConcurrencySafe 报告本工具能否并发调用 —— 子代理并发会争抢同一工作区, 恒为 false。
func (t *AgentTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }
// CheckPermissions 返回权限判定 —— 返回 nil 表示不额外拦截, 交由上层权限模式决定。
func (t *AgentTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 执行 Agent 工具调用 — 派生子代理。
// 对应 TS: AgentTool.tsx 中的 call() → runAgent.ts 中的 runAgent()
//
// 流程:
//  1. 解析输入
//  2. 创建子代理上下文 (createSubagentContext)
//  3. 调用 runAgent (本质是嵌套的 queryLoop)
//  4. 收集子代理的所有输出
//  5. 返回最终结果
func (t *AgentTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in agentInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	if t.runAgent == nil {
		return &tool.ToolResult{Content: "Agent 运行函数未配置", IsError: true}, nil
	}

	// 图外派生的可见性收编 (design/01 §4.8, 见 subagent_span.go)。
	//
	// 挂在**这里**而不是各 runNestedAgent 里: 全仓每一次子代理派生都必经本方法
	// (feishu/session.go:390,621 与 main.go:3065 的 RunAgentFunc 唯一调用方就是它),
	// 挂一处就覆盖两条路径, 且将来新增入口自动获得同样的可见性。分别在两个
	// runNestedAgent 里各写一遍则必然漂移 —— 那正是 §1.2 记的那种漂移形态。
	start := time.Now()
	tokensBefore := subagentTokensBefore(ctx)
	depth := SubagentDepth(ctx) + 1
	ctx = withSubagentDepth(ctx, depth)

	result, err := t.runAgent(ctx, in.Prompt, RunOptions{
		SubagentType: in.SubagentType,
		Model:        in.Model,
		ReadOnly:     in.ReadOnly,
		ParentID:     tctx.AgentID,
	})
	// 成败两路都要记: 失败的派生同样烧了 token、同样占了墙钟, 只记成功的会让
	// "为什么这个节点跑了 8 分钟才产出两行"永远解释不了。
	observeSubagent(t.trace, ctx, in, depth, result, err, start, tokensBefore)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("Agent 执行失败: %v", err), IsError: true}, nil
	}

	return &tool.ToolResult{Content: result}, nil
}

// ============================================================================
// Agent Teams (团队管理)
// ============================================================================
//
// 这里曾经有一整套并行的团队抽象: Team / TeamMember / TeamManager + 2 参版
// CreateTeam / DeleteTeam / ListTeams, 以及它的 Team.Mailbox []TeamMessage 与
// SendMessage / GetMessages。2026-09-25 整体删除 (21.8.9-D6 收敛)。
//
// 删掉的理由不是"没接线", 是三件叠加:
//
//  1. **零调用方**。NewTeamManager() 全仓零调用; 2 参 CreateTeam(name, description)
//     与真实在用的 ProductionTeamManager.CreateTeam(name, workflow, objective, chatID)
//     参数个数都不同, 不存在"换个名字被调到"的可能。
//  2. **重名而同形不同义**。它叫 Team, 生产在用的叫 ProductionTeam; 它叫 TeamManager,
//     在用的叫 ProductionTeamManager。读代码的人无法从名字判断哪个是活的 —— 这正是
//     本轮"四份邮箱"乱象的生成机制: 一份死抽象留在原地, 下一个人会照着它再抄一份。
//  3. **它自带的那份邮箱是坏的**。Team.Mailbox 是裸 slice, GetMessages 每次全量返回,
//     没有"只投递一次"的游标语义 —— 重跑/精修阶段会把老消息重复拼进 prompt。
//
// 全仓的团队与信箱唯一实现: pkg/agent/teams.go (ProductionTeam/ProductionTeamManager)
// 与 pkg/agent/mailbox.go (PersistentMailbox)。见 21.8.9-D6。
