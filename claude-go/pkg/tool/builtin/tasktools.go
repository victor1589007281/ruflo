// tasktools.go 实现 V2 任务系统的四个内置工具（TaskCreate / TaskGet / TaskUpdate / TaskList）。
// 对应 Claude Code Agent Teams 风格的共享任务列表，数据持久化到项目作用域下的 JSON 文件。
//
// 设计要点:
//   - 所有工具共享同一 TaskStore，内部 sync.Mutex 保证并发安全；
//   - 任务以 UUID 为主键，创建/更新后落盘；
//   - TaskCreate、TaskUpdate 为写入类工具；TaskGet、TaskList 为只读工具。
//
// # 三个任务系统里谁是唯一真源
//
// 仓库里并存三套任务状态。它们的地位**不相等**，混用会让"还剩什么没做完"失去可信度：
//
//	真源   pkg/agent/taskservice.go 的 FileQueueTaskService
//	       —— 唯一有 CAS (TransitionCAS) / revision / append-only journal / 终态的台账。
//	视图①  TodoWriteTool (todowrite.go): 模型自己的草稿纸, 纯内存, 不落盘, 不进台账。
//	视图②  本文件的 TaskStore: 真源在 pkg/agent 不可达时的降级台账。
//
// **为什么真源不能是这里**：pkg/agent 不能 import pkg/tool/builtin —— builtin 经
// evotools → evolution/govern 反向成环（见 pkg/agent/constraints.go:112-113 的记录）。
// 因此团队编排层（它才是"谁还没做完"的消费者）在编译期就看不见本包，只能看见
// taskservice。把真源放在这里，等于把编排层最需要的那份状态放在它拿不到的地方。
//
// # 路径必须是项目作用域
//
// 本项目早期默认落 os.TempDir()/claude-go-v2-tasks.json —— 那是**全机器共享**的单文件，
// 两个项目并发跑会互相覆盖，且重启后 /tmp 被清理即丢。TaskStorePathFor 从项目目录派生，
// 不同项目得到不同路径（H1-1 的验收判据就是"两个项目的任务文件路径不相等"）。
package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	taskCreateToolName = "TaskCreate"
	taskGetToolName    = "TaskGet"
	taskUpdateToolName = "TaskUpdate"
	taskListToolName   = "TaskList"
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
	DependsOn   []string `json:"dependsOn,omitempty"`   // DAG: 前置依赖的 task ID 列表
	Priority    int      `json:"priority,omitempty"`    // 0=normal, 1=high, 2=critical
	WriteScopes []string `json:"writeScopes,omitempty"` // F13: 任务声明的写域 (共享核心状态/契约/manifest 等领域键), 供规划层串行化与观测
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

// TaskStorePathFor 解析任务文件的项目作用域路径。
//
// 解析顺序与 basedir.ResolveDefault 同源，保证任务文件与其他模块数据目录落在同一个根下：
//
//	stateDir 非空            → <stateDir>/tasks/tasks.json
//	stateDir 空, projectDir   → <projectDir>/.claude-go/tasks/tasks.json
//
// **刻意不回退到 os.TempDir()**：/tmp 是全机器共享的，两个项目并发跑会互相覆盖，
// 这正是 H1-1 要修掉的形态。stateDir 与 projectDir 都为空时才退回相对路径 ".claude-go"。
func TaskStorePathFor(stateDir, projectDir string) string {
	root := basedir.ResolveDefault(stateDir, projectDir)
	layout, err := basedir.NewLayout(root)
	if err != nil {
		// NewLayout 只在 root 为空时报错，而 ResolveDefault 保证了非空；真出错时
		// 也要给一个项目相对路径，绝不落到共享的临时目录。
		return filepath.Join(".claude-go", "tasks", "tasks.json")
	}
	return layout.TasksFilePath()
}

// DefaultTaskStorePath 返回当前进程的项目作用域任务文件路径。
//
// 读 CLAUDE_GO_STATE_DIR：该变量此前**只被写、从没被读**（pkg/worker/k8sjob.go:639
// 给 worker job 注入它），所以 k8s worker 里任务文件其实一直落在容器的 cwd 下。
// 在这里读它是补上写入侧本来就期望的那一半。
func DefaultTaskStorePath() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	return TaskStorePathFor(os.Getenv("CLAUDE_GO_STATE_DIR"), cwd)
}

