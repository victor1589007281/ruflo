package wiki

// contract_18080_test.go — design/02 §七风险① 要求的**表驱动契约测试**(wiki 侧)。
//
// 为什么必须有: :18080 是 8+ 个下游平台 (storyloom/mediaforge/testforge/growring/
// docforge/interviewforge/nav/aiops) 的唯一入口, design/02 §3.5 把这些端点的
// 「路径与语义」冻结为契约。之前只有"某个 handler 行为对不对"的散装测试, 没有
// 一张能被 diff 的清单 —— 于是删掉/改名一个端点在 CI 上毫无声响。
//
// 三条断言, 分别挡三类事故:
//   ① 路由仍在且未被兜底路由吃掉 → 挡"端点消失/被 SPA fallback 遮蔽";
//   ② 表与源码双向一致 → 挡"新增端点没登记进契约"和"端点被删了表还留着";
//   ③ 鉴权前缀覆盖 → 挡"新增对外端点忘了纳入 pkg/httpauth 保护前缀"。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/httpauth"
)

// endpoint 一条冻结的端点契约。
type endpoint struct {
	Pattern string // 注册用的 mux pattern (末尾 "/" 有无是语义的一部分)
	Probe   string // 用于探测的具体请求路径 (前缀路由时给一个实例路径)
	// GetStatus 期望的 GET 状态码。0 = 不做活体断言。
	// 405 表示该端点是 POST-only —— 这是下游能观察到的语义, 属于契约。
	GetStatus int
	Note      string
}

// wikiContract 是 :18080 上 wiki 侧的全部端点 (design/02 §6 覆盖矩阵的
// "wiki 9 端点"那一行)。改动这张表 = 改动对外契约, 必须同步 design/02。
var wikiContract = []endpoint{
	{Pattern: "/wiki/status", Probe: "/wiki/status", GetStatus: http.StatusOK, Note: "只读状态"},
	{Pattern: "/wiki/ingest", Probe: "/wiki/ingest", GetStatus: http.StatusMethodNotAllowed, Note: "POST only"},
	{Pattern: "/wiki/query", Probe: "/wiki/query", GetStatus: http.StatusMethodNotAllowed, Note: "POST only"},
	{Pattern: "/wiki/lint", Probe: "/wiki/lint", GetStatus: http.StatusMethodNotAllowed, Note: "POST only"},
	{Pattern: "/wiki/organize", Probe: "/wiki/organize", GetStatus: http.StatusMethodNotAllowed, Note: "POST only"},
	{Pattern: "/wiki/health-check", Probe: "/wiki/health-check", GetStatus: http.StatusMethodNotAllowed, Note: "POST only"},
	{Pattern: "/sync/ima", Probe: "/sync/ima", GetStatus: http.StatusMethodNotAllowed, Note: "POST only"},
	{Pattern: "/sync/weread", Probe: "/sync/weread", GetStatus: http.StatusMethodNotAllowed, Note: "POST only"},
	// 无调度器时 503 (而不是 404): 下游据此区分"没配 sync"与"路径写错了"。
	{Pattern: "/sync/status/", Probe: "/sync/status/job-1", GetStatus: http.StatusServiceUnavailable, Note: "GET, 前缀路由"},
}

func newContractServer(t *testing.T) *APIServer {
	t.Helper()
	return NewAPIServer(NewEngine(t.TempDir(), "", "", ""), "")
}

// ① 每条端点都真注册在 mux 上, 且命中的正是契约里那个 pattern。
//
// 用 mux.Handler(req) 拿回命中的 pattern 而不是只看状态码: dashboard 在同一个
// mux 上注册了 "/" 兜底 (SPA), 一旦某条 /wiki 路由被删掉, 请求会静静落到兜底
// 路由并返回 200 index.html —— 只看状态码的测试会全绿。
func Test契约_wiki端点全部注册且未被兜底遮蔽(t *testing.T) {
	api := newContractServer(t)
	// 复现生产形态: 同一 mux 上还挂着 SPA 兜底。
	api.Mux().HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>spa</html>"))
	})

	for _, ep := range wikiContract {
		req := httptest.NewRequest(http.MethodGet, ep.Probe, nil)
		_, pattern := api.Mux().Handler(req)
		if pattern != ep.Pattern {
			t.Errorf("%s 命中 pattern=%q, 期望 %q (端点被删或被兜底路由遮蔽)", ep.Probe, pattern, ep.Pattern)
		}
	}
}

// ② 表与源码双向一致。
//
// 单向断言只能挡"端点被删", 挡不住"悄悄加了个端点却没进契约表"——
// 后者恰恰是 /cluster/* 当年漏掉鉴权的成因 (新增路由没人过一遍清单)。
func Test契约_wiki端点表与源码双向一致(t *testing.T) {
	found := scanRegisteredPatterns(t, "api.go")
	want := map[string]bool{}
	for _, ep := range wikiContract {
		want[ep.Pattern] = true
	}
	for _, p := range found {
		if !want[p] {
			t.Errorf("源码注册了 %q 但契约表里没有 —— 新增对外端点必须登记进 wikiContract 并同步 design/02 §3.5/§6", p)
		}
		delete(want, p)
	}
	if len(want) > 0 {
		var missing []string
		for p := range want {
			missing = append(missing, p)
		}
		sort.Strings(missing)
		t.Errorf("契约表声明了 %v 但源码里没注册 —— 端点被删/改名会打断下游平台", missing)
	}
}

// ③ 全部端点都落在 pkg/httpauth 的保护前缀内。
// 反面教材: /cluster/* 加上时没人想起鉴权是按前缀白名单做的, 于是漏了一整组。
func Test契约_wiki端点全部在鉴权保护前缀内(t *testing.T) {
	cfg := httpauth.Config{Secret: "x"}
	for _, ep := range wikiContract {
		if !cfg.Protects(ep.Probe) {
			t.Errorf("%s 不在 httpauth 保护前缀内 —— 配了 token 也拦不住它", ep.Probe)
		}
	}
}

// ④ 方法语义: POST-only 的端点对 GET 必须回 405, 不能回 200 或 404。
// 405 是下游可观察的行为, 属于冻结范围。
func Test契约_wiki端点方法语义(t *testing.T) {
	api := newContractServer(t)
	for _, ep := range wikiContract {
		if ep.GetStatus == 0 {
			continue
		}
		rr := httptest.NewRecorder()
		api.Mux().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, ep.Probe, nil))
		if rr.Code != ep.GetStatus {
			t.Errorf("GET %s = %d, 契约要求 %d (%s)", ep.Probe, rr.Code, ep.GetStatus, ep.Note)
		}
	}
}

// reMuxRegister 抓 s.mux.HandleFunc("...") / s.mux.Handle("...") 的字面量 pattern。
var reMuxRegister = regexp.MustCompile(`(?m)\bmux\.Handle(?:Func)?\(\s*"([^"]+)"`)

// scanRegisteredPatterns 从源文件里扫出注册的路由 pattern。
//
// 为什么读源码而不是问 mux: http.ServeMux 没有枚举已注册 pattern 的公开 API,
// 而"反向发现未登记端点"必须能枚举。测试读自己包里的源文件是可接受的代价 ——
// 换成把 registerRoutes 的入参改成可注入的接口, 会为了测试改动生产装配路径。
func scanRegisteredPatterns(t *testing.T, files ...string) []string {
	t.Helper()
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		for _, m := range reMuxRegister.FindAllStringSubmatch(string(b), -1) {
			p := m[1]
			if p == "/" || strings.HasPrefix(p, "/static/") {
				continue // SPA 兜底与静态资源不属于 API 契约
			}
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
