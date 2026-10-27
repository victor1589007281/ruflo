package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// TestFetchPromptBudgetHashEvidenceAndContinuation 钉住全文缓存位于选段之前：
// 不同 prompt/预算/offset 必须复用同一次网络抓取，证据仍能回查完整正文。
func TestFetchPromptBudgetHashEvidenceAndContinuation(t *testing.T) {
	allowAnyFetchURL(t)
	var calls atomic.Int32
	body := "前导甲 " + strings.Repeat("unrelated ", 40) +
		"\n\nphotosynthesis chlorophyll target fact is verbatim.\n\n" +
		strings.Repeat("tail-marker ", 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	first, err := m.Fetch(context.Background(), FetchRequest{
		URL: srv.URL, Prompt: "photosynthesis chlorophyll", MaxChars: 160, MaxContentTokens: 80,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	doc := first.Document
	if doc.Selection != "relevant" || !strings.Contains(doc.Markdown, "target fact") {
		t.Fatalf("prompt did not select target window: %+v", doc)
	}
	if doc.EstimatedTokens > 80 || doc.EstimatedTokens != len([]byte(doc.Markdown)) {
		t.Fatalf("UTF-8 token upper bound not enforced: tokens=%d bytes=%d", doc.EstimatedTokens, len([]byte(doc.Markdown)))
	}
	full := strings.TrimSpace(normalizeMarkdown(body))
	if doc.Hash != hashDocument(full) || doc.Hash == hashDocument(doc.Markdown) {
		t.Fatalf("hash must cover full document, not excerpt: %q", doc.Hash)
	}
	if doc.TotalChars != utf8.RuneCountInString(full) || !doc.Truncated || doc.NextOffset <= 0 {
		t.Fatalf("bad continuation metadata: %+v", doc)
	}
	if len(doc.Evidence) == 0 {
		t.Fatal("prompt-selected document returned no evidence")
	}
	fullRunes := []rune(full)
	for _, ev := range doc.Evidence {
		quoteRunes := []rune(ev.Quote)
		if ev.Offset < 0 || ev.Offset+len(quoteRunes) > len(fullRunes) || string(fullRunes[ev.Offset:ev.Offset+len(quoteRunes)]) != ev.Quote {
			t.Fatalf("evidence does not resolve against full rune text: %+v", ev)
		}
	}

	second, err := m.Fetch(context.Background(), FetchRequest{
		URL: srv.URL, Prompt: "tail-marker", MaxChars: 50,
	})
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if !second.Document.CacheHit || second.Document.Hash != doc.Hash || !strings.Contains(second.Document.Markdown, "tail-marker") {
		t.Fatalf("full document was not reused across prompts: %+v", second.Document)
	}

	continued, err := m.Fetch(context.Background(), FetchRequest{
		URL: srv.URL, MaxChars: 50, Offset: doc.NextOffset,
	})
	if err != nil {
		t.Fatalf("continuation Fetch: %v", err)
	}
	if !continued.Document.CacheHit || continued.Document.Selection != "continuation" ||
		continued.Document.Offset != doc.NextOffset || continued.Document.Hash != doc.Hash {
		t.Fatalf("bad continuation: %+v", continued.Document)
	}

	out, err := NewLegacySearcher(m).FetchRequestJSON(context.Background(), FetchRequest{
		URL: srv.URL, Prompt: "photosynthesis", MaxContentTokens: 20,
	})
	if err != nil {
		t.Fatalf("FetchRequestJSON: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid Fetch JSON: %v\n%s", err, out)
	}
	for _, key := range []string{
		"url", "markdown", "hash", "offset", "next_offset", "total_chars",
		"estimated_content_tokens", "token_count_method", "cache_hit", "stale", "selection", "evidence", "elapsed_ms",
	} {
		if _, ok := payload[key]; !ok {
			t.Errorf("Fetch JSON missing %q: %s", key, out)
		}
	}
	for _, key := range []string{"URL", "Markdown", "Hash", "Evidence", "CacheHit"} {
		if _, ok := payload[key]; ok {
			t.Errorf("Fetch JSON exposed unstable Go field %q: %s", key, out)
		}
	}
	evidence, ok := payload["evidence"].([]any)
	if !ok || len(evidence) == 0 {
		t.Fatalf("Fetch JSON evidence = %#v", payload["evidence"])
	}
	evidenceFields, ok := evidence[0].(map[string]any)
	if !ok {
		t.Fatalf("Fetch JSON evidence item = %#v", evidence[0])
	}
	for _, key := range []string{"id", "url", "quote", "offset", "hash", "fetched_at"} {
		if _, ok := evidenceFields[key]; !ok {
			t.Errorf("Fetch JSON evidence missing %q: %#v", key, evidenceFields)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("HTTP calls = %d, want 1", got)
	}
}

func TestFetchPromptFunctionWordsDoNotOutrankDefinition(t *testing.T) {
	allowAnyFetchURL(t)
	definition := "A span represents a unit of work or operation. Spans are the building blocks of Traces. In OpenTelemetry, they include the following information:"
	according := "When a span is created, it is one of Client, Server, Internal, Producer, or Consumer. This span kind provides a hint to the tracing backend as to how the trace should be assembled. According to the OpenTelemetry specification, the parent of a server span is often a remote client span, and the child of a client span is usually a server span. Additional detail makes this unrelated section too large to join the earlier window."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, definition+"\n\n"+according)
	}))
	defer srv.Close()
	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	resp, err := m.Fetch(context.Background(), FetchRequest{
		URL: srv.URL, Prompt: "What is the definition of a Trace and a Span according to OpenTelemetry documentation?", MaxChars: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Document.Selection != "relevant" || !strings.HasPrefix(resp.Document.Markdown, "A span represents") {
		t.Fatalf("function word 'according' outranked the definition: %+v", resp.Document)
	}
}

func TestFetchPromptSelectsDispersedFactsWithinByteBudget(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		prompt  string
		want    []string
		notWant []string
	}{
		{
			name: "paper metadata and architecture",
			body: "[Submitted on 12 Jun 2017 (v1), last revised 2 Aug 2023 (this version, v7)]\n\n" +
				"# Title:Attention Is All You Need\n\n" +
				"Authors:Ashish Vaswani, Noam Shazeer, Niki Parmar, Jakob Uszkoreit, Llion Jones, Aidan N. Gomez, Lukasz Kaiser, Illia Polosukhin" +
				"View a PDF of the paper titled Attention Is All You Need, by Ashish Vaswani and 7 other authorsView PDFHTML (experimental)\n\n" +
				"> Abstract:The dominant sequence transduction models are based on complex recurrent or convolutional neural networks. " +
				"We propose a new simple network architecture, the Transformer, based solely on attention mechanisms, dispensing with recurrence and convolutions entirely. " +
				"Experiments on two machine translation tasks show these models to be superior in quality.\n\n" +
				"## Access Paper:\n\n- Author\n- Venue\n- Institution\n- Topic\n\n" +
				"### DBLP - CS Bibliography\n\nlisting | bibtexAshish Vaswani\nNoam Shazeer\nNiki Parmar\nJakob Uszkoreit\nLlion Jones…Loading...\n\n" +
				"# Bibliographic and Citation Tools\n\nBibliographic Explorer Toggle Bibliographic Explorer(What is the Explorer?) " +
				"Connected Papers Toggle Connected Papers(What is Connected Papers?) Litmaps Toggle Litmaps(What is Litmaps?) " +
				"scite.ai Toggle scite(What is scite?) Smart Citations(What are Smart Citations?)Code, Data, Media\n\n" +
				"Which authors of this paper are endorsers? | Disable MathJax (What is MathJax?)",
			prompt:  "List the paper authors, the submission date, and the architecture proposed in this paper.",
			want:    []string{"Submitted on 12 Jun 2017", "Illia Polosukhin", "Transformer"},
			notWant: []string{"View a PDF", "Access Paper", "Disable MathJax", "listing | bibtex", "Connected Papers"},
		},
		{
			name: "statement date and decision",
			body: "## Press Release\n\nPDF\n\nSeptember 16, 2026\n\n" +
				"### Federal Reserve issues FOMC statement\n\nFor release at 2:00 p.m. EDT\n\n" +
				"The Federal Open Market Committee approved the following statement for release by a 12 – 0 vote:\n\n" +
				"The Committee decided to raise the target range for the federal funds rate by 1/4 percentage point to 3-3/4 to 4 percent, in support of the Federal Reserve's dual mandate. " +
				"The Committee is continuing its policy of maintaining ample reserves in the banking system.",
			prompt: "What target range for the federal funds rate did the FOMC decide, and what is the date of the statement?",
			want:   []string{"September 16, 2026", "target range for the federal funds rate"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowAnyFetchURL(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			m := NewManager(DefaultConfig(), testEgress(t), nil)
			defer m.Close()
			resp, err := m.Fetch(context.Background(), FetchRequest{
				URL: srv.URL, Prompt: tt.prompt, MaxContentTokens: 400,
			})
			if err != nil {
				t.Fatal(err)
			}
			doc := resp.Document
			if doc.EstimatedTokens > 400 || doc.EstimatedTokens != len([]byte(doc.Markdown)) {
				t.Fatalf("invalid byte budget: %+v", doc)
			}
			for _, want := range tt.want {
				if !strings.Contains(doc.Markdown, want) {
					t.Errorf("selected excerpt lacks %q: %s", want, doc.Markdown)
				}
			}
			for _, unwanted := range tt.notWant {
				if strings.Contains(doc.Markdown, unwanted) {
					t.Errorf("selected excerpt contains navigation %q: %s", unwanted, doc.Markdown)
				}
			}
			full := []rune(strings.TrimSpace(normalizeMarkdown(tt.body)))
			for _, ev := range doc.Evidence {
				quote := []rune(ev.Quote)
				if ev.Offset < 0 || ev.Offset+len(quote) > len(full) || string(full[ev.Offset:ev.Offset+len(quote)]) != ev.Quote {
					t.Errorf("evidence offset does not resolve to full text: %+v", ev)
				}
			}
		})
	}
}

func TestFetchPromptEvidenceIsNotLimitedToExcerptPrefix(t *testing.T) {
	allowAnyFetchURL(t)
	body := strings.Repeat("A Tracer creates spans and traces for an operation. ", 12) +
		"\n\nA span represents a unit of work. Spans are the building blocks of Traces."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()
	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	resp, err := m.Fetch(context.Background(), FetchRequest{
		URL: srv.URL, Prompt: "What are the definitions of a Trace and a Span?", MaxChars: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	const target = "Spans are the building blocks of Traces."
	if !strings.Contains(resp.Document.Markdown, target) {
		t.Fatal("test fixture does not contain target in returned Markdown")
	}
	for _, ev := range resp.Document.Evidence {
		if strings.Contains(ev.Quote, target) {
			return
		}
	}
	t.Fatalf("prompt evidence ignored the answer beyond the 500-rune prefix: %+v", resp.Document.Evidence)
}

func TestFetchSingleflightCachesFullDocument(t *testing.T) {
	allowAnyFetchURL(t)
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, strings.Repeat("alpha beta gamma ", 100))
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()
	const workers = 12
	results := make(chan *FetchResponse, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			resp, err := m.Fetch(context.Background(), FetchRequest{
				URL: srv.URL, Prompt: fmt.Sprintf("alpha %d", i), MaxChars: 80 + i,
			})
			results <- resp
			errs <- err
		}(i)
	}
	<-started
	close(release)
	var hash string
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Fetch: %v", err)
		}
		resp := <-results
		if resp == nil || resp.Document == nil {
			t.Fatal("missing document")
		}
		if hash == "" {
			hash = resp.Document.Hash
		} else if resp.Document.Hash != hash {
			t.Fatalf("cache variants changed full hash: %q != %q", resp.Document.Hash, hash)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("HTTP calls = %d, want 1", got)
	}
}

func TestFetchStaleIfErrorDoesNotBypassPermanentFailures(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		wantStale   bool
		wantKind    ErrorKind
	}{
		{name: "temporary 503", status: http.StatusServiceUnavailable, wantStale: true},
		{name: "forbidden", status: http.StatusForbidden, wantKind: ErrBlocked},
		{name: "not found", status: http.StatusNotFound, wantKind: ErrInvalid},
		{name: "unsupported", status: http.StatusOK, contentType: "application/zip", wantKind: ErrUnsupported},
		{name: "corrupt pdf", status: http.StatusOK, contentType: "application/pdf", wantKind: ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowAnyFetchURL(t)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "text/plain")
					_, _ = fmt.Fprint(w, "cached original")
					return
				}
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				} else {
					w.Header().Set("Content-Type", "text/plain")
				}
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprint(w, "replacement must not be trusted")
			}))
			defer srv.Close()

			m := NewManager(DefaultConfig(), testEgress(t), nil)
			defer m.Close()
			now := time.Now()
			m.fetchCache.now = func() time.Time { return now }
			if _, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL}); err != nil {
				t.Fatalf("prime cache: %v", err)
			}
			now = now.Add(2 * m.cfg.CacheTTL)
			resp, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL})
			if tt.wantStale {
				if err != nil {
					t.Fatalf("stale fallback: %v", err)
				}
				if !resp.Document.Stale || !resp.Document.CacheHit || resp.Document.Markdown != "cached original" ||
					!containsString(resp.Degraded, "stale_if_error:unavailable") {
					t.Fatalf("stale fallback not explicit: %+v, degraded=%v", resp.Document, resp.Degraded)
				}
			} else {
				if err == nil || KindOf(err) != tt.wantKind {
					t.Fatalf("permanent failure bypassed by stale cache: resp=%+v err=%v", resp, err)
				}
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("HTTP calls = %d, want 2", got)
			}
		})
	}
}

