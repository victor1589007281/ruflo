package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/cluster"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/hotreload"
	"github.com/anthropic/claude-go/pkg/httpauth"
	"github.com/anthropic/claude-go/pkg/metrics"
)

// CronController 是 dashboard 写接口所需的定时任务能力子集。
// *agent.CronScheduler 已实现全部方法; 由飞书 bot 进程 (:18080) 在挂载 dashboard 时注入其
// 活动调度器 (见 SetCronController)。独立 dashboard (:7777) 未注入, cronCtl 为 nil,
// 写接口返回 501, 只读列表照常工作。
type CronController interface {
	AddJob(job *agent.CronJob) error
	RemoveJob(id string) error
	PauseJob(id string) error
	ResumeJob(id string) error
	UpdateJob(id string, patch *agent.CronJob) error
	GetJob(id string) *agent.CronJob
	ListJobs() []*agent.CronJob
	// TriggerJob 立即执行一次任务 (手动触发; 不动调度计划)。
	TriggerJob(id string) error
}

//go:embed web/*
var webFS embed.FS

// TeamActionFunc 团队操作回调: action 可为 "stop"/"restart"/"delete"。
type TeamActionFunc func(action, teamName string, payload map[string]interface{}) error

// Config dashboard 启动配置。
type Config struct {
	StateDir   string         // 数据根目录 (.claude-go)
	Addr       string         // 监听地址, 例如 "127.0.0.1:7777"
	CacheTTL   time.Duration  // 数据缓存 TTL, 0 禁用
	TeamAction TeamActionFunc // 团队操作回调 (create/run/stop/restart/delete)，为 nil 时尝试转发到 BotAPIURL
	BotAPIURL  string         // Feishu bot 的 wiki API URL (如 "http://127.0.0.1:18080"), 用于转发 team 操作
	// BotAPIToken 转发到 BotAPIURL 时携带的鉴权 token。BotAPIURL 侧启用
	// wiki.apiSecret 后必须设置, 否则 team 操作转发会 401。
	BotAPIToken string
	// Secret 本 dashboard 自身 /api/* 的鉴权 token。空 = 不鉴权 (fail-open),
	// 详见 pkg/httpauth 的包注释。
	Secret string
	// LLMComplete LLM 文本补全回调 (供 LLM 生成工作流编排/skill); nil 时返回 503。
	// 由主进程(feishu bot)注入其 LLM 客户端。
	LLMComplete func(ctx context.Context, systemPrompt, userPrompt string) (string, error)
	// MCPServers 返回运行态 MCP 服务器列表 (可序列化); nil 时 /api/mcp/servers 返回空。
	MCPServers func() interface{}
	// ReloadSkills 创建 skill 后让主进程技能库即时重载 (可选)。
	ReloadSkills func()
}

// Server HTTP 服务。
type Server struct {
	cfg         Config
	provider    *Provider
	mux         *http.ServeMux
	server      *http.Server
	jobs        *diagJobStore                        // 异步 LLM 诊断作业
	scraper     *metrics.JSONLScraper                // JSONL → Prometheus 采集器 (MySQL Exporter 模式)
	cronCtl     func() CronController                // 定时任务写控制器【解析器】(仅 :18080 飞书进程注入; nil/返回nil 时写接口 501)
	modelLister func() []string                      // 可用模型别名【解析器】(SetModelLister 注入; nil 时 /api/models 501)
	poolLister  func() ([]cluster.WorkerInfo, error) // worker 池摘要【解析器】(13.7.9; nil 时 /api/pool 只报本进程)
}

// SetCronController 注入活动定时任务调度器的【解析器】, 启用 /api/cron 写接口 (创建/更新/启停/删除)。
// 用解析器而非具体值是关键: dashboard 在 NewBot 内经 APIExtensions 挂载, 那一刻主进程的
// botRef 尚为 nil(botRef 在 NewBot 返回后才赋值), 必须请求期再解析 —— 与 TeamAction/
// LLMComplete 等同进程回调同一惰性模式。解析器返回 nil 时写接口 501。
func (s *Server) SetCronController(fn func() CronController) { s.cronCtl = fn }

// SetModelLister 注入可用模型别名的【解析器】 (providers 注册的 alias 清单),
// 启用 GET /api/models —— webapp 的 cron 任务模型下拉等"可选模型"消费方从这里取数。
// 同一惰性解析模式: 主进程配置在挂载时就绪, 但保持解析器形态与 SetCronController 一致。
func (s *Server) SetModelLister(fn func() []string) { s.modelLister = fn }

// SetPoolLister 注入 worker 池摘要【解析器】(13.7.9), 启用 GET /api/pool 的
// workers 段。control 进程装配时传 clusterReg.Alive; 独立 dashboard (:7777)
// 不注入 —— 池摘要只来自本进程 (tool.ObservedPool), workers 字段缺省。
func (s *Server) SetPoolLister(fn func() ([]cluster.WorkerInfo, error)) { s.poolLister = fn }

// resolveCron 请求期解析活动调度器; 未注入或主进程未就绪时返回 nil (写接口据此 501)。
func (s *Server) resolveCron() CronController {
	if s.cronCtl == nil {
		return nil
	}
	return s.cronCtl()
}

