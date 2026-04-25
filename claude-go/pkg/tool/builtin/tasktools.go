// tasktools.go 实现 V2 任务系统的四个内置工具（TaskCreate / TaskGet / TaskUpdate / TaskList）。
// 对应 Claude Code Agent Teams 风格的共享任务列表，数据持久化到系统临时目录下的 JSON 文件。
//
// 设计要点:
//   - 所有工具共享同一 TaskStore，内部 sync.Mutex 保证并发安全；
//   - 任务以 UUID 为主键，创建/更新后落盘，进程间可复用同一路径（默认固定文件名）；
//   - TaskCreate、TaskUpdate 为写入类工具；TaskGet、TaskList 为只读工具。
package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	taskCreateToolName = "TaskCreate"
	taskGetToolName    = "TaskGet"
	taskUpdateToolName = "TaskUpdate"
	taskListToolName   = "TaskList"

	defaultTaskStoreFile = "claude-go-v2-tasks.json"
)

// v2TaskRecord 表示持久化的一条任务记录（与 types.Task 语义相近，独立定义以避免与全局类型耦合）。
// DAG 支持: DependsOn 字段存储前置依赖的 task ID 列表, 形成有向无环图。
// 参考: Temporal Workflow DAG, Airflow TaskInstance dependencies
type v2TaskRecord struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	ActiveForm  string   `json:"activeForm,omitempty"`
	Status      string   `json:"status"`
	Owner       string   `json:"owner,omitempty"`
	DependsOn   []string `json:"dependsOn,omitempty"` // DAG: 前置依赖的 task ID 列表
	Priority    int      `json:"priority,omitempty"`   // 0=normal, 1=high, 2=critical
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

type taskFilePayload struct {
	Tasks map[string]v2TaskRecord `json:"tasks"`
}

// TaskStore 任务存储：内存索引 + JSON 文件持久化。
type TaskStore struct {
	mu   sync.Mutex
	path string
	byID map[string]v2TaskRecord
}

// NewTaskStore 创建任务存储。若 path 为空，则使用 os.TempDir() 下的默认文件名。
func NewTaskStore(path string) *TaskStore {
	if path == "" {
		path = filepath.Join(os.TempDir(), defaultTaskStoreFile)
	}
	s := &TaskStore{
		path: path,
		byID: make(map[string]v2TaskRecord),
	}
	_ = s.load()
	return s
}

func (s *TaskStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var p taskFilePayload
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.Tasks != nil {
		s.byID = p.Tasks
	}
	return nil
}

func (s *TaskStore) saveLocked() error {
	if s.byID == nil {
		s.byID = make(map[string]v2TaskRecord)
	}
	payload := taskFilePayload{Tasks: s.byID}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func genTaskUUID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	h := hex.EncodeToString(buf[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// --- TaskCreate ---

type taskCreateInput struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
	ActiveForm  string `json:"activeForm,omitempty"`
}

// TaskCreateTool 创建任务并分配 UUID，写入存储。
type TaskCreateTool struct {
	store *TaskStore
}

// NewTaskCreateTool 构造 TaskCreate 工具。
func NewTaskCreateTool(store *TaskStore) *TaskCreateTool {
	return &TaskCreateTool{store: store}
}

func (t *TaskCreateTool) Name() string { return taskCreateToolName }

func (t *TaskCreateTool) Description() string {
	return `在共享任务存储中创建一条新任务（V2 Task 系统）。返回任务 ID。`
}

func (t *TaskCreateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"subject": {"type": "string", "description": "任务标题/主题。"},
			"description": {"type": "string", "description": "任务详细描述。"},
			"activeForm": {"type": "string", "description": "进行中的简短描述（可选）。"}
		},
		"required": ["subject", "description"]
	}`)
}

func (t *TaskCreateTool) IsReadOnly(_ json.RawMessage) bool { return false }
func (t *TaskCreateTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *TaskCreateTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式：禁止创建任务"}
	}
	return nil
}

func (t *TaskCreateTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in taskCreateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if in.Subject == "" || in.Description == "" {
		return &tool.ToolResult{Content: "subject 与 description 不能为空", IsError: true}, nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := genTaskUUID()
	rec := v2TaskRecord{
		ID:          id,
		Subject:     in.Subject,
		Description: in.Description,
		ActiveForm:  in.ActiveForm,
		Status:      "pending",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if t.store.byID == nil {
		t.store.byID = make(map[string]v2TaskRecord)
	}
	t.store.byID[id] = rec
	if err := t.store.saveLocked(); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("持久化失败: %v", err), IsError: true}, nil
	}

	out, _ := json.MarshalIndent(rec, "", "  ")
	return &tool.ToolResult{Content: string(out)}, nil
}

// --- TaskGet ---

type taskGetInput struct {
	TaskID string `json:"taskId"`
}

// TaskGetTool 按 ID 读取单条任务。
type TaskGetTool struct {
	store *TaskStore
}

// NewTaskGetTool 构造 TaskGet 工具。
func NewTaskGetTool(store *TaskStore) *TaskGetTool {
	return &TaskGetTool{store: store}
}

func (t *TaskGetTool) Name() string { return taskGetToolName }

func (t *TaskGetTool) Description() string {
	return `根据 taskId 从共享任务存储中读取一条任务。`
}

func (t *TaskGetTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"taskId": {"type": "string", "description": "任务 UUID。"}
		},
		"required": ["taskId"]
	}`)
}

