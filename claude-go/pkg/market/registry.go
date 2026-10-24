package market

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现按"市场 + 能力"路由的注册表。
//
// 集群实测 (2026-10-24, k8s agents 命名空间) 表明东方财富 push2 经常不可达且
// 失败慢 (3–6s), 因此**不能**做串行"主源失败再切备源": 那会直接吃掉
// SnapshotDeadline=3s。这里的取数策略是:
//   - 同一能力的多个源并行发起;
//   - 单源硬超时 1.5s (sourceTimeout), 不受上游吞掉 context 的影响;
//   - 最先返回的成功结果作为主值, 其余成功结果只用于核价;
//   - 失败源的错误只进 Warnings, 不阻断已拿到的数据。
//
// 能力未声明的 provider 不参与路由, 不做"试一下再失败"。

const (
	// sourceTimeout 是单源硬超时。集群实测东财失败需 3–6s, 必须在这里截断。
	sourceTimeout = 1500 * time.Millisecond
	// crossCheckWindow 是拿到第一个报价后, 等待第二源核价的时间窗。
	crossCheckWindow = 150 * time.Millisecond
)

// ProviderInfo 是注册表对外暴露的 provider 摘要。
type ProviderInfo struct {
	Name         string
	Markets      []Market
	Capabilities CapSet
	Healthy      bool
}

// Registry 是数据面路由入口。
type Registry struct {
	cfg       Config
	resolver  *StaticResolver
	providers []Provider
}

// NewRegistry 创建注册表。cfg 中显式 Enabled=false 的 provider 会被跳过;
// cfg 未列出的 provider 视为启用 (便于测试与外部组装)。
func NewRegistry(cfg Config, providers ...Provider) *Registry {
	r := &Registry{cfg: cfg, resolver: NewResolver()}
	for _, p := range providers {
		if p == nil {
			continue
		}
		if pc, ok := cfg.Providers[p.Name()]; ok && !pc.Enabled {
			continue
		}
		r.providers = append(r.providers, p)
	}
	return r
}

// Resolve 把输入解析成规范标的。
func (r *Registry) Resolve(ctx context.Context, input string) (Instrument, error) {
	return r.resolver.Resolve(ctx, input)
}

// Providers 返回已注册 provider 的能力矩阵与健康状态。
func (r *Registry) Providers() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(r.providers))
	for _, p := range r.providers {
		caps := make(CapSet, len(p.Capabilities()))
		for k, v := range p.Capabilities() {
			caps[k] = v
		}
		markets := append([]Market(nil), p.Markets()...)
		out = append(out, ProviderInfo{
			Name:         p.Name(),
			Markets:      markets,
			Capabilities: caps,
			Healthy:      p.Healthy(),
		})
	}
	return out
}

// Quote 返回单个报价。多源并行, 取最先成功的完整结果; 不做跨源核价
// (核价只在 Snapshot 中做, 因为需要 Warnings 承载两个值)。
func (r *Registry) Quote(ctx context.Context, input string, asOf time.Time) (*Quote, error) {
	inst, err := r.Resolve(ctx, input)
	if err != nil {
		return nil, err
	}
	ps := r.eligible(inst.Market, CapQuote)
	if len(ps) == 0 {
		return nil, r.unsupported(inst, CapQuote)
	}
	results := fanOut(ctx, ps, false, func(c context.Context, p Provider) (*Quote, error) {
		qp, ok := p.(QuoteProvider)
		if !ok {
			return nil, Wrap(p.Name(), ErrUnsupported, nil)
		}
		return qp.Quote(c, inst)
	})
	for _, res := range results {
		if res.err == nil && res.value != nil {
			return normalizeQuote(res.value, inst, res.provider.Name(), asOf), nil
		}
	}
	return nil, firstErrorOf(results, r.unsupported(inst, CapQuote))
}

