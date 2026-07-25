package graph

// journal.go —— 事件溯源 Journal (design/01 §4.3): append-only 事件日志是
// 唯一进度真源, 恢复 = 重放 (Replay), resume 凭 journal 而非快照文件。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 事件类型 (design/01 §4.3 事件枚举的 v1 子集)。
const (
	EvRunCreated    = "run.created"
	EvNodeScheduled = "node.scheduled"
	EvNodeStarted   = "node.started"
	EvNodeCompleted = "node.completed"
	EvNodeFailed    = "node.failed"
	EvNodeRetried   = "node.retried"
	EvNodeSkipped   = "node.skipped"
	EvLoopIteration = "loop.iteration"
	EvRunFinished   = "run.finished"
)

// Event 一条 journal 事件 (design/01 §4.3)。
// Seq 由 Journal 实现分配 (单调递增, 调用方传入值被忽略); TS 为 0 时由实现补当前时间。
type Event struct {
	Seq    int64          `json:"seq"`
	TS     int64          `json:"ts"` // unix milli
	Type   string         `json:"type"`
	RunID  string         `json:"run_id"`
	NodeID string         `json:"node_id,omitempty"`
	Data   map[string]any `json:"data,omitempty"` // node.completed 带 output/score; run.finished 带 status
}

// Journal append-only 事件日志接口。实现必须并发安全 (引擎多 goroutine 追加)。
type Journal interface {
	Append(Event) error
	ReadAll() ([]Event, error)
}

// ---------------------------------------------------------------------------
// MemoryJournal —— 内存实现 (测试用)
// ---------------------------------------------------------------------------

// MemoryJournal 内存 journal, 供测试与无持久化需求的场景。
type MemoryJournal struct {
	mu      sync.Mutex
	lastSeq int64
	events  []Event
}

// NewMemoryJournal 构造内存 journal。
func NewMemoryJournal() *MemoryJournal { return &MemoryJournal{} }

// Append 追加事件, 自动分配 Seq / 补 TS。
func (m *MemoryJournal) Append(ev Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSeq++
	ev.Seq = m.lastSeq
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	m.events = append(m.events, ev)
	return nil
}

// ReadAll 返回全部事件的副本。
func (m *MemoryJournal) ReadAll() ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.events))
	copy(out, m.events)
	return out, nil
}

// ---------------------------------------------------------------------------
// FileJournal —— <dir>/journal.jsonl 单文件 append-only 实现
// ---------------------------------------------------------------------------

// FileJournal 文件 journal: <dir>/journal.jsonl, O_APPEND 单文件。
// 崩溃安全: 写入只追加整行; 读取逐行解码并跳过无法解码的行 (进程崩溃可能留下
// 尾部截断的半行——半行只丢该条事件, 不污染其余); 重新打开时若文件末尾缺换行,
// 先补一个 '\n' 把残行封死, 保证后续追加的事件独占新行、不会与残行粘连。
type FileJournal struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	lastSeq int64
}

// NewFileJournal 打开 (必要时创建) <dir>/journal.jsonl。
// 已有内容会被扫描一遍以初始化 Seq 计数并治愈尾部残行。
func NewFileJournal(dir string) (*FileJournal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "journal.jsonl")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var lastSeq int64
	for _, ev := range decodeJournalLines(data) {
		if ev.Seq > lastSeq {
			lastSeq = ev.Seq
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	// 治愈崩溃残行: 末尾无换行时补 '\n', 后续追加不会与残行粘连。
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := f.Write([]byte("\n")); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &FileJournal{path: path, f: f, lastSeq: lastSeq}, nil
}

// Append 追加一行 JSON 事件, 自动分配 Seq / 补 TS。
func (j *FileJournal) Append(ev Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	ev.Seq = j.lastSeq + 1
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(line, '\n')); err != nil {
		return err
	}
	j.lastSeq = ev.Seq
	return nil
}

