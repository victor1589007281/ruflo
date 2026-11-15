package providers

// gov.go —— 中国政府网「国务院政策文件库」检索（免 key）。
//
// 收录国务院/国务院办公厅及各部委发布的国发、国办发、国函、部令等规范性
// 文件原文，是「某件事国家怎么规定的 / 补贴与监管口径 / 政策原文」的一手
// 来源。与 cninfo 的分工：cninfo 是上市公司披露（年报/招股书），这里才是
// 政府公告与政策法规。
//
// 站点搜索页 (sousuo.www.gov.cn) 背后的 JSON 端点。实测 (2026-11-15, 集群
// 内直连) 返回 200/0.2s。两个检索字段的行为差别很大：
//   - searchfield=title：按词严格 AND，多词长问句常 0 命中。实测
//     "数据安全 管理办法" = 0，但 "数据安全" = 1 (《网络数据安全管理条例》)；
//   - searchfield 留空（全文）：召回大但排序会漂（"新能源汽车补贴" 首条
//     是《扩大消费"十五五"规划》）。
// 所以走三级阶梯，每一级都只在上一级空手时才发：
//   1. 标题 + 原查询；
//   2. 标题 + 去掉「政策/补贴/通知」这类通用词后的查询（留下实体词）；
//   3. 全文 + 原查询（最后的兜底，宁可噪声也不要零结果）。
//
// 频率：公共检索端点，按 1 req/s 节流；只取标题/发文字号/摘要，不抓正文。

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// govTagRE 去掉结果里的高亮标签。stripHTML 会把标签换成空格，对中文会把
// 「房地产市场」拆成「房地产 市场」，所以先原地删。
var govTagRE = strings.NewReplacer("<em>", "", "</em>", "", "<EM>", "", "</EM>", "")

type govProvider struct {
	*base
	endpoint string
}

// newGov 构造国务院政策文件库源（直连，L2）。
func newGov(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = "https://sousuo.www.gov.cn/search-gov/data"
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase("gov", cfg, pc, eg, search.ModeDirect, search.TierL2, 1.1)
	b.kinds = search.KindSet(search.KindSearch)
	b.setMinInterval(time.Second)
	return &govProvider{base: b, endpoint: endpoint}, nil
}

// Search 先按标题检索，空结果再放宽到全文。
func (p *govProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	cctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()
	limit := p.limitOf(req.Limit)

	results, err := p.query(cctx, query, limit, "title")
	if err != nil {
		return nil, err
	}
	// 第 2 级: 标题 + 去掉通用词。文件标题是「关于<实体>的通知」，问句里
	// 的「政策/补贴/管理办法」几乎不会出现在同一个标题里，严格 AND 必空。
	if len(results) == 0 && len(strings.Fields(query)) > 1 {
		if alt := simplifyGovQuery(query); alt != "" && alt != query {
			if more, err := p.query(cctx, alt, limit, "title"); err == nil {
				results = more
			}
		}
	}
	// 第 3 级: 全文。排序会漂，但比零结果强。
	if len(results) == 0 && len(strings.Fields(query)) > 1 {
		if more, err := p.query(cctx, query, limit, ""); err == nil {
			results = more
		}
	}
	return finishResults(p.name, req, results, limit), nil
}

// govGenericWords 是问句里表达「我要一份文件」的词，不是文件标题的组成部分。
// 去掉它们能把「新能源汽车补贴 政策」收敛成「新能源汽车」，标题检索才命中。
var govGenericWords = []string{
	"政策", "法规", "条例", "办法", "规定", "细则", "通知", "意见", "公告",
	"规划", "补贴", "监管", "审批", "全文", "原文", "最新", "解读", "实施",
}

// simplifyGovQuery 去掉通用词与年份，只留实体词。
func simplifyGovQuery(q string) string {
	q = yearTokenRE.ReplaceAllString(q, " ")
	for _, w := range govGenericWords {
		q = strings.ReplaceAll(q, w, " ")
	}
	return strings.Join(strings.Fields(q), " ")
}

// query 执行一次政策文件检索。searchfield 为空表示全文检索。
func (p *govProvider) query(ctx context.Context, query string, limit int, searchfield string) ([]search.Result, error) {
	if err := p.pace(ctx); err != nil {
		return nil, search.Wrap(p.name, search.ErrTimeout, err)
	}
	params := url.Values{
		"t":           {"zhengcelibrary_gw"}, // 政策文件库（国务院/国办/部委）
		"q":           {query},
		"timetype":    {"timeqb"}, // 不限时间
		"sort":        {"score"},
		"sortType":    {"1"},
		"searchfield": {searchfield},
		"p":           {"1"},
		"n":           {strconv.Itoa(limit)},
	}
	var payload struct {
		Code     int    `json:"code"`
		Msg      string `json:"msg"`
		SearchVO *struct {
			TotalCount int `json:"totalCount"`
			ListVO     []struct {
				Title      string `json:"title"`
				URL        string `json:"url"`
				PubtimeStr string `json:"pubtimeStr"`
				Pcode      string `json:"pcode"`   // 发文字号, 如 国办发〔2026〕29号
				Summary    string `json:"summary"` // 摘要 (可能带高亮标签)
			} `json:"listVO"`
		} `json:"searchVO"`
	}
	if _, err := doJSON(ctx, p.client(), p.name, p.ua, http.MethodGet, withQuery(p.endpoint, params), http.Header{
		"Accept": {"application/json"},
	}, nil, &payload); err != nil {
		return nil, err
	}
	// 站点用 code=1001 表示「没有找到相关结果」, 不是错误。
	if payload.Code != 0 && payload.Code != 200 && payload.Code != 1001 {
		return nil, search.Wrap(p.name, search.ErrUnavailable, errors.New(strings.TrimSpace(payload.Msg)))
	}
	if payload.SearchVO == nil {
		return nil, nil
	}

	results := make([]search.Result, 0, len(payload.SearchVO.ListVO))
	for _, it := range payload.SearchVO.ListVO {
		link := upgradeHTTPS(strings.TrimSpace(it.URL))
		if link == "" {
			continue
		}
		title := strings.Join(strings.Fields(govTagRE.Replace(it.Title)), " ")
		if title == "" {
			title = link
		}
		// 发文字号是政策文件的关键标识（模型据此核对版本与效力），排在最前。
		snippet := title
		if pcode := strings.TrimSpace(it.Pcode); pcode != "" {
			snippet = pcode + "：" + title
		}
		if summary := strings.Join(strings.Fields(stripHTML(govTagRE.Replace(it.Summary))), " "); summary != "" {
			snippet = strings.TrimSpace(snippet + "。" + summary)
		}
		results = append(results, search.Result{
			URL:       link,
			Title:     title,
			Snippet:   snippetText(snippet, maxSnippetRunes),
			Published: parseGovDate(it.PubtimeStr),
			Lang:      "zh",
		})
	}
	return results, nil
}

// parseGovDate 解析站点的时间格式（2011.01.27 / 2011-01-27）。
func parseGovDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if i := strings.IndexAny(s, " "); i > 0 {
		s = s[:i] // "2026.10.23 10:00" 之类只取日期
	}
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, "/", "-")
	if t, err := time.Parse("2006-1-2", s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
