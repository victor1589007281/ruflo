package weakmodel

import "testing"

func TestResolveExplicitOverride(t *testing.T) {
	cases := []struct {
		env    string
		wantOn bool
	}{
		{"1", true}, {"true", true}, {"on", true}, {"YES", true},
		{"0", false}, {"false", false}, {"off", false}, {"no", false},
	}
	for _, c := range cases {
		p := Resolve("kimi", func(string) string { return c.env })
		if p.Enabled != c.wantOn {
			t.Errorf("env=%q: Enabled=%v, want %v", c.env, p.Enabled, c.wantOn)
		}
	}
	// 显式开对非 ollama provider 也生效
	p := Resolve("kimi", func(string) string { return "1" })
	if !p.Enabled || !p.SchemaRetry || !p.ToolMasking || !p.PlanningHints || !p.ResultPostprocess {
		t.Errorf("explicit on should enable all levers, got %+v", p)
	}
}

func TestResolveAutoByProvider(t *testing.T) {
	env := func(string) string { return "" }
	if p := Resolve("ollama", env); !p.Enabled {
		t.Error("provider=ollama 应自动启用弱模型增强")
	}
	if p := Resolve("kimi", env); p.Enabled {
		t.Error("provider=kimi 不应自动启用")
	}
	if p := Resolve("", env); p.Enabled {
		t.Error("空 provider 不应自动启用")
	}
}

func TestResolveMaskExtra(t *testing.T) {
	env := func(k string) string {
		if k == EnvMaskExtra {
			return " Foo , Bar,,"
		}
		return "1"
	}
	p := Resolve("ollama", env)
	if len(p.MaskedExtra) != 2 || p.MaskedExtra[0] != "Foo" || p.MaskedExtra[1] != "Bar" {
		t.Errorf("MaskedExtra 解析错误: %+v", p.MaskedExtra)
	}
}

func TestDefaultMaskedToolsNonEmpty(t *testing.T) {
	if len(DefaultMaskedTools) < 20 {
		t.Errorf("默认掩码清单过短: %d", len(DefaultMaskedTools))
	}
	seen := map[string]bool{}
	for _, n := range DefaultMaskedTools {
		if seen[n] {
			t.Errorf("掩码清单重复: %s", n)
		}
		seen[n] = true
	}
	// 编码核心工具绝不允许进掩码清单
	for _, core := range []string{"Shell", "Read", "Write", "StrReplace", "Glob", "Grep", "TodoWrite"} {
		if seen[core] {
			t.Errorf("核心工具 %s 被误掩码", core)
		}
	}
}

// TestRewindStaysVisibleToWeakModel 回滚出口 (H3-1) 不能被掩掉。
//
// 弱模型改坏文件的概率本来就高, 而它恰恰最不擅长"凭记忆把文件改回去" —— 那是
// 长程任务里最常见的一种自毁 (第 80 轮亲手抹掉第 60 轮的成果)。掩掉 Rewind
// 等于把这个档位最需要的一格拿掉。
//
// 两条都断: 既不能进静态掩码 (执行面硬门), 也必须在核心可见集里 (动态掩码的
// 下限 —— 粘性集合只增不减, 核心集是每次会话开局就有的那一批)。
func TestRewindStaysVisibleToWeakModel(t *testing.T) {
	const name = "Rewind"
	for _, n := range DefaultMaskedTools {
		if n == name {
			t.Fatalf("%s 被放进了静态掩码清单 —— 弱模型看不到回滚出口", name)
		}
	}
	for _, n := range CoreVisibleTools {
		if n == name {
			return
		}
	}
	t.Fatalf("%s 不在 CoreVisibleTools 里 —— 弱模型默认看不到回滚出口", name)
}
