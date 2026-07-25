package graph

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 失效事件必须让该节点在下一次 resume 时真的重跑 —— 这是事件溯源的 refine:
// 进度真源始终是 journal, 不靠删快照文件"让它忘记"。
func TestInvalidateFrom_节点重跑(t *testing.T) {
	spec := linearSpec("a", "b", "c")

	// 基线: 让 c 失败, run 停在 partial —— 只有未完结的 run 才是"待恢复基线"
	// (完整跑完的 run 再被调用即新一轮, 既有语义会清空缓存重跑全部, 与失效无关)。
	j := NewMemoryJournal()
	rf := newICRunner()
	rf.fail["c"] = true
	if _, err := (&Engine{Runner: rf, Journal: j, sleepFn: noSleep}).Run(
		context.Background(), spec, RunOpts{RunID: "run-p"}); err != nil {
		t.Fatal(err)
	}

	// 对照: 不失效直接续跑 → a/b 吃缓存, 只有 c 重跑
	r3 := newICRunner()
	if _, err := (&Engine{Runner: r3, Journal: NewMemoryJournalFrom(j), sleepFn: noSleep}).Run(
		context.Background(), spec, RunOpts{RunID: "run-p", Resume: true}); err != nil {
		t.Fatal(err)
	}
	if r3.count("a") != 0 || r3.count("b") != 0 || r3.count("c") != 1 {
		t.Fatalf("对照组不对: a=%d b=%d c=%d, 期望 0/0/1", r3.count("a"), r3.count("b"), r3.count("c"))
	}

	// 失效 b 后续跑: b 必须重跑, a 仍吃缓存
	j2 := NewMemoryJournalFrom(j)
	if err := InvalidateFrom(j2, "run-p", []string{"b"}, "refine:b"); err != nil {
		t.Fatal(err)
	}
	r4 := newICRunner()
	if _, err := (&Engine{Runner: r4, Journal: j2, sleepFn: noSleep}).Run(
		context.Background(), spec, RunOpts{RunID: "run-p", Resume: true}); err != nil {
		t.Fatal(err)
	}
	if r4.count("b") != 1 {
		t.Errorf("失效后 b 执行 %d 次, 期望 1 —— 失效事件没生效, refine 就静默无效了", r4.count("b"))
	}
	if r4.count("a") != 0 {
		t.Errorf("未失效的 a 执行了 %d 次, 不该重跑", r4.count("a"))
	}
}

// noSleep 免掉重试退避的真实等待。
func noSleep(context.Context, time.Duration) bool { return true }

// NewMemoryJournalFrom 复制一份 journal (让每个分支从同一基线出发互不干扰)。
func NewMemoryJournalFrom(src *MemoryJournal) *MemoryJournal {
	evs, _ := src.ReadAll()
	out := NewMemoryJournal()
	for _, ev := range evs {
		ev.Seq = 0
		_ = out.Append(ev)
	}
	return out
}

// RunID 写错的失效事件会被 Replay 静默过滤 —— 所以 runID 为空必须直接报错,
// 而不是写出一条注定不生效的事件。
func TestInvalidateFrom_缺RunID报错(t *testing.T) {
	j := NewMemoryJournal()
	if err := InvalidateFrom(j, "", []string{"a"}, "x"); err == nil {
		t.Error("缺 runID 应报错 (否则事件会被 Replay 静默过滤)")
	}
	if err := InvalidateFrom(nil, "r", []string{"a"}, "x"); err == nil {
		t.Error("nil journal 应报错")
	}
	// 空/空白节点名被跳过而不报错
	if err := InvalidateFrom(j, "r", []string{"", "   "}, "x"); err != nil {
		t.Errorf("空节点名应被跳过: %v", err)
	}
	evs, _ := j.ReadAll()
	if len(evs) != 0 {
		t.Errorf("空节点名却写了 %d 条事件", len(evs))
	}
}

