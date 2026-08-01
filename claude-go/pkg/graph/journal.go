package graph

// journal.go —— 事件溯源 Journal (design/01 §4.3): append-only 事件日志是
// 唯一进度真源, 恢复 = 重放 (Replay), resume 凭 journal 而非快照文件。

import (
	"encoding/json"
	"errors"
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

	// EvLoopTerminated 节点级 Loop 被**可插拔终止器**终止 (design/01 §4.4, terminator.go)。
	// Data: terminator / signal (哪一路信号) / detail / iteration / iterations /
	// score / status, 以及 best-of-N 回滚的结局 (rollback=applied|suppressed|
	// rejected|unresolved + best_round + rollback_reason)。
	// 没有它, "为什么第 3 轮就停了"与"为什么产出不是最后一轮的"事后完全不可解释 ——
	// 五路信号里有三路 (收敛/退化/回滚) 从产出本身看不出任何痕迹。
	EvLoopTerminated = "loop.terminated"
	// EvGroupTerminated loop-group 被终止器终止 (键同 EvLoopTerminated, 另带
	// result_from / output —— 组的最终产出经 Replay 用于 resume)。
	//
	// **它是组循环 resume 的真源**: 组的轮次进度靠 loop.group.iteration 计数,
	// 但"终止器已经判定该停了"这件事不在轮次里 —— 少了这条事件, resume 会带着一个
	// 全新 (无历史) 的终止器把剩下的轮次继续跑完, 等于把早停判定作废。
	EvGroupTerminated = "loop.group.terminated"

	// EvNodeInvalidated 节点被显式失效 (design/01 §4.3 refine 凭 InvalidateFrom)。
	// Data: reason。Replay 见到它就把该节点从 Completed 里摘掉, 于是下一次 resume
	// 会重跑它 —— **这才是事件溯源的 refine**: 进度真源始终是 journal, 而不是靠
	// 删掉某个快照文件来"让它忘记"。
	EvNodeInvalidated = "node.invalidated"

	// EvSubgraphSpawned 节点内 agent 派生了子图 (design/01 §4.8)。
	// Data: spawn(请求指纹) / namespace / nodes / result_from / depth / subgraph。
	// 有它 subagent 才对编排层可见 —— 改造前派生的裸 QueryEngine 一条痕迹都不留。
	EvSubgraphSpawned = "subgraph.spawned"
	// EvSubgraphRejected 派生被边界闸拦下 (深度/次数/条数/总量/结构非法)。
	// 没有它, "agent 说它派生了但图里什么都没有"在事后完全不可解释。
	EvSubgraphRejected = "subgraph.rejected"

	// EvNodeSuspended 节点挂起 (NodeStatusSuspended, 见 suspend.go)。
	// Data: reason / revive_after_ms / revives / human(bool) / prompt(human 节点)。
	// **它是挂起态 resume 的真源**: 没有它, 崩溃重启后 Replay 只知道"这个节点没完成",
	// 分辨不出是"根本没跑过"还是"跑过且在等人" —— 后者要把上次挂起的原因与答复
	// 一起交回给节点 (NodeInput.Revive), 前者不能。
	EvNodeSuspended = "node.suspended"
	// EvNodeRevived 挂起节点在**同一次运行内**被重新派发 (in-run revive)。
	// Data: revive(第几次) / after_ms / clamped(等待时长是否被夹到上限) / reason。
	// Replay 见到它就把该节点从挂起态里摘掉 (它已经被叫起来了, 后续要么终态要么再挂)。
	EvNodeRevived = "node.revived"
	// EvHumanRequested human 节点第一次发现没有答复 (design/01 §4.3 事件枚举)。
	// Data: prompt (要问什么) / role。平台读它去问人, 答复经 RespondHuman 写回。
	EvHumanRequested = "human.requested"
	// EvHumanResponded 外部答复到达 (design/01 §4.3)。**由平台经 RespondHuman 追加**,
	// 不是引擎自己发的 —— 引擎在下一次 resume 时凭 Replay 读到它, 于是 human 节点
	// 完成而不再挂起。Data: response / by。
	EvHumanResponded = "human.responded"
	// EvSubgraphEntered subgraph 节点解析出被引用的图并开始执行 (§4.1, subgraph.go)。
	// Data: graph / version / namespace / nodes / result_from / depth / source
	// (registry|journal) / subgraph(JSON 字符串)。
	// **subgraph 载荷是 resume 的真源**: 注册表是进程内可变的 (热注册工作流会改它),
	// 若 resume 时重新去注册表取, 恢复出的图可能与首跑不是同一张 —— 与
	// map.expanded/graph.expanded 同一条红线 (恢复出的图必须与首跑一致)。
	EvSubgraphEntered = "subgraph.entered"

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

// DecodeJournal 解码一份 journal.jsonl 的原始字节 (容忍尾部截断行)。
//
// 给**只读**旁路用 (团队层的 team.json 投影, 见 pkg/agent/team_projection.go):
// 那条路要在不创建目录、不持写句柄、不泄 fd 的前提下读整个文件, 而
// NewFileJournal 会 MkdirAll + 开一个写句柄 —— 遍历全部团队目录时会给每个从未跑过图
// 的团队凭空造出一个空 journal。
func DecodeJournal(data []byte) []Event { return decodeJournalLines(data) }

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

	// Suspended 上一次运行结束时仍处于**挂起态**的节点 (按限定 NodeID)。
	// 挂起节点**不进** Completed —— 它没跑完, 缓存它等于永远挂着。resume 会重跑它,
	// 并把这里的原因/答复经 NodeInput.Revive 交回去, 于是节点知道自己是续跑
	// (human 节点据此直接完成而不是再问一遍)。
	Suspended map[string]SuspendState
	// HumanResponses 已到达的人工答复 (按限定 NodeID)。由平台经 RespondHuman 追加。
	HumanResponses map[string]HumanResponse
	// Subgraphs subgraph 节点 → 上一次运行**实际跑的那张子图**(已冻结)。
	// resume 时按此重建, 不重新查注册表 —— 注册表是进程内可变的 (热注册),
	// 重查可能得到与首跑不同的图 (与 Expansions 同一理由)。
	Subgraphs map[string]SubgraphRecord
}

