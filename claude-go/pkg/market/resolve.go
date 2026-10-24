package market

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// 本文件实现零成本的本地标的解析。解析只依赖交易所代码规则与内置名称表,
// 不访问网络: 未命中的输入返回 ErrUnresolved, 由上层决定是否用搜索补代码。
//
// 输入歧义是真实故障源: 000001 既是平安银行 (SZ) 也是上证指数 (SH),
// 因此裸 6 位数字按下列优先级解析, 指数的显式写法必须带 SH: 前缀。

// StaticResolver 是基于规则与内置名称表的解析器。
type StaticResolver struct {
	names map[string][]Instrument
}

// 内置名称表条目。一个名称可以映射到多个标的 (例如 A/H 双重上市), 此时
// Resolve 返回 ErrUnresolved, Candidates 返回全部候选。
type nameEntry struct {
	name string
	inst Instrument
}

// 市场到币种/时区的固定映射。
var (
	currencyByMarket = map[Market]string{
		MarketCN:     "CNY",
		MarketHK:     "HKD",
		MarketUS:     "USD",
		MarketCrypto: "USD",
		MarketMacro:  "USD",
	}
	timezoneByMarket = map[Market]string{
		MarketCN:     "Asia/Shanghai",
		MarketHK:     "Asia/Hong_Kong",
		MarketUS:     "America/New_York",
		MarketCrypto: "UTC",
		MarketMacro:  "UTC",
	}
)

