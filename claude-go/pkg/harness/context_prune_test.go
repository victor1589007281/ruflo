package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/engine"
)

func assertIDsEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s 长度不符: got %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] 不符: got %v, want %v", what, i, got, want)
		}
	}
}

// 三个块 Priority 1/5/3、各 40 token，预算 100 ⇒ 裁掉 Priority 最小的那个，
// 其余两块保持原始相对顺序。
func TestDedupeAndPrune_BudgetDropsLowestPriority(t *testing.T) {
	in := []ContextBlock{
		{ID: "a", Text: "甲", Priority: 1, Tokens: 40},
		{ID: "b", Text: "乙", Priority: 5, Tokens: 40},
		{ID: "c", Text: "丙", Priority: 3, Tokens: 40},
	}
	got := dedupeAndPrune(in, 100)
	assertIDsEqual(t, "IDs", got.IDs, []string{"b", "c"})
	assertIDsEqual(t, "DroppedIDs", got.DroppedIDs, []string{"a"})
	if got.Tokens != 80 {
		t.Fatalf("裁剪后 token 应为 80, 实际 %d", got.Tokens)
	}
	if !got.TokensKnown {
		t.Fatal("有块带 Tokens，TokensKnown 应为 true")
	}
	if got.Degraded != "context_budget_pruned" {
		t.Fatalf("降级标签应为 context_budget_pruned, 实际 %q", got.Degraded)
	}
	if len(got.Blocks) != 2 || got.Blocks[0].Text != "乙" || got.Blocks[1].Text != "丙" {
		t.Fatalf("Blocks 应保持剩余块的原序: %+v", got.Blocks)
	}
}

// 同为 Priority 时裁掉靠后的：三块同权、预算只够两块。
func TestDedupeAndPrune_TieDropsLater(t *testing.T) {
	in := []ContextBlock{
		{ID: "a", Text: "甲", Priority: 3, Tokens: 10},
		{ID: "b", Text: "乙", Priority: 3, Tokens: 10},
		{ID: "c", Text: "丙", Priority: 3, Tokens: 10},
	}
	got := dedupeAndPrune(in, 20)
	assertIDsEqual(t, "IDs", got.IDs, []string{"a", "b"})
	assertIDsEqual(t, "DroppedIDs", got.DroppedIDs, []string{"c"})
	if got.Tokens != 20 {
		t.Fatalf("裁剪后 token 应为 20, 实际 %d", got.Tokens)
	}
}

// budget=0 表示不限：全保留，不产生 dropped。
func TestDedupeAndPrune_BudgetZeroNoPrune(t *testing.T) {
	in := []ContextBlock{
		{ID: "a", Text: "甲", Priority: 1, Tokens: 40},
		{ID: "b", Text: "乙", Priority: 5, Tokens: 40},
		{ID: "c", Text: "丙", Priority: 3, Tokens: 40},
	}
	got := dedupeAndPrune(in, 0)
	assertIDsEqual(t, "IDs", got.IDs, []string{"a", "b", "c"})
	if len(got.DroppedIDs) != 0 {
		t.Fatalf("budget=0 不该裁块, 实际 dropped %v", got.DroppedIDs)
	}
	if got.Tokens != 120 {
		t.Fatalf("总 token 应为 120, 实际 %d", got.Tokens)
	}
	if got.Degraded != "" {
		t.Fatalf("未去重未裁剪时不该有降级, 实际 %q", got.Degraded)
	}
}

// 块自带 Tokens 优先于文本估算：两个"短文本"自报 1000 token。
//
// 若走估算，两条短文本合计只有 2 token，budget=10 会全保留；按自报值则连一块
// 都放不下（1000 > 10），所以 budget=10 时必须全裁，且裁的顺序是先低优先级。
// 另给 budget=1000（够一块）验证"只留 Priority 大的"。
func TestDedupeAndPrune_NativeTokensBeatEstimate(t *testing.T) {
	in := []ContextBlock{
		{ID: "low", Text: "短", Priority: 1, Tokens: 1000},
		{ID: "high", Text: "短", Priority: 5, Tokens: 1000},
	}
	got := dedupeAndPrune(in, 10)
	if len(got.Blocks) != 0 || len(got.IDs) != 0 {
		t.Fatalf("自报 1000 token 时 budget=10 应全裁, 实际 IDs=%v", got.IDs)
	}
	assertIDsEqual(t, "DroppedIDs", got.DroppedIDs, []string{"low", "high"})
	if got.Tokens != 0 {
		t.Fatalf("全裁后 Tokens 应为 0, 实际 %d", got.Tokens)
	}
	if got.TokensKnown {
		t.Fatal("空选择时 TokensKnown 应为 false")
	}

	got = dedupeAndPrune(in, 1000)
	assertIDsEqual(t, "IDs(预算够一块)", got.IDs, []string{"high"})
	assertIDsEqual(t, "DroppedIDs(预算够一块)", got.DroppedIDs, []string{"low"})
	if got.Tokens != 1000 {
		t.Fatalf("保留块的 token 应为 1000, 实际 %d", got.Tokens)
	}
}

