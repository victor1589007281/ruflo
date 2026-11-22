package search

// fetch_feed_test.go —— fetch 对 Atom/RSS feed 的抽取测试。
//
// 事故背景 (2026-11-19): arXiv listing 等学术 feed 返回
// application/atom+xml, 落在 default 分支被判 unsupported content type,
// 学术链路上 4 次抓取直接失败。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchParsesAtomFeed 钉住 Atom (arXiv 风格) 的结构化抽取:
// 标题/条目链接/日期/摘要(HTML 片段转纯文本)。
func TestFetchParsesAtomFeed(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>cs.DB updates</title>
  <entry>
    <title>Pome: Parallelizing I/Os for LSM-tree Storage</title>
    <link href="https://arxiv.org/abs/2307.16693" rel="alternate"/>
    <published>2026-06-21T00:00:00Z</published>
    <summary>We present &lt;b&gt;Pome&lt;/b&gt;, a parallel LSM approach. &lt;a href="https://arxiv.org"&gt;arXiv&lt;/a&gt;</summary>
  </entry>
  <entry>
    <title>MetaDB: File-System Structure in Distributed Databases</title>
    <link href="https://arxiv.org/abs/2610.32247" rel="alternate"/>
    <updated>2026-10-30T12:00:00Z</updated>
  </entry>
</feed>`)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	doc, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/list/cs.DB/recent"})
	if err != nil {
		t.Fatalf("Fetch atom feed: %v", err)
	}
	if doc.Document.Title != "cs.DB updates" {
		t.Errorf("title = %q", doc.Document.Title)
	}
	if !strings.Contains(doc.Document.Markdown, "[Pome: Parallelizing I/Os for LSM-tree Storage](https://arxiv.org/abs/2307.16693)") {
		t.Errorf("markdown 缺第 1 条链接标题:\n%s", doc.Document.Markdown)
	}
	// 摘要里的 HTML 标签必须剥掉。
	if strings.Contains(doc.Document.Markdown, "<b>") || strings.Contains(doc.Document.Markdown, "href=") {
		t.Errorf("markdown 残留 HTML:\n%s", doc.Document.Markdown)
	}
	if !strings.Contains(doc.Document.Markdown, "Pome, a parallel LSM approach") {
		t.Errorf("markdown 缺摘要正文:\n%s", doc.Document.Markdown)
	}
	if !strings.Contains(doc.Document.Markdown, "— 2026-06-21") {
		t.Errorf("markdown 缺第 1 条日期:\n%s", doc.Document.Markdown)
	}
	// 第 2 条只有 updated, 也要出链接; 日期取 updated。
	if !strings.Contains(doc.Document.Markdown, "[MetaDB: File-System Structure in Distributed Databases](https://arxiv.org/abs/2610.32247)") {
		t.Errorf("markdown 缺第 2 条:\n%s", doc.Document.Markdown)
	}
	if !strings.Contains(doc.Document.Markdown, "— 2026-10-30") {
		t.Errorf("markdown 缺第 2 条日期 (updated):\n%s", doc.Document.Markdown)
	}
}

// TestFetchParsesRSSFeed 钉住 RSS 2.0 变体 (channel/item, link 是文本节点)。
func TestFetchParsesRSSFeed(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		fmt.Fprint(w, `<?xml version="1.0"?>
<rss version="2.0">
  <channel>
    <title>DBLP CS Papers</title>
    <item>
      <title>Learned Cardinality Estimation, Revisited.</title>
      <link>https://example.edu/papers/ce.html</link>
      <pubDate>Mon, 02 Jan 2026 08:00:00 -0700</pubDate>
      <description>&lt;p&gt;Abstract text here.&lt;/p&gt;</description>
    </item>
    <item>
      <title>No date item</title>
      <link>https://example.edu/papers/nd.html</link>
    </item>
  </channel>
</rss>`)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	doc, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/rss"})
	if err != nil {
		t.Fatalf("Fetch rss feed: %v", err)
	}
	if doc.Document.Title != "DBLP CS Papers" {
		t.Errorf("channel title = %q", doc.Document.Title)
	}
	if !strings.Contains(doc.Document.Markdown, "[Learned Cardinality Estimation, Revisited.](https://example.edu/papers/ce.html)") {
		t.Errorf("markdown 缺 item:\n%s", doc.Document.Markdown)
	}
	if !strings.Contains(doc.Document.Markdown, "— 2026-01-02") {
		t.Errorf("RSS pubDate 应解析成 2026-01-02:\n%s", doc.Document.Markdown)
	}
	if !strings.Contains(doc.Document.Markdown, "[No date item](https://example.edu/papers/nd.html)") {
		t.Errorf("无日期条目也应保留:\n%s", doc.Document.Markdown)
	}
}

// TestFetchRejectsNonFeedXML 钉住非 feed XML 仍判 unsupported ——
// 数据 API 的 XML 不应被误当 feed 硬抽。
func TestFetchRejectsNonFeedXML(t *testing.T) {
	allowAnyFetchURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0"?><dataset><row><a>1</a></row></dataset>`)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	_, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/data.xml"})
	if err == nil {
		t.Fatal("非 feed XML 应返回 unsupported")
	}
	if KindOf(err) != ErrUnsupported {
		t.Fatalf("kind = %v, want ErrUnsupported", KindOf(err))
	}
}
