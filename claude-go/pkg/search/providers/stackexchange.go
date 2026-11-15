package providers

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// stackExchangeProvider 通过 StackExchange API 搜索问答。
type stackExchangeProvider struct {
	*base
	endpoint string
	apiKey   string
}

// newStackExchange 构造 StackExchange 源 (直连, L2)。
func newStackExchange(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://api.stackexchange.com/2.3/search/advanced"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("stackexchange", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	return &stackExchangeProvider{base: b, endpoint: endpoint, apiKey: strings.TrimSpace(pc.APIKey)}, nil
}

// stackExchangeSite 是一个可检索的 Stack Exchange 子站。
//
// 只选 1 个专业站与 Stack Overflow 一起查: 匿名 API 的日配额是按 IP 共享的,
// 每次检索打 5 个站会很快耗尽配额; 而两站已能覆盖"通用报错 + 领域问题"。
type stackExchangeSite struct {
	site    string
	host    string
	signals []string
}

// stackExchangeSites 是检索站清单; 顺序决定信号同分时的优先站。
var stackExchangeSites = []stackExchangeSite{
	{site: "stackoverflow", host: "stackoverflow.com"},
	{site: "dba", host: "dba.stackexchange.com", signals: []string{
		"sql", "database", "postgres", "postgresql", "mysql", "mariadb", "sqlite",
		"oracle db", "sql server", "mongodb", "redis", "elasticsearch", "clickhouse",
		"query plan", "execution plan", "deadlock", "transaction", "replication",
		"数据库", "索引", "事务", "死锁", "主从", "分库分表",
	}},
	{site: "unix", host: "unix.stackexchange.com", signals: []string{
		"bash", "shell", "linux", "unix", "cron", "systemd", "permission", "chmod",
		"sed", "awk", "grep", "命令行", "文件权限", "权限", "定时任务",
	}},
	{site: "serverfault", host: "serverfault.com", signals: []string{
		"nginx", "apache", "iis", "nginx.conf", "windows server", "vmware", "raid",
		"backup", "ssl certificate", "证书", "服务器", "运维", "负载均衡",
	}},
	{site: "networkengineering", host: "networkengineering.stackexchange.com", signals: []string{
		"tcp", "udp", "bgp", "vlan", "ospf", "subnet", "firewall", "iptables",
		"routing", "route", "dns", "nat", "cidr", "mtu", "交换机", "路由", "防火墙",
	}},
}

// Search 调用 /2.3/search/advanced。
//
// 默认查 Stack Overflow + 一个按 query 信号选出的专业站; 调用方显式给了
// allowed_domains 时只查其中命中的 SE 子站 (一个都没命中则直接返回空, 不
// 绕过调用方的域名约束)。
func (p *stackExchangeProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	limit := p.limitOf(req.Limit)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	sites := selectStackExchangeSites(query, req.AllowedDomains)
	if len(sites) == 0 {
		return nil, nil
	}

	results := make([]search.Result, 0, limit*len(sites))
	var firstErr error
	for _, site := range sites {
		items, err := p.searchSite(ctx, site, query, limit)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results = append(results, items...)
	}
	// 部分站失败时保留已拿到的结果; 全部失败才把错误交给调度器熔断。
	if len(results) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return finishResults(p.name, req, dedupeResults(results), limit), nil
}

// searchSite 查询单个 SE 子站。
func (p *stackExchangeProvider) searchSite(ctx context.Context, site stackExchangeSite, query string, limit int) ([]search.Result, error) {
	params := url.Values{
		"order":    {"desc"},
		"sort":     {"relevance"},
		"q":        {query},
		"site":     {site.site},
		"pagesize": {strconv.Itoa(limit)},
		"filter":   {"default"},
	}
	if p.apiKey != "" {
		params.Set("key", p.apiKey)
	}
	target := withQuery(p.endpoint, params)

	var payload struct {
		Items []struct {
			Title        string   `json:"title"`
			Link         string   `json:"link"`
			Body         string   `json:"body"`
			Score        int      `json:"score"`
			CreationDate int64    `json:"creation_date"`
			Tags         []string `json:"tags"`
		} `json:"items"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, target, http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Items))
	for _, it := range payload.Items {
		link := strings.TrimSpace(it.Link)
		if !isHTTPURL(link) {
			continue
		}
		snippet := snippetText(it.Body, maxSnippetRunes)
		if len(it.Tags) > 0 {
			tags := "[" + strings.Join(it.Tags, ", ") + "]"
			snippet = strings.TrimSpace(tags + " " + snippet)
		}
		var published time.Time
		if it.CreationDate > 0 {
			published = time.Unix(it.CreationDate, 0).UTC()
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     snippetText(html.UnescapeString(it.Title), 200),
			Snippet:   snippet,
			Score:     float64(it.Score),
			Published: published,
			Lang:      "en",
		})
	}
	return results, nil
}

// selectStackExchangeSites 选出本次要查的 2 个站。
//
// 显式 allowed_domains 优先: 这是调用方更强的约束, 不能因为"技术问题默认查
// SO"就绕过它。未显式约束时, SO 常驻, 第二个名额给信号命中数最高的专业站;
// 都没有信号时只查 SO。
func selectStackExchangeSites(query string, allowed []string) []stackExchangeSite {
	if len(allowed) > 0 {
		var out []stackExchangeSite
		for _, site := range stackExchangeSites {
			for _, domain := range allowed {
				if hostMatchesDomain(site.host, domain) {
					out = append(out, site)
					break
				}
			}
		}
		return out
	}

	lower := strings.ToLower(query)
	best := -1
	bestScore := 0
	for i, site := range stackExchangeSites {
		if site.site == "stackoverflow" {
			continue
		}
		score := 0
		for _, sig := range site.signals {
			if strings.Contains(lower, strings.ToLower(sig)) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}

	out := make([]stackExchangeSite, 0, 2)
	out = append(out, stackExchangeSites[0])
	if best >= 0 {
		out = append(out, stackExchangeSites[best])
	}
	return out
}

// hostMatchesDomain 判断 host 是否落在 domain 内 (含子域)。
func hostMatchesDomain(host, domain string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// dedupeResults 按 URL 去重, 保留先出现的条目 (SO 在专业站之前)。
func dedupeResults(in []search.Result) []search.Result {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, r := range in {
		key := strings.ToLower(strings.TrimSpace(r.URL))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}
