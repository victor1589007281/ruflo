package memory

import (
	"strings"
	"testing"
)

func TestQueryBuilder_Fluent(t *testing.T) {
	t.Parallel()
	q := NewQueryBuilder().Namespace("ns1").Text("hello").Limit(10).Offset(2).WithTag("t1").MinScore(0.3)
	opts := q.Build()
	if opts.Namespace != "ns1" || opts.K != 10 || opts.Offset != 2 || len(opts.Tags) != 1 || opts.Tags[0] != "t1" {
		t.Fatalf("build: %+v", opts)
	}
	if q.QueryText() != "hello" {
		t.Fatalf("query text %q", q.QueryText())
	}
	if opts.MinScore != 0.3 {
		t.Fatalf("min score %v", opts.MinScore)
	}
}

func TestQueryTemplates(t *testing.T) {
	t.Parallel()
	r := QueryTemplates.RecentPatterns().Build()
	if r.Namespace != "patterns" || r.K != 20 || !r.Descending || r.OrderBy != "updated_at" {
		t.Fatalf("recent patterns: %+v", r)
	}
	ac := QueryTemplates.AgentContext("agent-1").Build()
	if ac.Filters["agent_id"] != "agent-1" {
		t.Fatalf("agent context: %+v", ac)
	}
	qb := QueryTemplates.SimilarCode("fn main")
	sc := qb.Build()
	if sc.Namespace != "code" || sc.MinScore != 0.7 || qb.QueryText() != "fn main" {
		t.Fatalf("similar code: %+v text=%q", sc, qb.QueryText())
	}
	rr := QueryTemplates.Recent("collab").Build()
	if rr.Namespace != "collab" || rr.K != 25 {
		t.Fatalf("Recent(ns): %+v", rr)
	}
	bk := QueryTemplates.ByKey("n", "mykey").Build()
	if bk.Namespace != "n" || bk.Filters["key"] != "mykey" || bk.K != 1 {
		t.Fatalf("ByKey: %+v", bk)
	}
}

func TestQueryBuilder_Defaults(t *testing.T) {
	t.Parallel()
	opts := DefaultMemoryQuery().Build()
	if opts.Namespace != "default" || opts.K != 50 {
		t.Fatalf("defaults %+v", opts)
	}
	if !strings.Contains(opts.Namespace, "default") {
		t.Fatal(opts.Namespace)
	}
}
