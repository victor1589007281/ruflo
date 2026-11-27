package builtin

// seenurls_test.go —— 会话级 seen-URL 登记簿与 WebFetch 臆造提示的测试。
//
// 背景 (2026-11-26 行研官遥测): agent 凭训练记忆拼 release URL
// (vllm v0.35.0 / sglang v1.0.0 ...), 404 占 fetch 错误的三分之一。
// 提示是软约束: 404 + 没见过 → 附提示; 见过 / 非 404 / 无会话 → 原样。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/search"
	"github.com/anthropic/claude-go/pkg/tool"
)

func TestSeenURLsNormalizeAndLookup(t *testing.T) {
	sid := "s-norm"
	recordSeenURLs(sid, []string{
		"https://www.Example.com/Path/A/",
		"https://github.com/vllm-project/vllm/releases/tag/v0.34.0#frag",
	})
	// 大小写/www./末尾斜杠/fragment 差异不影响命中。
	for _, u := range []string{
		"https://example.com/Path/A",
		"https://example.com/Path/A/",
		"https://github.com/vllm-project/vllm/releases/tag/v0.34.0",
	} {
		if !seenURL(sid, u) {
			t.Errorf("应命中: %s", u)
		}
	}
	// 路径不同 (臆造版本号) 不命中。
	if seenURL(sid, "https://github.com/vllm-project/vllm/releases/tag/v0.35.0") {
		t.Error("不同 tag 不应命中")
	}
	// 空会话 = 无登记簿, 一律视为见过 (不提示)。
	if !seenURL("", "https://never-seen.example.com/") {
		t.Error("空 sessionID 应恒为见过")
	}
}

func TestSeenURLsFromTextExtractsLinks(t *testing.T) {
	sid := "s-text"
	recordSeenURLsFromText(sid, `{"results":[{"url":"https://arxiv.org/abs/2611.33098","title":"x"}],"gate_passed":true}`)
	if !seenURL(sid, "https://arxiv.org/abs/2611.33098") {
		t.Error("JSON 输出里的 URL 应被登记")
	}
	if seenURL(sid, "https://arxiv.org/abs/2611.99999") {
		t.Error("未出现的 URL 不应命中")
	}
}

// hintFetcher 返回可编程错误的抓取桩。
type hintFetcher struct{ err error }

func (f *hintFetcher) FetchJSON(context.Context, string, int) (string, error) {
	return "", f.err
}
func (f *hintFetcher) FetchRequestJSON(context.Context, search.FetchRequest) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return `{"url":"https://ok.example.com","markdown":"body links https://linked.example.com/a"}`, nil
}

func fetchCall(t *testing.T, wt *WebFetchTool, sid, url string) *tool2Result {
	t.Helper()
	res, err := wt.Call(context.Background(), json.RawMessage(fmt.Sprintf(`{"url":%q}`, url)),
		&tool.ToolContext{SessionID: sid})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return &tool2Result{Content: res.Content, IsError: res.IsError}
}

type tool2Result struct {
	Content string
	IsError bool
}

func TestWebFetchNotFoundHintForUnseenURL(t *testing.T) {
	sid := "s-hint"
	notFound := search.Wrap("fetch", search.ErrNotFound, fmt.Errorf("http status 404"))
	tl := &WebFetchTool{fetcher: &hintFetcher{err: notFound}}

	// 臂 1: 没见过 + 404 → 附提示。
	got := fetchCall(t, tl, sid, "https://github.com/vllm-project/vllm/releases/tag/v0.99.0")
	if !got.IsError || !strings.Contains(got.Content, "未出现在本会话") {
		t.Errorf("臆造 URL 的 404 应附提示: %s", got.Content)
	}

	// 臂 2: 见过 (来自搜索结果) + 404 → 原样, 不附提示。
	recordSeenURLs(sid, []string{"https://github.com/vllm-project/vllm/releases/tag/v0.34.0"})
	got = fetchCall(t, tl, sid, "https://github.com/vllm-project/vllm/releases/tag/v0.34.0")
	if !got.IsError || strings.Contains(got.Content, "未出现在本会话") {
		t.Errorf("搜索见过的 URL 不应附提示: %s", got.Content)
	}

	// 臂 3: 非 404 (403) 即使没见过也不附提示。
	blocked := search.Wrap("fetch", search.ErrBlocked, fmt.Errorf("http status 403"))
	tl2 := &WebFetchTool{fetcher: &hintFetcher{err: blocked}}
	got = fetchCall(t, tl2, sid, "https://perplexity.ai/hub/blog/x")
	if !got.IsError || strings.Contains(got.Content, "未出现在本会话") {
		t.Errorf("403 不应附臆造提示: %s", got.Content)
	}

	// 臂 4: 无会话上下文 → 不附提示 (保持旧行为)。
	res, err := tl.Call(context.Background(), json.RawMessage(`{"url":"https://x.example.com/a"}`), nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(res.Content, "未出现在本会话") {
		t.Errorf("无会话不应附提示: %s", res.Content)
	}
}

func TestWebFetchSuccessRecordsPageLinks(t *testing.T) {
	sid := "s-links"
	tl := &WebFetchTool{fetcher: &hintFetcher{}}
	got := fetchCall(t, tl, sid, "https://ok.example.com/feed")
	if got.IsError {
		t.Fatalf("fetch: %s", got.Content)
	}
	// 成功页正文里的链接进入登记簿, 之后 404 不再提示。
	if !seenURL(sid, "https://linked.example.com/a") {
		t.Error("成功页里的链接应被登记")
	}
}