// 无 Tokens 时按 rune 数/4 向上取整估算；这是下界，不是真实 tokenizer。
func TestDedupeAndPrune_EstimatesTokensWhenMissing(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"a", 1},
		{strings.Repeat("a", 100), 25},
		{strings.Repeat("a", 101), 26},
		{strings.Repeat("中", 9), 3},
	}
	for _, c := range cases {
		if got := EstimateContextTokens(c.text); got != c.want {
			t.Fatalf("EstimateContextTokens(%d runes) = %d, want %d", len(c.text), got, c.want)
		}
	}

	got := dedupeAndPrune([]ContextBlock{{ID: "e", Text: strings.Repeat("a", 100)}}, 0)
	if got.Tokens != 25 {
		t.Fatalf("100 个 ASCII 字符应估成 25 token, 实际 %d", got.Tokens)
	}
}

// 重复 ID 保留第一个（内容也保留第一份），去重不算进 DroppedIDs。
func TestDedupeAndPrune_DuplicateKeepsFirst(t *testing.T) {
	in := []ContextBlock{
		{ID: "f1", Text: "第一版", Tokens: 10},
		{ID: "f1", Text: "第二版", Tokens: 10},
		{ID: "f2", Text: "别的", Tokens: 10},
	}
	got := dedupeAndPrune(in, 0)
	assertIDsEqual(t, "IDs", got.IDs, []string{"f1", "f2"})
	if len(got.DroppedIDs) != 0 {
		t.Fatalf("去重不是预算裁剪, DroppedIDs 应为空, 实际 %v", got.DroppedIDs)
	}
	if len(got.Blocks) != 2 || got.Blocks[0].Text != "第一版" {
		t.Fatalf("同 ID 应保留第一份内容, 实际 %+v", got.Blocks)
	}
	if got.Degraded != "context_duplicate_id_dropped" {
		t.Fatalf("去重应标 context_duplicate_id_dropped, 实际 %q", got.Degraded)
	}
}

// 预算小到一块都放不下：全裁，Blocks/IDs 空、DroppedIDs 全量、Tokens=0、
// TokensKnown=false。
func TestDedupeAndPrune_BudgetTooSmallDropsAll(t *testing.T) {
	in := []ContextBlock{
		{ID: "a", Text: "甲", Priority: 1, Tokens: 100},
		{ID: "b", Text: "乙", Priority: 2, Tokens: 100},
	}
	got := dedupeAndPrune(in, 10)
	if len(got.Blocks) != 0 || len(got.IDs) != 0 {
		t.Fatalf("应全部裁掉, 实际 Blocks=%v IDs=%v", got.Blocks, got.IDs)
	}
	assertIDsEqual(t, "DroppedIDs", got.DroppedIDs, []string{"a", "b"})
	if got.Tokens != 0 || got.TokensKnown {
		t.Fatalf("全裁后 Tokens=0/TokensKnown=false, 实际 %d/%v", got.Tokens, got.TokensKnown)
	}
	if got.Degraded != "context_budget_pruned" {
		t.Fatalf("应标 context_budget_pruned, 实际 %q", got.Degraded)
	}
}

// 预算裁剪与去重同时发生时，预算裁剪的标签优先。
func TestDedupeAndPrune_PruneTagWinsOverDuplicate(t *testing.T) {
	in := []ContextBlock{
		{ID: "a", Text: "甲", Priority: 1, Tokens: 40},
		{ID: "a", Text: "甲-重复", Priority: 1, Tokens: 40},
		{ID: "b", Text: "乙", Priority: 5, Tokens: 40},
	}
	got := dedupeAndPrune(in, 40)
	assertIDsEqual(t, "IDs", got.IDs, []string{"b"})
	assertIDsEqual(t, "DroppedIDs", got.DroppedIDs, []string{"a"})
	if got.Degraded != "context_budget_pruned" {
		t.Fatalf("两者都有时预算裁剪优先, 实际 %q", got.Degraded)
	}
}

