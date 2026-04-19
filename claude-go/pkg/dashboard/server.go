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
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/metrics"
)

//go:embed web/*
var webFS embed.FS

// TeamActionFunc 团队操作回调: action 可为 "stop"/"restart"/"delete"。
type TeamActionFunc func(action, teamName string) error

// Config dashboard 启动配置。
type Config struct {
	StateDir   string         // 数据根目录 (.claude-go)
	Addr       string         // 监听地址, 例如 "127.0.0.1:7777"
	CacheTTL   time.Duration  // 数据缓存 TTL, 0 禁用
	TeamAction TeamActionFunc // 团队操作回调 (stop/restart/delete)，为 nil 时尝试转发到 BotAPIURL
	BotAPIURL  string         // Feishu bot 的 wiki API URL (如 "http://127.0.0.1:18080"), 用于转发 team 操作
}

// Server HTTP 服务。
type Server struct {
	cfg      Config
	provider *Provider
	mux      *http.ServeMux
	server   *http.Server
	jobs     *diagJobStore // 异步 LLM 诊断作业
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
	s.registerRoutesOn(mux)
	return s
}

// ListenAndServe 启动 HTTP 服务, 阻塞直到被 Stop 或出错。
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("dashboard listen %s: %w", s.cfg.Addr, err)
	}
	log.Printf("[dashboard] listening on http://%s  (stateDir=%s)", ln.Addr(), s.cfg.StateDir)
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
	mux.HandleFunc("/api/cron", s.handleCron)
	mux.HandleFunc("/api/dreaming", s.handleDreaming)
	mux.HandleFunc("/api/evolution", s.handleEvolution)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/insights", s.handleInsights)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/stream/overview", s.handleStreamOverview)

	// v1.1 扩展
	mux.HandleFunc("/api/workflows", s.handleWorkflows)
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

	// v1.5: 异步 LLM 诊断作业 (team/dreaming/evolution)
	//   POST /api/diag/jobs           -> 创建作业 (入参 {kind, target?})
	//   GET  /api/diag/jobs           -> 列出最近作业
	//   GET  /api/diag/jobs/{id}      -> 查询作业状态 / 结果
	//   POST /api/dreaming/trigger    -> 主动触发一次 dreaming
	mux.HandleFunc("/api/diag/jobs", s.handleDiagJobs)
	mux.HandleFunc("/api/diag/jobs/", s.handleDiagJobDetail)
	mux.HandleFunc("/api/dreaming/trigger", s.handleDreamingTrigger)

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
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown sub-path %q", sub))
	}
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
	list, err := s.provider.ListCronJobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
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
