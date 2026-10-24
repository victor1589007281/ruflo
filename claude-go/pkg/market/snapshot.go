package market

import (
	"context"
	"fmt"
	"math"
	"time"
)

// 本文件实现时点快照。三条硬约束:
//
//  1. 前视过滤在代码里做, 不靠调用方或 prompt:
//     fact.AsOf > as_of 或 bar.Date > as_of 一律丢弃并计入 Warnings;
//  2. 非交易日回退到最近交易日, 并在 Warnings 里写明实际日期;
//  3. 同一能力的多个源并行取数, 总预算 (Deadline) 内拿多少算多少;
//     超时返回已取得的部分并标记 Partial, 不编造缺失值。
//
// 快照从不推断、不填默认值: 拿不到的字段留空, 能力缺失写入 Missing。

// defaultSnapshotFields 是未显式指定 Fields 时抓取的能力集合。
var defaultSnapshotFields = []Capability{CapQuote, CapHistory, CapFundamental, CapFiling}

// Snapshot 生成时点快照。
func (r *Registry) Snapshot(ctx context.Context, input string, opts SnapshotOptions) (*Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	inst, err := r.Resolve(ctx, input)
	if err != nil {
		return nil, err
	}

	requestedAsOf := opts.AsOf
	if requestedAsOf.IsZero() {
		requestedAsOf = time.Now()
	}

	snap := &Snapshot{Inst: inst, AsOf: requestedAsOf}
	filterAsOf := requestedAsOf // 前视过滤始终对用户请求的时点, 而不是回退后的日期
	if isCalendarMarket(inst.Market) && !IsTradingDay(inst.Market, requestedAsOf) {
		last := LastTradingDay(inst.Market, requestedAsOf)
		snap.AsOf = last
		snap.Stale = true
		snap.Warnings = append(snap.Warnings, fmt.Sprintf(
			"as_of %s 是非交易日, 已回退到最近交易日 %s; 数据时点以回退日为准",
			requestedAsOf.In(marketLocation(inst.Market)).Format("2006-01-02"),
			last.Format("2006-01-02")))
	}

	fields := dedupeCaps(opts.Fields)
	if len(fields) == 0 {
		fields = append(fields, defaultSnapshotFields...)
	}

	deadline := opts.Deadline
	if deadline <= 0 {
		deadline = r.cfg.SnapshotDeadline
	}
	workCtx := ctx
	var cancel context.CancelFunc
	if deadline > 0 {
		workCtx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	// 各能力组并行: 单源超时 1.5s, 组与组之间不串行累加。
	ch := make(chan part, len(fields))
	for _, capability := range fields {
		capability := capability
		go func() {
			ch <- r.snapshotPart(workCtx, inst, capability, opts, filterAsOf)
		}()
	}

	parts := make(map[Capability]part, len(fields))
	budgetHit := false
collectLoop:
	for i := 0; i < len(fields); i++ {
		// 优先收结果: 避免截止时刻与结果同时就绪时误判为超时。
		select {
		case p := <-ch:
			parts[p.cap] = p
			continue
		default:
		}
		select {
		case p := <-ch:
			parts[p.cap] = p
		case <-workCtx.Done():
			budgetHit = true
		}
		if budgetHit {
			// 把已经就绪的结果收走; 未完成的组不再等待。
			for {
				select {
				case p := <-ch:
					parts[p.cap] = p
				default:
					break collectLoop
				}
			}
		}
	}

	// 按请求顺序汇总, 保证输出稳定。
	seenSource := map[string]bool{}
	for _, capability := range fields {
		p, ok := parts[capability]
		if !ok {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("能力 %s 在预算内未完成, 已返回部分数据", capability))
			snap.Partial = true
			continue
		}
		snap.Warnings = append(snap.Warnings, p.warnings...)
		for _, s := range p.sources {
			if s == "" || seenSource[s] {
				continue
			}
			seenSource[s] = true
			snap.Sources = append(snap.Sources, s)
		}
		if p.missing {
			snap.Missing = append(snap.Missing, string(capability))
			snap.Partial = true
		}
		switch capability {
		case CapQuote:
			snap.Quote = p.quote
		case CapHistory:
			snap.Bars = p.bars
		case CapFundamental:
			snap.Facts = p.facts
		case CapFiling:
			snap.Filings = p.filings
		case CapNews:
			snap.News = p.news
		}
	}

	if budgetHit || workCtx.Err() != nil {
		snap.Partial = true
		if deadline > 0 {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("快照预算 %s 耗尽, 已返回超时前取得的部分数据", deadline))
		} else {
			snap.Warnings = append(snap.Warnings, "快照上下文已结束, 已返回超时前取得的部分数据")
		}
	}
	if len(snap.Missing) > 0 {
		snap.Partial = true
	}

	// Stale: 回退日之外, 若报价时间早于快照日, 说明拿到的是上一交易日的值。
	if !snap.Stale && snap.Quote != nil && !snap.Quote.AsOf.IsZero() && isCalendarMarket(inst.Market) {
		quoteDay := snap.Quote.AsOf.In(marketLocation(inst.Market))
		snapDay := snap.AsOf.In(marketLocation(inst.Market))
		if dateOnly(quoteDay).Before(dateOnly(snapDay)) {
			snap.Stale = true
			snap.Warnings = append(snap.Warnings, fmt.Sprintf(
				"报价时点 %s 早于快照日 %s, 标记为历史/延迟数据",
				quoteDay.Format("2006-01-02 15:04"), snapDay.Format("2006-01-02")))
		}
	}

	return snap, nil
}