// NewTaskStore 创建任务存储。若 path 为空，则使用项目作用域下的默认路径
// （见 DefaultTaskStorePath），**不是** os.TempDir()。
func NewTaskStore(path string) *TaskStore {
	if path == "" {
		path = DefaultTaskStorePath()
	}
	s := &TaskStore{
		path: path,
		byID: make(map[string]v2TaskRecord),
	}
	_ = s.load()
	return s
}

// Path 返回该存储实际使用的文件路径。用于验收"两个项目的任务文件路径不相等"。
func (s *TaskStore) Path() string { return s.path }

// load 从磁盘载入任务文件到内存索引; 文件不存在视为空台账 (不算错误)。
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

// saveLocked 把内存索引落盘; 调用方必须已持有 s.mu (名字里的 Locked 即此意)。
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
	// 先写 .tmp 再 rename: 崩溃时读到的要么是完整旧文件要么是完整新文件, 不会留下半个 JSON。
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// genTaskUUID 生成 RFC 4122 v4 格式的 UUID 字符串 (第 7 字节置版本位、第 9 字节置变体位)。
func genTaskUUID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	h := hex.EncodeToString(buf[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// uniqueTrimmedStringsTask 对字符串列表做 trim + 去重 + 丢弃空串 (F13 写域/依赖清洗用;
// builtin 包不依赖 pkg/agent, 本地实现)。
func uniqueTrimmedStringsTask(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// Name 返回工具名 "TaskCreate"。
func (t *TaskCreateTool) Name() string { return taskCreateToolName }

// Description 返回工具描述 (在共享任务存储中创建一条新任务并返回任务 ID)。
func (t *TaskCreateTool) Description() string {
	return `在共享任务存储中创建一条新任务（V2 Task 系统）。返回任务 ID。`
}

// InputSchema 返回 JSON Schema (subject 与 description 必填, activeForm 可选)。
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

// IsReadOnly 恒为 false —— TaskCreate 会写入任务存储。
func (t *TaskCreateTool) IsReadOnly(_ json.RawMessage) bool        { return false }
// IsConcurrencySafe 恒为 true —— 内部靠 TaskStore 的互斥锁串行化写入。
func (t *TaskCreateTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 在计划模式下拒绝创建任务, 其余情况放行。
func (t *TaskCreateTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式：禁止创建任务"}
	}
	return nil
}

// Call 校验入参、生成 UUID、以 pending 状态落盘并返回新任务的 JSON。
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

// Name 返回工具名 "TaskGet"。
func (t *TaskGetTool) Name() string { return taskGetToolName }

// Description 返回工具描述 (按 taskId 读取一条任务)。
func (t *TaskGetTool) Description() string {
	return `根据 taskId 从共享任务存储中读取一条任务。`
}

// InputSchema 返回 JSON Schema (taskId 必填)。
func (t *TaskGetTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"taskId": {"type": "string", "description": "任务 UUID。"}
		},
		"required": ["taskId"]
	}`)
}

// IsReadOnly 恒为 true —— TaskGet 只读任务存储。
func (t *TaskGetTool) IsReadOnly(_ json.RawMessage) bool        { return true }
// IsConcurrencySafe 恒为 true —— 只读不改变状态。
func (t *TaskGetTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 不额外设限, 交回通用权限流程。
func (t *TaskGetTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 按 taskId 查任务; 未找到时返回 IsError 结果而不是 Go error。
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

// Name 返回工具名 "TaskUpdate"。
func (t *TaskUpdateTool) Name() string { return taskUpdateToolName }

// Description 返回工具描述 (仅更新调用方提供的字段)。
func (t *TaskUpdateTool) Description() string {
	return `更新指定任务的状态、描述或负责人（可选字段仅更新提供的项）。`
}

// InputSchema 返回 JSON Schema (taskId 必填, status/description/owner 可选)。
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

// IsReadOnly 恒为 false —— TaskUpdate 会写入任务存储。
func (t *TaskUpdateTool) IsReadOnly(_ json.RawMessage) bool        { return false }
// IsConcurrencySafe 恒为 true —— 内部靠 TaskStore 的互斥锁串行化写入。
func (t *TaskUpdateTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 在计划模式下拒绝更新任务, 其余情况放行。
func (t *TaskUpdateTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "计划模式：禁止更新任务"}
	}
	return nil
}

// Call 校验入参、按字段是否为 nil 做局部更新、刷新 UpdatedAt 并落盘, 返回更新后的任务 JSON。
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

// Name 返回工具名 "TaskList"。
func (t *TaskListTool) Name() string { return taskListToolName }

// Description 返回工具描述 (列出全部任务, JSON 数组)。
func (t *TaskListTool) Description() string {
	return `列出共享任务存储中的全部任务（JSON 数组）。`
}

// InputSchema 返回空对象 Schema (TaskList 不接受参数)。
func (t *TaskListTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {},
		"additionalProperties": false
	}`)
}

