package harnesspro

import "sync"

// store.go —— 一次进化运行期内的共享状态 (harness 注册表 + 冻结测试卷)。
//
// 线程安全: map 分片并发写 (edit 节点 N 路并行注册新候选), 全部经互斥锁。
// 持久化由 orchestrator 在阶段边界做 (harnesses 落盘即 git commit 的对象),
// store 本身只保运行态 —— 图 journal 才是事件溯源的权威记录。

// Store 一次进化运行的共享注册表。
type Store struct {
	mu       sync.Mutex
	harness  map[string]Harness      // 全部已注册 harness (含 h0 与全部候选)
	tests    map[string][]FrozenTest // 冻结测试卷 (taskID → 10 条), 出卷后只读
	taskByID map[string]Task         // 任务索引
}

// NewStore 构造注册表。
func NewStore(tasks []Task) *Store {
	s := &Store{
		harness:  map[string]Harness{},
		tests:    map[string][]FrozenTest{},
		taskByID: map[string]Task{},
	}
	for _, t := range tasks {
		s.taskByID[t.ID] = t
	}
	return s
}

// RegisterHarness 注册一套 harness (editor 节点产出新候选时调用)。
func (s *Store) RegisterHarness(h Harness) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.harness[h.ID] = h
}

// Harness 读取一套 harness。
func (s *Store) Harness(id string) (Harness, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.harness[id]
	return h, ok
}

// Harnesses 快照全部 harness (报告用)。
func (s *Store) Harnesses() []Harness {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Harness, 0, len(s.harness))
	for _, h := range s.harness {
		out = append(out, h)
	}
	return out
}

// FreezeTests 冻结一套任务的测试卷 (TestSynth 之后**只读**; 重复冻结拒绝)。
func (s *Store) FreezeTests(taskID string, tests []FrozenTest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tests[taskID]; ok {
		return errFrozen{taskID: taskID}
	}
	cp := append([]FrozenTest(nil), tests...)
	s.tests[taskID] = cp
	return nil
}

// Tests 读取冻结测试卷 (运行期唯一读口; 无卷返回 nil)。
func (s *Store) Tests(taskID string) []FrozenTest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]FrozenTest(nil), s.tests[taskID]...)
}

// Task 任务索引查询。
func (s *Store) Task(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.taskByID[id]
	return t, ok
}

// errFrozen 重复冻结错误。
type errFrozen struct{ taskID string }

func (e errFrozen) Error() string {
	return "harnesspro: 任务 " + e.taskID + " 的测试卷已冻结, 拒绝重写出卷"
}
