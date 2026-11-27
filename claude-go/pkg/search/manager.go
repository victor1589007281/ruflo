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
	cfg         Config
	egress      *Egress
	providers   []Provider
	cache       *searchCache
	flights     *flightGroup
	fetchCache  *documentCache
	fetchFlight *fetchFlightGroup
	quota       *quotaTracker
	metrics     *Metrics
	shared      *sharedStore
	budget      *sharedBudget
	renderer    JSRenderer // nil = 按本次实际 HTTP 出口创建受控 Chromium 渲染器

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
	if cfg.RedisKeyPrefix == "" {
		cfg.RedisKeyPrefix = defaultRedisKeyPrefix
	}
	if cfg.RedisTimeout <= 0 {
		cfg.RedisTimeout = defaultRedisTimeout
	}
	if cfg.RedisStaleTTL <= 0 {
		cfg.RedisStaleTTL = defaultRedisStaleTTL
	}
	if cfg.SharedBudgetWindow <= 0 {
		cfg.SharedBudgetWindow = defaultSharedBudgetWindow
	}
	if cfg.SharedBudgetReservationTTL <= 0 {
		cfg.SharedBudgetReservationTTL = defaultSharedBudgetReservationTTL
	}

	m := &Manager{
		cfg:         cfg,
		egress:      eg,
		cache:       newSearchCache(cfg.CacheMaxEntries),
		flights:     &flightGroup{},
		fetchCache:  newDocumentCache(cfg.CacheMaxEntries),
		fetchFlight: &fetchFlightGroup{},
		quota:       newQuotaTracker(),
		metrics:     NewMetrics(),
	}
	m.shared = newSharedStore(cfg)
	m.budget = newSharedBudget(cfg, m.shared)
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
	var err error
	req, err = normalizeSearchRequest(req, m.cfg.DefaultLimit)
	if err != nil {
		return nil, err
	}

	plan := PlanQuery(req.Query)
	// 生态品牌词推导的域名限定 (微信/头条生态文章): 调用方显式传了
	// allowed_domains 时以调用方为准, 否则套用规划结果。providers 会把它
	// 翻成 site: 提示改善召回, 融合前仍由 Manager 对返回 URL 做硬过滤,
	// 单域名结果自动豁免域名多样性门 (见 gate.go DomainScoped)。
	if len(req.AllowedDomains) == 0 && len(plan.ScopedDomains) > 0 {
		req.AllowedDomains = plan.ScopedDomains
	}
	if f := strings.TrimSpace(req.Freshness); f != "" {
		plan.Freshness = strings.ToLower(f)
	} else {
		// 推断时效也必须进入 provider 请求和 EvaluateQuality，不能只影响选源。
		req.Freshness = plan.Freshness
	}
	if language := normalizeLang(req.Language); language != "" {
		plan.Language = language
	} else {
		req.Language = plan.Language
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
		err := Wrap("", ErrNoProvider, errors.New("no search provider available"))
		m.recordSearch(plan, req, nil, err, time.Since(start), nil)
		return nil, err
	}

	names := make([]string, 0, len(candidates))
	for _, p := range candidates {
		names = append(names, p.Name())
	}
	key := searchCacheKey(req, names, plan.Freshness)

	var cacheDegraded []string
	cached, ok, currentDegraded := m.cachedSearch(ctx, key)
	if ok {
		cached.CacheHit = true
		cached.ElapsedMS = time.Since(start).Milliseconds()
		m.recordSearch(plan, req, &cached, nil, time.Since(start), nil)
		return m.widenIfFreshFailed(ctx, req, plan, &cached, start)
	}
	cacheDegraded = append(cacheDegraded, currentDegraded...)

	resp, err, _ := m.flights.Do(ctx, key, func() (SearchResponse, error) {
		// 等待期间可能已有别的 Manager 填好缓存，二次检查避免重复打源/预占。
		cached, ok, sharedDegraded := m.cachedSearch(ctx, key)
		cacheDegraded = append(cacheDegraded, sharedDegraded...)
		if ok {
			cached.CacheHit = true
			return cached, nil
		}
		return m.executeSearch(ctx, req, plan, candidates, degraded, key)
	})
	if err != nil {
		return nil, err
	}
	resp = cloneSearchResponse(resp)
	resp.Degraded = dedupeStrings(append(resp.Degraded, cacheDegraded...))
	resp.ElapsedMS = time.Since(start).Milliseconds()
	return m.widenIfFreshFailed(ctx, req, plan, &resp, start)
}

