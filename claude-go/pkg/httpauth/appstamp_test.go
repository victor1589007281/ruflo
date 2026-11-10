package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// appEchoHandler 把 ctx 里的 app/appRef 回写到响应头，便于断言 stamping 结果。
func appEchoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-App", types.AppFromContext(r.Context()))
		w.Header().Set("X-Echo-AppRef", types.AppRefFromContext(r.Context()))
		w.WriteHeader(http.StatusOK)
	})
}

func TestStampApp_VerifiedByRegistryToken(t *testing.T) {
	cfg := Config{
		Secret: "global-secret",
		Apps:   []AppEntry{{App: "studyforge", Token: "sf-token"}},
	}
	srv := Middleware(cfg)(appEchoHandler())

	// 注册表 token → verified appId（纯 id，无 claimed: 前缀），且放行鉴权
	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set(TokenHeader, "sf-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("注册表 token 应放行, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Echo-App"); got != "studyforge" {
		t.Fatalf("ctx app = %q, want studyforge (verified)", got)
	}

	// 全局 Secret 仍走老路：token 对得上但不在注册表 → app 是 unknown
	req = httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set(TokenHeader, "global-secret")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Echo-App"); got != types.DefaultAppID {
		t.Fatalf("全局 Secret 的 ctx app = %q, want %q", got, types.DefaultAppID)
	}
}

func TestStampApp_RegistryTokenReplacesClaimedHeader(t *testing.T) {
	// 同时带注册表 token 和 X-App-Id：以凭据背书的注册表为准，
	// 自报头不能覆盖已验证身份（否则攻击者给别人的 token 配自己的头就冒名了）。
	cfg := Config{Apps: []AppEntry{{App: "askforge", Token: "af-token"}}}
	srv := Middleware(cfg)(appEchoHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set(TokenHeader, "af-token")
	req.Header.Set(AppHeader, "evil-app")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Echo-App"); got != "askforge" {
		t.Fatalf("ctx app = %q, want askforge（注册表优先于自报头）", got)
	}
}

func TestStampApp_ClaimedHeaderWithoutToken(t *testing.T) {
	// fail-open（无 Secret）+ 无注册表 + 只带 X-App-Id → claimed:<id>
	cfg := Config{}
	srv := Middleware(cfg)(appEchoHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set(AppHeader, "moqi")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Echo-App"); got != "claimed:moqi" {
		t.Fatalf("ctx app = %q, want claimed:moqi", got)
	}
	if got := rec.Header().Get("X-Echo-AppRef"); got != "" {
		t.Fatalf("appRef = %q, want 空", got)
	}
}

func TestStampApp_NoIdentityStillStampsUnknown(t *testing.T) {
	// 无 Secret 且无注册表：鉴权恒放行，但 stamping 仍发生 —— 迁移期 nav APP
	// 先加 X-App-Id 头后换 token，claimed: 形态必须在零配置下就可见。
	cfg := Config{}
	srv := Middleware(cfg)(appEchoHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fail-open 应放行, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Echo-App"); got != types.DefaultAppID {
		t.Fatalf("ctx app = %q, want %q（零配置也 stamping）", got, types.DefaultAppID)
	}
}

func TestStampApp_AppRefPassthrough(t *testing.T) {
	cfg := Config{Apps: []AppEntry{{App: "deck", Token: "deck-token"}}}
	srv := Middleware(cfg)(appEchoHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set(TokenHeader, "deck-token")
	req.Header.Set(AppRefHeader, "ticket-42")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Echo-AppRef"); got != "ticket-42" {
		t.Fatalf("appRef = %q, want ticket-42（原样透传）", got)
	}
}

func TestMiddleware_RegistryTokenPassesAuth(t *testing.T) {
	// 有全局 Secret 时，注册表 token 同样放行 —— per-app 凭据是共享 Secret 的迁移踏板。
	cfg := Config{
		Secret: "global-secret",
		Apps:   []AppEntry{{App: "testforge", Token: "tf-token"}},
	}
	srv := Middleware(cfg)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set(TokenHeader, "tf-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("注册表 token 应通过鉴权, got %d", rec.Code)
	}

	// 未登记 token 仍 401
	req = httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set(TokenHeader, "random-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登记 token 应 401, got %d", rec.Code)
	}
}

func TestStampApp_UnknownByDefault(t *testing.T) {
	cfg := Config{Secret: "s"}
	srv := Middleware(cfg)(appEchoHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	req.Header.Set(TokenHeader, "s")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Echo-App"); got != types.DefaultAppID {
		t.Fatalf("ctx app = %q, want %q（未自报 → unknown）", got, types.DefaultAppID)
	}
}
