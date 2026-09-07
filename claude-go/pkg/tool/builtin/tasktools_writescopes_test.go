package builtin

import (
	"path/filepath"
	"testing"
)

// F13 writeScopes: AddTaskFull 落盘往返 + trim/unique 清洗。

func TestAddTaskFullWriteScopesPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	store := NewTaskStore(path)
	id, err := store.AddTaskFull("带写域任务", "desc", "coder", nil, 1,
		[]string{"  contract:store ", "", "state:mvcc", "contract:store"})
	if err != nil {
		t.Fatalf("AddTaskFull failed: %v", err)
	}
	if id == "" {
		t.Fatalf("expected non-empty id")
	}
	// 薄包装不设写域
	id2, err := store.AddTaskWithDeps("无写域任务", "desc", "coder", []string{id}, 0)
	if err != nil {
		t.Fatalf("AddTaskWithDeps failed: %v", err)
	}

	// 新实例从磁盘加载: 写域持久化
	reloaded := NewTaskStore(path)
	all := reloaded.GetAllTasks()
	if len(all) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(all))
	}
	for _, s := range all {
		switch s.ID {
		case id:
			if len(s.WriteScopes) != 2 || s.WriteScopes[0] != "contract:store" || s.WriteScopes[1] != "state:mvcc" {
				t.Fatalf("expected trimmed/unique scopes [contract:store state:mvcc], got %+v", s.WriteScopes)
			}
		case id2:
			if len(s.WriteScopes) != 0 {
				t.Fatalf("AddTaskWithDeps must not set writeScopes, got %+v", s.WriteScopes)
			}
			if len(s.DependsOn) != 1 || s.DependsOn[0] != id {
				t.Fatalf("dependency not persisted: %+v", s.DependsOn)
			}
		default:
			t.Fatalf("unexpected task %s", s.ID)
		}
	}
}

func TestUniqueTrimmedStringsTask(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{"", "  "}, nil},
		{[]string{" a ", "a", "b"}, []string{"a", "b"}},
		{[]string{"x"}, []string{"x"}},
	}
	for _, c := range cases {
		got := uniqueTrimmedStringsTask(c.in)
		if (len(got) == 0) != (len(c.want) == 0) {
			t.Fatalf("uniqueTrimmedStringsTask(%q) = %q, want %q", c.in, got, c.want)
		}
		for i := range c.want {
			if i >= len(got) || got[i] != c.want[i] {
				t.Fatalf("uniqueTrimmedStringsTask(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	}
}