// widenIfFreshFailed 在 fresh 门失败时自动把窗口放宽一档 (day→week→month)
// 重检一次, 返回放宽后的响应并标注 FreshnessWidened。
//
// 为什么必须自动化: "fresh → 放宽一档重试" 先写进静态规则、又加进错误现场
// 的 gate_hint, 弱模型 (deepseek-v4.1-flash) 的遵从仍是概率性的 —— 2026-11-27
// 实测三份日报 26 次检索 14 次挂 fresh, 大半是论文查询误用 week。模型本来就
// 会在约一半的情况下手动换 month 重搜, 自动化后 provider 成本基本持平, 但
// 红灯与行为不确定性消失。month 不触发 fresh 门 (freshnessRequired 只认
// day/week), 所以递归深度天然有界: day 最多 day→week→month 两级。
func (m *Manager) widenIfFreshFailed(ctx context.Context, req SearchRequest, plan QueryPlan, resp *SearchResponse, start time.Time) (*SearchResponse, error) {
	if resp == nil || resp.GateReason != "fresh" || !freshnessRequired(plan.Freshness) {
		return resp, nil
	}
	widened := widenFreshness(plan.Freshness)
	m.metrics.Inc("search_fresh_widened_total", map[string]string{"from": plan.Freshness, "to": widened})
	req2 := req
	req2.Freshness = widened
	req2.widenedFrom = plan.Freshness
	resp2, err := m.Search(ctx, req2)
	if err != nil || resp2 == nil {
		// 放宽重检本身失败 (网络/配额): 不丢原始结果, 返回带 gate_hint 的原响应。
		return resp, nil
	}
	// 链式放宽 (day→week 又失败→month) 时把链路拼全: "day→week→month"。
	if resp2.FreshnessWidened != "" {
		resp2.FreshnessWidened = plan.Freshness + "→" + resp2.FreshnessWidened
	} else {
		resp2.FreshnessWidened = plan.Freshness + "→" + widened
	}
	// 两轮的成本与降级都是本次调用的真实开销, 合并进返回响应 (遥测里两轮
	// 各自有独立记录, 靠 widenedFrom 关联)。
	resp2.CostUSD += resp.CostUSD
	resp2.Degraded = dedupeStrings(append(resp2.Degraded, resp.Degraded...))
	resp2.ElapsedMS = time.Since(start).Milliseconds()
	return resp2, nil
}

// widenFreshness 返回下一档更宽的时效窗口; fresh 门只对 day/week 生效,
// 所以只需处理这两档。
func widenFreshness(freshness string) string {
	if strings.EqualFold(strings.TrimSpace(freshness), "day") {
		return "week"
	}
	return "month"
}

