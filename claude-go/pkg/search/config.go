package search

// config.go —— 检索数据面的配置装载与默认值。
//
// 配置分三层, 优先级从高到低:
//  1. 装配层注入的进程级值 (目前只有 config.json 的 search.githubToken);
//  2. SEARCH_* 环境变量 (出口/超时/缓存/层级);
//  3. DefaultConfig 的零成本默认值。
//
// 这里刻意不读 HTTP_PROXY/HTTPS_PROXY: 宿主环境的代理变量会与"按 provider
// 钉死出口"互相污染, 出口只认 SEARCH_PROXY_URL。
import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// configuredGitHubToken 是装配层从 config.json 的 search.githubToken 注入的 token。
//
// 为什么要一个进程级变量而不是参数: 数据面是进程级单例 (builtin.DefaultDataStack
// 用 sync.Once 装配), 而 LoadConfig 的调用点分散在工具注册、MCP server 与子命令里。
// 让每个调用点都去读一遍 config.json 会把"读哪一份配置"的分叉带进来; 由装配层
// 在第一次使用数据面之前注入一次, 只有一处来源。
//
// 用读写锁而不是普通变量: dashboard/serve 会在运行期热加载配置并重新注入,
// 与正在执行的搜索并发。空值表示"配置没给", 此时回退到环境变量以兼容尚未迁移的
// 旧部署; 环境变量不再是配置入口。
var (
	githubTokenMu         sync.RWMutex
	configuredGitHubToken string
)

// SetGitHubToken 注入 config.json 的 search.githubToken。
//
// 传空字符串表示清除配置值; LoadConfig 会回退到 GITHUB_TOKEN 环境变量。
func SetGitHubToken(token string) {
	githubTokenMu.Lock()
	configuredGitHubToken = strings.TrimSpace(token)
	githubTokenMu.Unlock()
}