// nameEntries 是内置常见标的名称表; 新增名称只需追加条目。
var nameEntries = []nameEntry{
	// A 股
	{"贵州茅台", Instrument{Market: MarketCN, Code: "600519", Name: "贵州茅台"}},
	{"茅台", Instrument{Market: MarketCN, Code: "600519", Name: "贵州茅台"}},
	{"平安银行", Instrument{Market: MarketCN, Code: "000001", Name: "平安银行"}},
	{"宁德时代", Instrument{Market: MarketCN, Code: "300750", Name: "宁德时代"}},
	{"比亚迪", Instrument{Market: MarketCN, Code: "002594", Name: "比亚迪"}},
	{"五粮液", Instrument{Market: MarketCN, Code: "000858", Name: "五粮液"}},
	{"招商银行", Instrument{Market: MarketCN, Code: "600036", Name: "招商银行"}},
	{"工商银行", Instrument{Market: MarketCN, Code: "601398", Name: "工商银行"}},
	{"中国平安", Instrument{Market: MarketCN, Code: "601318", Name: "中国平安"}},
	{"中信证券", Instrument{Market: MarketCN, Code: "600030", Name: "中信证券"}},
	{"万科A", Instrument{Market: MarketCN, Code: "000002", Name: "万科A"}},
	{"格力电器", Instrument{Market: MarketCN, Code: "000651", Name: "格力电器"}},
	{"美的集团", Instrument{Market: MarketCN, Code: "000333", Name: "美的集团"}},
	{"隆基绿能", Instrument{Market: MarketCN, Code: "601012", Name: "隆基绿能"}},
	{"紫金矿业", Instrument{Market: MarketCN, Code: "601899", Name: "紫金矿业"}},
	{"京东方A", Instrument{Market: MarketCN, Code: "000725", Name: "京东方A"}},
	{"东方财富", Instrument{Market: MarketCN, Code: "300059", Name: "东方财富"}},
	{"海康威视", Instrument{Market: MarketCN, Code: "002415", Name: "海康威视"}},
	{"立讯精密", Instrument{Market: MarketCN, Code: "002475", Name: "立讯精密"}},
	{"药明康德", Instrument{Market: MarketCN, Code: "603259", Name: "药明康德"}},
	{"中国石油", Instrument{Market: MarketCN, Code: "601857", Name: "中国石油"}},
	{"中国移动", Instrument{Market: MarketCN, Code: "600941", Name: "中国移动"}},
	{"恒瑞医药", Instrument{Market: MarketCN, Code: "600276", Name: "恒瑞医药"}},
	{"长江电力", Instrument{Market: MarketCN, Code: "600900", Name: "长江电力"}},
	{"兴业银行", Instrument{Market: MarketCN, Code: "601166", Name: "兴业银行"}},
	{"伊利股份", Instrument{Market: MarketCN, Code: "600887", Name: "伊利股份"}},
	{"三一重工", Instrument{Market: MarketCN, Code: "600031", Name: "三一重工"}},
	{"上汽集团", Instrument{Market: MarketCN, Code: "600104", Name: "上汽集团"}},
	{"中国中免", Instrument{Market: MarketCN, Code: "601888", Name: "中国中免"}},
	{"平安", Instrument{Market: MarketCN, Code: "000001", Name: "平安银行"}},
	{"平安", Instrument{Market: MarketCN, Code: "601318", Name: "中国平安"}},

	// 港股
	{"腾讯控股", Instrument{Market: MarketHK, Code: "00700", Name: "腾讯控股"}},
	{"腾讯", Instrument{Market: MarketHK, Code: "00700", Name: "腾讯控股"}},
	{"阿里巴巴", Instrument{Market: MarketHK, Code: "09988", Name: "阿里巴巴-W"}},
	{"阿里", Instrument{Market: MarketHK, Code: "09988", Name: "阿里巴巴-W"}},
	{"美团", Instrument{Market: MarketHK, Code: "03690", Name: "美团-W"}},
	{"小米集团", Instrument{Market: MarketHK, Code: "01810", Name: "小米集团-W"}},
	{"京东集团", Instrument{Market: MarketHK, Code: "09618", Name: "京东集团-SW"}},
	{"网易", Instrument{Market: MarketHK, Code: "09999", Name: "网易-S"}},
	{"快手", Instrument{Market: MarketHK, Code: "01024", Name: "快手-W"}},
	{"汇丰控股", Instrument{Market: MarketHK, Code: "00005", Name: "汇丰控股"}},
	{"友邦保险", Instrument{Market: MarketHK, Code: "01299", Name: "友邦保险"}},
	{"中国海洋石油", Instrument{Market: MarketHK, Code: "00883", Name: "中国海洋石油"}},

	// 美股
	{"苹果", Instrument{Market: MarketUS, Code: "AAPL", Name: "Apple"}},
	{"特斯拉", Instrument{Market: MarketUS, Code: "TSLA", Name: "Tesla"}},
	{"英伟达", Instrument{Market: MarketUS, Code: "NVDA", Name: "NVIDIA"}},
	{"微软", Instrument{Market: MarketUS, Code: "MSFT", Name: "Microsoft"}},
	{"谷歌", Instrument{Market: MarketUS, Code: "GOOGL", Name: "Alphabet"}},
	{"亚马逊", Instrument{Market: MarketUS, Code: "AMZN", Name: "Amazon"}},
	{"脸书", Instrument{Market: MarketUS, Code: "META", Name: "Meta"}},
	{"奈飞", Instrument{Market: MarketUS, Code: "NFLX", Name: "Netflix"}},
	{"超威半导体", Instrument{Market: MarketUS, Code: "AMD", Name: "AMD"}},
	{"英特尔", Instrument{Market: MarketUS, Code: "INTC", Name: "Intel"}},
	{"波音", Instrument{Market: MarketUS, Code: "BA", Name: "Boeing"}},
	{"高盛", Instrument{Market: MarketUS, Code: "GS", Name: "Goldman Sachs"}},
	{"摩根大通", Instrument{Market: MarketUS, Code: "JPM", Name: "JPMorgan"}},
	{"可口可乐", Instrument{Market: MarketUS, Code: "KO", Name: "Coca-Cola"}},
	{"麦当劳", Instrument{Market: MarketUS, Code: "MCD", Name: "McDonald's"}},
}

// cryptoCodes 是内置的加密资产代码。必须在"1-5 个字母 = 美股"规则之前判断,
// 否则 BTC 会被误解析成美股代码。
var cryptoCodes = map[string]string{
	"BTC":  "BTC",
	"ETH":  "ETH",
	"BNB":  "BNB",
	"SOL":  "SOL",
	"XRP":  "XRP",
	"DOGE": "DOGE",
	"ADA":  "ADA",
}

var (
	reExplicitPrefix = regexp.MustCompile(`(?i)^(SH|SZ|BJ|HK|US|CRYPTO)\s*[:.]\s*(.+)$`)
	reTightPrefix    = regexp.MustCompile(`(?i)^(SH|SZ|BJ|HK)([0-9]{4,6})$`)
	reUSCode         = regexp.MustCompile(`^[A-Za-z][A-Za-z.\-]{0,9}$`)
	reCryptoCode     = regexp.MustCompile(`^[A-Za-z]{2,10}$`)
)

