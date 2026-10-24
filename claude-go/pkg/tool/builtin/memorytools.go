// memorytools.go —— 记忆的按需取回两件: memory_search / memory_read。
//
// 与「把整段记忆塞进系统提示」相比, 这两件把记忆从**推送**改成**拉取**:
//
//	push: 每次请求都注入 top-7 全文 (~19K 字符, 实测占系统提示的 2/3), 其中大部分
//	      与当前任务无关, 却要每次付钱、还稀释注意力;
//	pull: 提示里只放索引 (每条一行, ~1K 字符), 模型认出相关的那条再按 id 取全文。
//
// 这是 pool_search / pool_load 的同一套做法 (渐进披露), 只是素材换成了记忆。
//
// 两个工具的分工刻意划清:
//
//	memory_search  换关键词**重查**, 返回索引 (id + 标题 + 预览), 不返回正文;
//	memory_read    按 id 取**正文**, 有未截断快照时取快照。
//
// 为什么不合成一个带 `full bool` 的工具: 两者的代价相差一个数量级 (一次检索 vs
// 一次读盘), 合成一个会让"只是看一眼有哪些记忆"这种廉价动作与"把 4K 正文拖进
// 上下文"这种昂贵动作共用一个名字, 模型的调用意图就无从分辨, 预算也无从施加。

package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/textutil"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// MemoryIndexModeGetenv 是索引模式开关的环境变量名 (宿主无配置系统时用)。
//
// 语义与 builtin.PoolFallbackEnabled 同构 (见 pkg/tool/pool.go):
// 未设置 = 默认值, "0"/"false"/"off" = 关, 其余 = 开。
const MemoryIndexModeGetenv = "CLAUDE_GO_MEMORY_INDEX_MODE"

// memoryIndexModeDefault 默认**关**。改这一条等于改所有部署的注入形态, 所以它
// 归宿主开关管, 而不是在代码里翻默认值 —— 先把开关和遥测跑一段, 量出索引模式
// 下 memory_read 的实际调用率, 再决定要不要默认开。
const memoryIndexModeDefault = false

// MemoryIndexModeEnabled 返回是否用索引形态注入记忆。
func MemoryIndexModeEnabled(getenv func(string) string) bool {
	if getenv == nil {
		getenv = noEnv
	}
	v := strings.ToLower(strings.TrimSpace(getenv(MemoryIndexModeGetenv)))
	switch v {
	case "":
		return memoryIndexModeDefault
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// noEnv 是 getenv 的缺省实现: 宿主没注入读环境变量的能力时一律当"未设置"。
func noEnv(string) string { return "" }

// ---------------------------------------------------------------------------
// memory_search
// ---------------------------------------------------------------------------

// MemorySearchTool 按关键词检索记忆, 只回索引 (id + 标题 + 预览)。
//
// 注意：getenv 字段目前**既没有赋值点也没有读取点** —— 注入形态的开关实际由
// 宿主在别处读 MemoryIndexModeEnabled 决定, 这个字段是接线的残留。
type MemorySearchTool struct {
	store  *memory.TieredStore
	getenv func(string) string
}

// NewMemorySearchTool 构造检索工具 (store 为 nil 时工具仍注册, 调用时如实报错)。
func NewMemorySearchTool(store *memory.TieredStore) *MemorySearchTool {
	return &MemorySearchTool{store: store}
}

// Name 返回工具名 memory_search。
func (t *MemorySearchTool) Name() string { return "memory_search" }

// Description 返回给模型看的说明: 强调"只回索引不回正文", 并指引用 memory_read 取全文。
func (t *MemorySearchTool) Description() string {
	return "Search long-term memory (past team runs and sessions) by keyword. Returns an index only — id, date, topics, one-line preview — not the full text; call memory_read with an id to get a hit's full text. The same memories may already appear as a <memory_index> block in your system prompt."
}

// InputSchema 返回输入 schema: query 可空 (空 = 列最近的记忆), limit 默认 8。
func (t *MemorySearchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Keywords describing what you are looking for (e.g. 'JD 分析 面试题', 'team_result build failure'). Empty lists the most recent memories."},
			"limit": {"type": "integer", "description": "Max hits (default 8)."}
		}
	}`)
}

// IsReadOnly 恒为 true: 检索不修改记忆库。
func (t *MemorySearchTool) IsReadOnly(_ json.RawMessage) bool        { return true }

// IsConcurrencySafe 恒为 true: 检索无副作用, 可并发。
func (t *MemorySearchTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 返回 nil: 不做工具级权限判定。
func (t *MemorySearchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 按关键词检索记忆, 只返回索引 (id + 标题 + 预览)。
//
// 算法解读：
//
//	原理：把"记忆库为空""检索无命中""命中若干条"三种结局**分开表述** ——
//	      三者的正确下一步完全不同 (前者不该改写查询, 中者该换更通用的关键词,
//	      后者该按 id 取正文)。
//	关键不变量：索引的呈现必须与系统提示里 <memory_index> 块**用同一个格式化函数**
//	      (memory.FormatIndexForPrompt) —— 两侧长得不一样, 模型就无法把搜到的
//	      条目与提示里已有的条目对上, 会重复取同一条。
//	代价与边界：store 为 nil 或返回零条时不报错, 而是返回一段说明文本;
//	      因此"工具不可用"在 metrics 上表现为正常调用。
//	设计权衡：只回索引不回正文 —— 一次检索的成本与一次读盘相差一个数量级,
//	      合成一个工具会让"看一眼有哪些记忆"与"把正文拖进上下文"共用一个名字。
func (t *MemorySearchTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errOnly("memory_search 输入解析失败: %v", err), nil
	}
	if t.store == nil {
		return errOnly("memory_search 不可用: 本会话未接入长期记忆 (MemoryStore 为 nil)。"), nil
	}
	if in.Limit <= 0 {
		in.Limit = 8
	}
	if t.store.Count() == 0 {
		// 零条与"没搜到"是两回事, 分开说 —— 前者说明这个部署压根没积累记忆,
		// 模型不该据此改写查询。
		return &tool.ToolResult{Content: "长期记忆当前为空 (没有任何已积累的条目)。"}, nil
	}
	hits := t.store.Retrieve(in.Query, in.Limit)
	if len(hits) == 0 {
		return &tool.ToolResult{Content: "没有匹配的记忆。换更通用的关键词再试, 或直接用现有信息完成任务。"}, nil
	}
	// 复用与注入同一个格式化函数: 两侧的呈现必须一致, 否则模型在系统提示里
	// 看到的索引与这里搜出来的长得不一样, 它就无法把两边对上。
	return &tool.ToolResult{Content: memory.FormatIndexForPrompt(hits)}, nil
}

// ---------------------------------------------------------------------------
// memory_read
// ---------------------------------------------------------------------------

// MemoryReadTool 按 id 取回一条记忆的正文 (有未截断快照时取快照)。
type MemoryReadTool struct {
	store *memory.TieredStore
}

// NewMemoryReadTool 构造读取工具。
func NewMemoryReadTool(store *memory.TieredStore) *MemoryReadTool {
	return &MemoryReadTool{store: store}
}

// Name 返回工具名 memory_read。
func (t *MemoryReadTool) Name() string { return "memory_read" }

// Description 返回给模型看的说明: 按 id 取正文, 有未截断快照时优先给快照。
func (t *MemoryReadTool) Description() string {
	return "Read one long-term memory by its id (ids come from the <memory_index> block or from memory_search). Returns the full text — the untruncated original when one was stored, otherwise the summary."
}

// memoryReadMaxRunes 单次读取的正文上限 (字符数)。
// 与 blob 的 1MB 硬上限是两回事: 那个管"存不存", 这个管"一次往上下文里拖多少"。
// 取 20000 (约 6K token): 足够一份团队报告的主体, 又不至于一次调用就把预算打满。
const memoryReadMaxRunes = 20000

// InputSchema 返回输入 schema: id 必填 (取自 <memory_index> 或 memory_search 的结果)。
func (t *MemoryReadTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Memory id, e.g. \"20260918-101530-0001\"."}
		},
		"required": ["id"]
	}`)
}

