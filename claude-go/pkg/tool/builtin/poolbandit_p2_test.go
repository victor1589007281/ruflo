// poolbandit_p2_test.go —— 13.7-P2 常驻面聚合 + L1 配给评分单测。
//
// 锁定验收 (docforge planning-skills-surge §13.7.5/§13.7.7):
//  1. residentCandidates 阈值 (Mean≥0.7 且 Trials≥5 双门槛, 任一不满足即拒);
//  2. 跨键聚合: 同资产任一 (team,role) 身份达标即候选, 取最高均值、Trials 累加;
//  3. 排序 (均值×样本量降序, 同分名称确定性) + ResidentLimit 截断;
//  4. SetResidentFromBandit 只收池成员 (pool 过滤联动);
//  5. RankScore / assetMean (L1 配给评分: 跨键最高均值, 无记录=0)。
package builtin

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/trace"
)

// seedResident 直接写后验计数, 绕过采样随机性 (阈值测试要确定性)。
func seedResident(b *poolBandit, team, role, asset string, succ, fail int) {
	for i := 0; i < succ; i++ {
		b.update(team, role, asset, true)
	}
	for i := 0; i < fail; i++ {
		b.update(team, role, asset, false)
	}
}

// TestResidentCandidatesThresholds 双门槛验收: 均值 <0.7 拒 / 样本 <5 拒 /
// 恰好 4/5 (均值 0.8) 通过。
func TestResidentCandidatesThresholds(t *testing.T) {
	b := newPoolBandit("") // 纯内存
	// 纯成功 4 次: 均值 1.0 但样本 4 < 5 → 拒 (小样本偶然防护)。
	seedResident(b, "t", "r", "few", 4, 0)
	// 5 次 4 成 1 败: 均值 0.8 ≥ 0.7 且样本 5 → 收。
	seedResident(b, "t", "r", "ok", 4, 1)
	// 6 次 4 成 2 败: 均值 0.667 < 0.7 → 拒。
	seedResident(b, "t", "r", "lo", 4, 2)
	// 5 次 3 成 2 败: 均值 0.6 → 拒 (且样本恰 5, 隔离均值维度)。
	seedResident(b, "t", "r", "mid", 3, 2)

	got := map[string]bool{}
	for _, c := range b.residentCandidates() {
		got[c.Name] = true
	}
	if got["few"] {
		t.Error("样本 4 < residentMinTrials 应拒 (1/1 类偶然防护)")
	}
	if !got["ok"] {
		t.Error("均值 0.8/样本 5 双达标应收")
	}
	if got["lo"] {
		t.Error("均值 0.667 < residentPosteriorFloor 应拒")
	}
	if got["mid"] {
		t.Error("均值 0.6 应拒 (样本恰好达标, 隔离均值判定)")
	}
}

// TestResidentCandidatesCrossKeyAggregation 跨键聚合: 同一资产在 team-A 全成功、
// team-B 失败链 —— 任一身份达标即候选; Mean 取最高 (team-A 的 1.0), Trials 跨键累加。
func TestResidentCandidatesCrossKeyAggregation(t *testing.T) {
	b := newPoolBandit("")
	// team-A: 5 全成功 (均值 1.0); team-B: 2 全失败 (均值 0)。
	seedResident(b, "team-A", "worker", "cross", 5, 0)
	seedResident(b, "team-B", "worker", "cross", 0, 2)
	// 对照: 只有 team-B 失败链的资产, 无达标身份 → 拒。
	seedResident(b, "team-B", "worker", "onlybad", 0, 7)

	cands := b.residentCandidates()
	var cross *residentAgg
	for i := range cands {
		if cands[i].Name == "cross" {
			cross = &cands[i]
		}
	}
	if cross == nil {
		t.Fatal("team-A 身份达标 (5 全成功) 应使 cross 入候选")
	}
	if cross.Mean != 1.0 {
		t.Errorf("Mean 应取跨键最高 (1.0), got %v", cross.Mean)
	}
	if cross.Trials != 7 { // (5+0) + (0+2)
		t.Errorf("Trials 应跨键累加 (7), got %v", cross.Trials)
	}
	for _, c := range cands {
		if c.Name == "onlybad" {
			t.Error("仅失败链资产 (均值 0) 不应入候选")
		}
	}
}