// NewResolver 创建内置规则解析器。名称表在包初始化时构建一次, 解析本身无状态。
func NewResolver() *StaticResolver {
	r := &StaticResolver{names: make(map[string][]Instrument, len(nameEntries))}
	for _, e := range nameEntries {
		inst := e.inst
		inst.Canonical = canonicalOf(inst.Market, inst.Code)
		inst.Currency = currencyByMarket[inst.Market]
		inst.Timezone = timezoneByMarket[inst.Market]
		key := normalizeName(e.name)
		r.names[key] = append(r.names[key], inst)
	}
	return r
}

// Resolve 把用户输入解析成唯一规范标的。输入歧义或无法识别时返回 ErrUnresolved,
// 不静默取第一个候选。
func (r *StaticResolver) Resolve(ctx context.Context, input string) (Instrument, error) {
	if err := ctxErr(ctx); err != nil {
		return Instrument{}, err
	}
	cands := r.Candidates(ctx, input)
	switch len(cands) {
	case 0:
		return Instrument{}, Wrap("resolver", ErrUnresolved, fmt.Errorf("无法解析标的 %q", strings.TrimSpace(input)))
	case 1:
		return cands[0], nil
	default:
		names := make([]string, 0, len(cands))
		for _, c := range cands {
			names = append(names, c.Canonical)
		}
		return Instrument{}, Wrap("resolver", ErrUnresolved,
			fmt.Errorf("标的 %q 有多个候选: %s; 请使用 市场:代码 显式指定", strings.TrimSpace(input), strings.Join(names, ", ")))
	}
}

// Candidates 返回输入可能对应的全部标的。无法识别时返回 nil; 裸代码规则命中时
// 返回单元素列表 (000001 默认平安银行, 上证指数必须写 SH:000001)。
func (r *StaticResolver) Candidates(ctx context.Context, input string) []Instrument {
	if ctxErr(ctx) != nil {
		return nil
	}
	s := strings.TrimSpace(input)
	if s == "" {
		return nil
	}

	// 1) 显式前缀: SH:600519 / sh.600519 / US:AAPL / CRYPTO:BTC
	if m := reExplicitPrefix.FindStringSubmatch(s); m != nil {
		if inst, ok := instrumentForPrefix(m[1], m[2]); ok {
			return []Instrument{inst}
		}
		return nil
	}
	// 2) 紧贴前缀: SH600519 / HK00700
	if m := reTightPrefix.FindStringSubmatch(s); m != nil {
		if inst, ok := instrumentForPrefix(m[1], m[2]); ok {
			return []Instrument{inst}
		}
		return nil
	}

	// 3) 名称表 (大小写不敏感, 仅影响 ASCII 名称)
	if list, ok := r.names[normalizeName(s)]; ok {
		out := make([]Instrument, len(list))
		copy(out, list)
		return out
	}

	upper := strings.ToUpper(s)
	// 4) 加密资产必须在美股字母规则之前
	if code, ok := cryptoCodes[upper]; ok {
		return []Instrument{newInstrument(MarketCrypto, code, code)}
	}

	// 5) 6 位数字: 60/68→SH, 00/30→SZ, 8/4→BJ
	if len(s) == 6 && isDigits(s) {
		switch {
		case strings.HasPrefix(s, "60"), strings.HasPrefix(s, "68"):
			return []Instrument{newInstrument(MarketCN, s, "")}
		case strings.HasPrefix(s, "00"), strings.HasPrefix(s, "30"):
			return []Instrument{newInstrument(MarketCN, s, "")}
		case strings.HasPrefix(s, "8"), strings.HasPrefix(s, "4"):
			return []Instrument{newInstrument(MarketCN, s, "")}
		}
		return nil
	}

	// 6) 5 位数字 = 港股
	if len(s) == 5 && isDigits(s) {
		return []Instrument{newInstrument(MarketHK, s, "")}
	}

	// 7) 1-5 个字母 = 美股 (允许用户输入小写)
	if len(s) <= 5 && reUSCode.MatchString(s) && isASCIIAlpha(s) {
		return []Instrument{newInstrument(MarketUS, upper, "")}
	}

	return nil
}

