package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
)

// 本次修复的对象: 派生子 agent 时模型自己往 Agent 工具的 model 字段里填名字,
// 填出来的名字网关根本没有。修复前那种名字会被 resolveModel **静默接受**
// (splitAlias + GetProvider 只要 provider 存在就凑得齐字段), 一路透传上游换回 401。
//
// 这组测试钉住两件事, 缺一不可:
//  1. 没声明的名字不再被放行 —— 这是修复本身;
//  2. 声明过的名字一切照旧 —— 没有第 2 条的话, "全部拒绝" 也能让第 1 条变绿。

// testGateway 造一个形状与生产一致的网关: 四个 provider 全部声明了 models,
// 与 /mnt/data/claude-go/config/config.json 同构 (opencode 33 个模型里没有任何 claude)。
func testGateway(t *testing.T) *gateway {
	t.Helper()
	models := func(alias ...string) map[string]modelconfig.ModelConfig {
		m := make(map[string]modelconfig.ModelConfig, len(alias))
		for _, a := range alias {
			m[a] = modelconfig.ModelConfig{}
		}
		return m
	}
	cfg := modelconfig.ConfigJSON{
		Providers: map[string]modelconfig.ProviderConfig{
			"opencode": {
				Name:    "opencode",
				BaseURL: "https://opencode.ai/zen/go/v1",
				APIKey:  "sk-test-opencode",
				Models: models(
					"opencode:deepseek-v4.1-flash",
					"opencode:deepseek-flash",
					"opencode:deepseek-v4-pro",
				),
			},
			"kimi": {
				Name:    "kimi",
				BaseURL: "https://api.kimi.com/coding/v1",
				APIKey:  "sk-test-kimi",
				Models:  models("kimi:k3", "kimi:kimi-for-coding"),
			},
		},
	}
	cfg.AI.DefaultModelAlias = "opencode:deepseek-v4.1-flash"

	registry, resolver, err := modelconfig.LoadFromConfig(cfg)
	if err != nil {
		t.Fatalf("构建解析器失败: %v", err)
	}
	return &gateway{registry: registry, resolver: resolver, tryPrefix: "opencode"}
}

func reqWithSessionHdr(v string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://x/v1/messages", nil)
	if v != "" {
		r.Header.Set(claudeSessionHeader, v)
	}
	return r
}

// ── 1. 核心断言: 没声明的名字不得被放行 ──

func TestResolveModel_RejectsUndeclaredModel(t *testing.T) {
	g := testGateway(t)
	// 这三种名字是生产日志里实际出现过的 (resolved=claude-opus-4-8 / claude-sonnet-5),
	// 外加一个我用来验证的、确定不存在的名字。
	for _, name := range []string{
		"claude-opus-4-8",         // 裸名 → tryPrefix 拼成 opencode:claude-opus-4-8
		"claude-sonnet-5",         // 同上
		"opencode:claude-opus-4-8", // 带前缀的完整形态
		"does-not-exist-xyz",      // 生产日志里被放行过的那个
		"kimi:claude-opus-4-8",    // 换个 provider 也一样
	} {
		if _, err := g.resolveModel(name); err == nil {
			t.Errorf("未声明的模型名 %q 不该被接受 —— 这正是子 agent 401 的根因", name)
		}
	}
}

// 钉住 registeredOnly 存在的**理由**: 只看 ResolveAlias 的返回值齐不齐是判不出来的。
//
// 这条断言的是底层库的既有行为 (resolveAlias 查不到 ModelEntry 时退到
// splitAlias + GetProvider), 而不是网关自己的代码 —— 但它正是本次故障的机理,
// 所以必须钉住: 哪天 resolveAlias 改成"查不到就返回空", registeredOnly 依然正确,
// 而这条会失败并提醒可以简化; 反过来若有人把 registeredOnly 退回字段判据,
// 这条会解释为什么不行。
func TestResolveAlias_FieldsLookCompleteEvenForUndeclaredModel(t *testing.T) {
	g := testGateway(t)
	rc := g.resolver.ResolveAlias("opencode:claude-opus-4-8")
	if rc.BaseURL == "" || rc.APIKey == "" || rc.ProviderName == "" {
		t.Fatalf("前提已变: resolveAlias 现在对未声明的模型返回不完整配置 —— "+
			"registeredOnly 可以简化回字段判据了 (BaseURL=%q APIKey=%q ProviderName=%q)",
			rc.BaseURL, rc.APIKey, rc.ProviderName)
	}
	// 而注册表里其实根本没有这个模型 —— 字段齐 ≠ 模型存在, 两者必须分开判。
	if g.registry.GetModel("opencode:claude-opus-4-8") != nil {
		t.Fatal("前提已变: 注册表里居然有了这个模型")
	}
}

