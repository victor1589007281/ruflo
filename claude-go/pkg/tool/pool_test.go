// pool_test.go —— 13.7-P0 池状态机单测 (docforge planning-skills-surge §13.7.7)。
//
// 锁定四组验收:
//  1. PoolEnabled env 开关解析 (显式开/关, 默认关, nil getenv 关);
//  2. AddMember/Load/Release/IsLoaded/MemberNames 状态机 (登记序去重, 幂等);
//  3. Advertised 三分支 (always / 池外 / loaded) 与 nil 池零变化;
//  4. FilterAPITools (nil 池原样同切片 / 按广告面过滤) + Governed 治理
//     (denied 命中拒 / allowed fail-closed / 未设放行)。
package tool

import (
	"reflect"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// ---------------------------------------------------------------------------
// 1. PoolEnabled
// ---------------------------------------------------------------------------

func TestPoolEnabled(t *testing.T) {
	on := map[string]bool{"1": true, "true": true, "TRUE": true, "on": true, "yes": true, " Yes ": true}
	for val, want := range on {
		if got := PoolEnabled(func(string) string { return val }); got != want {
			t.Errorf("PoolEnabled(%q)=%v, want true", val, got)
		}
	}
	off := []string{"", "0", "false", "no", "off", "whatever", "  "}
	for _, val := range off {
		if got := PoolEnabled(func(string) string { return val }); got {
			t.Errorf("PoolEnabled(%q)=%v, want false", val, got)
		}
	}
	// 未设置 (空 getenv 实现) 与 nil getenv 都关。
	if PoolEnabled(func(string) string { return "" }) {
		t.Error("PoolEnabled(unset) must be false")
	}
	if PoolEnabled(nil) {
		t.Error("PoolEnabled(nil) must be false")
	}
}

// ---------------------------------------------------------------------------
// 2. 状态机
// ---------------------------------------------------------------------------

func TestPoolMembershipAndLoad(t *testing.T) {
	p := NewPool()
	if len(p.MemberNames()) != 0 {
		t.Fatal("new pool must be empty")
	}

	// 登记序: 重复登记忽略, 空名忽略。
	p.AddMember("B")
	p.AddMember("A")
	p.AddMember("B")
	p.AddMember("")
	if got := p.MemberNames(); !reflect.DeepEqual(got, []string{"B", "A"}) {
		t.Fatalf("MemberNames()=%v, want [B A] (登记序去重)", got)
	}

	// 未登记的 Load 拒绝 (含 AddMember 空名回退的情形)。
	if p.Load("X") {
		t.Error("Load(unregistered) must be false")
	}
	if !p.Load("B") || !p.Load("A") {
		t.Fatal("Load(member) must be true")
	}
	if p.Load("B") {
		t.Error("Load(already loaded) must be false (幂等)")
	}
	if !p.IsLoaded("B") || !p.IsLoaded("A") || p.IsLoaded("X") {
		t.Fatal("IsLoaded state mismatch")
	}

	// Release: 未加载拒绝; 已加载释放后 IsLoaded 翻转, 再 Release 拒绝。
	if p.Release("X") {
		t.Error("Release(not loaded) must be false")
	}
	if !p.Release("A") {
		t.Fatal("Release(loaded) must be true")
	}
	if p.Release("A") {
		t.Error("double Release must be false")
	}
	if p.IsLoaded("A") {
		t.Error("released member must not be loaded")
	}

	// 释放后重新 Load 可行 (会话内循环)。
	if !p.Load("A") {
		t.Error("re-Load after release must be true")
	}
}

// ---------------------------------------------------------------------------
// 3. Advertised
// ---------------------------------------------------------------------------

func TestPoolAdvertised(t *testing.T) {
	p := NewPool()
	p.AddMember("A")
	p.AddMember("B")
	p.SetAlways("always1")

	// always: 池内但恒常驻。
	if !p.Advertised("always1") {
		t.Error("always member must be advertised")
	}
	// 池外 (未登记): 恒可见 (平滑迁移②: 54 内置保留广告)。
	if !p.Advertised("Read") {
		t.Error("non-member must be advertised")
	}
	// 池内未加载: 不可见 (「注册不懒、广告懒」)。
	if p.Advertised("A") {
		t.Error("unloaded member must NOT be advertised")
	}
	// 池内已加载: 可见。
	p.Load("A")
	if !p.Advertised("A") {
		t.Error("loaded member must be advertised")
	}
	// 未加载成员 (B) 在 A 加载后仍不可见。
	if p.Advertised("B") {
		t.Error("unloaded member B must stay unadvertised")
	}

	// nil 池: 恒 true (默认关, 行为零变化)。
	var np *Pool
	if !np.Advertised("anything") {
		t.Error("nil pool must advertise everything")
	}
}

// ---------------------------------------------------------------------------
// 4. FilterAPITools + Governed
// ---------------------------------------------------------------------------

func TestFilterAPITools(t *testing.T) {
	mk := func(names ...string) []types.APITool {
		out := make([]types.APITool, 0, len(names))
		for _, n := range names {
			out = append(out, types.APITool{Name: n, Description: "d:" + n})
		}
		return out
	}

	// nil 池: 原样返回同一切片 (零分配)。
	var np *Pool
	tools := mk("Read", "Bash")
	if got := np.FilterAPITools(tools); !sameAPITools(got, tools) || &got[0] != &tools[0] {
		t.Error("nil pool must return the same slice")
	}

	p := NewPool()
	p.AddMember("Lazy1")
	p.AddMember("Lazy2")
	p.Load("Lazy1")
	got := p.FilterAPITools(mk("Read", "Lazy1", "Lazy2", "Bash", "Lazy1"))
	want := []string{"Read", "Lazy1", "Bash", "Lazy1"} // Lazy2 未加载被滤掉, Lazy1 重复保留
	if len(got) != len(want) {
		t.Fatalf("FilterAPITools len=%d, want %d (%v)", len(got), len(want), names(got))
	}
	for i, n := range want {
		if got[i].Name != n {
			t.Errorf("FilterAPITools[%d]=%s, want %s", i, got[i].Name, n)
		}
	}
}

func TestGoverned(t *testing.T) {
	// 无治理集: 放行。
	p := NewPool()
	if !p.Governed("anything") {
		t.Error("no governance must allow")
	}

	// denied 命中即拒; 未命中放行。
	p.SetGovernance(map[string]bool{"denied1": true}, nil)
	if p.Governed("denied1") {
		t.Error("denied hit must reject")
	}
	if !p.Governed("other") {
		t.Error("denied miss must allow")
	}

	// allowed fail-closed: 非 nil 且不含即拒; 空集拒一切。
	p.SetGovernance(nil, map[string]bool{"ok1": true})
	if !p.Governed("ok1") {
		t.Error("allowed member must pass")
	}
	if p.Governed("not-listed") {
		t.Error("allowed list excludes unknown (fail-closed)")
	}
	p.SetGovernance(nil, map[string]bool{})
	if p.Governed("ok1") {
		t.Error("empty allowed list must reject everything")
	}

	// 叠加: allowed 放行仍被 denied 压制。
	p.SetGovernance(map[string]bool{"x": true}, map[string]bool{"x": true, "y": true})
	if p.Governed("x") {
		t.Error("denied must win over allowed")
	}
	if !p.Governed("y") {
		t.Error("allowed y must pass")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func sameAPITools(a, b []types.APITool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

func names(ts []types.APITool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}