// instrumentForPrefix 校验并构造带显式市场前缀的标的。
// 前缀是交易所 (SH/SZ/BJ/HK/US/CRYPTO), 不是 Market 常量: SH 与 SZ 都属于
// MarketCN, 但必须保留在 Canonical 里, 否则 SH:000001 (上证指数) 与
// SZ:000001 (平安银行) 无法区分。
func instrumentForPrefix(prefix, rawCode string) (Instrument, bool) {
	exchange := strings.ToUpper(prefix)
	code := strings.TrimSpace(rawCode)
	code = strings.TrimPrefix(code, ".")
	switch exchange {
	case "SH", "SZ", "BJ":
		// SH/SZ/BJ 都是 6 位数字。前缀必须与代码段一致, 但指数代码例外:
		// SH:000001 (上证指数) / SH:000300 (沪深300) / SZ:399001 (深证成指)
		// 无法仅凭代码段归属, 只能由显式前缀确定。
		if len(code) != 6 || !isDigits(code) {
			return Instrument{}, false
		}
		derived := prefixMarketOf(code)
		if derived != "" {
			// 个股代码必须与显式前缀一致; 指数代码允许前缀纠正。
			if derived != exchange && !isIndexCode(exchange, code) {
				return Instrument{}, false
			}
		} else if !isIndexCode(exchange, code) {
			return Instrument{}, false
		}
		return Instrument{
			Canonical: exchange + ":" + code,
			Market:    MarketCN,
			Code:      code,
			Currency:  currencyByMarket[MarketCN],
			Timezone:  timezoneByMarket[MarketCN],
		}, true
	case "HK":
		if !isDigits(code) || len(code) < 1 || len(code) > 5 {
			return Instrument{}, false
		}
		for len(code) < 5 {
			code = "0" + code
		}
		return newInstrument(MarketHK, code, ""), true
	case "US":
		for _, c := range code {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '-') {
				return Instrument{}, false
			}
		}
		if code == "" {
			return Instrument{}, false
		}
		return newInstrument(MarketUS, strings.ToUpper(code), ""), true
	case "CRYPTO":
		u := strings.ToUpper(code)
		if !reCryptoCode.MatchString(u) {
			return Instrument{}, false
		}
		return newInstrument(MarketCrypto, u, u), true
	}
	return Instrument{}, false
}

// isIndexCode 判断显式前缀下的是否为指数代码。上证指数/沪深300 等以 000
// 开头, 深证成指以 399 开头; 这类代码无法用个股前缀规则推导。
func isIndexCode(exchange, code string) bool {
	switch exchange {
	case "SH":
		return strings.HasPrefix(code, "000")
	case "SZ":
		return strings.HasPrefix(code, "399")
	}
	return false
}

// prefixMarketOf 返回 6 位 A 股代码对应的交易所前缀; 无法归属时返回空串。
func prefixMarketOf(code string) string {
	switch {
	case strings.HasPrefix(code, "60"), strings.HasPrefix(code, "68"):
		return "SH"
	case strings.HasPrefix(code, "00"), strings.HasPrefix(code, "30"):
		return "SZ"
	case strings.HasPrefix(code, "8"), strings.HasPrefix(code, "4"):
		return "BJ"
	}
	return ""
}

// canonicalOf 生成规范标识 (SH:600519 / SZ:000001 / HK:00700 / US:AAPL / CRYPTO:BTC)。
// A 股的交易所前缀由代码段推导; 显式 SH:000001 这类指数写法见 instrumentForPrefix。
func canonicalOf(m Market, code string) string {
	if m == MarketCN {
		if ex := prefixMarketOf(code); ex != "" {
			return ex + ":" + code
		}
	}
	return string(m) + ":" + code
}

// newInstrument 用统一的市场元数据填充 Instrument。
func newInstrument(m Market, code, name string) Instrument {
	return Instrument{
		Canonical: canonicalOf(m, code),
		Market:    m,
		Code:      code,
		Name:      name,
		Currency:  currencyByMarket[m],
		Timezone:  timezoneByMarket[m],
	}
}

// normalizeName 统一名称查表键。ASCII 名称大小写不敏感, 中文原样。
func normalizeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// isDigits 判断是否全为 ASCII 数字。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isASCIIAlpha 判断是否只含 ASCII 字母 (避免把全角字符当成美股代码)。
func isASCIIAlpha(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// ctxErr 把已取消/超时的上下文转成数据面错误。
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return Wrap("resolver", ErrTimeout, err)
	}
	return nil
}
