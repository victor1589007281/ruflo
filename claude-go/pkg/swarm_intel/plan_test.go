package swarm_intel

import (
	"context"
	"testing"
)

// TestPlanTopologyMatchesFlow 钉住"传播方式按信息怎么流动选"。
//
// 曾经的默认: 所有模式都用完全图 —— 对市场/博弈成立, 对舆论扩散/邻里依从/组织
// 层级不成立 (完全图把结构效应抹平, 人人看见人人, 涌现就只剩平均化)。
func TestPlanTopologyMatchesFlow(t *testing.T) {
	cases := map[string]string{
		"social":    "smallworld", // 熟人圈 + 长程桥
		"rumor":     "smallworld",
		"org":       "smallworld", // 部门内密集 + 跨部门少量通道
		"health":    "ring",       // 邻里依从
		"climate":   "ring",
		"market":    "full", // 全局价格信号
		"game":      "full",
		"policy":    "full",
		"education": "full",
	}
	for mode, want := range cases {
		p := planFor("simulate", mode, "某市地铁涨价后通勤客流会怎么变化")
		if p.Topology != want {
			t.Errorf("mode %s 拓扑 = %q, want %q", mode, p.Topology, want)
		}
		if p.Rounds < 4 || p.Rounds > 10 {
			t.Errorf("mode %s 轮次帽 %d 超出 [4,10]", mode, p.Rounds)
		}
		if p.MinRounds != 3 {
			t.Errorf("mode %s MinRounds = %d, want 3 (2 轮太容易误判收敛)", mode, p.MinRounds)
		}
		if p.Agents < 4 || p.Agents > 8 {
			t.Errorf("mode %s agent 数 %d 超出 [4,8]", mode, p.Agents)
		}
		if p.Why == "" {
			t.Errorf("mode %s 缺 Why: 一键模式要能解释参数依据", mode)
		}
	}
}

// TestPlanRoundsGrowWithComplexity 钉住"阶段信号抬轮次帽, 但不超过 10"。
func TestPlanRoundsGrowWithComplexity(t *testing.T) {
	simple := planFor("simulate", "social", "大家怎么看这件事")
	if simple.Rounds != modePlans["social"].rounds {
		t.Errorf("无阶段信号时不该抬帽: got %d, want %d", simple.Rounds, modePlans["social"].rounds)
	}
	complex := planFor("simulate", "social", "先是政策出台, 然后行业逐步调整, 随后舆情反复, 最终形成长期格局")
	if complex.Rounds <= simple.Rounds {
		t.Errorf("多阶段问题的轮次帽没有抬高: %d vs %d", complex.Rounds, simple.Rounds)
	}
	if complex.Rounds > 10 {
		t.Errorf("轮次帽超过上限 10: %d", complex.Rounds)
	}
	if complex.Why == simple.Why {
		t.Error("抬帽时 Why 应说明原因")
	}
}

// TestPlanUnknownModeFallsBack 钉住目录外模式不会得到零值计划 (零值=CLI 默认 3 轮)。
func TestPlanUnknownModeFallsBack(t *testing.T) {
	p := planFor("simulate", "no-such-mode", "随便什么问题")
	if p.Topology == "" || p.Rounds < 4 || p.Agents == 0 {
		t.Fatalf("未知模式没有兜底: %+v", p)
	}
}

// TestPlanPredictHasNoTopology 钉住预测链路不塞拓扑参数。
func TestPlanPredictHasNoTopology(t *testing.T) {
	p := planFor("predict", "social", "明年会不会降息")
	if p.Topology != "" || p.Rounds != 0 || p.Why == "" {
		t.Fatalf("预测计划不该带拓扑/轮次: %+v", p)
	}
}

// TestClassificationCarriesPlan 钉住 ClassifyObjective 的**每条**返回路径都带 Plan
// (成功路径与 LLM 失败的关键词兜底路径都要有)。
func TestClassificationCarriesPlan(t *testing.T) {
	// 成功路径
	ok := NewEngine(&classifyLLM{resp: `{"kind":"simulate","modes":[{"mode":"climate","score":0.9}],"reason":"社区行为"}`},
		Config{StoreBackend: "jsonl", DataDir: t.TempDir()})
	c := ok.ClassifyObjective(context.Background(), "社区垃圾分类推行为什么会走样")
	if c.Plan == nil {
		t.Fatal("成功识别的 Classification 没有 Plan")
	}
	if c.Plan.Topology != "ring" || c.Plan.Rounds < 4 {
		t.Fatalf("climate 应给环格拓扑与 ≥4 轮帽: %+v", c.Plan)
	}

	// 兜底路径 (LLM 报错): Plan 依然要按兜底出来的主模式给
	bad := NewEngine(&classifyLLM{err: errBoom{}}, Config{StoreBackend: "jsonl", DataDir: t.TempDir()})
	f := bad.ClassifyObjective(context.Background(), "社区垃圾分类推行为什么会走样")
	if !f.Fallback || f.Plan == nil {
		t.Fatalf("兜底路径应带 Plan: fallback=%v plan=%+v", f.Fallback, f.Plan)
	}
	if f.Plan.Topology == "" {
		t.Fatalf("兜底路径的 Plan 也要有拓扑: %+v", f.Plan)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "llm unavailable" }
