// todowrite.go 实现 TodoWrite 工具。
// 对应 TS 源码: review/claude/src/tools/TodoWriteTool/TodoWriteTool.ts
//
// 功能:
//   - 创建/更新待办事项列表 (用于任务管理)
//   - 支持 merge 模式 (合并到现有列表)
//   - 状态: pending, in_progress, completed, cancelled
//   - 数据存储在内存中 (AppState.todos)
//
// TodoWrite 是 Claude Code 内置的任务管理工具，
// LLM 用它来追踪复杂任务的进度。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const TodoWriteToolName = "TodoWrite"

type todoWriteInput struct {
	Todos []types.TodoItem `json:"todos"`
	Merge bool             `json:"merge,omitempty"`
}

// TodoWriteTool 待办事项写入工具
type TodoWriteTool struct {
	mu    sync.Mutex
	todos map[string]types.TodoItem // key: todo ID
}

// NewTodoWriteTool 构造待办工具, 初始化为空清单。
func NewTodoWriteTool() *TodoWriteTool {
	return &TodoWriteTool{
		todos: make(map[string]types.TodoItem),
	}
}

// Name 返回工具名 TodoWrite。
func (t *TodoWriteTool) Name() string { return TodoWriteToolName }

// Description 返回工具说明 (英文原文, 与 TS 版保持一致)。
func (t *TodoWriteTool) Description() string {
	return `Use this tool to create and manage a structured task list for your current coding session.`
}

// InputSchema 返回输入 schema; todos 至少 2 条, merge 控制是否并入现有清单。
func (t *TodoWriteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"todos": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"id": {"type": "string"},
						"content": {"type": "string"},
						"status": {"type": "string", "enum": ["pending", "in_progress", "completed", "cancelled"]}
					},
					"required": ["id", "content", "status"]
				},
				"minItems": 2
			},
			"merge": {"type": "boolean", "description": "Whether to merge with existing todos."}
		},
		"required": ["todos"]
	}`)
}

// IsReadOnly 本工具会改写内存中的清单, 恒为 false。
func (t *TodoWriteTool) IsReadOnly(_ json.RawMessage) bool        { return false }

// IsConcurrencySafe 内部虽有 mutex, 但语义是整表覆盖, 故声明为不可并发。
func (t *TodoWriteTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// CheckPermissions 不额外索要授权, 恒返回 nil 表示放行。
func (t *TodoWriteTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 创建/更新待办事项。
// 对应 TS: TodoWriteTool.ts 中的 call()
//
// 算法解读：
//
//	原理：以 ID 为键做 upsert —— 非 merge 模式先清空整表再全量写入, merge 模式逐条按 ID
//	      合并, 已存在的条目只覆盖非空字段, 新 ID 直接插入。
//	关键不变量：t.todos 的 key 恒等于 item.ID; 且 merge 模式下空串表示"这一列不改" ——
//	      由此推出**无法用 merge 把某个字段清空**, 想清空只能走非 merge 全量覆盖。
//	代价与边界：全程持 t.mu, 非 merge 还是整表重建, 并发调用会被串行化; 输出遍历 map,
//	      **顺序随机** (需要稳定顺序的调用方要用 Snapshot / GetTodosSorted)。
//	设计权衡：本函数不做输入校验 —— schema 里的 minItems:2、status 枚举都由上层 schema
//	      校验兜底, 在这里再抄一遍迟早会与 schema 分叉。
func (t *TodoWriteTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in todoWriteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// 非 merge 是"整表替换"而不是"清空后合并": 本次没出现在 todos 里的条目会被直接丢弃。
	if !in.Merge {
		t.todos = make(map[string]types.TodoItem)
	}

	for _, item := range in.Todos {
		if in.Merge {
			// 已存在的 ID 只做字段级合并, 不会退化成整条替换 (否则缺字段的增量更新会清空旧值)。
			if existing, ok := t.todos[item.ID]; ok {
				if item.Content != "" {
					existing.Content = item.Content
				}
				if item.Status != "" {
					existing.Status = item.Status
				}
				t.todos[item.ID] = existing
				continue
			}
		}
		t.todos[item.ID] = item
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Successfully updated TODOs (%d items):\n", len(t.todos)))
	// 遍历 map, 输出顺序随机 —— 需要确定性文本的调用方请用 Snapshot。
	for _, item := range t.todos {
		sb.WriteString(fmt.Sprintf("- [%s] %s: %s\n", item.Status, item.ID, item.Content))
	}

	return &tool.ToolResult{Content: sb.String()}, nil
}

// GetTodos 返回当前所有待办事项 (供外部查询)
func (t *TodoWriteTool) GetTodos() map[string]types.TodoItem {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make(map[string]types.TodoItem, len(t.todos))
	for k, v := range t.todos {
		result[k] = v
	}
	return result
}

// TodoSnapshot 是清单的一条扁平快照。字段与 internal_hook.TaskRosterItem 对齐,
// 但两组类型分属不同的包 —— 引擎侧用函数签名注入正是为了绕开直接依赖。
type TodoSnapshot struct {
	ID      string
	Content string
	Status  string
}

// Snapshot 返回待办清单的稳定快照 (按 ID 排序)。
//
// **为什么需要这个方法**: GetTodos 返回的是 map, 遍历顺序随机。清单会被嵌进压缩
// 摘要并逐轮重发, 顺序一抖前缀缓存就从插入点起全废。这里排好序, 保证同一份清单
// 在任何进程里都渲染出逐字节相同的文本。
//
// 这是 GetTodos 的第一个非测试调用方 —— 在此之前它零调用方, 清单写进去就沉底。
func (t *TodoWriteTool) Snapshot() []TodoSnapshot {
	// 两段式加锁: map 的遍历顺序随机, 要先在锁内取出键、锁外排序, 再锁内取值。
	t.mu.Lock()
	ids := make([]string, 0, len(t.todos))
	for id := range t.todos {
		ids = append(ids, id)
	}
	t.mu.Unlock()

	sort.Strings(ids)

	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TodoSnapshot, 0, len(ids))
	for _, id := range ids {
		item, ok := t.todos[id]
		if !ok {
			continue // 两次加锁之间被删掉了
		}
		out = append(out, TodoSnapshot{ID: item.ID, Content: item.Content, Status: item.Status})
	}
	return out
}

// GetTodosSorted 是 GetTodos 的有序版本, 供需要稳定输出顺序的调用方使用。
func (t *TodoWriteTool) GetTodosSorted() []types.TodoItem {
	snaps := t.Snapshot()
	out := make([]types.TodoItem, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, types.TodoItem{ID: s.ID, Content: s.Content, Status: s.Status})
	}
	return out
}
