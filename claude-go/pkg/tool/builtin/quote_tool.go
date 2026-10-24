package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// QuoteTool 多数据源实时行情 + 上市状态核实工具。
// 借鉴 hermes finance-data: 聚合腾讯财经/新浪财经，交叉验证价格，
// 用于确认标的【是否已上市】并拿到真实最新价 —— 避免模型用旧知识误判"未上市"。
const QuoteToolName = "FetchQuote"

type QuoteTool struct{ client *http.Client }

// NewQuoteTool 构造行情工具, 内部 HTTP 客户端超时固定为 12s。
func NewQuoteTool() *QuoteTool {
	return &QuoteTool{client: &http.Client{Timeout: 12 * time.Second}}
}

// Name 返回工具名 FetchQuote。
func (t *QuoteTool) Name() string { return QuoteToolName }

// Description 返回工具说明, 含代码格式示例与"必须用本工具核实上市状态"的强约束。
func (t *QuoteTool) Description() string {
	return `多数据源实时行情 + 上市状态核实（A股/港股/美股）。
聚合腾讯财经、新浪财经交叉验证，返回最新价/涨跌/名称，并明确标注【是否已上市】。
⚠️ 判断一家公司是否上市、查实时股价时，必须用本工具核实，不要凭记忆断言"未上市"。
代码示例: 600519(A股) / hk00100 或 00100(港股) / NVDA(美股)。`
}

