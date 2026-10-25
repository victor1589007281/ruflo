package agent

import "testing"

// TestSearchResearcherRoleIsBounded 钉住 search-researcher 的两条调优声明:
// 回合预算不能退回 12 (实测会在成文前撞上限), 工具面必须收窄到调研实际需要的
// 那一小组 (此前是 461 个, 每轮重发全部 schema)。
func TestSearchResearcherRoleIsBounded(t *testing.T) {
	rr := NewRoleRegistry(t.TempDir())
	rd := rr.Get("search-researcher")
	if rd == nil {
		t.Fatal("search-researcher 不在内置角色表里")
	}
	// 20 轮实测在 CBAM (多份 PDF 逐段提取) 上仍会撞上限, 28 轮才有成文余量。
	if rd.MaxTurns < 26 {
		t.Fatalf("MaxTurns = %d, want >= 26 (为多 PDF 检索/落账/成文留余量)", rd.MaxTurns)
	}
	if rd.FinalizeTurns < 2 {
		t.Fatalf("FinalizeTurns = %d, want >= 2 (最后两轮必须由引擎强制收口)", rd.FinalizeTurns)
	}
	if !rd.MergeBashTargets {
		t.Fatal("MergeBashTargets = false: 同一 PDF/URL 的重复 Bash 不会被拦")
	}
	if !rd.RestrictWritesToCwd {
		t.Fatal("RestrictWritesToCwd = false: Write 可以把 evidence.md 写到工作目录之外")
	}
	if !rd.CitationGate {
		t.Fatal("CitationGate = false: 过半引用未抓取时不会要求修订")
	}
	if !rd.NormalizeEvidence {
		t.Fatal("NormalizeEvidence = false: evidence.md 的「同上」不会被展开")
	}
	if rd.Tools == nil {
		t.Fatal("Tools 为 nil: 白名单没声明, 会退回 461 个工具的全量面")
	}
	want := map[string]bool{
		"WebSearch": true, "WebFetch": true,
		"Read": true, "Write": true, "Bash": true, "TodoWrite": true,
		"market_snapshot": true, "market_quote": true, "market_history": true,
		"market_fundamentals": true, "market_filings": true,
		"market_news": true, "market_macro": true,
	}
	got := make(map[string]bool, len(rd.Tools))
	for _, name := range rd.Tools {
		got[name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("缺少必需工具 %q; 实际 %v", name, rd.Tools)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("白名单混进了非调研工具 %q; 实际 %v", name, rd.Tools)
		}
	}
}
