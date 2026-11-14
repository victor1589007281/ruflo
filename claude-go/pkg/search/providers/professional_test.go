package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// TestProfessionalProvidersParse 用一个 httptest 服务覆盖全部新增专业源的解析。
// 重点不是字段齐全，而是：URL 可打开、标题不丢、摘要被清成纯文本、顺序稳定。
func TestProfessionalProvidersParse(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/wds", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("rows") != "2" {
			t.Errorf("worldbank rows: %v", q)
		}
		// 模拟 WDS 的严格 AND：带年份的长查询返回 0，provider 去掉年份重试。
		if strings.Contains(q.Get("qterm"), "2024") {
			fmt.Fprint(w, `{"documents":{"facets":{}}}`)
			return
		}
		if q.Get("qterm") != "annual report" {
			t.Errorf("worldbank params: %v", q)
		}
		// documents 是对象，顺序就是相关性；facets 必须被跳过。
		fmt.Fprint(w, `{"documents":{
			"D1":{"display_title":"World Development Report 2024","abstracts":{"cdata!":"Growth <b>slowed</b> sharply."},"docdt":"2024-06-01T00:00:00Z","url":"http://documents.worldbank.org/a","lang":"en"},
			"facets":{"count":2},
			"D2":{"display_title":"Country Economic Memorandum","abstracts":{"cdata!":"Second doc."},"docdt":"2023-01-02","url":"http://documents.worldbank.org/b","lang":"en"}
		}}`)
	})
	mux.HandleFunc("/dois", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "dataset" || r.URL.Query().Get("page[size]") != "2" {
			t.Errorf("datacite params: %v", r.URL.Query())
		}
		fmt.Fprint(w, `{"data":[{"id":"10.1/x","attributes":{"doi":"10.1/x","url":"http://example.org/data","publisher":"World Bank","publicationYear":2023,"language":"en","titles":[{"title":"Household Survey Data"}],"descriptions":[{"description":"<p>Micro data</p>","descriptionType":"Abstract"}]}}]}`)
	})
	mux.HandleFunc("/records", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "report" || r.URL.Query().Get("size") != "2" {
			t.Errorf("zenodo params: %v", r.URL.Query())
		}
		fmt.Fprint(w, `{"hits":{"hits":[{"id":7,"doi_url":"https://doi.org/10.5281/zenodo.7","links":{"self_html":"https://zenodo.org/records/7"},"metadata":{"title":"Technical Report","description":"<p>Text</p>","publication_date":"2022-03-04","language":"en","resource_type":{"title":"Report"}}}]}}`)
	})
	mux.HandleFunc("/articles/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "open access") {
			t.Errorf("doaj path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("pageSize") != "2" {
			t.Errorf("doaj pageSize = %q", r.URL.Query().Get("pageSize"))
		}
		fmt.Fprint(w, `{"results":[{"bibjson":{"title":"Open Access Paper","abstract":"Abstract text","year":2021,"link":[{"url":"https://journal.example/a","type":"fulltext"}],"identifier":[{"id":"10.1/a","type":"doi"}],"journal":{"title":"Journal of Tests"}}}]}`)
	})
	mux.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("search_query"), `all:"preprint`) {
			t.Errorf("arxiv search_query = %q", r.URL.Query().Get("search_query"))
		}
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>http://arxiv.org/abs/2401.00001v1</id>
    <title>A Preprint
      Title</title>
    <summary>  We study   agents. </summary>
    <published>2024-01-02T03:04:05Z</published>
    <author><name>Doe, J</name></author>
  </entry>