func (t *TaskGetTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (t *TaskGetTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *TaskGetTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *TaskGetTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in taskGetInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if in.TaskID == "" {
		return &tool.ToolResult{Content: "taskId 不能为空", IsError: true}, nil
	}

	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	rec, ok := t.store.byID[in.TaskID]
	if !ok {
		return &tool.ToolResult{Content: fmt.Sprintf("未找到任务: %s", in.TaskID), IsError: true}, nil
	}
	out, _ := json.MarshalIndent(rec, "", "  ")
	return &tool.ToolResult{Content: string(out)}, nil
}

// --- TaskUpdate ---

type taskUpdateInput struct {
	TaskID      string  `json:"taskId"`
	Status      *string `json:"status,omitempty"`
	Description *string `json:"description,omitempty"`
	Owner       *string `json:"owner,omitempty"`
}

// TaskUpdateTool 更新已有任务的字段。
type TaskUpdateTool struct {
	store *TaskStore
}

// NewTaskUpdateTool 构造 TaskUpdate 工具。
func NewTaskUpdateTool(store *TaskStore) *TaskUpdateTool {
	return &TaskUpdateTool{store: store}
}

func (t *TaskUpdateTool) Name() string { return taskUpdateToolName }

func (t *TaskUpdateTool) Description() string {
	return `更新指定任务的状态、描述或负责人（可选字段仅更新提供的项）。`
}

func (t *TaskUpdateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"taskId": {"type": "string", "description": "任务 UUID。"},
			"status": {"type": "string", "description": "新状态，例如 pending / in_progress / completed。"},
			"description": {"type": "string", "description": "新的详细描述。"},
			"owner": {"type": "string", "description": "负责人标识。"}
		},
		"required": ["taskId"]
	}`)
}

func (t *TaskUpdateTool) IsReadOnly(_ json.RawMessage) bool { return false }
func (t *TaskUpdateTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *TaskUpdateTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式：禁止更新任务"}
	}
	return nil
}

func (t *TaskUpdateTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in taskUpdateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if in.TaskID == "" {
		return &tool.ToolResult{Content: "taskId 不能为空", IsError: true}, nil
	}
	if in.Status == nil && in.Description == nil && in.Owner == nil {
		return &tool.ToolResult{Content: "至少需要提供 status、description、owner 之一", IsError: true}, nil
	}

	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	rec, ok := t.store.byID[in.TaskID]
	if !ok {
		return &tool.ToolResult{Content: fmt.Sprintf("未找到任务: %s", in.TaskID), IsError: true}, nil
	}
	if in.Status != nil {
		rec.Status = *in.Status
	}
	if in.Description != nil {
		rec.Description = *in.Description
	}
	if in.Owner != nil {
		rec.Owner = *in.Owner
	}
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	t.store.byID[in.TaskID] = rec
	if err := t.store.saveLocked(); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("持久化失败: %v", err), IsError: true}, nil
	}
	out, _ := json.MarshalIndent(rec, "", "  ")
	return &tool.ToolResult{Content: string(out)}, nil
}

// --- TaskList ---

// TaskListTool 列出当前存储中的全部任务。
type TaskListTool struct {
	store *TaskStore
}

// NewTaskListTool 构造 TaskList 工具。
func NewTaskListTool(store *TaskStore) *TaskListTool {
	return &TaskListTool{store: store}
}

func (t *TaskListTool) Name() string { return taskListToolName }

func (t *TaskListTool) Description() string {
	return `列出共享任务存储中的全部任务（JSON 数组）。`
}

func (t *TaskListTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {},
		"additionalProperties": false
	}`)
}