// ReadAll 从头读取全部可解码事件 (跳过截断/垃圾行, 崩溃安全)。
func (j *FileJournal) ReadAll() ([]Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	data, err := os.ReadFile(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return decodeJournalLines(data), nil
}

// Close 关闭底层文件句柄。
func (j *FileJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}

// decodeJournalLines 逐行解码, 容忍尾部截断行与垃圾行 (直接跳过)。
func decodeJournalLines(data []byte) []Event {
	if len(data) == 0 {
		return nil
	}
	var out []Event
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // 崩溃截断的半行 / 外部污染行: 只丢该行
		}
		out = append(out, ev)
	}
	return out
}

// ---------------------------------------------------------------------------
// Replay —— 恢复 = 重放 (design/01 §4.3)
// ---------------------------------------------------------------------------

// RunState 重放 journal 得到的运行状态快照。
type RunState struct {
	Completed map[string]NodeResult // 已完成节点 (按节点 ID), 后写覆盖先写
	Finished  bool                  // 是否已记 run.finished
	Status    string                // run.finished 携带的最终状态
}

// Replay 重放事件序列重建 RunState。
//   - node.completed 从 Data 恢复 NodeResult (output/score); 同一节点多条
//     completed 事件后写覆盖先写 (loop / 多次 resume 场景取最后一轮);
//   - run.finished 置 Finished 与 Status。
//
// resume 语义: Engine 启动时若 RunState 里已有 completed 节点,
// 直接作为缓存产出跳过执行 (见 engine.go)。
func Replay(events []Event) *RunState {
	st := &RunState{Completed: map[string]NodeResult{}}

	// —— 只重放"最近一次 run"的事件 ——
	//
	// journal 在生产是 per-team 而非 per-run 的 (graph_adapter 落
	// <team.dataDir>/graph-journal/journal.jsonl), 同一团队多次运行会把多轮
	// 事件追加进同一文件。早期实现只 switch ev.Type、完全不看 ev.RunID,
	// 于是第二次运行会把上一轮的 node.completed 全部当成本轮已完成 →
	// ready-set 一开始就发现所有节点都在终态表里 → **调度零个节点、直接返回
	// 上一轮的产出并报 completed**。refine 与重跑因此静默失效 (且 refine 的
	// "整体重跑"只删 checkpoints.json, 不碰 journal)。
	//
	// 定位方式: 最后一个 run.created 之后的事件即本轮; 同时按该 RunID 过滤,
	// 双保险防止 journal 里有交错写入 (并发/残留)。
	start := 0
	lastRun := ""
	for i, ev := range events {
		if ev.Type == EvRunCreated {
			start, lastRun = i, ev.RunID
		}
	}
	if lastRun == "" {
		// 无 run.created (老 journal 或只落了节点事件): 退化为取最后一条带
		// RunID 的事件作为本轮标识, 至少不跨 run 混用。
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].RunID != "" {
				lastRun = events[i].RunID
				break
			}
		}
	}

	for _, ev := range events[start:] {
		if lastRun != "" && ev.RunID != "" && ev.RunID != lastRun {
			continue
		}
		switch ev.Type {
		case EvNodeCompleted:
			if ev.NodeID == "" {
				continue
			}
			r := NodeResult{Status: NodeStatusCompleted}
			if s, ok := ev.Data["output"].(string); ok {
				r.Output = s
			}
			if f, ok := ev.Data["score"].(float64); ok {
				r.Score = f
			}
			st.Completed[ev.NodeID] = r
		case EvRunFinished:
			st.Finished = true
			if s, ok := ev.Data["status"].(string); ok {
				st.Status = s
			}
		}
	}

	// 已**完整跑完**的 run 不是"待恢复"的基线: 再次被调用意味着新一轮
	// (refine / 重跑), 应从头执行。清空缓存但保留 Finished/Status 供调用方判断。
	//
	// partial/failed 仍保留缓存: 那是"部分完成待续跑", 复用已成功节点、重试
	// 其余——与 checkpoints.json 的既有语义一致, 不改变用户可感知行为。
	if st.Finished && st.Status == RunStatusCompleted {
		st.Completed = map[string]NodeResult{}
	}
	return st
}
