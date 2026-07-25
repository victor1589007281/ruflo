package replay

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedCandidate 按任务序号返回预设产出 (用来精确摆出失败/空产出/正常三种样本)。
type scriptedCandidate struct {
	name string
	// out 按调用顺序无关, 而是按 objective 取 —— 采样会把同一 objective 跑多次。
	out map[string]string
	err error
}

func (c scriptedCandidate) Name() string { return c.name }
func (c scriptedCandidate) Produce(_ context.Context, t Task) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	return c.out[t.Objective], nil
}

func twoTasks() []Task {
	return []Task{
		{ID: "t1", Objective: "A", Expect: []string{"OK"}},
		{ID: "t2", Objective: "B", Expect: []string{"OK"}},
	}
}

// 只有一个档位可跑时必须拒绝: 单档冒烟不是 H6, 是单档回放换了名字。
func TestSmoke_单档运行必被拒(t *testing.T) {
	tiers := []Tier{{Name: "primary", Candidate: scriptedCandidate{
		name: "p", out: map[string]string{"A": "OK-a", "B": "OK-b"}}}}
	rep, err := Smoke(context.Background(), "prod", twoTasks(), tiers, nil, SmokeConfig{Samples: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed {
		t.Fatal("只跑了 1 个档位却放行了 —— MinTiers 闸没生效")
	}
	if rep.TiersRun != 1 {
		t.Errorf("TiersRun 应为 1, got %d", rep.TiersRun)
	}
	if !strings.Contains(strings.Join(rep.Blocking, " "), "单档冒烟不是 H6") {
		t.Errorf("拒绝理由要点明多档要求, got %v", rep.Blocking)
	}
}

// 未配置 candidate 的档位不得被当作"这档通过了"。
func TestSmoke_未配置档位不计通过(t *testing.T) {
	tiers := []Tier{
		{Name: "primary", Candidate: scriptedCandidate{name: "p", out: map[string]string{"A": "OK", "B": "OK"}}},
		{Name: "fallback"}, // 无 candidate
		{Name: "local"},    // 无 candidate
	}
	rep, err := Smoke(context.Background(), "prod", twoTasks(), tiers, nil, SmokeConfig{Samples: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.TiersRun != 1 {
		t.Fatalf("只有 1 档真跑, got %d", rep.TiersRun)
	}
	if rep.Passed {
		t.Fatal("两个未配置档位不该把它凑成'多档通过'")
	}
	for _, tr := range rep.Tiers {
		if tr.Tier == "fallback" && (tr.Available || tr.Passed) {
			t.Errorf("未配置档位应 Available=false 且 Passed=false, got %+v", tr)
		}
	}
}

// 两档都健康 → 放行。
func TestSmoke_双档全过则放行(t *testing.T) {
	good := map[string]string{"A": "OK-a", "B": "OK-b"}
	tiers := []Tier{
		{Name: "primary", Model: "big", Candidate: scriptedCandidate{name: "p", out: good}},
		{Name: "local", Model: "small", Candidate: scriptedCandidate{name: "l", out: good}},
	}
	rep, err := Smoke(context.Background(), "prod", twoTasks(), tiers, nil, SmokeConfig{Samples: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed {
		t.Fatalf("双档全健康应放行, blocking=%v", rep.Blocking)
	}
	for _, tr := range rep.Tiers {
		if tr.Samples != 4 { // 2 任务 × 2 采样
			t.Errorf("档位 %s 样本数应为 4, got %d", tr.Tier, tr.Samples)
		}
		if tr.Coverage != 1 || tr.FailRate != 0 {
			t.Errorf("档位 %s 应满覆盖零失败: %+v", tr.Tier, tr)
		}
	}
}

// 弱档位解析不出正确格式 (断言不过) → 该档失败率超限 → 整体拒绝。
// 这正是 H6 要抓的东西: 强模型能猜出意图, 弱模型直接跑偏。
func TestSmoke_弱档位解析失败则拒绝(t *testing.T) {
	tiers := []Tier{
		{Name: "primary", Candidate: scriptedCandidate{name: "p", out: map[string]string{"A": "OK", "B": "OK"}}},
		{Name: "weak", Candidate: scriptedCandidate{name: "w", out: map[string]string{"A": "胡言乱语", "B": "答非所问"}}},
	}
	rep, err := Smoke(context.Background(), "prod", twoTasks(), tiers, nil, SmokeConfig{Samples: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed {
		t.Fatal("有档位全失败却放行了")
	}
	var weak TierReport
	for _, tr := range rep.Tiers {
		if tr.Tier == "weak" {
			weak = tr
		}
	}
	if weak.FailRate != 1 || weak.Passed {
		t.Errorf("weak 档应 100%% 失败且不过: %+v", weak)
	}
	if !strings.Contains(strings.Join(weak.Notes, " "), "解析鲁棒性") {
		t.Errorf("失败原因要点明鲁棒性, got %v", weak.Notes)
	}
}

// 空产出计入失败并单独计数 (zero-turn 短路不启动评估器)。
func TestSmoke_空产出计失败且单独计数(t *testing.T) {
	tiers := []Tier{
		{Name: "a", Candidate: scriptedCandidate{name: "a", out: map[string]string{"A": "OK", "B": "OK"}}},
		{Name: "b", Candidate: scriptedCandidate{name: "b", out: map[string]string{}}}, // 全空
	}
	rep, err := Smoke(context.Background(), "prod", twoTasks(), tiers, nil, SmokeConfig{Samples: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range rep.Tiers {
		if tr.Tier != "b" {
			continue
		}
		if tr.ZeroTurns != 2 || tr.Failures != 2 {
			t.Errorf("空产出应既计 ZeroTurns 又计 Failures: %+v", tr)
		}
	}
	if rep.Passed {
		t.Error("有档位全空产出不该放行")
	}
}

// candidate 报错也算失败 (不是"跳过")。
func TestSmoke_候选报错计失败(t *testing.T) {
	tiers := []Tier{
		{Name: "a", Candidate: scriptedCandidate{name: "a", out: map[string]string{"A": "OK", "B": "OK"}}},
		{Name: "b", Candidate: scriptedCandidate{name: "b", err: errors.New("模型 400")}},
	}
	rep, err := Smoke(context.Background(), "prod", twoTasks(), tiers, nil, SmokeConfig{Samples: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed {
		t.Fatal("有档位全报错不该放行")
	}
}

// 任务集为空 → 拒绝并说明 (而不是"0 个任务全过")。
func TestSmoke_空任务集拒绝(t *testing.T) {
	rep, err := Smoke(context.Background(), "prod", nil, []Tier{{Name: "a"}}, nil, SmokeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed || len(rep.Blocking) == 0 {
		t.Fatalf("空任务集必须拒绝并说明: %+v", rep)
	}
}

// 采样指纹必须互不相同, 否则续跑判重会把同一任务的多次采样折成一条。
func TestSmoke_采样指纹互不相同(t *testing.T) {
	t1 := Task{ID: "x", Objective: "A", Input: "in"}
	t2 := t1
	t2.Input = t1.Input + "​"
	if t1.Fingerprint() == t2.Fingerprint() {
		t.Fatal("采样后缀必须改变指纹, 否则 Resume 会把多次采样当成一条")
	}
	// 而 objective 不能被改动 —— 那是真正喂给产物的东西。
	if t2.Objective != t1.Objective {
		t.Fatal("采样不得改动 Objective")
	}
}

// 档位名进文件名前要消毒 (档位名可能来自配置)。
func TestSanitizeTier_消毒路径字符(t *testing.T) {
	if got := sanitizeTier("../../etc/passwd"); strings.ContainsAny(got, "./") {
		t.Errorf("消毒后不该含路径字符: %q", got)
	}
	if got := sanitizeTier(""); got != "tier" {
		t.Errorf("空名应回落 tier, got %q", got)
	}
}