// executeSearch 执行实际调度、融合与质量门, 并写入缓存。
// 单次调用最多打两个源；质量不足通过响应显式暴露，由上层在自己的预算内重试。
func (m *Manager) executeSearch(ctx context.Context, req SearchRequest, plan QueryPlan, candidates []Provider, degraded []string, cacheKey string) (SearchResponse, error) {
	start := time.Now()
	perProvider := map[string]int64{}
	scheduled := candidates
	if len(scheduled) > maxSourceConcurrency {
		scheduled = scheduled[:maxSourceConcurrency]
	}
	for _, p := range scheduled {
		m.metrics.Inc("egress_requests_total", map[string]string{
			"provider": p.Name(),
			"egress":   string(providerPin(p, m.cfg, m.egress)),
		})
	}

	providerReq := req
	if len(req.AllowedDomains) > 0 || len(req.BlockedDomains) > 0 {
		// 小 limit 不能让 provider 只返回少量域外候选后就被硬过滤为空。
		// 适度多取仍在同一次有界请求内完成，最终 Top-K 继续使用原始 limit。
		const domainFilterMinResults = 10
		if providerReq.Limit < domainFilterMinResults {
			providerReq.Limit = domainFilterMinResults
		}
	}
	outcomes := m.scheduleSearch(ctx, providerReq, scheduled)
	if err := ctx.Err(); err != nil {
		return SearchResponse{}, Wrap("", classifyError(err), err)
	}

	var raw []Result
	var used []string
	var executionDegraded []string
	var actualCost float64
	anySuccess := false
	for _, o := range outcomes {
		perProvider[o.name] = o.elapsed.Milliseconds()
		if o.err != nil {
			failure := degradedFor(o.name, o.err)
			degraded = append(degraded, failure)
			executionDegraded = append(executionDegraded, failure)
			continue
		}
		anySuccess = true
		actualCost += o.cost
		used = append(used, o.name)
		raw = append(raw, o.results...)
	}
	for _, p := range scheduled {
		m.metrics.Set("provider_quota_remaining", float64(m.quota.remaining(p)), map[string]string{
			"provider": p.Name(),
		})
	}
	needFresh := freshnessRequired(plan.Freshness)

	if !anySuccess {
		// 只回 "all providers failed" 会让上层无法区分「网络整体不可达」
		// 与「选中的两个源恰好都超时」。把逐源原因带出去，调用方才能决定
		// 是换源、重试还是如实告诉用户检索不可用。
		detail := strings.Join(dedupeStrings(degraded), "; ")
		if detail == "" {
			detail = "no provider returned a response"
		}
		errAll := Wrap("", ErrNoProvider, errors.New("all providers failed: "+detail))
		m.recordSearch(plan, req, nil, errAll, time.Since(start), perProvider)
		return SearchResponse{}, errAll
	}
	filteredRaw := filterResultDomains(raw, req)
	degraded = dedupeStrings(degraded)
	if len(filteredRaw) == 0 {
		// 有源成功但零结果 (含域名过滤后为空): 如实返回 empty, 不伪造也不回退。
		reason := "empty"
		if len(raw) > 0 && (len(req.AllowedDomains) > 0 || len(req.BlockedDomains) > 0) {
			reason = "domain_filter"
		}
		resp := SearchResponse{
			Status:     "empty",
			Results:    []Result{},
			Providers:  used,
			Degraded:   degraded,
			CostUSD:    actualCost,
			GateReason: reason,
		}
		resp = m.putSearchCache(ctx, cacheKey, resp, m.cacheTTL(plan, req))
		m.recordSearch(plan, req, &resp, nil, time.Since(start), perProvider)
		return resp, nil
	}

	fused := Fuse(filteredRaw, providerWeights(candidates, m.cfg))
	fused = rerankResults(fused, req)
	if len(fused) > req.Limit {
		fused = fused[:req.Limit]
	}
	quality := EvaluateQuality(filteredRaw, req)
	passed := quality.Pass(needFresh)
	m.metrics.Inc("search_gate_result", map[string]string{"result": quality.gateReason(needFresh)})

	resp := SearchResponse{
		// 未调度源的退避仍留在 Degraded 供运维归因, 但只有本次实际打源
		// 失败才把可用结果标成 partial; 缓存故障也不改变数据源自身状态。
		Status:       responseStatus(len(fused), executionDegraded),
		Results:      fused,
		Providers:    used,
		Degraded:     degraded,
		CostUSD:      actualCost,
		GatePassed:   passed,
		GateReason:   quality.gateReason(needFresh),
		DatedResults: quality.DatedResults,
	}
	// 时效门失败时在错误现场给模型可执行的下一步 (见 freshGateHint 注释)。
	if resp.GateReason == "fresh" {
		resp.GateHint = freshGateHint(quality, plan.Freshness, time.Now())
	}
	resp = m.putSearchCache(ctx, cacheKey, resp, m.cacheTTL(plan, req))
	m.recordSearch(plan, req, &resp, nil, time.Since(start), perProvider)
	return resp, nil
}

