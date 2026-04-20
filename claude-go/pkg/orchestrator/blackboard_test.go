package orchestrator

import (
	"sync"
	"testing"
	"time"
)

func TestBlackboard_WriteRead(t *testing.T) {
	bb := NewBlackboard()
	defer bb.Close()

	ver, err := bb.Write("key1", "hello", WriteMeta{Author: "test", Category: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if ver != 1 {
		t.Errorf("expected version 1, got %d", ver)
	}

	val, meta, err := bb.Read("key1")
	if err != nil {
		t.Fatal(err)
	}
	if val != "hello" {
		t.Errorf("expected hello, got %v", val)
	}
	if meta.Author != "test" {
		t.Errorf("expected author test, got %s", meta.Author)
	}
	if meta.Version != 1 {
		t.Errorf("expected version 1, got %d", meta.Version)
	}
}

func TestBlackboard_VersionIncrement(t *testing.T) {
	bb := NewBlackboard()
	defer bb.Close()

	v1, _ := bb.Write("k", "v1", WriteMeta{Author: "a"})
	v2, _ := bb.Write("k", "v2", WriteMeta{Author: "a"})

	if v1 != 1 || v2 != 2 {
		t.Errorf("versions should be 1, 2; got %d, %d", v1, v2)
	}

	val, _, _ := bb.Read("k")
	if val != "v2" {
		t.Errorf("expected v2, got %v", val)
	}
}

func TestBlackboard_ReadNotFound(t *testing.T) {
	bb := NewBlackboard()
	defer bb.Close()

	_, _, err := bb.Read("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent key")
	}
}

func TestBlackboard_Query(t *testing.T) {
	bb := NewBlackboard()
	defer bb.Close()

	bb.Write("task/1/output", "r1", WriteMeta{Author: "runner", Category: "output"})
	bb.Write("task/2/output", "r2", WriteMeta{Author: "runner", Category: "output"})
	bb.Write("task/1/eval", "e1", WriteMeta{Author: "eval", Category: "eval"})

	results := bb.Query("task/", QueryFilter{Category: "output"})
	if len(results) != 2 {
		t.Errorf("expected 2 output entries, got %d", len(results))
	}

	results = bb.Query("task/1/", QueryFilter{})
	if len(results) != 2 {
		t.Errorf("expected 2 entries for task/1/, got %d", len(results))
	}
}

func TestBlackboard_Watch(t *testing.T) {
	bb := NewBlackboard()
	defer bb.Close()

	ch := bb.Watch("events/")

	go func() {
		time.Sleep(10 * time.Millisecond)
		bb.Write("events/1", "evt1", WriteMeta{Author: "test"})
	}()

	select {
	case evt := <-ch:
		if evt.NewValue != "evt1" {
			t.Errorf("expected evt1, got %v", evt.NewValue)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for watch event")
	}
}

func TestBlackboard_MaxValueSize(t *testing.T) {
	bb := NewBlackboard(WithMaxValueSize(10))
	defer bb.Close()

	_, err := bb.Write("small", "hi", WriteMeta{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = bb.Write("big", "this is a very long value exceeding 10 bytes", WriteMeta{})
	if err == nil {
		t.Error("expected error for oversized value")
	}
}

func TestBlackboard_Snapshot(t *testing.T) {
	bb := NewBlackboard()
	defer bb.Close()

	bb.Write("a", 1, WriteMeta{})
	bb.Write("b", 2, WriteMeta{})

	snap := bb.Snapshot()
	if len(snap) != 2 {
		t.Errorf("expected 2 entries, got %d", len(snap))
	}
}

func TestBlackboard_ConcurrentWrites(t *testing.T) {
	bb := NewBlackboard()
	defer bb.Close()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bb.Write("shared", i, WriteMeta{Author: "worker"})
		}(i)
	}
	wg.Wait()

	val, meta, err := bb.Read("shared")
	if err != nil {
		t.Fatal(err)
	}
	if val == nil {
		t.Error("expected non-nil value")
	}
	if meta.Version != 100 {
		t.Errorf("expected version 100, got %d", meta.Version)
	}
}
