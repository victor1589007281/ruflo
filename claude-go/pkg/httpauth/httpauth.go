// Package httpauth 提供 HTTP 接入面的共享密钥鉴权中间件。
//
// 为什么是包级中间件而不是逐路由包装：:18080 上的 /api/*（50 条）、/wiki/*、
// /sync/*、/cluster/*、SPA 与 /metrics 全部注册在同一个 http.ServeMux 上
// （pkg/wiki.APIServer.Mux()，由 pkg/dashboard.MountOn 与 pkg/cluster.Mount
// 复用）。历史上鉴权是逐路由 s.auth(...) 包装的，结果 /api/* 与后加的
// /cluster/* 都漏了——**逐路由包装的高度不对**。在 Handler 层统一拦一次，
// 新增路由默认被保护，不会再漏。
//
// 设计取舍：
//
//   - **fail-open**：Secret 为空时全部放行。这不是疏忽，而是兼容底线——
//     现有 8+ 下游平台（storyloom/mediaforge/testforge/growring/…）全部直连
//     :18080 且都不带鉴权头，且 design/02 已把 /api/* 路径与语义冻结为契约。
//     故本中间件上线时行为完全中性，只有显式配置 token 后才开始拦。
//   - **保护前缀白名单而非"全保护 + 例外"**：dashboard 的 SPA 用 "/" 兜底
//     路由，任意未匹配路径都返回 index.html。若采用"全保护 + 例外"，浏览器
//     访问 /teams 这类前端路由会拿到 401 而不是页面。故只保护明确的 API 前缀。
package httpauth

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// TokenHeader 是除标准 Authorization 外额外接受的头，便于不便设置
// Authorization 的调用方（如某些反向代理链路）传递 token。
const TokenHeader = "X-Claude-Go-Token"

// DefaultProtectPrefixes 是默认纳入鉴权的路径前缀。
// 不含 /metrics（Prometheus 抓取方不带鉴权头）与 SPA 静态资源。
var DefaultProtectPrefixes = []string{"/api/", "/wiki/", "/sync/", "/cluster/"}

// DefaultExemptPaths 是即使命中保护前缀也放行的精确路径。
// /api/health 必须豁免：dashboard 守护进程的健康探针
// （cmd/claude-go/main.go 的 daemon 探测）不带 token，且健康检查
// 本身不应依赖凭据。
var DefaultExemptPaths = []string{"/api/health"}

// AppHeader 是调用方自报 APP 身份的请求头。带了登记过的凭据（见 AppEntry）时
// 值以纯 appId 落 ctx（verified）；只带头不带凭据时中间件改写为 "claimed:<id>"
// 落 ctx —— 可观测但不可信，与 owner 的"自报不等于身份"同一原则。
const AppHeader = "X-App-Id"

// AppRefHeader 是调用方业务参考号（如指挥台工单号）的请求头，原样透传不做归一。
const AppRefHeader = "X-App-Ref"

// AppEntry 是一条 APP 凭据登记：token → appId。
// 登记过的 token 同时完成两件事——鉴权放行（等效于命中 Secret）与来源背书
// （ctx 里的 app 是纯 appId 而不是 claimed: 形态）。
type AppEntry struct {
	// App 是 APP 的稳定标识（studyforge / askforge / moqi / deck …），
	// 也是 metrics 与任务档案里的分组键，全仓唯一。
	App string `json:"app"`
	// Token 是该 APP 的凭据。与 Secret 同等对待：恒定时间比较，不明文写日志。
	Token string `json:"token"`
}

// Config 描述一次鉴权装配。
type Config struct {
	// Secret 为空 → 全部放行（见包注释的 fail-open 取舍）。
	Secret string
	// Apps 是 APP 凭据注册表（token → appId）。nil/空 = 没有 APP 凭据概念，
	// 此时 X-App-Id 一律落 "claimed:<id>"，行为对存量调用方完全中性。
	Apps []AppEntry
	// ProtectPrefixes 为 nil 时用 DefaultProtectPrefixes。
	ProtectPrefixes []string
	// ExemptPaths 为 nil 时用 DefaultExemptPaths。精确匹配。
	ExemptPaths []string
}

// prefixes 返回生效的保护前缀: 未配置时用 DefaultProtectPrefixes。
func (c Config) prefixes() []string {
	if c.ProtectPrefixes == nil {
		return DefaultProtectPrefixes
	}
	return c.ProtectPrefixes
}

// exempts 返回生效的豁免路径 (精确匹配): 未配置时用 DefaultExemptPaths。
func (c Config) exempts() []string {
	if c.ExemptPaths == nil {
		return DefaultExemptPaths
	}
	return c.ExemptPaths
}

// Protects 报告给定路径在本配置下是否需要鉴权。
// 导出以便测试与运维自检（例如启动时打印被保护的前缀）。
func (c Config) Protects(path string) bool {
	for _, e := range c.exempts() {
		if path == e {
			return false
		}
	}
	for _, p := range c.prefixes() {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// ExtractToken 从请求里取出 token。接受两种形式：
//
//	Authorization: Bearer <token>   （Bearer 前缀大小写不敏感）
//	X-Claude-Go-Token: <token>
//
// 兼容历史行为：Authorization 不带 Bearer 前缀时整个头值当作 token
// （pkg/wiki 原 auth() 就是 TrimPrefix，无前缀等价于原值）。
func ExtractToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(TokenHeader)); v != "" {
		return v
	}
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(v) >= 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}

