package harness

// R-H17 接线的验收：TurnRequest + api.FallbackInfo → 账本条目 / 回退链。
//
// 断言全部用精确值而非"非零"：这两个函数只做形状转换，任何额外的"聪明"处理
// （补默认 Reason、猜 PriceUnknown、重新排序）都应该表现为一个失败的具体字段，
// 而不是一个看起来仍然合理的非零值。

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

func TestTurnCostEntry(t *testing.T) {
	req := TurnRequest{
		OwnerID:   "alice",
		SessionID: "s1",
		TurnID:    "t1",
		// req.Role 故意与入参 role 不同：职责划分是"账本键的 role 由调用方显式传入"，
		// req.Role 不参与组装，避免两处都填时谁生效变成隐式规则。
		Role: RoleExtract,
	}
	usage := types.Usage{InputTokens: 10, OutputTokens: 2, CacheReadInputTokens: 3}

	got := TurnCostEntry(req, RoleDialogue, "cheap", usage, true)
	want := CostEntry{
		CostKey:    CostKey{Role: RoleDialogue, TurnID: "t1", SessionID: "s1", OwnerID: "alice"},
		Model:      "cheap",
		Usage:      usage,
		UsageKnown: true,
	}
	if got != want {
		t.Fatalf("TurnCostEntry 组装错误\n got: %+v\nwant: %+v", got, want)
	}
	if got.PriceUnknown {
		t.Fatal("TurnCostEntry 不应替调用方猜 PriceUnknown: 价格可用性由账本判定")
	}

	// usageKnown=false 必须原样传下去（缺失 ≠ 0 token）。
	unknown := TurnCostEntry(req, "", "cheap", types.Usage{}, false)
	if unknown.UsageKnown {
		t.Fatal("usageKnown=false 被改写成了已知")
	}
	if unknown.Role != "" {
		t.Fatalf("空 role 应原样交给账本归一, 实际被提前改写为 %q", unknown.Role)
	}

	// 归一确实发生在 Record 侧：空 role 落账后是 dialogue。
	rec := NewCostLedger(nil).Record(unknown)
	if rec.Role != RoleDialogue {
		t.Fatalf("账本未把空 role 归一为 dialogue, 实际 %q", rec.Role)
	}
}

func TestRecordTurnFallbacks(t *testing.T) {
	key := CostKey{Role: RoleDialogue, TurnID: "t1", SessionID: "s1", OwnerID: "alice"}

	t.Run("NilLedger", func(t *testing.T) {
		got := RecordTurnFallbacks(nil, key, []api.FallbackInfo{{FromModel: "a", ToModel: "b"}})
		if got != nil {
			t.Fatalf("nil 账本应返回 nil, 实际 %+v", got)
		}
	})

	t.Run("EmptyInfos", func(t *testing.T) {
		l := NewCostLedger(nil)
		if got := RecordTurnFallbacks(l, key, nil); got != nil {
			t.Fatalf("空 infos 应返回 nil, 实际 %+v", got)
		}
		snap := l.Snapshot()
		if len(snap.Fallbacks) != 0 || len(snap.Entries) != 0 {
			t.Fatalf("空 infos 不应改变账本: fallbacks=%d entries=%d", len(snap.Fallbacks), len(snap.Entries))
		}
	})

	t.Run("TwoEvents", func(t *testing.T) {
		l := NewCostLedger(nil)
		infos := []api.FallbackInfo{
			{FromModel: "strong", ToModel: "cheap", Reason: "quota", StatusCode: 402},
			{FromModel: "cheap", ToModel: "tiny", StatusCode: 529},
		}
		got := RecordTurnFallbacks(l, key, infos)
		if len(got) != 2 {
			t.Fatalf("应返回 2 条事件, 实际 %d: %+v", len(got), got)
		}

		chain := l.Snapshot().FallbacksForTurn("t1")
		if len(chain) != 2 {
			t.Fatalf("按 turn 应查到 2 条回退, 实际 %d: %+v", len(chain), chain)
		}
		// 顺序必须与输入一致：A→B→C 的因果顺序是 trace 的语义。
		for i, want := range []FallbackEvent{
			{CostKey: key, FromModel: "strong", ToModel: "cheap", Reason: "quota"},
			{CostKey: key, FromModel: "cheap", ToModel: "tiny", Reason: "upstream_529"},
		} {
			if chain[i] != want {
				t.Errorf("chain[%d]\n got: %+v\nwant: %+v", i, chain[i], want)
			}
			if got[i] != want {
				t.Errorf("返回值[%d]\n got: %+v\nwant: %+v", i, got[i], want)
			}
		}
	})

	t.Run("ReasonRules", func(t *testing.T) {
		cases := []struct {
			name string
			info api.FallbackInfo
			want string
		}{
			{"Explicit", api.FallbackInfo{Reason: "quota_exceeded", StatusCode: 500}, "quota_exceeded"},
			{"StatusCode", api.FallbackInfo{StatusCode: 503}, "upstream_503"},
			{"Neither", api.FallbackInfo{}, "fallback"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				l := NewCostLedger(nil)
				RecordTurnFallbacks(l, key, []api.FallbackInfo{tc.info})
				chain := l.Snapshot().FallbacksForTurn("t1")
				if len(chain) != 1 {
					t.Fatalf("应记录 1 条回退, 实际 %d", len(chain))
				}
				if chain[0].Reason != tc.want {
					t.Fatalf("Reason = %q, 期望 %q", chain[0].Reason, tc.want)
				}
			})
		}
	})
}
