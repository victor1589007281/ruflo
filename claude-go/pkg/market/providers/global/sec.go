package global

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

// secProvider 适配 SEC EDGAR: ticker→CIK 映射与 submissions 披露列表。
//
// 集群内未实测, 按设计固定 direct。SEC 硬性要求可识别 User-Agent 与 ≤10 req/s;
// 因此所有请求都带 SEC_USER_AGENT (或 cfg.UserAgent), 并由本进程内限速器排队。
//
// 财务 (XBRL) 有意未实现: 科目口径、期间对齐与单位换算成本高, 本层不做推测,
// Fundamentals 恒返回 ErrUnsupported, 且不声明 CapFundamental。
type secProvider struct {
	*gBase
	wwwBase string // www.sec.gov: company_tickers.json 与 Archives 所在主机
	qps     float64

	limMu   sync.Mutex
	lastReq time.Time

	cikMu     sync.Mutex
	cikLoadMu sync.Mutex
	cikAt     time.Time
	ciks      map[string]string // 归一化 ticker → 10 位 CIK
}

// newSEC 构造 SEC 适配器。基类地址是 data.sec.gov, 网页/归档地址是 www.sec.gov。
func newSEC(pc market.ProviderConfig, cfg market.Config, client *http.Client) *secProvider {
	ua := secUserAgent(pc, cfg)
	b := newBase("sec", pc, cfg, client, ua,
		[]market.Market{market.MarketUS},
		gCaps(market.CapFiling), "https://data.sec.gov")

	wwwBase := "https://www.sec.gov"
	if b.base != "https://data.sec.gov" {
		// 测试或镜像场景: 让 ticker/归档也指向同一主机。
		wwwBase = b.base
	}
	qps := pc.QPS
	if qps <= 0 {
		qps = 10
	}
	return &secProvider{gBase: b, wwwBase: strings.TrimRight(wwwBase, "/"), qps: qps}
}

// EgressMode 返回 SEC 的固定出口; data.sec.gov 与 www.sec.gov 均走直连。
func (p *secProvider) EgressMode() string { return "direct" }

// secUserAgent 选择 SEC 要求的 User-Agent: 显式配置 > 环境变量 > 全局 UA。
func secUserAgent(pc market.ProviderConfig, cfg market.Config) string {
	if strings.TrimSpace(pc.UserAgent) != "" {
		return pc.UserAgent
	}
	if v := strings.TrimSpace(os.Getenv("SEC_USER_AGENT")); v != "" {
		return v
	}
	if strings.TrimSpace(cfg.UserAgent) != "" {
		return cfg.UserAgent
	}
	return "claude-go-market/1.0"
}

// secTickerEntry 是 company_tickers.json 的单项。
type secTickerEntry struct {
	CIK    int64  `json:"cik_str"`
	Ticker string `json:"ticker"`
	Title  string `json:"title"`
}

// secSubmissions 是 data.sec.gov/submissions/CIK##########.json 的字段子集。
type secSubmissions struct {
	Name    string `json:"name"`
	Filings struct {
		Recent struct {
			AccessionNumber       []string `json:"accessionNumber"`
			FilingDate            []string `json:"filingDate"`
			ReportDate            []string `json:"reportDate"`
			Form                  []string `json:"form"`
			PrimaryDocument       []string `json:"primaryDocument"`
			PrimaryDocDescription []string `json:"primaryDocDescription"`
		} `json:"recent"`
	} `json:"filings"`
}

