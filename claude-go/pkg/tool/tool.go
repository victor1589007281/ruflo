// Package tool 定义了工具系统的核心接口和注册表。
// 对应 TS 源码: review/claude/src/Tool.ts
//
// 设计思路:
// Claude Code 的工具系统基于 Tool<Input, Output> 泛型接口，
// 每个工具定义自己的 inputSchema (Zod)、call()、checkPermissions()、
// isConcurrencySafe() 等。Go 中无法完全复刻 TS 泛型 + Zod，
// 改用 interface + json.RawMessage 实现相同的运行时行为。
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/types"
)

// DelegateToolName 子代理分解工具名 (Path B)。
// 规划方 (主模型) 用它把工具密集/可并行的子任务拆给在快速执行模型上运行的
// 独立子代理执行。engine 与 agent 包都引用它 —— 放在 tool 包避免 agent↔engine 环。
const DelegateToolName = "delegate_task"

// delegationTools 委派工具集合: 其 tool_result 消费回合应回主模型综合,
// 不应路由到快速执行模型 (路由例外见 engine.isDelegationResultTurn)。
var delegationTools = map[string]bool{
	DelegateToolName: true,
	"Agent":          true,
	"Task":           true, // Agent 的遗留别名
}

// IsDelegationTool 判定工具名是否属于子代理委派类。
func IsDelegationTool(name string) bool { return delegationTools[name] }

// GlobalPermissionChecker 全局权限与单工具 CheckPermissions 的合并入口；
// 由 pkg/permissions.Checker 实现。
type GlobalPermissionChecker interface {
	CheckGlobal(toolName string, input json.RawMessage, isReadOnly bool, toolPerm *types.PermissionResult) types.PermissionResult
	AddSessionAllowRule(toolName string)
}

// Tool 定义一个可被 LLM 调用的工具。
// 对应 TS: Tool.ts 中的 Tool<Input, Output, P> 类型。
//
// 每个工具实现此接口即可被注册到工具池中。
// queryLoop 通过 Name() 匹配 LLM 返回的 tool_use.name，
// 通过 Call() 执行工具，并将结果作为 tool_result 返回给 LLM。
type Tool interface {
	// Name 返回工具名称 (LLM 调用时使用此名称)
	Name() string

	// Description 返回工具描述 (用于系统提示词中)
	Description() string

	// InputSchema 返回 JSON Schema (用于 API tools 参数)
	InputSchema() json.RawMessage

	// Call 执行工具。input 是 LLM 生成的 JSON 输入，
	// 返回 ToolResult (文本结果 + 是否错误)。
	// 对应 TS: tool.call(toolInput, context)
	Call(ctx context.Context, input json.RawMessage, tctx *ToolContext) (*ToolResult, error)

	// IsConcurrencySafe 返回此工具对于给定输入是否可以并发执行。
	// 对应 TS: tool.isConcurrencySafe?.(parsedInput, context)
	// 只读操作 (如 FileRead, Glob, Grep) 通常是并发安全的，
	// 写入操作 (如 FileWrite, Bash) 必须串行执行。
	// 这是 toolOrchestration.ts 中 partitionToolCalls 的核心判断依据。
	IsConcurrencySafe(input json.RawMessage) bool

	// CheckPermissions 检查此工具在给定输入下是否需要权限确认。
	// 返回 nil 表示 "allow" (无需确认)。
	// 对应 TS: tool.checkPermissions?.(toolInput, context)
	CheckPermissions(input json.RawMessage, tctx *ToolContext) *types.PermissionResult

	// IsReadOnly 返回此工具在给定输入下是否是只读的 (用于权限分类)。
	// input 可为 nil（例如仅列出工具时）；实现方应对 nil 做保守默认。
	IsReadOnly(input json.RawMessage) bool
}

// AliasedTool 可选接口：定义工具别名。
// 对应 TS: Tool.ts 中的 aliases?: string[]
// 如果工具实现此接口，Registry.Get 也会通过别名匹配。
type AliasedTool interface {
	Aliases() []string
}

// ToolResult 工具执行结果。
// 对应 TS: Tool.ts 中的 ToolResult<T>。
type ToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
	// Images 可选图像附件 (如 FileRead 读取图片文件)。非空时 RunToolUse 会在 tool_result
	// 块之后以 image 内容块追加到同一条 user 消息 (Anthropic 多模态格式), 供视觉模型直接查看。
	Images []types.MediaSource `json:"images,omitempty"`
}

