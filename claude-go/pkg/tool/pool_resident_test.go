// pool_resident_test.go —— 13.7-P2 常驻面状态机 + 数量护栏 + 未命中回退开关单测。
//
// 锁定验收 (docforge planning-skills-surge §13.7.5/§13.7.7):
//  1. SetResident 整面替换 / 只收池成员 / 治理集外拒绝 (denied 命中即拒,
//     allowed fail-closed) / changed 返回语义;
//  2. resident 与 loaded 平级: Release 只收回 loaded 不收回 resident,
//     Advertised = always ∪ 池外 ∪ loaded ∪ resident;
//  3. PoolFallbackEnabled (默认开, falsy 才关) —— 与 PoolEnabled 相反方向;
//  4. CheckGuardrails 软上限盘点 (超限不拒绝, 只出告警事实)。
package tool

import (
	"reflect"
	"testing"
)

// ---------------------------------------------------------------------------
// 1. SetResident: 整面替换 + 池成员过滤 + 治理拒绝
// ---------------------------------------------------------------------------

func TestSetResidentFaceSemantics(t *testing.T) {
	p := NewPool()
	p.AddMember("A")
	p.AddMember("B")
	p.AddMember("C")
	p.SetGovernance(map[string]bool{"B": true}, nil) // B 被 denied

	// 首次设面: ghost 与 denied B 被滤掉, 只收池内治理集外成员。
	if !p.SetResident([]string{"A", "B", "C", "ghost"}) {
		t.Fatal("首次设面应返回 changed=true")
	}
	if got := p.ResidentNames(); !reflect.DeepEqual(got, []string{"A", "C"}) {
		t.Fatalf("ResidentNames=%v, want [A C] (B denied 拒, ghost 非池成员拒)", got)
	}

	// 同面再设: changed=false (调用方据此决定是否记日志/留痕)。
	if p.SetResident([]string{"A", "B", "C", "ghost"}) {
		t.Error("同面重复设置应返回 changed=false")
	}

	// 整面替换: 新名单替换旧名单 (A 移出)。
	if !p.SetResident([]string{"C"}) {
		t.Fatal("面收缩应返回 changed=true")
	}
	if got := p.ResidentNames(); !reflect.DeepEqual(got, []string{"C"}) {
		t.Fatalf("替换后 ResidentNames=%v, want [C]", got)
	}
	if p.IsResident("A") {
		t.Error("整面替换后 A 不应仍在常驻面")
	}

	// 空名单 = 清面。
	if !p.SetResident(nil) {
		t.Fatal("清面应返回 changed=true")
	}
	if len(p.ResidentNames()) != 0 {
		t.Error("空名单应清空常驻面")
	}

	// allowed fail-closed: 白名单外的成员不得进常驻面 (不能变成绕过 AllowedTools 的后门)。
	p2 := NewPool()
	p2.AddMember("X")
	p2.AddMember("Y")
	p2.SetGovernance(nil, map[string]bool{"X": true})
	if !p2.SetResident([]string{"X", "Y"}) {
		t.Fatal("X 进面应返回 changed=true")
	}
	if got := p2.ResidentNames(); !reflect.DeepEqual(got, []string{"X"}) {
		t.Fatalf("allowed fail-closed: ResidentNames=%v, want [X]", got)
	}

	// nil 池安全。
	var np *Pool
	if np.SetResident([]string{"x"}) {
		t.Error("nil pool SetResident must be false")
	}
	if np.ResidentNames() != nil || np.IsResident("x") {
		t.Error("nil pool resident accessors must be zero-value")
	}
}

// ---------------------------------------------------------------------------
// 2. resident ∥ loaded: Release 不收回常驻, Advertised 四路并集
// ---------------------------------------------------------------------------

func TestResidentIndependentOfRelease(t *testing.T) {
	p := NewPool()
	p.AddMember("A")
	p.AddMember("B")
	p.Load("A")
	p.Load("B")
	p.SetResident([]string{"A"})

	// Release 只动 loaded: A 被释放 (loaded 收回) 但 resident 保留。
	if !p.Release("A") || !p.Release("B") {
		t.Fatal("Release(loaded) must be true")
	}
	if p.IsLoaded("A") {
		t.Error("A 已释放, IsLoaded 应为 false")
	}
	if !p.IsResident("A") {
		t.Error("Release 不得收回 resident (常驻是广告位不是加载状态)")
	}
	if !p.Advertised("A") {
		t.Error("常驻资产在 loaded 收回后仍应在广告面 (resident ∪)")
	}
	// B 无常驻, 释放后移出广告面。
	if p.Advertised("B") {
		t.Error("已释放且无常驻的成员应移出广告面")
	}

	// 常驻成员未加载也直接进广告面 (免 search 的语义本体)。
	p3 := NewPool()
	p3.AddMember("R")
	p3.SetResident([]string{"R"})
	if p3.IsLoaded("R") {
		t.Error("SetResident 不得顺带置 loaded (与加载面平级不重叠)")
	}
	if !p3.Advertised("R") {
		t.Error("常驻成员应免加载直接进广告面")
	}
}

// ---------------------------------------------------------------------------
// 3. PoolFallbackEnabled (默认开)
// ---------------------------------------------------------------------------

func TestPoolFallbackEnabled(t *testing.T) {
	if !PoolFallbackEnabled(nil) {
		t.Error("nil getenv 必须默认开 (回退是保护语义)")
	}
	if !PoolFallbackEnabled(func(string) string { return "" }) {
		t.Error("未设置必须默认开")
	}
	for _, val := range []string{"0", "false", "FALSE", "off", "no", " 0 ", " Off "} {
		if PoolFallbackEnabled(func(string) string { return val }) {
			t.Errorf("PoolFallbackEnabled(%q) 应为 false (显式 falsy 才关)", val)
		}
	}
	for _, val := range []string{"1", "true", "yes", "whatever"} {
		if !PoolFallbackEnabled(func(string) string { return val }) {
			t.Errorf("PoolFallbackEnabled(%q) 应为 true (非 falsy 即开)", val)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. CheckGuardrails 软上限盘点
// ---------------------------------------------------------------------------

func TestCheckGuardrails(t *testing.T) {
	// 阈值内: 全部不告警。
	for _, g := range CheckGuardrails(MaxPoolTools, MaxActiveSkills, MaxShadowSkills) {
		if g.Exceeded {
			t.Errorf("%s 恰在上限 (%d/%d) 不应告警 (软上限是 > 不是 >=)", g.Kind, g.Current, g.Limit)
		}
	}
	// 超限: 只出事实不拒绝。
	gs := CheckGuardrails(MaxPoolTools+1, MaxActiveSkills+3, MaxShadowSkills)
	want := map[string]struct {
		cur, lim int
		exceeded bool
	}{
		"tool":   {MaxPoolTools + 1, MaxPoolTools, true},
		"skill":  {MaxActiveSkills + 3, MaxActiveSkills, true},
		"shadow": {MaxShadowSkills, MaxShadowSkills, false},
	}
	for _, g := range gs {
		w, ok := want[g.Kind]
		if !ok {
			t.Fatalf("未知护栏类别 %q", g.Kind)
		}
		if g.Current != w.cur || g.Limit != w.lim || g.Exceeded != w.exceeded {
			t.Errorf("%s 护栏 = {%d, %d, %v}, want {%d, %d, %v}", g.Kind, g.Current, g.Limit, g.Exceeded, w.cur, w.lim, w.exceeded)
		}
	}
}