// History 返回 K 线。多源并行, 取最先成功的完整结果。
func (r *Registry) History(ctx context.Context, input string, p HistoryParams) ([]Bar, error) {
	inst, err := r.Resolve(ctx, input)
	if err != nil {
		return nil, err
	}
	ps := r.eligible(inst.Market, CapHistory)
	if len(ps) == 0 {
		return nil, r.unsupported(inst, CapHistory)
	}
	results := fanOutSlow(ctx, ps, false, func(c context.Context, prov Provider) ([]Bar, error) {
		hp, ok := prov.(HistoryProvider)
		if !ok {
			return nil, Wrap(prov.Name(), ErrUnsupported, nil)
		}
		return hp.History(c, inst, p)
	})
	for _, res := range results {
		if res.err == nil && res.value != nil {
			return normalizeBars(res.value, inst, p, r.cfg.DefaultAdjust), nil
		}
	}
	return nil, firstErrorOf(results, r.unsupported(inst, CapHistory))
}

// Fundamentals 返回财务/估值事实。单源能力的第三方口径差异大, 不做多源投票。
func (r *Registry) Fundamentals(ctx context.Context, input string, period string) ([]MarketFact, error) {
	inst, err := r.Resolve(ctx, input)
	if err != nil {
		return nil, err
	}
	ps := r.eligible(inst.Market, CapFundamental)
	if len(ps) == 0 {
		return nil, r.unsupported(inst, CapFundamental)
	}
	results := fanOutSlow(ctx, ps, false, func(c context.Context, prov Provider) ([]MarketFact, error) {
		fp, ok := prov.(FundamentalsProvider)
		if !ok {
			return nil, Wrap(prov.Name(), ErrUnsupported, nil)
		}
		return fp.Fundamentals(c, inst, period)
	})
	for _, res := range results {
		if res.err == nil {
			return normalizeFacts(res.value, inst, res.provider.Name()), nil
		}
	}
	return nil, firstErrorOf(results, r.unsupported(inst, CapFundamental))
}

// Filings 返回公告/披露。since 是请求下界, 不代替前视过滤。
func (r *Registry) Filings(ctx context.Context, input string, since time.Time) ([]Filing, error) {
	inst, err := r.Resolve(ctx, input)
	if err != nil {
		return nil, err
	}
	ps := r.eligible(inst.Market, CapFiling)
	if len(ps) == 0 {
		return nil, r.unsupported(inst, CapFiling)
	}
	results := fanOutSlow(ctx, ps, false, func(c context.Context, prov Provider) ([]Filing, error) {
		fp, ok := prov.(FilingsProvider)
		if !ok {
			return nil, Wrap(prov.Name(), ErrUnsupported, nil)
		}
		return fp.Filings(c, inst, since)
	})
	for _, res := range results {
		if res.err == nil {
			return normalizeFilings(res.value, inst, res.provider.Name()), nil
		}
	}
	return nil, firstErrorOf(results, r.unsupported(inst, CapFiling))
}

// News 返回个股新闻。
//
// 与 Snapshot 的区别: 这里不做前视过滤 —— 调用方要的是"最近 N 天",
// 时点一致性由 Snapshot 负责; 历史时点分析请走 Snapshot。
func (r *Registry) News(ctx context.Context, input string, days int) ([]NewsItem, error) {
	inst, err := r.Resolve(ctx, input)
	if err != nil {
		return nil, err
	}
	if days <= 0 {
		days = 7
	}
	ps := r.eligible(inst.Market, CapNews)
	if len(ps) == 0 {
		return nil, r.unsupported(inst, CapNews)
	}
	results := fanOutSlow(ctx, ps, false, func(c context.Context, p Provider) ([]NewsItem, error) {
		np, ok := p.(NewsProvider)
		if !ok {
			return nil, Wrap(p.Name(), ErrUnsupported, nil)
		}
		return np.News(c, inst, days)
	})
	for _, res := range results {
		if res.err == nil {
			out := make([]NewsItem, 0, len(res.value))
			for _, n := range res.value {
				if n.Symbol == "" {
					n.Symbol = inst.Canonical
				}
				out = append(out, n)
			}
			return out, nil
		}
	}
	return nil, firstErrorOf(results, r.unsupported(inst, CapNews))
}

