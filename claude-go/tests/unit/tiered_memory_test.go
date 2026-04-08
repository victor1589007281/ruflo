// 多层记忆系统测试
package unit

import (
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/memory"
)

func TestTieredStore_AddAndRetrieve(t *testing.T) {
	store := memory.NewTieredStore()

	store.Add(&memory.MemoryEntry{
		Content:    "用户喜欢使用 Go 语言开发后端服务",
		Topics:     []string{"go", "backend"},
		Source:     "extraction",
		Importance: 0.8,
	})
	store.Add(&memory.MemoryEntry{
		Content:    "项目使用 PostgreSQL 数据库",
		Topics:     []string{"database", "postgresql"},
		Source:     "extraction",
		Importance: 0.7,
	})
	store.Add(&memory.MemoryEntry{
		Content:    "天气预报显示明天下雨",
		Topics:     []string{"weather"},
		Source:     "extraction",
		Importance: 0.3,
	})

	if store.Count() != 3 {
		t.Errorf("期望 3 条记忆, 实际 %d", store.Count())
	}

	// 检索 Go 相关
	results := store.Retrieve("Go 语言后端开发", 5)
	if len(results) == 0 {
		t.Fatal("应找到 Go 相关记忆")
	}
	if !containsSubstr(results[0].Content, "Go") {
		t.Errorf("最相关结果应包含 Go: %s", results[0].Content)
	}

	// 检索数据库相关
	results = store.Retrieve("数据库 PostgreSQL 查询", 5)
	if len(results) == 0 {
		t.Fatal("应找到数据库相关记忆")
	}
}

func TestTieredStore_ForgetCurve(t *testing.T) {
	store := memory.NewTieredStore()

	// 添加一条很旧的低重要性记忆
	old := &memory.MemoryEntry{
		Content:    "临时调试信息",
		Source:     "extraction",
		Importance: 0.2,
		CreatedAt:  time.Now().Add(-720 * time.Hour), // 30天前
		LastAccess: time.Now().Add(-720 * time.Hour),
	}
	store.Add(old)

	// 添加一条新的高重要性记忆
	fresh := &memory.MemoryEntry{
		Content:    "重要架构决策: 使用事件溯源",
		Source:     "extraction",
		Importance: 0.9,
	}
	store.Add(fresh)

	// 旧的低重要性记忆应已大幅衰减
	if old.Retention() >= 0.3 {
		t.Logf("旧记忆保留度: %.4f (30天前, importance=0.2)", old.Retention())
		t.Error("30天前的低重要性记忆保留度应低于 0.3")
	}

	// 新的高重要性记忆应保留度高
	if fresh.Retention() < 0.8 {
		t.Errorf("新记忆保留度应高于 0.8: %.4f", fresh.Retention())
	}

	// Prune 应清理低保留度记忆
	pruned := store.Prune()
	if pruned < 1 {
		t.Error("应至少清理 1 条过期记忆")
	}
	if store.Count() != 1 {
		t.Errorf("清理后应剩 1 条, 实际 %d", store.Count())
	}
}

func TestTieredStore_SpacedRepetition(t *testing.T) {
	entry := &memory.MemoryEntry{
		Content:    "间隔重复测试",
		Importance: 0.5,
		CreatedAt:  time.Now().Add(-48 * time.Hour),
		LastAccess: time.Now().Add(-48 * time.Hour),
	}

	ret1 := entry.Retention()

	// 模拟被召回 5 次
	for i := 0; i < 5; i++ {
		entry.Touch()
	}

	ret2 := entry.Retention()

	if ret2 <= ret1 {
		t.Errorf("间隔重复后保留度应提高: before=%.4f, after=%.4f", ret1, ret2)
	}
	t.Logf("间隔重复效果: %.4f → %.4f (5次召回)", ret1, ret2)
}

func TestTieredStore_RetrievalTouches(t *testing.T) {
	store := memory.NewTieredStore()

	store.Add(&memory.MemoryEntry{
		Content:    "API 接口使用 RESTful 设计",
		Topics:     []string{"api", "rest"},
		Source:     "extraction",
		Importance: 0.7,
	})

	// 第一次检索
	results := store.Retrieve("api 接口设计", 5)
	if len(results) == 0 {
		t.Fatal("应找到 API 相关记忆")
	}
	if results[0].AccessCount != 1 {
		t.Errorf("第一次检索后 accessCount 应为 1: %d", results[0].AccessCount)
	}

	// 第二次检索
	store.Retrieve("api 接口", 5)
	if results[0].AccessCount != 2 {
		t.Errorf("第二次检索后 accessCount 应为 2: %d", results[0].AccessCount)
	}
}

func TestFormatForPrompt(t *testing.T) {
	entries := []*memory.MemoryEntry{
		{Content: "记忆条目 A", Topics: []string{"test"}},
		{Content: "记忆条目 B", Topics: []string{"test"}},
	}

	result := memory.FormatForPrompt(entries)
	if !containsSubstr(result, "relevant_memories") {
		t.Error("应包含 relevant_memories 标签")
	}
	if !containsSubstr(result, "记忆条目 A") || !containsSubstr(result, "记忆条目 B") {
		t.Error("应包含所有记忆内容")
	}
}

func TestFormatForPrompt_Empty(t *testing.T) {
	result := memory.FormatForPrompt(nil)
	if result != "" {
		t.Error("空记忆应返回空字符串")
	}
}

func TestTieredStore_TopK(t *testing.T) {
	store := memory.NewTieredStore()

	for i := 0; i < 20; i++ {
		store.Add(&memory.MemoryEntry{
			Content:    "go 编程记忆条目",
			Source:     "extraction",
			Importance: 0.5,
		})
	}

	results := store.Retrieve("go 编程", 3)
	if len(results) > 3 {
		t.Errorf("topK=3 但返回 %d 条", len(results))
	}
}
