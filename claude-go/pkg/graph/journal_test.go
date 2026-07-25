package graph

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMemoryJournalAppendRead 内存 journal 基本读写与 Seq 分配。
func TestMemoryJournalAppendRead(t *testing.T) {
	j := NewMemoryJournal()
	for _, typ := range []string{EvRunCreated, EvNodeStarted, EvNodeCompleted} {
		if err := j.Append(Event{Type: typ, RunID: "r1", NodeID: "a"}); err != nil {
			t.Fatalf("Append 失败: %v", err)
		}
	}
	evs, err := j.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll 失败: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("事件数 = %d, 期望 3", len(evs))
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("第 %d 条事件 Seq = %d, 期望 %d", i, ev.Seq, i+1)
		}
		if ev.TS == 0 {
			t.Fatalf("第 %d 条事件 TS 未补齐", i)
		}
	}
}

// TestFileJournalRoundTrip 文件 journal 基本读写与重开续 Seq。
func TestFileJournalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	j, err := NewFileJournal(dir)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	j.Append(Event{Type: EvRunCreated, RunID: "r1"})
	j.Append(Event{Type: EvNodeCompleted, RunID: "r1", NodeID: "a",
		Data: map[string]any{"output": "a-out", "score": 88.5}})
	j.Close()

	j2, err := NewFileJournal(dir)
	if err != nil {
		t.Fatalf("重开 NewFileJournal: %v", err)
	}
	defer j2.Close()
	j2.Append(Event{Type: EvRunFinished, RunID: "r1", Data: map[string]any{"status": "completed"}})
	evs, err := j2.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("事件数 = %d, 期望 3", len(evs))
	}
	if evs[2].Seq != 3 {
		t.Fatalf("重开后追加事件 Seq = %d, 期望续接为 3", evs[2].Seq)
	}
	if evs[1].Data["output"] != "a-out" || evs[1].Data["score"] != 88.5 {
		t.Fatalf("Data 未按 JSON 往返: %+v", evs[1].Data)
	}
}

// TestFileJournalCrashSafety 崩溃安全: 尾部写入半行垃圾后 ReadAll 仍解出完整
// 事件, 且重开后追加不被残行污染。
func TestFileJournalCrashSafety(t *testing.T) {
	dir := t.TempDir()
	j, err := NewFileJournal(dir)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := j.Append(Event{Type: EvNodeCompleted, RunID: "r1", NodeID: "n",
			Data: map[string]any{"output": "o"}}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// 模拟崩溃: 尾部留下无换行的半行垃圾。
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("打开文件注入垃圾: %v", err)
	}
	f.WriteString(`{"seq":99,"ts":1,"type":"node.compl`)
	f.Close()

	// 同一实例 ReadAll 容忍尾部截断行。
	evs, err := j.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("垃圾尾行后事件数 = %d, 期望 3", len(evs))
	}
	j.Close()

	// 重开: 治愈残行 (补换行), 追加事件不与残行粘连, Seq 续接。
	j2, err := NewFileJournal(dir)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer j2.Close()
	if err := j2.Append(Event{Type: EvRunFinished, RunID: "r1",
		Data: map[string]any{"status": "partial"}}); err != nil {
		t.Fatalf("重开后 Append: %v", err)
	}
	evs, err = j2.ReadAll()
	if err != nil {
		t.Fatalf("重开后 ReadAll: %v", err)
	}
	if len(evs) != 4 {
		t.Fatalf("重开追加后事件数 = %d, 期望 4", len(evs))
	}
	if last := evs[len(evs)-1]; last.Type != EvRunFinished || last.Seq != 4 {
		t.Fatalf("末事件 = %+v, 期望 run.finished 且 Seq=4", last)
	}
}

// TestReplay 重放语义: completed 从 Data 恢复; 后写覆盖先写; run.finished 置位。
func TestReplay(t *testing.T) {
	evs := []Event{
		{Seq: 1, Type: EvRunCreated, RunID: "r1"},
		{Seq: 2, Type: EvNodeCompleted, RunID: "r1", NodeID: "a",
			Data: map[string]any{"output": "第一轮", "score": 60.0}},
		{Seq: 3, Type: EvNodeFailed, RunID: "r1", NodeID: "b", Data: map[string]any{"error": "boom"}},
		{Seq: 4, Type: EvNodeCompleted, RunID: "r1", NodeID: "a",
			Data: map[string]any{"output": "最后一轮", "score": 85.0}},
		{Seq: 5, Type: EvRunFinished, RunID: "r1", Data: map[string]any{"status": RunStatusPartial}},
	}
	st := Replay(evs)
	if len(st.Completed) != 1 {
		t.Fatalf("Completed 数 = %d, 期望 1 (failed 节点不缓存)", len(st.Completed))
	}
	a := st.Completed["a"]
	if a.Status != NodeStatusCompleted || a.Output != "最后一轮" || a.Score != 85 {
		t.Fatalf("节点 a 重放结果 = %+v, 期望后写覆盖先写", a)
	}
	if !st.Finished || st.Status != RunStatusPartial {
		t.Fatalf("Finished/Status = %v/%q, 期望 true/partial", st.Finished, st.Status)
	}
}