// Macro 返回一条宏观序列的观测值。
//
// series 是数据源侧的序列名 (FRED: CPIAUCSL; World Bank: NY.GDP.MKTP.CD),
// 不做标的解析 —— 宏观序列不是可交易标的, 也没有统一代码体系。
// 多源并行, 取注册顺序最先成功的结果; 全部失败时返回主源错误。
func (r *Registry) Macro(ctx context.Context, series string, from, to time.Time) ([]MarketFact, error) {
	series = strings.TrimSpace(series)
	if series == "" {
		return nil, Wrap("registry", ErrNotAvailable, fmt.Errorf("宏观序列名不能为空"))
	}
	ps := r.eligible(MarketMacro, CapMacro)
	if len(ps) == 0 {
		return nil, Wrap("registry", ErrUnsupported, fmt.Errorf("没有声明 macro 能力的可用 provider"))
	}
	results := fanOutSlow(ctx, ps, false, func(c context.Context, p Provider) ([]MarketFact, error) {
		mp, ok := p.(MacroProvider)
		if !ok {
			return nil, Wrap(p.Name(), ErrUnsupported, nil)
		}
		return mp.Macro(c, series, from, to)
	})
	for _, res := range results {
		if res.err == nil {
			inst := Instrument{Market: MarketMacro, Code: strings.ToUpper(series), Name: series}
			return normalizeFacts(res.value, inst, res.provider.Name()), nil
		}
	}
	return nil, firstErrorOf(results, Wrap("registry", ErrUnsupported, nil))
}

// eligible 返回对 market+cap 可用且健康的 provider, 保持注册顺序 (顺序决定
// 同一时间窗内成功结果的优先级)。
func (r *Registry) eligible(m Market, cap Capability) []Provider {
	var out []Provider
	for _, p := range r.providers {
		if !p.Healthy() || !p.Capabilities().Has(cap) {
			continue
		}
		ok := false
		for _, pm := range p.Markets() {
			if pm == m {
				ok = true
				break
			}
		}
		if ok {
			out = append(out, p)
		}
	}
	return out
}

// unsupported 构造"市场/能力没有可用源"的错误。
func (r *Registry) unsupported(inst Instrument, cap Capability) error {
	return Wrap("registry", ErrUnsupported,
		fmt.Errorf("市场 %s 的能力 %s 没有可用 provider", inst.Market, cap))
}

// firstErrorOf 在所有源失败时挑注册顺序最靠前的错误, 便于定位主源问题。
func firstErrorOf[T any](results []fanResult[T], fallback error) error {
	bestIdx := -1
	var best error
	for _, res := range results {
		if res.err == nil {
			continue
		}
		if bestIdx == -1 || res.idx < bestIdx {
			bestIdx, best = res.idx, res.err
		}
	}
	if best != nil {
		return best
	}
	return fallback
}

// fanResult 是并行调用的单个结果。
type fanResult[T any] struct {
	idx      int
	provider Provider
	value    T
	err      error
}

// fanOut 并行调用同一能力的全部 provider。
//
// 行为:
//   - 报价这类"要快"的能力用 1.5s 硬超时 (见 quoteFanOut), 历史/财务/宏观
//     用 fanOutSlow: World Bank、SEC 的首包常超过 1.5s, 用报价预算去卡它们
//     只会得到稳定超时;
//   - waitSecond=false: 第一个成功结果立即返回;
//   - waitSecond=true: 第一个成功后最多再等 crossCheckWindow 收集第二源用于核价;
//   - 全部源都失败时返回所有错误, 由调用方挑选。
func fanOut[T any](ctx context.Context, providers []Provider, waitSecond bool,
	call func(context.Context, Provider) (T, error)) []fanResult[T] {
	return fanOutWithin(ctx, providers, waitSecond, sourceTimeout, sourceTimeout+crossCheckWindow, call)
}

// fanOutSlow 用于非报价能力: 单源 8s, 组预算 8.2s。
func fanOutSlow[T any](ctx context.Context, providers []Provider, waitSecond bool,
	call func(context.Context, Provider) (T, error)) []fanResult[T] {
	return fanOutWithin(ctx, providers, waitSecond, 8*time.Second, 8*time.Second+crossCheckWindow, call)
}