// NewServer 构造 dashboard server。
func NewServer(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:7777"
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 2 * time.Second
	}
	s := &Server{
		cfg:      cfg,
		provider: NewProvider(cfg.StateDir, cfg.CacheTTL),
		mux:      http.NewServeMux(),
		jobs:     newDiagJobStore(100),
	}
	// 幂等: 即使主进程已初始化过, 这里也是 no-op。保证 dashboard 单独前台运行时
	// 也能捕获自身对 LLM 的诊断调用。
	metrics.InitGlobalLLMCollector(cfg.StateDir)

	// 加载模型配置并设置 alias resolver，让指标中 model_alias 正确填充
	cfg2, err := feishu.LoadJSONConfig("")
	if err == nil && cfg2 != nil {
		if registry, _, err := modelconfig.LoadFromConfig(cfg2.ToModelConfigJSON()); err == nil && registry != nil {
			metrics.SetAliasResolver(registry.LookupAliasByModelName)
			// 配置热加载: dashboard 只消费 alias 解析（它自己做 LLM 诊断时按请求现读
			// 配置，本就免疫变更），所以这里只需在 config.json 变化时重设这个全局
			// 函数指针 —— 不重设，指标里的 model_alias 会一直按启动时的注册表解析。
			if p, perr := feishu.ResolveJSONConfigPath(""); perr == nil {
				hotreload.WatchConfig(p, 5*time.Second, "dashboard", func(path string) error {
					c, e := feishu.LoadJSONConfig(path)
					if e != nil || c == nil {
						return fmt.Errorf("重新加载失败: %v", e)
					}
					r, _, e := modelconfig.LoadFromConfig(c.ToModelConfigJSON())
					if e != nil || r == nil {
						return fmt.Errorf("构建注册表失败: %v", e)
					}
					metrics.SetAliasResolver(r.LookupAliasByModelName)
					return nil
				})
			}
		}
	}
	// 从 Swarm Intel / Cron JSON 数据回放历史指标到 Prometheus
	s.provider.ExportSwarmIntelMetrics()
	s.provider.ExportCronMetrics()
	// JSONL Scraper: MySQL Exporter 模式, 在每次 /metrics 请求时读取 CLI 进程写入的 JSONL
	s.scraper = metrics.NewJSONLScraper(cfg.StateDir)
	s.registerRoutes()

	s.server = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// MountOn 把 dashboard 挂到外部 mux 上 (不启动独立 HTTP 监听)。
// 用于把 dashboard 合并到飞书 bot 的 wiki API 端口等统一 HTTP 服务。
// 返回的 Server 不可再调用 ListenAndServe, 但 provider 等仍可被外部访问。
func MountOn(cfg Config, mux *http.ServeMux) *Server {
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 2 * time.Second
	}
	s := &Server{
		cfg:      cfg,
		provider: NewProvider(cfg.StateDir, cfg.CacheTTL),
		mux:      mux, // 复用外部 mux
		jobs:     newDiagJobStore(100),
	}
	metrics.InitGlobalLLMCollector(cfg.StateDir)

	// 加载模型配置并设置 alias resolver
	cfg2, err := feishu.LoadJSONConfig("")
	if err == nil && cfg2 != nil {
		if registry, _, err := modelconfig.LoadFromConfig(cfg2.ToModelConfigJSON()); err == nil && registry != nil {
			metrics.SetAliasResolver(registry.LookupAliasByModelName)
		}
	}

	s.provider.ExportSwarmIntelMetrics()
	s.provider.ExportCronMetrics()
	s.scraper = metrics.NewJSONLScraper(cfg.StateDir)
	s.registerRoutesOn(mux)

	return s
}

// recoverMiddleware 捕获 handler panic, 记录日志并返回 500, 防止进程崩溃。
type recoverMiddleware struct {
	handler http.Handler
}

func (r recoverMiddleware) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("[dashboard] panic in %s %s: %v", req.Method, req.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal server error"}`))
		}
	}()
	r.handler.ServeHTTP(w, req)
}

func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("dashboard listen %s: %w", s.cfg.Addr, err)
	}
	log.Printf("[dashboard] listening on http://%s  (stateDir=%s)", ln.Addr(), s.cfg.StateDir)
	httpauth.WarnIfExposed("dashboard", ln.Addr().String(), s.cfg.Secret)
	// 用 panic-recovery 包装 handler, 防止任意 handler panic 导致进程崩溃;
	// 鉴权在 recovery 之外, 使未授权请求不进入任何业务 handler。
	s.server.Handler = httpauth.Middleware(httpauth.Config{Secret: s.cfg.Secret})(
		recoverMiddleware{handler: s.mux})
	if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop 优雅停止。
func (s *Server) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.cfg.Addr }

// Handler 返回内部 http.Handler, 允许外部 HTTP 服务嵌入 dashboard (例如飞书
// bot 的 wiki API 端口统一服务)。
func (s *Server) Handler() http.Handler { return s.mux }

// RegisterOn 把 dashboard 全部路由 (包括 static + /api/* + SPA fallback) 挂到
// 外部 mux 上, 实现多个 HTTP service 合并到同一端口。
// 调用方需保证外部 mux 上没有冲突的 pattern。
func (s *Server) RegisterOn(mux *http.ServeMux) {
	s.registerRoutesOn(mux)
}

func (s *Server) registerRoutes() {
	s.registerRoutesOn(s.mux)
}