// githubAPIToken 返回当前生效的 GitHub token: 配置优先, 环境变量兜底。
func githubAPIToken() string {
	githubTokenMu.RLock()
	tok := configuredGitHubToken
	githubTokenMu.RUnlock()
	if tok != "" {
		return tok
	}
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

// Config 是检索服务的运行配置。零值可用: DefaultConfig 给出一套零成本配置。
type Config struct {
	MaxTier      Tier
	MaxCostUSD   float64
	DefaultLimit int
	UserAgent    string

	Egress EgressConfig

	SearchTimeout time.Duration // 单次 search 硬超时
	FetchTimeout  time.Duration // 单次 fetch 硬超时
	HedgeDelay    time.Duration // 对冲延迟

	CacheTTL        time.Duration
	CacheFreshTTL   time.Duration
	CacheMaxEntries int

	// Providers 以名字为键。未出现的 provider 使用其构造时的默认值。
	Providers map[string]ProviderConfig
}

// ProviderConfig 是单源配置。
type ProviderConfig struct {
	Enabled  bool
	Egress   Mode
	BaseURL  string
	APIKey   string
	Weight   float64
	Tier     Tier
	Timeout  time.Duration
	Disabled bool
}

// EgressConfig 是出口配置。
type EgressConfig struct {
	Mode          Mode
	ProxyURL      string
	NoProxy       []string
	ProbeURL      string
	Interval      time.Duration
	Timeout       time.Duration
	FailThreshold int
	RecoverAfter  time.Duration
}

// DefaultConfig 返回零成本默认配置。
//
// 出口默认 auto: 配了代理就用代理探测结果, 没配就全直连。
// 付费层默认不可达。
func DefaultConfig() Config {
	return Config{
		MaxTier:         TierL2,
		MaxCostUSD:      0,
		DefaultLimit:    8,
		UserAgent:       "claude-go-search/1.0 (+https://github.com/victor1589007281/ruflo)",
		SearchTimeout:   4 * time.Second,
		FetchTimeout:    8 * time.Second,
		HedgeDelay:      400 * time.Millisecond,
		CacheTTL:        10 * time.Minute,
		CacheFreshTTL:   60 * time.Second,
		CacheMaxEntries: 5000,
		Egress: EgressConfig{
			Mode:          ModeAuto,
			ProxyURL:      "",
			NoProxy:       []string{"127.0.0.1", "localhost", "::1"},
			ProbeURL:      "https://example.com",
			Interval:      30 * time.Second,
			Timeout:       2 * time.Second,
			FailThreshold: 3,
			RecoverAfter:  60 * time.Second,
		},
		Providers: map[string]ProviderConfig{},
	}
}

// LoadConfig 从环境变量与装配层注入的 config.json search 段加载配置; 未设置的项保持默认。
//
// 支持的环境变量:
//
//	SEARCH_PROXY_URL / SEARCH_PROXY_MODE(direct|proxy|auto) / SEARCH_NO_PROXY
//	SEARCH_MAX_TIER(L0..L3) / SEARCH_MAX_COST_USD
//	SEARCH_TIMEOUT / SEARCH_FETCH_TIMEOUT / SEARCH_HEDGE_DELAY
//	SEARCH_CACHE_TTL / SEARCH_CACHE_ENTRIES
//	PARALLEL_URL / EXA_API_KEY / TAVILY_API_KEY / FIRECRAWL_API_KEY / JINA_API_KEY
//	GITHUB_TOKEN (仅兼容旧部署; config.json 的 search.githubToken 优先)
//
// 本服务不读取 HTTP_PROXY/HTTPS_PROXY, 避免与宿主环境互相污染。
func LoadConfig() Config {
	cfg := DefaultConfig()

	if v := os.Getenv("SEARCH_PROXY_URL"); v != "" {
		cfg.Egress.ProxyURL = v
	}
	if v := os.Getenv("SEARCH_PROXY_MODE"); v != "" {
		cfg.Egress.Mode = Mode(strings.ToLower(v))
	}
	if v := os.Getenv("SEARCH_NO_PROXY"); v != "" {
		cfg.Egress.NoProxy = splitList(v)
	}
	// 代理自身必须绕过代理, 否则会成环。
	if cfg.Egress.ProxyURL != "" {
		if host := hostOf(cfg.Egress.ProxyURL); host != "" {
			cfg.Egress.NoProxy = appendUnique(cfg.Egress.NoProxy, host)
		}
	}

	if v := os.Getenv("SEARCH_MAX_TIER"); v != "" {
		cfg.MaxTier = parseTier(v)
	}
	if v := os.Getenv("SEARCH_MAX_COST_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MaxCostUSD = f
		}
	}
	cfg.SearchTimeout = durationEnv("SEARCH_TIMEOUT", cfg.SearchTimeout)
	cfg.FetchTimeout = durationEnv("SEARCH_FETCH_TIMEOUT", cfg.FetchTimeout)
	cfg.HedgeDelay = durationEnv("SEARCH_HEDGE_DELAY", cfg.HedgeDelay)
	cfg.CacheTTL = durationEnv("SEARCH_CACHE_TTL", cfg.CacheTTL)
	if v := os.Getenv("SEARCH_CACHE_ENTRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.CacheMaxEntries = n
		}
	}

	// 常用 provider 的最小配置; 未启用 key 的源由构造器跳过。
	cfg.Providers["parallel"] = ProviderConfig{
		Enabled: true, Egress: ModeDirect,
		BaseURL: envOr("PARALLEL_URL", "https://search.parallel.ai/mcp"),
		Weight:  1.0, Tier: TierL0,
	}
	cfg.Providers["exa_anon"] = ProviderConfig{
		Enabled: true, Egress: ModeDirect,
		BaseURL: envOr("EXA_MCP_URL", "https://mcp.exa.ai/mcp"),
		Weight:  0.9, Tier: TierL0,
	}
	cfg.Providers["exa"] = ProviderConfig{
		Enabled: os.Getenv("EXA_API_KEY") != "",
		APIKey:  os.Getenv("EXA_API_KEY"), Egress: ModeDirect,
		BaseURL: "https://api.exa.ai", Weight: 1.0, Tier: TierL1,
	}
	cfg.Providers["tavily"] = ProviderConfig{
		Enabled: os.Getenv("TAVILY_API_KEY") != "",
		APIKey:  os.Getenv("TAVILY_API_KEY"), Egress: ModeDirect,
		BaseURL: "https://api.tavily.com", Weight: 1.0, Tier: TierL1,
	}
	cfg.Providers["bing_rss"] = ProviderConfig{
		Enabled: true, Egress: ModeDirect, Weight: 0.6, Tier: TierL2,
	}
	cfg.Providers["wikipedia"] = ProviderConfig{
		Enabled: true, Egress: ModeProxy, Weight: 0.8, Tier: TierL2,
	}
	cfg.Providers["gnews"] = ProviderConfig{
		Enabled: true, Egress: ModeProxy, Weight: 0.7, Tier: TierL2,
	}

	// 领域直连源: 全部免 key, 只在其擅长的意图上被路由选中 (见 router.go)。
	// 端点与权重由各构造函数内置, 这里只负责启用。
	for name, weight := range map[string]float64{
		"github":        1.0,
		"stackexchange": 1.0,
		"openalex":      1.0,
		"crossref":      1.0,
		"pubmed":        1.0,
		"hn":            0.7,
		"npm":           1.0,
		"pypi":          1.0,
	} {
		cfg.Providers[name] = ProviderConfig{Enabled: true, Egress: ModeDirect, Weight: weight, Tier: TierL2}
	}
	if v := githubAPIToken(); v != "" {
		p := cfg.Providers["github"]
		p.APIKey = v
		cfg.Providers["github"] = p
	}
	return cfg
}

// parseTier 解析 L0..L3; 非法值回落到 L2。
func parseTier(s string) Tier {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "L0":
		return TierL0
	case "L1":
		return TierL1
	case "L2":
		return TierL2
	case "L3":
		return TierL3
	default:
		return TierL2
	}
}

// durationEnv 读取时长环境变量, 未设置或非法时返回 def。
func durationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// envOr 返回环境变量值或默认值。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitList 按逗号切分并去空白。
func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// appendUnique 追加不重复的字符串。
func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}