// SuspendState 一个节点在上一次运行结束时的挂起态 (Replay 重建)。
type SuspendState struct {
	Reason string // 挂起原因 (node.suspended 的 reason)
	Human  bool   // 是否在等人工答复 (human 节点 / runner 自报 human)
	Prompt string // 要问什么 (human.requested 的 prompt), 供平台直接展示
	// Revives 上一次运行内已用掉的唤醒次数 (仅记账与展示)。
	// **不影响新一轮的额度** —— 见 Revival.Count 的注释。
	Revives int
}

// HumanResponse 一条人工答复 (Replay 重建)。
type HumanResponse struct {
	Response string
	TS       int64 // unix milli
}

// SubgraphRecord 一次 subgraph 节点解析出的子图 (已冻结, 与 journal 一起存档)。
type SubgraphRecord struct {
	Graph      string    // 被引用的图名
	Version    string    // 被引用图的版本 (纯记账: 用于事后判断注册表是否漂移过)
	ResultFrom string    // 取哪个成员的产出
	Sub        GraphSpec // 当时真正执行的那张图
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
	// Terminated 上一次运行里终止器已判定该组结束 (loop.group.terminated)。
	// resume 见到它就直接拿 Output/Score/Status 收尾, **一轮都不再跑** ——
	// 否则新进程里的终止器没有历史 (收敛/退化都是跨轮判断), 会把早停判定作废,
	// 表现为"明明第 2 轮就该停, resume 之后又跑到第 5 轮"。
	Terminated bool
	// TermSignal 终止信号名 (仅记账/可观测用)。
	TermSignal string
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
		Completed:      map[string]NodeResult{},
		MapShards:      map[string][]string{},
		GroupIters:     map[string]GroupIterState{},
		Suspended:      map[string]SuspendState{},
		HumanResponses: map[string]HumanResponse{},
		Subgraphs:      map[string]SubgraphRecord{},
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
			// 终态到了 ⇒ 它不再是挂起态 (同一次运行里"挂起→revive→完成"是常态)。
			delete(st.Suspended, ev.NodeID)
		case EvNodeFailed, EvNodeSkipped:
			// 只做一件事: 摘掉挂起态。挂起是**非终态**, 任何终态事件都终结它;
			// 不摘的后果是 resume 时把一个已经失败/跳过的节点当成"还在等人"。
			if ev.NodeID != "" {
				delete(st.Suspended, ev.NodeID)
			}
		case EvNodeSuspended:
			if ev.NodeID == "" {
				continue
			}
			// 合并而不是覆盖: human.requested 与 node.suspended 各带一半信息
			// (前者带问题原文, 后者带原因/次数), 两条事件的先后顺序不该影响结果。
			ss := st.Suspended[ev.NodeID]
			if s, ok := ev.Data["reason"].(string); ok {
				ss.Reason = s
			}
			if b, ok := ev.Data["human"].(bool); ok && b {
				ss.Human = true
			}
			if s, ok := ev.Data["prompt"].(string); ok && s != "" {
				ss.Prompt = s
			}
			if f, ok := ev.Data["revives"].(float64); ok {
				ss.Revives = int(f)
			} else if i, ok := ev.Data["revives"].(int); ok {
				ss.Revives = i
			}
			st.Suspended[ev.NodeID] = ss
		case EvNodeRevived:
			// 已被叫起来 ⇒ 不再是"上一次运行结束时还挂着"的状态。
			// 它之后要么落终态 (上面两支), 要么再发一条 node.suspended。
			if ev.NodeID != "" {
				delete(st.Suspended, ev.NodeID)
			}
		case EvHumanRequested:
			if ev.NodeID == "" {
				continue
			}
			ss := st.Suspended[ev.NodeID]
			ss.Human = true
			if s, ok := ev.Data["prompt"].(string); ok {
				ss.Prompt = s
			}
			st.Suspended[ev.NodeID] = ss
		case EvHumanResponded:
			if ev.NodeID == "" {
				continue
			}
			hr := HumanResponse{TS: ev.TS}
			if s, ok := ev.Data["response"].(string); ok {
				hr.Response = s
			}
			st.HumanResponses[ev.NodeID] = hr
		case EvSubgraphEntered:
			if ev.NodeID == "" {
				continue
			}
			s, _ := ev.Data["subgraph"].(string)
			if s == "" {
				continue
			}
			rec := SubgraphRecord{}
			if json.Unmarshal([]byte(s), &rec.Sub) != nil {
				continue // 载荷坏了就当没记过: 下一次按注册表重新解析 (并重新记账)
			}
			if v, ok := ev.Data["graph"].(string); ok {
				rec.Graph = v
			}
			if v, ok := ev.Data["version"].(string); ok {
				rec.Version = v
			}
			if v, ok := ev.Data["result_from"].(string); ok {
				rec.ResultFrom = v
			}
			st.Subgraphs[ev.NodeID] = rec
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
		case EvGroupTerminated:
			// 终止事件在最后一条 loop.group.iteration **之后**追加, 故在这里覆盖
			// Output/Score/Status 得到的正是组真正交付的那一份 (best-of-N 回滚后
			// 它与最后一轮不同 —— 只看轮次事件会拿到被回滚掉的那份劣化产出)。
			if ev.NodeID == "" {
				continue
			}
			gs := st.GroupIters[ev.NodeID]
			gs.Terminated = true
			if s, ok := ev.Data["signal"].(string); ok {
				gs.TermSignal = s
			}
			if s, ok := ev.Data["output"].(string); ok {
				gs.Output = s
			}
			if s, ok := ev.Data["status"].(string); ok && s != "" {
				gs.Status = s
			}
			if f, ok := ev.Data["score"].(float64); ok {
				gs.Score = f
			}
			st.GroupIters[ev.NodeID] = gs
		case EvNodeInvalidated:
			if ev.NodeID == "" {
				continue
			}
			// 摘掉缓存的同时, 该节点派生出来的运行图形态也要一起摘: 分片集/组轮次
			// 留着会让重跑的节点继承上一轮的扇出与轮次进度, 等于"失效了一半"。
			delete(st.Completed, ev.NodeID)
			delete(st.MapShards, ev.NodeID)
			delete(st.GroupIters, ev.NodeID)
			// 挂起态/人工答复/子图快照同属"该节点上一轮的进度", 一并摘掉。
			// 人工答复必须摘: 失效的语义是"这个节点要重新走一遍", 留着旧答复会让
			// 重跑的 human 节点用一份陈旧答复静默通过, 而人根本没被再问一次。
			delete(st.Suspended, ev.NodeID)
			delete(st.HumanResponses, ev.NodeID)
			delete(st.Subgraphs, ev.NodeID)
			// 展开产物同理: 该节点上一轮展开出的子图不该在重跑前就存在
			// (否则重跑会与旧子图并存, 得到一张谁都没声明过的图)。
			if len(st.Expansions) > 0 {
				kept := st.Expansions[:0]
				for _, rec := range st.Expansions {
					if rec.Parent != ev.NodeID {
						kept = append(kept, rec)
					}
				}
				st.Expansions = kept
			}
			// 该节点展开/派生/子图引用出来的子节点缓存也要摘 (它们的 ID 带父节点前缀)。
			// SubgraphIDInfix 这一条不能漏: 漏了会让一个被失效的 subgraph 节点重跑时
			// 直接吃到上一轮全部成员的缓存 ⇒ "失效"只失效了容器, 里面一步没重跑。
			for id := range st.Completed {
				if invalidatedChildID(ev.NodeID, id) {
					delete(st.Completed, id)
				}
			}
			for id := range st.Suspended {
				if invalidatedChildID(ev.NodeID, id) {
					delete(st.Suspended, id)
				}
			}
			for id := range st.HumanResponses {
				if invalidatedChildID(ev.NodeID, id) {
					delete(st.HumanResponses, id)
				}
			}
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
		// 挂起态/答复/子图快照同理。**答复必须清**: 上一轮已交付, 新一轮是新的一次
		// 询问, 拿旧答复顶替等于人没被问就替他答了 (与 node.invalidated 同一口径)。
		st.Suspended = map[string]SuspendState{}
		st.HumanResponses = map[string]HumanResponse{}
		st.Subgraphs = map[string]SubgraphRecord{}
	}
	return st
}

