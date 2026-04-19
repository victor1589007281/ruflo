package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// KLineData 标准化 K 线数据。
type KLineData struct {
	Symbol string       `json:"symbol"`
	Name   string       `json:"name"`
	Period string       `json:"period"` // "daily"/"weekly"/"monthly"
	Data   []KLinePoint `json:"data"`
}

// KLinePoint 单根 K 线。
type KLinePoint struct {
	Date   string  `json:"date"`
	Open   float64 `json:"open"`
	Close  float64 `json:"close"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Volume float64 `json:"volume"`
	Amount float64 `json:"amount,omitempty"`
}

// FinanceClient 金融数据 API 客户端。
type FinanceClient struct {
	client *http.Client
}

func NewFinanceClient() *FinanceClient {
	return &FinanceClient{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

var reStockCode = regexp.MustCompile(`(?i)^(sh|sz|bj|hk|us)?\.?(\d{5,6}|[A-Z]{1,5})$`)

// IsFinanceQuery 检测是否是金融数据查询 (股票代码/K线关键词)。
func IsFinanceQuery(query string) bool {
	lower := strings.ToLower(query)
	if strings.Contains(lower, "k线") || strings.Contains(lower, "kline") ||
		strings.Contains(lower, "股票") || strings.Contains(lower, "行情") {
		return true
	}
	return reStockCode.MatchString(strings.TrimSpace(query))
}

// FetchKLine 获取 K 线数据 (默认东方财富 API)。
// symbol: "600519" (贵州茅台), "000001" (平安银行), "AAPL" (苹果)
// period: "daily", "weekly", "monthly"
// count: 返回数据条数
func (fc *FinanceClient) FetchKLine(ctx context.Context, symbol, period string, count int) (*KLineData, error) {
	if count <= 0 {
		count = 120
	}
	if period == "" {
		period = "daily"
	}

	code := normalizeStockCode(symbol)
	if code == "" {
		return nil, fmt.Errorf("无法识别的股票代码: %s", symbol)
	}

	klt := "101" // 日K
	switch period {
	case "weekly":
		klt = "102"
	case "monthly":
		klt = "103"
	}

	apiURL := fmt.Sprintf(
		"https://push2his.eastmoney.com/api/qt/stock/kline/get?"+
			"secid=%s&fields1=f1,f2,f3,f4,f5,f6&fields2=f51,f52,f53,f54,f55,f56,f57,f58&"+
			"klt=%s&fqt=1&end=20500101&lmt=%d",
		code, klt, count)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://quote.eastmoney.com/")

	resp, err := fc.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("东方财富 API 请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Data struct {
			Code   string   `json:"code"`
			Name   string   `json:"name"`
			Klines []string `json:"klines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	if len(result.Data.Klines) == 0 {
		return nil, fmt.Errorf("未获取到 %s 的 K 线数据", symbol)
	}

	kline := &KLineData{
		Symbol: result.Data.Code,
		Name:   result.Data.Name,
		Period: period,
	}

	for _, line := range result.Data.Klines {
		parts := strings.Split(line, ",")
		if len(parts) < 6 {
			continue
		}
		kline.Data = append(kline.Data, KLinePoint{
			Date:   parts[0],
			Open:   parseFloat(parts[1]),
			Close:  parseFloat(parts[2]),
			High:   parseFloat(parts[3]),
			Low:    parseFloat(parts[4]),
			Volume: parseFloat(parts[5]),
			Amount: safeParseFloat(parts, 6),
		})
	}

	return kline, nil
}

// FormatKLineText 将 K 线数据格式化为可读文本。
func FormatKLineText(kl *KLineData) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s (%s) %s K线\n\n", kl.Name, kl.Symbol, kl.Period))
	sb.WriteString("| 日期 | 开盘 | 收盘 | 最高 | 最低 | 成交量 |\n")
	sb.WriteString("|:---|:---|:---|:---|:---|:---|\n")

	start := 0
	if len(kl.Data) > 30 {
		start = len(kl.Data) - 30
	}
	for _, p := range kl.Data[start:] {
		sb.WriteString(fmt.Sprintf("| %s | %.2f | %.2f | %.2f | %.2f | %.0f |\n",
			p.Date, p.Open, p.Close, p.High, p.Low, p.Volume))
	}

	if len(kl.Data) > 0 {
		latest := kl.Data[len(kl.Data)-1]
		var prev KLinePoint
		if len(kl.Data) > 1 {
			prev = kl.Data[len(kl.Data)-2]
		}
		change := 0.0
		if prev.Close > 0 {
			change = (latest.Close - prev.Close) / prev.Close * 100
		}
		sb.WriteString(fmt.Sprintf("\n**最新**: %s 收盘 %.2f (涨跌 %+.2f%%)\n",
			latest.Date, latest.Close, change))
	}

	return sb.String()
}

// normalizeStockCode 将用户输入转换为东方财富 secid 格式。
func normalizeStockCode(symbol string) string {
	s := strings.TrimSpace(strings.ToUpper(symbol))

	if strings.HasPrefix(s, "SH") || strings.HasPrefix(s, "SZ") ||
		strings.HasPrefix(s, "BJ") {
		prefix := s[:2]
		code := s[2:]
		code = strings.TrimPrefix(code, ".")
		switch prefix {
		case "SH":
			return "1." + code
		case "SZ":
			return "0." + code
		case "BJ":
			return "0." + code
		}
	}

	// 6开头 = 上海, 0/3开头 = 深圳, 8/4开头 = 北交所
	if len(s) == 6 {
		switch {
		case strings.HasPrefix(s, "6"):
			return "1." + s
		case strings.HasPrefix(s, "0") || strings.HasPrefix(s, "3"):
			return "0." + s
		case strings.HasPrefix(s, "8") || strings.HasPrefix(s, "4"):
			return "0." + s
		}
	}

	return ""
}

func parseFloat(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

func safeParseFloat(parts []string, idx int) float64 {
	if idx >= len(parts) {
		return 0
	}
	return parseFloat(parts[idx])
}