// recordSearch 写一条检索使用记录。
//
// 记录是**纯增强**: 这里不返回错误、不改响应, 写盘失败只在 telemetry 里报一次。
// 面板的"优化方向"全部来自这些逐条现场 (缓存命中/门原因/慢源/零结果查询)。
func (m *Manager) recordSearch(plan QueryPlan, req SearchRequest, resp *SearchResponse, err error, latency time.Duration, perProvider map[string]int64) {
	rec := Record{
		Kind:        "search",
		Query:       req.Query,
		Intent:      string(plan.Intent),
		Lang:        plan.Language,
		Freshness:   plan.Freshness,
		WidenedFrom: req.widenedFrom,
		LatencyMS:   latency.Milliseconds(),
		ProviderMS:  perProvider,
	}
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
	} else if resp != nil {
		rec.Status = resp.Status
		rec.CacheHit = resp.CacheHit
		rec.GatePassed = resp.GatePassed
		rec.GateReason = resp.GateReason
		rec.Dated = resp.DatedResults
		rec.Providers = resp.Providers
		rec.Failed = resp.Degraded
		rec.Results = len(resp.Results)
		rec.CostUSD = resp.CostUSD
		rec.TopDomains = topDomains(resp.Results, 3)
		rec.Domains = countDomains(resp.Results)
	}
	recordUsage(rec)
}

// recordFetch 写一条抓取使用记录 (成功/失败/缓存命中都算一次使用)。
func (m *Manager) recordFetch(req FetchRequest, resp *FetchResponse, err error, latency time.Duration) {
	rec := Record{Kind: "fetch", URL: req.URL, LatencyMS: latency.Milliseconds()}
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
	} else if resp != nil && resp.Document != nil {
		rec.Status = "success"
		rec.CacheHit = resp.Document.CacheHit
		rec.Bytes = resp.Document.TotalChars
		rec.Results = len(resp.Document.Evidence)
		rec.Failed = resp.Degraded
		rec.Lang = resp.Document.Via
	} else if resp != nil {
		rec.Status = "empty"
		rec.Failed = resp.Degraded
	}
	recordUsage(rec)
}

// recordResearch 写一条研究使用记录 (编排层: 轮数/停止原因/证据数)。
func (m *Manager) recordResearch(question string, res *ResearchResult, err error, latency time.Duration) {
	rec := Record{Kind: "research", Query: question, LatencyMS: latency.Milliseconds()}
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
	} else if res != nil {
		rec.Status = "success"
		rec.StopReason = res.StopReason
		rec.Rounds = res.Rounds
		rec.Results = len(res.Results)
		rec.Evidence = len(res.Evidence)
	}
	recordUsage(rec)
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
func (m *Manager) Research(ctx context.Context, question string, opts ResearchOptions) (out *ResearchResult, err error) {
	start := time.Now()
	// 研究是"多次检索+抓取"的编排, 它自己的记录只保留编排层事实
	// (轮数/停止原因/证据数); 每条子查询/抓取另有自己的记录。
	defer func() { m.recordResearch(question, out, err, time.Since(start)) }()
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
				// 研究问题命中生态品牌词时, 整条研究的子查询都限定到该生态,
				// 即使子查询切分后丢了品牌词 (调用方未显式指定时 Manager 才会套用)。
				AllowedDomains: plan.ScopedDomains,
				Deadline:       deadline,
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
// provider 自行实现 Closer 时逐个关闭; 内部创建的兜底出口和本 Manager 持有的
// Redis 连接也会释放。外部传入的 Egress 由调用方管理, 这里不越权关闭。重复调用安全。
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
	shared := m.shared
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
	if shared != nil {
		if err := shared.Close(); err != nil {
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