// IsReadOnly 恒为 true: 只读记忆库与快照文件。
func (t *MemoryReadTool) IsReadOnly(_ json.RawMessage) bool        { return true }

// IsConcurrencySafe 恒为 true: 读取无副作用, 可并发。
func (t *MemoryReadTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 返回 nil: 不做工具级权限判定。
func (t *MemoryReadTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 按 id 取回一条记忆的正文: 有未截断快照时给原文, 否则回落摘要, 最后按字符数截断。
//
// 注意：id 查不到是**正常结局**而非故障 —— 记忆会被衰减淘汰 (Retention 低于阈值
// 时 Prune), 所以文案明确给出"可能已被淘汰 + 用 memory_search 重新检索"的下一步,
// 免得模型以为自己记错了 id 而反复重试同一个。
func (t *MemoryReadTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errOnly("memory_read 输入解析失败: %v", err), nil
	}
	if t.store == nil {
		return errOnly("memory_read 不可用: 本会话未接入长期记忆 (MemoryStore 为 nil)。"), nil
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return errOnly("memory_read 需要 id (取自 <memory_index> 或 memory_search 的结果)。"), nil
	}

	entry, ok := t.store.Get(id)
	if !ok {
		// 记忆会被衰减淘汰 (Retention 低于阈值时 Prune), 所以"id 查不到"是正常
		// 结局而不是故障。如实说, 并给出下一步 —— 别让模型以为是自己记错了 id
		// 而反复重试同一个。
		return errOnly("没有 id 为 %q 的记忆 (可能已被衰减淘汰)。用 memory_search 重新检索。", id), nil
	}

	body := entry.Content
	kind := "摘要"
	if entry.Ref != "" {
		if full, ok := t.store.ReadOriginal(entry.Ref); ok {
			body = full
			kind = "完整原文"
		}
		// 取不到快照时静默回落摘要: 快照是加分项 (见 blob.go 的降级方向),
		// 它丢了不该让这条记忆变得读不出来。
	}

	header := fmt.Sprintf("id=%s date=%s source=%s topics=%v (%s)\n\n",
		entry.ID, entry.CreatedAt.Format("2006-01-02 15:04:05"), entry.Source, entry.Topics, kind)
	content := textutil.Truncate(body, memoryReadMaxRunes, "\n...(已截断, 原文更长)")
	return &tool.ToolResult{Content: header + content}, nil
}

// RegisterMemoryTools 注册记忆两件套 (store 为 nil 时也注册 —— 见各自的 Call)。
func RegisterMemoryTools(reg *tool.Registry, store *memory.TieredStore) {
	reg.Register(NewMemorySearchTool(store))
	reg.Register(NewMemoryReadTool(store))
}
