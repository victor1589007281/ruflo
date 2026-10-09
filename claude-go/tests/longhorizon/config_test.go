package longhorizon

import (
	"strings"
	"testing"
)

func mustDoc(t *testing.T, s string) any {
	t.Helper()
	d, err := ParseConfigDoc([]byte(s))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return d
}

// TestH42_配置diff按语义路径报 —— 缩进/键序/数字写法不算差异。
//
// 这条挡的是"文本 diff"那个诱人的错误实现: 用文本 diff 时, 只要有人重排了一次
// 键, 两组就会报出几十处"差异", 于是"只差被测开关"这条永远过不了, 人会把它删掉。
func TestH42_配置diff按语义路径报(t *testing.T) {
	a := mustDoc(t, `{"ai":{"maxTurns":25,"maxTokens":32768},"engine":{"x":true}}`)
	b := mustDoc(t, `{
	  "engine": {"x": true},
	  "ai": {"maxTokens": 32768, "maxTurns": 25}
	}`)
	if d := DiffPaths(a, b); len(d) != 0 {
		t.Fatalf("键序不同不该算差异, 实得 %v", d)
	}

	c := mustDoc(t, `{"ai":{"maxTurns":25,"maxTokens":32768},"engine":{"x":false}}`)
	d := DiffPaths(a, c)
	if len(d) != 1 || !strings.Contains(d[0], "/engine/x") {
		t.Fatalf("应只报 /engine/x, 实得 %v", d)
	}
}

// TestH42_缺失键与类型变化都报 —— 报空集合是这里最危险的一种正确。
func TestH42_缺失键与类型变化都报(t *testing.T) {
	a := mustDoc(t, `{"ai":{"maxTurns":25,"maxTokens":32768}}`)
	b := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	d := DiffPaths(a, b)
	if len(d) != 1 || !strings.Contains(d[0], "/ai/maxTokens") || !strings.Contains(d[0], "(缺失)") {
		t.Fatalf("缺键必须报出, 实得 %v", d)
	}

	c := mustDoc(t, `{"ai":{"maxTurns":{"nested":25},"maxTokens":32768}}`)
	d2 := DiffPaths(a, c)
	if len(d2) == 0 {
		t.Fatal("标量变成对象必须报出 —— 报空会让「只差一个开关」这句话失去意义")
	}
}

// TestH42_数组按下标比对 —— 长度不同要报出来。
func TestH42_数组按下标比对(t *testing.T) {
	a := mustDoc(t, `{"xs":[1,2,3]}`)
	b := mustDoc(t, `{"xs":[1,9,3]}`)
	d := DiffPaths(a, b)
	if len(d) != 1 || !strings.Contains(d[0], "/xs[1]") {
		t.Fatalf("应报 /xs[1], 实得 %v", d)
	}

	c := mustDoc(t, `{"xs":[1,2]}`)
	d2 := DiffPaths(a, c)
	found := false
	for _, s := range d2 {
		if strings.Contains(s, "长度") {
			found = true
		}
	}
	if !found {
		t.Fatalf("长度不同必须报出, 实得 %v", d2)
	}
}

// TestH42_只差被测开关才算配对 —— 主臂。
func TestH42_只差被测开关才算配对(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25,"maxTokens":32768}}`)
	pair, err := MaxTurnsPair(base)
	if err != nil {
		t.Fatalf("构造配对失败: %v", err)
	}
	if err := pair.AssertOnlyKnobDiffers(); err != nil {
		t.Fatalf("只差一个键的配对被判不合规: %v", err)
	}
	if !strings.Contains(pair.DiffSummary(), "/ai/longHorizonMaxTurns") {
		t.Fatalf("差异说明里必须出现被测键:\n%s", pair.DiffSummary())
	}
}

// TestH42_多出的差异必须被抓住 —— 反面臂, 这条是整个 H4-2 的地基。
//
// 两组若还差着别的东西, 成功率之差就不能归给被测开关。而这类漂移在汇总数字上
// 完全看不出来 (B 组高了 12 个点, 看起来像"这个开关有效")。
func TestH42_多出的差异必须被抓住(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25,"maxTokens":32768}}`)
	pair, err := MaxTurnsPair(base)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	// 有人只给 B 组改了 maxTurns —— 典型的"演进中漂移"。
	pair.B.Config = mustDoc(t, `{"ai":{"maxTurns":150,"maxTokens":32768,"longHorizonMaxTurns":150}}`)

	err = pair.AssertOnlyKnobDiffers()
	if err == nil {
		t.Fatal("B 组多改了 maxTurns, 必须被判不合规")
	}
	if !strings.Contains(err.Error(), "/ai/maxTurns") {
		t.Fatalf("报错必须指名多出来的是哪一处, 实得: %v", err)
	}
}

