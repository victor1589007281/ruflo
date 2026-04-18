package dashboard

// Backup / Restore HTTP 接口。
// 路由 (见 server.go registerRoutes):
//   GET    /api/backups                     列出备份
//   POST   /api/backups/create               创建备份 (body: {label?, includeReports?})
//   POST   /api/backups/restore              恢复备份 (body: {path, skipSafetyBackup?, overwrite?})
//   DELETE /api/backups?path=xxx             删除备份
//   GET    /api/backups/manifest?path=xxx    读单条 manifest (不下载整个包)
//   GET    /api/backups/download?path=xxx    直接下载 .tar.gz
//   GET    /api/llm/status                   返回 LLM client 当前解析出的 profile, 便于前端判断诊断可用性

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropic/claude-go/pkg/backup"
)

// ======================================================================
// /api/backups  (list)
// /api/backups?path=xxx  (DELETE)
// ======================================================================

func (s *Server) handleBackupsList(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := backup.List(s.cfg.StateDir)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"stateDir": s.cfg.StateDir,
			"count":    len(list),
			"entries":  list,
		})
	case http.MethodDelete:
		p := r.URL.Query().Get("path")
		if p == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("query ?path=xxx 必填"))
			return
		}
		if err := backup.Delete(s.cfg.StateDir, p); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "deleted": p})
	default:
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET or DELETE required"))
	}
}

// ======================================================================
// /api/backups/create  POST
// ======================================================================

type createBackupReq struct {
	Label          string   `json:"label,omitempty"`
	OutPath        string   `json:"outPath,omitempty"`
	Targets        []string `json:"targets,omitempty"`
	IncludeReports *bool    `json:"includeReports,omitempty"`
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req createBackupReq
	if r.Body != nil {
		defer r.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(r.Body, 32*1024))
		_ = json.Unmarshal(body, &req)
	}
	includeReports := true
	if req.IncludeReports != nil {
		includeReports = *req.IncludeReports
	}
	res, err := backup.Create(backup.CreateOptions{
		StateDir:       s.cfg.StateDir,
		OutPath:        req.OutPath,
		Label:          req.Label,
		Targets:        req.Targets,
		IncludeReports: includeReports,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ======================================================================
// /api/backups/restore  POST
// ======================================================================

type restoreBackupReq struct {
	Path             string `json:"path"`
	SkipSafetyBackup bool   `json:"skipSafetyBackup,omitempty"`
	Overwrite        *bool  `json:"overwrite,omitempty"`
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req restoreBackupReq
	if r.Body != nil {
		defer r.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(r.Body, 32*1024))
		_ = json.Unmarshal(body, &req)
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("path 必填 (body.path)"))
		return
	}
	overwrite := true
	if req.Overwrite != nil {
		overwrite = *req.Overwrite
	}
	// 限制只能恢复 stateDir/backups 下的归档, 防止误把任意文件解压进来
	root, _ := filepath.Abs(filepath.Join(s.cfg.StateDir, "backups"))
	abs, _ := filepath.Abs(req.Path)
	if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("path 必须位于 %s 之下", root))
		return
	}

	res, err := backup.Restore(backup.RestoreOptions{
		StateDir:         s.cfg.StateDir,
		ArchivePath:      req.Path,
		SkipSafetyBackup: req.SkipSafetyBackup,
		Overwrite:        overwrite,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ======================================================================
// /api/backups/manifest?path=xxx  GET
// ======================================================================

func (s *Server) handleBackupManifest(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("?path=xxx 必填"))
		return
	}
	root, _ := filepath.Abs(filepath.Join(s.cfg.StateDir, "backups"))
	abs, _ := filepath.Abs(p)
	if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("path 必须位于 %s 之下", root))
		return
	}
	m, err := backup.ReadManifest(p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ======================================================================
// /api/backups/download?path=xxx  GET (serve tar.gz)
// ======================================================================

func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("?path=xxx 必填"))
		return
	}
	root, _ := filepath.Abs(filepath.Join(s.cfg.StateDir, "backups"))
	abs, _ := filepath.Abs(p)
	if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("path 必须位于 %s 之下", root))
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", filepath.Base(abs)))
	if info != nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	}
	_, _ = io.Copy(w, f)
}

// ======================================================================
// /api/llm/status  GET
// 返回当前 dashboard 能不能做 LLM 诊断, profile 从哪里读到
// ======================================================================

func (s *Server) handleLLMStatus(w http.ResponseWriter, r *http.Request) {
	_, profile, err := GetSharedLLMClient()
	status := map[string]interface{}{
		"profile": profile,
		"ready":   err == nil,
	}
	if err != nil {
		status["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, status)
}