func (s *Server) registerRoutesOn(mux *http.ServeMux) {
	// 静态资源
	sub, err := fs.Sub(webFS, "web")
	if err == nil {
		fileServer := http.FileServer(http.FS(sub))
		mux.Handle("/static/", http.StripPrefix("/static/", fileServer))
	}

	// API
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/overview", s.handleOverview)
	mux.HandleFunc("/api/teams", s.handleTeams)
	mux.HandleFunc("/api/teams/", s.handleTeamDetail)
	mux.HandleFunc("/api/metrics", s.handleMetrics)
	mux.HandleFunc("/api/metrics/", s.handleMetricsModule)
	mux.HandleFunc("/api/cron", s.handleCron)      // GET 列表 / POST 创建
	mux.HandleFunc("/api/cron/", s.handleCronItem) // /api/cron/{id}: PATCH 更新/启停 · DELETE 删除; /api/cron/{id}/trigger: POST 立即触发
	mux.HandleFunc("/api/models", s.handleModels)  // GET 可用模型别名 (SetModelLister 注入)
	mux.HandleFunc("/api/dreaming", s.handleDreaming)
	mux.HandleFunc("/api/evolution", s.handleEvolution)
	mux.HandleFunc("/api/evolution/quant", s.handleEvolutionQuant)   // 13.8.5 量化驾驶舱 (更具体, 优先于上面)
	mux.HandleFunc("/api/evolution/trends", s.handleEvolutionTrends) // 13.8.9 进化趋势 (日折叠+7d delta+回归+轮次)
	mux.HandleFunc("/api/pool", s.handlePool)                        // 13.7.9 池摘要聚合 (SetPoolLister 注入)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/insights", s.handleInsights)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/stream/overview", s.handleStreamOverview)

	// v1.1 扩展
	mux.HandleFunc("/api/workflows", s.handleWorkflows)
	mux.HandleFunc("/api/workflows/generate", s.handleWorkflowGenerate) // LLM 生成编排 (更具体, 优先于下面)
	mux.HandleFunc("/api/roles", s.handleRoles)                         // 角色富信息 (DAG 节点面板 / Agents 展示页; 无 names 则列全部)
	mux.HandleFunc("/api/intent", s.handleIntent)                       // 会话意图识别 (LLM 语义: 是否需编排 + workflow/objective)
	mux.HandleFunc("/api/verify-goal", s.handleVerifyGoal)              // 目标验收 (供会话编排目标循环: 未达成带 gap 重跑)
	// 能力管理: skills / tools / MCP / 引用关系
	mux.HandleFunc("/api/skills", s.handleSkills)                 // GET 列表 / POST 创建
	mux.HandleFunc("/api/skills/generate", s.handleSkillGenerate) // LLM 生成 skill 草稿
	mux.HandleFunc("/api/skills/", s.handleSkillDetail)           // GET 详情 / DELETE
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/mcp/servers", s.handleMCPServers)
	// L5 platform-mcp-server (design/02 §3.5): 平台能力以 MCP 工具对外。
	// 落在 /api/ 前缀内 ⇒ 自动被 pkg/httpauth 的保护前缀覆盖, 见 platform_mcp.go。
	s.registerPlatformMCP(mux)
	mux.HandleFunc("/api/references", s.handleReferences)
	mux.HandleFunc("/api/workflows/", s.handleWorkflow) // /api/workflows/:name
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/logs/stream", s.handleLogsStream)
	mux.HandleFunc("/api/logs/tail", s.handleLogsTail)
	mux.HandleFunc("/api/hivemind", s.handleHiveMind)
	mux.HandleFunc("/api/timeseries/", s.handleTimeSeries) // /api/timeseries/:module/:metric
	mux.HandleFunc("/api/actions/", s.handleAction)        // /api/actions/{kind}/{target}
	mux.HandleFunc("/api/dreaming/diagnosis", s.handleDreamingDiagnosis)

	// v1.2: backup / llm
	mux.HandleFunc("/api/backups", s.handleBackupsList)
	mux.HandleFunc("/api/backups/create", s.handleBackupCreate)
	mux.HandleFunc("/api/backups/restore", s.handleBackupRestore)
	mux.HandleFunc("/api/backups/manifest", s.handleBackupManifest)
	mux.HandleFunc("/api/backups/download", s.handleBackupDownload)
	mux.HandleFunc("/api/llm/status", s.handleLLMStatus)

	// v1.3: 多源日志 / 团队 LLM 诊断 / LLM 统计 / 黑板写入
	mux.HandleFunc("/api/logs/sources", s.handleLogsSources)
	mux.HandleFunc("/api/llm/stats", s.handleLLMStats)

	// v1.4: LLM 速率/熔断/限流 快照, 全量 metrics 目录 & 审计, 团队 DAG / checkpoints / logs
	mux.HandleFunc("/api/llm/rate", s.handleLLMRate)
	mux.HandleFunc("/api/llm/guard", s.handleLLMGuard)
	mux.HandleFunc("/api/metrics/catalog", s.handleMetricsCatalog)
	mux.HandleFunc("/api/metrics/coverage", s.handleMetricsCoverage)
	mux.HandleFunc("/api/metrics/full", s.handleMetricsFullModule)
	mux.HandleFunc("/api/prom/query", s.handlePromQuery)

	// v1.5: 异步 LLM 诊断作业 (team/dreaming/evolution)
	//   POST /api/diag/jobs           -> 创建作业 (入参 {kind, target?})
	//   GET  /api/diag/jobs           -> 列出最近作业
	//   GET  /api/diag/jobs/{id}      -> 查询作业状态 / 结果
	//   POST /api/dreaming/trigger    -> 主动触发一次 dreaming
	mux.HandleFunc("/api/diag/jobs", s.handleDiagJobs)
	mux.HandleFunc("/api/diag/jobs/", s.handleDiagJobDetail)
	mux.HandleFunc("/api/dreaming/trigger", s.handleDreamingTrigger)

	// RewardBus 奖励源 gate.e2e (design/03 §4.2): 下游平台 (testforge 等) 把 e2e /
	// 验收门禁结论回传到某次 run。见 run_feedback.go —— source 是锁定字段, run 认领
	// 不到直接 404 且不落盘。
	mux.HandleFunc("/api/runs/", s.handleRunFeedback) // POST /api/runs/{runId}/feedback

	// Prometheus 端点: 使用 JSONLScraper 包装, 类似 MySQL Exporter 模式, 每次采集前读取 CLI 进程的 JSONL
	mux.Handle("/metrics", metrics.WrapWithScrape(s.scraper, metrics.PrometheusHandler()))

	// 根路径和 SPA fallback
	mux.HandleFunc("/", s.handleIndex)
}