// budgetProvider 供 adaptContextProviderWithBudget 的集成用例使用。
type budgetProvider struct{ patch ContextPatch }

func (p budgetProvider) ProvideTurnContext(_ context.Context, _ TurnContext) (ContextPatch, error) {
	return p.patch, nil
}

// 集成：provider 返回带 Trace 的 patch，预算裁剪后 sink 收到的 selection.IDs
// 与 trace 必须精确对应真正发出去的集合。
func TestAdaptContextBudget_SinkGetsSelectionAndTrace(t *testing.T) {
	p := budgetProvider{patch: ContextPatch{
		Blocks: []ContextBlock{
			{ID: "a", Text: "甲", Priority: 1, Tokens: 40},
			{ID: "b", Text: "乙", Priority: 5, Tokens: 40},
			{ID: "c", Text: "丙", Priority: 3, Tokens: 40},
		},
		Trace: "retrieval: 3 hits",
	}}

	var gotSel ContextSelection
	var gotTrace string
	calls := 0
	prov := adaptContextProviderWithBudget(p, TurnRequest{SessionID: "s1", TurnID: "t1"}, 100,
		func(sel ContextSelection, trace string) {
			calls++
			gotSel = sel
			gotTrace = trace
		})
	if prov == nil {
		t.Fatal("provider 非 nil 时不该返回 nil")
	}
	blocks, degraded, err := prov(context.Background(), engine.EphemeralView{})
	if err != nil {
		t.Fatalf("适配器不该向引擎抛错: %v", err)
	}
	if calls != 1 {
		t.Fatalf("sink 应恰好调用一次, 实际 %d", calls)
	}
	assertIDsEqual(t, "sink IDs", gotSel.IDs, []string{"b", "c"})
	assertIDsEqual(t, "sink DroppedIDs", gotSel.DroppedIDs, []string{"a"})
	if gotTrace != "retrieval: 3 hits" {
		t.Fatalf("sink 应拿到 provider 的 trace, 实际 %q", gotTrace)
	}
	if gotSel.Tokens != 80 || !gotSel.TokensKnown {
		t.Fatalf("sink 的 token 统计不对: %d/%v", gotSel.Tokens, gotSel.TokensKnown)
	}
	if degraded != "context_budget_pruned" {
		t.Fatalf("返回的降级标签应为 context_budget_pruned, 实际 %q", degraded)
	}
	if len(blocks) != 2 || blocks[0].Text != "乙" || blocks[1].Text != "丙" {
		t.Fatalf("返回给引擎的块应与 selection 一致: %+v", blocks)
	}
}

// 集成：provider 返回 error 时仍降级、不抛错，且不调用 sink（无 patch 即无 trace）。
func TestAdaptContextBudget_ErrorDegrades(t *testing.T) {
	prov := adaptContextProviderWithBudget(errorContextProvider{}, TurnRequest{SessionID: "s", TurnID: "t"}, 100,
		func(ContextSelection, string) {
			t.Fatal("provider 出错时不该调用 sink")
		})
	blocks, degraded, err := prov(context.Background(), engine.EphemeralView{})
	if err != nil {
		t.Fatalf("provider 出错不该抛给引擎: %v", err)
	}
	if len(blocks) != 0 || degraded != "context_degraded" {
		t.Fatalf("应返回空块 + context_degraded, 实际 %d/%q", len(blocks), degraded)
	}
}

// 集成：provider 自报的 Degraded 优先于结构性降级（它知道 harness 看不到的原因）。
func TestAdaptContextBudget_ProviderDegradedWins(t *testing.T) {
	p := budgetProvider{patch: ContextPatch{
		Blocks:   []ContextBlock{{ID: "a", Text: "甲", Priority: 1, Tokens: 40}},
		Degraded: "retrieval_timeout",
	}}
	prov := adaptContextProviderWithBudget(p, TurnRequest{SessionID: "s", TurnID: "t"}, 10, nil)
	_, degraded, err := prov(context.Background(), engine.EphemeralView{})
	if err != nil {
		t.Fatalf("不该抛错: %v", err)
	}
	if degraded != "retrieval_timeout" {
		t.Fatalf("provider 自报降级应优先, 实际 %q", degraded)
	}
}

type errorContextProvider struct{}

func (errorContextProvider) ProvideTurnContext(_ context.Context, _ TurnContext) (ContextPatch, error) {
	return ContextPatch{}, context.DeadlineExceeded
}
