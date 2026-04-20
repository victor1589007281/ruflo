package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TaskSnapshot 检查点中的单个任务状态快照。
type TaskSnapshot struct {
	ID          string    `json:"id"`
	State       TaskState `json:"state"`
	Retries     int       `json:"retries"`
	Error       string    `json:"error,omitempty"`
	Output      any       `json:"output,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	CompletedAt time.Time `json:"completedAt,omitempty"`
}

// ExecutionMetrics 引擎级别的执行指标。
type ExecutionMetrics struct {
	TotalTasks     int           `json:"totalTasks"`     // 总任务数
	CompletedTasks int           `json:"completedTasks"` // 已完成数
	FailedTasks    int           `json:"failedTasks"`    // 已失败数
	CancelledTasks int           `json:"cancelledTasks"` // 已取消数
	SuspendedTasks int           `json:"suspendedTasks"` // 挂起中数
	TotalRetries   int           `json:"totalRetries"`   // 累计重试次数
	Elapsed        time.Duration `json:"elapsed"`        // 总耗时
}

// ExecutionState 完整的执行状态检查点。
// 包含所有任务的快照和执行指标, 可用于断点续跑。
type ExecutionState struct {
	ExecID    string                  `json:"execId"`
	GraphID   string                  `json:"graphId"`
	GraphName string                  `json:"graphName"`
	Tasks     map[string]TaskSnapshot `json:"tasks"`
	Blackboard map[string]Entry       `json:"blackboard,omitempty"`
	Metrics   ExecutionMetrics        `json:"metrics"`
	SavedAt   time.Time               `json:"savedAt"`
}

// CheckpointStore 检查点存储接口, 支持保存、加载和列举。
type CheckpointStore interface {
	Save(execID string, state *ExecutionState) error
	Load(execID string) (*ExecutionState, error)
	List() ([]string, error)
}

// FileCheckpointStore 文件系统检查点存储实现。
// 每个检查点以 JSON 格式保存到指定目录。
type FileCheckpointStore struct {
	mu  sync.Mutex
	dir string
}

// NewFileCheckpointStore 创建文件检查点存储。
func NewFileCheckpointStore(dir string) *FileCheckpointStore {
	return &FileCheckpointStore{dir: dir}
}

// Save 将检查点写入文件。使用互斥锁防止并发写入同一目录。
func (s *FileCheckpointStore) Save(execID string, state *ExecutionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	state.SavedAt = time.Now()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	path := filepath.Join(s.dir, execID+".json")
	return os.WriteFile(path, data, 0o644)
}

// Load 从文件加载检查点。
func (s *FileCheckpointStore) Load(execID string) (*ExecutionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, execID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取失败: %w", err)
	}
	var state ExecutionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("反序列化失败: %w", err)
	}
	return &state, nil
}

// List 列举所有已保存的检查点 ID。
func (s *FileCheckpointStore) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			ids = append(ids, e.Name()[:len(e.Name())-5])
		}
	}
	return ids, nil
}

// MemoryCheckpointStore 内存检查点存储 (用于测试)。
type MemoryCheckpointStore struct {
	mu    sync.Mutex
	store map[string]*ExecutionState
}

func NewMemoryCheckpointStore() *MemoryCheckpointStore {
	return &MemoryCheckpointStore{store: make(map[string]*ExecutionState)}
}

func (s *MemoryCheckpointStore) Save(execID string, state *ExecutionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *state
	cp.SavedAt = time.Now()
	s.store[execID] = &cp
	return nil
}

func (s *MemoryCheckpointStore) Load(execID string) (*ExecutionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.store[execID]
	if !ok {
		return nil, fmt.Errorf("检查点不存在: %s", execID)
	}
	return st, nil
}

func (s *MemoryCheckpointStore) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.store))
	for id := range s.store {
		ids = append(ids, id)
	}
	return ids, nil
}
