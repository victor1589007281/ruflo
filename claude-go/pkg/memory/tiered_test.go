package memory

// tiered_test.go —— 这个包此前**一个测试都没有**, 而它刚被 -race 抓出一个真实的数据竞争
// (后台落盘序列化 entry 指针 vs Retrieve 里的 Touch 改同一对象)。
//
// 所以这里只写两类测试, 都对着那个缺陷本身:
//   ① 并发检索 + 落盘 —— 必须在 `-race` 下干净; 这是唯一能自动发现该缺陷的形态。
//   ② 快照是**值拷贝** —— 光靠 ① 不够: race 检测器只在两个访问真的交错时才报,
//      而"拷了指针"这件事在低并发下可能一路绿。所以对不变式单独下断言。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Test持久化_并发检索时无竞争 —— 缺陷的原始形态: Add 起后台落盘、Retrieve 同时 Touch。
//
// ⚠️ 这个测试**只有在 `-race` 下才有牙**: 不带 -race 时旧代码也能跑绿（竞争多半不改变
// 可观测结果）。CI 若只跑无 -race 的一遍, 等于没验。
func Test持久化_并发检索时无竞争(t *testing.T) {
	dir := t.TempDir()
	s := NewTieredStoreWithPersist(dir)

	// 先放一批可被检索到的条目 (Importance 0.9 ⇒ 每次 Add 都会触发一次后台落盘)。
	for i := 0; i < 40; i++ {
		s.Add(&MemoryEntry{
			Content:    "团队 alpha 交付了报告 事件溯源 快照恢复",
			Source:     "team_result",
			Importance: 0.9,
			Topics:     []string{"team", "report"},
		})
	}

	var wg sync.WaitGroup
	// 检索方: Retrieve 内部对命中的 entry 调 Touch(), 改 AccessCount/LastAccess。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = s.Retrieve("团队 报告", 5)
		}
	}()
	// 写入方: 继续 Add 高权重条目, 每条都再起一次后台落盘。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			s.Add(&MemoryEntry{Content: "追加记忆", Source: "manual", Importance: 0.85})
		}
	}()
	wg.Wait()
	s.WaitPersist() // 等后台落盘收敛, 否则 t.TempDir() 清理会与写盘竞态

	// 落盘结果必须是**完整可解析**的 JSON: 撕裂写/半个对象会在这里现形。
	raw, err := os.ReadFile(filepath.Join(dir, "episodic_memory.json"))
	if err != nil {
		t.Fatalf("读持久化文件: %v", err)
	}
	var got []MemoryEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("持久化文件不是合法 JSON: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("持久化文件里一条记忆都没有")
	}
	// 时间字段必须是**合理**的: 撕裂读 time.Time 能写出无意义时间戳, 而下次 loadFromDisk
	// 会拿它算 Retention() —— 一条记忆可能因此被判成早该遗忘并丢弃。
	now := time.Now()
	for _, e := range got {
		if e.LastAccess.After(now.Add(time.Minute)) || e.LastAccess.Before(now.Add(-24*time.Hour)) {
			t.Fatalf("LastAccess 不合理 (疑似撕裂读): %v (entry %s)", e.LastAccess, e.ID)
		}
		if e.AccessCount < 0 {
			t.Fatalf("AccessCount 为负 (疑似撕裂读): %d", e.AccessCount)
		}
	}
}

// Test快照是值拷贝_与原对象无共享 —— 对不变式直接下断言。
//
// 为什么不能只靠上面那个并发测试: race 检测器要两个访问真的交错才报, 而"快照拷的是指针"
// 在低并发下可以一路绿。这里把不变式本身钉住: 改原对象不该影响已取的快照, 且 Topics
// 这个唯一的引用字段也不能共享底层数组 (半个深拷贝比不拷更难查)。
func Test快照是值拷贝_与原对象无共享(t *testing.T) {
	e := &MemoryEntry{
		ID: "m1", Content: "x", Importance: 0.9,
		AccessCount: 1, LastAccess: time.Now(),
		Topics: []string{"a", "b"},
	}
	snap := e.snapshot()

	e.Touch() // 模拟 Retrieve 的并发改写
	e.Topics[0] = "改了"

	if snap.AccessCount != 1 {
		t.Fatalf("快照的 AccessCount 被原对象改动影响: got %d want 1", snap.AccessCount)
	}
	if snap.Topics[0] != "a" {
		t.Fatalf("快照与原对象共享 Topics 底层数组: got %q want \"a\"", snap.Topics[0])
	}
	// nil Topics 不该被拷成空切片: 那会让 JSON 从省略字段变成 "topics":[]。
	e2 := &MemoryEntry{ID: "m2"}
	if s2 := e2.snapshot(); s2.Topics != nil {
		t.Fatalf("nil Topics 应保持 nil (否则 omitempty 失效, JSON 形态变了): %v", s2.Topics)
	}
}