// ============== Handlers ==============

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"stateDir": s.cfg.StateDir,
		"exists":   s.provider.Exists(),
		"version":  "1.0.0",
		"time":     time.Now(),
	})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	resp, err := s.provider.BuildOverview()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTeams(w http.ResponseWriter, r *http.Request) {
	list, err := s.provider.ListTeams()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleTeamDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/teams/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("team name required"))
		return
	}
	name := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	switch sub {
	case "":
		detail, err := s.provider.GetTeam(name)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, detail)
	case "blackboard":
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			s.handleTeamBlackboardWrite(w, r, name)
			return
		}
		bb, err := s.provider.Blackboard(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, bb)
	case "report":
		detail, err := s.provider.GetTeam(name)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"content": detail.Report})
	case "diagnose":
		s.handleTeamDiagnose(w, r, name)
	case "dag":
		s.handleTeamDAG(w, r, name)
	case "checkpoints":
		s.handleTeamCheckpoints(w, r, name)
	case "logs":
		s.handleTeamLogs(w, r, name)
	case "media":
		s.handleTeamMedia(w, r, name, parts[2:])
	case "artifacts":
		s.handleTeamArtifacts(w, r, name, parts[2:])
	case "swarm-plan":
		s.handleTeamSwarmPlan(w, r, name)
	case "refine":
		s.handleTeamRefine(w, r, name)
	case "fork":
		s.handleTeamFork(w, r, name)
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown sub-path %q", sub))
	}
}

// execTeamActionDirect 同步直连执行团队动作 (不走 .dashboard/actions 队列)。
// 供 webapp 等外部服务直接触发; 需要主进程注入了 TeamAction (即 :18080 飞书挂载的 dashboard)。
func (s *Server) execTeamActionDirect(w http.ResponseWriter, action, name string, payload map[string]interface{}) {
	if s.cfg.TeamAction == nil {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Errorf("无团队执行器: 请直连 :18080 (飞书挂载、已注入 TeamAction 的 dashboard) 触发"))
		return
	}
	if err := s.cfg.TeamAction(action, name, payload); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "team": name, "action": action})
}

// handleTeamRefine POST /api/teams/{name}/refine  body: {feedback, targetStage?}
func (s *Server) handleTeamRefine(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var body struct {
		Feedback    string `json:"feedback"`
		TargetStage string `json:"targetStage"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(body.Feedback) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("feedback 不能为空"))
		return
	}
	s.execTeamActionDirect(w, "refine", name, map[string]interface{}{
		"feedback": body.Feedback, "targetStage": body.TargetStage,
	})
}

// handleTeamFork POST /api/teams/{name}/fork  body: {newName}
func (s *Server) handleTeamFork(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var body struct {
		NewName string `json:"newName"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(body.NewName) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("newName 不能为空"))
		return
	}
	s.execTeamActionDirect(w, "fork", name, map[string]interface{}{"newName": body.NewName})
}