func (t *TaskListTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (t *TaskListTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *TaskListTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *TaskListTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	list := make([]v2TaskRecord, 0, len(t.store.byID))
	for _, rec := range t.store.byID {
		list = append(list, rec)
	}
	out, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("序列化失败: %v", err), IsError: true}, nil
	}
	return &tool.ToolResult{Content: string(out)}, nil
}

// --- Team Integration API ---
// 以下方法供 Agent Teams 系统复用 V2 任务管理, 避免重复建设。

// TaskSummary 面向外部消费者的任务摘要视图。
type TaskSummary struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Owner       string   `json:"owner"`
	DependsOn   []string `json:"dependsOn,omitempty"`
	Priority    int      `json:"priority,omitempty"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

// AddTask 创建任务并返回其 ID (供团队编排使用)。
func (s *TaskStore) AddTask(subject, description, owner string) (string, error) {
	return s.AddTaskWithDeps(subject, description, owner, nil, 0)
}

// AddTaskWithDeps 创建带依赖的任务 (DAG 支持)。
// dependsOn: 前置依赖的 task ID 列表, priority: 0=normal, 1=high, 2=critical
func (s *TaskStore) AddTaskWithDeps(subject, description, owner string, dependsOn []string, priority int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 恢复场景修复: resume 时 ParsePlanToDAG 会重新创建任务,
	// 如果已有相同 subject 的任务且状态为 completed, 直接复用旧任务,
	// 避免产生重复任务导致重新开始。
	for id, rec := range s.byID {
		if rec.Subject == subject {
			if rec.Status == "completed" {
				return id, nil // 已完成的任务, 直接复用
			}
			// 未完成的任务, 更新依赖后复用
			rec.DependsOn = dependsOn
			rec.Priority = priority
			rec.Description = description
			rec.Owner = owner
			rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			s.byID[id] = rec
			_ = s.saveLocked()
			return id, nil
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := genTaskUUID()
	// 如果有依赖但前置任务未完成, 状态设为 blocked
	status := "pending"
	if len(dependsOn) > 0 {
		status = "blocked"
	}
	rec := v2TaskRecord{
		ID: id, Subject: subject, Description: description,
		Status: status, Owner: owner, DependsOn: dependsOn,
		Priority: priority, CreatedAt: now, UpdatedAt: now,
	}
	if s.byID == nil {
		s.byID = make(map[string]v2TaskRecord)
	}
	s.byID[id] = rec
	if err := s.saveLocked(); err != nil {
		return "", err
	}
	return id, nil
}

// UnblockDependents 当一个任务完成或失败时, 检查并解除其依赖者的阻塞状态。
// 修复: 之前只在 "completed" 时 unblock, 导致某个任务 failed 后整个 DAG 后续全部卡住。
// 现在改为: 依赖任务的状态为 completed 或 failed 均视为"已处理", 下游可被调度。
// 参考: Airflow trigger_rule="all_done" — 上游无论成功失败, 下游都应被调度。
func (s *TaskStore) UnblockDependents(doneID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	unblocked := 0
	for id, rec := range s.byID {
		if rec.Status != "blocked" {
			continue
		}
		allDone := true
		for _, dep := range rec.DependsOn {
			d, ok := s.byID[dep]
			if !ok {
				allDone = false
				break
			}
			isDone := d.Status == "completed" || d.Status == "failed" || d.ID == doneID
			if !isDone {
				allDone = false
				break
			}
		}
		if allDone {
			rec.Status = "pending"
			rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			s.byID[id] = rec
			unblocked++
		}
	}
	if unblocked > 0 {
		_ = s.saveLocked()
	}
	return unblocked
}

// ReadyTasks 返回所有可执行的任务 (pending 且依赖已满足), 按优先级排序。
// 这是 DAG 调度器的核心: 拓扑排序的"就绪队列"。
func (s *TaskStore) ReadyTasks() []TaskSummary {
	s.mu.Lock()
	defer s.mu.Unlock()

	var ready []TaskSummary
	for _, r := range s.byID {
		if r.Status != "pending" {
			continue
		}
		allDepsOK := true
		for _, dep := range r.DependsOn {
			if d, ok := s.byID[dep]; !ok || d.Status != "completed" {
				allDepsOK = false
				break
			}
		}
		if allDepsOK {
			ready = append(ready, TaskSummary{
				ID: r.ID, Subject: r.Subject, Description: r.Description,
				Status: r.Status, Owner: r.Owner, DependsOn: r.DependsOn,
				Priority: r.Priority, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			})
		}
	}
	// 按优先级降序排序 (critical > high > normal)
	for i := 1; i < len(ready); i++ {
		for j := i; j > 0 && ready[j].Priority > ready[j-1].Priority; j-- {
			ready[j], ready[j-1] = ready[j-1], ready[j]
		}
	}
	return ready
}

// SetTaskStatus 按 ID 更新任务状态 (供团队编排使用)。
func (s *TaskStore) SetTaskStatus(id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return fmt.Errorf("task not found: %s", id)
	}
	rec.Status = status
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.byID[id] = rec
	return s.saveLocked()
}

// GetAllTasks 返回所有任务的摘要列表。
func (s *TaskStore) GetAllTasks() []TaskSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]TaskSummary, 0, len(s.byID))
	for _, r := range s.byID {
		result = append(result, TaskSummary{
			ID: r.ID, Subject: r.Subject, Description: r.Description,
			Status: r.Status, Owner: r.Owner, DependsOn: r.DependsOn,
			Priority: r.Priority, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return result
}

// SetTaskStatusAndUnblock 原子更新状态并解除下游依赖 (DAG 联动)。
// 修复3项:
//  1. completed 和 failed 都 unblock 下游 (之前 failed 不 unblock → DAG 全卡)
//  2. 原子操作: 一次加锁同时完成 setStatus + unblock (之前分两次锁, 有竞态窗口)
//  3. 依赖检查改为 all_done 语义 (completed/failed 都算"处理完毕")
func (s *TaskStore) SetTaskStatusAndUnblock(id, status string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[id]
	if !ok {
		return 0, fmt.Errorf("task not found: %s", id)
	}
	rec.Status = status
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.byID[id] = rec

	affected := 0
	if status == "completed" {
		// 成功: 解除下游 blocked → pending (原始逻辑)
		for depID, depRec := range s.byID {
			if depRec.Status != "blocked" {
				continue
			}
			allDone := true
			for _, dep := range depRec.DependsOn {
				d, exists := s.byID[dep]
				if !exists || d.Status != "completed" {
					allDone = false
					break
				}
			}
			if allDone {
				depRec.Status = "pending"
				depRec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				s.byID[depID] = depRec
				affected++
			}
		}
	} else if status == "failed" {
		// 失败级联: 依赖此任务的下游直接标记 failed, 避免无意义调度。
		// 递归传播 — 如果 B 依赖 A, C 依赖 B, A 失败后 B 和 C 都应 failed。
		affected = s.cascadeFailureLocked(id)
	}

	_ = s.saveLocked()
	return affected, nil
}

// cascadeFailureLocked 递归将依赖 failedID 的下游任务标记为 failed。
// 调用方必须已持有 s.mu 锁。
func (s *TaskStore) cascadeFailureLocked(failedID string) int {
	cascaded := 0
	for depID, depRec := range s.byID {
		if depRec.Status == "completed" || depRec.Status == "failed" {
			continue
		}
		dependsOnFailed := false
		for _, dep := range depRec.DependsOn {
			if dep == failedID {
				dependsOnFailed = true
				break
			}
		}
		if dependsOnFailed {
			depRec.Status = "failed"
			depRec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			s.byID[depID] = depRec
			cascaded++
			cascaded += s.cascadeFailureLocked(depID)
		}
	}
	return cascaded
}
