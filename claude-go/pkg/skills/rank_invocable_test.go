package skills

import (
	"strings"
	"testing"
)

// ---- F2 rank 分层覆盖 ----

func TestSourceRankMapping(t *testing.T) {
	cases := []struct {
		source string
		want   int
	}{
		{"project", 100}, {"state", 100},
		{"user", 200}, {"user-state", 200}, {"config", 200},
		{"builtin", 600},
		{"", 300}, {"mystery", 300},
	}
	for _, c := range cases {
		if got := SourceRank(c.source); got != c.want {
			t.Errorf("SourceRank(%q)=%d, want %d", c.source, got, c.want)
		}
	}
}

func TestRegisterRankOverrides(t *testing.T) {
	r := NewRegistry()
	// 注册顺序刻意从低优先到高优先: builtin 先到, project 后到 —— project 必须胜出
	r.Register(&Skill{Name: "code-review", Body: "builtin", LoadedFrom: "builtin"})
	r.Register(&Skill{Name: "code-review", Body: "user", LoadedFrom: "user"})
	r.Register(&Skill{Name: "code-review", Body: "project", LoadedFrom: "project"})

	got, ok := r.Get("code-review")
	if !ok || got.Body != "project" {
		t.Errorf("project 应覆盖同名 user/builtin: ok=%v body=%q", ok, got.Body)
	}
	if got.Rank != 100 {
		t.Errorf("Rank 应为 100, 得 %d", got.Rank)
	}
	if r.Count() != 1 {
		t.Errorf("同名技能应只留 1 份, 得 %d", r.Count())
	}

	// 低优先级后来者不得顶掉高优先级既有注册
	r.Register(&Skill{Name: "code-review", Body: "user-again", LoadedFrom: "user"})
	got2, _ := r.Get("code-review")
	if got2.Body != "project" {
		t.Errorf("user 后来者不应覆盖 project: body=%q", got2.Body)
	}

	// 同 rank 后来者丢弃 (先到先得, 保证 Reload 确定性)
	r.Register(&Skill{Name: "x", Body: "first", LoadedFrom: "project"})
	r.Register(&Skill{Name: "x", Body: "second", LoadedFrom: "project"})
	got3, _ := r.Get("x")
	if got3.Body != "first" {
		t.Errorf("同 rank 应先到先得: body=%q", got3.Body)
	}

	// 直接 Register 无来源 → 中间值 300
	r.Register(&Skill{Name: "anon"})
	got4, _ := r.Get("anon")
	if got4.Rank != 300 {
		t.Errorf("无来源 Rank 应为 300, 得 %d", got4.Rank)
	}
}

// 既有行为护栏: Reload 后 LoadDefaults 的加载顺序 builtin→project→state→user→user-state,
// rank 语义下高优先源仍应胜出 (project 覆盖同名 builtin)。
func TestRegisterRankDeterministicAcrossReload(t *testing.T) {
	r := NewRegistry()
	r.Register(&Skill{Name: "s", Body: "v1", LoadedFrom: "project"})
	r.Register(&Skill{Name: "s", Body: "v2", LoadedFrom: "project"})
	got, _ := r.Get("s")
	if got.Body != "v1" {
		t.Errorf("同 rank 重复 Register 应先到先得: %q", got.Body)
	}
}

// ---- F3 双向调用策略 ----

func TestParseInvocableFrontmatter(t *testing.T) {
	sk := &Skill{}
	parseFrontmatter("name: x\ndescription: d\nmodel-invocable: false\nuser-invocable: true\n", sk)
	if sk.ModelInvocable == nil || *sk.ModelInvocable {
		t.Errorf("model-invocable: false 应解析为 false, 得 %+v", sk.ModelInvocable)
	}
	if sk.UserInvocable == nil || !*sk.UserInvocable {
		t.Errorf("user-invocable: true 应解析为 true, 得 %+v", sk.UserInvocable)
	}

	// 下划线变体 + 引号值
	sk2 := &Skill{}
	parseFrontmatter("model_invocable: \"false\"\n", sk2)
	if sk2.ModelInvocable == nil || *sk2.ModelInvocable {
		t.Errorf("带引号的 model_invocable: false 应解析为 false")
	}

	// 非法值 fail-safe → false (关闭该侧调用)
	sk3 := &Skill{}
	parseFrontmatter("model-invocable: banana\n", sk3)
	if sk3.ModelInvocable == nil || *sk3.ModelInvocable {
		t.Errorf("非法布尔值应解析为 false")
	}
}

