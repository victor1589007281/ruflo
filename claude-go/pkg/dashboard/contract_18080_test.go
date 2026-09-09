package dashboard

// contract_18080_test.go — design/02 §七风险① 要求的**表驱动契约测试**(dashboard 侧)。
//
//	「① 兼容层是生命线——:18080 端点契约先写成表驱动测试再动内部」
//
// 这张表就是那份清单: design/02 §6 覆盖矩阵里「:18080 dashboard ~50 端点」
// 那一行的逐条展开。8+ 个下游平台 (storyloom/mediaforge/testforge/growring/
// docforge/interviewforge/nav/aiops) 直接打这些路径, 删一个、改一个前缀语义
// (把 "/api/x" 改成 "/api/x/") 都是线上事故。
//
// 四条断言各自挡一类事故, 见每个测试函数的注释。最要紧的是**双向一致**那条:
// dashboard 在同一 mux 上注册了 "/" 兜底 (SPA), 端点被删之后请求会静静落到
// 兜底路由拿到 200 + index.html —— 只看状态码的测试会全绿, 下游却已经在解析
// HTML 当 JSON 了。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/httpauth"
)

// apiEndpoint 一条冻结的 /api 端点契约。
type apiEndpoint struct {
	Pattern string // mux pattern。末尾 "/" 有无是语义的一部分 (前缀路由 vs 精确路由)
	Probe   string // 探测路径; 前缀路由给一个实例路径
	Prefix  bool   // 是否前缀路由 (Pattern 以 "/" 结尾)
}

// dashboardContract 是 :18080 / :7777 上 dashboard 注册的全部路由。
//
// 维护规则 (违反会被 Test契约_端点表与源码双向一致 打回):
//   - 新增端点 → 往这张表里加一行, 并同步 design/02 §3.5 与 §6;
//   - 删除/改名端点 → 先确认 8+ 下游平台没人用, design/02 把这批路径冻结了。
var dashboardContract = []apiEndpoint{
	// 基础
	{Pattern: "/api/health", Probe: "/api/health"},
	{Pattern: "/api/overview", Probe: "/api/overview"},
	{Pattern: "/api/teams", Probe: "/api/teams"},
	{Pattern: "/api/teams/", Probe: "/api/teams/demo", Prefix: true},
	{Pattern: "/api/metrics", Probe: "/api/metrics"},
	{Pattern: "/api/metrics/", Probe: "/api/metrics/llm", Prefix: true},
	{Pattern: "/api/cron", Probe: "/api/cron"},
	{Pattern: "/api/cron/", Probe: "/api/cron/job-1", Prefix: true},
	{Pattern: "/api/dreaming", Probe: "/api/dreaming"},
	{Pattern: "/api/evolution", Probe: "/api/evolution"},
	// 13.8.5 量化驾驶舱 (FoldSnapshot L1/L2 + 阈值告警位); 比 /api/evolution 更具体。
	{Pattern: "/api/evolution/quant", Probe: "/api/evolution/quant"},
	// 13.8.9 进化趋势 (日折叠序列 + 7d 三态 delta + 回归计数 + 学习轮次)。
	{Pattern: "/api/evolution/trends", Probe: "/api/evolution/trends"},
	// 13.7.9 池观测聚合 (SetPoolLister 注入 worker 摘要; 未注入时 workers 缺省)。
	{Pattern: "/api/pool", Probe: "/api/pool"},
	// 可用模型别名 (SetModelLister 注入; 未注入时 501)。
	{Pattern: "/api/models", Probe: "/api/models"},
	{Pattern: "/api/tasks", Probe: "/api/tasks"},
	{Pattern: "/api/insights", Probe: "/api/insights"},
	{Pattern: "/api/projects", Probe: "/api/projects"},
	{Pattern: "/api/stream/overview", Probe: "/api/stream/overview"},

	// v1.1 编排/角色/意图/能力
	{Pattern: "/api/workflows", Probe: "/api/workflows"},
	{Pattern: "/api/workflows/generate", Probe: "/api/workflows/generate"},
	{Pattern: "/api/workflows/", Probe: "/api/workflows/novel-v2", Prefix: true},
	{Pattern: "/api/roles", Probe: "/api/roles"},
	{Pattern: "/api/intent", Probe: "/api/intent"},
	{Pattern: "/api/verify-goal", Probe: "/api/verify-goal"},
	{Pattern: "/api/skills", Probe: "/api/skills"},
	{Pattern: "/api/skills/generate", Probe: "/api/skills/generate"},
	{Pattern: "/api/skills/", Probe: "/api/skills/some-skill", Prefix: true},
	{Pattern: "/api/tools", Probe: "/api/tools"},
	{Pattern: "/api/mcp/servers", Probe: "/api/mcp/servers"},
	{Pattern: "/api/references", Probe: "/api/references"},
	{Pattern: "/api/search", Probe: "/api/search"},
	{Pattern: "/api/logs/stream", Probe: "/api/logs/stream"},
	{Pattern: "/api/logs/tail", Probe: "/api/logs/tail"},
	{Pattern: "/api/hivemind", Probe: "/api/hivemind"},
	{Pattern: "/api/timeseries/", Probe: "/api/timeseries/llm/tokens", Prefix: true},
	{Pattern: "/api/actions/", Probe: "/api/actions/team/run/demo", Prefix: true},
	{Pattern: "/api/dreaming/diagnosis", Probe: "/api/dreaming/diagnosis"},

	// v1.2 备份 / LLM
	{Pattern: "/api/backups", Probe: "/api/backups"},
	{Pattern: "/api/backups/create", Probe: "/api/backups/create"},
	{Pattern: "/api/backups/restore", Probe: "/api/backups/restore"},
	{Pattern: "/api/backups/manifest", Probe: "/api/backups/manifest"},
	{Pattern: "/api/backups/download", Probe: "/api/backups/download"},
	{Pattern: "/api/llm/status", Probe: "/api/llm/status"},

	// v1.3
	{Pattern: "/api/logs/sources", Probe: "/api/logs/sources"},
	{Pattern: "/api/llm/stats", Probe: "/api/llm/stats"},

	// v1.4
	{Pattern: "/api/llm/rate", Probe: "/api/llm/rate"},
	{Pattern: "/api/llm/guard", Probe: "/api/llm/guard"},
	{Pattern: "/api/metrics/catalog", Probe: "/api/metrics/catalog"},
	{Pattern: "/api/metrics/coverage", Probe: "/api/metrics/coverage"},
	{Pattern: "/api/metrics/full", Probe: "/api/metrics/full"},
	{Pattern: "/api/prom/query", Probe: "/api/prom/query"},

	// v1.5 异步诊断作业
	{Pattern: "/api/diag/jobs", Probe: "/api/diag/jobs"},
	{Pattern: "/api/diag/jobs/", Probe: "/api/diag/jobs/j-1", Prefix: true},
	{Pattern: "/api/dreaming/trigger", Probe: "/api/dreaming/trigger"},

	// RewardBus 奖励源回传 (design/03 §4.2 gate.e2e, 本轮新增)。
	// 前缀路由: 只服务 /api/runs/{runId}/feedback 这一个形状, 其余形状 404。
	{Pattern: "/api/runs/", Probe: "/api/runs/run-1/feedback", Prefix: true},

	// L5 platform-mcp-server (design/02 §3.5, 本轮新增)
	{Pattern: PlatformMCPPath, Probe: PlatformMCPPath},

	// Prometheus 抓取端点。刻意**不在** httpauth 保护前缀内 (抓取方不带 token),
	// 因此单列并在鉴权测试里显式豁免 —— 豁免要写下来, 不能靠"恰好没被断言到"。
	{Pattern: "/metrics", Probe: "/metrics"},
}