// TestH42_开关没真的被改到也要报 —— 反面臂, 最难识破的一种。
//
// 两组输入逐字节相同时, 报告会给出"两组成功率完全相同 ⇒ 这个开关没有效果"。
// 而真相是开关设错了 (变量名拼错/取值非法), 根本没有 A/B。
func TestH42_开关没真的被改到也要报(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair := ConfigPair{
		Base: base,
		A:    RunSpec{Config: base},
		B:    RunSpec{Config: base}, // 忘了改
		Knob: MaxTurnsKnob(),
	}
	err := pair.AssertOnlyKnobDiffers()
	if err == nil {
		t.Fatal("两组输入完全相同时必须报错, 实得 nil —— 那不是一次 A/B")
	}
	if !strings.Contains(err.Error(), "完全相同") {
		t.Fatalf("报错应指出两组完全相同, 实得: %v", err)
	}
}

// TestH42_环境变量上的开关也认 —— 闸门那一组的形态。
//
// 闸门不在配置里 (它是 CLAUDE_GO_RUN_INTERCEPTORS 上的白名单项), 一个只核配置的
// 台子会对它给出"两组配置完全相同"的假绿。
func TestH42_环境变量上的开关也认(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair := GatePair(base)
	if err := pair.AssertOnlyKnobDiffers(); err != nil {
		t.Fatalf("闸门配对被判不合规: %v", err)
	}
	if !strings.Contains(pair.DiffSummary(), runInterceptorsEnv) {
		t.Fatalf("差异说明里必须出现环境变量名:\n%s", pair.DiffSummary())
	}
}

// TestH42_环境变量上的开关被单独认出来 —— 正面臂, 挡的是"所有 env 差异一律算不合规"。
//
// 只看反面 (多设一个变量要报错) 的话, 一个把 env 差异**全部**归进 offKnob 的实现
// 照样能过那条 —— 而它会让每一个走环境变量的开关 (如闸门) 永远无法配对。
// 所以这里正面判一次: 只差被测环境变量时**必须**判合规。
func TestH42_环境变量上的开关被单独认出来(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair := GatePair(base)

	// 台子必须认出这处 env 差异**属于被测开关**, 而不是"多出来的差异"。
	if err := pair.AssertOnlyKnobDiffers(); err != nil {
		t.Fatalf("只差被测环境变量的配对被判不合规 (env 差异没被认到开关上): %v", err)
	}

	// 开关名写错时 (env 变量名对不上配置声明), 同一对必须被判不合规 ——
	// 否则一个"随便设了个变量"的台子也会被判成有效 A/B。
	wrong := pair
	wrong.Knob = KnobEnv{Name: "闸门", Env: "CLAUDE_GO_TYPO", ConfigPath: ""}
	if err := wrong.AssertOnlyKnobDiffers(); err == nil {
		t.Fatal("声明的环境变量名与实际改的不是同一个, 必须被判不合规")
	}

	// 声明里没说走 env (Env 为空) 时, env 上的差异必须算作"多出来的差异"。
	noEnv := pair
	noEnv.Knob = KnobEnv{Name: "闸门", ConfigPath: ""}
	if err := noEnv.AssertOnlyKnobDiffers(); err == nil {
		t.Fatal("开关声明没走 env 时, env 上的差异必须被算成多出来的差异")
	}
}

// TestH42_环境变量上的额外差异也要被抓住 —— 反面臂。
func TestH42_环境变量上的额外差异也要被抓住(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair := GatePair(base)
	pair.B.Env["CLAUDE_GO_SOMETHING_ELSE"] = "1" // 有人只给 B 组加了个变量
	if err := pair.AssertOnlyKnobDiffers(); err == nil {
		t.Fatal("B 组多设了一个环境变量, 必须被判不合规 —— 否则任何增益都可能来自它")
	}
}

