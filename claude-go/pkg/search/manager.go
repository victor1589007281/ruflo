package search

// manager.go —— 检索数据面的门面 (facade)。
//
// Manager 把 provider 集合、出口、缓存、单飞、配额与指标装在一起, 对上层
// 只暴露三个动作:
//   - Search: 规划 -> 选源 -> 对冲调度 -> RRF 融合 -> 质量门;
//   - Fetch: 抓取单页并抽正文 (见 fetch.go);
//   - Research: 不调用模型的有界研究, 只用逐字证据拼答案 (见 evidence.go)。
//
// 并发约束: 同一 Manager 可被多个 goroutine 同时调用; 相同查询会经 singleflight
// 合并, 只打一次源。Close 之后 Search/Fetch 返回 ErrUnavailable。
import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProviderStatus 是 provider 的运行状态快照, 供健康检查与运维面板使用。
type ProviderStatus struct {
	Name      string
	Tier      Tier
	Healthy   bool
	Egress    Mode
	Remaining int // -1 表示该源不报告配额
}

// ResearchOptions 是有界研究的预算。
//
// 零值表示使用默认: 1 轮、8 次搜索、6 次抓取、30 秒墙钟。
type ResearchOptions struct {
	MaxRounds   int
	MaxSearches int
	MaxFetches  int
	WallClock   time.Duration
}

// ResearchResult 是确定性研究的产出。
//
// Answer 只由逐字证据拼接, 不生成新事实; StopReason 取值
// budget | saturated | done | timeout。
type ResearchResult struct {
	Answer     string
	Evidence   []Evidence
	Results    []Result
	Rounds     int
	StopReason string
}

// Manager 编排检索、抓取与研究。
//
// 它持有 provider、LRU 缓存、单飞组、配额状态与指标; 同一 Manager
// 可被并发调用。Close 之后 Search/Fetch 返回 unavailable。
type Manager struct {
	cfg       Config
	egress    *Egress
	providers []Provider
	cache     *searchCache
	flights   *flightGroup
	quota     *quotaTracker
	metrics   *Metrics

	mu         sync.Mutex
	closed     bool
	ownsEgress bool
}

// NewManager 构造检索管理器。
//
// cfg 的零值字段会补成可用默认; eg 为 nil 时内部建一个纯直连出口
// (由 Manager.Close 负责释放)。providers 为 nil 时只提供缓存/指标等
// 空实现, Search 会返回 ErrNoProvider。
func NewManager(cfg Config, eg *Egress, providers []Provider) *Manager {
	if cfg.DefaultLimit <= 0 {
		cfg.DefaultLimit = 8
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "claude-go-search/1.0"
	}
	if cfg.SearchTimeout <= 0 {
		cfg.SearchTimeout = 4 * time.Second
	}
	if cfg.FetchTimeout <= 0 {
		cfg.FetchTimeout = 8 * time.Second
	}
	if cfg.HedgeDelay < 0 {
		cfg.HedgeDelay = 0
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 10 * time.Minute
	}
	if cfg.CacheFreshTTL <= 0 {
		cfg.CacheFreshTTL = 60 * time.Second
	}
	if cfg.CacheMaxEntries <= 0 {
		cfg.CacheMaxEntries = 5000
	}

	m := &Manager{
		cfg:     cfg,
		egress:  eg,
		cache:   newSearchCache(cfg.CacheMaxEntries),
		flights: &flightGroup{},
		quota:   newQuotaTracker(),
		metrics: NewMetrics(),
	}
	if m.egress == nil {
		fallback, err := NewEgress(EgressConfig{Mode: ModeDirect})
		if err == nil {
			m.egress = fallback
			m.ownsEgress = true
		}
	}
	for _, p := range providers {
		if p != nil {
			m.providers = append(m.providers, p)
		}
	}
	return m
}

// Metrics 返回指标登记器, 供调用方读取 Snapshot。
func (m *Manager) Metrics() *Metrics { return m.metrics }