// 失效必须连带摘掉该节点派生出来的运行图形态: 分片集/组轮次/展开产物/子节点缓存。
// 留着它们等于"失效了一半" —— 重跑的节点会继承上一轮的扇出与轮次进度。
func TestInvalidateFrom_连带摘掉派生形态(t *testing.T) {
	evs := []Event{
		{Seq: 1, Type: EvRunCreated, RunID: "r"},
		{Seq: 2, Type: EvNodeCompleted, RunID: "r", NodeID: "m", Data: map[string]any{"output": "x"}},
		{Seq: 3, Type: EvMapExpanded, RunID: "r", NodeID: "m", Data: map[string]any{"shards": `["a","b"]`}},
		{Seq: 4, Type: EvNodeCompleted, RunID: "r", NodeID: "m#0", Data: map[string]any{"output": "s0"}},
		{Seq: 5, Type: EvNodeCompleted, RunID: "r", NodeID: "m/child", Data: map[string]any{"output": "c"}},
		{Seq: 6, Type: EvNodeCompleted, RunID: "r", NodeID: "m" + SpawnIDInfix + "abc/sub", Data: map[string]any{"output": "sp"}},
		{Seq: 7, Type: EvGroupIteration, RunID: "r", NodeID: "m", Data: map[string]any{"status": "completed"}},
		{Seq: 8, Type: EvGraphExpanded, RunID: "r", NodeID: "m",
			Data: map[string]any{"parent": "m", "depth": float64(1), "subgraph": `{"nodes":[{"id":"z","kind":"agent","agent":{"role":"w"}}]}`}},
		{Seq: 9, Type: EvNodeCompleted, RunID: "r", NodeID: "other", Data: map[string]any{"output": "keep"}},
		{Seq: 10, Type: EvNodeInvalidated, RunID: "r", NodeID: "m", Data: map[string]any{"reason": "refine"}},
	}
	st := Replay(evs)
	for _, gone := range []string{"m", "m#0", "m/child", "m" + SpawnIDInfix + "abc/sub"} {
		if _, ok := st.Completed[gone]; ok {
			t.Errorf("%q 仍在 Completed 里 —— 派生产物没被连带摘掉", gone)
		}
	}
	if _, ok := st.MapShards["m"]; ok {
		t.Error("分片集没被摘掉: 重跑会继承上一轮的扇出")
	}
	if _, ok := st.GroupIters["m"]; ok {
		t.Error("组轮次没被摘掉: 重跑会从上一轮轮次继续")
	}
	if len(st.Expansions) != 0 {
		t.Errorf("展开记录没被摘掉 (%d 条): 重跑会与旧子图并存", len(st.Expansions))
	}
	if _, ok := st.Completed["other"]; !ok {
		t.Error("未失效的节点被误摘了")
	}
}

// 事件顺序敏感: 失效之后又完成的节点应算已完成 (重跑成功了)。
func TestInvalidateFrom_失效后重新完成(t *testing.T) {
	st := Replay([]Event{
		{Seq: 1, Type: EvRunCreated, RunID: "r"},
		{Seq: 2, Type: EvNodeCompleted, RunID: "r", NodeID: "a", Data: map[string]any{"output": "旧"}},
		{Seq: 3, Type: EvNodeInvalidated, RunID: "r", NodeID: "a"},
		{Seq: 4, Type: EvNodeCompleted, RunID: "r", NodeID: "a", Data: map[string]any{"output": "新"}},
	})
	got, ok := st.Completed["a"]
	if !ok {
		t.Fatal("失效后重新完成的节点应算已完成")
	}
	if got.Output != "新" {
		t.Errorf("产出 = %q, 期望取重跑后的 新", got.Output)
	}
}

// 失效事件本身要留痕 (谁在什么时候因什么失效了哪些节点)。
func TestInvalidateFrom_留痕(t *testing.T) {
	j := NewMemoryJournal()
	if err := InvalidateFrom(j, "r1", []string{"impl", "review"}, "refine:impl"); err != nil {
		t.Fatal(err)
	}
	evs, _ := j.ReadAll()
	if len(evs) != 2 {
		t.Fatalf("事件数 = %d, 期望 2", len(evs))
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		if ev.Type != EvNodeInvalidated || ev.RunID != "r1" {
			t.Errorf("事件不对: %+v", ev)
		}
		if r, _ := ev.Data["reason"].(string); !strings.Contains(r, "refine:impl") {
			t.Errorf("原因未留痕: %v", ev.Data)
		}
		seen[ev.NodeID] = true
	}
	if !seen["impl"] || !seen["review"] {
		t.Errorf("失效节点不全: %v", seen)
	}
}
