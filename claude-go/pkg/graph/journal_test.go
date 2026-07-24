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