// Filings 返回 since 之后的 10-K / 10-Q / 8-K (含 /A 修正件), 按披露日期倒序。
func (p *secProvider) Filings(ctx context.Context, inst market.Instrument, since time.Time) ([]market.Filing, error) {
	if inst.Market != market.MarketUS {
		return nil, market.Wrap(p.Name(), market.ErrUnresolved,
			fmt.Errorf("sec does not serve market %s", inst.Market))
	}
	ticker := normalizeTicker(gCode(inst))
	if ticker == "" {
		return nil, market.Wrap(p.Name(), market.ErrUnresolved, fmt.Errorf("empty ticker"))
	}
	cik, err := p.cikFor(ctx, ticker)
	if err != nil {
		return nil, err
	}

	rawURL := p.base + "/submissions/CIK" + cik + ".json"
	var sub secSubmissions
	if err := p.secGetJSON(ctx, rawURL, &sub); err != nil {
		return nil, err
	}

	recent := sub.Filings.Recent
	out := make([]market.Filing, 0, len(recent.Form))
	for i, form := range recent.Form {
		form = strings.TrimSpace(form)
		if !secFormAllowed(form) {
			continue
		}
		dateText := secAt(recent.FilingDate, i)
		published, err := time.ParseInLocation("2006-01-02", dateText, time.UTC)
		if err != nil {
			continue
		}
		if !since.IsZero() && published.Before(since) {
			continue
		}
		acc := secAt(recent.AccessionNumber, i)
		doc := secAt(recent.PrimaryDocument, i)
		desc := strings.TrimSpace(secAt(recent.PrimaryDocDescription, i))
		title := form
		if desc != "" {
			title = form + " " + desc
		}
		out = append(out, market.Filing{
			ID:        acc,
			Symbol:    inst.Canonical,
			Title:     title,
			Type:      form,
			URL:       p.archiveURL(cik, acc, doc),
			Published: published.UTC(),
			Source:    p.Name(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Published.After(out[j].Published) })
	return out, nil
}

// Fundamentals 有意未实现: 不编造 XBRL 数字, 能力集里也没有 CapFundamental。
func (p *secProvider) Fundamentals(ctx context.Context, inst market.Instrument, period string) ([]market.MarketFact, error) {
	return nil, market.Wrap(p.Name(), market.ErrUnsupported, nil)
}

// cikFor 把 ticker 解析成 10 位 CIK; company_tickers.json 进程内缓存 24h。
func (p *secProvider) cikFor(ctx context.Context, ticker string) (string, error) {
	p.cikMu.Lock()
	if p.ciks != nil && time.Since(p.cikAt) < 24*time.Hour {
		cik, ok := p.ciks[ticker]
		p.cikMu.Unlock()
		if !ok {
			return "", market.Wrap(p.Name(), market.ErrUnresolved,
				fmt.Errorf("ticker %s not found in SEC company_tickers", ticker))
		}
		return cik, nil
	}
	p.cikMu.Unlock()

	// 单飞加载: 避免并发请求同时抓 company_tickers.json 触发限流。
	p.cikLoadMu.Lock()
	defer p.cikLoadMu.Unlock()
	p.cikMu.Lock()
	if p.ciks != nil && time.Since(p.cikAt) < 24*time.Hour {
		cik, ok := p.ciks[ticker]
		p.cikMu.Unlock()
		if !ok {
			return "", market.Wrap(p.Name(), market.ErrUnresolved,
				fmt.Errorf("ticker %s not found in SEC company_tickers", ticker))
		}
		return cik, nil
	}
	p.cikMu.Unlock()

	rawURL := p.wwwBase + "/files/company_tickers.json"
	var raw map[string]secTickerEntry
	if err := p.secGetJSON(ctx, rawURL, &raw); err != nil {
		return "", err
	}
	ciks := make(map[string]string, len(raw))
	for _, e := range raw {
		t := normalizeTicker(e.Ticker)
		if t == "" || e.CIK <= 0 {
			continue
		}
		ciks[t] = fmt.Sprintf("%010d", e.CIK)
	}
	p.cikMu.Lock()
	p.ciks = ciks
	p.cikAt = time.Now()
	p.cikMu.Unlock()

	cik, ok := ciks[ticker]
	if !ok {
		return "", market.Wrap(p.Name(), market.ErrUnresolved,
			fmt.Errorf("ticker %s not found in SEC company_tickers", ticker))
	}
	return cik, nil
}

// secGet 在限速器排队后执行 GET。
func (p *secProvider) secGet(ctx context.Context, rawURL string) ([]byte, error) {
	if err := p.wait(ctx); err != nil {
		return nil, err
	}
	return p.get(ctx, rawURL)
}

// secGetJSON 在限速器排队后执行 GET 并解码 JSON。
func (p *secProvider) secGetJSON(ctx context.Context, rawURL string, out any) error {
	if err := p.wait(ctx); err != nil {
		return err
	}
	return p.getJSON(ctx, rawURL, out)
}

// wait 按 QPS 预留请求时隙; SEC 免费额度标称 10 req/s。
func (p *secProvider) wait(ctx context.Context) error {
	if p.qps <= 0 {
		return nil
	}
	interval := time.Duration(float64(time.Second) / p.qps)
	if interval <= 0 {
		return nil
	}
	p.limMu.Lock()
	now := time.Now()
	var delay time.Duration
	if p.lastReq.IsZero() || !now.Before(p.lastReq.Add(interval)) {
		p.lastReq = now
	} else {
		next := p.lastReq.Add(interval)
		delay = next.Sub(now)
		p.lastReq = next
	}
	p.limMu.Unlock()
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// archiveURL 拼接 EDGAR 原文地址; 无 primaryDocument 时退到该 accession 的目录。
func (p *secProvider) archiveURL(cik, accession, doc string) string {
	acc := strings.ReplaceAll(accession, "-", "")
	cikNum := strings.TrimLeft(cik, "0")
	if cikNum == "" {
		cikNum = "0"
	}
	base := p.wwwBase + "/Archives/edgar/data/" + cikNum + "/" + acc + "/"
	if strings.TrimSpace(doc) == "" {
		return base
	}
	return base + doc
}

// normalizeTicker 归一美股代码: 大写并把 BRK.B 转成 SEC 使用的 BRK-B。
func normalizeTicker(code string) string {
	t := strings.ToUpper(strings.TrimSpace(code))
	t = strings.ReplaceAll(t, ".", "-")
	return t
}

// secFormAllowed 过滤出 10-K / 10-Q / 8-K 及其修正件。
func secFormAllowed(form string) bool {
	switch strings.ToUpper(strings.TrimSpace(form)) {
	case "10-K", "10-K/A", "10-Q", "10-Q/A", "8-K", "8-K/A":
		return true
	default:
		return false
	}
}

// secAt 安全读取 submissions 中的平行数组。
func secAt(v []string, i int) string {
	if i < 0 || i >= len(v) {
		return ""
	}
	return strings.TrimSpace(v[i])
}