// ToolContext 工具执行时的上下文环境。
// 对应 TS: Tool.ts 中的 ToolUseContext (简化版)。
// 包含当前工作目录、权限模式、消息历史等运行时信息。
type ToolContext struct {
	Cwd                string               `json:"cwd"`
	PermissionMode     types.PermissionMode `json:"permissionMode"`
	AbortCh            <-chan struct{}      `json:"-"` // 对应 TS: abortController.signal
	Messages           []types.Message      `json:"messages,omitempty"`
	SessionID          string               `json:"sessionId,omitempty"` // 会话级状态工具 (plan mode) 用
	PlanFileDir        string               `json:"planFileDir,omitempty"` // 计划文件目录: ExitPlanMode 将计划落盘, 实施阶段可读取
	MainLoopModel      string               `json:"mainLoopModel,omitempty"`
	ExecutionModel     string               `json:"executionModel,omitempty"` // 自动路由的快速执行模型 (Path B); 空=未配置
	AgentID            types.AgentID        `json:"agentId,omitempty"`
	IsNonInteractive   bool                 `json:"isNonInteractive"`
	Debug              bool                 `json:"debug"`
	MaxToolResultChars int                  `json:"maxToolResultChars,omitempty"`
	// WeakResultPostprocess 弱模型 tool_result 确定性后处理 (方案三 L3):
	// 单行截断 + 超行数保头尾, 在 compactToolResultContent 入口生效。
	WeakResultPostprocess bool `json:"weakResultPostprocess,omitempty"`
	// WeakEditGuard 写入前语法护栏 (方案三 L2): edit/write 拒写语法坏内容
	// 并回注错误 (SWE-agent ACI), 覆盖 .go/.py/.json。
	WeakEditGuard bool `json:"weakEditGuard,omitempty"`
	// GlobalPerm 若非 nil，RunToolUse 会用其 CheckGlobal 合并全局策略与工具自带权限结果
	GlobalPerm GlobalPermissionChecker `json:"-"`
}

// PathInPlanDir 判定展开后的绝对路径是否位于计划文件目录 (PlanFileDir) 内。
// 规划期 (PermissionModePlan) 的唯一可写例外就是计划文件目录 —— 模型可在此起草/
// 更新计划, 对齐 Claude 客户端 plan mode 只放行 plans 目录的语义; 其余路径一律拒写。
// 子路径判定用 filepath.Rel (仿 pkg/agent/team_workspace.go:106), 杜绝 `../` 逃逸。
// PlanFileDir 为空 (未配置) 时恒 false (无例外, 规划期全拒写)。
func (t *ToolContext) PathInPlanDir(path string) bool {
	if t == nil || strings.TrimSpace(t.PlanFileDir) == "" {
		return false
	}
	base := filepath.Clean(t.PlanFileDir)
	target := filepath.Clean(path)
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Registry 工具注册表，管理所有可用工具。
// 对应 TS: tools.ts 中的 getAllBaseTools() + assembleToolPool()。
//
// 注册表在查询循环开始时组装，包括：
//   - 内置工具 (FileRead, FileWrite, Bash 等)
//   - MCP 工具 (从外部 MCP 服务器发现的工具)
//   - Agent 工具 (Task/Agent)
type Registry struct {
	mu      sync.RWMutex
	tools   map[string]Tool
	aliases map[string]string // alias → primary name
	order   []string          // 保持注册顺序
}

// NewRegistry 创建空的工具注册表
func NewRegistry() *Registry {
	return &Registry{
		tools:   make(map[string]Tool),
		aliases: make(map[string]string),
	}
}

// Register 注册一个工具。同名工具会被覆盖。
// 如果工具实现了 AliasedTool 接口，同时注册别名。
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Name()
	if _, exists := r.tools[name]; !exists {
		r.order = append(r.order, name)
	}
	r.tools[name] = t

	// 注册别名 (对应 TS: tool.aliases)
	if at, ok := t.(AliasedTool); ok {
		for _, alias := range at.Aliases() {
			r.aliases[alias] = name
		}
	}
}

// Get 根据名称或别名查找工具。
// 对应 TS: findToolByName(tools, name) + toolMatchesName()
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if t, ok := r.tools[name]; ok {
		return t, true
	}
	// 通过别名查找
	if primary, ok := r.aliases[name]; ok {
		t, ok := r.tools[primary]
		return t, ok
	}
	return nil, false
}

// All 返回所有已注册工具的有序列表。
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		if t, ok := r.tools[name]; ok {
			result = append(result, t)
		}
	}
	return result
}

// APITools 返回用于 API 请求的工具定义列表。
// 对应 TS: queryLoop 中构造 tools 参数传给 API。
func (r *Registry) APITools() []types.APITool {
	tools := r.All()
	result := make([]types.APITool, len(tools))
	for i, t := range tools {
		result[i] = types.APITool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		}
	}
	return result
}

// Names 返回所有工具名称列表
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, len(r.order))
	copy(names, r.order)
	return names
}

// Count 返回已注册工具数量
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Remove 移除一个工具
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// FindByName 查找工具，找不到时返回错误
func FindByName(tools []Tool, name string) (Tool, error) {
	for _, t := range tools {
		if t.Name() == name {
			return t, nil
		}
	}
	return nil, fmt.Errorf("工具 %q 未找到", name)
}
