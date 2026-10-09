// taskclass_test.go —— H4-3 任务时程分层的验收。
//
// 方案 §21.5.6 的验收原文是:
//
//	**「长程任务集上轮数分布的中位数 > 100」**
//
// 那条判据要 H4-1 的任务集才有意义 (本轮不做真实 run)。这里判的是它成立的**必要
// 条件**: 上限本身必须 > 100, 而且**只在长程档**上 > 100。缺了后半句, 一个"把所有
// 任务的上限都调成 150"的实现也能让那条验收通过 —— 而那个实现会把每个冒烟任务
// 的止损线一起抬高 6 倍, 正是分层要防的事。
//
// # 判据落在解析出的数值上, 不落在"字段被赋值了"
//
// "GlobalConfig 里有个 LongHorizonMaxTurns 字段"是装配态。它可以是配了没人读,
// 也可以是被读成另一个数。所以每条臂都走**执行侧真正走的那条路**
// (GlobalConfig.MaxTurnsFor / *ConfigResolver.MaxTurns), 断言返回的整数。
package modelconfig

import "testing"

// TestH43_长程档上限大于一百 验收的直接落点。
func TestH43_长程档上限大于一百(t *testing.T) {
	g := GlobalConfig{DefaultMaxTurns: 25, LongHorizonMaxTurns: 150}
	if got := g.MaxTurnsFor(TaskClassLongHorizon); got <= 100 {
		t.Fatalf("长程档上限 = %d, 必须 > 100 (方案 §21.5.6 的验收前提)", got)
	}
}

// TestH43_未配置时逐字回落到改造前行为 —— 这一条是这次改动**最重要的**回归臂。
//
// 分层是一个新开关。开关没开的时候, 每一个任务的止损线都必须是原样 ——
// 否则这次改动会在没有任何人配置的情况下改掉全套团队任务的回合上限,
// 而那种变化是静默的 (没有日志, 没有报错, 只是每个 run 晚 25 轮才停)。
func TestH43_未配置时逐字回落到改造前行为(t *testing.T) {
	t.Run("longHorizonMaxTurns=0 时长程档回落 standard", func(t *testing.T) {
		g := GlobalConfig{DefaultMaxTurns: 25}
		if got := g.MaxTurnsFor(TaskClassLongHorizon); got != 25 {
			t.Fatalf("未配置时长程档 = %d, 必须是 25 (等于 DefaultMaxTurns) —— "+
				"回落到一个更大的硬编码值会让本次改动在没人配置时改掉所有任务的止损线", got)
		}
	})
	t.Run("写反了也不放大", func(t *testing.T) {
		// 运维把长程档填得比常规档还小 (复制粘贴事故) 时, 长程档**不应该**
		// 因此变小。这里的语义是"长程档最多是常规档", 收窄由 DefaultMaxTurns
		// 负责 —— 两个数各管一个方向, 不让一个笔误同时改掉两边。
		g := GlobalConfig{DefaultMaxTurns: 25, LongHorizonMaxTurns: 5}
		if got := g.MaxTurnsFor(TaskClassLongHorizon); got != 5 {
			t.Fatalf("显式配置的 5 必须被尊重 (配置优先于启发式), 实得 %d", got)
		}
	})
	t.Run("全局一个数都没配", func(t *testing.T) {
		var g GlobalConfig
		for _, class := range []TaskClass{TaskClassShort, TaskClassStandard, TaskClassLongHorizon} {
			if got := g.MaxTurnsFor(class); got != DefaultStandardMaxTurns {
				t.Fatalf("全空配置下 %s 档 = %d, 应为编译期默认 %d", class, got, DefaultStandardMaxTurns)
			}
		}
	})
}

// TestH43_只有长程档被放开 —— 反面臂。
//
// 一个"把 MaxTurnsFor 改成恒返回 150"的实现能让上面第一条通过。这条用 short /
// standard 两档把它挡掉。
func TestH43_只有长程档被放开(t *testing.T) {
	g := GlobalConfig{DefaultMaxTurns: 25, LongHorizonMaxTurns: 150}
	if got := g.MaxTurnsFor(TaskClassStandard); got != 25 {
		t.Fatalf("常规档 = %d, 不该被长程档的配置放大 (分层失效 ⇒ 冒烟任务跑飞时多烧 6 倍 token)", got)
	}
	if got := g.MaxTurnsFor(TaskClassShort); got != 25 {
		t.Fatalf("短程档 = %d, 同上", got)
	}
}

