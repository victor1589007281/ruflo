// taskio.go 实现后台任务的输出读取与进程终止工具：TaskOutput、TaskStop（别名 KillShell）。
//
// 设计说明:
//   - BackgroundTaskRegistry 维护 task_id → 子进程 + 线程安全输出缓冲；
//   - 宿主在 cmd.Start() 成功后应调用 RegisterStreamingTask，且 cmd 的 Stdout/Stderr 已写入同一 *SafeBuffer；
//   - TaskOutput 为只读（读取已捕获输出）；可选 timeout（毫秒）在截止前短暂等待更多输出；
//   - TaskStop 会向进程组发送终止信号（尽力而为），并从注册表移除条目。
//
// 对应常见场景：Shell 在后台拉起长时间任务后，通过 task_id 轮询输出或强制结束。
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	taskOutputToolName = "TaskOutput"
	taskStopToolName   = "TaskStop"
	killShellAlias     = "KillShell"
)

var _ tool.AliasedTool = (*TaskStopTool)(nil)

// SafeBuffer 并发安全的字节缓冲，可作为 cmd.Stdout / cmd.Stderr 的 io.Writer。
type SafeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// NewSafeBuffer 创建可用于 RegisterStreamingTask 的输出缓冲。
func NewSafeBuffer() *SafeBuffer {
	return &SafeBuffer{}
}

func (s *SafeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String 返回当前已缓冲输出的快照（拷贝字符串）。
func (s *SafeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type bgTaskEntry struct {
	cmd *exec.Cmd
	out *SafeBuffer
}

// BackgroundTaskRegistry 后台任务注册表（进程 + 聚合输出）。
type BackgroundTaskRegistry struct {
	mu    sync.Mutex
	tasks map[string]*bgTaskEntry
}

// NewBackgroundTaskRegistry 创建空注册表。
func NewBackgroundTaskRegistry() *BackgroundTaskRegistry {
	return &BackgroundTaskRegistry{
		tasks: make(map[string]*bgTaskEntry),
	}
}

// RegisterStreamingTask 登记一个已 Start 的子进程及其输出缓冲。
// out 必须与 cmd.Stdout、cmd.Stderr 所使用的缓冲为同一实例，以便聚合日志。
func (r *BackgroundTaskRegistry) RegisterStreamingTask(taskID string, cmd *exec.Cmd, out *SafeBuffer) error {
	if taskID == "" {
		return fmt.Errorf("task_id 不能为空")
	}
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("cmd 必须已 Start 且 Process 非空")
	}
	if out == nil {
		return fmt.Errorf("输出缓冲不能为 nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[taskID]; ok {
		return fmt.Errorf("任务 ID 已存在: %s", taskID)
	}
	r.tasks[taskID] = &bgTaskEntry{cmd: cmd, out: out}
	return nil
}

func (r *BackgroundTaskRegistry) get(taskID string) (*bgTaskEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.tasks[taskID]
	return e, ok
}

func (r *BackgroundTaskRegistry) remove(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tasks, taskID)
}

// --- TaskOutput ---

type taskOutputInput struct {
	TaskID  string `json:"task_id"`
	Timeout *int   `json:"timeout,omitempty"` // 毫秒；可选，>0 时在窗口内轮询输出
}

// TaskOutputTool 读取后台任务已捕获的标准输出/错误（聚合缓冲）。
type TaskOutputTool struct {
	reg *BackgroundTaskRegistry
}

// NewTaskOutputTool 构造 TaskOutput 工具。
func NewTaskOutputTool(reg *BackgroundTaskRegistry) *TaskOutputTool {
	return &TaskOutputTool{reg: reg}
}

func (t *TaskOutputTool) Name() string { return taskOutputToolName }

func (t *TaskOutputTool) Description() string {
	return `按 task_id 读取后台任务截至目前写入聚合缓冲区的输出；可选 timeout（毫秒）在短时间窗口内等待更多输出。`
}

func (t *TaskOutputTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "description": "后台任务标识符。"},
			"timeout": {"type": "integer", "description": "可选等待窗口（毫秒），用于在极短时间内轮询新输出。"}
		},
		"required": ["task_id"]
	}`)
}

func (t *TaskOutputTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (t *TaskOutputTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *TaskOutputTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *TaskOutputTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in taskOutputInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if in.TaskID == "" {
		return &tool.ToolResult{Content: "task_id 不能为空", IsError: true}, nil
	}

	entry, ok := t.reg.get(in.TaskID)
	if !ok {
		return &tool.ToolResult{Content: fmt.Sprintf("未知任务: %s", in.TaskID), IsError: true}, nil
	}

	wait := 0 * time.Millisecond
	if in.Timeout != nil && *in.Timeout > 0 {
		wait = time.Duration(*in.Timeout) * time.Millisecond
	}

	deadline := time.Now().Add(wait)
	var text string
	for {
		text = entry.out.String()
		if wait == 0 || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return &tool.ToolResult{Content: fmt.Sprintf("已取消: %v", ctx.Err()), IsError: true}, nil
		case <-time.After(10 * time.Millisecond):
		}
	}

	if text == "" {
		text = "(尚无输出)"
	}
	return &tool.ToolResult{Content: text}, nil
}

// --- TaskStop ---

type taskStopInput struct {
	TaskID string `json:"task_id"`
}

// TaskStopTool 终止后台任务对应子进程；LLM 也可通过别名 KillShell 调用同一工具。
type TaskStopTool struct {
	reg *BackgroundTaskRegistry
}

// NewTaskStopTool 构造 TaskStop 工具。
func NewTaskStopTool(reg *BackgroundTaskRegistry) *TaskStopTool {
	return &TaskStopTool{reg: reg}
}

func (t *TaskStopTool) Name() string { return taskStopToolName }

func (t *TaskStopTool) Aliases() []string { return []string{killShellAlias} }

func (t *TaskStopTool) Description() string {
	return `按 task_id 终止后台子进程（尽力发送 SIGTERM/SIGKILL）；注册表中的条目会被移除。`
}

func (t *TaskStopTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "description": "要停止的后台任务标识符。"}
		},
		"required": ["task_id"]
	}`)
}

func (t *TaskStopTool) IsReadOnly(_ json.RawMessage) bool { return false }
func (t *TaskStopTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *TaskStopTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式：禁止终止后台进程"}
	}
	return nil
}

func (t *TaskStopTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in taskStopInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if in.TaskID == "" {
		return &tool.ToolResult{Content: "task_id 不能为空", IsError: true}, nil
	}

	entry, ok := t.reg.get(in.TaskID)
	if !ok {
		return &tool.ToolResult{Content: fmt.Sprintf("未知任务: %s", in.TaskID), IsError: true}, nil
	}

	proc := entry.cmd.Process
	if proc == nil {
		t.reg.remove(in.TaskID)
		return &tool.ToolResult{Content: "进程句柄不存在，已清理注册项", IsError: true}, nil
	}

	// 使用 Kill 保证跨平台尽力终止子进程（具体信号语义依赖操作系统）。
	_ = proc.Kill()

	t.reg.remove(in.TaskID)
	return &tool.ToolResult{Content: fmt.Sprintf("已请求停止任务 %s", in.TaskID)}, nil
}
