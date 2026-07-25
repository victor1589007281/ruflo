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

	// EvMapExpanded map 节点完成扇出切分 (design/01 §4.2)。
	// Data: count / shards(JSON 数组字符串) / ids / truncated / source / split。
	// **它是 resume 的真源**: 恢复时按事件里记下的分片重建, 不重新切分上游产出
	// —— 上游产出来自 LLM, 重切可能得到不同的分片集, 事件溯源就失效了。
	EvMapExpanded = "map.expanded"
	// EvGraphExpanded 动态展开: 节点产出被并入运行图 (design/01 §4.2)。
	// Data: parent / depth / count / subgraph(JSON 字符串, 已命名空间化)。
	// 恢复时按此重建运行图, 不重新问 runner。
	EvGraphExpanded = "graph.expanded"
	// EvExpandRejected 展开被边界闸拦下 (深度/条数/总量/约束放宽/结构非法)。
	// 只记账不改变节点终态; 有它才能解释"模型给了子图但图没变大"。
	EvExpandRejected = "graph.expand_rejected"
	// EvGroupIteration loop-group 一轮结束 (design/01 §4.4)。
	// Data: iteration / status / output / score。**resume 按已完成轮次续跑**。
	EvGroupIteration = "loop.group.iteration"

	// EvBudgetConsumed 一次节点执行后的预算记账 (design/01 §4.3 事件枚举 + §4.10)。
	// Data: node_runs / node_runs_this, 以及 runner 真回报用量时的 tokens / tokens_total。
	// **重放时不重建台账**: 预算是"本次运行"的量, resume 后续跑应按新预算重新计,
	// 否则一个跑了六轮的图永远无法 resume (旧账已把预算吃满)。
	EvBudgetConsumed = "budget.consumed"
	// EvBudgetExceeded 预算超限致节点被拒 (design/01 §4.10)。
	// Data: kind(node_runs|per_node_runs|wall_clock|tokens) / limit / got / action。
	// 有它才能区分"节点失败"与"没让它跑"。
	EvBudgetExceeded = "budget.exceeded"
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

	// MapShards map 节点 → 上次扇出的分片内容 (按序)。resume 时按此重建分片集,
	// **不重新切分上游产出** (上游是 LLM 产出, 重切可能得到不同分片集)。
	MapShards map[string][]string
	// Expansions 动态展开记录 (按发生序)。resume 时按此重建运行图,
	// 而不是重新问 runner —— 否则恢复会得到与首跑不同的图, 事件溯源就失效了。
	Expansions []ExpandRecord
	// GroupIters loop-group 节点 → 已完成的组轮次状态 (resume 从下一轮续跑)。
	GroupIters map[string]GroupIterState
}

// ExpandRecord 一条已生效的动态展开 (节点 ID 已命名空间化)。
type ExpandRecord struct {
	Parent string
	Depth  int
	Sub    Expansion
}

// GroupIterState loop-group 已完成轮次的快照。
type GroupIterState struct {
	Done   int     // 已完成的轮次数 (= 下一轮的轮次号)
	Status string  // 最后一轮 ResultFrom 节点的状态
	Output string  // 最后一轮产出 (供 Feedback 回灌)
	Score  float64 // 最后一轮评分 (供 Until 求值)
}

// Replay 重放事件序列重建 RunState。
//   - node.completed 从 Data 恢复 NodeResult (output/score); 同一节点多条
//     completed 事件后写覆盖先写 (loop / 多次 resume 场景取最后一轮);
//   - run.finished 置 Finished 与 Status。
//
// resume 语义: Engine 启动时若 RunState 里已有 completed 节点,
// 直接作为缓存产出跳过执行 (见 engine.go)。
func Replay(events []Event) *RunState {
	st := &RunState{
		Completed:  map[string]NodeResult{},
		MapShards:  map[string][]string{},
		GroupIters: map[string]GroupIterState{},
	}

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
			// map 节点的分片结果一并恢复: reduce 在 resume 后可能要重跑, 而它的
			// 上游 map 是缓存命中不再执行的 —— 分片结果只能来自 journal。
			if s, ok := ev.Data["shards"].(string); ok && s != "" {
				var shards []ShardResult
				if json.Unmarshal([]byte(s), &shards) == nil {
					r.Shards = shards
				}
			}
			st.Completed[ev.NodeID] = r
		case EvMapExpanded:
			if ev.NodeID == "" {
				continue
			}
			if s, ok := ev.Data["shards"].(string); ok && s != "" {
				var vals []string
				if json.Unmarshal([]byte(s), &vals) == nil {
					st.MapShards[ev.NodeID] = vals
				}
			}
		case EvGraphExpanded:
			rec := ExpandRecord{Parent: ev.NodeID}
			if s, ok := ev.Data["parent"].(string); ok && s != "" {
				rec.Parent = s
			}
			if f, ok := ev.Data["depth"].(float64); ok {
				rec.Depth = int(f)
			} else if i, ok := ev.Data["depth"].(int); ok {
				rec.Depth = i
			}
			s, _ := ev.Data["subgraph"].(string)
			if s == "" || json.Unmarshal([]byte(s), &rec.Sub) != nil {
				continue // 载荷坏了就当没展开过: 宁可少一段子图, 不要半个图
			}
			st.Expansions = append(st.Expansions, rec)
		case EvGroupIteration:
			if ev.NodeID == "" {
				continue
			}
			gs := st.GroupIters[ev.NodeID]
			gs.Done++
			if s, ok := ev.Data["status"].(string); ok {
				gs.Status = s
			}
			if s, ok := ev.Data["output"].(string); ok {
				gs.Output = s
			}
			if f, ok := ev.Data["score"].(float64); ok {
				gs.Score = f
			}
			st.GroupIters[ev.NodeID] = gs
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
		// 展开/扇出/组轮次同属"上一轮的运行图形态", 必须一起清空: 只清 Completed
		// 会让新一轮继承上一轮展开出来的节点, 得到一张谁都没声明过的图。
		st.MapShards = map[string][]string{}
		st.Expansions = nil
		st.GroupIters = map[string]GroupIterState{}
	}
	return st
}