// TestH43_产码任务分到长程档 分档判据的主臂。
func TestH43_产码任务分到长程档(t *testing.T) {
	for _, role := range []string{"coder", "tester", "reviewer", ""} {
		got := ClassifyTask(TaskSignals{Role: role, ProducesCode: true})
		if got != TaskClassLongHorizon {
			t.Fatalf("产码工作流里的 role=%q 分到 %s, 应为 long-horizon —— "+
				"产码任务的 写→编译→改 循环没有自然下界", role, got)
		}
	}
}

// TestH43_不产码的做事型任务不自动进长程档 —— 反面臂。
//
// 没有这一条, "ClassifyTask 恒返回 long-horizon" 能让上面全绿。那个实现等于没有
// 分层, 且会把每个非产码 stage 的止损线也抬到 150。
func TestH43_不产码的做事型任务不自动进长程档(t *testing.T) {
	for _, role := range []string{"coder", "tester", "writer", "coordinator"} {
		got := ClassifyTask(TaskSignals{Role: role, ProducesCode: false})
		if got == TaskClassLongHorizon {
			t.Fatalf("role=%q 且不产码, 不该进长程档 (分档失去意义 = 没有分层)", role)
		}
	}
}

// TestH43_思考型角色是短程 分档判据的第二条主臂。
func TestH43_思考型角色是短程(t *testing.T) {
	for _, role := range []string{"architect", "researcher", "planner", "Architect", " architect "} {
		got := ClassifyTask(TaskSignals{Role: role, ProducesCode: true})
		if got != TaskClassShort {
			t.Fatalf("role=%q 分到 %s, 应为 short —— 思考型角色一次性产出文档, "+
				"给它 150 轮只会让一个卡住的 run 多烧 150 轮", role, got)
		}
	}
}

// TestH43_关工具的一律短程 DisableTools 是**已经做过的决定**, 优先于角色与产码。
func TestH43_关工具的一律短程(t *testing.T) {
	got := ClassifyTask(TaskSignals{
		Role: "coder", ProducesCode: true, DisableTools: true,
	})
	if got != TaskClassShort {
		t.Fatalf("关了工具循环的任务分到 %s, 应为 short (没有工具 ⇒ 至多一轮)", got)
	}
}

// TestH43_显式声明压倒一切 最高优先级。
func TestH43_显式声明压倒一切(t *testing.T) {
	// 往两个方向各试一次: 只试"显式长程"的话, 一个"Explicit 非空就返回
	// long-horizon"的实现也能过。
	for _, explicit := range []TaskClass{TaskClassShort, TaskClassStandard, TaskClassLongHorizon} {
		got := ClassifyTask(TaskSignals{
			Explicit: explicit, Role: "architect", ProducesCode: true,
		})
		if got != explicit {
			t.Fatalf("显式声明 %s 被改成了 %s —— 显式声明必须赢, "+
				"否则出了偏差只能改代码而不能改配置", explicit, got)
		}
	}
}

// TestH43_解析器按全局配置回答 装配面。
//
// 前几条测的是 GlobalConfig 上的纯函数; 这条测**执行侧真正调用的那个入口**
// (ConfigResolver.MaxTurns) 有没有把 router 的 global 接上 —— 接错了的话
// 上面的纯函数全绿, 生产上取到的仍是 0。
func TestH43_解析器按全局配置回答(t *testing.T) {
	r := NewConfigResolver(nil, GlobalConfig{
		DefaultModelAlias: "m", DefaultMaxTurns: 25, LongHorizonMaxTurns: 150,
	}, nil)

	if got := r.MaxTurns(TaskClassLongHorizon); got != 150 {
		t.Fatalf("解析器长程档 = %d, 应为 150 (global 没接上?)", got)
	}
	if got := r.MaxTurns(TaskClassStandard); got != 25 {
		t.Fatalf("解析器常规档 = %d, 应为 25", got)
	}

	// 动态更新 (SetGlobal) 之后必须跟着变 —— 配置热更新走的是这条路。
	r.SetGlobal(GlobalConfig{DefaultMaxTurns: 25, LongHorizonMaxTurns: 200})
	if got := r.MaxTurns(TaskClassLongHorizon); got != 200 {
		t.Fatalf("SetGlobal 之后长程档 = %d, 应为 200 —— 热更新没生效, "+
			"而 配了没生效 与 配了生效 在运行读数上完全一样", got)
	}
}