// TestResidentCandidatesOrderAndLimit 排序 (均值×样本量降序) 与 ResidentLimit 截断。
func TestResidentCandidatesOrderAndLimit(t *testing.T) {
	b := newPoolBandit("")
	seedResident(b, "t", "r", "a1", 10, 0) // 1.0×10 = 10
	seedResident(b, "t", "r", "a2", 10, 1) // 0.909×11 ≈ 10
	seedResident(b, "t", "r", "a3", 30, 0) // 1.0×30 = 30
	seedResident(b, "t", "r", "a4", 6, 1)  // 0.857×7 ≈ 6
	// ResidentLimit 截断用: 15 个全 5/0 (1.0×5=5, 同分按名称序)。
	for i := 0; i < 15; i++ {
		seedResident(b, "t", "r", string(rune('e'+i)), 5, 0) // e..s
	}
	cands := b.residentCandidates()
	if len(cands) != ResidentLimit {
		t.Fatalf("应截断到 ResidentLimit=%d, got %d", ResidentLimit, len(cands))
	}
	// a3 (30) 第一; 同分资产按名称序紧跟其后。
	if cands[0].Name != "a3" {
		t.Errorf("最高分 a3 应排首位, got %s", cands[0].Name)
	}
	for i := 1; i < len(cands); i++ {
		ki, kj := cands[i-1].Mean*cands[i-1].Trials, cands[i].Mean*cands[i].Trials
		if ki < kj {
			t.Fatalf("应按 均值×样本 降序, pos %d: %v < %v", i, ki, kj)
		}
		if ki == kj && cands[i-1].Name >= cands[i].Name {
			t.Fatalf("同分应按名称序, %s >= %s", cands[i-1].Name, cands[i].Name)
		}
	}
}

// TestSetResidentFromBanditPoolFilter 聚合候选进入池面时, 非池成员被 pool 侧
// 滤掉 (SetResident 只收池成员); nil policy/bandit 零操作。
func TestSetResidentFromBanditPoolFilter(t *testing.T) {
	pp, _ := newTestPoolPolicy(t)
	b := mustBandit(pp)
	b.rng = rand.New(rand.NewSource(7))
	seedResident(b, "t", "r", "in-pool", 5, 0)
	seedResident(b, "t", "r", "not-member", 9, 0) // 均值样本都更高, 但不在池内

	pool := tool.NewPool()
	pool.AddMember("in-pool")
	names := pp.SetResidentFromBandit(pool)
	if len(names) == 0 {
		t.Fatal("达标候选存在时应返回非空名单")
	}
	// 名单本身含 not-member (聚合语义只管后验表), 但池面不含。
	if !pool.IsResident("in-pool") || pool.IsResident("not-member") {
		t.Fatalf("池面应只收池成员: resident=%v", pool.ResidentNames())
	}

	// nil bandit / nil pool 零操作。
	pp2 := NewPoolPolicy("", nil) // bandit 未初始化
	if got := pp2.SetResidentFromBandit(tool.NewPool()); got != nil {
		t.Errorf("无 bandit 应返回 nil, got %v", got)
	}
	if got := pp.SetResidentFromBandit(nil); got != nil {
		t.Errorf("nil pool 应返回 nil, got %v", got)
	}
}

// TestRankScoreAssetMean L1 配给评分: 跨键最高均值; 无记录=0; 全失败=0;
// nil policy/bandit=0。
func TestRankScoreAssetMean(t *testing.T) {
	pp, _ := newTestPoolPolicy(t)
	b := mustBandit(pp)
	seedResident(b, "t1", "r", "hot", 4, 1)    // 0.8
	seedResident(b, "t2", "r", "hot", 1, 0)    // 1.0 (跨键最高)
	seedResident(b, "t1", "r", "allbad", 0, 5) // 0.0
	seedResident(b, "t1", "r", "mixed", 5, 5)  // 0.5

	cases := map[string]float64{
		"hot":    1.0, // 跨键取最高 (team-A 身份)
		"mixed":  0.5,
		"allbad": 0.0, // 全失败资产均值 0, 自然垫底
		"ghost":  0.0, // 后验表无记录 = 0 (排队尾)
	}
	for asset, want := range cases {
		if got := pp.RankScore(asset); got != want {
			t.Errorf("RankScore(%q) = %v, want %v", asset, got, want)
		}
	}
	if got := (&PoolPolicy{}).RankScore("hot"); got != 0 {
		t.Errorf("零值 policy (nil bandit) 应=0, got %v", got)
	}
	if got := (*PoolPolicy)(nil).RankScore("hot"); got != 0 {
		t.Errorf("nil policy 应=0, got %v", got)
	}
}

