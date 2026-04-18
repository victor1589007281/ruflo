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
)

//go:embed web/*
var webFS embed.FS

// Config dashboard 启动配置。
type Config struct {
	StateDir string        // 数据根目录 (.claude-go)
	Addr     string        // 监听地址, 例如 "127.0.0.1:7777"
	CacheTTL time.Duration // 数据缓存 TTL, 0 禁用
}

// Server HTTP 服务。
type Server struct {
	cfg      Config
	provider *Provider
	mux      *http.ServeMux
	server   *http.Server
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
	}
	s.registerRoutes()
	s.server = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
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

func (s *Server) registerRoutes() {
	// 静态资源
	sub, err := fs.Sub(webFS, "web")
	if err == nil {
		fileServer := http.FileServer(http.FS(sub))
		s.mux.Handle("/static/", http.StripPrefix("/static/", fileServer))
	}

	// API
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/overview", s.handleOverview)
	s.mux.HandleFunc("/api/teams", s.handleTeams)
	s.mux.HandleFunc("/api/teams/", s.handleTeamDetail)
	s.mux.HandleFunc("/api/metrics", s.handleMetrics)
	s.mux.HandleFunc("/api/metrics/", s.handleMetricsModule)
	s.mux.HandleFunc("/api/cron", s.handleCron)
	s.mux.HandleFunc("/api/dreaming", s.handleDreaming)
	s.mux.HandleFunc("/api/evolution", s.handleEvolution)
	s.mux.HandleFunc("/api/tasks", s.handleTasks)
	s.mux.HandleFunc("/api/insights", s.handleInsights)
	s.mux.HandleFunc("/api/projects", s.handleProjects)
	s.mux.HandleFunc("/api/stream/overview", s.handleStreamOverview)

	// 根路径和 SPA fallback
	s.mux.HandleFunc("/", s.handleIndex)
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