// snapshotPart 执行单个能力的并行取数与归一化。失败源的错误只进 warnings,
// 不向上返回 error (除了解析错误已在 Snapshot 前面处理)。
func (r *Registry) snapshotPart(ctx context.Context, inst Instrument, capability Capability,
	opts SnapshotOptions, filterAsOf time.Time) part {
	switch capability {
	case CapQuote:
		return r.snapshotQuote(ctx, inst, filterAsOf)
	case CapHistory:
		return r.snapshotHistory(ctx, inst, opts, filterAsOf)
	case CapFundamental:
		return r.snapshotFundamentals(ctx, inst, filterAsOf)
	case CapFiling:
		return r.snapshotFilings(ctx, inst, filterAsOf)
	case CapNews:
		return r.snapshotNews(ctx, inst, filterAsOf)
	default:
		return part{cap: capability, missing: true,
			warnings: []string{fmt.Sprintf("能力 %s 没有实现的数据源", capability)}}
	}
}

// part 是能力组结果。定义在包级以便各 snapshotXxx 共享。
type part struct {
	cap      Capability
	quote    *Quote
	bars     []Bar
	facts    []MarketFact
	filings  []Filing
	news     []NewsItem
	sources  []string
	warnings []string
	missing  bool
}

// snapshotQuote 并行取报价, 取最先成功的值, 并用第二源做跨源核价。
// 价差 >= VerifyThreshold 时保留主源值并在 Warnings 里并列两值。
func (r *Registry) snapshotQuote(ctx context.Context, inst Instrument, filterAsOf time.Time) part {
	out := part{cap: CapQuote}
	providers := r.eligible(inst.Market, CapQuote)
	if len(providers) == 0 {
		out.missing = true
		out.warnings = append(out.warnings, "quote: 没有声明该能力的可用 provider")
		return out
	}
	results := fanOut(ctx, providers, true, func(c context.Context, p Provider) (*Quote, error) {
		qp, ok := p.(QuoteProvider)
		if !ok {
			return nil, Wrap(p.Name(), ErrUnsupported, nil)
		}
		return qp.Quote(c, inst)
	})

	type success struct {
		provider Provider
		quote    *Quote
	}
	var okList []success
	for _, res := range results {
		if res.err == nil && res.value != nil {
			okList = append(okList, success{provider: res.provider, quote: res.value})
			out.sources = append(out.sources, res.provider.Name())
			continue
		}
		if res.err != nil {
			out.warnings = append(out.warnings, fmt.Sprintf("报价源 %s 失败: %v", res.provider.Name(), res.err))
		}
	}
	if len(okList) == 0 {
		out.missing = true
		out.warnings = append(out.warnings, "quote: 所有报价源均失败")
		return out
	}

	if notReturned := len(providers) - len(results); notReturned > 0 {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"有 %d 个报价源在核价窗口内未返回, 不计入 Sources", notReturned))
	}
	primary := normalizeQuote(okList[0].quote, inst, okList[0].provider.Name(), filterAsOf)
	out.quote = primary
	if len(okList) == 1 {
		if len(providers) > 1 {
			out.warnings = append(out.warnings,
				fmt.Sprintf("报价仅有单源 %s, 无法跨源核价", okList[0].provider.Name()))
		}
		return out
	}

	second := okList[1]
	threshold := r.cfg.VerifyThreshold
	if threshold <= 0 {
		threshold = 0.005
	}
	rel := relativeDiff(primary.Last, second.quote.Last)
	if rel >= threshold {
		// Quote 类型没有 Verified 字段, 用 Warnings 承载"未验证"结论与两个值;
		// 价差小于阈值时不写 Warnings, Sources 里同时出现两个源即为核价通过的证据。
		out.warnings = append(out.warnings, fmt.Sprintf(
			"报价跨源不一致: 主源 %s last=%.4f, 备源 %s last=%.4f, 相对差 %.3f%% (阈值 %.3f%%); 保留主源值 (unverified)",
			okList[0].provider.Name(), primary.Last,
			second.provider.Name(), second.quote.Last,
			rel*100, threshold*100))
	}

	// 报价也有前视风险: 历史 as_of 不能拿今天的实时价。
	if !primary.AsOf.IsZero() && isCalendarMarket(inst.Market) {
		if dateOnly(primary.AsOf.In(marketLocation(inst.Market))).After(dateOnly(filterAsOf.In(marketLocation(inst.Market)))) {
			out.warnings = append(out.warnings, fmt.Sprintf(
				"报价时点 %s 晚于请求的 as_of %s, 已丢弃以避免前视",
				primary.AsOf.Format("2006-01-02 15:04"), filterAsOf.Format("2006-01-02")))
			out.quote = nil
			out.missing = true
		}
	}
	return out
}

