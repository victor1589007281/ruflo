package memory

import "github.com/ruflo/ruflo-go/api"

// QueryBuilder is a fluent helper for api.SearchOptions used with MemoryService.Search.
type QueryBuilder struct {
	namespace  string
	query      string
	tags       []string
	limit      int
	offset     int
	minScore   float64
	orderBy    string
	descending bool
	filters    map[string]string
}

// Query starts a new builder with defaults (limit 10, minScore 0).
func Query() *QueryBuilder {
	return &QueryBuilder{limit: 10, minScore: 0.0}
}

// NewQueryBuilder is an alias for Query for fluent construction.
func NewQueryBuilder() *QueryBuilder {
	return Query()
}

// DefaultMemoryQuery returns a builder for the default namespace with limit 50.
func DefaultMemoryQuery() *QueryBuilder {
	return Query().Namespace("default").Limit(50).Offset(0)
}

// Namespace sets the memory namespace filter.
func (q *QueryBuilder) Namespace(ns string) *QueryBuilder {
	q.namespace = ns
	return q
}

// Text sets the semantic search query text (use QueryText with Search).
func (q *QueryBuilder) Text(query string) *QueryBuilder {
	q.query = query
	return q
}

// QueryText returns the text set by Text for use as the first argument to Search.
func (q *QueryBuilder) QueryText() string {
	return q.query
}

// WithTags requires all listed tags on matching entries.
func (q *QueryBuilder) WithTags(tags ...string) *QueryBuilder {
	q.tags = append(q.tags, tags...)
	return q
}

// WithTag appends a single tag (alias for WithTags).
func (q *QueryBuilder) WithTag(tag string) *QueryBuilder {
	return q.WithTags(tag)
}

// Limit sets the maximum number of results (maps to SearchOptions.K).
func (q *QueryBuilder) Limit(n int) *QueryBuilder {
	q.limit = n
	return q
}

// Offset skips the first n results after filtering and ordering.
func (q *QueryBuilder) Offset(n int) *QueryBuilder {
	q.offset = n
	return q
}

// MinScore sets the minimum vector similarity score (0–1).
func (q *QueryBuilder) MinScore(s float64) *QueryBuilder {
	q.minScore = s
	return q
}

// OrderBy sets sort field (e.g. updated_at, created_at, score) and direction.
func (q *QueryBuilder) OrderBy(field string, desc bool) *QueryBuilder {
	q.orderBy = field
	q.descending = desc
	return q
}

// Filter adds a metadata equality constraint.
func (q *QueryBuilder) Filter(key, value string) *QueryBuilder {
	if q.filters == nil {
		q.filters = map[string]string{}
	}
	q.filters[key] = value
	return q
}

// Build produces api.SearchOptions for MemoryService.Search(query, opts).
func (q *QueryBuilder) Build() api.SearchOptions {
	k := q.limit
	if k <= 0 {
		k = 10
	}
	var filters map[string]string
	if len(q.filters) > 0 {
		filters = make(map[string]string, len(q.filters))
		for fk, fv := range q.filters {
			filters[fk] = fv
		}
	}
	var tags []string
	if len(q.tags) > 0 {
		tags = append(tags, q.tags...)
	}
	return api.SearchOptions{
		Namespace:  q.namespace,
		Tags:       tags,
		K:          k,
		MinScore:   q.minScore,
		Offset:     q.offset,
		OrderBy:    q.orderBy,
		Descending: q.descending,
		Filters:    filters,
	}
}

// QueryTemplates holds pre-built query patterns.
var QueryTemplates = struct {
	RecentPatterns func() *QueryBuilder
	AgentContext   func(agentID string) *QueryBuilder
	SimilarCode    func(query string) *QueryBuilder
	Recent         func(ns string) *QueryBuilder
	ByKey          func(ns, key string) *QueryBuilder
}{
	RecentPatterns: func() *QueryBuilder {
		return Query().Namespace("patterns").OrderBy("updated_at", true).Limit(20)
	},
	AgentContext: func(agentID string) *QueryBuilder {
		return Query().Filter("agent_id", agentID)
	},
	SimilarCode: func(query string) *QueryBuilder {
		return Query().Namespace("code").Text(query).MinScore(0.7)
	},
	Recent: func(ns string) *QueryBuilder {
		return Query().Namespace(ns).OrderBy("updated_at", true).Limit(25).Offset(0)
	},
	ByKey: func(ns, key string) *QueryBuilder {
		return Query().Namespace(ns).Filter("key", key).Limit(1).Offset(0)
	},
}
