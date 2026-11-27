package search

// fetch_json_test.go —— fetch 对 application/json 的代码块化测试。
//
// 背景 (2026-11-26 行研官遥测): agent 查版本会打 api.github.com 的 REST API,
// 响应 application/json 落在 default 分支被判 unsupported, 之后 agent 绕路
// 猜 HTML release 页, 猜错就是一次臆造 URL 404。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchRendersJSONAsCodeBlock 钉住 JSON 响应的代码块化: 可解析就重新
// 缩进, 包在 ```json 围栏里, 状态为成功而不是 unsupported。
func TestFetchRendersJSONAsCodeBlock(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprint(w, `{"tag_name":"v1.2.3","name":"Release 1.2.3","published_at":"2026-11-20T08:00:00Z"}`)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	doc, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/repos/x/y/releases/latest"})
	if err != nil {
		t.Fatalf("Fetch JSON: %v", err)
	}
	md := doc.Document.Markdown
	if !strings.HasPrefix(md, "```json\n") || !strings.HasSuffix(md, "\n```") {
		t.Errorf("JSON 应包在代码块里:\n%s", md)
	}
	if !strings.Contains(md, `"tag_name": "v1.2.3"`) {
		t.Errorf("JSON 应被重新缩进 (key 后带空格):\n%s", md)
	}
}

// TestFetchRendersJSONSuffixTypes 钉住 application/*+json (hal+json /
// vnd.github+json 等) 与裸 JSON 走同一分支。
func TestFetchRendersJSONSuffixTypes(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.github+json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	doc, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/vnd"})
	if err != nil {
		t.Fatalf("Fetch +json: %v", err)
	}
	if !strings.Contains(doc.Document.Markdown, `"ok": true`) {
		t.Errorf("+json 应代码块化:\n%s", doc.Document.Markdown)
	}
}

// TestFetchJSONTruncatesHugeBodies 钉住超大 JSON 的截断: 正文不超过上限,
// 截断注明出现在代码块外。截断允许切在 JSON 中间 (文档体不要求可解析)。
func TestFetchJSONTruncatesHugeBodies(t *testing.T) {
	allowAnyFetchURL(t)
	huge := `{"data":"` + strings.Repeat("x", maxJSONDocChars+1000) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, huge)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	// MaxChars 放大到能覆盖整个 JSON 文档, 否则默认 16000 摘录窗根本到不了
	// 截断注明所在的位置。
	doc, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/huge", MaxChars: maxJSONDocChars + 10000})
	if err != nil {
		t.Fatalf("Fetch huge JSON: %v", err)
	}
	md := doc.Document.Markdown
	if !strings.Contains(md, "[JSON 已截断") {
		t.Errorf("超大 JSON 应有截断注明")
	}
	if doc.Document.TotalChars > maxJSONDocChars+200 {
		t.Errorf("规范化全文长度 %d 超上限", doc.Document.TotalChars)
	}
}

// TestFetchJSONKeepsUnparseableBodies 钉住不可解析体 (JSONP/ndjson) 的兜底:
// 不报错, 原文进代码块。
func TestFetchJSONKeepsUnparseableBodies(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"a":1}`+"\n"+`{"b":2}`) // ndjson: 整体不是合法 JSON
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	doc, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/ndjson"})
	if err != nil {
		t.Fatalf("Fetch ndjson: %v", err)
	}
	if !strings.Contains(doc.Document.Markdown, `{"a":1}`) {
		t.Errorf("ndjson 原文应保留:\n%s", doc.Document.Markdown)
	}
}