// Middleware 返回按 cfg 拦截 + 来源 stamping 的中间件。
//
// 与旧版"Secret 为空时返回恒等包装"不同， stamping **总是**发生：
// 8+ 下游平台目前全部 fail-open 直连（不带任何凭据），恰恰是这个部署形态
// 最需要把"谁在调用"标出来给任务档案/metrics。stamping 只往 ctx 里写一个
// 归一化字符串，不改任何请求语义，对存量调用方行为完全中性。
//
// 每个请求的处理分两半：
//  1. stamping（在鉴权分支之前）：token 命中注册表 → verified appId；
//     只带 X-App-Id → claimed:<id>；否则 unknown。另把 X-App-Ref 原样带上。
//  2. 鉴权（语义与旧版逐字节一致地保留）：Secret 为空 → 全放行；
//     否则 token == Secret 或 token 命中注册表 → 放行；其余 401。
//     注册表 token 放行意味着"持 per-app token 的调用方不必再知道全局 Secret"，
//     这是把存量共享 Secret 迁移成 per-app 凭据的踏板。
func Middleware(cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = stampApp(r, cfg.Apps)

			if cfg.Secret == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !cfg.Protects(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			got := []byte(ExtractToken(r))
			if tokenAllowed(got, cfg) {
				next.ServeHTTP(w, r)
				return
			}
			// 恒定时间比较，避免按字节比较泄漏 token 前缀。
			w.Header().Set("WWW-Authenticate", `Bearer realm="claude-go"`)
			writeUnauthorized(w)
		})
	}
}

// tokenAllowed 报告 token 是否放行：命中全局 Secret 或注册表中任一 APP 凭据。
// 每条候选都走恒定时间比较；长度不等时 ConstantTimeCompare 直接返回 0，
// 显式判长只为可读性；各候选之间不提前短路到逐字节比较。
func tokenAllowed(got []byte, cfg Config) bool {
	if len(got) == 0 {
		return false
	}
	if want := []byte(cfg.Secret); cfg.Secret != "" &&
		len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1 {
		return true
	}
	for _, e := range cfg.Apps {
		if want := []byte(e.Token); len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1 {
			return true
		}
	}
	return false
}

// stampApp 解析请求来源并写入 ctx，返回带新 ctx 的请求。
// 信任级别：注册表 token → 纯 appId（verified）；仅 X-App-Id 头 → claimed:<id>；
// 都没有 → unknown。AppRef 头原样透传（调用方自由文本，不归一）。
func stampApp(r *http.Request, apps []AppEntry) *http.Request {
	token := ExtractToken(r)
	for _, e := range apps {
		if e.Token != "" && len(token) == len(e.Token) &&
			subtle.ConstantTimeCompare([]byte(token), []byte(e.Token)) == 1 {
			ctx := types.WithApp(r.Context(), e.App)
			ctx = types.WithAppRef(ctx, strings.TrimSpace(r.Header.Get(AppRefHeader)))
			return r.WithContext(ctx)
		}
	}
	app := types.DefaultAppID
	if hdr := strings.TrimSpace(r.Header.Get(AppHeader)); hdr != "" {
		app = types.ClaimedApp(hdr)
	}
	ctx := types.WithApp(r.Context(), app)
	ctx = types.WithAppRef(ctx, strings.TrimSpace(r.Header.Get(AppRefHeader)))
	return r.WithContext(ctx)
}

// writeUnauthorized 写 401 响应, 形态与 pkg/wiki 原 auth() 保持一致, 避免下游解析逻辑分叉。
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// 与 pkg/wiki 原 auth() 的响应体保持一致，避免下游解析逻辑分叉。
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}

// IsLoopbackAddr 判断监听地址是否只绑本机回环。
// 空 host（":18080" / "0.0.0.0:18080" / "[::]:18080"）都视为对外暴露。
func IsLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// WarnIfExposed 在"绑非回环地址且未配置 token"时打一条显著 WARN。
// 这两个条件单独都可接受（本机回环无鉴权、或对外但有 token），
// 同时成立才是真实暴露面。name 用于区分调用方（如 "wiki-api"/"dashboard"）。
func WarnIfExposed(name, addr, secret string) {
	exposed := !IsLoopbackAddr(addr)
	if !exposed {
		return
	}
	if secret == "" {
		log.Printf("[%s] ⚠️  安全警告: 监听 %s（非回环，可从局域网/tailscale 访问）"+
			"且未配置鉴权 token —— /api/*、/wiki/*、/sync/*、/cluster/* 全部对外开放。"+
			"请设置 wiki.apiSecret，或把监听地址收回 127.0.0.1（wiki.apiHost）。", name, addr)
		return
	}
	log.Printf("[%s] 监听 %s（非回环），已启用 token 鉴权。", name, addr)
}