// invalidatedChildID 判断限定 ID 是否属于被失效节点派生出的子节点
// (展开 <父>/<子> / 派生 <父>~sp<指纹>/<子> / 子图 <父>~sg/<成员> / 分片 <父>#<i>)。
func invalidatedChildID(parent, id string) bool {
	return strings.HasPrefix(id, parent+GroupMemberIDSep) ||
		strings.HasPrefix(id, parent+SpawnIDInfix) ||
		strings.HasPrefix(id, parent+SubgraphIDInfix) ||
		strings.HasPrefix(id, parent+ShardIDSep)
}

// InvalidateFrom 显式失效一组节点 (design/01 §4.3: "refine 凭 InvalidateFrom(nodeID) 事件")。
//
// 为什么必须有它: 改造前的 refine 走"删快照文件"这条路 —— 整体重跑删
// graph-journal 目录, 按阶段精修只删 checkpoints.json。后者在图模式下**完全失效**:
// 灰度开关开着时 wf.Mode 仍是 "pipeline", 于是按阶段精修不会被转成整体重跑, 但它
// 只动 checkpoints.json 而 graph-journal 原样保留 → Resume 重放全部 node.completed
// → 零节点执行、直接返回旧产出, 用户的反馈静默消失。
//
// 有了失效事件, 进度真源始终是 journal 本身: 不再需要"删掉某个文件来让它忘记",
// 而且失效这件事自己也留了痕 (谁在什么时候因什么失效了哪些节点)。
//
// runID 必须是**要失效的那次运行**的 ID —— Replay 只重放最近一次 run, 事件写错
// RunID 就会被过滤掉、静默不生效。
func InvalidateFrom(j Journal, runID string, nodeIDs []string, reason string) error {
	if j == nil {
		return errors.New("graph: InvalidateFrom 需要 journal")
	}
	if runID == "" {
		return errors.New("graph: InvalidateFrom 需要 runID (写错 RunID 的失效事件会被 Replay 静默过滤)")
	}
	var firstErr error
	for _, id := range nodeIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		err := j.Append(Event{
			TS: time.Now().UnixMilli(), Type: EvNodeInvalidated,
			RunID: runID, NodeID: id,
			Data: map[string]any{"reason": reason},
		})
		if err != nil && firstErr == nil {
			firstErr = err // 继续写其余的: 少失效一个节点比整批不生效好排查
		}
	}
	return firstErr
}