// Search 执行一次多源检索。
//
// 行为:
//   - 空 query 返回 ErrInvalid;
//   - 按 MaxTier/健康/出口/能力位过滤源, 最多 2 源对冲;
//   - 单源失败只记 degraded, 不整体失败;
//   - 零结果且全部源失败返回 ErrNoProvider;
//   - 命中 LRU 或单飞等待时直接复用结果。
func (m *Manager) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, Wrap("", classifyError(err), err)
	}
	if m.isClosed() {
		return nil, Wrap("", ErrUnavailable, errors.New("search manager closed"))
	}
	if strings.TrimSpace(req.Query) == "" {
		return nil, Wrap("", ErrInvalid, errors.New("empty query"))
	}

	plan := PlanQuery(req.Query)
	if f := strings.TrimSpace(req.Freshness); f != "" {
		plan.Freshness = strings.ToLower(f)
	}

	candidates, degraded := selectProviders(m.providers, plan, m.cfg, m.egress, m.quota)
	tier := ""
	if len(candidates) > 0 {
		tier = candidates[0].Tier().String()
	}
	m.metrics.Inc("search_requests_total", map[string]string{
		"intent": string(plan.Intent),
		"lang":   plan.Language,
		"tier":   tier,
	})
	if len(candidates) == 0 {
		return nil, Wrap("", ErrNoProvider, errors.New("no search provider available"))
	}

	names := make([]string, 0, len(candidates))
	for _, p := range candidates {
		names = append(names, p.Name())
	}
	key := searchCacheKey(req.Query, names, req.Language, plan.Freshness)

	if cached, ok := m.cache.get(key); ok {
		cached.CacheHit = true
		cached.ElapsedMS = time.Since(start).Milliseconds()
		m.metrics.Inc("cache_hit_total", map[string]string{"cache_type": "search"})
		return &cached, nil
	}

	resp, err, _ := m.flights.Do(ctx, key, func() (SearchResponse, error) {
		// 等待期间可能已有别的请求填好缓存, 二次检查避免重复打源。
		if cached, ok := m.cache.get(key); ok {
			cached.CacheHit = false
			return cached, nil
		}
		return m.executeSearch(ctx, req, plan, candidates, degraded, key)
	})
	if err != nil {
		return nil, err
	}
	resp.CacheHit = false
	resp.ElapsedMS = time.Since(start).Milliseconds()
	return &resp, nil
}

// executeSearch 执行实际调度、融合与质量门, 并写入缓存。
func (m *Manager) executeSearch(ctx context.Context, req SearchRequest, plan QueryPlan, candidates []Provider, degraded []string, cacheKey string) (SearchResponse, error) {
	for _, p := range candidates {
		m.metrics.Inc("egress_requests_total", map[string]string{
			"provider": p.Name(),
			"egress":   string(providerPin(p, m.cfg, m.egress)),
		})
	}

	outcomes := m.scheduleSearch(ctx, req, candidates)
	if err := ctx.Err(); err != nil {
		return SearchResponse{}, Wrap("", classifyError(err), err)
	}

	var raw []Result
	var used []string
	anySuccess := false
	for _, o := range outcomes {
		if o.err != nil {
			degraded = append(degraded, degradedFor(o.name, o.err))
			continue
		}
		anySuccess = true
		used = append(used, o.name)
		raw = append(raw, o.results...)
	}
	for _, p := range candidates {
		m.metrics.Set("provider_quota_remaining", float64(m.quota.remaining(p)), map[string]string{
			"provider": p.Name(),
		})
	}

	if !anySuccess {
		return SearchResponse{}, Wrap("", ErrNoProvider, errors.New("all providers failed"))
	}
	if len(raw) == 0 {
		// 有源成功但确实零结果: 如实返回空结果, 不伪造。
		resp := SearchResponse{
			Providers: used,
			Degraded:  dedupeStrings(degraded),
			CostUSD:   0,
		}
		m.cache.put(cacheKey, resp, m.cacheTTL(plan, req))
		return resp, nil
	}

	fused := Fuse(raw, providerWeights(candidates, m.cfg))
	quality := EvaluateQuality(raw, req)
	needFresh := freshnessRequired(plan.Freshness)
	passed := quality.Pass(needFresh)
	m.metrics.Inc("search_gate_result", map[string]string{"result": quality.gateReason(needFresh)})

	resp := SearchResponse{
		Results:    fused,
		Providers:  used,
		Degraded:   dedupeStrings(degraded),
		CostUSD:    0, // 零成本约束: 任何情况下都不上报伪造成本
		GatePassed: passed,
		GateReason: quality.gateReason(needFresh),
	}
	m.cache.put(cacheKey, resp, m.cacheTTL(plan, req))
	return resp, nil
}

