// consolidator_gate_test.go —— 13.8.6 P1 记忆更新门的语义锁定。
//
// 规划锚点 (The Past Is Prologue, arXiv:2606.31121): 记忆整理器 (dreaming) 加
// "更新守门" —— 低置信新事实只追加不覆盖, 冲突需反思级置信才替换。
//
// 核心断言:
//  1. 低置信 (Importance < 0.8) 矛盾事实: 只追加 + 记 contradicts 连接, 既有记忆不动。
//  2. 反思级置信 (>= 0.8) 矛盾事实: 裁决 —— 旧条目归档, 新事实入库, 记 supersedes 连接。
//  3. evergreen 旧条目永远不可被裁决 (永久豁免), 即使新事实达反思级。
//  4. M2 验收判据: 守门在 **dreaming 路径** (IncrementalDistill 全链) 有覆盖测试。
//  5. 无矛盾事实走 GateAppended, 与老 Add 行为等价。
//
// 夹具注意: NewMemoryFact 对 Importance >= 0.9 置 Evergreen —— 裁决夹具的新事实
// 用 0.85 (达反思级但非 evergreen), 旧事实用 0.7。FactStore 用空 persistDir 免磁盘
// IO (Add 对 importance>=0.8 会异步 PersistToDisk, 与 t.TempDir 清理有竞态)。
package dreaming

import (
	"context"
	"testing"

	"github.com/anthropic/claude-go/pkg/memory"
)

// stubGateLLM 可编程的 SimpleComplete 假件 (llmExtractFacts 路径)。
type stubGateLLM struct{ reply string }

func (f stubGateLLM) SimpleComplete(_ context.Context, _, _ string) (string, error) {
	return f.reply, nil
}

// newGateHarness 构造 FactStore (免磁盘) + Consolidator (无 LLM → 启发式提取)。
func newGateHarness(t *testing.T) (*memory.FactStore, *Consolidator) {
	t.Helper()
	fs := memory.NewFactStore("")
	c := NewConsolidator(fs, nil, nil)
	return fs, c
}

// sess 造一条带主题的会话记录 (启发式路径: 整个摘要单条 fact, 重要性随会话)。
func sess(summary string, importance float64) SessionRecord {
	return SessionRecord{
		ChatID: "c1", Turns: 3,
		Summary: summary, Topics: []string{"logdb"}, Source: "user_chat",
		Importance: importance,
	}
}

// seedOldFact 旧事实: "已修复/正常" 标记词 (isContradiction 的 fixedMarkers 命中)。
// importance 0.7: 达不到反思级, 也不是 evergreen。
func seedOldFact(fs *memory.FactStore) *memory.MemoryFact {
	old := memory.NewMemoryFact("", "登录崩溃已修复, 一切正常", memory.CategoryEvent, 0.7, "chat", []string{"logdb"})
	fs.Add(old)
	return old
}

// 低置信矛盾新事实 → 只追加, 旧条目存活 (Prologue 主语义)。
func TestGate_低置信矛盾只追加不覆盖(t *testing.T) {
	fs, c := newGateHarness(t)
	old := seedOldFact(fs)
	before := fs.Count()

	res, err := c.IncrementalDistill(context.Background(), []SessionRecord{
		sess("登录崩溃已修复, 一切正常", 0.5), // 与旧事实同内容 → 不矛盾
		sess("登录问题仍有问题, 未修复", 0.5), // 矛盾标记词命中, 但置信 0.5 < 0.8
	})
	if err != nil {
		t.Fatalf("IncrementalDistill: %v", err)
	}
	if res.GateKept == 0 {
		t.Fatalf("低置信矛盾应走 GateKept (只追加), res=%+v", res)
	}
	if fs.Count() != before+2 {
		t.Fatalf("低置信矛盾事实应追加不覆盖: before=%d after=%d", before, fs.Count())
	}
	if old.Archived {
		t.Fatal("低置信矛盾不得归档既有记忆")
	}
	found := false
	for _, conn := range fs.Connections() {
		if conn.Relation == "contradicts" && conn.FactIDB == old.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("低置信矛盾应记 contradicts 连接, conns=%+v", fs.Connections())
	}
}