// handleTeamMedia 列出/服务团队的媒体产出 (creative-v2 等生成的 PNG/PDF/MP4/GIF/PPTX/HTML)。
//
//	GET /api/teams/{name}/media            → 产出文件清单 (JSON)
//	GET /api/teams/{name}/media/{file...}  → 服务单个文件 (带 path-traversal 守卫)
//
// 注: 产出 HTML 为模型生成内容, 通过 CSP sandbox 隔离, 防止注入 dashboard 同源。
func (s *Server) handleTeamMedia(w http.ResponseWriter, r *http.Request, name string, fileParts []string) {
	if !safeName(name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid team name"))
		return
	}
	mediaDir := filepath.Join(s.provider.StateDir(), "teams", name, "media")
	absDir, err := filepath.Abs(mediaDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if len(fileParts) == 0 || (len(fileParts) == 1 && fileParts[0] == "") {
		// 列清单: 必须【递归】。原实现是非递归 os.ReadDir 且直接 skip 子目录, 于是
		// media/sub/deep.png 这类嵌套产物永远采不到 (工作流按场景/分镜建子目录很常见),
		// 前端就显示"该团队暂无媒体产出"。这里复用 agent 侧的统一采集器 (同一份递归 +
		// 跳过 .git/node_modules/__pycache__ 的规则), 避免又多一份会漂移的实现。
		type mediaFile struct {
			Name string `json:"name"` // 相对 media/ 的路径 (嵌套时形如 sub/deep.png)
			Ext  string `json:"ext"`
			Size int64  `json:"size"`
			URL  string `json:"url"`
		}
		files := []mediaFile{}
		// media 目录是团队专属 (dataDir 内), 不需要 mtime 归属过滤 → 当作 data 根扫。
		refs, scanErr := agent.CollectArtifactsFor(agent.ArtifactScanSpec{Team: name, DataDir: absDir})
		for _, ref := range refs {
			files = append(files, mediaFile{
				Name: ref.Rel,
				Ext:  strings.ToLower(strings.TrimPrefix(filepath.Ext(ref.Rel), ".")),
				Size: ref.Size,
				URL:  "/api/teams/" + url.PathEscape(name) + "/media/" + escapeRelPath(ref.Rel),
			})
		}
		resp := map[string]interface{}{"files": files}
		// fail-open 语义: 采集出错时必须说出来, 不能让"扫失败"和"真的没有产物"长得
		// 一样 —— 下游据此误判过一次 (把已完成的成品置成待审)。
		if scanErr != nil {
			resp["scanned"] = false
			resp["error"] = scanErr.Error()
		} else {
			resp["scanned"] = true
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 服务单个文件: 拼接后取绝对路径, 必须仍在 mediaDir 之内 (防 ../ 穿越)。
	full, err := filepath.Abs(filepath.Join(absDir, filepath.Join(fileParts...)))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if full != absDir && !strings.HasPrefix(full, absDir+string(os.PathSeparator)) {
		writeError(w, http.StatusForbidden, fmt.Errorf("path traversal blocked"))
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, fmt.Errorf("media not found"))
		return
	}
	// 模型生成的 HTML: 用 CSP sandbox 隔离 (允许脚本以保留动画预览, 但不与 dashboard 同源)。
	if strings.HasSuffix(strings.ToLower(full), ".html") || strings.HasSuffix(strings.ToLower(full), ".svg") {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts;")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	http.ServeFile(w, r, full)
}

// escapeRelPath 逐段转义相对路径, 保留 / 分隔 (整段 PathEscape 会把 / 编成 %2F,
// 嵌套产物的 URL 就取不到了)。
func escapeRelPath(rel string) string {
	segs := strings.Split(rel, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return strings.Join(segs, "/")
}

// artifactsResponse 产物清单响应。source 说明清单来自哪里:
// manifest = 团队结束时落盘的 ARTIFACTS.json (含物化记账的硬归属证据);
// live = 本次请求现扫的 (团队还在跑, 或 ?refresh=1)。
type artifactsResponse struct {
	agent.ArtifactManifest
	Source string `json:"source"`
}

// handleTeamArtifacts 团队产物清单 (合并 dataDir 与 team.Cwd 两个落点)。
//
//	GET /api/teams/{name}/artifacts                              → 清单
//	GET /api/teams/{name}/artifacts/file/{rel...}?root=data|work  → 取单个文件
//
// 为什么需要它: /media 只看 <stateDir>/teams/<n>/media, 而 agent 用 Write/Bash 写的
// 文件、media_gen 六个媒体工具产的媒体、MaterializeCode 落的代码都在 team.Cwd 下,
// 任何只看一处的入口都会把"有产物"报成"无产物"。
func (s *Server) handleTeamArtifacts(w http.ResponseWriter, r *http.Request, name string, parts []string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	if !safeName(name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid team name"))
		return
	}
	spec := agent.ArtifactScanSpec{
		Team:    name,
		DataDir: filepath.Join(s.provider.StateDir(), "teams", name),
	}
	// cwd / startedAt 只能从 team.json 取 (team.Cwd 是序列化字段, dataDir 是私有的)。
	if detail, err := s.provider.GetTeam(name); err == nil && detail != nil {
		spec.Cwd = detail.Cwd
		spec.StartedAt = detail.StartedAt
		spec.FinishedAt = detail.FinishedAt
		if spec.StartedAt.IsZero() {
			spec.StartedAt = detail.CreatedAt
		}
	}

	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		// 优先返回落盘清单: 它含 MaterializeCode 记账的路径 (进程内私有字段, 事后重扫
		// 拿不到), 且是那次运行的归档快照。?refresh=1 或还没有清单时现扫。
		if r.URL.Query().Get("refresh") != "1" {
			if m, err := agent.ReadArtifactManifest(spec.DataDir); err == nil && m != nil {
				writeJSON(w, http.StatusOK, artifactsResponse{ArtifactManifest: *m, Source: "manifest"})
				return
			}
		}
		writeJSON(w, http.StatusOK, artifactsResponse{
			ArtifactManifest: agent.BuildArtifactManifestFor(spec), Source: "live",
		})
		return
	}

	if parts[0] != "file" {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown artifacts sub-path %q", parts[0]))
		return
	}
	s.serveTeamArtifactFile(w, r, spec, parts[1:])
}

// serveTeamArtifactFile 取单个产物文件。三重守卫:
// ① root 必须白名单化到 ArtifactRoots 真实返回的根 (不接受调用方给路径);
// ② 拼接后取绝对路径必须仍在该根内 (防 ../ 穿越, 与 media 端点同一形态);
// ③ work 根额外要求文件通过归属判据 —— cwd 是用户真实 git 仓库, 里面有本团队从未
//
//	产出的私有文件 (.env / 密钥 / 源码), 不能因为"在根内"就一律外送。
func (s *Server) serveTeamArtifactFile(w http.ResponseWriter, r *http.Request, spec agent.ArtifactScanSpec, relParts []string) {
	kind := r.URL.Query().Get("root")
	if kind == "" {
		kind = agent.ArtifactRootData
	}
	var rootPath string
	for _, root := range agent.RootsForScan(spec) {
		if root.Kind == kind {
			rootPath = root.Path
			break
		}
	}
	if rootPath == "" {
		writeError(w, http.StatusForbidden, fmt.Errorf("root %q 不在白名单内", kind))
		return
	}
	if len(relParts) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("file path required"))
		return
	}
	full, err := filepath.Abs(filepath.Join(rootPath, filepath.Join(relParts...)))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if full != rootPath && !strings.HasPrefix(full, rootPath+string(os.PathSeparator)) {
		writeError(w, http.StatusForbidden, fmt.Errorf("path traversal blocked"))
		return
	}
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, fmt.Errorf("artifact not found"))
		return
	}
	if kind == agent.ArtifactRootWork {
		rel := strings.TrimPrefix(strings.TrimPrefix(full, rootPath), string(os.PathSeparator))
		if !s.workArtifactAllowed(spec, filepath.ToSlash(rel), info.ModTime()) {
			writeError(w, http.StatusForbidden, fmt.Errorf("该文件不属于本团队产物"))
			return
		}
	}
	// 模型生成的 HTML/SVG 用 CSP sandbox 隔离 (与 media 端点一致), 防止注入 dashboard 同源。
	lower := strings.ToLower(full)
	if strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".svg") {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts;")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	http.ServeFile(w, r, full)
}

