package learners

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func scoreVec(id, target string, scores map[string]float64) ScoreVector {
	sum := 0.0
	for _, v := range scores {
		sum += v
	}
	return ScoreVector{ProposalID: id, Target: target, TaskScores: scores, Mean: sum / float64(len(scores)), Ran: len(scores)}
}

func TestDominates(t *testing.T) {
	a := map[string]float64{"t1": 1, "t2": 0.5, "t3": 1}
	b := map[string]float64{"t1": 1, "t2": 0.4, "t3": 0.9}
	if !Dominates(a, b) {
		t.Error("a 应支配 b (t2/t3 严格更好, 其余持平)")
	}
	if Dominates(b, a) {
		t.Error("b 不应支配 a")
	}
	// 有一项更差就不支配
	c := map[string]float64{"t1": 0, "t2": 1, "t3": 1}
	if Dominates(a, c) || Dominates(c, a) {
		t.Error("互有胜负应互不支配")
	}
	// 缺考任务不构成劣势: d 只考了 t1 且满分, a 在共同任务 t1 上与 d 持平 → 互不支配
	d := map[string]float64{"t1": 1}
	if Dominates(a, d) || Dominates(d, a) {
		t.Error("缺考不应被当成零分")
	}
}

func TestFrontier(t *testing.T) {
	allRound := scoreVec("all", "x", map[string]float64{"t1": 0.8, "t2": 0.8})
	specialist := scoreVec("spec", "x", map[string]float64{"t1": 1.0, "t2": 0.2})
	loser := scoreVec("loser", "x", map[string]float64{"t1": 0.5, "t2": 0.5})
	front := Frontier([]ScoreVector{allRound, specialist, loser})
	ids := map[string]bool{}
	for _, v := range front {
		ids[v.ProposalID] = true
	}
	if !ids["all"] || !ids["spec"] {
		t.Error("全能与偏科都应在前沿 (GEPA 多样性核心), 得", ids)
	}
	if ids["loser"] {
		t.Error("被支配者不应入前沿")
	}
}

func TestSelectForMutationRoundRobin(t *testing.T) {
	// 同任务集全覆盖, 互有胜负 → 同属前沿, 轮值必须轮换
	pool := []ScoreVector{
		scoreVec("p1", "x", map[string]float64{"t1": 1, "t2": 0.5}),
		scoreVec("p2", "x", map[string]float64{"t1": 0.9, "t2": 1}),
		scoreVec("other", "y", map[string]float64{"t1": 1}),
	}
	first := SelectForMutation(pool, "x", 0)
	second := SelectForMutation(pool, "x", 1)
	if first == "" || second == "" {
		t.Fatal("池非空应选出父代")
	}
	if first == second {
		t.Error("轮值应在不同前沿候选间轮换")
	}
	if SelectForMutation(pool, "nope", 0) != "" {
		t.Error("无该 target 的候选应返回空")
	}
}

func TestPoolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := RecordScore(dir, scoreVec("p1", "x", map[string]float64{"t1": 1})); err != nil {
		t.Fatal(err)
	}
	if err := RecordScore(dir, scoreVec("p2", "x", map[string]float64{"t1": 0.5})); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadPool(dir)
	if err != nil || len(pool) != 2 {
		t.Fatalf("池读写回环失败: %v n=%d", err, len(pool))
	}
	if err := RecordScore(dir, ScoreVector{}); err == nil {
		t.Error("缺字段应拒绝入池")
	}
}

// mockReflector 返回一个去掉占位符 {objective} 的候选 (测守恒检查),
// 或按要求返回带占位符的合法候选。
type mockReflector struct {
	body string
	err  error
	got  string // 记录收到的 user 提示, 供断言参照段存在
}

func (m *mockReflector) SimpleComplete(_ context.Context, _, user string) (string, error) {
	m.got = user
	return m.body, m.err
}

func TestEvolvePromptV2_ReflectorAndGuards(t *testing.T) {
	dir := t.TempDir()
	base := PromptEvolveInput{
		Target: "dev/implement", Current: "完成 {objective} 并自测",
		Failures: []string{"忘了跑测试", "改错文件"}, MeanScore: -0.6, Samples: 5,
	}

	// ① 带 T3 参照的正常路径: 参照段必须真的进了反思提示词
	mr := &mockReflector{body: "先读再改 {objective}, 每步跑测试, 收尾给摘要"}
	p, err := EvolvePromptV2(context.Background(), dir, mr, PromptEvolveInputV2{
		PromptEvolveInput: base,
		SuccessRefs:       []string{"成功做法: 先 Read 再 StrReplace, 改完立即 go test"},
	})
	if err != nil || p == nil {
		t.Fatalf("正常路径应产草案: %v", err)
	}
	if !strings.Contains(mr.got, "成功参照") || !strings.Contains(mr.got, "先 Read 再 StrReplace") {
		t.Error("T3 成功参照未进入反思提示词")
	}
	if p.CreatedBy != "learners.EvolvePromptV2" {
		t.Errorf("CreatedBy 应标记 V2, 得 %s", p.CreatedBy)
	}
	if p.Status != "proposed" {
		t.Errorf("草案必须是 proposed 态, 得 %s", p.Status)
	}

	// ② 占位符丢失 → 确定性拒绝
	mr2 := &mockReflector{body: "一段没有占位符的候选正文"}
	if _, err := EvolvePromptV2(context.Background(), dir, mr2, PromptEvolveInputV2{PromptEvolveInput: base}); err == nil ||
		!strings.Contains(err.Error(), "占位符") {
		t.Errorf("丢占位符应拒, 得 %v", err)
	}

	// ③ 无负证据 → 拒绝
	bad := base
	bad.MeanScore = 0.5
	if _, err := EvolvePromptV2(context.Background(), dir, mr, PromptEvolveInputV2{PromptEvolveInput: bad}); err == nil {
		t.Error("均值非负应拒绝反思")
	}

	// ④ nil reflector → 拒绝 (H3)
	if _, err := EvolvePromptV2(context.Background(), dir, nil, PromptEvolveInputV2{PromptEvolveInput: base}); err == nil {
		t.Error("nil reflector 应拒绝")
	}

	// ⑤ 回显原文 → 拒绝
	mr3 := &mockReflector{body: base.Current}
	if _, err := EvolvePromptV2(context.Background(), dir, mr3, PromptEvolveInputV2{PromptEvolveInput: base}); err == nil {
		t.Error("回显原文应拒绝")
	}

	// ⑥ 超长候选 → 拒绝
	mr4 := &mockReflector{body: strings.Repeat("x", gepaMaxPromptChars+1) + "{objective}"}
	if _, err := EvolvePromptV2(context.Background(), dir, mr4, PromptEvolveInputV2{PromptEvolveInput: base}); err == nil {
		t.Error("超长候选应拒绝")
	}
	fmt.Println("V2 guards all fired")
}