// metricsExemptPath 是允许不被鉴权覆盖的路径 (Prometheus 抓取)。
const metricsExemptPath = "/metrics"

// newContractMux 造一个装配完成的 mux (含 SPA 兜底), 不起监听。
func newContractMux(t *testing.T) *http.ServeMux {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), ".claude-go")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	// MountOn 是 :18080 的生产装配路径 (pkg/feishu 用它把 dashboard 挂到 wiki mux)。
	// 用它而不是 NewServer, 断言的才是生产真正跑的那条注册路径。
	MountOn(Config{StateDir: stateDir}, mux)
	return mux
}

// ① 每条端点都真注册, 且命中的正是契约里那个 pattern。
//
// pattern 相等这一条比"状态码非 404"强得多: SPA 兜底会把任何未匹配路径变成
// 200 + index.html, 端点消失时状态码断言全绿, pattern 断言才会红。
func Test契约_端点全部注册且未被SPA兜底遮蔽(t *testing.T) {
	mux := newContractMux(t)
	for _, ep := range dashboardContract {
		req := httptest.NewRequest(http.MethodGet, ep.Probe, nil)
		_, pattern := mux.Handler(req)
		if pattern != ep.Pattern {
			t.Errorf("%s 命中 pattern=%q, 期望 %q (端点被删/改名, 或被 \"/\" 兜底吃掉)",
				ep.Probe, pattern, ep.Pattern)
		}
	}
}

