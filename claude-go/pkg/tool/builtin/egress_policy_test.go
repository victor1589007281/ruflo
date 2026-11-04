// egress_policy_test.go — H-F2 的 WebFetch 域名策略与不可信标记验收。
//
// 全部用 fake fetcher（契约测试里的 contractFetcher），不访问真实外网。
package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func fetchWithPolicy(t *testing.T, p DomainPolicy, rawURL string) (*WebFetchTool, *contractFetcher, *toolResultView) {
	t.Helper()
	f := &contractFetcher{}
	tool := &WebFetchTool{fetcher: f}
	tool.SetDomainPolicy(p)
	got, err := tool.Call(context.Background(), json.RawMessage(`{"url":"`+rawURL+`"}`), nil)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	return tool, f, &toolResultView{content: got.Content, isErr: got.IsError, trust: got.Trust, source: got.SourceURI}
}

type toolResultView struct {
	content string
	isErr   bool
	trust   string
	source  string
}

func TestWebFetchPolicy_AllowedSubdomainFetchesAndTagsUntrusted(t *testing.T) {
	_, f, got := fetchWithPolicy(t, DomainPolicy{Allowed: []string{"example.com"}}, "https://api.example.com/article")
	if got.isErr {
		t.Fatalf("allowed 子域不应被拒绝: %+v", got)
	}
	if f.req.URL != "https://api.example.com/article" {
		t.Fatalf("allowed 域应真的抓取, req=%+v", f.req)
	}
	if got.trust != "untrusted" {
		t.Errorf("WebFetch 成功结果必须标记 untrusted, 实得 %q", got.trust)
	}
	if got.source != "https://api.example.com/article" {
		t.Errorf("SourceURI 应为请求 URL, 实得 %q", got.source)
	}
}

func TestWebFetchPolicy_DeniedHostFailsWithoutFetching(t *testing.T) {
	_, f, got := fetchWithPolicy(t, DomainPolicy{Denied: []string{"evil.com"}}, "https://sub.evil.com/x")
	if !got.isErr {
		t.Fatalf("denied 域必须返回错误: %+v", got)
	}
	if !strings.Contains(got.content, "denied") {
		t.Errorf("错误文案必须明确说明被域名策略拒绝: %q", got.content)
	}
	if f.req.URL != "" {
		t.Errorf("denied 域不得发起抓取, req=%+v", f.req)
	}
}

func TestWebFetchPolicy_DenyWinsOverAllow(t *testing.T) {
	_, _, got := fetchWithPolicy(t, DomainPolicy{
		Allowed: []string{"example.com"},
		Denied:  []string{"blocked.example.com"},
	}, "https://blocked.example.com/x")
	if !got.isErr || !strings.Contains(got.content, "denied") {
		t.Fatalf("deny 必须优先于 allow: %+v", got)
	}
}

func TestWebFetchPolicy_UnknownRequiresConfirmation(t *testing.T) {
	tool := &WebFetchTool{}
	tool.SetDomainPolicy(DomainPolicy{
		Allowed:                  []string{"example.com"},
		UnknownNeedsConfirmation: true,
	})

	cases := []struct {
		url  string
		want bool
		why  string
	}{
		{`https://example.com/x`, false, "allowed 域无需确认"},
		{`https://api.example.com/x`, false, "allowed 子域无需确认"},
		{`https://unknown.test/x`, true, "未知域需要确认"},
		{`not a url`, false, "非法 URL 交给 Call 报错, 不先弹确认"},
		{``, false, "空 URL 不要求确认"},
	}
	for _, tc := range cases {
		raw, _ := json.Marshal(map[string]string{"url": tc.url})
		if got := tool.NeedsConfirmationFor(raw); got != tc.want {
			t.Errorf("%s: NeedsConfirmationFor(%q)=%v, want %v", tc.why, tc.url, got, tc.want)
		}
	}

	// deny 域由 Call 直接拒绝，不需要确认（确认也放不过去）。
	tool.SetDomainPolicy(DomainPolicy{Denied: []string{"evil.com"}, UnknownNeedsConfirmation: true})
	if tool.NeedsConfirmationFor(json.RawMessage(`{"url":"https://evil.com/x"}`)) {
		t.Error("denied 域不应要求确认 (应直接拒绝)")
	}
}

func TestWebFetchPolicy_ZeroValueKeepsLegacyBehavior(t *testing.T) {
	tool := &WebFetchTool{}
	if tool.NeedsConfirmationFor(json.RawMessage(`{"url":"https://anything.test/x"}`)) {
		t.Fatal("零值策略必须不要求确认 (旧行为)")
	}
	if d := (DomainPolicy{}).Decide("anything.test"); d != DomainUnknown {
		t.Fatalf("零值策略的裁决应为 Unknown(放行且不确认), 实得 %v", d)
	}
}

func TestDomainPolicy_HostMatchingIsSubdomainNotSuffix(t *testing.T) {
	p := DomainPolicy{Denied: []string{"example.com"}}
	cases := map[string]DomainDecision{
		"example.com":       DomainDenied,
		"a.example.com":     DomainDenied,
		"notexample.com":    DomainUnknown, // 经典后缀越权写法
		"example.com.evil":  DomainUnknown,
		"EXAMPLE.COM":       DomainDenied, // 大小写不敏感
		"example.com.":      DomainDenied, // FQDN 末尾点
		"a.example.com:443": DomainDenied, // 端口不参与匹配
	}
	for host, want := range cases {
		if got := p.Decide(host); got != want {
			t.Errorf("Decide(%q)=%v, want %v", host, got, want)
		}
	}

	// 名单项可以手写 URL / 端口 / *. 前缀。
	p = DomainPolicy{Allowed: []string{"https://Example.COM:443/path", "*.other.test"}}
	if p.Decide("example.com") != DomainAllowed {
		t.Error("名单项应支持完整 URL 与端口")
	}
	if p.Decide("x.other.test") != DomainAllowed {
		t.Error("名单项应支持 *. 前缀")
	}
}

// TestWebSearch_ResultsAreUntrusted H-F2: 搜索结果同样不可信，并带来源标记。
func TestWebSearch_ResultsAreUntrusted(t *testing.T) {
	tool := NewWebSearchTool(fixedSearcher{})
	got, err := tool.Call(context.Background(), json.RawMessage(`{"query":"golang generics"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.IsError {
		t.Fatalf("搜索不应失败: %+v", got)
	}
	if got.Trust != "untrusted" {
		t.Errorf("搜索结果必须标记 untrusted, 实得 %q", got.Trust)
	}
	if got.SourceURI != "search:golang generics" {
		t.Errorf("SourceURI 应为 search:<query>, 实得 %q", got.SourceURI)
	}
	if !strings.Contains(strings.ToLower(tool.Description()), "untrusted data") {
		t.Error("工具描述必须明确返回内容是不可信数据")
	}
}

type fixedSearcher struct{}

func (fixedSearcher) Search(context.Context, string) (title, text, html string, err error) {
	return "Golang", "generics result text", "", nil
}