// TestH42_含点的键不被路径归一化弄错 —— 反面臂。
//
// 模型别名/版本号里本来就有点 (qwen2.5-coder)。若把点当路径分隔符, 这个键会被
// 悄悄改写成另一条路径, "只差被测开关"的判断随之失效。
func TestH42_含点的键不被路径归一化弄错(t *testing.T) {
	base := mustDoc(t, `{"ai":{"modelAlias":"ollama:qwen2.5-coder"}}`)
	pair := ConfigPair{
		Base: base,
		A:    RunSpec{Config: mustDoc(t, `{"ai":{"modelAlias":"ollama:qwen2.5-coder"}}`)},
		B:    RunSpec{Config: mustDoc(t, `{"ai":{"modelAlias":"ollama:qwen2.5-coder","longHorizonMaxTurns":150}}`)},
		Knob: MaxTurnsKnob(),
	}
	// 两组都**没有** longHorizonMaxTurns 之外的名目, 但 B 有了这个键而 A 没有 ——
	// Knob 路径是 /ai/longHorizonMaxTurns, 正好命中。
	if err := pair.AssertOnlyKnobDiffers(); err != nil {
		t.Fatalf("含点的键不该影响路径判定: %v", err)
	}
	// 再确认: 别名里那个 "2.5" 没有被当成路径 /ai/modelAlias/ollama:qwen2/5-coder。
	if d := DiffPaths(pair.A.Config, pair.B.Config); len(d) != 1 {
		t.Fatalf("应只报一处差异, 实得 %v", d)
	}
}

// TestH42_路径本身含点时也不被归一化改写 —— 这条才真正看着 normalizePath。
//
// 上面那条用的是**值**里含点的别名 (ollama:qwen2.5-coder), 但值从来不参与路径比较 ——
// 于是把 normalizePath 改成"把点当路径分隔符"的变异照样全绿 (变异 M5 就是这么活下来的)。
// 要害在于**键本身含点**: 配置里 `"qwen2.5-coder"` 这样的键是合法的, 差异行会写成
// `/ai/qwen2.5-coder/maxTurns`; 若归一化把点换成斜杠, 声明的被测路径就被悄悄改写成
// 另一条路径 —— 于是"只差被测开关"的判定失效, 而报告上看不出来。
func TestH42_路径本身含点时也不被归一化改写(t *testing.T) {
	a := mustDoc(t, `{"ai":{"qwen2.5-coder":{"maxTurns":25}}}`)
	b := mustDoc(t, `{"ai":{"qwen2.5-coder":{"maxTurns":150}}}`)

	pair := ConfigPair{
		Base: a,
		A:    RunSpec{Config: a},
		B:    RunSpec{Config: b},
		Knob: KnobEnv{Name: "含点键的开关", ConfigPath: "/ai/qwen2.5-coder/maxTurns"},
	}
	// 差异确实只在被测路径上 —— 必须判合规。
	if err := pair.AssertOnlyKnobDiffers(); err != nil {
		t.Fatalf("被测路径含点时被判不合规 —— 归一化把含点的键改写了: %v", err)
	}
	// 无论归一化怎么写, 差异行的路径必须是原样那份。
	if d := DiffPaths(a, b); len(d) != 1 || pathOf(d[0]) != "/ai/qwen2.5-coder/maxTurns" {
		t.Fatalf("差异路径被改写: %v", d)
	}
}

// TestH42_SetPath按路径写值 —— 生成两组配置的手段。
func TestH42_SetPath按路径写值(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25,"maxTokens":32768}}`)
	out, err := SetPath(base, "/ai/longHorizonMaxTurns", 150)
	if err != nil {
		t.Fatalf("写路径失败: %v", err)
	}
	// 原文档不能被改动 —— 两组都从 base 派生, 改一处会同时改两组。
	m := base.(map[string]any)["ai"].(map[string]any)
	if _, leaked := m["longHorizonMaxTurns"]; leaked {
		t.Fatal("SetPath 改动了原文档 —— 两组派生自同一份 base 时会互相污染")
	}
	om := out.(map[string]any)["ai"].(map[string]any)
	if om["longHorizonMaxTurns"] != 150 {
		t.Fatalf("写进去的值 = %v", om["longHorizonMaxTurns"])
	}
	// 建不存在的中间路径。
	out2, err := SetPath(base, "/engine/gate/enabled", true)
	if err != nil {
		t.Fatalf("写新路径失败: %v", err)
	}
	if out2.(map[string]any)["engine"].(map[string]any)["gate"].(map[string]any)["enabled"] != true {
		t.Fatal("中间路径没有被建出来")
	}
}