// fanOutWithin 是 fanOut 的通用实现。
func fanOutWithin[T any](ctx context.Context, providers []Provider, waitSecond bool,
	perSource, groupBudget time.Duration, call func(context.Context, Provider) (T, error)) []fanResult[T] {
	if len(providers) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	groupCtx, cancel := context.WithTimeout(ctx, groupBudget)
	defer cancel()

	ch := make(chan fanResult[T], len(providers))
	for i, p := range providers {
		i, p := i, p
		go func() {
			callCtx, c := context.WithTimeout(groupCtx, perSource)
			defer c()
			v, err := call(callCtx, p)
			ch <- fanResult[T]{idx: i, provider: p, value: v, err: err}
		}()
	}

	out := make([]fanResult[T], 0, len(providers))
	successes, received := 0, 0
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case res := <-ch:
			received++
			out = append(out, res)
			if res.err == nil {
				successes++
				if !waitSecond || successes >= 2 || received == len(providers) {
					return out
				}
				if timer == nil {
					timer = time.NewTimer(crossCheckWindow)
					timerC = timer.C
				}
			} else if received == len(providers) {
				return out
			}
		case <-timerC:
			return out
		case <-groupCtx.Done():
			// 超时后把已经就绪的结果带走, 不再等待可能永不返回的 goroutine。
			for {
				select {
				case res := <-ch:
					out = append(out, res)
				default:
					return out
				}
			}
		}
	}
}

// normalizeQuote 归一化报价。ChangePct 一律由 Last/PrevClose 重算;
// 无法重算时置 0, 绝不采信第三方涨跌幅字段。
func normalizeQuote(q *Quote, inst Instrument, source string, asOf time.Time) *Quote {
	if q == nil {
		return nil
	}
	q.Inst = mergeInstrument(q.Inst, inst)
	if q.Source == "" {
		q.Source = source
	}
	if q.PrevClose != 0 {
		q.ChangePct = (q.Last - q.PrevClose) / q.PrevClose * 100
	} else {
		q.ChangePct = 0
	}
	if q.AsOf.IsZero() {
		if !asOf.IsZero() {
			q.AsOf = asOf
		} else {
			q.AsOf = time.Now()
		}
	}
	if q.Session == "" {
		q.Session = SessionOf(inst.Market, q.AsOf)
	}
	if q.Session == SessionHalted {
		q.Tradable = false
	}
	return q
}

// normalizeBars 归一化 K 线: 显式复权口径、按请求区间裁剪、按日期升序。
func normalizeBars(bars []Bar, inst Instrument, p HistoryParams, defaultAdjust string) []Bar {
	adjust := p.Adjust
	if adjust == "" {
		adjust = defaultAdjust
	}
	if adjust == "" {
		adjust = AdjustQFQ
	}
	out := make([]Bar, 0, len(bars))
	for _, b := range bars {
		if !p.From.IsZero() && b.Date.Before(p.From) {
			continue
		}
		if !p.To.IsZero() && b.Date.After(p.To) {
			continue
		}
		if b.Adjust == "" {
			b.Adjust = adjust
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}

// normalizeFacts 只补身份与来源元数据, 不填充任何数值、币种或时点。
func normalizeFacts(facts []MarketFact, inst Instrument, source string) []MarketFact {
	now := time.Now()
	for i := range facts {
		if facts[i].Symbol == "" {
			facts[i].Symbol = inst.Canonical
		}
		if facts[i].Source == "" {
			facts[i].Source = source
		}
		if facts[i].FetchedAt.IsZero() {
			facts[i].FetchedAt = now
		}
	}
	return facts
}

// normalizeFilings 补公告的标的与来源, 按发布时间倒序。
func normalizeFilings(filings []Filing, inst Instrument, source string) []Filing {
	for i := range filings {
		if filings[i].Symbol == "" {
			filings[i].Symbol = inst.Canonical
		}
		if filings[i].Source == "" {
			filings[i].Source = source
		}
	}
	sort.SliceStable(filings, func(i, j int) bool {
		return filings[i].Published.After(filings[j].Published)
	})
	return filings
}

// mergeInstrument 用已解析的规范身份补齐 provider 返回的 Instrument。
// 已存在的字段不覆盖, 避免把 provider 的原始信息改写掉。
func mergeInstrument(got, resolved Instrument) Instrument {
	if got.Canonical == "" {
		got.Canonical = resolved.Canonical
	}
	if got.Market == "" {
		got.Market = resolved.Market
	}
	if got.Code == "" {
		got.Code = resolved.Code
	}
	if got.Name == "" {
		got.Name = resolved.Name
	}
	if got.Currency == "" {
		got.Currency = resolved.Currency
	}
	if got.Timezone == "" {
		got.Timezone = resolved.Timezone
	}
	return got
}