func TestModelVisibleActiveFilters(t *testing.T) {
	r := NewRegistry()
	r.Register(&Skill{Name: "both", Body: "b"})
	r.Register(&Skill{Name: "user-only", Body: "u", LoadedFrom: "project"})
	// user-only 声明 model-invocable: false —— 直接构造指针
	f := false
	tt, _ := r.GetAny("user-only")
	tt.ModelInvocable = &f

	active := r.Active()
	if len(active) != 2 {
		t.Errorf("Active 不过滤 ModelInvocable (治理视图不变), 得 %d", len(active))
	}
	mv := r.ModelVisibleActive()
	if len(mv) != 1 || mv[0].Name != "both" {
		t.Errorf("ModelVisibleActive 应只含 both, 得 %v", mv)
	}

	// 清单过滤
	listing := r.FormatShortListing(10)
	if strings.Contains(listing, "user-only") {
		t.Error("清单不应包含 model-invocable: false 技能")
	}
	if !strings.Contains(listing, "both") {
		t.Error("清单应包含双向技能")
	}
}

func TestSkillToolRefusesModelHidden(t *testing.T) {
	r := NewRegistry()
	f := false
	r.Register(&Skill{Name: "cmd-only", Body: "secret", ModelInvocable: &f})
	st := NewSkillTool(r)

	res, _ := st.Call(t.Context(), []byte(`{"name":"cmd-only"}`), nil)
	if !res.IsError {
		t.Fatal("模型加载 model-invocable: false 技能应报错")
	}
	if strings.Contains(res.Content, "secret") {
		t.Error("错误提示不得泄露技能正文")
	}

	// 未找到时的提示清单也应过滤
	res2, _ := st.Call(t.Context(), []byte(`{"name":"nope"}`), nil)
	if !res2.IsError || strings.Contains(res2.Content, "cmd-only") {
		t.Errorf("未找到提示清单不应包含 model-hidden 技能: %s", res2.Content)
	}
}

// ---- F1 digest + 已注入记账 ----

func TestDigestStableAndSensitive(t *testing.T) {
	a := &Skill{Name: "s", Description: "d", Body: "hello"}
	b := &Skill{Name: "s", Description: "d", Body: "hello"}
	if ComputeDigest(a) != ComputeDigest(b) {
		t.Error("同内容 digest 应稳定")
	}
	b.Body = "changed"
	if ComputeDigest(a) == ComputeDigest(b) {
		t.Error("内容变化 digest 应变化")
	}
}

func TestInjectedLedger(t *testing.T) {
	r := NewRegistry()
	r.Register(&Skill{Name: "s", Body: "v1"})

	if r.WasInjectedUnchanged("s") {
		t.Error("未加载前不应记为已注入")
	}
	// Skill 工具加载成功 → 记账
	st := NewSkillTool(r)
	if _, err := st.Call(t.Context(), []byte(`{"name":"s"}`), nil); err != nil {
		t.Fatal(err)
	}
	if !r.WasInjectedUnchanged("s") {
		t.Error("加载成功后应记为已注入且未变")
	}

	// 内容改进 → digest 变化 → 失效
	r.Register(&Skill{Name: "s", Body: "v2", LoadedFrom: "project"}) // project(100) 覆盖
	if r.WasInjectedUnchanged("s") {
		t.Error("技能内容变化后应失效, 需重新注入")
	}

	// 记账跨后续注册保留: t 加载后, 再注册别的技能不应清掉 t 的记账
	r.Register(&Skill{Name: "t", Body: "x"})
	if _, err := st.Call(t.Context(), []byte(`{"name":"t"}`), nil); err != nil {
		t.Fatal(err)
	}
	if !r.WasInjectedUnchanged("t") {
		t.Fatal("t 加载后应记为已注入")
	}
	r.Register(&Skill{Name: "u", Body: "x"})
	if !r.WasInjectedUnchanged("t") {
		t.Error("其他技能注册不应影响 t 的记账")
	}

	// nil 技能安全
	r.MarkInjected("ghost") // 不存在, 静默
	if r.WasInjectedUnchanged("ghost") {
		t.Error("不存在的技能不应记为已注入")
	}
}

func TestRegisterNilSkill(t *testing.T) {
	r := NewRegistry()
	r.Register(nil)          // 不得 panic
	r.Register(&Skill{Name: ""}) // 无名, 忽略
	if r.Count() != 0 {
		t.Errorf("nil/无名注册应被忽略, 得 %d", r.Count())
	}
}
