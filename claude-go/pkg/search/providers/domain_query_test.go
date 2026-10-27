package providers

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/search"
)

func TestSearchQueryWithDomainHints(t *testing.T) {
	tests := []struct {
		name  string
		query string
		req   search.SearchRequest
		want  string
	}{
		{name: "unchanged without constraints", query: " docs ", want: "docs"},
		{
			name: "single allowed domain", query: "docs",
			req:  search.SearchRequest{AllowedDomains: []string{"kubernetes.io"}},
			want: "docs site:kubernetes.io",
		},
		{
			name: "allowed alternatives and blocked domains", query: "paper",
			req: search.SearchRequest{
				AllowedDomains: []string{"arxiv.org", "example.com"},
				BlockedDomains: []string{"example.com", "mirror.example"},
			},
			want: "paper (site:arxiv.org OR site:example.com) -site:example.com -site:mirror.example",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := searchQueryWithDomainHints(tt.query, tt.req); got != tt.want {
				t.Fatalf("query=%q, want %q", got, tt.want)
			}
		})
	}
}
