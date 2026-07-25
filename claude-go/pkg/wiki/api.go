package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/httpauth"
	"github.com/anthropic/claude-go/pkg/sync"
)

// APIServer 为 Obsidian 插件等外部客户端提供 HTTP API。
//
// 注意本结构实际承载的远不止 wiki：dashboard 的 /api/*（50 条）、cluster 的
// /cluster/*、SPA 与 /metrics 都经 Mux()/Handle() 挂到同一个 ServeMux 上，
// 由 Start() 统一对外服务。因此 Start() 里的鉴权与监听地址决定了整个
// :18080 面的暴露程度，改动前务必读 pkg/httpauth 的包注释。
type APIServer struct {
	engine    *Engine
	secret    string
	host      string // 监听地址的 host 部分；空 = 全部网卡（历史行为）
	mux       *http.ServeMux
	server    *http.Server
	scheduler *sync.Scheduler
}

// NewAPIServer 创建 Wiki HTTP API 服务器。
func NewAPIServer(engine *Engine, secret string) *APIServer {
	s := &APIServer{
		engine: engine,
		secret: secret,
		mux:    http.NewServeMux(),
	}
	s.mux.HandleFunc("/wiki/status", s.auth(s.handleStatus))
	s.mux.HandleFunc("/wiki/ingest", s.auth(s.handleIngest))
	s.mux.HandleFunc("/wiki/query", s.auth(s.handleQuery))
	s.mux.HandleFunc("/wiki/lint", s.auth(s.handleLint))
	s.mux.HandleFunc("/wiki/organize", s.auth(s.handleOrganize))
	s.mux.HandleFunc("/wiki/health-check", s.auth(s.handleHealthCheck))

	s.mux.HandleFunc("/sync/ima", s.auth(s.handleSyncIMA))
	s.mux.HandleFunc("/sync/weread", s.auth(s.handleSyncWeRead))
	s.mux.HandleFunc("/sync/status/", s.auth(s.handleSyncStatus))
	return s
}

// SetScheduler 设置同步调度器，启用 /sync/* 端点。
func (s *APIServer) SetScheduler(sched *sync.Scheduler) {
	s.scheduler = sched
}

// Mux 返回 APIServer 内部的 ServeMux, 便于调用方追加路由 (如 dashboard 复用
// 同一 HTTP 端口)。dashboard/feishu-bot 统一 HTTP 服务使用。
func (s *APIServer) Mux() *http.ServeMux { return s.mux }

// Handle 代理 ServeMux.Handle, 允许外部把自己的路由挂到 wiki API 同一个端口。
func (s *APIServer) Handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
}

// HandleFunc 代理 ServeMux.HandleFunc。
func (s *APIServer) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	s.mux.HandleFunc(pattern, h)
}

// SetBindHost 设置监听地址的 host 部分（如 "127.0.0.1"）。
// 空字符串沿用历史行为：绑全部网卡，局域网与 tailscale 均可访问。
func (s *APIServer) SetBindHost(host string) { s.host = host }

// Start 启动 HTTP 服务（非阻塞）。
//
// 鉴权在 Handler 层统一施加，覆盖挂在同一 mux 上的全部路由（含 dashboard
// 的 /api/* 与 cluster 的 /cluster/*）。历史上鉴权是逐路由 s.auth(...) 包装的，
// 结果这两组都漏了——高度不对。secret 为空时中间件是恒等包装，行为完全中性。
func (s *APIServer) Start(port int) error {
	addr := fmt.Sprintf("%s:%d", s.host, port)
	handler := httpauth.Middleware(httpauth.Config{Secret: s.secret})(s.mux)
	s.server = &http.Server{
		Addr:    addr,
		Handler: handler,
		// 防 Slowloris：无此超时时慢速发头的连接可无限占用。
		ReadHeaderTimeout: 20 * time.Second,
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return fmt.Errorf("wiki-api: 监听 %s 失败: %w", addr, err)
	}
	httpauth.WarnIfExposed("wiki-api", addr, s.secret)
	go func() {
		log.Printf("[wiki-api] 启动 HTTP 服务 %s", addr)
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[wiki-api] 服务异常: %v", err)
		}
	}()
	return nil
}