// workArtifactAllowed cwd 侧文件的归属校验: 落在团队的归属时间窗内 (与采集器同一
// 判据), 或落盘清单里明确记过它 (物化记账的文件可能 mtime 更早)。
func (s *Server) workArtifactAllowed(spec agent.ArtifactScanSpec, rel string, mod time.Time) bool {
	if agent.ArtifactAttributed(spec, mod) {
		return true
	}
	if m, err := agent.ReadArtifactManifest(spec.DataDir); err == nil && m != nil {
		for _, f := range m.Files {
			if f.Root == agent.ArtifactRootWork && f.Rel == rel {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	list, err := s.provider.AllMetricSummaries()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleMetricsModule(w http.ResponseWriter, r *http.Request) {
	module := strings.TrimPrefix(r.URL.Path, "/api/metrics/")
	module = strings.Trim(module, "/")
	if module == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("module required"))
		return
	}
	// 默认返回 events + summary 组合体, 方便前端一次拉取
	limit := parseIntQuery(r, "limit", 2000)
	sinceStr := r.URL.Query().Get("since")
	var since time.Time
	if sinceStr != "" {
		if d, err := time.ParseDuration(sinceStr); err == nil {
			since = time.Now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			since = t
		}
	}
	events, err := s.provider.ModuleMetricEvents(module, limit, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "csv" {
		writeMetricsCSV(w, module, events)
		return
	}
	summary, _ := s.provider.ModuleMetricSummary(module)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"module":  module,
		"events":  events,
		"summary": summary,
	})
}

// writeMetricsCSV 将指标事件导出 CSV (timestamp, module, name, value, labels)。
func writeMetricsCSV(w http.ResponseWriter, module string, events []MetricEventDTO) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"metrics-%s.csv\"", module))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("timestamp,module,name,value,labels\n"))
	for _, e := range events {
		lbl := ""
		if len(e.Labels) > 0 {
			b, _ := json.Marshal(e.Labels)
			lbl = strings.ReplaceAll(string(b), "\"", "\"\"")
		}
		line := fmt.Sprintf("%s,%s,%s,%v,\"%s\"\n",
			e.Timestamp.Format(time.RFC3339),
			module,
			csvEscape(e.Name),
			e.Value,
			lbl,
		)
		_, _ = w.Write([]byte(line))
	}
}

func csvEscape(s string) string {
	if strings.ContainsAny(s, ",\n\"") {
		return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
	}
	return s
}

