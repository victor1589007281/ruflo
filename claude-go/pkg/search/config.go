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
	"math"
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

	// RedisURL 非空才启用跨 Manager 缓存。KeyPrefix 必须由同一组副本共享；
	// StaleTTL 是 fresh TTL 之后额外保留全文的时间，不影响正常命中判据。
	RedisURL       string
	RedisKeyPrefix string
	RedisTimeout   time.Duration
	RedisStaleTTL  time.Duration

	// SharedBudget* 是独立于 MaxCostUSD 的跨副本付费总量预算。Limit=0
	// 关闭共享预算；开启后付费源必须经 Redis 原子预占，故障时 fail-closed。
	// ReservationTTL 只清理未知结局的 reservation 元数据，不提前释放窗口额度。
	SharedBudgetLimitUSD       float64
	SharedBudgetWindow         time.Duration
	SharedBudgetReservationTTL time.Duration

	// Providers 以名字为键。未出现的 provider 使用其构造时的默认值。
	Providers map[string]ProviderConfig

	// ProviderPriority 是调用方自定义的源优先级（按名字，越靠前越先调度）。
	//
	// 非空时覆盖按意图/语言推导的偏好表；表外源仍按 tier/权重参与排序，
	// 不会被排除。给调用方这个开关的原因是: 不同产品的检索负载差异很大
	// (一次取证会连发 15+ 条查询), 而匿名源的免费额度差异也很大——
	// 2026-11-14 实测 Parallel 免费层在第 3 次调用即触发限流, Exa 匿名源
	// 连续 14 次全部成功。固定按"理想质量"排序会让产品侧反复打到限流源。
	// 空值保持原有行为, 不影响既有调用方。
	ProviderPriority []string

	// IntentPriority 按意图覆盖源顺序, 键为 "intent" 或 "intent:lang"
	// (如 "academic:zh", "data", "news")。查找顺序:
	// "intent:lang" -> "intent" -> ProviderPriority -> 内置偏好表。
	//
	// 为什么不只用一个全局列表: 专业数据源(世界银行/巨潮/DOAJ)只在特定意图
	// 下该排第一, 全局覆盖会让它们污染日常事实查询; 而把排序逻辑全塞进
	// pkg/search 又会让产品无法按自己的额度情况调整。按意图分层是两者的折中。
	IntentPriority map[string][]string

	// RemoteRender 是远程 JS 渲染兜底 (Firecrawl MCP keyless/带 key)。
	// 本地 Chromium 渲染失败 (无浏览器/超时/挑战页) 时按配置尝试。
	RemoteRender RemoteRenderConfig

	// WaybackFallback 允许 Fetch 在原站 4xx/5xx/超时时回退到
	// Wayback Machine 最近快照 (web.archive.org 需出口可达, 默认关闭)。
	WaybackFallback bool
}