func TestFetchRejectsInvalidBudgetBeforeNetwork(t *testing.T) {
	allowAnyFetchURL(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, "must not be fetched")
	}))
	defer srv.Close()
	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	for _, req := range []FetchRequest{
		{URL: srv.URL, MaxChars: -1},
		{URL: srv.URL, MaxContentTokens: -1},
		{URL: srv.URL, Offset: -1},
	} {
		if _, err := m.Fetch(context.Background(), req); KindOf(err) != ErrInvalid {
			t.Fatalf("request %+v: kind=%s err=%v", req, KindOf(err), err)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid requests made %d HTTP calls", got)
	}
}

func TestFetchUTF8TokenBudgetAndContinuation(t *testing.T) {
	allowAnyFetchURL(t)
	const body = "A中文🙂B尾"
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()
	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	var rebuilt strings.Builder
	offset, steps := 0, 0
	for {
		steps++
		if steps > len([]rune(body))+1 {
			t.Fatal("continuation did not terminate")
		}
		resp, err := m.Fetch(context.Background(), FetchRequest{
			URL: srv.URL, Offset: offset, MaxContentTokens: 5,
		})
		if err != nil {
			t.Fatalf("Fetch offset %d: %v", offset, err)
		}
		doc := resp.Document
		if doc.Markdown == "" || !utf8.ValidString(doc.Markdown) ||
			doc.EstimatedTokens != len([]byte(doc.Markdown)) || doc.EstimatedTokens > 5 {
			t.Fatalf("invalid bounded excerpt: %+v", doc)
		}
		if doc.Offset != offset {
			t.Fatalf("offset = %d, want %d", doc.Offset, offset)
		}
		rebuilt.WriteString(doc.Markdown)
		if doc.NextOffset == 0 {
			break
		}
		if doc.NextOffset <= offset {
			t.Fatalf("non-advancing next_offset %d at %d", doc.NextOffset, offset)
		}
		offset = doc.NextOffset
	}
	if rebuilt.String() != body {
		t.Fatalf("continuation lost or changed text: %q != %q", rebuilt.String(), body)
	}
	if _, err := m.Fetch(context.Background(), FetchRequest{
		URL: srv.URL, Offset: 1, MaxContentTokens: 1,
	}); KindOf(err) != ErrInvalid {
		t.Fatalf("one byte cannot fit a Chinese rune: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("HTTP calls = %d, want cached full-document reuse", got)
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
