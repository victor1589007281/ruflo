package search

// fuse.go —— 多源结果融合 (RRF) 与 URL 规范化。
//
// 融合按 Reciprocal Rank Fusion: 每个源内部的名次贡献 1/(k+rank), 再按
// provider 权重加权。RRF 只使用名次, 不使用各源不可比的原始分数, 因此
// 不需要为每个 provider 做分数校准。CanonicalURL 先做去 fragment/utm/尾斜杠,
// 让同一资源在不同源里的写法落到同一个 key, 避免刷满结果表。
import (
	"net/url"
	"sort"
	"strings"
)

// rrfK 是 RRF 的平滑常数, 设计文档 §5.3 固定为 60。
const rrfK = 60.0

// CanonicalURL 规范化 URL, 用于跨源去重。
//
// 规则来自设计文档 §5.3: 去 fragment、去 utm_* 参数、host 小写、去尾斜杠。
// 其余 query 参数按名/值排序, 保证同一资源的不同书写顺序得到同一 key。
// 解析失败时退化为去空白的原串, 不丢结果。
func CanonicalURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""

	// 过滤已知追踪参数与空参数, 其余按键值排序。trk/fbclid 等参数只记录
	// 点击来源，不改变正文；保留它们会把同一篇论文或报道重复塞进结果表。
	if u.RawQuery != "" {
		q := u.Query()
		for key := range q {
			if isTrackingQueryKey(key) || isRedundantArxivFile(u, key, q[key]) {
				q.Del(key)
			}
		}
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			vals := q[k]
			if len(vals) == 0 {
				continue
			}
			sort.Strings(vals)
			for _, v := range vals {
				parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
			}
		}
		u.RawQuery = strings.Join(parts, "&")
	}

	// 去尾斜杠, 根路径统一为空。
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" {
		u.Path = ""
	}
	return u.String()
}

// isTrackingQueryKey 只匹配明确的点击/会话追踪键，不按模糊子串删除业务参数。
func isTrackingQueryKey(key string) bool {
	key = strings.ToLower(key)
	if strings.HasPrefix(key, "utm_") {
		return true
	}
	switch key {
	case "trk", "trkparms", "fbclid", "gclid", "dclid", "msclkid", "mc_cid", "mc_eid", "igshid", "spm", "scm":
		return true
	default:
		return false
	}
}

// isRedundantArxivFile 只删除 arXiv abstract 页中与路径 ID 完全相同的 file 值。
// 其他站点、其他 file 值和 download 等参数都保留，避免把不同业务资源错误合并。
func isRedundantArxivFile(u *url.URL, key string, values []string) bool {
	if !strings.EqualFold(key, "file") || len(values) != 1 {
		return false
	}
	host := u.Hostname()
	if host != "arxiv.org" && host != "www.arxiv.org" {
		return false
	}
	id := strings.TrimPrefix(u.Path, "/abs/")
	return id != u.Path && id != "" && values[0] == id
}

// Fuse 用加权 RRF 融合多源结果。
//
// 公式: score(d) = Σ_p weight(p) / (rrfK + rank_p(d))。同一 provider 内
// 重复 canonical URL 只计一次; 跨 provider 的同一 URL 累加分数。
// weights 以 provider 名为键, 缺失或非正数时按 1.0 处理。
// 返回结果按分数降序、best rank 升序、URL 升序稳定排序, Rank 重写为
// 融合后的名次 (1 起), Score 重写为 RRF 分数。
func Fuse(results []Result, weights map[string]float64) []Result {
	type aggregate struct {
		best      Result
		bestRank  int
		score     float64
		providers map[string]struct{}
	}

	merged := make(map[string]*aggregate)
	seenPerProvider := make(map[string]map[string]struct{})

	for _, r := range results {
		canon := CanonicalURL(r.URL)
		if canon == "" {
			continue
		}
		provider := r.Provider
		seen := seenPerProvider[provider]
		if seen == nil {
			seen = make(map[string]struct{})
			seenPerProvider[provider] = seen
		}
		if _, dup := seen[canon]; dup {
			continue // 同一 provider 内重复 URL 只计一次
		}
		seen[canon] = struct{}{}

		rank := r.Rank
		if rank <= 0 {
			rank = 1
		}
		w := weights[provider]
		if w <= 0 {
			w = 1.0
		}

		agg := merged[canon]
		if agg == nil {
			agg = &aggregate{best: r, bestRank: rank, providers: make(map[string]struct{})}
			merged[canon] = agg
		}
		agg.score += w / (rrfK + float64(rank))
		agg.providers[provider] = struct{}{}
		if rank < agg.bestRank {
			agg.bestRank = rank
			agg.best = r
		}
	}

	out := make([]Result, 0, len(merged))
	for canon, agg := range merged {
		r := agg.best
		r.URL = canon
		r.Score = agg.score
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		return out[i].URL < out[j].URL
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// providerWeights 生成 Fuse 所需的 provider 权重表。
//
// 优先级: provider 自实现的 Weighted 接口 > Config.Providers 的 Weight > 1.0。
// 非正权重按 1.0 处理, 避免某个源被配置成 0 后彻底消失。
func providerWeights(providers []Provider, cfg Config) map[string]float64 {
	out := make(map[string]float64, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		var w float64
		if x, ok := p.(Weighted); ok {
			w = x.Weight()
		}
		if w <= 0 {
			if pc, ok := cfg.Providers[p.Name()]; ok {
				w = pc.Weight
			}
		}
		if w <= 0 {
			w = 1.0
		}
		out[p.Name()] = w
	}
	return out
}
