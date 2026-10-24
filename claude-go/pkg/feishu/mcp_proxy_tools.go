package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/textutil"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	mcpToolSearchName = "MCPToolSearch"
	mcpToolInvokeName = "MCPToolInvoke"
)

// registerMCPProxyTools 注册 MCPToolSearch / MCPToolInvoke 两个 MCP 代理工具;
// mgr 为 nil (dynmcp 未启用) 时什么都不做。
func registerMCPProxyTools(reg *tool.Registry, mgr *dynmcp.Manager) {
	if mgr == nil {
		return
	}
	reg.Register(&mcpToolSearch{mgr: mgr})
	reg.Register(&mcpToolInvoke{mgr: mgr})
}

type mcpToolSearch struct {
	mgr *dynmcp.Manager
}

// Name 返回工具名 MCPToolSearch。
func (t *mcpToolSearch) Name() string { return mcpToolSearchName }

// Description 返回给模型看的说明: 先按关键词检索已连接的 MCP 工具, 再用 MCPToolInvoke
// 调用 —— 完整的 MCP schema 刻意不进基础提示词。
func (t *mcpToolSearch) Description() string {
	return "Search connected MCP tools by keyword. Use this before MCPToolInvoke; full MCP schemas are intentionally not injected into the base prompt."
}

// InputSchema 声明入参: query / max_results / include_schema。
func (t *mcpToolSearch) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Keyword, capability, or MCP tool name to search for."},
			"max_results": {"type": "integer", "description": "Maximum results to return, default 12, max 50."},
			"include_schema": {"type": "boolean", "description": "Whether to include a truncated input schema for matching tools."}
		}
	}`)
}

// IsReadOnly 恒为 true —— 检索不产生任何副作用。
func (t *mcpToolSearch) IsReadOnly(_ json.RawMessage) bool        { return true }

// IsConcurrencySafe 恒为 true —— 只读检索可并行。
func (t *mcpToolSearch) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 返回 nil: 检索不改变任何状态, 交给全局权限链判定。
func (t *mcpToolSearch) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 按关键词检索 MCP 工具, 返回"工具名 + 截断后的一句话描述"(可选带截断的 schema) 清单。
//
// 算法解读：
//
//	原理：MCP 工具的完整 schema 很大, 全量注入基础提示词会挤爆上下文预算; 于是改成两段式 ——
//	      本工具按关键词搜出候选, 模型再用 MCPToolInvoke 点名调用。这是"把大 schema 挡在
//	      提示词之外"这条设计的入口。
//	关键不变量：max_results 必须夹在 [1,50] —— <=0 回落成 12, >50 截到 50。没有上界时
//	      一次检索就能把整个 MCP 工具面拉回上下文, 正是本设计要避免的事。
//	代价与边界：匹配是**全 token 的 AND 子串匹配** (见 searchMCPToolRefs), 不做语义检索;
//	      查询词里任一分词没命中就整条落选, 换个说法就搜不到。描述与 schema 分别截断到
//	      220 / 1200 字符, 超长部分直接丢弃。
//	设计权衡：schema 默认不带 (include_schema=false) —— 先给名字让模型挑, 需要细节时再
//	      单独取, 比一次性全给省得多; 代价是多一次往返。
func (t *mcpToolSearch) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Query         string `json:"query"`
		MaxResults    int    `json:"max_results"`
		IncludeSchema bool   `json:"include_schema"`
	}
	_ = json.Unmarshal(input, &in)
	if in.MaxResults <= 0 {
		in.MaxResults = 12
	}
	if in.MaxResults > 50 {
		in.MaxResults = 50
	}

	refs := searchMCPToolRefs(t.mgr.GetConnections(), in.Query, in.MaxResults)
	if len(refs) == 0 {
		return &tool.ToolResult{Content: "No matching MCP tools found."}, nil
	}

	var sb strings.Builder
	sb.WriteString("Matching MCP tools:\n")
	for _, ref := range refs {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", ref.FullName, truncateText(ref.Info.Description, 220)))
		if in.IncludeSchema && len(ref.Info.InputSchema) > 0 {
			sb.WriteString(fmt.Sprintf("  schema: %s\n", truncateText(compactJSON(ref.Info.InputSchema), 1200)))
		}
	}
	sb.WriteString("\nCall MCPToolInvoke with tool_name set to one of the names above.")
	return &tool.ToolResult{Content: sb.String()}, nil
}

type mcpToolInvoke struct {
	mgr *dynmcp.Manager
}

// Name 返回工具名 MCPToolInvoke。
func (t *mcpToolInvoke) Name() string { return mcpToolInvokeName }

// Description 返回给模型看的说明: 用 MCPToolSearch 找到名字后再调用该 MCP 工具。
func (t *mcpToolInvoke) Description() string {
	return "Invoke a connected MCP tool by name after discovering it with MCPToolSearch. This keeps large MCP tool schemas out of normal prompts."
}

// InputSchema 声明入参: tool_name (必填) 与 arguments。
func (t *mcpToolInvoke) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"tool_name": {"type": "string", "description": "Full MCP tool name, e.g. mcp_ruflo_search, or an unambiguous raw tool name."},
			"arguments": {"type": "object", "description": "Arguments to pass to the MCP tool."}
		},
		"required": ["tool_name"]
	}`)
}