// InputSchema 返回输入 schema; 只有一个必填字段 symbol。
func (t *QuoteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"symbol":{"type":"string","description":"股票代码: 600519 / hk00100 / 00100 / NVDA"}},
		"required":["symbol"]
	}`)
}

// IsReadOnly 行情查询无副作用, 恒为只读。
func (t *QuoteTool) IsReadOnly(_ json.RawMessage) bool        { return true }

// IsConcurrencySafe 纯 HTTP 读且无共享状态, 可并发调用。
func (t *QuoteTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 只读查询不需要额外授权, 恒返回 nil 表示放行。
func (t *QuoteTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

type quoteInput struct {
	Symbol string `json:"symbol"`
}

type srcQuote struct {
	Source string
	Name   string
	Price  float64
	Chg    float64
	OK     bool
}

// Call 向多源行情接口取价并交叉验证, 返回最新价/涨跌与【是否已上市】的结论。
//
// 算法解读：
//
//	原理：向腾讯财经与新浪财经各取一次行情, 只要有一路解析成功就判定该标的已在交易,
//	      再用两路报价的离散程度给数据可信度打标 —— 存在的意义是让模型不必凭旧知识
//	      断言"未上市"。
//	关键不变量：**单源失败不等于未上市** —— 判定"已上市"用的是 len(srcs) > 0, 只有两路
//	      全空才走未上市分支; 把任一路的失败直接当成结论, 会把已上市标的判成未上市,
//	      而这正是本工具要消灭的错误。
//	代价与边界：两路是串行 HTTP, 最坏耗时约两个超时; 无缓存、无重试。价格一致性判据是
//	      (hi-lo)/hi < 5%, 对停牌/涨跌停/跨市场时差等极端行情会误报"存在差异"。
//	设计权衡：宁可多打一次网络也不缓存 —— 行情必须实时, 用旧价格误判上市状态的代价
//	      远高于一次请求; 因此也没有把"网络故障"与"确实查不到"分成两种返回。
func (t *QuoteTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in quoteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "输入解析错误", IsError: true}, nil
	}
	// 归一化失败 (空串) 视为输入错误, 此时不发起任何网络请求。
	code := normalizeQuoteCode(in.Symbol)
	if code == "" {
		return &tool.ToolResult{Content: "无法识别的代码: " + in.Symbol, IsError: true}, nil
	}

	var srcs []srcQuote
	if q := t.tencent(ctx, code); q.OK {
		srcs = append(srcs, q)
	}
	if q := t.sina(ctx, code); q.OK {
		srcs = append(srcs, q)
	}

	// 注意：这里返回的是**正常结果** (IsError=false), 调用方无法靠 IsError 区分
	// "两路都网络失败"与"确实未上市", 只能靠这段文案。
	if len(srcs) == 0 {
		return &tool.ToolResult{Content: fmt.Sprintf(
			"⚠️ 多源(腾讯/新浪)均未查到 %s 的实时行情。可能未上市或代码有误。"+
				"如需了解未上市公司，请用 WebSearch 查'融资/估值/IPO'。", in.Symbol)}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("✅ %s 【已上市交易】(多源核实)\n", srcs[0].Name))
	prices := make([]float64, 0, len(srcs))
	for _, q := range srcs {
		sb.WriteString(fmt.Sprintf("- %s: 最新价 %.3f  涨跌 %.2f%%\n", q.Source, q.Price, q.Chg))
		prices = append(prices, q.Price)
	}
	if len(prices) >= 2 {
		lo, hi := prices[0], prices[0]
		for _, p := range prices {
			if p < lo {
				lo = p
			}
			if p > hi {
				hi = p
			}
		}
		// 5% 是经验阈值: 既容得下不同数据源的报价时滞, 又能在真出现偏差时报警。
		if hi > 0 && (hi-lo)/hi < 0.05 {
			sb.WriteString("→ 多源价格一致，数据可信。\n")
		} else {
			sb.WriteString("→ 多源价格存在差异，请以最新成交为准。\n")
		}
	}
	return &tool.ToolResult{Content: sb.String()}, nil
}

// tencent 腾讯财经 qt.gtimg.cn (gb2312, ~ 分隔)
func (t *QuoteTool) tencent(ctx context.Context, code string) srcQuote {
	url := "https://qt.gtimg.cn/q=" + code
	body, err := t.get(ctx, url, "")
	if err != nil {
		return srcQuote{}
	}
	dec := simplifiedchinese.GBK.NewDecoder()
	utf8body, _ := dec.String(string(body))
	m := regexp.MustCompile(`v_[a-zA-Z0-9]+="(.+?)"`).FindStringSubmatch(utf8body)
	if m == nil {
		return srcQuote{}
	}
	vals := strings.Split(m[1], "~")
	if len(vals) < 4 || vals[3] == "" {
		return srcQuote{}
	}
	chg := 0.0
	if len(vals) > 32 {
		chg = parseFloatSafe(vals[32])
	}
	return srcQuote{Source: "腾讯财经", Name: vals[1], Price: parseFloatSafe(vals[3]), Chg: chg, OK: true}
}

// sina 新浪财经 hq.sinajs.cn
func (t *QuoteTool) sina(ctx context.Context, code string) srcQuote {
	body, err := t.get(ctx, "https://hq.sinajs.cn/list="+code, "https://finance.sina.com.cn")
	if err != nil {
		return srcQuote{}
	}
	dec := simplifiedchinese.GBK.NewDecoder()
	utf8body, _ := dec.String(string(body))
	m := regexp.MustCompile(`="(.+?)"`).FindStringSubmatch(utf8body)
	if m == nil || m[1] == "" {
		return srcQuote{}
	}
	parts := strings.Split(m[1], ",")
	// 港股: 名英,名中,开,昨收,高,低,现价... ; A股: 名,开,昨收,现价...
	if strings.HasPrefix(code, "hk") && len(parts) > 6 {
		return srcQuote{Source: "新浪财经", Name: parts[1], Price: parseFloatSafe(parts[6]), OK: true}
	}
	if (strings.HasPrefix(code, "sh") || strings.HasPrefix(code, "sz")) && len(parts) > 3 {
		prev := parseFloatSafe(parts[2])
		cur := parseFloatSafe(parts[3])
		chg := 0.0
		if prev > 0 {
			chg = (cur - prev) / prev * 100
		}
		return srcQuote{Source: "新浪财经", Name: parts[0], Price: cur, Chg: chg, OK: true}
	}
	return srcQuote{}
}

// get 发一次 GET 并读回原始 body; referer 非空时带上 (新浪不带 Referer 会返 403)。
//
// 注意：不检查 HTTP 状态码 —— 403/404 的响应体也会被原样返回, 由各解析函数
// 的正则不匹配兜底成"该源无数据"。
func (t *QuoteTool) get(ctx context.Context, url, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// normalizeQuoteCode 归一化为腾讯/新浪通用代码 (hk00100/sh600519/sz000001/usNVDA)
func normalizeQuoteCode(s string) string {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "hk") || strings.HasPrefix(low, "sh") ||
		strings.HasPrefix(low, "sz") || strings.HasPrefix(low, "us") {
		return low
	}
	isDigit := regexp.MustCompile(`^\d+$`).MatchString(s)
	if isDigit {
		switch len(s) {
		case 5:
			return "hk" + s
		case 6:
			if strings.HasPrefix(s, "6") || strings.HasPrefix(s, "9") {
				return "sh" + s
			}
			return "sz" + s
		}
		return s
	}
	return "us" + strings.ToUpper(s) // 字母 = 美股
}

// parseFloatSafe 尽力解析浮点数, 解析失败或非数字时返回 0 (不报错、不 panic)。
func parseFloatSafe(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	return f
}
