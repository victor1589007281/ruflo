package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestMiddleware(t *testing.T) {
	const secret = "s3cr3t-token"

	cases := []struct {
		name   string
		secret string
		path   string
		header [2]string // [名, 值]；名为空表示不带头
		want   int
	}{
		// fail-open：未配置 token 时一律放行（这是 8+ 下游平台的兼容底线）
		{"未配置token_受保护路径_无头", "", "/api/teams", [2]string{}, http.StatusOK},
		{"未配置token_受保护路径_乱填头", "", "/api/teams", [2]string{"Authorization", "Bearer wrong"}, http.StatusOK},
		{"未配置token_cluster", "", "/cluster/enqueue", [2]string{}, http.StatusOK},

		// 配置 token 后，受保护前缀必须带正确 token
		{"有token_api_无头", secret, "/api/teams", [2]string{}, http.StatusUnauthorized},
		{"有token_api_错token", secret, "/api/teams", [2]string{"Authorization", "Bearer nope"}, http.StatusUnauthorized},
		{"有token_api_对token", secret, "/api/teams", [2]string{"Authorization", "Bearer " + secret}, http.StatusOK},
		{"有token_api_小写bearer", secret, "/api/teams", [2]string{"Authorization", "bearer " + secret}, http.StatusOK},
		{"有token_api_无bearer前缀", secret, "/api/teams", [2]string{"Authorization", secret}, http.StatusOK},
		{"有token_api_专用头", secret, "/api/teams", [2]string{TokenHeader, secret}, http.StatusOK},
		{"有token_api_token是前缀", secret, "/api/teams", [2]string{"Authorization", "Bearer " + secret[:5]}, http.StatusUnauthorized},
		{"有token_api_token多字符", secret, "/api/teams", [2]string{"Authorization", "Bearer " + secret + "x"}, http.StatusUnauthorized},

		// 其余受保护前缀
		{"有token_wiki", secret, "/wiki/status", [2]string{}, http.StatusUnauthorized},
		{"有token_sync", secret, "/sync/ima", [2]string{}, http.StatusUnauthorized},
		{"有token_cluster_enqueue", secret, "/cluster/enqueue", [2]string{}, http.StatusUnauthorized},
		{"有token_cluster_pull", secret, "/cluster/pull", [2]string{}, http.StatusUnauthorized},

		// 豁免与非保护路径：即使配了 token 也放行
		{"有token_健康探针豁免", secret, "/api/health", [2]string{}, http.StatusOK},
		{"有token_SPA根", secret, "/", [2]string{}, http.StatusOK},
		{"有token_SPA前端路由", secret, "/teams", [2]string{}, http.StatusOK},
		{"有token_静态资源", secret, "/static/app.js", [2]string{}, http.StatusOK},
		{"有token_metrics不拦", secret, "/metrics", [2]string{}, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Middleware(Config{Secret: tc.secret})(okHandler())
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.header[0] != "" {
				req.Header.Set(tc.header[0], tc.header[1])
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("%s %s = %d, 期望 %d (body=%q)", req.Method, tc.path, rr.Code, tc.want, rr.Body.String())
			}
			if tc.want == http.StatusUnauthorized {
				if got := rr.Header().Get("WWW-Authenticate"); got == "" {
					t.Error("401 响应缺少 WWW-Authenticate 头")
				}
				if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("401 Content-Type = %q, 期望 application/json", ct)
				}
			}
		})
	}
}

// 回归：曾经的实现是逐路由 s.auth(...) 包装，导致 /api/* 与后加的 /cluster/*
// 漏保护。本测试锁定"新增路由默认被保护"这一性质：只要落在受保护前缀下，
// 无需任何额外注册动作就应被拦。
func TestMiddleware_新增路由默认被保护(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/api/brand-new-endpoint", okHandler())
	mux.Handle("/cluster/brand-new-endpoint", okHandler())
	h := Middleware(Config{Secret: "tok"})(mux)

	for _, p := range []string{"/api/brand-new-endpoint", "/cluster/brand-new-endpoint"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s 未被自动保护: got %d", p, rr.Code)
		}
	}
}

func TestExtractToken(t *testing.T) {
	cases := []struct{ header, value, want string }{
		{"Authorization", "Bearer abc", "abc"},
		{"Authorization", "bearer abc", "abc"},
		{"Authorization", "BEARER abc", "abc"},
		{"Authorization", "Bearer   abc  ", "abc"},
		{"Authorization", "abc", "abc"},
		{"Authorization", "", ""},
		{TokenHeader, "abc", "abc"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.value != "" {
			req.Header.Set(c.header, c.value)
		}
		if got := ExtractToken(req); got != c.want {
			t.Errorf("%s=%q → %q, 期望 %q", c.header, c.value, got, c.want)
		}
	}
}

// 专用头优先于 Authorization，便于反代链路覆盖上游注入的 Authorization。
func TestExtractToken_专用头优先(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer from-authz")
	req.Header.Set(TokenHeader, "from-custom")
	if got := ExtractToken(req); got != "from-custom" {
		t.Errorf("got %q, 期望 from-custom", got)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{":18080", false}, // 默认格式 = 全网卡，这正是 :18080 的现状
		{"0.0.0.0:18080", false},
		{"[::]:18080", false},
		{"192.168.1.9:18080", false},
		{"100.111.70.76:18080", false}, // tailscale 地址也算暴露
		{"127.0.0.1:18080", true},
		{"[::1]:18080", true},
		{"localhost:18080", true},
		{"127.0.0.1", true},
		{"0.0.0.0", false},
	}
	for _, c := range cases {
		if got := IsLoopbackAddr(c.addr); got != c.want {
			t.Errorf("IsLoopbackAddr(%q) = %v, 期望 %v", c.addr, got, c.want)
		}
	}
}

func TestConfig_自定义保护前缀(t *testing.T) {
	cfg := Config{
		Secret:          "tok",
		ProtectPrefixes: []string{"/only-this/"},
		ExemptPaths:     []string{},
	}
	if cfg.Protects("/api/teams") {
		t.Error("/api/teams 不应被保护（已自定义前缀）")
	}
	if !cfg.Protects("/only-this/x") {
		t.Error("/only-this/x 应被保护")
	}
	// 空 ExemptPaths 切片（非 nil）表示"无豁免"，健康探针也应被拦
	if !cfg.Protects("/only-this/health") {
		t.Error("显式空豁免列表下不应回退到默认豁免")
	}
}