// IsReadOnly 恒为 true —— TaskList 只读任务存储。
func (t *TaskListTool) IsReadOnly(_ json.RawMessage) bool        { return true }
// IsConcurrencySafe 恒为 true —— 只读不改变状态。
func (t *TaskListTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 不额外设限, 交回通用权限流程。
func (t *TaskListTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 返回存储中全部任务的 JSON 数组; map 遍历顺序随机, 输出不保证稳定序。
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
	WriteScopes []string `json:"writeScopes,omitempty"`
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
	return s.AddTaskFull(subject, description, owner, dependsOn, priority, nil)
}

// AddTaskFull 创建带依赖与写域的任务 (F13 writeScopes)。
// writeScopes: 任务声明的写域键列表 (如 "contract:store"、"state:mvcc")。
// 复用已完成/未完成同名任务时, 写域作为可观测元数据随任务保留, 不参与匹配。
func (s *TaskStore) AddTaskFull(subject, description, owner string, dependsOn []string, priority int, writeScopes []string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 恢复场景修复: resume 时 ParsePlanToDAG 会重新创建任务,
	// 搜集所有相同 subject 的任务, 优先复用已完成的, 避免产生重复任务。
	var completedIDs, otherIDs []string
	for id, rec := range s.byID {
		if rec.Subject == subject {
			if rec.Status == "completed" {
				completedIDs = append(completedIDs, id)
			} else {
				otherIDs = append(otherIDs, id)
			}
		}
	}

	// 优先级 1: 复用最早的创建的已完成任务 (时间最可靠)
	if len(completedIDs) > 0 {
		bestID := completedIDs[0]
		bestTime := s.byID[bestID].CreatedAt
		for _, id := range completedIDs[1:] {
			if t := s.byID[id].CreatedAt; t < bestTime {
				bestID, bestTime = id, t
			}
		}
		return bestID, nil
	}

	// 优先级 2: 复用最早的未完成的任务
	if len(otherIDs) > 0 {
		bestID := otherIDs[0]
		bestTime := s.byID[bestID].CreatedAt
		for _, id := range otherIDs[1:] {
			if t := s.byID[id].CreatedAt; t < bestTime {
				bestID, bestTime = id, t
			}
		}
		rec := s.byID[bestID]
		// 恢复场景关键修复: resume 时 ParsePlanToDAG 会重新遍历所有任务,
		// 动态 file-write/conflict 依赖依赖处理顺序. 若顺序与原始运行不同,
		// 动态依赖方向可能反转, 与显式依赖叠加后形成死循环.
		// 正确做法: 复用现有任务时保留其原始依赖图, 不覆盖.
		// 原始依赖图已经过验证(团队曾在此基础上执行), 更安全.
		// dependsOn = filterTaskDependsOn(dependsOn, bestID)
		// dependsOn = s.filterCycleCreatingDeps(bestID, dependsOn)
		// rec.DependsOn = dependsOn
		rec.Priority = priority
		rec.Description = description
		rec.Owner = owner
		rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		s.byID[bestID] = rec
		_ = s.saveLocked()
		return bestID, nil
	}

	// 优先级 3: 创建新任务
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := genTaskUUID()
	dependsOn = filterTaskDependsOn(dependsOn, id)
	// 如果有依赖但前置任务未完成, 状态设为 blocked
	status := "pending"
	if len(dependsOn) > 0 {
		status = "blocked"
	}
	rec := v2TaskRecord{
		ID: id, Subject: subject, Description: description,
		Status: status, Owner: owner, DependsOn: dependsOn,
		Priority: priority, WriteScopes: uniqueTrimmedStringsTask(writeScopes),
		CreatedAt: now, UpdatedAt: now,
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

// filterTaskDependsOn 清洗依赖列表: 去空白、丢弃空串/自依赖/重复项。
func filterTaskDependsOn(dependsOn []string, selfID string) []string {
	if len(dependsOn) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(dependsOn))
	filtered := make([]string, 0, len(dependsOn))
	for _, dep := range dependsOn {
		dep = strings.TrimSpace(dep)
		if dep == "" || dep == selfID || seen[dep] {
			continue
		}
		seen[dep] = true
		filtered = append(filtered, dep)
	}
	return filtered
}

// filterCycleCreatingDeps 过滤掉会为目标任务引入有向环的依赖.
// 在 resume 场景下, 动态 file-write/conflict 依赖可能因任务处理顺序反转而产生反向依赖,
// 与显式依赖叠加后形成死循环. 此方法通过 BFS 检查添加 bestID -> dep 是否会形成环
// (即 dep 的依赖链中是否已包含 bestID).
func (s *TaskStore) filterCycleCreatingDeps(bestID string, dependsOn []string) []string {
	if len(dependsOn) == 0 {
		return nil
	}
	filtered := make([]string, 0, len(dependsOn))
	for _, dep := range dependsOn {
		if dep == bestID {
			continue // 自依赖已在 filterTaskDependsOn 中过滤, 双重保险
		}
		if s.isReachable(dep, bestID) {
			// 添加 bestID -> dep 会形成环, 跳过此依赖
			continue
		}
		filtered = append(filtered, dep)
	}
	return filtered
}

// isReachable 从 startID 出发沿依赖边 BFS, 检查是否能到达 targetID.
func (s *TaskStore) isReachable(startID, targetID string) bool {
	visited := make(map[string]bool)
	queue := []string{startID}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if curr == targetID {
			return true
		}
		if visited[curr] {
			continue
		}
		visited[curr] = true
		if rec, ok := s.byID[curr]; ok {
			for _, dep := range rec.DependsOn {
				if !visited[dep] {
					queue = append(queue, dep)
				}
			}
		}
	}
	return false
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
//
// 依赖满足语义 (all_done): completed 或 failed 都算"已处理完毕"。
// 这与 SetTaskStatusAndUnblock 的 unblock 语义保持一致 — 否则会出现:
//  1. 上游任务 hard-gate failed → SetTaskStatusAndUnblock 将下游 blocked → pending (用 all_done)
//  2. ReadyTasks 仍要求依赖 completed → 已 pending 的下游永远进不了 ready 队列
//  3. 调度器 10 分钟后触发 stall recovery → 把 failed 任务重置成 pending → 重新执行 → 再失败
//
// 死循环的根因。修复后, failed 依赖也视为已满足, 下游可正常向前推进。
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
			d, ok := s.byID[dep]
			if !ok {
				allDepsOK = false
				break
			}
			// all_done 语义: completed 和 failed 都算"已处理完毕".
			// 失败依赖不再阻塞下游, 让 LLM 仍有机会基于残缺上下文推进.
			if d.Status != "completed" && d.Status != "failed" {
				allDepsOK = false
				break
			}
		}
		if allDepsOK {
			ready = append(ready, TaskSummary{
				ID: r.ID, Subject: r.Subject, Description: r.Description,
				Status: r.Status, Owner: r.Owner, DependsOn: r.DependsOn,
				Priority: r.Priority, WriteScopes: r.WriteScopes,
				CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
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
//
// 终态 (completed/failed) 会**顺带做一次 all_done 解锁扫描**, 与 SetTaskStatusAndUnblock
// 同语义。这不是锦上添花 —— 生产里所有失败路径走的都是**这一个**函数
// (pkg/agent/workflow.go:922、pkg/agent/swarm.go:762、pkg/agent/orchestrator.go:7321),
// 而它此前只改这一个字段、不做任何下游联动。后果实测在台账上:
// 1246 条 blocked 里有 **245 条**的上游已经是 failed, 即永远不会再被解锁的僵尸条目。
//
// 语义是"解锁"不是"级联失败": 上游 failed 计为已处理完毕, 下游 blocked → pending,
// 让模型仍有机会基于残缺上下文推进。硬级联 failed 的老行为已被刻意废弃
// (见 cascadeFailureLocked 的注释: 单点 fail 会炸掉 50+ 兄弟分支)。
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
	if status == "completed" || status == "failed" {
		s.unblockAllDoneLocked()
	}
	return s.saveLocked()
}

// unblockAllDoneLocked 把所有"依赖均已处理完毕"的 blocked 任务置为 pending。
// 调用方必须已持有 s.mu。返回解锁条数。
func (s *TaskStore) unblockAllDoneLocked() int {
	unblocked := 0
	for id, rec := range s.byID {
		if rec.Status != "blocked" {
			continue
		}
		// 注意: 这里**不要**再单独判一次 len(DependsOn)==0。空依赖列表走下面的
		// allDone 循环会自然得到 allDone==true, 与专门的分支行为完全一致 ——
		// 变异测试证实那条分支从不承重。
		allDone := true
		for _, dep := range rec.DependsOn {
			d, exists := s.byID[dep]
			if !exists {
				allDone = false
				break
			}
			if d.Status != "completed" && d.Status != "failed" {
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
	return unblocked
}

// pending 与 blocked 的默认存活上限。超过即判为无人认领的孤儿条目。
//
// 取值依据: pending 的正常寿命是"被 Submit 到 runner 接手"这一段, 生产上是秒级;
// 30 分钟给了"先 Submit、稍后才注入 runner"的余量 (Resume 明确支持这条路, 见
// taskservice.go 的 `case TaskStatePending` 分支)。blocked 还要叠加上游的真实执行
// 时间, 所以给到 6 小时。
const (
	DefaultPendingTTL = 30 * time.Minute
	DefaultBlockedTTL = 6 * time.Hour
)

// TaskSweepResult 是一次对账的结果计数, 分原因返回以便定位。
type TaskSweepResult struct {
	Unblocked      int // blocked → pending (依赖已全部处理完毕)
	ExpiredPending int // pending → failed (超时无人认领)
	ExpiredBlocked int // blocked → failed (依赖悬空, 见 DanglingDeps)
	DanglingDeps   int // 其中因依赖 ID 在台账里根本不存在而作废的
}

// Total 返回本次对账改动的条目总数。
func (r TaskSweepResult) Total() int {
	return r.Unblocked + r.ExpiredPending + r.ExpiredBlocked
}

// SweepStale 对台账做一次对账, 消灭两类吸收态。调用方必须自己决定何时调用
// (它**不会**在 NewTaskStore 里自动跑 —— 打开一个台账不该顺手改用户数据)。
//
// 两类吸收态:
//  1. blocked 而依赖已全部终态 (或依赖 ID 悬空): 没有任何事件会再来解锁它。
//     512 条台账实测里 245 条属于这一类。
//  2. pending 超过 pendingTTL: 提交后没有任何执行器接手。taskservice 的
//     startRun 在 runner == nil 时**静默返回**, 任务就此永久停在 pending。
//
// TTL <= 0 表示该维度不做过期 (用于只想解锁、不想判死的场景)。
func (s *TaskStore) SweepStale(now time.Time, pendingTTL, blockedTTL time.Duration) TaskSweepResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	var res TaskSweepResult

	// 第 1 遍: 依赖悬空的 blocked 先判死 —— 它们等的是一个不存在的 ID。
	for id, rec := range s.byID {
		if rec.Status != "blocked" {
			continue
		}
		dangling := false
		for _, dep := range rec.DependsOn {
			if _, ok := s.byID[dep]; !ok {
				dangling = true
				break
			}
		}
		if dangling {
			s.failLocked(id, &rec, "依赖的任务 ID 不在台账里 (悬空引用)")
			res.ExpiredBlocked++
			res.DanglingDeps++
		}
	}

	// 第 2 遍: 依赖已全部终态的 blocked → pending。放在过期之前, 因为被解锁的条目
	// UpdatedAt 会被刷新, 于是它不会在同一轮里又被当成超时条目判死。
	res.Unblocked = s.unblockAllDoneLocked()

	// 第 3 遍: 超时。
	if pendingTTL > 0 {
		for id, rec := range s.byID {
			if rec.Status != "pending" {
				continue
			}
			if s.staleLocked(rec, now, pendingTTL) {
				s.failLocked(id, &rec, fmt.Sprintf("pending 超过 %s 无人接手", pendingTTL))
				res.ExpiredPending++
			}
		}
	}
	if blockedTTL > 0 {
		for id, rec := range s.byID {
			if rec.Status != "blocked" {
				continue
			}
			if s.staleLocked(rec, now, blockedTTL) {
				s.failLocked(id, &rec, fmt.Sprintf("blocked 超过 %s 仍未解锁", blockedTTL))
				res.ExpiredBlocked++
			}
		}
	}

	if res.Total() > 0 {
		_ = s.saveLocked()
	}
	return res
}

// SweepStaleTasks 是 pkg/agent 那侧 DAGTaskTracker 接口要的形态 (返回改动条数而非结果结构),
// 让编排层不必 import 本包就能对账。语义与 SweepStale 完全一致。
func (s *TaskStore) SweepStaleTasks(pendingTTL, blockedTTL time.Duration) int {
	return s.SweepStale(time.Now().UTC(), pendingTTL, blockedTTL).Total()
}

// staleLocked 判断一条记录是否已超过 TTL。时间戳用 UpdatedAt, 缺省回退 CreatedAt;
// 两者都读不出来的记录算作 stale —— 一条无法判断年龄的条目正是对账要处理的对象。
func (s *TaskStore) staleLocked(rec v2TaskRecord, now time.Time, ttl time.Duration) bool {
	ts := rec.UpdatedAt
	if ts == "" {
		ts = rec.CreatedAt
	}
	if ts == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return true
	}
	return now.Sub(t) > ttl
}

// failLocked 把一条记录推入 failed 终态并记下原因。调用方必须已持有 s.mu。
func (s *TaskStore) failLocked(id string, rec *v2TaskRecord, reason string) {
	rec.Status = "failed"
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if rec.Description == "" {
		rec.Description = reason
	}
	s.byID[id] = *rec
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
			Priority: r.Priority, WriteScopes: r.WriteScopes,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return result
}

// ReevaluateBlockedTasks 扫描所有 blocked 任务, 若其所有依赖均已 completed/failed,
// 则将其状态改为 pending. 专用于 resume 场景: 原始运行中已完成的检查点任务不会触发
// SetTaskStatusAndUnblock, 导致下游 blocked 任务永久卡住.
func (s *TaskStore) ReevaluateBlockedTasks() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	affected := 0
	for id, rec := range s.byID {
		if rec.Status != "blocked" {
			continue
		}
		allDone := true
		for _, dep := range rec.DependsOn {
			d, exists := s.byID[dep]
			if !exists {
				allDone = false
				break
			}
			if d.Status != "completed" && d.Status != "failed" {
				allDone = false
				break
			}
		}
		if allDone {
			rec.Status = "pending"
			rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			s.byID[id] = rec
			affected++
		}
	}
	if affected > 0 {
		_ = s.saveLocked()
	}
	return affected
}

// SetTaskStatusAndUnblock 原子更新状态并解除下游依赖 (DAG 联动)。
// 修复4项:
//  1. completed 和 failed 都 unblock 下游 (之前 failed 不 unblock → DAG 全卡)
//  2. 原子操作: 一次加锁同时完成 setStatus + unblock (之前分两次锁, 有竞态窗口)
//  3. 依赖检查改为 all_done 语义 (completed/failed 都算"处理完毕")
//  4. 失败路径不再硬级联标记下游 failed, 而是同样尝试 unblock (failed 计为 "done");
//     这样上游任务失败不会无脑阻塞兄弟分支, 让 LLM 仍有机会基于残缺上下文产出可用代码。
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
	if status == "completed" || status == "failed" {
		// 统一语义: 任何"已处理完毕"的状态都触发下游 unblock 检查.
		// 下游 blocked → pending 仅当所有依赖均已 completed 或 failed.
		// failed 依赖不再阻塞兄弟分支, 也不再级联标记下游 failed.
		for depID, depRec := range s.byID {
			if depRec.Status != "blocked" {
				continue
			}
			allDone := true
			for _, dep := range depRec.DependsOn {
				d, exists := s.byID[dep]
				if !exists {
					allDone = false
					break
				}
				if d.Status != "completed" && d.Status != "failed" {
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
	}

	_ = s.saveLocked()
	return affected, nil
}

// cascadeFailureLocked 递归将依赖 failedID 的下游任务标记为 failed.
//
// 已废弃: 当前 SetTaskStatusAndUnblock 不再调用此函数. 历史行为是失败级联阻塞,
// 在 LLM 质量不稳的多任务编排中会导致大面积无谓阻塞 (单点 fail → 50+ 兄弟分支 fail).
// 改为 all_done 语义后, 失败任务仍计为 "已处理完毕", 不会硬性阻塞下游兄弟.
// 保留函数仅为兼容外部调用方; 调用方应改用 SetTaskStatusAndUnblock(failed).
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
