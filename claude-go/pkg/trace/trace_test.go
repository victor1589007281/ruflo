package trace

import (
	"context"
	"strings"
	"testing"
)

func TestWithFromMergeSemantics(t *testing.T) {
	ctx := context.Background()

	// 逐层注入: RunID → NodeID → TurnID, 每层不得丢失上层字段
	ctx = With(ctx, IDs{RunID: "run-x"})
	ctx = With(ctx, IDs{NodeID: "stage-a"})
	ctx = With(ctx, IDs{TurnID: "t3"})

	got := From(ctx)
	if got.RunID != "run-x" || got.NodeID != "stage-a" || got.TurnID != "t3" {
		t.Fatalf("合并注入丢失字段: %+v", got)
	}

	// 覆盖语义: 非空字段覆盖, 空字段保留
	ctx2 := With(ctx, IDs{NodeID: "stage-b"})
	got2 := From(ctx2)
	if got2.NodeID != "stage-b" || got2.RunID != "run-x" || got2.TurnID != "t3" {
		t.Fatalf("覆盖语义错误: %+v", got2)
	}
	// 原 ctx 不受影响 (context 不可变性)
	if From(ctx).NodeID != "stage-a" {
		t.Fatal("With 修改了父 context")
	}
}

func TestFromZeroValue(t *testing.T) {
	if got := From(context.Background()); got != (IDs{}) {
		t.Fatalf("无标记 ctx 应返回零值, got %+v", got)
	}
	if got := From(nil); got != (IDs{}) { //nolint:staticcheck // 防御 nil ctx
		t.Fatalf("nil ctx 应返回零值, got %+v", got)
	}
}

func TestNewIDNonEmptyAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewID("c")
		if id == "" || !strings.HasPrefix(id, "c-") {
			t.Fatalf("id 格式错误: %q", id)
		}
		if seen[id] {
			t.Fatalf("100 次内出现重复 id: %q", id)
		}
		seen[id] = true
	}
}

func TestNewRunIDContainsName(t *testing.T) {
	id := NewRunID("myteam")
	if !strings.HasPrefix(id, "run-myteam-") {
		t.Fatalf("RunID 应含团队名前缀: %q", id)
	}
}
