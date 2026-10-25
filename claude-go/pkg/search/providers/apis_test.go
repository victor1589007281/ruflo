package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// TestGitHubProviderSendsConfiguredToken 钉住 config.json → ProviderConfig.APIKey
// 确实变成了出站 Authorization 头。
//
// 只测"LoadConfig 里填了 APIKey"不够: 中间任何一层把 pc.APIKey 丢掉, 表现都是
// GitHub 搜索仍能返回结果 (只是按未认证的 10 req/min 限流), 靠结果数量看不出来。
func TestGitHubProviderSendsConfiguredToken(t *testing.T) {
	const want = "Bearer config-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer srv.Close()

	p, err := newGitHub(search.DefaultConfig(), search.ProviderConfig{
		Enabled: true, BaseURL: srv.URL, APIKey: "config-token",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Search(context.Background(), search.SearchRequest{Query: "go", Limit: 1}); err != nil {
		t.Fatalf("Search: %v", err)
	}
}

// TestDirectAPIs 用一个 httptest 服务覆盖全部直连 API 的解析。
func TestDirectAPIs(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/github/search/repositories", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "cli tool" || r.URL.Query().Get("per_page") != "2" {
			t.Errorf("github params: %v", r.URL.Query())
		}
		if r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Error("github api version header missing")
		}
		fmt.Fprint(w, `{"items":[{"full_name":"acme/tool","html_url":"https://github.com/acme/tool","description":"a cli","stargazers_count":42,"language":"Go","updated_at":"2024-02-03T04:05:06Z"}]}`)
	})

	mux.HandleFunc("/se/2.3/search/advanced", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("site") != "stackoverflow" || q.Get("q") != "go" || q.Get("pagesize") != "2" {
			t.Errorf("stackexchange params: %v", q)
		}
		fmt.Fprint(w, `{"items":[{"title":"Why &quot;Go&quot;?","link":"https://stackoverflow.com/q/1","body":"<p>Use <code>net/http</code></p>","score":7,"creation_date":1600000000,"tags":["go","http"]}]}`)
	})

	mux.HandleFunc("/openalex/works", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("search") != "graphs" || r.URL.Query().Get("per-page") != "2" {
			t.Errorf("openalex params: %v", r.URL.Query())
		}
		fmt.Fprint(w, `{"results":[{"id":"https://openalex.org/W1","doi":"10.1234/abc","display_name":"Graph Paper","publication_date":"2021-04-05","publication_year":2021,"primary_location":{"landing_page_url":"https://publisher.example/p"},"abstract_inverted_index":{"hello":[0],"world":[1]}}]}`)
	})

	mux.HandleFunc("/crossref/works", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "paper" || r.URL.Query().Get("rows") != "2" {
			t.Errorf("crossref params: %v", r.URL.Query())
		}
		fmt.Fprint(w, `{"message":{"items":[{"title":["A Paper"],"URL":"https://doi.example/paper","DOI":"10.5555/paper","abstract":"<jats:p>Some <jats:italic>text</jats:italic></jats:p>","container-title":["Journal"],"published":{"date-parts":[[2020,3,2]]}}]}}`)
	})

	mux.HandleFunc("/eutils/esearch.fcgi", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("db") != "pubmed" || r.URL.Query().Get("term") != "cancer" {
			t.Errorf("esearch params: %v", r.URL.Query())
		}
		fmt.Fprint(w, `{"esearchresult":{"idlist":["111","222"]}}`)
	})
	mux.HandleFunc("/eutils/esummary.fcgi", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != "111,222" {
			t.Errorf("esummary id: %q", r.URL.Query().Get("id"))
		}
		fmt.Fprint(w, `{"result":{"uids":["111","222"],
			"111":{"uid":"111","title":"Cancer Study","pubdate":"2022 Jan 5","source":"J Test","fulljournalname":"Journal of Tests","elocationid":"doi: 10.1/x","authors":[{"name":"Doe J"},{"name":"Smith A"}]},
			"222":{"uid":"222","title":"Second Study","pubdate":"2021","source":"Other"}}}`)
	})

	mux.HandleFunc("/hn/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "startup" || r.URL.Query().Get("hitsPerPage") != "2" {
			t.Errorf("hn params: %v", r.URL.Query())
		}
		fmt.Fprint(w, `{"hits":[
			{"objectID":"1","title":"Launch","url":"https://example.com/launch","created_at":"2024-01-02T03:04:05Z","points":10},
			{"objectID":"2","story_title":"Ask HN","comment_text":"<p>a comment</p>","created_at":"2024-02-03T00:00:00Z","num_comments":3}
		]}`)
	})

	mux.HandleFunc("/npm/-/v1/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("text") != "react" || r.URL.Query().Get("size") != "2" {
			t.Errorf("npm params: %v", r.URL.Query())
		}
		fmt.Fprint(w, `{"objects":[{"package":{"name":"react","version":"18.0.0","description":"UI library","date":"2024-01-01T00:00:00Z","links":{"npm":"https://www.npmjs.com/package/react"}},"score":{"final":0.9}}]}`)
	})

	var pypiMiss int32
	mux.HandleFunc("/pypi/requests/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"info":{"name":"requests","version":"2.31.0","summary":"HTTP for Humans","package_url":"https://pypi.org/project/requests/","project_urls":{"Source":"https://github.com/psf/requests"}}}`)
	})
	mux.HandleFunc("/pypi/missing/json", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&pypiMiss, 1)
		http.Error(w, "Not Found", http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	cfg := search.DefaultConfig()

	searchOne := func(t *testing.T, p search.Provider, query string, limit int) []search.Result {
		t.Helper()
		res, err := p.Search(context.Background(), search.SearchRequest{Query: query, Limit: limit})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		return res
	}

	t.Run("github", func(t *testing.T) {
		p, err := newGitHub(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/github/search/repositories"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res := searchOne(t, p, "cli tool", 2)
		if len(res) != 1 || res[0].Title != "acme/tool" || !strings.Contains(res[0].Snippet, "★ 42") {
			t.Fatalf("unexpected: %+v", res)
		}
		if res[0].Lang != "go" {
			t.Errorf("want lang go, got %q", res[0].Lang)
		}
	})

	t.Run("stackexchange", func(t *testing.T) {
		p, err := newStackExchange(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/se/2.3/search/advanced"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res := searchOne(t, p, "go", 2)
		if len(res) != 1 || res[0].Title != `Why "Go"?` {
			t.Fatalf("unexpected: %+v", res)
		}
		if !strings.Contains(res[0].Snippet, "[go, http]") || !strings.Contains(res[0].Snippet, "net/http") {
			t.Errorf("unexpected snippet: %q", res[0].Snippet)
		}
		if !res[0].Published.Equal(time.Unix(1600000000, 0).UTC()) {
			t.Errorf("unexpected published: %v", res[0].Published)
		}
	})

	t.Run("openalex", func(t *testing.T) {
		p, err := newOpenAlex(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/openalex/works"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res := searchOne(t, p, "graphs", 2)
		if len(res) != 1 || res[0].URL != "https://doi.org/10.1234/abc" {
			t.Fatalf("unexpected: %+v", res)
		}
		if res[0].Snippet != "hello world" {
			t.Errorf("abstract not reconstructed: %q", res[0].Snippet)
		}
	})

	t.Run("crossref", func(t *testing.T) {
		p, err := newCrossref(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/crossref/works"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res := searchOne(t, p, "paper", 2)
		if len(res) != 1 || res[0].Title != "A Paper" || res[0].Snippet != "Some text" {
			t.Fatalf("unexpected: %+v", res)
		}
		if !res[0].Published.Equal(time.Date(2020, 3, 2, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("unexpected published: %v", res[0].Published)
		}
	})

	t.Run("pubmed", func(t *testing.T) {
		p, err := newPubMed(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/eutils"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res := searchOne(t, p, "cancer", 2)
		if len(res) != 2 {
			t.Fatalf("want 2 results, got %d", len(res))
		}
		if res[0].URL != "https://pubmed.ncbi.nlm.nih.gov/111/" || res[0].Title != "Cancer Study" {
			t.Errorf("unexpected result 0: %+v", res[0])
		}
		if !strings.Contains(res[0].Snippet, "Doe J") || !strings.Contains(res[0].Snippet, "Journal of Tests") {
			t.Errorf("unexpected snippet: %q", res[0].Snippet)
		}
		if res[1].Title != "Second Study" {
			t.Errorf("unexpected order: %+v", res)
		}
	})

	t.Run("hn", func(t *testing.T) {
		p, err := newHN(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/hn/search"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res := searchOne(t, p, "startup", 2)
		if len(res) != 2 {
			t.Fatalf("want 2 results, got %d", len(res))
		}
		if res[0].URL != "https://example.com/launch" {
			t.Errorf("unexpected result 0: %+v", res[0])
		}
		if res[1].URL != "https://news.ycombinator.com/item?id=2" || res[1].Snippet != "a comment" {
			t.Errorf("unexpected comment result: %+v", res[1])
		}
	})

	t.Run("npm", func(t *testing.T) {
		p, err := newNPM(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/npm/-/v1/search"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res := searchOne(t, p, "react", 2)
		if len(res) != 1 || res[0].URL != "https://www.npmjs.com/package/react" {
			t.Fatalf("unexpected: %+v", res)
		}
		if !strings.Contains(res[0].Snippet, "v18.0.0") {
			t.Errorf("unexpected snippet: %q", res[0].Snippet)
		}
	})

	t.Run("pypi", func(t *testing.T) {
		p, err := newPyPI(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/pypi"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res := searchOne(t, p, "requests", 0)
		if len(res) != 1 || res[0].URL != "https://pypi.org/project/requests/" {
			t.Fatalf("unexpected: %+v", res)
		}
		if !strings.Contains(res[0].Snippet, "HTTP for Humans") || !strings.Contains(res[0].Snippet, "v2.31.0") {
			t.Errorf("unexpected snippet: %q", res[0].Snippet)
		}
	})

	t.Run("pypi not found", func(t *testing.T) {
		p, err := newPyPI(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/pypi"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := p.Search(context.Background(), search.SearchRequest{Query: "missing"})
		if err != nil {
			t.Fatalf("404 should be empty result, got %v", err)
		}
		if len(res) != 0 {
			t.Fatalf("want empty, got %+v", res)
		}
		if atomic.LoadInt32(&pypiMiss) != 1 {
			t.Errorf("expected one 404 request, got %d", pypiMiss)
		}
	})

	t.Run("pypi invalid name", func(t *testing.T) {
		p, err := newPyPI(cfg, search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/pypi"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Search(context.Background(), search.SearchRequest{Query: "not a package"})
		if search.KindOf(err) != search.ErrUnsupported {
			t.Fatalf("want ErrUnsupported, got %v", err)
		}
	})
}

// TestErrorClassification 覆盖 403/429/503/超时/挑战页的归一化。
func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		header  http.Header
		body    string
		timeout time.Duration
		want    search.ErrorKind
	}{
		{name: "429", status: http.StatusTooManyRequests, body: "slow down", want: search.ErrRateLimited},
		{name: "403 blocked", status: http.StatusForbidden, body: "forbidden", want: search.ErrBlocked},
		{name: "403 rate limit", status: http.StatusForbidden, header: http.Header{"X-Ratelimit-Remaining": {"0"}}, body: "quota", want: search.ErrRateLimited},
		{name: "503", status: http.StatusServiceUnavailable, body: "down", want: search.ErrUnavailable},
		{name: "challenge 200", status: http.StatusOK, header: http.Header{"Content-Type": {"text/html"}}, body: "<html>captcha</html>", want: search.ErrBlocked},
		{name: "timeout", status: http.StatusOK, timeout: 20 * time.Millisecond, want: search.ErrTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.timeout > 0 {
					time.Sleep(150 * time.Millisecond)
					return
				}
				for k, vs := range tc.header {
					for _, v := range vs {
						w.Header().Add(k, v)
					}
				}
				if w.Header().Get("Content-Type") == "" {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			p, err := newHN(search.DefaultConfig(), search.ProviderConfig{
				Enabled: true, BaseURL: srv.URL + "/hn/search", Timeout: tc.timeout,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Search(context.Background(), search.SearchRequest{Query: "q"})
			if got := search.KindOf(err); got != tc.want {
				t.Fatalf("want %s, got %s (%v)", tc.want, got, err)
			}
		})
	}
}

// TestCircuitBreaker 覆盖连续失败后的本地熔断与成功恢复。
func TestCircuitBreaker(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"hits":[]}`)
	}))
	defer srv.Close()

	p, _ := newHN(search.DefaultConfig(), search.ProviderConfig{Enabled: true, BaseURL: srv.URL + "/hn/search"}, nil)
	for i := 0; i < circuitThreshold; i++ {
		_, _ = p.Search(context.Background(), search.SearchRequest{Query: "q"})
	}
	if p.Healthy() {
		t.Fatal("provider should be open after repeated failures")
	}
	fail.Store(false)
	// 熔断窗口内仍不可用, 但健康位到期后成功请求应清零。
	if _, err := p.Search(context.Background(), search.SearchRequest{Query: "q"}); err != nil {
		t.Fatalf("request during open window should still execute: %v", err)
	}
	if !p.Healthy() {
		t.Fatal("success should close the circuit")
	}
}