// TestPoolSearchMissFallbackSpan 未命中路径: span 记 fallback=true + candidates=0;
// 回退开关开着时提示文案带「未命中回退」; 关掉 (显式 falsy env) 时不带。
func TestPoolSearchMissFallbackSpan(t *testing.T) {
	pp, ts := newTestPoolPolicy(t)
	pool, reg, _ := buildTwoAssetPool(t)
	st := NewPoolSearchTool(pool, reg, nil, pp)
	st.getenv = func(string) string { return "" } // 未设置 = 默认开
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-miss"})

	res, err := st.Call(ctx, json.RawMessage(`{"query":"nonexistent-xyz"}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("未命中不应报错: %v / %v", err, res)
	}
	if !strings.Contains(res.Content, "未命中回退") {
		t.Errorf("默认开时提示应带回退语义: %s", res.Content)
	}
	spans, err := ts.ReadRun("run-miss")
	if err != nil {
		t.Fatalf("ReadRun: %v", err)
	}
	found := false
	for _, sp := range spans {
		if sp.Kind == tracestore.KindPolicyDecision && sp.Name == PoolSearchName {
			found = true
			if n, ok := sp.Attrs["candidates"].(float64); !ok || n != 0 {
				t.Errorf("candidates 应=0, got %v", sp.Attrs["candidates"])
			}
			if fb, ok := sp.Attrs["fallback"].(bool); !ok || !fb {
				t.Errorf("fallback 应=true, got %v", sp.Attrs["fallback"])
			}
		}
	}
	if !found {
		t.Fatalf("未命中路径应留 policy_decision Span: %+v", spans)
	}

	// 显式关闭回退: 提示不带回退语义。
	pp2, _ := newTestPoolPolicy(t)
	pool2, reg2, _ := buildTwoAssetPool(t)
	st2 := NewPoolSearchTool(pool2, reg2, nil, pp2)
	st2.getenv = func(k string) string {
		if k == tool.EnvPoolFallback {
			return "0"
		}
		return ""
	}
	res2, _ := st2.Call(ctx, json.RawMessage(`{"query":"nonexistent-xyz"}`), nil)
	if strings.Contains(res2.Content, "未命中回退") {
		t.Errorf("显式 falsy 关闭时不应带回退提示: %s", res2.Content)
	}
}

// TestPoolSearchHitReaggregatesResident 命中路径触发运行期再聚合 (13.7-P2):
// 加载积累后验至达标 → pool_search 命中一次 → 常驻面被刷新进池。
func TestPoolSearchHitReaggregatesResident(t *testing.T) {
	pp, _ := newTestPoolPolicy(t)
	b := mustBandit(pp)
	pool, reg, _ := buildTwoAssetPool(t)
	seedResident(b, "t", "r", "asset-b", 5, 0) // 5 全成功, 达常驻双门槛

	st := NewPoolSearchTool(pool, reg, nil, pp)
	if pool.IsResident("asset-b") {
		t.Fatal("search 前 asset-b 不应在常驻面 (未聚合)")
	}
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-rea"})
	if _, err := st.Call(ctx, json.RawMessage(`{"query":"deploy"}`), nil); err != nil {
		t.Fatalf("pool_search: %v", err)
	}
	if !pool.IsResident("asset-b") {
		t.Error("命中路径应触发再聚合, 达标资产进常驻面")
	}

	// 后验回落 → 下次命中再聚合自然收回 (整面替换)。
	b.update("t", "r", "asset-b", false)
	b.update("t", "r", "asset-b", false)
	b.update("t", "r", "asset-b", false) // 5 成 3 败 → 均值 0.625 < 0.7
	if _, err := st.Call(ctx, json.RawMessage(`{"query":"deploy"}`), nil); err != nil {
		t.Fatalf("pool_search(2): %v", err)
	}
	if pool.IsResident("asset-b") {
		t.Error("均值跌穿门槛后, 再聚合应把资产移出常驻面")
	}
}