func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	resp, err := s.provider.BuildInsights()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if r.URL.Query().Get("llm") == "1" {
		resp.LLMEnabled = true
		summary, err := buildLLMSummary(r.Context(), resp)
		if err != nil {
			resp.LLMError = err.Error()
		} else {
			resp.LLMSummary = summary
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	list, err := s.provider.ListProjects(root)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleStreamOverview 使用 SSE 推送 overview (轮询 provider, 支持 ?interval=2s)。
func (s *Server) handleStreamOverview(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	interval := 3 * time.Second
	if v := r.URL.Query().Get("interval"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Second && d <= time.Minute {
			interval = d
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	send := func() bool {
		resp, err := s.provider.BuildOverview()
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			flusher.Flush()
			return true
		}
		b, _ := json.Marshal(resp)
		fmt.Fprintf(w, "event: overview\ndata: %s\n\n", string(b))
		flusher.Flush()
		return true
	}
	send()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

func (s *Server) handleCron(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 有活动调度器(:18080)时直接返回其内存态, 与写路径同源、避开 cron_jobs.json 的
		// map/array 格式差异; 独立 dashboard(:7777)无调度器时回退读文件(parser 已兼容两种格式)。
		if ctl := s.resolveCron(); ctl != nil {
			writeJSON(w, http.StatusOK, ctl.ListJobs())
			return
		}
		list, err := s.provider.ListCronJobs()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		// 创建定时任务 (写入活动调度器并持久化)。
		ctl := s.resolveCron()
		if ctl == nil {
			writeError(w, http.StatusNotImplemented, fmt.Errorf("此 dashboard 实例未挂载定时任务调度器 (请直连 :18080 飞书进程)"))
			return
		}
		var job agent.CronJob
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("请求体解析失败: %w", err))
			return
		}
		job.ID = "" // ID 由调度器生成
		if err := ctl.AddJob(&job); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, job)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("不支持的方法 %s", r.Method))
	}
}