// RemoteRenderConfig 配置远程渲染服务 (当前实现为 Firecrawl MCP 端点)。
//
// Enabled=false 时完全关闭; APIKey 为空走 keyless 档 (有服务端日额度,
// 超额返回 429, 由渲染链路的调用方按 unavailable 处理)。Endpoint 默认
// Firecrawl 托管 MCP; 也可指向自托管 firecrawl-mcp 或其它实现了同形
// tools/call 协议的端点。
type RemoteRenderConfig struct {
	Enabled  bool
	Endpoint string
	APIKey   string
	Timeout  time.Duration
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

	// CostEstimateUSD 是每次真实 Search 调用前的预占估算，不是实际成本；
	// 实际成本由 SearchCostReporter 或 Result.Cost 在完成时结算。
	CostEstimateUSD float64
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
		MaxTier:                    TierL2,
		MaxCostUSD:                 0,
		DefaultLimit:               8,
		UserAgent:                  "claude-go-search/1.0 (+https://github.com/victor1589007281/ruflo)",
		SearchTimeout:              4 * time.Second,
		FetchTimeout:               8 * time.Second,
		HedgeDelay:                 400 * time.Millisecond,
		CacheTTL:                   10 * time.Minute,
		CacheFreshTTL:              60 * time.Second,
		CacheMaxEntries:            5000,
		RedisKeyPrefix:             defaultRedisKeyPrefix,
		RedisTimeout:               defaultRedisTimeout,
		RedisStaleTTL:              defaultRedisStaleTTL,
		SharedBudgetWindow:         defaultSharedBudgetWindow,
		SharedBudgetReservationTTL: defaultSharedBudgetReservationTTL,
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
//	SEARCH_REDIS_URL / SEARCH_REDIS_KEY_PREFIX / SEARCH_REDIS_TIMEOUT
//	SEARCH_REDIS_STALE_TTL (fresh 过期后额外保留全文，供临时故障降级)
//	SEARCH_SHARED_BUDGET_LIMIT_USD / SEARCH_SHARED_BUDGET_WINDOW
//	SEARCH_SHARED_BUDGET_RESERVATION_TTL
//	SEARCH_PROVIDER_COST_ESTIMATES_USD (例: exa=0.01,tavily=0.005)
//	PARALLEL_URL / EXA_API_KEY / TAVILY_API_KEY / FIRECRAWL_API_KEY / JINA_API_KEY
//	GITHUB_TOKEN (仅兼容旧部署; config.json 的 search.githubToken 优先)
//	SEARCH_REMOTE_RENDER(1 开启) / SEARCH_REMOTE_RENDER_URL / SEARCH_REMOTE_RENDER_TIMEOUT
//	SEARCH_WAYBACK_FALLBACK(1 开启)
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
	cfg.RedisURL = strings.TrimSpace(os.Getenv("SEARCH_REDIS_URL"))
	if v := strings.TrimSpace(os.Getenv("SEARCH_REDIS_KEY_PREFIX")); v != "" {
		cfg.RedisKeyPrefix = v
	}
	cfg.RedisTimeout = durationEnv("SEARCH_REDIS_TIMEOUT", cfg.RedisTimeout)
	cfg.RedisStaleTTL = durationEnv("SEARCH_REDIS_STALE_TTL", cfg.RedisStaleTTL)
	if v := os.Getenv("SEARCH_SHARED_BUDGET_LIMIT_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.SharedBudgetLimitUSD = f
		}
	}
	cfg.SharedBudgetWindow = durationEnv("SEARCH_SHARED_BUDGET_WINDOW", cfg.SharedBudgetWindow)
	cfg.SharedBudgetReservationTTL = durationEnv("SEARCH_SHARED_BUDGET_RESERVATION_TTL", cfg.SharedBudgetReservationTTL)

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
	//
	// 专业公开库 (arxiv/doaj/datacite/zenodo/worldbank/cninfo) 也在这张表里:
	// router.go 的意图计划早就点名要用它们, 但**不登记就构造不出来** ——
	// 计划里的名字会被静默跳过, 表现为"学术/数据/公告类问题永远只有通用网页结果"。
	// 权重与各 provider 内置默认值保持一致 (worldbank 1.1 / zenodo 0.9), 否则
	// newBase 会被这里的 0 值/偏低值覆盖, 面板显示的权重也不是实际生效值。
	for name, weight := range map[string]float64{
		"github":        1.0,
		"stackexchange": 1.0,
		"techdocs":      1.0,
		"openalex":      1.0,
		"openreview":    1.0,
		"crossref":      1.0,
		"pubmed":        1.0,
		"hn":            0.7,
		"npm":           1.0,
		"pypi":          1.0,
		"arxiv":         1.0,
		"doaj":          1.0,
		"datacite":      1.0,
		"zenodo":        0.9,
		"worldbank":     1.1,
		"cninfo":        1.0,
		"gov":           1.1,
		"gdelt":         0.7,
		"douban":        0.6,
		"weibo":         0.6,
	} {
		cfg.Providers[name] = ProviderConfig{Enabled: true, Egress: ModeDirect, Weight: weight, Tier: TierL2}
	}
	if v := githubAPIToken(); v != "" {
		p := cfg.Providers["github"]
		p.APIKey = v
		cfg.Providers["github"] = p
	}
	loadProviderCostEstimates(&cfg, os.Getenv("SEARCH_PROVIDER_COST_ESTIMATES_USD"))

	// 反爬/归档渠道: 默认全关, 按需以环境变量开启 (见 remote_render.go /
	// fetch.go 顶部说明各自的适用面与出口要求)。
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("SEARCH_REMOTE_RENDER"))); v == "1" || v == "true" || v == "on" {
		cfg.RemoteRender.Enabled = true
	}
	cfg.RemoteRender.Endpoint = envOr("SEARCH_REMOTE_RENDER_URL", "https://mcp.firecrawl.dev/v2/mcp")
	cfg.RemoteRender.APIKey = os.Getenv("FIRECRAWL_API_KEY") // 空 = keyless 档
	cfg.RemoteRender.Timeout = durationEnv("SEARCH_REMOTE_RENDER_TIMEOUT", 30*time.Second)
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("SEARCH_WAYBACK_FALLBACK"))); v == "1" || v == "true" || v == "on" {
		cfg.WaybackFallback = true
	}

	// 面板的源覆盖 (启用/停用/权重/顺序) 在**进程启动时**合并一次: 见 sources.go
	// 顶部关于"为什么不做热更新"的说明。
	applySourcesFile(&cfg)
	// 使用记录: 目录不可用时自动降级为不记录, 不影响检索。
	InitTelemetry()
	return cfg
}

// SourcesSnapshotPath 供装配层日志/诊断使用, 返回生效快照文件路径。
func SourcesSnapshotPath() string { return EffectiveSourcesPath() }

// loadProviderCostEstimates 只设置预占估算，不改变 provider 的启用状态。
// 未出现过的自定义源按既有缺省语义视为启用；非法金额忽略并保持零成本。
func loadProviderCostEstimates(cfg *Config, raw string) {
	for _, item := range splitList(raw) {
		name, value, ok := strings.Cut(item, "=")
		name = strings.TrimSpace(name)
		cost, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if !ok || name == "" || err != nil || cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
			continue
		}
		p, exists := cfg.Providers[name]
		if !exists {
			p.Enabled = true
		}
		p.CostEstimateUSD = cost
		cfg.Providers[name] = p
	}
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
