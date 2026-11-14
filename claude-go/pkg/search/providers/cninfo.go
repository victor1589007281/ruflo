package providers

// cninfo.go —— 巨潮资讯公告检索（免 key）。
//
// 巨潮资讯网是中国证监会指定的上市公司信息披露平台。年报、半年报、招股书、
// 重大事项公告都在这里首发，是「经济年报 / 公司经营数据」的一手来源。
// API: https://www.cninfo.com.cn/new/hisAnnouncement/query（POST 表单）
//
// 频率与合规：公开披露信息，仅做关键词检索，不下载整份 PDF；按 1 req/s
// 节流并复用 Manager 缓存。返回详情页链接而不是直链 PDF，让用户/模型自己
// 决定是否抓取全文（WebFetch 侧还有域名白名单）。

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// emTagRE 去掉巨潮高亮词的内联标签。stripHTML 会把标签替换成空格，
// 对中文会把「年度报告」拆成「年度 报告」，所以先原地删掉。
var emTagRE = regexp.MustCompile(`(?i)</?em>`)

// cninfoDisclosureWords 是「这条查询在找上市公司披露」的判据，也是空结果
// 时做二次检索的前提。没有这些词时（如统计公报、行业报告），巨潮本来
// 就不是对口的源，不应为了放宽查询多打两次请求。
var cninfoDisclosureWords = []string{
	"年度报告", "半年度报告", "季度报告", "年报", "半年报", "季报",
	"招股说明书", "招股书", "公告", "业绩预告", "业绩快报", "重大事项",
}

// cninfoMetricWords 是问句里的分析性词汇，不是公告标题的一部分。
// 巨潮的 searchkey 按词严格匹配，多一个「营收」就把命中数打成 0。
var cninfoMetricWords = []string{
	"营业收入", "营收", "归母净利润", "净利润", "毛利率", "净利率",
	"利润", "收入", "现金流", "负债率", "市值", "估值", "同比", "环比", "增长",
}

// simplifyCNInfoQuery 把问句收敛成公告检索能命中的形状：去掉年份与
// 分析性词汇，只留实体 + 披露类型（如「比亚迪 年年度报告」）。
func simplifyCNInfoQuery(q string) string {
	q = yearTokenRE.ReplaceAllString(q, " ")
	for _, w := range cninfoMetricWords {
		q = strings.ReplaceAll(q, w, " ")
	}
	return strings.Join(strings.Fields(q), " ")
}

func hasDisclosureWord(q string) bool {
	for _, w := range cninfoDisclosureWords {
		if strings.Contains(q, w) {
			return true
		}
	}
	return false
}

type cninfoProvider struct {
	*base
	endpoint string
}

// newCNInfo 构造巨潮资讯公告源（直连，L2）。
func newCNInfo(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://www.cninfo.com.cn/new/hisAnnouncement/query"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("cninfo", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.0)
	b.kinds = search.KindSet(search.KindSearch)
	b.setMinInterval(time.Second)
	return &cninfoProvider{base: b, endpoint: endpoint}, nil
}

// Search 调用公告全文检索。
func (p *cninfoProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	cctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()
	limit := p.limitOf(req.Limit)

	results, err := p.query(cctx, query, limit)
	if err != nil {
		return nil, err
	}
	// 巨潮的 searchkey 按词严格匹配：年份按「公告年份」过滤（2025 年年报
	// 实际披露于 2026 年，带 2025 反而 0 命中），「营收」这类分析词也不在
	// 公告标题里。只在空结果且查询确实在找披露时，收敛一次再试。
	if len(results) == 0 && hasDisclosureWord(query) {
		if alt := simplifyCNInfoQuery(query); alt != "" && alt != query && hasDisclosureWord(alt) {
			if more, err := p.query(cctx, alt, limit); err == nil {
				results = more
			}
		}
	}
	return finishResults(p.name, req, results, limit), nil
}

// query 执行一次公告检索。
func (p *cninfoProvider) query(ctx context.Context, query string, limit int) ([]search.Result, error) {
	if err := p.pace(ctx); err != nil {
		return nil, search.Wrap(p.name, search.ErrTimeout, err)
	}
	form := url.Values{
		"pageNum":   {"1"},
		"pageSize":  {strconv.Itoa(limit)},
		"column":    {"szse"}, // 深沪京 A 股统一入口
		"tabName":   {"fulltext"},
		"searchkey": {query},
		"isHLtitle": {"true"},
		"sortName":  {""},
		"sortType":  {""},
		"plate":     {""},
		"stock":     {""},
		"secid":     {""},
		"category":  {""},
		"trade":     {""},
		"seDate":    {""},
	}
	header := http.Header{
		"Accept":       {"application/json, text/javascript, */*; q=0.01"},
		"Content-Type": {"application/x-www-form-urlencoded; charset=UTF-8"},
		"Referer":      {"https://www.cninfo.com.cn/new/commonUrl?url=disclosure/list/notice"},
	}
	var payload struct {
		Announcements []struct {
			SecCode    string `json:"secCode"`
			SecName    string `json:"secName"`
			Title      string `json:"announcementTitle"`
			Time       int64  `json:"announcementTime"`
			AdjunctURL string `json:"adjunctUrl"`
			ID         string `json:"announcementId"`
			OrgID      string `json:"orgId"`
		} `json:"announcements"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodPost, p.endpoint, header,
		strings.NewReader(form.Encode()), &payload); err != nil {
		return nil, err
	}

	results := make([]search.Result, 0, len(payload.Announcements))
	for _, it := range payload.Announcements {
		title := strings.Join(strings.Fields(stripHTML(emTagRE.ReplaceAllString(it.Title, ""))), " ")
		if title == "" {
			continue
		}
		published := time.Time{}
		if it.Time > 0 {
			published = time.UnixMilli(it.Time).UTC()
		}
		link := cninfoDetailURL(it.SecCode, it.ID, it.OrgID, published)
		if link == "" {
			continue
		}
		snippet := title
		if name := strings.TrimSpace(stripHTML(emTagRE.ReplaceAllString(it.SecName, ""))); name != "" {
			snippet = strings.TrimSpace(name + "（" + it.SecCode + "）：" + title)
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     title,
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Published: published,
			Lang:      "zh",
		})
	}
	return results, nil
}

// cninfoDetailURL 拼公告详情页；缺少必要参数时返回空。
func cninfoDetailURL(secCode, announcementID, orgID string, published time.Time) string {
	secCode = strings.TrimSpace(secCode)
	announcementID = strings.TrimSpace(announcementID)
	if secCode == "" || announcementID == "" {
		return ""
	}
	q := url.Values{
		"stockCode":      {secCode},
		"announcementId": {announcementID},
	}
	if orgID = strings.TrimSpace(orgID); orgID != "" {
		q.Set("orgId", orgID)
	}
	if !published.IsZero() {
		q.Set("announceTime", published.Format("2006-01-02"))
	}
	return "https://www.cninfo.com.cn/new/disclosure/detail?" + q.Encode()
}