// 回归 B-1：journal 在生产是 per-team 而非 per-run 的，同一团队多次运行把多轮
// 事件追加进同一文件。早期 Replay 只 switch ev.Type、不看 ev.RunID，于是第二轮
// 会把第一轮的 node.completed 当成本轮已完成 → 调度零节点、直接返回上一轮产出
// 并报 completed（refine/重跑静默失效）。
func TestReplay_不跨run混用缓存(t *testing.T) {
	// r1 完整跑完 a、b；r2 刚创建还没跑任何节点
	evs := []Event{
		{Seq: 1, Type: EvRunCreated, RunID: "r1"},
		{Seq: 2, Type: EvNodeCompleted, RunID: "r1", NodeID: "a", Data: map[string]any{"output": "r1-a"}},
		{Seq: 3, Type: EvNodeCompleted, RunID: "r1", NodeID: "b", Data: map[string]any{"output": "r1-b"}},
		{Seq: 4, Type: EvRunFinished, RunID: "r1", Data: map[string]any{"status": RunStatusCompleted}},
		{Seq: 5, Type: EvRunCreated, RunID: "r2"},
	}
	st := Replay(evs)
	if len(st.Completed) != 0 {
		t.Fatalf("新一轮 r2 不应继承 r1 的已完成节点, 实得 %d 条: %+v", len(st.Completed), st.Completed)
	}
	if st.Finished {
		t.Error("r2 尚未 finished, 不应置位")
	}
}

// 同一 run 内崩溃后重启：必须能续跑（这是 Resume 的正当用途，不能被上面的修复误伤）。
func TestReplay_同run崩溃后可续跑(t *testing.T) {
	evs := []Event{
		{Seq: 1, Type: EvRunCreated, RunID: "r1"},
		{Seq: 2, Type: EvNodeCompleted, RunID: "r1", NodeID: "a", Data: map[string]any{"output": "已完成"}},
		// 无 run.finished —— 进程在这里被 kill
	}
	st := Replay(evs)
	if len(st.Completed) != 1 || st.Completed["a"].Output != "已完成" {
		t.Fatalf("未完结的 run 应可续跑, 实得 %+v", st.Completed)
	}
	if st.Finished {
		t.Error("无 run.finished 不应置位")
	}
}

// 完整跑完的 run 不作恢复基线；partial/failed 仍作基线（对齐 checkpoints.json 语义）。
func TestReplay_终态决定是否作恢复基线(t *testing.T) {
	mk := func(status string) []Event {
		return []Event{
			{Seq: 1, Type: EvRunCreated, RunID: "r1"},
			{Seq: 2, Type: EvNodeCompleted, RunID: "r1", NodeID: "a", Data: map[string]any{"output": "o"}},
			{Seq: 3, Type: EvRunFinished, RunID: "r1", Data: map[string]any{"status": status}},
		}
	}
	if got := len(Replay(mk(RunStatusCompleted)).Completed); got != 0 {
		t.Errorf("completed 终态应清空缓存, 实得 %d", got)
	}
	for _, s := range []string{RunStatusPartial, RunStatusFailed} {
		if got := len(Replay(mk(s)).Completed); got != 1 {
			t.Errorf("%s 终态应保留缓存供续跑, 实得 %d", s, got)
		}
	}
}

// 老 journal 没有 run.created 时也不能跨 RunID 混用。
func TestReplay_无runCreated时按RunID兜底(t *testing.T) {
	evs := []Event{
		{Seq: 1, Type: EvNodeCompleted, RunID: "old", NodeID: "a", Data: map[string]any{"output": "旧"}},
		{Seq: 2, Type: EvNodeCompleted, RunID: "new", NodeID: "b", Data: map[string]any{"output": "新"}},
	}
	st := Replay(evs)
	if len(st.Completed) != 1 {
		t.Fatalf("应只取最后一个 RunID 的事件, 实得 %d 条: %+v", len(st.Completed), st.Completed)
	}
	if _, ok := st.Completed["b"]; !ok {
		t.Error("应保留 new 轮的节点 b")
	}
}