// IsReadOnly 按工具名的**前缀**猜测被调 MCP 工具是否只读 (read/get/list/search/... 打头即算只读)。
//
// 注意：这是纯粹的名字启发式, 与被调工具的真实行为无关 —— 一个叫 "searchAndDelete" 的
// 工具会被判成只读。名字查不到或有歧义时保守返回 false。这里只记录, 不改代码。
func (t *mcpToolInvoke) IsReadOnly(input json.RawMessage) bool {
	ref, ok := findMCPToolRef(t.mgr.GetConnections(), toolNameFromInput(input))
	if !ok {
		return false
	}
	// 前缀表是唯一的判据 —— 名字以 read/get/list/... 开头就当作只读, 不看被调工具的
	// 真实行为 (见上面的「注意」)。
	name := strings.ToLower(ref.Info.Name)
	for _, prefix := range []string{"read", "get", "list", "search", "query", "fetch", "describe", "show", "status", "info", "check", "view"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// IsConcurrencySafe 恒为 false —— 无法预知被调 MCP 工具的副作用, 保守串行。
func (t *mcpToolInvoke) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// CheckPermissions 返回 nil: MCP 工具的授权由连接侧配置决定, 本层不附加策略。
func (t *mcpToolInvoke) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 按名字解析出唯一匹配的 MCP 工具并转发调用, 原样返回其文本结果。
//
// 算法解读：
//
//	原理：MCPToolSearch 给出候选名, 本工具负责"名字 → 唯一连接 → 转发调用"这一段解析。
//	关键不变量：名字必须**唯一匹配** (FullName / 裸名 / <conn>.<tool> 三种写法之一)。
//	      命中多于一条时按"找不到"处理并提示先检索 —— 同名工具静默挑第一个会调错后端,
//	      而"调用失败"比"调错了但成功了"容易发现得多。
//	代价与边界：arguments 为空时补成 {} (部分 MCP 服务端不接受 nil 参数); 返回的是纯文本,
//	      结构化结果 (图片 / 资源引用) 在这一层被拍平; 无重试、无本层超时。
//	设计权衡：不在这里加重试与超时 —— 那是连接层 (pkg/mcp.Connection) 的职责, 重复一遍
//	      只会让超时语义出现两个真相源。
func (t *mcpToolInvoke) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		ToolName  string          `json:"tool_name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("MCPToolInvoke input parse failed: %v", err), IsError: true}, nil
	}
	if len(in.Arguments) == 0 {
		in.Arguments = json.RawMessage(`{}`)
	}

	ref, ok := findMCPToolRef(t.mgr.GetConnections(), in.ToolName)
	if !ok {
		return &tool.ToolResult{Content: fmt.Sprintf("MCP tool %q not found or ambiguous. Use MCPToolSearch first.", in.ToolName), IsError: true}, nil
	}
	result, err := ref.Conn.CallTool(ctx, ref.Info.Name, in.Arguments)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("MCP tool call failed: %v", err), IsError: true}, nil
	}
	return &tool.ToolResult{Content: result}, nil
}

type mcpToolRef struct {
	FullName string
	Conn     *mcp.Connection
	Info     mcp.ToolInfo
}

// searchMCPToolRefs 在全部 MCP 工具里按关键词做 AND 子串匹配, 最多返回 limit 条。
//
// 算法解读：
//
//	原理：query 按空白切成小写 token, 工具的 "FullName + 名字 + 描述" 作干草堆, 所有
//	      token 都出现才算命中 (AND); query 为空 (切不出 token) 时直接取前 limit 条。
//	关键不变量：命中数一到 limit 立即跳出 —— 结果条数由调用方给定, 且**不重排序**, 顺序
//	      就是 allMCPToolRefs 的 FullName 字典序, 因此同样的查询结果可复现。
//	代价与边界：O(工具数 × token 数) 全表扫描, 每次调用重算 (无索引); 工具面大时是纯
//	      CPU 浪费, 当前规模下可忽略。子串匹配对拼写差异与同义词无能为力。
//	设计权衡：用子串而非分词或向量检索 —— 零依赖、确定性, 对工具名这类短标识符已经够用。
func searchMCPToolRefs(conns []*mcp.Connection, query string, limit int) []mcpToolRef {
	refs := allMCPToolRefs(conns)
	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		if len(refs) > limit {
			return refs[:limit]
		}
		return refs
	}

	var matches []mcpToolRef
	for _, ref := range refs {
		haystack := strings.ToLower(ref.FullName + " " + ref.Info.Name + " " + ref.Info.Description)
		ok := true
		for _, token := range tokens {
			if !strings.Contains(haystack, token) {
				ok = false
				break
			}
		}
		if ok {
			matches = append(matches, ref)
			if len(matches) >= limit {
				break
			}
		}
	}
	return matches
}

// findMCPToolRef 按名字解析**唯一**匹配的 MCP 工具: 接受 FullName (mcp_<conn>_<tool>)、
// 裸工具名与 <conn>.<tool> 三种写法; 命中数不等于 1 一律判为"找不到"。
func findMCPToolRef(conns []*mcp.Connection, name string) (mcpToolRef, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return mcpToolRef{}, false
	}
	var matches []mcpToolRef
	for _, ref := range allMCPToolRefs(conns) {
		if name == ref.FullName || name == ref.Info.Name || name == ref.Conn.Config.Name+"."+ref.Info.Name {
			matches = append(matches, ref)
		}
	}
	if len(matches) != 1 {
		return mcpToolRef{}, false
	}
	return matches[0], true
}

// allMCPToolRefs 把各连接下的工具拍平成引用列表, 并按 FullName 升序排序
// (顺序稳定, 让检索结果可复现)。
func allMCPToolRefs(conns []*mcp.Connection) []mcpToolRef {
	var refs []mcpToolRef
	for _, conn := range conns {
		for _, info := range conn.Tools {
			refs = append(refs, mcpToolRef{
				FullName: fmt.Sprintf("mcp_%s_%s", conn.Config.Name, info.Name),
				Conn:     conn,
				Info:     info,
			})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].FullName < refs[j].FullName })
	return refs
}

// toolNameFromInput 从原始入参里只取 tool_name 一个字段; 解析失败返回空串。
func toolNameFromInput(input json.RawMessage) string {
	var in struct {
		ToolName string `json:"tool_name"`
	}
	_ = json.Unmarshal(input, &in)
	return in.ToolName
}

// compactJSON 把 JSON 重新编码成紧凑形式 (去掉空白); 解析或重编码失败时原样返回输入。
func compactJSON(raw json.RawMessage) string {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(data)
}

// truncateText 把字符串截到 max 个字符, 超出部分以 "...(truncated)" 结尾。
func truncateText(s string, max int) string {
	return textutil.Truncate(s, max, "...(truncated)")
}