</feed>`)
	})
	mux.HandleFunc("/cninfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("cninfo method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.PostForm.Get("searchkey") != "年报" || r.PostForm.Get("pageSize") != "2" {
			t.Errorf("cninfo form: %v", r.PostForm)
		}
		fmt.Fprint(w, `{"announcements":[{"secCode":"600000","secName":"浦发银行","announcementTitle":"2025 年年度<em>报告</em>","announcementTime":1767225600000,"adjunctUrl":"finalpage/2026-01-01/1.PDF","announcementId":"123","orgId":"g1"}]}`)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := search.DefaultConfig()
	mk := func(ctor providerCtor, path string) search.Provider {
		p, err := ctor(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + path}, nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return p
	}

	t.Run("worldbank", func(t *testing.T) {
		p := mk(newWorldBank, "/wds")
		res, err := p.Search(context.Background(), search.SearchRequest{Query: "annual report", Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("got %d results, want 2: %+v", len(res), res)
		}
		if res[0].Title != "World Development Report 2024" || res[0].URL != "https://documents.worldbank.org/a" {
			t.Errorf("first result = %+v", res[0])
		}
		if strings.Contains(res[0].Snippet, "<b>") {
			t.Errorf("snippet not stripped: %q", res[0].Snippet)
		}
		if res[0].Published.IsZero() {
			t.Errorf("docdt not parsed: %+v", res[0])
		}
		if res[1].Title != "Country Economic Memorandum" {
			t.Errorf("order not preserved: %+v", res)
		}
	})

	t.Run("datacite", func(t *testing.T) {
		p := mk(newDataCite, "/dois")
		res, err := p.Search(context.Background(), search.SearchRequest{Query: "dataset", Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].URL != "https://example.org/data" {
			t.Fatalf("got %+v", res)
		}
		if res[0].Title != "Household Survey Data" || !strings.Contains(res[0].Snippet, "Micro data") {
			t.Errorf("parse = %+v", res[0])
		}
	})

	t.Run("zenodo", func(t *testing.T) {
		p := mk(newZenodo, "/records")
		res, err := p.Search(context.Background(), search.SearchRequest{Query: "report", Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].URL != "https://doi.org/10.5281/zenodo.7" {
			t.Fatalf("got %+v", res)
		}
		if !strings.Contains(res[0].Snippet, "Report：") {
			t.Errorf("resource type missing: %q", res[0].Snippet)
		}
		if res[0].Published.Year() != 2022 {
			t.Errorf("published = %v", res[0].Published)
		}
	})

	t.Run("doaj", func(t *testing.T) {
		p := mk(newDOAJ, "/articles")
		res, err := p.Search(context.Background(), search.SearchRequest{Query: "open access", Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].URL != "https://journal.example/a" {
			t.Fatalf("got %+v", res)
		}
		if !strings.Contains(res[0].Snippet, "Journal of Tests") {
			t.Errorf("journal missing: %q", res[0].Snippet)
		}
	})

	t.Run("arxiv", func(t *testing.T) {
		p := mk(newArxiv, "/query")
		res, err := p.Search(context.Background(), search.SearchRequest{Query: "preprint", Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].URL != "https://arxiv.org/abs/2401.00001v1" {
			t.Fatalf("got %+v", res)
		}
		if res[0].Title != "A Preprint Title" {
			t.Errorf("title = %q", res[0].Title)
		}
		if !strings.Contains(res[0].Snippet, "Doe, J") {
			t.Errorf("authors missing: %q", res[0].Snippet)
		}
	})

	t.Run("cninfo", func(t *testing.T) {
		p := mk(newCNInfo, "/cninfo")
		res, err := p.Search(context.Background(), search.SearchRequest{Query: "年报", Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 {
			t.Fatalf("got %+v", res)
		}
		if res[0].Title != "2025 年年度报告" {
			t.Errorf("title = %q (em tag not stripped?)", res[0].Title)
		}
		if !strings.Contains(res[0].URL, "announcementId=123") || !strings.Contains(res[0].URL, "stockCode=600000") {
			t.Errorf("detail url = %q", res[0].URL)
		}
		if strings.Contains(res[0].Snippet, "<em>") {
			t.Errorf("snippet 未清理高亮标签: %q", res[0].Snippet)
		}
		if res[0].Lang != "zh" || res[0].Published.IsZero() {
			t.Errorf("lang/published = %q/%v", res[0].Lang, res[0].Published)
		}
	})
}

// TestWorldBankDropsYearOnEmpty 钉住 WDS 的严格 AND 兜底：
// 带年份的空结果会去掉年份重试一次，且只重试一次。
func TestWorldBankDropsYearOnEmpty(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query().Get("qterm")
		if strings.Contains(q, "2024") {
			fmt.Fprint(w, `{"documents":{"facets":{}}}`)
			return
		}
		fmt.Fprint(w, `{"documents":{"D1":{"display_title":"Inflation Report","docdt":"2023-01-01","url":"http://documents.worldbank.org/x","lang":"en"}}}`)
	}))
	defer srv.Close()

	cfg := search.DefaultConfig()
	p, err := newWorldBank(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "inflation report 2024", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Title != "Inflation Report" {
		t.Fatalf("fallback 未生效: %+v", res)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2（原查询 + 去年份重试）", calls)
	}
}

// TestCNInfoSimplifiesQueryOnEmpty 钉住巨潮的严格匹配兜底：
// 年份按公告年份过滤、分析词不在标题里，带这些词的查询会 0 命中；
// provider 应收敛成「实体 + 披露类型」后重试一次。
func TestCNInfoSimplifiesQueryOnEmpty(t *testing.T) {
	var calls int
	var lastKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		calls++
		lastKey = r.PostForm.Get("searchkey")
		if strings.Contains(lastKey, "2025") || strings.Contains(lastKey, "营收") {
			fmt.Fprint(w, `{"announcements":[]}`)
			return
		}
		fmt.Fprint(w, `{"announcements":[{"secCode":"002594","secName":"比亚迪","announcementTitle":"2025年年度报告","announcementTime":1767225600000,"announcementId":"9","orgId":"g"}]}`)
	}))
	defer srv.Close()

	cfg := search.DefaultConfig()
	p, err := newCNInfo(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Search(context.Background(), search.SearchRequest{Query: "比亚迪 2025 年年度报告 营收", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("fallback 未生效（lastKey=%q）: %+v", lastKey, res)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if lastKey != "比亚迪 年年度报告" {
		t.Errorf("收敛后的 searchkey = %q", lastKey)
	}
}

// TestCNInfoNoFallbackWithoutDisclosureWord: 统计/行业报告类查询本就不该
// 在巨潮上反复重试，空结果直接返回。
func TestCNInfoNoFallbackWithoutDisclosureWord(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"announcements":[]}`)
	}))
	defer srv.Close()

	cfg := search.DefaultConfig()
	p, err := newCNInfo(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Search(context.Background(), search.SearchRequest{Query: "中国新能源汽车行业报告 市场规模", Limit: 3}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1（未命中披露词不应重试）", calls)
	}
}

// TestBasePaceSerializesRequests 钉住单源节流：两次请求之间至少间隔 minInterval。
func TestBasePaceSerializesRequests(t *testing.T) {
	b := &base{minInterval: 30 * time.Millisecond}
	ctx := context.Background()
	start := time.Now()
	if err := b.pace(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.pace(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("pace 未生效: %v", elapsed)
	}
}

// TestBasePaceHonorsContext: 等待期间取消必须立刻返回，不能卡满间隔。
func TestBasePaceHonorsContext(t *testing.T) {
	b := &base{minInterval: time.Hour}
	if err := b.pace(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := b.pace(ctx); err == nil {
		t.Fatal("pace 应返回 context 错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("pace 未响应取消: %v", elapsed)
	}
}
