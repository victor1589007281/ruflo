package unit

import (
	"os"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/memory"
)

func TestMemoryFact_Retention(t *testing.T) {
	t.Run("Fact category decays slowly", func(t *testing.T) {
		f := memory.NewMemoryFact("f1", "func HandleAuth() error", memory.CategoryFact, 0.8, "test", nil)
		r := f.Retention()
		if r < 0.7 {
			t.Errorf("Fact retention should be high immediately, got %f", r)
		}
	})

	t.Run("Context category decays fast", func(t *testing.T) {
		f := memory.NewMemoryFact("c1", "临时调试信息", memory.CategoryContext, 0.5, "test", nil)
		f.LastAccess = time.Now().Add(-24 * time.Hour)
		r := f.Retention()
		if r >= 0.3 {
			t.Errorf("Context should decay fast after 24h, got %f", r)
		}
	})

	t.Run("Evergreen facts never forget", func(t *testing.T) {
		f := memory.NewMemoryFact("e1", "Critical architecture", memory.CategoryFact, 0.95, "test", nil)
		if !f.Evergreen {
			t.Error("Fact with importance >= 0.9 should be evergreen")
		}
		f.LastAccess = time.Now().Add(-720 * time.Hour)
		r := f.Retention()
		if r < 0.9 {
			t.Errorf("Evergreen retention should stay at importance, got %f", r)
		}
	})

	t.Run("Spaced repetition strengthens", func(t *testing.T) {
		f := memory.NewMemoryFact("s1", "Some fact", memory.CategoryEvent, 0.6, "test", nil)
		f.LastAccess = time.Now().Add(-48 * time.Hour)
		before := f.Retention()
		f.Touch()
		f.Touch()
		f.Touch()
		after := f.Retention()
		if after <= before {
			t.Errorf("Touch should strengthen retention: before=%f, after=%f", before, after)
		}
	})
}

func TestFactStore_CRUD(t *testing.T) {
	dir := t.TempDir()
	fs := memory.NewFactStore(dir)

	fs.Add(memory.NewMemoryFact("", "func main() { ... }", memory.CategoryFact, 0.8, "test", []string{"go", "main"}))
	fs.Add(memory.NewMemoryFact("", "用户喜欢TDD", memory.CategoryPreference, 0.6, "test", []string{"tdd"}))
	fs.Add(memory.NewMemoryFact("", "临时调试", memory.CategoryContext, 0.3, "test", nil))

	if fs.Count() != 3 {
		t.Errorf("Expected 3 facts, got %d", fs.Count())
	}

	results := fs.Retrieve("main function go", 2)
	if len(results) == 0 {
		t.Error("Expected to retrieve at least 1 fact for 'main function go'")
	}

	catFacts := fs.GetByCategory(memory.CategoryPreference)
	if len(catFacts) != 1 {
		t.Errorf("Expected 1 preference fact, got %d", len(catFacts))
	}
}

func TestFactStore_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	fs1 := memory.NewFactStore(dir)
	fs1.Add(memory.NewMemoryFact("persist-1", "架构决策: 使用事件溯源", memory.CategoryFact, 0.85, "test", []string{"architecture"}))
	fs1.Add(memory.NewMemoryFact("persist-2", "部署完成 v2.0", memory.CategoryEvent, 0.7, "test", nil))
	fs1.PersistToDisk()

	if _, err := os.Stat(dir + "/memory_facts.json"); os.IsNotExist(err) {
		t.Fatal("memory_facts.json should exist after persist")
	}

	fs2 := memory.NewFactStore(dir)
	if fs2.Count() != 2 {
		t.Errorf("Reloaded store should have 2 facts, got %d", fs2.Count())
	}
}

func TestIngestor_Classify(t *testing.T) {
	fs := memory.NewFactStore("")
	ts := memory.NewTieredStore()
	ig := memory.NewIngestor(fs, ts)

	tests := []struct {
		content  string
		expected memory.MemoryCategory
	}{
		{"func HandleAuth(ctx context.Context) error", memory.CategoryFact},
		{"我喜欢先写测试再写实现", memory.CategoryPreference},
		{"TODO: 完成用户认证模块", memory.CategoryGoal},
		{"部署了 v3.0 到生产环境", memory.CategoryEvent},
		{"刚才讨论了一些想法", memory.CategoryContext},
	}

	for _, tt := range tests {
		f := ig.IngestText(tt.content, "test", nil)
		if f == nil {
			t.Errorf("IngestText should not return nil for: %s", tt.content)
			continue
		}
		if f.Category != tt.expected {
			t.Errorf("For '%s': expected %s, got %s", tt.content, tt.expected, f.Category)
		}
	}
}

func TestDecayManager_RunCycle(t *testing.T) {
	dir := t.TempDir()
	fs := memory.NewFactStore(dir)

	// Add some facts with varying importance
	fs.Add(memory.NewMemoryFact("d1", "Important architecture", memory.CategoryFact, 0.95, "test", nil))
	f2 := memory.NewMemoryFact("d2", "Old debug info", memory.CategoryContext, 0.2, "test", nil)
	f2.LastAccess = time.Now().Add(-72 * time.Hour)
	fs.Add(f2)

	dm := memory.NewDecayManager(fs)
	archived, _ := dm.RunCycle()

	if archived == 0 {
		t.Error("Expected at least 1 archived fact (old context)")
	}

	stats := fs.Stats()
	if stats.EvergreenFacts == 0 {
		t.Error("Expected evergreen facts (importance >= 0.9)")
	}
}