// ② 表与源码双向一致 —— 挡"悄悄新增端点没登记进契约"。
//
// 这是 /cluster/* 当年整组漏鉴权的同一类错误: 新增路由时没人过一遍清单。
// 有了这条, 任何新端点都必须在契约表里留一行, 于是必然被鉴权断言覆盖到。
func Test契约_端点表与源码双向一致(t *testing.T) {
	found := scanRegisteredPatterns(t, "server.go", "platform_mcp.go")
	want := map[string]bool{}
	for _, ep := range dashboardContract {
		want[ep.Pattern] = true
	}
	for _, p := range found {
		if !want[p] {
			t.Errorf("源码注册了 %q 但契约表里没有 —— 新增端点必须登记进 dashboardContract 并同步 design/02 §3.5/§6", p)
		}
		delete(want, p)
	}
	if len(want) > 0 {
		var missing []string
		for p := range want {
			missing = append(missing, p)
		}
		sort.Strings(missing)
		t.Errorf("契约表声明了 %v 但源码里没注册 —— 端点被删/改名会打断下游 8+ 平台", missing)
	}
}

// ③ 除显式豁免外, 全部端点落在 pkg/httpauth 的保护前缀内。
//
// 两条豁免都是有理由的, 且理由写在 pkg/httpauth 里:
//   - /metrics: Prometheus 抓取方不带鉴权头;
//   - /api/health: 守护进程健康探针不带 token (DefaultExemptPaths)。
func Test契约_端点鉴权覆盖(t *testing.T) {
	cfg := httpauth.Config{Secret: "x"}
	for _, ep := range dashboardContract {
		protected := cfg.Protects(ep.Probe)
		switch ep.Probe {
		case metricsExemptPath:
			if protected {
				t.Errorf("%s 被纳入鉴权 —— Prometheus 抓取会 401", ep.Probe)
			}
		case "/api/health":
			if protected {
				t.Errorf("%s 被纳入鉴权 —— dashboard 守护进程的健康探针会 401", ep.Probe)
			}
		default:
			if !protected {
				t.Errorf("%s 不在 httpauth 保护前缀内 —— 配了 token 也拦不住它", ep.Probe)
			}
		}
	}
}

// ④ 前缀路由的"末尾斜杠"语义不能漂移。
//
// 把 "/api/teams/" 改成 "/api/teams" 会让 /api/teams/<name> 全部落到兜底路由;
// 反过来把精确路由改成前缀路由会静默吞掉相邻路径。两种都不是编译错误, 只有
// 断言能拦。
func Test契约_前缀路由语义(t *testing.T) {
	for _, ep := range dashboardContract {
		gotPrefix := strings.HasSuffix(ep.Pattern, "/")
		if gotPrefix != ep.Prefix {
			t.Errorf("%s: Pattern 的末尾斜杠(%v) 与契约声明的 Prefix(%v) 不一致",
				ep.Pattern, gotPrefix, ep.Prefix)
		}
	}
	mux := newContractMux(t)
	// /api/teams/<name> 必须命中前缀路由, 不能命中 /api/teams 精确路由或兜底。
	_, p := mux.Handler(httptest.NewRequest(http.MethodGet, "/api/teams/whatever/blackboard", nil))
	if p != "/api/teams/" {
		t.Errorf("/api/teams/whatever/blackboard 命中 %q, 期望 \"/api/teams/\"", p)
	}
}

// reMuxRegister 抓 mux.HandleFunc("...") / mux.Handle("...") 的字面量 pattern,
// 以及 mux.Handle(标识符, ...) 形态里的常量名 (platform MCP 用常量注册)。
var (
	reMuxRegister      = regexp.MustCompile(`(?m)\bmux\.Handle(?:Func)?\(\s*"([^"]+)"`)
	reMuxRegisterConst = regexp.MustCompile(`(?m)\bmux\.Handle(?:Func)?\(\s*([A-Z][A-Za-z0-9_]*)\s*,`)
)

// knownPatternConsts 把"用常量注册"的 pattern 解析回字面值。
// 新增这类注册时必须在这里登记, 否则双向一致测试会报"契约表声明了但源码没注册"。
var knownPatternConsts = map[string]string{
	"PlatformMCPPath": PlatformMCPPath,
}

// scanRegisteredPatterns 从源文件扫出注册的路由 pattern。
//
// 为什么读源码而不是问 mux: http.ServeMux 没有枚举已注册 pattern 的公开 API,
// 而"反向发现未登记端点"必须能枚举。替代方案是把 registerRoutesOn 的入参换成
// 可注入的接口 —— 为了测试改生产装配路径, 代价更大。
func scanRegisteredPatterns(t *testing.T, files ...string) []string {
	t.Helper()
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		src := string(b)
		for _, m := range reMuxRegister.FindAllStringSubmatch(src, -1) {
			p := m[1]
			if p == "/" || strings.HasPrefix(p, "/static/") {
				continue // SPA 兜底与静态资源不属于 API 契约
			}
			out = append(out, p)
		}
		for _, m := range reMuxRegisterConst.FindAllStringSubmatch(src, -1) {
			v, ok := knownPatternConsts[m[1]]
			if !ok {
				t.Errorf("%s 用常量 %s 注册了路由, 但 knownPatternConsts 里没登记 —— "+
					"契约扫描看不见它", f, m[1])
				continue
			}
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
