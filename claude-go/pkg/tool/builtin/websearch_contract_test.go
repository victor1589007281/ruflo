package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/search"
)

type contractSearcher struct {
	req search.SearchRequest
}

func (s *contractSearcher) Search(context.Context, string) (string, string, string, error) {
	return "", "", "", nil
}
func (s *contractSearcher) SearchRequestJSON(_ context.Context, req search.SearchRequest) (string, error) {
	s.req = req
	return `{"status":"success","results":[]}`, nil
}

func TestWebSearchForwardsStructuredParameters(t *testing.T) {
	s := &contractSearcher{}
	tool := NewWebSearchTool(s)
	input := json.RawMessage(`{"query":"focused","original_question":"full question","limit":3,"language":"zh","freshness":"week","allowed_domains":["example.com"],"blocked_domains":["bad.example"]}`)
	got, err := tool.Call(context.Background(), input, nil)
	if err != nil || got.IsError {
		t.Fatalf("Call: %+v, %v", got, err)
	}
	if s.req.Query != "focused" || s.req.OriginalQuestion != "full question" || s.req.Limit != 3 || len(s.req.AllowedDomains) != 1 || len(s.req.BlockedDomains) != 1 {
		t.Fatalf("lost parameters: %+v", s.req)
	}
}

func TestWebSearchMaxResultsCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name      string
		input     string
		wantLimit int
	}{
		{name: "alias only", input: `{"query":"memory benchmarks","max_results":8}`, wantLimit: 8},
		{name: "matching limits", input: `{"query":"memory benchmarks","limit":8,"max_results":8}`, wantLimit: 8},
		{name: "limit only", input: `{"query":"memory benchmarks","limit":3}`, wantLimit: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &contractSearcher{}
			got, err := NewWebSearchTool(s).Call(context.Background(), json.RawMessage(tt.input), nil)
			if err != nil || got == nil || got.IsError {
				t.Fatalf("Call: %+v, %v", got, err)
			}
			if s.req.Limit != tt.wantLimit {
				t.Fatalf("forwarded limit=%d, want %d", s.req.Limit, tt.wantLimit)
			}
		})
	}

	s := &contractSearcher{}
	got, err := NewWebSearchTool(s).Call(context.Background(), json.RawMessage(`{"query":"x","limit":3,"max_results":8}`), nil)
	if err != nil || got == nil || !got.IsError || !strings.Contains(got.Content, "must match") {
		t.Fatalf("conflicting limits were not rejected: %+v, %v", got, err)
	}
	if s.req.Query != "" {
		t.Fatalf("conflicting request reached searcher: %+v", s.req)
	}

	legacy := &WebSearchTool{searcher: legacyOnlySearcher{}}
	got, err = legacy.Call(context.Background(), json.RawMessage(`{"query":"x","max_results":8}`), nil)
	if err != nil || got == nil || !got.IsError || !strings.Contains(got.Content, "advanced search parameters") {
		t.Fatalf("legacy searcher silently discarded max_results: %+v, %v", got, err)
	}
}

func TestWebSearchRejectsUnknownFieldsAndLegacyFiltering(t *testing.T) {
	s := &contractSearcher{}
	tool := NewWebSearchTool(s)
	for _, input := range []string{
		`{"query":"x","domains":["example.com"]}`,
		`{"query":"x","max_uses":999999}`,
	} {
		got, _ := tool.Call(context.Background(), json.RawMessage(input), nil)
		if got == nil || !got.IsError || !strings.Contains(got.Content, "unknown field") {
			t.Fatalf("unknown/server-only field accepted: %s -> %+v", input, got)
		}
	}
	legacy := &WebSearchTool{searcher: legacyOnlySearcher{}}
	got, _ := legacy.Call(context.Background(), json.RawMessage(`{"query":"x","allowed_domains":["example.com"]}`), nil)
	if got == nil || !got.IsError {
		t.Fatalf("legacy filter silently ignored: %+v", got)
	}
}

type legacyOnlySearcher struct{}

func (legacyOnlySearcher) Search(context.Context, string) (string, string, string, error) {
	return "title", "unfiltered", "", nil
}

func TestWebSearchRejectsTrailingJSON(t *testing.T) {
	tool := NewWebSearchTool(&contractSearcher{})
	for _, input := range []string{
		`{"query":"x"} {"query":"y"}`,
		`{"query":"x"} trailing`,
	} {
		got, err := tool.Call(context.Background(), json.RawMessage(input), nil)
		if err != nil || got == nil || !got.IsError {
			t.Fatalf("trailing JSON accepted: %q -> %+v, %v", input, got, err)
		}
	}
}

func TestWebSearchUnavailableIsError(t *testing.T) {
	tool := &WebSearchTool{}
	got, _ := tool.Call(context.Background(), json.RawMessage(`{"query":"x"}`), nil)
	if got == nil || !got.IsError {
		t.Fatalf("unconfigured search reported success: %+v", got)
	}
}
