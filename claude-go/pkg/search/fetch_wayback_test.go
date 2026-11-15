package search

// fetch_wayback_test.go —— Wayback 归档兜底的分类、准入与端到端链路测试。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// withWaybackSnapshot 把归档快照地址覆写为指定端点, 测试结束还原。
func withWaybackSnapshot(t *testing.T, raw string) {
	t.Helper()
	old := waybackSnapshotURL
	target, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse snapshot url: %v", err)
	}
	waybackSnapshotURL = func(u *url.URL) (*url.URL, error) {
		snap := *target
		return &snap, nil
	}
	t.Cleanup(func() { waybackSnapshotURL = old })
}

// TestStatusKindNotFound 钉住 404/410 独立成 ErrNotFound: 与请求非法的
// ErrInvalid (SSRF/协议) 分开, 前者回退归档、两者都不配旧缓存伪装。
func TestStatusKindNotFound(t *testing.T) {
	cases := []struct {
		code int
		want ErrorKind
	}{
		{http.StatusNotFound, ErrNotFound},
		{http.StatusGone, ErrNotFound},
		{http.StatusBadRequest, ErrInvalid},
		{http.StatusForbidden, ErrBlocked},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusInternalServerError, ErrUnavailable},
	}
	for _, c := range cases {
		if got := statusKind(c.code); got != c.want {
			t.Errorf("statusKind(%d) = %s, want %s", c.code, got, c.want)
		}
	}
}

// TestWaybackRetryableKinds 钉住哪些原站失败值得回退归档: 死链 (404/410)、
// 5xx、超时、拒爬 (403/429) 都回退; 请求非法 (SSRF/协议) 与内容类型不
// 支持不回退。
func TestWaybackRetryableKinds(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"invalid (SSRF)", Wrap("fetch", ErrInvalid, fmt.Errorf("guard")), false},
		{"unsupported (content-type)", Wrap("fetch", ErrUnsupported, fmt.Errorf("pdf")), false},
		{"not_found (404)", Wrap("fetch", ErrNotFound, fmt.Errorf("http status 404")), true},
		{"blocked (403)", Wrap("fetch", ErrBlocked, fmt.Errorf("http status 403")), true},
		{"rate_limited (429)", Wrap("fetch", ErrRateLimited, fmt.Errorf("http status 429")), true},
		{"timeout", Wrap("fetch", ErrTimeout, fmt.Errorf("deadline")), true},
		{"unavailable (5xx)", Wrap("fetch", ErrUnavailable, fmt.Errorf("http status 500")), true},
	}
	for _, c := range cases {
		if got := waybackRetryable(c.err); got != c.want {
			t.Errorf("%s: waybackRetryable = %v, want %v", c.name, got, c.want)
		}
	}
}

// waybackFixture 搭一对服务器: 原站 (对 /dead 返回 404) 与归档快照站。
// 返回原站 URL、归档命中计数与清理函数。
func waybackFixture(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var archiveHits atomic.Int32
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveHits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Archived</title></head><body><article><p>归档里的既往公开内容。</p></article></body></html>`)
	}))
	t.Cleanup(archive.Close)
	withWaybackSnapshot(t, archive.URL+"/web/2id_/")

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(origin.Close)
	return origin.URL + "/dead", &archiveHits
}

// TestFetchFallsBackToWaybackOnDeadPage 钉住端到端兜底链: 原站 404
// (ErrNotFound) → 归档快照接管 → 文档归回原 URL 并带 wayback 标记,
// Degraded 里落账 wayback_fallback:not_found。
func TestFetchFallsBackToWaybackOnDeadPage(t *testing.T) {
	allowAnyFetchURL(t)
	originURL, archiveHits := waybackFixture(t)

	cfg := DefaultConfig()
	cfg.WaybackFallback = true
	m := NewManager(cfg, testEgress(t), nil)
	defer m.Close()

	resp, err := m.Fetch(context.Background(), FetchRequest{URL: originURL})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	doc := resp.Document
	if doc.Via != "wayback" {
		t.Errorf("via = %q, want wayback", doc.Via)
	}
	if doc.URL != originURL {
		t.Errorf("doc.URL = %q, want 归回原 URL %q", doc.URL, originURL)
	}
	if !strings.Contains(doc.Markdown, "归档里的既往公开内容") {
		t.Errorf("markdown = %q", doc.Markdown)
	}
	var found bool
	for _, d := range resp.Degraded {
		if strings.HasPrefix(d, "wayback_fallback:") {
			found = true
		}
	}
	if !found {
		t.Errorf("Degraded = %v, want wayback_fallback:*", resp.Degraded)
	}
	if archiveHits.Load() != 1 {
		t.Errorf("archive hits = %d, want 1", archiveHits.Load())
	}
}

// TestFetchSkipsWaybackOnInvalid 钉住请求非法 (400 → ErrInvalid) 不回退
// 归档: 换出口/换归档端点都没有意义, 更不能让归档充当 SSRF 代理。
func TestFetchSkipsWaybackOnInvalid(t *testing.T) {
	allowAnyFetchURL(t)
	var archiveHits atomic.Int32
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveHits.Add(1)
	}))
	defer archive.Close()
	withWaybackSnapshot(t, archive.URL+"/web/2id_/")

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer origin.Close()

	cfg := DefaultConfig()
	cfg.WaybackFallback = true
	m := NewManager(cfg, testEgress(t), nil)
	defer m.Close()

	_, err := m.Fetch(context.Background(), FetchRequest{URL: origin.URL + "/x"})
	if KindOf(err) != ErrInvalid {
		t.Fatalf("kind = %s, want invalid (%v)", KindOf(err), err)
	}
	if archiveHits.Load() != 0 {
		t.Errorf("archive must not be consulted for invalid input, hits = %d", archiveHits.Load())
	}
}

// TestFetchSkipsWaybackWhenDisabled 钉住开关默认关闭: 不开
// SEARCH_WAYBACK_FALLBACK 时死链就是死链, 不偷偷连归档。
func TestFetchSkipsWaybackWhenDisabled(t *testing.T) {
	allowAnyFetchURL(t)
	originURL, archiveHits := waybackFixture(t)

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	if _, err := m.Fetch(context.Background(), FetchRequest{URL: originURL}); err == nil {
		t.Fatal("dead page without fallback must error")
	}
	if archiveHits.Load() != 0 {
		t.Errorf("archive must not be consulted when disabled, hits = %d", archiveHits.Load())
	}
}