// 反思级置信矛盾新事实 → 裁决: 旧条目归档 + supersedes 连接。
// 新事实用 0.85: 达反思级但 < 0.9 (非 evergreen, 避免 NewMemoryFact 的自动豁免)。
func TestGate_反思级置信才裁决(t *testing.T) {
	fs, c := newGateHarness(t)
	old := seedOldFact(fs)

	res, err := c.IncrementalDistill(context.Background(), []SessionRecord{
		sess("登录问题仍有问题, 未修复", 0.85),
	})
	if err != nil {
		t.Fatalf("IncrementalDistill: %v", err)
	}
	if res.Superseded == 0 {
		t.Fatalf("反思级置信矛盾应裁决 (Superseded), res=%+v", res)
	}
	if !old.Archived {
		t.Fatal("裁决后旧条目应被归档")
	}
	found := false
	for _, conn := range fs.Connections() {
		if conn.Relation == "supersedes" && conn.FactIDB == old.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("裁决应记 supersedes 连接, conns=%+v", fs.Connections())
	}
}

// evergreen 旧条目永不可被裁决 —— 即使新事实达反思级置信。
func TestGate_evergreen不可裁决(t *testing.T) {
	fs, c := newGateHarness(t)
	ever := memory.NewMemoryFact("", "架构决策: 双层存储, 一切正常", memory.CategoryFact, 0.95, "user", []string{"logdb"})
	if !ever.Evergreen {
		t.Fatal("夹具应产生 evergreen 事实")
	}
	fs.Add(ever)

	res, err := c.IncrementalDistill(context.Background(), []SessionRecord{
		sess("架构决策已废弃, 问题仍有问题", 0.85),
	})
	if err != nil {
		t.Fatalf("IncrementalDistill: %v", err)
	}
	if ever.Archived {
		t.Fatal("evergreen 永久豁免, 不得被裁决归档")
	}
	if res.GateKept == 0 {
		t.Fatalf("evergreen 矛盾应走 GateKept (只追加+记连接), res=%+v", res)
	}
	found := false
	for _, conn := range fs.Connections() {
		if conn.Relation == "contradicts" && conn.FactIDB == ever.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("evergreen 矛盾应记 contradicts 连接, conns=%+v", fs.Connections())
	}
}

// M2 全链: LLM 提取路径 (llmExtractFacts) 同样过门 —— 低置信不裁决, 反思级裁决。
func TestGate_LLM路径同样过门(t *testing.T) {
	fs := memory.NewFactStore("")
	old := memory.NewMemoryFact("", "缓存已修复", memory.CategoryEvent, 0.7, "chat", []string{"cache"})
	fs.Add(old)

	// LLM 吐一条矛盾事实; 事实置信 = sess.Importance。
	llm := stubGateLLM{reply: "[fact] 缓存仍有问题, 未修复"}
	c := NewConsolidator(fs, llm, nil)

	// 低置信会话 (0.5) → GateKept, 旧条目存活。
	res, err := c.IncrementalDistill(context.Background(), []SessionRecord{
		{ChatID: "c1", Summary: "缓存问题讨论", Topics: []string{"cache"}, Source: "user_chat", Importance: 0.5},
	})
	if err != nil {
		t.Fatalf("IncrementalDistill: %v", err)
	}
	if res.GateKept == 0 {
		t.Fatalf("LLM 低置信矛盾应走 GateKept, res=%+v", res)
	}
	if old.Archived {
		t.Fatal("低置信矛盾不得归档旧条目")
	}

	// 反思级会话 (0.85) → Superseded 裁决。
	res2, err := c.IncrementalDistill(context.Background(), []SessionRecord{
		{ChatID: "c2", Summary: "缓存复盘", Topics: []string{"cache"}, Source: "user_chat", Importance: 0.85},
	})
	if err != nil {
		t.Fatalf("IncrementalDistill#2: %v", err)
	}
	if res2.Superseded == 0 {
		t.Fatalf("LLM 反思级矛盾应裁决, res2=%+v", res2)
	}
	if !old.Archived {
		t.Fatal("裁决后旧条目应被归档")
	}
}

// 无矛盾事实 → GateAppended (守门对正常路径零扰动)。
func TestGate_无矛盾直通(t *testing.T) {
	fs, c := newGateHarness(t)
	res, err := c.IncrementalDistill(context.Background(), []SessionRecord{
		sess("新增了 rate limiter 模块的设计决策", 0.7),
	})
	if err != nil {
		t.Fatalf("IncrementalDistill: %v", err)
	}
	if res.Superseded != 0 || res.GateKept != 0 || res.NewFacts != 1 {
		t.Fatalf("无矛盾应直通: res=%+v", res)
	}
	if fs.Count() != 1 {
		t.Fatalf("应有 1 条活跃事实, got %d", fs.Count())
	}
}