// ── 2. 正例: 声明过的名字照旧解析 (没有这条, "全拒" 也能让上面变绿) ──

func TestResolveModel_StillAcceptsDeclaredModels(t *testing.T) {
	g := testGateway(t)

	// 完整别名: 网关日志里 15861 次的那个主力模型
	rc, err := g.resolveModel("opencode:deepseek-flash")
	if err != nil {
		t.Fatalf("已声明的完整别名应解析成功: %v", err)
	}
	if rc.Alias != "opencode:deepseek-flash" || rc.ProviderName != "deepseek-flash" {
		t.Errorf("别名解析结果不对: Alias=%q ProviderName=%q", rc.Alias, rc.ProviderName)
	}

	// 裸名: claude-go 客户端走 ProviderName 路径时只发后半段
	if _, err := g.resolveModel("deepseek-flash"); err != nil {
		t.Errorf("已声明模型的裸名应解析成功: %v", err)
	}

	// 全表反查: 非默认前缀 provider 的模型, 靠 LookupAliasByModelName 命中
	rc3, err := g.resolveModel("k3")
	if err != nil {
		t.Fatalf("kimi 的裸名 k3 应反查到 kimi:k3: %v", err)
	}
	if rc3.Alias != "kimi:k3" {
		t.Errorf("反查应得到 kimi:k3, got %q", rc3.Alias)
	}
}

// ── 3. 会话记忆: 子 agent 继承父会话的模型 ──

func TestFallbackForModel_PrefersSessionLastGood(t *testing.T) {
	g := testGateway(t)
	const sess = "6cf701da-8441-45ad-8758-56063115f142"

	// 父会话正常跑了一轮, 用 kimi:k3 (刻意不选全局默认, 否则这条测试无法区分
	// "命中了会话记忆" 与 "退到了全局默认" —— 两者会给出同一个答案)。
	g.rememberGood(sess, "kimi:k3")

	sub, gotSess, ok := g.fallbackForModel(reqWithSessionHdr(sess))
	if !ok {
		t.Fatal("有会话记忆时应能兜底")
	}
	if sub.Alias != "kimi:k3" {
		t.Errorf("应继承本会话上次用过的模型, got %q (若得到 opencode:deepseek-v4.1-flash 说明退到了全局默认)", sub.Alias)
	}
	if gotSess != sess {
		t.Errorf("会话键应原样回传, got %q", gotSess)
	}
}

func TestFallbackForModel_ColdSessionUsesGlobalDefault(t *testing.T) {
	g := testGateway(t)
	// 新会话的第一个请求就是子 agent: 没有任何记忆可用。
	sub, _, ok := g.fallbackForModel(reqWithSessionHdr("brand-new-session"))
	if !ok {
		t.Fatal("冷会话应退到全局默认")
	}
	if sub.Alias != "opencode:deepseek-v4.1-flash" {
		t.Errorf("冷会话应退到全局默认 opencode:deepseek-v4.1-flash, got %q", sub.Alias)
	}
}

