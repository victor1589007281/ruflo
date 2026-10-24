package global

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

const worldBankJSON = `[
	{"page":1,"pages":1,"per_page":1000,"total":2,"sourceid":"2","lastupdated":"2024-01-01"},
	[
		{"indicator":{"id":"NY.GDP.MKTP.CD","value":"GDP (current US$)"},
		 "country":{"id":"US","value":"United States"},"countryiso3code":"USA",
		 "date":"2022","value":25462700000000,"unit":"","obs_status":"","decimal":0},
		{"indicator":{"id":"NY.GDP.MKTP.CD","value":"GDP (current US$)"},
		 "country":{"id":"US","value":"United States"},"countryiso3code":"USA",
		 "date":"2021","value":null,"unit":"","obs_status":"","decimal":0}
	]
]`

// TestWorldBankMacro 验证两段式响应解析、年度日期与 null 缺口。
func TestWorldBankMacro(t *testing.T) {
	var gotPath string
	var gotDate string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotDate = r.URL.Query().Get("date")
		io.WriteString(w, worldBankJSON)
	}))
	defer srv.Close()

	pc := market.ProviderConfig{Enabled: true, BaseURL: srv.URL, Timeout: 2 * time.Second}
	p := newWorldBank(pc, market.DefaultConfig(), srv.Client())
	facts, err := p.Macro(context.Background(), "US:NY.GDP.MKTP.CD",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2022, 12, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Macro: %v", err)
	}
	if gotPath != "/v2/country/US/indicator/NY.GDP.MKTP.CD" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotDate != "2020:2022" {
		t.Fatalf("date = %q", gotDate)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %d, want 1 (null 被跳过)", len(facts))
	}
	f := facts[0]
	if f.Value != 25462700000000 || f.Field != "NY.GDP.MKTP.CD" {
		t.Fatalf("fact = %+v", f)
	}
	if !f.AsOf.Equal(time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("AsOf = %v", f.AsOf)
	}
	if f.Period != "2022" || f.Delay != "annual" || f.Source != "worldbank" {
		t.Fatalf("period/delay/source = %q/%q/%q", f.Period, f.Delay, f.Source)
	}
	if f.Text != "GDP (current US$)" || f.Symbol != "US:NY.GDP.MKTP.CD" {
		t.Fatalf("text/symbol = %q/%q", f.Text, f.Symbol)
	}
}

// TestWorldBankAlias 验证省略国家时默认 US 且口语别名展开。
func TestWorldBankAlias(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, worldBankJSON)
	}))
	defer srv.Close()

	pc := market.ProviderConfig{Enabled: true, BaseURL: srv.URL, Timeout: 2 * time.Second}
	p := newWorldBank(pc, market.DefaultConfig(), srv.Client())
	if _, err := p.Macro(context.Background(), "GDP", time.Time{}, time.Time{}); err != nil {
		t.Fatalf("Macro: %v", err)
	}
	if gotPath != "/v2/country/US/indicator/NY.GDP.MKTP.CD" {
		t.Fatalf("path = %q", gotPath)
	}
}

// TestWorldBankUpstreamError 验证错误消息转换为未解析。
func TestWorldBankUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"message":[{"id":"120","key":"Invalid value","value":"bad indicator"}]}]`)
	}))
	defer srv.Close()

	pc := market.ProviderConfig{Enabled: true, BaseURL: srv.URL, Timeout: 2 * time.Second}
	p := newWorldBank(pc, market.DefaultConfig(), srv.Client())
	_, err := p.Macro(context.Background(), "US:BAD.INDICATOR", time.Time{}, time.Time{})
	if market.KindOf(err) != market.ErrUnresolved {
		t.Fatalf("kind = %q", market.KindOf(err))
	}
}
