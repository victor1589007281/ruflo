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

// Config 描述一次鉴权装配。
type Config struct {
	// Secret 为空 → 全部放行（见包注释的 fail-open 取舍）。
	Secret string
	// ProtectPrefixes 为 nil 时用 DefaultProtectPrefixes。
	ProtectPrefixes []string
	// ExemptPaths 为 nil 时用 DefaultExemptPaths。精确匹配。
	ExemptPaths []string
}

func (c Config) prefixes() []string {
	if c.ProtectPrefixes == nil {
		return DefaultProtectPrefixes
	}
	return c.ProtectPrefixes
}

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

// Middleware 返回按 cfg 拦截的中间件。Secret 为空时返回恒等包装。
func Middleware(cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if cfg.Secret == "" {
			return next
		}
		want := []byte(cfg.Secret)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Protects(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			got := []byte(ExtractToken(r))
			// 恒定时间比较，避免按字节比较泄漏 token 前缀。
			// 长度不等时 ConstantTimeCompare 直接返回 0，这里显式判长
			// 只为可读性；两条路径都不提前短路到逐字节比较。
			if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="claude-go"`)
				writeUnauthorized(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

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
