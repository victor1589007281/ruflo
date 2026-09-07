// l1_listing_rank_test.go —— 13.7-P2 L1 配给: SetRanker 注入 + 清单描述位配给单测。
//
// 锁定验收 (docforge planning-skills-surge §13.7.5「L1 配给」):
//  1. nil ranker = 纯名称序 (行为零变化, 与 ModelVisibleActive 的 All() 排序一致);
//  2. 有 ranker 时按评分降序 (同分名称确定性), 两处渲染点 (FormatShortListing /
//     FormatShortListingForDir) 同一语义;
//  3. 描述位预算 (shortListingDescBudget) 按顺序分配 —— 低评分资产在预算耗尽后
//     只留名称 (「高加载频次资产常驻描述位, 低频资产只留名称」);
//  4. ranker 与 rank 分层正交: 只改渲染顺序, 不碰注册覆盖语义。
package skills

import (
	"strings"
	"testing"
)

// fixedRanker 定值评分器 (评分即资产名在 ranks 里的下标倒序 —— 下标小 = 分高)。
func fixedRanker(ranks map[string]float64) func(string) float64 {
	return func(name string) float64 { return ranks[name] }
}

// TestSetRankerOrdering 核心验收: 高后验技能的描述位排在清单前部, 无评分资产
// (ranker 返回 0/负) 落到名称序队尾; 同分按名称确定性。
func TestSetRankerOrdering(t *testing.T) {
	r := NewRegistry()
	r.Register(&Skill{Name: "z-hot", Body: "b", Description: "high posterior"})
	r.Register(&Skill{Name: "m-warm", Body: "b", Description: "mid posterior"})
	r.Register(&Skill{Name: "a-cold", Body: "b", Description: "zero posterior"})
	r.Register(&Skill{Name: "k-tie1", Body: "b", Description: "tie one"})
	r.Register(&Skill{Name: "d-tie2", Body: "b", Description: "tie two"})

	// 名称序基线 (nil ranker): 行为零变化。
	base := r.FormatShortListing(0)
	if !strings.HasPrefix(strings.TrimLeft(base, "<available_skills summary=\"short\">\n"), "- a-cold:") {
		t.Errorf("nil ranker 应保持名称序 (a-cold 首位):\n%s", base)
	}
	// 逐行断言基线顺序。
	assertLinesOrder(t, base, []string{"a-cold", "d-tie2", "k-tie1", "m-warm", "z-hot"})

	// 注入评分: z-hot=0.9, m-warm=0.8, 其余 0 (tie → 名称序, d-tie2 < k-tie1)。
	r.SetRanker(fixedRanker(map[string]float64{"z-hot": 0.9, "m-warm": 0.8}))
	assertLinesOrder(t, r.FormatShortListing(0), []string{"z-hot", "m-warm", "a-cold", "d-tie2", "k-tie1"})

	// 清 nil: 回名称序 (feishu 未开池分支的语义)。
	r.SetRanker(nil)
	assertLinesOrder(t, r.FormatShortListing(0), []string{"a-cold", "d-tie2", "k-tie1", "m-warm", "z-hot"})
}

// TestL1DescBudgetAllocation 描述位配给: 小预算下 (limit 软上限) 高评分资产
// 先占描述位, 低频资产只留名称。limit=2 时只有前 2 项 (评分最高) 带描述。
func TestL1DescBudgetAllocation(t *testing.T) {
	r := NewRegistry()
	r.Register(&Skill{Name: "low", Body: "b", Description: "low desc"})
	r.Register(&Skill{Name: "high", Body: "b", Description: "high desc"})
	r.SetRanker(fixedRanker(map[string]float64{"high": 0.9, "low": 0.1}))

	out := r.FormatShortListing(2) // 前 2 项都可带描述 → 都在
	for _, name := range []string{"high", "low"} {
		line := "- " + name + ": "
		if !strings.Contains(out, line) {
			t.Errorf("limit=2 未超预算, %s 应保留描述:\n%s", name, out)
		}
	}

	// limit=1: 只有评分最高的 high 带描述, low 只留名称。
	out = r.FormatShortListing(1)
	if !strings.Contains(out, "- high: high desc") {
		t.Errorf("评分最高的资产应先占描述位:\n%s", out)
	}
	if !strings.Contains(out, "- low\n") || strings.Contains(out, "- low: low desc") {
		t.Errorf("limit 外资产只留名称:\n%s", out)
	}

	// ForDir 同一语义 (带 paths 声明时才走 dir 分支; dir="" 直通主渲染)。
	dirOut := r.FormatShortListingForDir(1, "")
	assertLinesOrder(t, dirOut, []string{"high", "low"})
}

// TestRankerOrthogonalToRankOverride ranker 只改渲染顺序, 不碰 rank 覆盖语义:
// project(100) 覆盖同名 builtin 后, ranker 改序不改变 Get 语义, 也不重复注册。
func TestRankerOrthogonalToRankOverride(t *testing.T) {
	r := NewRegistry()
	r.Register(&Skill{Name: "code-review", Body: "builtin", LoadedFrom: "builtin"})
	r.Register(&Skill{Name: "code-review", Body: "project", LoadedFrom: "project"})
	r.Register(&Skill{Name: "solo", Body: "b"})
	r.SetRanker(fixedRanker(map[string]float64{"solo": 1.0, "code-review": 0.5}))

	out := r.FormatShortListing(0)
	assertLinesOrder(t, out, []string{"solo", "code-review"}) // 渲染序按 ranker
	if r.Count() != 2 {
		t.Fatalf("ranker 不得影响注册覆盖: 得 %d", r.Count())
	}
	got, ok := r.Get("code-review")
	if !ok || got.Body != "project" || got.Rank != 100 {
		t.Errorf("ranker 正交: Get 语义应不变, got %+v", got)
	}
}

// TestSortForListingNegativeScores 负分视同无评分, 与 0 分资产同队按名称序;
// 全负分 = 整体保持名称序 (SetRanker 注释的退化语义)。
func TestSortForListingNegativeScores(t *testing.T) {
	r := NewRegistry()
	r.Register(&Skill{Name: "b-neg", Body: "b"})
	r.Register(&Skill{Name: "a-neg", Body: "b"})
	r.SetRanker(func(string) float64 { return -1 })
	assertLinesOrder(t, r.FormatShortListing(0), []string{"a-neg", "b-neg"})
}

// assertLinesOrder 断言 <available_skills> 清单里的名称出现顺序。
func assertLinesOrder(t *testing.T, listing string, want []string) {
	t.Helper()
	// 找到每行 "- <name>" 的出现位置, 按位置递增校验。
	prev := -1
	for _, name := range want {
		idx := strings.Index(listing, "- "+name+":") // 带描述
		if idx < 0 {
			idx = strings.Index(listing, "- "+name+"\n") // 只留名称
		}
		if idx < 0 {
			t.Fatalf("清单缺 %s:\n%s", name, listing)
		}
		if idx < prev {
			t.Fatalf("顺序错: %s 出现在前一名单之后:\n%s", name, listing)
		}
		prev = idx
	}
}
