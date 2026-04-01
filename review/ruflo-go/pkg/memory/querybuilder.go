// 查询构建器（memory 包）：Fluent API 链式调用组装 api.SearchOptions，与 MemoryService.Search 配合；
// Build 复制 map/slice 避免外部篡改；QueryTemplates 预置 patterns/代码相似度等常用查询形态。
package memory

import "github.com/ruflo/ruflo-go/api"

// QueryBuilder 累积检索条件，最终 Build 为 api.SearchOptions。
type QueryBuilder struct {
	namespace  string            // 命名空间过滤
	query      string            // 语义检索文本（传给 Search 的首参）
	tags       []string          // 要求条目包含的标签（AND）
	limit      int               // 对应 SearchOptions.K
	offset     int               // 分页偏移
	minScore   float64           // 最低相似度阈值
	orderBy    string            // 排序字段：updated_at/created_at/score 等
	descending bool              // 是否降序
	filters    map[string]string // 元数据等值过滤
}

// Query 新建构建器，默认 limit=10、minScore=0。
func Query() *QueryBuilder {
	return &QueryBuilder{limit: 10, minScore: 0.0}
}

// NewQueryBuilder 与 Query 等价，便于命名风格统一。
func NewQueryBuilder() *QueryBuilder {
	return Query()
}

// DefaultMemoryQuery 预置 namespace=default、limit=50。
func DefaultMemoryQuery() *QueryBuilder {
	return Query().Namespace("default").Limit(50).Offset(0)
}

// Namespace 设置命名空间并返回自身以链式调用。
func (q *QueryBuilder) Namespace(ns string) *QueryBuilder {
	q.namespace = ns
	return q
}

// Text 设置语义查询字符串；实际检索时与 QueryText 配合传入 Search。
func (q *QueryBuilder) Text(query string) *QueryBuilder {
	q.query = query
	return q
}

// QueryText 取出 Text 保存的查询串。
func (q *QueryBuilder) QueryText() string {
	return q.query
}

// WithTags 追加标签约束（条目须同时包含所给标签）。
func (q *QueryBuilder) WithTags(tags ...string) *QueryBuilder {
	q.tags = append(q.tags, tags...)
	return q
}

// WithTag 单标签便捷方法。
func (q *QueryBuilder) WithTag(tag string) *QueryBuilder {
	return q.WithTags(tag)
}

// Limit 设置返回条数上限 K。
func (q *QueryBuilder) Limit(n int) *QueryBuilder {
	q.limit = n
	return q
}

// Offset 在过滤与排序后跳过前 n 条。
func (q *QueryBuilder) Offset(n int) *QueryBuilder {
	q.offset = n
	return q
}

// MinScore 设置向量相似度下限（实现中为 1-距离）。
func (q *QueryBuilder) MinScore(s float64) *QueryBuilder {
	q.minScore = s
	return q
}

// OrderBy 指定排序字段与升降序，交由 Search 内 sort.SliceStable 解释。
func (q *QueryBuilder) OrderBy(field string, desc bool) *QueryBuilder {
	q.orderBy = field
	q.descending = desc
	return q
}

// Filter 增加一条元数据等值条件（懒创建 filters map）。
func (q *QueryBuilder) Filter(key, value string) *QueryBuilder {
	if q.filters == nil {
		q.filters = map[string]string{}
	}
	q.filters[key] = value
	return q
}

// Build 拷贝内部状态到值类型 SearchOptions，避免外部持有内部 map 引用。
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
