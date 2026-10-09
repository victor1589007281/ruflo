package memory

// blob_test.go —— 原文快照。
//
// 这套测试针对的是**一个很容易做对、但错了也不会报错**的性质: 摘要仍然进提示词,
// 原文仍然取得回来。任何一边坏掉, 现象都是静默的 —— 要么模型看到一条没有句柄的
// 记忆 (退化成改前的行为, 没人会发现), 要么句柄指不到东西 (模型调工具才发现)。
// 所以在盘上把这两件事都断言死。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newBlobStore(t *testing.T) (*TieredStore, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewTieredStoreWithPersist(dir)
	t.Cleanup(s.WaitPersist)
	return s, dir
}

// 摘要进 Content、原文进快照, 且能按 Ref 逐字节取回 —— 这是整个机制的全部意义。
func Test快照_原文可按句柄取回(t *testing.T) {
	s, dir := newBlobStore(t)
	summary := "短摘要: 团队完成了 JD 分析"
	original := summary + strings.Repeat("这是未被截断的完整产出。", 200)

	e := &MemoryEntry{Content: summary, Source: "team_result", Importance: 0.9}
	s.AddWithOriginal(e, original)

	if e.Ref == "" {
		t.Fatal("原文远长于 Content, 应当产生 Ref")
	}
	got, ok := s.ReadOriginal(e.Ref)
	if !ok {
		t.Fatalf("按 Ref=%q 取不回原文", e.Ref)
	}
	if got != original {
		t.Fatalf("原文不是逐字节相同: 长度 %d vs %d", len(got), len(original))
	}
	// 快照要落在记忆目录下 —— 它是"记忆的一部分", 不该跑到别处去。
	if _, err := os.Stat(filepath.Join(dir, blobDirName, e.Ref+blobExt)); err != nil {
		t.Fatalf("快照文件不在预期位置: %v", err)
	}
}

// 原文比 Content 长不了多少 (只是头部包装的差异) 时不该存 —— 存了只会让模型
// 多跑一次工具, 却读到几乎一样的东西。
func Test快照_原文没长多少就不存(t *testing.T) {
	s, _ := newBlobStore(t)
	e := &MemoryEntry{Content: "一段摘要", Source: "manual", Importance: 0.9}
	s.AddWithOriginal(e, "一段摘要") // 完全相同
	if e.Ref != "" {
		t.Fatalf("原文与 Content 相同, 不应产生 Ref, 实际 %q", e.Ref)
	}
}

// 不带原文的 Add 走老路径: Content 照常写入, Ref 留空。
func Test快照_Add不带原文时无Ref(t *testing.T) {
	s, _ := newBlobStore(t)
	e := &MemoryEntry{Content: strings.Repeat("x", 5000), Source: "extraction"}
	s.Add(e)
	if e.Ref != "" {
		t.Fatalf("Add 不应产生 Ref, 实际 %q", e.Ref)
	}
	if s.Count() != 1 {
		t.Fatalf("记忆本身必须照常写入, 实际 %d 条", s.Count())
	}
}

// 内容寻址: 同一条原文存两次落在同一个文件上 —— 这是"不需要去重表"的前提。
func Test快照_相同原文共用一个句柄(t *testing.T) {
	s, _ := newBlobStore(t)
	original := strings.Repeat("同一条团队产出。", 100)

	a := &MemoryEntry{Content: "摘要A", Importance: 0.9}
	b := &MemoryEntry{Content: "摘要B", Importance: 0.9}
	s.AddWithOriginal(a, original)
	s.AddWithOriginal(b, original)

	if a.Ref == "" || a.Ref != b.Ref {
		t.Fatalf("相同原文应得同一句柄: %q vs %q", a.Ref, b.Ref)
	}
}

// 句柄来自磁盘上的记忆条目, 可能被外部改过。非法句柄必须被**拒绝**, 而不是被拼进
// 路径 —— 否则 "../episodic_memory" 这类值就能把读取带出 blobs/ 目录。
func Test快照_非法句柄被拒绝(t *testing.T) {
	s, dir := newBlobStore(t)
	// 放一个合法的记忆文件在上一级, 作为"越界读"的靶子。
	if err := os.WriteFile(filepath.Join(dir, "episodic_memory.json"), []byte("SECRET"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"",
		"../../episodic_memory",             // 短且含非十六进制
		strings.Repeat("a", blobRefLen-1),   // 长度不对
		strings.Repeat("a", blobRefLen+1),   // 长度不对
		strings.Repeat("z", blobRefLen),     // 长度对但不是十六进制
		strings.Repeat("../", blobRefLen/3), // 长度对但非十六进制
	} {
		if got, ok := s.ReadOriginal(bad); ok {
			t.Fatalf("非法句柄 %q 不应读成功, 实际读到 %q", bad, got)
		}
	}
}

// 未启用持久化 (纯内存模式) 时整个机制关闭: 那时连记忆都不落盘, 单独留一份原文
// 只会制造"记忆没了、引用还在"的假象。
func Test快照_不落盘时不产生Ref(t *testing.T) {
	s := NewTieredStore()
	e := &MemoryEntry{Content: "摘要", Importance: 0.9}
	s.AddWithOriginal(e, strings.Repeat("很长的原文。", 100))
	if e.Ref != "" {
		t.Fatalf("未启用持久化时不应产生 Ref, 实际 %q", e.Ref)
	}
}

// 原文超大时不存 —— 半截原文比没有原文更坏 (模型会以为拿到的是全文)。
func Test快照_超限不存(t *testing.T) {
	s, _ := newBlobStore(t)
	e := &MemoryEntry{Content: "摘要", Importance: 0.9}
	s.AddWithOriginal(e, strings.Repeat("x", blobMaxRunes+1))
	if e.Ref != "" {
		t.Fatalf("超过 %d 字的原文不应存快照, 实际 %q", blobMaxRunes, e.Ref)
	}
}

// Ref 必须随条目一起落盘、随重启一起恢复 —— 否则进程重启后所有句柄都指空,
// 而这一条在运行期是看不出来的。
func Test快照_Ref随记忆落盘并恢复(t *testing.T) {
	dir := t.TempDir()
	s := NewTieredStoreWithPersist(dir)
	original := strings.Repeat("跨重启的原文。", 100)
	e := &MemoryEntry{Content: "摘要", Source: "team_result", Importance: 0.9}
	s.AddWithOriginal(e, original)
	s.WaitPersist() // 等 Add 起的后台落盘结束

	// 新实例从磁盘恢复。
	s2 := NewTieredStoreWithPersist(dir)
	got, ok := s2.ReadOriginal(e.Ref)
	if !ok {
		t.Fatalf("重启后 Ref=%q 取不回原文", e.Ref)
	}
	if got != original {
		t.Fatal("重启后原文与写入时不一致")
	}
}