// Stop 优雅停止。
func (s *APIServer) Stop() {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(ctx)
	}
}

func (s *APIServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.secret != "" {
			token := r.Header.Get("Authorization")
			token = strings.TrimPrefix(token, "Bearer ")
			if token != s.secret {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

func (s *APIServer) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Status())
}

func (s *APIServer) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var req struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	var ingestErr error
	if req.URL != "" {
		ingestErr = s.engine.Ingest(ctx, req.URL)
	} else if req.Text != "" {
		title := req.Title
		if title == "" {
			title = "api-ingest"
		}
		ingestErr = s.engine.IngestText(ctx, title, req.Text, "api")
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url 或 text 至少提供一个"})
		return
	}
	if ingestErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": ingestErr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *APIServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var req struct {
		Question  string `json:"question"`
		Archive   bool   `json:"archive"`
		WebSearch bool   `json:"webSearch"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Question == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "question 不能为空"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	if req.WebSearch {
		answer, err := s.engine.QueryWithWebSearch(ctx, req.Question)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"answer": answer})
		return
	}

	if req.Archive {
		answer, archived, err := s.engine.QueryAndArchive(ctx, req.Question)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"answer": answer, "archived": archived})
	} else {
		answer, err := s.engine.Query(ctx, req.Question)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"answer": answer})
	}
}

func (s *APIServer) handleLint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()

	report, err := s.engine.Lint(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *APIServer) handleOrganize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var req struct {
		Mode string `json:"mode"` // "full" 或 "incremental"
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	var result *OrganizeResult
	var err error
	if req.Mode == "incremental" {
		result, err = s.engine.IncrementalOrganize(ctx)
	} else {
		result, err = s.engine.Organize(ctx)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *APIServer) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	report, err := s.engine.HealthCheck(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *APIServer) handleSyncIMA(w http.ResponseWriter, r *http.Request) {
	s.handleSyncTrigger(w, r, "ima")
}

func (s *APIServer) handleSyncWeRead(w http.ResponseWriter, r *http.Request) {
	s.handleSyncTrigger(w, r, "weread")
}

func (s *APIServer) handleSyncTrigger(w http.ResponseWriter, r *http.Request, source string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	if s.scheduler == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sync scheduler not configured"})
		return
	}
	cfg := s.scheduler.Config()
	var fn func(context.Context) error
	switch source {
	case "ima":
		fn = func(ctx context.Context) error {
			adapter := sync.NewIMAAdapter(cfg.IMA.ClientID, cfg.IMA.APIKey)
			res, err := sync.RunSync(cfg, adapter.Source(), adapter)
			if err == nil {
				log.Printf("[sync/api] IMA sync finished: %+v", res)
			}
			return err
		}
	case "weread":
		fn = func(ctx context.Context) error {
			adapter := sync.NewWeReadAdapter(cfg.WeRead.APIKey)
			res, err := sync.RunSync(cfg, adapter.Source(), adapter)
			if err == nil {
				log.Printf("[sync/api] WeRead sync finished: %+v", res)
			}
			return err
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown source"})
		return
	}
	job := s.scheduler.RunNow(source, fn)
	log.Printf("[sync/api] triggered %s sync job %s", source, job.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"jobId": job.ID})
}

func (s *APIServer) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	if s.scheduler == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sync scheduler not configured"})
		return
	}
	prefix := "/sync/status/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing job id"})
		return
	}
	jobID := strings.TrimPrefix(r.URL.Path, prefix)
	job, ok := s.scheduler.JobStatusByID(jobID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func readJSON(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取请求体失败: %w", err)
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}
