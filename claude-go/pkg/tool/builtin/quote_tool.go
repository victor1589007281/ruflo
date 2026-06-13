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

func NewQuoteTool() *QuoteTool {
	return &QuoteTool{client: &http.Client{Timeout: 12 * time.Second}}
}

func (t *QuoteTool) Name() string { return QuoteToolName }

func (t *QuoteTool) Description() string {
	return `多数据源实时行情 + 上市状态核实（A股/港股/美股）。
聚合腾讯财经、新浪财经交叉验证，返回最新价/涨跌/名称，并明确标注【是否已上市】。
⚠️ 判断一家公司是否上市、查实时股价时，必须用本工具核实，不要凭记忆断言"未上市"。
代码示例: 600519(A股) / hk00100 或 00100(港股) / NVDA(美股)。`
}

func (t *QuoteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"symbol":{"type":"string","description":"股票代码: 600519 / hk00100 / 00100 / NVDA"}},
		"required":["symbol"]
	}`)
}

func (t *QuoteTool) IsReadOnly(_ json.RawMessage) bool       { return true }
func (t *QuoteTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
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

func (t *QuoteTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in quoteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "输入解析错误", IsError: true}, nil
	}
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

func parseFloatSafe(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	return f
}