// handleModels 处理 GET /api/models: 返回 providers 注册的可用模型别名清单。
// 消费方: webapp cron 面板的任务模型下拉等。未注入解析器时 501。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("不支持的方法 %s", r.Method))
		return
	}
	if s.modelLister == nil {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("此实例未注入模型清单解析器"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": s.modelLister()})
}

// handleCronItem 处理 /api/cron/{id}: PATCH/PUT 更新或启停, DELETE 删除。
// 仅在注入了活动调度器的实例 (:18080) 上可用; 否则返回 501。
// 另: POST /api/cron/{id}/trigger 立即触发一次 (经 CronScheduler.TriggerJob 真实执行,
// 取代此前全仓无消费方的 "cron.trigger" 动作队列假承诺)。
func (s *Server) handleCronItem(w http.ResponseWriter, r *http.Request) {
	ctl := s.resolveCron()
	if ctl == nil {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("此 dashboard 实例未挂载定时任务调度器 (请直连 :18080 飞书进程)"))
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/cron/")

	// POST /api/cron/{id}/trigger: 立即触发一次 (真实执行, 非动作队列)。
	if strings.HasSuffix(id, "/trigger") {
		jobID := strings.TrimSuffix(id, "/trigger")
		if jobID == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("缺少任务 id"))
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("不支持的方法 %s", r.Method))
			return
		}
		if err := ctl.TriggerJob(jobID); err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": jobID, "triggered": true})
		return
	}

	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("缺少或非法的任务 id"))
		return
	}

	switch r.Method {
	case http.MethodDelete:
		if err := ctl.RemoveJob(id); err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": id})
	case http.MethodPatch, http.MethodPut:
		var body struct {
			Name     string  `json:"name"`
			Schedule string  `json:"schedule"`
			JobType  string  `json:"jobType"`
			Payload  string  `json:"payload"`
			Workflow string  `json:"workflow"`
			ChatID   string  `json:"chatId"`
			Model    *string `json:"model"`   // 指针: 显式提供才更新; 空串 = 清除回默认
			Enabled  *bool   `json:"enabled"` // 指针: 仅在请求显式提供时才启停
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("请求体解析失败: %w", err))
			return
		}
		// 启停 (显式提供 enabled 时)
		if body.Enabled != nil {
			var err error
			if *body.Enabled {
				err = ctl.ResumeJob(id)
			} else {
				err = ctl.PauseJob(id)
			}
			if err != nil {
				writeError(w, http.StatusNotFound, err)
				return
			}
		}
		// 字段更新 (任一非空字段, 或显式提供的 model)
		if body.Name != "" || body.Schedule != "" || body.JobType != "" || body.Payload != "" || body.Workflow != "" || body.ChatID != "" || body.Model != nil {
			patch := &agent.CronJob{
				Name: body.Name, Schedule: body.Schedule, JobType: body.JobType,
				Payload: body.Payload, Workflow: body.Workflow, ChatID: body.ChatID,
			}
			if body.Model != nil {
				m := strings.TrimSpace(*body.Model)
				if m == "" {
					m = "-" // 显式空串 → 清除哨兵 (UpdateJob: 空 = 不动, "-" = 清除)
				}
				patch.Model = m
			}
			if err := ctl.UpdateJob(id, patch); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
		if j := ctl.GetJob(id); j != nil {
			writeJSON(w, http.StatusOK, j)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": id})
	default:
		w.Header().Set("Allow", "PATCH, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("不支持的方法 %s", r.Method))
	}
}

func (s *Server) handleDreaming(w http.ResponseWriter, r *http.Request) {
	resp, err := s.provider.LoadDreaming()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleEvolution(w http.ResponseWriter, r *http.Request) {
	resp, err := s.provider.LoadEvolution()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	list, err := s.provider.ListTasks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// 所有非 API / 非 static 路径返回 index.html (SPA)
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/static/") {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "index.html missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// ============== Helpers ==============

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, APIError{Error: err.Error()})
}

func parseIntQuery(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// handleMetricsFullModule 返回某模块的所有 Prometheus 时序数据 (带最近历史)。
// GET /api/metrics/full?module=llm&hours=24
func (s *Server) handleMetricsFullModule(w http.ResponseWriter, r *http.Request) {
	module := r.URL.Query().Get("module")
	if module == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("module required"))
		return
	}
	hours := parseIntQuery(r, "hours", 24)
	promURL := os.Getenv("CLAUDE_GO_PROMETHEUS_URL")
	if promURL == "" {
		// 无外部 Prometheus, 回退到 JSONL
		events, err := s.provider.ModuleMetricEvents(module, 5000, time.Now().Add(-time.Duration(hours)*time.Hour))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		summary, _ := s.provider.ModuleMetricSummary(module)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"module":  module,
			"source":  "jsonl",
			"events":  events,
			"summary": summary,
		})
		return
	}

	// 从 Prometheus 查询
	end := time.Now()
	start := end.Add(-time.Duration(hours) * time.Hour)
	step := time.Duration(float64(hours*3600)/500) * time.Second
	if step < 15*time.Second {
		step = 15 * time.Second
	}

	// 查询所有该模块的指标
	query := fmt.Sprintf(`{module="%s"}`, module)
	resp, err := queryPromRange(promURL, query, start, end, step)
	if err != nil || resp.Status != "success" {
		// 如果 range query 失败, 回退到 instant
		instResp, instErr := queryPromInstant(promURL, query)
		if instErr != nil || instResp.Status != "success" {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"module":  module,
				"source":  "prometheus",
				"error":   "no data",
				"events":  []MetricEventDTO{},
				"summary": map[string]interface{}{},
			})
			return
		}
		// 构造事件
		var events []MetricEventDTO
		for _, r := range instResp.Data.Result {
			if len(r.Value) < 2 {
				continue
			}
			tsFloat, _ := r.Value[0].(float64)
			valFloat, _ := r.Value[1].(string)
			val, _ := strconv.ParseFloat(valFloat, 64)
			events = append(events, MetricEventDTO{
				Timestamp: time.Unix(int64(tsFloat), 0),
				Module:    module,
				Name:      r.Metric["__name__"],
				Value:     val,
				Labels:    r.Metric,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"module":  module,
			"source":  "prometheus",
			"events":  events,
			"summary": summarizeEvents(module, events),
		})
		return
	}

	// 转换 range query 结果
	var events []MetricEventDTO
	for _, r := range resp.Data.Result {
		name := r.Metric["__name__"]
		for _, v := range r.Values {
			if len(v) < 2 {
				continue
			}
			tsFloat, _ := v[0].(float64)
			valFloat, _ := v[1].(string)
			val, _ := strconv.ParseFloat(valFloat, 64)
			events = append(events, MetricEventDTO{
				Timestamp: time.Unix(int64(tsFloat), 0),
				Module:    module,
				Name:      name,
				Value:     val,
				Labels:    r.Metric,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"module":  module,
		"source":  "prometheus",
		"events":  events,
		"summary": summarizeEvents(module, events),
	})
}

// handlePromQuery 代理 Prometheus PromQL 查询。
// GET /api/prom/query?query=...&start=...&end=...&step=...
// GET /api/prom/query?type=instant&query=...
func (s *Server) handlePromQuery(w http.ResponseWriter, r *http.Request) {
	promURL := os.Getenv("CLAUDE_GO_PROMETHEUS_URL")
	if promURL == "" {
		writeError(w, http.StatusBadGateway, fmt.Errorf("Prometheus not configured (CLAUDE_GO_PROMETHEUS_URL)"))
		return
	}

	queryStr := r.URL.Query().Get("query")
	if queryStr == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("query required"))
		return
	}

	qtype := r.URL.Query().Get("type")
	if qtype == "instant" || qtype == "" && r.URL.Query().Get("start") == "" {
		resp, err := queryPromInstant(promURL, queryStr)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// range query
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	stepStr := r.URL.Query().Get("step")

	var start, end time.Time
	var step time.Duration
	var err error

	if t, e := time.Parse(time.RFC3339, startStr); e == nil {
		start = t
	} else if d, e := time.ParseDuration(startStr); e == nil {
		start = time.Now().Add(-d)
	} else {
		start = time.Now().Add(-1 * time.Hour)
	}

	if t, e := time.Parse(time.RFC3339, endStr); e == nil {
		end = t
	} else {
		end = time.Now()
	}

	if stepStr != "" {
		step, err = time.ParseDuration(stepStr)
		if err != nil {
			step = 15 * time.Second
		}
	} else {
		step = 15 * time.Second
	}

	resp, err := queryPromRange(promURL, queryStr, start, end, step)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// summarizeEvents 从事件列表构造简易摘要。
func summarizeEvents(module string, events []MetricEventDTO) map[string]interface{} {
	metrics := map[string][]float64{}
	for _, e := range events {
		metrics[e.Name] = append(metrics[e.Name], e.Value)
	}
	out := map[string]interface{}{"module": module}
	for name, vs := range metrics {
		if len(vs) == 0 {
			continue
		}
		sum := 0.0
		minV, maxV := vs[0], vs[0]
		for _, v := range vs {
			sum += v
			if v < minV {
				minV = v
			}
			if v > maxV {
				maxV = v
			}
		}
		out[name] = map[string]interface{}{
			"count": len(vs),
			"last":  vs[len(vs)-1],
			"avg":   sum / float64(len(vs)),
			"min":   minV,
			"max":   maxV,
		}
	}
	return out
}
