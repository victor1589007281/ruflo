// lsptools.go 实现一组高级/集成向工具（LSP、配置、技能、结构化输出、工具搜索）。
//
// 说明:
//   - LSP：本 Go 移植未连接语言服务器，统一返回固定提示「LSP not connected」。
//   - Config：进程内简单键值配置，用于演示 get/set；非持久化。
//   - Skill：模拟斜杠指令/技能入口，仅返回引导说明，不执行外部命令。
//   - StructuredOutput：将 LLM 传入的 JSON 原样回传，便于 SDK 结构化输出管线调试。
//   - ToolSearch：按关键词在 tool.Registry 中搜索已注册工具的名称与描述。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	LSPToolName                = "LSP"
	ConfigToolName             = "Config"
	SkillToolName              = "Skill"
	StructuredOutputToolName   = "StructuredOutput"
	ToolSearchToolName         = "ToolSearch"
)

var validLSPOperations = map[string]struct{}{
	"goToDefinition":   {},
	"findReferences": {},
	"hover":          {},
	"documentSymbol": {},
	"workspaceSymbol": {},
}

// --- LSPTool ---

type lspInput struct {
	Operation string `json:"operation"`
	FilePath  string `json:"filePath"`
	Line      *int   `json:"line,omitempty"`
	Character *int   `json:"character,omitempty"`
}

// LSPTool 语言服务器协议相关操作的占位实现。
type LSPTool struct{}

// NewLSPTool 构造 LSP 工具。
func NewLSPTool() *LSPTool {
	return &LSPTool{}
}

func (t *LSPTool) Name() string { return LSPToolName }

func (t *LSPTool) Description() string {
	return `对接到语言服务器：支持 goToDefinition、findReferences、hover、documentSymbol、workspaceSymbol。当前 Go 移植未连接 LSP。`
}

func (t *LSPTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"operation": {
				"type": "string",
				"enum": ["goToDefinition", "findReferences", "hover", "documentSymbol", "workspaceSymbol"],
				"description": "LSP 操作类型"
			},
			"filePath": {"type": "string", "description": "目标文件路径（部分操作可为工作区相对路径）"},
			"line": {"type": "integer", "description": "可选，0-based 或 1-based 由客户端约定；此处仅透传"},
			"character": {"type": "integer", "description": "可选，列偏移"}
		},
		"required": ["operation", "filePath"]
	}`)
}

func (t *LSPTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *LSPTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *LSPTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *LSPTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in lspInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if _, ok := validLSPOperations[in.Operation]; !ok {
		return &tool.ToolResult{Content: fmt.Sprintf("未知 operation: %q", in.Operation), IsError: true}, nil
	}
	if strings.TrimSpace(in.FilePath) == "" {
		return &tool.ToolResult{Content: "filePath 不能为空", IsError: true}, nil
	}
	return &tool.ToolResult{Content: "LSP not connected"}, nil
}

// --- ConfigTool ---

type configInput struct {
	Setting string  `json:"setting"`
	Value   *string `json:"value,omitempty"`
}

// ConfigTool 进程内配置读写（非持久化）。
type ConfigTool struct {
	mu     sync.RWMutex
	values map[string]string
}

// NewConfigTool 构造配置工具，并初始化空存储。
func NewConfigTool() *ConfigTool {
	return &ConfigTool{values: make(map[string]string)}
}

func (t *ConfigTool) Name() string { return ConfigToolName }

func (t *ConfigTool) Description() string {
	return `读取或设置简单配置项。未提供 value 时视为查询；提供 value 时视为设置（内存中，重启后丢失）。`
}

func (t *ConfigTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"setting": {"type": "string", "description": "配置键名"},
			"value": {"type": "string", "description": "可选。若省略则执行 get；若提供则执行 set"}
		},
		"required": ["setting"]
	}`)
}

// IsReadOnly 在未提供 value 字段（get）时为只读；提供 value（set）时为写入。
func (t *ConfigTool) IsReadOnly(input json.RawMessage) bool {
	if len(input) == 0 {
		return false
	}
	var in configInput
	if err := json.Unmarshal(input, &in); err != nil {
		return false
	}
	return in.Value == nil
}

func (t *ConfigTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *ConfigTool) CheckPermissions(input json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx == nil || tctx.PermissionMode != types.PermissionModePlan {
		return nil
	}
	var in configInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil
	}
	if in.Value != nil {
		return &types.PermissionResult{
			Behavior: types.PermissionDeny,
			Reason:   "计划模式下不允许修改配置（Config set）",
		}
	}
	return nil
}

func (t *ConfigTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in configInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(in.Setting) == "" {
		return &tool.ToolResult{Content: "setting 不能为空", IsError: true}, nil
	}
	if in.Value == nil {
		t.mu.RLock()
		v, ok := t.values[in.Setting]
		t.mu.RUnlock()
		if !ok {
			return &tool.ToolResult{Content: fmt.Sprintf("(未设置) setting=%q", in.Setting)}, nil
		}
		return &tool.ToolResult{Content: fmt.Sprintf("%s=%s", in.Setting, v)}, nil
	}
	t.mu.Lock()
	t.values[in.Setting] = *in.Value
	t.mu.Unlock()
	return &tool.ToolResult{Content: fmt.Sprintf("已设置 %s=%s", in.Setting, *in.Value)}, nil
}

