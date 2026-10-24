package global

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

const fredJSON = `{
	"realtime_start":"2024-04-01","realtime_end":"2024-04-01",
	"observations":[
		{"realtime_start":"2024-04-01","realtime_end":"2024-04-01","date":"2024-01-01","value":"3.5"},
		{"realtime_start":"2024-04-01","realtime_end":"2024-04-01","date":"2024-02-01","value":"."},
		{"realtime_start":"2024-04-01","realtime_end":"2024-04-01","date":"2024-03-01","value":"3.8"}
	]
}`

// TestFREDMacro 验证 observations 解析、缺失值跳过与 SourceURL 不含 key。
func TestFREDMacro(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/fred/series/observations" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, fredJSON)
	}))
	defer srv.Close()

	pc := market.ProviderConfig{
		Enabled: true, BaseURL: srv.URL, Timeout: 2 * time.Second, APIKey: "test-key",
	}
	p := newFRED(pc, market.DefaultConfig(), srv.Client())
	facts, err := p.Macro(context.Background(), "CPI",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{})
	if err != nil {
		t.Fatalf("Macro: %v", err)
	}
	for _, want := range []string{"series_id=CPIAUCSL", "api_key=test-key", "observation_start=2024-01-01"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query = %q, missing %q", gotQuery, want)
		}
	}
	if len(facts) != 2 {
		t.Fatalf("facts = %d, want 2 (\".\" 被跳过)", len(facts))
	}
	if facts[0].Value != 3.5 || facts[1].Value != 3.8 {
		t.Fatalf("values = %v/%v", facts[0].Value, facts[1].Value)
	}
	if !facts[0].AsOf.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("AsOf = %v", facts[0].AsOf)
	}
	if facts[0].Period != "2024-01-01" || facts[0].Delay != "eod" {
		t.Fatalf("period/delay = %q/%q", facts[0].Period, facts[0].Delay)
	}
	if strings.Contains(facts[0].SourceURL, "test-key") {
		t.Fatalf("SourceURL 泄露 api_key: %q", facts[0].SourceURL)
	}
	if !strings.Contains(facts[0].SourceURL, "series_id=CPIAUCSL") {
		t.Fatalf("SourceURL = %q", facts[0].SourceURL)
	}
}

// TestFREDWithoutKey 验证无 key 时不发请求并返回不可用。
func TestFREDWithoutKey(t *testing.T) {
	t.Setenv("FRED_API_KEY", "")
	pc := market.ProviderConfig{Enabled: true, BaseURL: "http://127.0.0.1:1", Timeout: time.Second}
	p := newFRED(pc, market.DefaultConfig(), nil)
	_, err := p.Macro(context.Background(), "CPI", time.Time{}, time.Time{})
	if market.KindOf(err) != market.ErrUnavailable {
		t.Fatalf("kind = %q", market.KindOf(err))
	}
}

// TestFREDSeriesAlias 验证别名展开与大写。
func TestFREDSeriesAlias(t *testing.T) {
	cases := map[string]string{
		"cpi":      "CPIAUCSL",
		"gdp":      "GDPC1",
		"dgs10":    "DGS10",
		"CPIAUCSL": "CPIAUCSL",
	}
	for in, want := range cases {
		if got := fredSeriesID(in); got != want {
			t.Fatalf("fredSeriesID(%q) = %q, want %q", in, got, want)
		}
	}
}
