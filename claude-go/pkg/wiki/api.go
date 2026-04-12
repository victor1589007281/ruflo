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
)

// APIServer 为 Obsidian 插件等外部客户端提供 HTTP API。
type APIServer struct {
	engine *Engine
	secret string
	mux    *http.ServeMux
	server *http.Server
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
	return s
}

// Start 启动 HTTP 服务（非阻塞）。
func (s *APIServer) Start(port int) error {
	addr := fmt.Sprintf(":%d", port)
	s.server = &http.Server{Addr: addr, Handler: s.mux}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return fmt.Errorf("wiki-api: 监听端口 %d 失败: %w", port, err)
	}
	go func() {
		log.Printf("[wiki-api] 启动 HTTP 服务 :%d", port)
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

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
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
		Question string `json:"question"`
		Archive  bool   `json:"archive"`
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
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	report, err := s.engine.HealthCheck(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
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
