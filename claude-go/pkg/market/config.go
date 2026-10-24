package market

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是数据面的运行配置。
type Config struct {
	UserAgent        string
	Timeout          time.Duration
	SnapshotDeadline time.Duration
	VerifyThreshold  float64 // 跨源价差阈值 (相对值, 0.005 = 0.5%)
	DefaultAdjust    string
	QuoteTTL         time.Duration
	QuoteTTLClosed   time.Duration
	HistoryTTL       time.Duration
	FundamentalsTTL  time.Duration
	FilingsTTL       time.Duration
	MacroTTL         time.Duration
	Providers        map[string]ProviderConfig
}

// ProviderConfig 是单个数据源配置。
type ProviderConfig struct {
	Enabled   bool
	BaseURL   string
	APIKey    string
	UserAgent string
	Timeout   time.Duration
	QPS       float64
}

// DefaultConfig 返回零成本默认配置: 只启用免 key 源。
func DefaultConfig() Config {
	return Config{
		UserAgent:        "claude-go-market/1.0 (+https://github.com/victor1589007281/ruflo)",
		Timeout:          8 * time.Second,
		SnapshotDeadline: 3 * time.Second,
		VerifyThreshold:  0.005,
		DefaultAdjust:    AdjustQFQ,
		QuoteTTL:         5 * time.Second,
		QuoteTTLClosed:   60 * time.Second,
		HistoryTTL:       6 * time.Hour,
		FundamentalsTTL:  24 * time.Hour,
		FilingsTTL:       time.Hour,
		MacroTTL:         12 * time.Hour,
		Providers: map[string]ProviderConfig{
			"eastmoney": {Enabled: true, BaseURL: "https://push2.eastmoney.com", Timeout: 5 * time.Second},
			"tencent":   {Enabled: true, BaseURL: "https://qt.gtimg.cn", Timeout: 5 * time.Second},
			"sina":      {Enabled: true, BaseURL: "https://hq.sinajs.cn", Timeout: 5 * time.Second},
			"yahoo":     {Enabled: true, BaseURL: "https://query1.finance.yahoo.com", Timeout: 8 * time.Second},
			"stooq":     {Enabled: true, BaseURL: "https://stooq.com", Timeout: 8 * time.Second},
			"sec":       {Enabled: true, BaseURL: "https://data.sec.gov", Timeout: 8 * time.Second, QPS: 10},
			"binance":   {Enabled: true, BaseURL: "https://api.binance.com", Timeout: 6 * time.Second},
			"coingecko": {Enabled: true, BaseURL: "https://api.coingecko.com", Timeout: 8 * time.Second},
			"fred":      {Enabled: true, BaseURL: "https://api.stlouisfed.org", Timeout: 8 * time.Second},
			"worldbank": {Enabled: true, BaseURL: "https://api.worldbank.org", Timeout: 8 * time.Second},
		},
	}
}

// LoadConfig 从环境变量覆盖默认配置。
//
// 支持:
//
//	TUSHARE_TOKEN / WIND_API_KEY / FINNHUB_API_KEY / FRED_API_KEY / SEC_USER_AGENT
//	MARKET_TIMEOUT / MARKET_SNAPSHOT_DEADLINE / MARKET_VERIFY_THRESHOLD
//	MARKET_DEFAULT_ADJUST
func LoadConfig() Config {
	cfg := DefaultConfig()
	if v := os.Getenv("MARKET_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Timeout = d
		}
	}
	if v := os.Getenv("MARKET_SNAPSHOT_DEADLINE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.SnapshotDeadline = d
		}
	}
	if v := os.Getenv("MARKET_VERIFY_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			cfg.VerifyThreshold = f
		}
	}
	if v := os.Getenv("MARKET_DEFAULT_ADJUST"); v != "" {
		switch strings.ToLower(v) {
		case AdjustNone, AdjustQFQ, AdjustHFQ:
			cfg.DefaultAdjust = strings.ToLower(v)
		}
	}
	if v := os.Getenv("SEC_USER_AGENT"); v != "" {
		cfg.UserAgent = v
		cfg.Providers["sec"] = with(cfg.Providers["sec"], func(p *ProviderConfig) { p.UserAgent = v })
	}
	if v := os.Getenv("FRED_API_KEY"); v != "" {
		cfg.Providers["fred"] = with(cfg.Providers["fred"], func(p *ProviderConfig) { p.APIKey = v })
	}
	if v := os.Getenv("TUSHARE_TOKEN"); v != "" {
		cfg.Providers["tushare"] = ProviderConfig{
			Enabled: true, BaseURL: "https://api.tushare.pro", APIKey: v, Timeout: 10 * time.Second,
		}
	}
	return cfg
}

// with 复制 ProviderConfig 后应用修改, 避免 map 值共享带来的隐式改动。
func with(p ProviderConfig, fn func(*ProviderConfig)) ProviderConfig {
	fn(&p)
	return p
}