// --- SkillTool ---

type skillInput struct {
	Skill string `json:"skill"`
	Args  string `json:"args,omitempty"`
}

// SkillTool 斜杠指令 / 技能入口的引导实现。
type SkillTool struct{}

// NewSkillTool 构造 Skill 工具。
func NewSkillTool() *SkillTool {
	return &SkillTool{}
}

func (t *SkillTool) Name() string { return SkillToolName }

func (t *SkillTool) Description() string {
	return `触发命名技能或斜杠命令（本实现仅返回如何在本仓库中使用 .claude/skills 的说明，不执行外部进程）。`
}

func (t *SkillTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"skill": {"type": "string", "description": "技能名称或斜杠命令标识"},
			"args": {"type": "string", "description": "可选，传给技能的参数字符串"}
		},
		"required": ["skill"]
	}`)
}

func (t *SkillTool) IsReadOnly(_ json.RawMessage) bool { return false }

func (t *SkillTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *SkillTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式下不执行 Skill"}
	}
	return nil
}

func (t *SkillTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in skillInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(in.Skill) == "" {
		return &tool.ToolResult{Content: "skill 不能为空", IsError: true}, nil
	}
	msg := fmt.Sprintf(
		"技能「%s」在本 Go 客户端中为占位实现：请在 Claude Code / Cursor 中使用对应 $skill 或斜杠命令加载 .claude/skills 下的 SKILL.md。",
		in.Skill,
	)
	if in.Args != "" {
		msg += fmt.Sprintf("\n\n传入的 args（未执行）: %s", in.Args)
	}
	return &tool.ToolResult{Content: msg}, nil
}

// --- StructuredOutputTool ---

// StructuredOutputTool 将输入 JSON 原样作为成功内容返回（用于结构化输出调试）。
type StructuredOutputTool struct{}

// NewStructuredOutputTool 构造 StructuredOutput 工具。
func NewStructuredOutputTool() *StructuredOutputTool {
	return &StructuredOutputTool{}
}

func (t *StructuredOutputTool) Name() string { return StructuredOutputToolName }

func (t *StructuredOutputTool) Description() string {
	return `SDK 结构化输出通道：接受任意 JSON 对象，响应内容与输入相同（紧凑序列化），便于链式工具与 schema 校验调试。`
}

func (t *StructuredOutputTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"description": "动态 JSON，由调用方与上层 schema 约定字段",
		"additionalProperties": true
	}`)
}

func (t *StructuredOutputTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *StructuredOutputTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *StructuredOutputTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *StructuredOutputTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	if len(input) == 0 {
		return &tool.ToolResult{Content: "null"}, nil
	}
	var v any
	if err := json.Unmarshal(input, &v); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入不是合法 JSON: %v", err), IsError: true}, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("序列化失败: %v", err), IsError: true}, nil
	}
	return &tool.ToolResult{Content: string(out)}, nil
}

// --- ToolSearchTool ---

type toolSearchInput struct {
	Query      string `json:"query"`
	MaxResults *int   `json:"max_results,omitempty"`
}

// ToolSearchTool 在注册表中按关键词搜索工具（名称与描述子串匹配，不区分大小写）。
type ToolSearchTool struct {
	reg *tool.Registry
}

// NewToolSearchTool 构造工具搜索器；reg 为当前会话可用的工具注册表。
func NewToolSearchTool(reg *tool.Registry) *ToolSearchTool {
	return &ToolSearchTool{reg: reg}
}

func (t *ToolSearchTool) Name() string { return ToolSearchToolName }

func (t *ToolSearchTool) Description() string {
	return `按关键词在已注册工具的名称与描述中搜索，返回匹配列表（用于工具发现）。`
}

func (t *ToolSearchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "搜索关键词（子串匹配）"},
			"max_results": {"type": "integer", "description": "可选，默认 20，最大返回条数"}
		},
		"required": ["query"]
	}`)
}

func (t *ToolSearchTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *ToolSearchTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *ToolSearchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *ToolSearchTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in toolSearchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	q := strings.TrimSpace(strings.ToLower(in.Query))
	if q == "" {
		return &tool.ToolResult{Content: "query 不能为空", IsError: true}, nil
	}
	max := 20
	if in.MaxResults != nil && *in.MaxResults > 0 {
		max = *in.MaxResults
	}
	if t.reg == nil {
		return &tool.ToolResult{Content: "内部错误: ToolRegistry 未注入", IsError: true}, nil
	}
	var lines []string
	for _, toolInst := range t.reg.All() {
		name := strings.ToLower(toolInst.Name())
		desc := strings.ToLower(toolInst.Description())
		if strings.Contains(name, q) || strings.Contains(desc, q) {
			lines = append(lines, fmt.Sprintf("- %s: %s", toolInst.Name(), toolInst.Description()))
			if len(lines) >= max {
				break
			}
		}
	}
	if len(lines) == 0 {
		return &tool.ToolResult{Content: fmt.Sprintf("未找到与 %q 匹配的工具。", in.Query)}, nil
	}
	return &tool.ToolResult{Content: strings.Join(lines, "\n")}, nil
}