// 拿不到会话身份 (CLI 没带这个头) 时仍要能兜底, 不能 500 也不能直接拒绝。
func TestFallbackForModel_NoSessionHeader(t *testing.T) {
	g := testGateway(t)
	sub, sess, ok := g.fallbackForModel(reqWithSessionHdr(""))
	if !ok {
		t.Fatal("无会话头时应退到全局默认, 而不是放弃")
	}
	if sess != "" {
		t.Errorf("无会话头时应回传空会话键, got %q", sess)
	}
	if sub.Alias != "opencode:deepseek-v4.1-flash" {
		t.Errorf("got %q", sub.Alias)
	}
}

// 会话记忆里存的名字如果已经不可用 (比如热加载把该模型删了), 必须退到全局默认,
// 而不是把一个拿不到凭据的名字当成有效兜底返回出去。
func TestFallbackForModel_StaleMemoryFallsBack(t *testing.T) {
	g := testGateway(t)
	const sess = "stale-session"
	g.rememberGood(sess, "kimi:k3")

	// 模拟热加载: 整个 provider 没了。
	g.registry.Reload([]modelconfig.ProviderConfig{{
		Name:    "opencode",
		BaseURL: "https://opencode.ai/zen/go/v1",
		APIKey:  "sk-test-opencode",
		Models:  map[string]modelconfig.ModelConfig{"opencode:deepseek-v4.1-flash": {}},
	}})

	sub, _, ok := g.fallbackForModel(reqWithSessionHdr(sess))
	if !ok {
		t.Fatal("陈旧记忆应退到全局默认, 而不是放弃兜底")
	}
	if sub.Alias != "opencode:deepseek-v4.1-flash" {
		t.Errorf("陈旧记忆应被丢弃并退到全局默认, got %q", sub.Alias)
	}
}

// ── 4. 记忆表本身: 不能无界增长 ──

func TestRememberGood_IsBounded(t *testing.T) {
	g := testGateway(t)
	for i := 0; i < lastGoodMax+200; i++ {
		g.rememberGood(sessionKeyOf(i), "opencode:deepseek-flash")
	}
	g.lgMu.Lock()
	n := len(g.lastGood)
	g.lgMu.Unlock()
	if n > lastGoodMax {
		t.Errorf("记忆表应被限制在 %d 以内, 实际 %d —— 不设界就是一条稳定的内存泄漏", lastGoodMax, n)
	}
	// 且裁剪之后仍然可用: 最新写入的那条必须还在。
	if _, _, ok := g.fallbackForModel(reqWithSessionHdr(sessionKeyOf(lastGoodMax+199))); !ok {
		t.Error("裁剪不该把刚写进去的会话一起删掉")
	}
}

// 过期条目要被清掉, 否则 TTL 就只是个摆设。
func TestRememberGood_ExpiresOldEntries(t *testing.T) {
	g := testGateway(t)
	g.lgMu.Lock()
	g.lastGood = map[string]lastGoodEntry{
		"ancient": {alias: "kimi:k3", at: time.Now().Add(-2 * lastGoodTTL)},
	}
	g.lgMu.Unlock()

	g.rememberGood("fresh", "kimi:k3")

	g.lgMu.Lock()
	_, stillThere := g.lastGood["ancient"]
	n := len(g.lastGood)
	g.lgMu.Unlock()
	if stillThere {
		t.Error("超过 TTL 的条目应在写入时被清掉")
	}
	if n != 1 {
		t.Errorf("清理后应只剩新写的那条, got %d", n)
	}
}

func sessionKeyOf(i int) string {
	return "sess-" + strconv.Itoa(i)
}

// ── 5. 访问器: 防读错字段名 ──
//
// config.json 里这一项叫 ai.modelAlias, 而 GlobalConfig 里的字段叫
// DefaultModelAlias —— 名字不一样, 靠 feishu.ToModelConfigJSON 翻译。
// 读错字段的话兜底会静默失效 (退化成"没有兜底"), 所以这里直接钉住取值。
func TestDefaultAlias_ReadsGlobalDefault(t *testing.T) {
	g := testGateway(t)
	if got := g.resolver.DefaultAlias(); got != "opencode:deepseek-v4.1-flash" {
		t.Errorf("DefaultAlias 应返回 GlobalConfig.DefaultModelAlias, got %q", got)
	}
}