// snapshotHistory 取日 K 并做前视硬过滤。bar.Date 晚于 as_of 的整根丢弃。
func (r *Registry) snapshotHistory(ctx context.Context, inst Instrument, opts SnapshotOptions, filterAsOf time.Time) part {
	out := part{cap: CapHistory}
	providers := r.eligible(inst.Market, CapHistory)
	if len(providers) == 0 {
		out.missing = true
		out.warnings = append(out.warnings, "history: 没有声明该能力的可用 provider")
		return out
	}
	adjust := opts.Adjust
	if adjust == "" {
		adjust = r.cfg.DefaultAdjust
	}
	if adjust == "" {
		adjust = AdjustQFQ
	}
	params := HistoryParams{From: time.Time{}, To: filterAsOf, Interval: "1d", Adjust: adjust}

	results := fanOutSlow(ctx, providers, false, func(c context.Context, p Provider) ([]Bar, error) {
		hp, ok := p.(HistoryProvider)
		if !ok {
			return nil, Wrap(p.Name(), ErrUnsupported, nil)
		}
		return hp.History(c, inst, params)
	})
	var bars []Bar
	used := ""
	for _, res := range results {
		if res.err == nil {
			bars = normalizeBars(res.value, inst, params, r.cfg.DefaultAdjust)
			used = res.provider.Name()
			out.sources = append(out.sources, used)
			break
		}
		out.warnings = append(out.warnings, fmt.Sprintf("K 线源 %s 失败: %v", res.provider.Name(), res.err))
	}
	if used == "" {
		out.missing = true
		out.warnings = append(out.warnings, "history: 所有 K 线源均失败")
		return out
	}

	kept := bars[:0]
	dropped := 0
	for _, b := range bars {
		if b.Date.After(filterAsOf) {
			dropped++
			continue
		}
		kept = append(kept, b)
	}
	if dropped > 0 {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"前视过滤: 丢弃 %d 根 Date 晚于 as_of (%s) 的 K 线", dropped, filterAsOf.Format("2006-01-02")))
	}
	out.bars = kept
	return out
}

// snapshotFundamentals 取财务/估值事实并按 AsOf 做前视过滤。
func (r *Registry) snapshotFundamentals(ctx context.Context, inst Instrument, filterAsOf time.Time) part {
	out := part{cap: CapFundamental}
	providers := r.eligible(inst.Market, CapFundamental)
	if len(providers) == 0 {
		out.missing = true
		out.warnings = append(out.warnings, "fundamental: 没有声明该能力的可用 provider")
		return out
	}
	results := fanOutSlow(ctx, providers, false, func(c context.Context, p Provider) ([]MarketFact, error) {
		fp, ok := p.(FundamentalsProvider)
		if !ok {
			return nil, Wrap(p.Name(), ErrUnsupported, nil)
		}
		return fp.Fundamentals(c, inst, "")
	})
	var facts []MarketFact
	used := ""
	for _, res := range results {
		if res.err == nil {
			facts = normalizeFacts(res.value, inst, res.provider.Name())
			used = res.provider.Name()
			out.sources = append(out.sources, used)
			break
		}
		out.warnings = append(out.warnings, fmt.Sprintf("财务源 %s 失败: %v", res.provider.Name(), res.err))
	}
	if used == "" {
		out.missing = true
		out.warnings = append(out.warnings, "fundamental: 所有财务源均失败")
		return out
	}

	kept := facts[:0]
	dropped, noAsOf := 0, 0
	for _, f := range facts {
		if f.AsOf.IsZero() {
			noAsOf++
			kept = append(kept, f)
			continue
		}
		if f.AsOf.After(filterAsOf) {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	if dropped > 0 {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"前视过滤: 丢弃 %d 条 AsOf 晚于 as_of (%s) 的财务事实", dropped, filterAsOf.Format("2006-01-02")))
	}
	if noAsOf > 0 {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"有 %d 条财务事实缺少 AsOf, 无法验证时点, 已保留但未标记 verified", noAsOf))
	}
	out.facts = kept
	return out
}

