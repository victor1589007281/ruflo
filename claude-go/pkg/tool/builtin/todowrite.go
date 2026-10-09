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

func NewTodoWriteTool() *TodoWriteTool {
	return &TodoWriteTool{
		todos: make(map[string]types.TodoItem),
	}
}

func (t *TodoWriteTool) Name() string { return TodoWriteToolName }

func (t *TodoWriteTool) Description() string {
	return `Use this tool to create and manage a structured task list for your current coding session.`
}

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

func (t *TodoWriteTool) IsReadOnly(_ json.RawMessage) bool        { return false }
func (t *TodoWriteTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *TodoWriteTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 创建/更新待办事项。
// 对应 TS: TodoWriteTool.ts 中的 call()
func (t *TodoWriteTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in todoWriteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if !in.Merge {
		t.todos = make(map[string]types.TodoItem)
	}

	for _, item := range in.Todos {
		if in.Merge {
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