// cacheTTL 选择查询缓存 TTL。
//
// 新闻类或显式 freshness=day 的查询用较短 TTL, 其余用通用 TTL。
func (m *Manager) cacheTTL(plan QueryPlan, req SearchRequest) time.Duration {
	// 只有真正要求"天/周"级时效时才用短 TTL; 版本/年份类查询虽走新闻源排序,
	// 但内容不会 5 秒一变, 用短 TTL 只会白白打源。
	f := strings.ToLower(strings.TrimSpace(plan.Freshness))
	fresh := f == "day" || f == "week" ||
		strings.EqualFold(strings.TrimSpace(req.Freshness), "day") ||
		strings.EqualFold(strings.TrimSpace(req.Freshness), "week")
	if fresh && m.cfg.CacheFreshTTL > 0 {
		return m.cfg.CacheFreshTTL
	}
	if m.cfg.CacheTTL > 0 {
		return m.cfg.CacheTTL
	}
	return 10 * time.Minute
}

// Research 执行一次确定性研究 (不调用模型)。
//
// 流程: 规则拆 1–3 条子查询 -> 搜索 -> 按 RRF 顺序抓取前 N 条 ->
// 从正文抽取逐字证据 -> 拼接抽取式摘要。停止条件全部在代码里判定,
// StopReason 只会是 budget / saturated / done / timeout。
func (m *Manager) Research(ctx context.Context, question string, opts ResearchOptions) (*ResearchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(question) == "" {
		return nil, Wrap("", ErrInvalid, errors.New("empty question"))
	}
	if m.isClosed() {
		return nil, Wrap("", ErrUnavailable, errors.New("search manager closed"))
	}

	maxRounds := opts.MaxRounds
	if maxRounds <= 0 {
		maxRounds = 1
	}
	maxSearches := opts.MaxSearches
	if maxSearches <= 0 {
		maxSearches = 8
	}
	maxFetches := opts.MaxFetches
	if maxFetches <= 0 {
		maxFetches = 6
	}
	wall := opts.WallClock
	if wall <= 0 {
		wall = 30 * time.Second
	}

	deadline := time.Now().Add(wall)
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	plan := PlanQuery(question)
	subQueries := plan.SubQueries
	if len(subQueries) > 3 {
		subQueries = subQueries[:3]
	}
	weights := providerWeights(m.providers, m.cfg)

	var allResults []Result
	fetched := make(map[string]*Document)
	knownDomains := make(map[string]struct{})
	searchesLeft, fetchesLeft := maxSearches, maxFetches
	rounds := 0
	stop := "done"
	anySearchSuccess := false
	var lastErr error

loop:
	for rounds < maxRounds {
		if rctx.Err() != nil {
			stop = "timeout"
			break
		}
		rounds++

		var roundResults []Result
		searchBudget := false
		for _, sq := range subQueries {
			if searchesLeft <= 0 {
				stop = "budget"
				searchBudget = true
				break
			}
			if rctx.Err() != nil {
				stop = "timeout"
				break loop
			}
			searchesLeft--
			resp, err := m.Search(rctx, SearchRequest{
				Query:     sq,
				Language:  plan.Language,
				Freshness: plan.Freshness,
				Deadline:  deadline,
			})
			if err != nil {
				lastErr = err
				continue
			}
			anySearchSuccess = true
			roundResults = append(roundResults, resp.Results...)
			allResults = append(allResults, resp.Results...)
		}
		if rctx.Err() != nil {
			stop = "timeout"
			break
		}

		// 饱和判定: 后续轮次若没有新增独立域名, 再搜也不会变好。
		newDomains := 0
		for _, r := range roundResults {
			host := canonicalHost(r.URL)
			if host == "" {
				continue
			}
			if _, ok := knownDomains[host]; !ok {
				knownDomains[host] = struct{}{}
				newDomains++
			}
		}
		if rounds > 1 && newDomains == 0 {
			stop = "saturated"
			break
		}

		fused := Fuse(allResults, weights)
		for _, r := range fused {
			if fetchesLeft <= 0 {
				stop = "budget"
				break loop
			}
			key := CanonicalURL(r.URL)
			if _, ok := fetched[key]; ok {
				continue
			}
			if rctx.Err() != nil {
				stop = "timeout"
				break loop
			}
			fetchesLeft--
			fr, err := m.Fetch(rctx, FetchRequest{URL: r.URL, Deadline: deadline})
			if err != nil || fr == nil || fr.Document == nil {
				continue
			}
			fetched[key] = fr.Document
		}

		// 搜索预算已耗尽时仍把本轮已拿到的结果抓完, 但不再进入下一轮。
		if searchBudget {
			break
		}
		if rounds >= maxRounds {
			stop = "done"
			break
		}
	}
	if rctx.Err() != nil {
		stop = "timeout"
	}
	if !anySearchSuccess {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, Wrap("", ErrNoProvider, errors.New("all searches failed"))
	}

	fused := Fuse(allResults, weights)
	keywords := extractKeywords(question)
	ledger := NewEvidenceLedger()
	for _, r := range fused {
		doc := fetched[CanonicalURL(r.URL)]
		if doc == nil {
			continue
		}
		if _, ok := ledger.AddBest(doc, keywords); !ok {
			m.metrics.Inc("evidence_reject_total", map[string]string{"reason": "no_verbatim_sentence"})
		}
	}
	evidence := ledger.Items()

	return &ResearchResult{
		Answer:     BuildAnswer(evidence),
		Evidence:   evidence,
		Results:    fused,
		Rounds:     rounds,
		StopReason: stop,
	}, nil
}

// Close 关闭 Manager。
//
// provider 自行实现 Closer 时逐个关闭; 内部创建的兜底出口也会释放。
// 外部传入的 Egress 由调用方管理, 这里不越权关闭。重复调用安全。
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	providers := append([]Provider(nil), m.providers...)
	owns := m.ownsEgress
	egress := m.egress
	m.mu.Unlock()

	var errs []error
	for _, p := range providers {
		if c, ok := p.(Closer); ok {
			if err := c.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if owns && egress != nil {
		if err := egress.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ProviderStatus 返回所有 provider 的状态快照, 按 tier/名字排序。
func (m *Manager) ProviderStatus() []ProviderStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	providers := append([]Provider(nil), m.providers...)
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil
	}

	out := make([]ProviderStatus, 0, len(providers))
	for _, p := range providers {
		out = append(out, ProviderStatus{
			Name:      p.Name(),
			Tier:      p.Tier(),
			Healthy:   p.Healthy(),
			Egress:    providerPin(p, m.cfg, m.egress),
			Remaining: m.quota.remaining(p),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier < out[j].Tier
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// isClosed 判断 Manager 是否已关闭。
func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// dedupeStrings 去重并保持首次出现顺序。
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