// snapshotFilings 取公告并按发布时间做前视过滤。
func (r *Registry) snapshotFilings(ctx context.Context, inst Instrument, filterAsOf time.Time) part {
	out := part{cap: CapFiling}
	providers := r.eligible(inst.Market, CapFiling)
	if len(providers) == 0 {
		out.missing = true
		out.warnings = append(out.warnings, "filing: 没有声明该能力的可用 provider")
		return out
	}
	results := fanOutSlow(ctx, providers, false, func(c context.Context, p Provider) ([]Filing, error) {
		fp, ok := p.(FilingsProvider)
		if !ok {
			return nil, Wrap(p.Name(), ErrUnsupported, nil)
		}
		return fp.Filings(c, inst, time.Time{})
	})
	var filings []Filing
	used := ""
	for _, res := range results {
		if res.err == nil {
			filings = normalizeFilings(res.value, inst, res.provider.Name())
			used = res.provider.Name()
			out.sources = append(out.sources, used)
			break
		}
		out.warnings = append(out.warnings, fmt.Sprintf("公告源 %s 失败: %v", res.provider.Name(), res.err))
	}
	if used == "" {
		out.missing = true
		out.warnings = append(out.warnings, "filing: 所有公告源均失败")
		return out
	}
	kept := filings[:0]
	dropped := 0
	for _, f := range filings {
		if !f.Published.IsZero() && f.Published.After(filterAsOf) {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	if dropped > 0 {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"前视过滤: 丢弃 %d 条发布时间晚于 as_of (%s) 的公告", dropped, filterAsOf.Format("2006-01-02")))
	}
	out.filings = kept
	return out
}

// snapshotNews 取个股新闻并按发布时间做前视过滤 (仅在显式请求 CapNews 时调用)。
func (r *Registry) snapshotNews(ctx context.Context, inst Instrument, filterAsOf time.Time) part {
	out := part{cap: CapNews}
	providers := r.eligible(inst.Market, CapNews)
	if len(providers) == 0 {
		out.missing = true
		out.warnings = append(out.warnings, "news: 没有声明该能力的可用 provider")
		return out
	}
	results := fanOutSlow(ctx, providers, false, func(c context.Context, p Provider) ([]NewsItem, error) {
		np, ok := p.(NewsProvider)
		if !ok {
			return nil, Wrap(p.Name(), ErrUnsupported, nil)
		}
		return np.News(c, inst, 7)
	})
	var news []NewsItem
	used := ""
	for _, res := range results {
		if res.err == nil {
			news = res.value
			used = res.provider.Name()
			out.sources = append(out.sources, used)
			break
		}
		out.warnings = append(out.warnings, fmt.Sprintf("新闻源 %s 失败: %v", res.provider.Name(), res.err))
	}
	if used == "" {
		out.missing = true
		out.warnings = append(out.warnings, "news: 所有新闻源均失败")
		return out
	}
	kept := news[:0]
	dropped := 0
	for _, n := range news {
		if !n.Published.IsZero() && n.Published.After(filterAsOf) {
			dropped++
			continue
		}
		kept = append(kept, n)
	}
	if dropped > 0 {
		out.warnings = append(out.warnings, fmt.Sprintf("前视过滤: 丢弃 %d 条晚于 as_of 的新闻", dropped))
	}
	out.news = kept
	return out
}

// dedupeCaps 去重并保留用户给定的顺序。
func dedupeCaps(in []Capability) []Capability {
	seen := map[Capability]bool{}
	out := make([]Capability, 0, len(in))
	for _, c := range in {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// isCalendarMarket 判断市场是否使用交易所交易日历。
func isCalendarMarket(m Market) bool {
	return m == MarketCN || m == MarketHK || m == MarketUS
}

// dateOnly 截断到日期, 用于避免同一天内的时间戳造成误判。
func dateOnly(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
}

// relativeDiff 返回两个价格的相对差 |a-b| / |a|, a 为主源价格。
// 主源为 0 时退回 |b|; 两者都为 0 视为相等。
func relativeDiff(a, b float64) float64 {
	denom := math.Abs(a)
	if denom == 0 {
		denom = math.Abs(b)
	}
	if denom == 0 {
		return 0
	}
	return math.Abs(a-b) / denom
}
