package dashboard

// harness_evo_catalog_handlers.go — 任务集目录与跨运行 harness 版本只读/比较 API。
// 版本正文只能来自编排器登记, 这里不提供任意文件路径读取, 因而不引入路径逃逸面。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropic/claude-go/pkg/harnesspro"
)

// handleHarnessEvoTaskSets GET 列表 / POST 注册不可变任务集。
func (s *Server) handleHarnessEvoTaskSets(w http.ResponseWriter, r *http.Request) {
	mgr := sharedHarnessEvoManager(s.cfg.StateDir)
	store := harnesspro.NewTaskSetStore(mgr.taskSetDir())
	switch r.Method {
	case http.MethodGet:
		sets, err := store.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasksets": sets})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 8*1024*1024)
		defer r.Body.Close()
		var set harnesspro.TaskSet
		if err := json.NewDecoder(r.Body).Decode(&set); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("任务集 JSON 解析失败: %w", err))
			return
		}
		if err := store.Register(set); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		// 以重新加载的磁盘内容回显, 证明 hash/task_count 与注册结果一致。
		registered, err := store.Get(set.Name + "@" + set.Version)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, registered)
	default:
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET/POST required"))
	}
}

// handleHarnessEvoTaskSet GET /api/harness-evo/tasksets/{name[@version]}[?version=v]。
func (s *Server) handleHarnessEvoTaskSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/harness-evo/tasksets/"), "/")
	if rest == "" || strings.Contains(rest, "/") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("path must be /api/harness-evo/tasksets/{name[@version]}"))
		return
	}
	if version := r.URL.Query().Get("version"); version != "" {
		if strings.Contains(rest, "@") {
			writeError(w, http.StatusBadRequest, fmt.Errorf("版本不能同时写在路径和 query"))
			return
		}
		rest += "@" + version
	}
	mgr := sharedHarnessEvoManager(s.cfg.StateDir)
	set, err := harnesspro.NewTaskSetStore(mgr.taskSetDir()).Get(rest)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// handleHarnessEvoVersions GET 版本列表/批量比较。ids 逗号分隔, 与 run/content 过滤可组合。
func (s *Server) handleHarnessEvoVersions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	mgr := sharedHarnessEvoManager(s.cfg.StateDir)
	registry, err := harnesspro.NewVersionRegistry(mgr.versionDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ids := map[string]bool{}
	if raw := r.URL.Query().Get("ids"); raw != "" {
		parts := strings.Split(raw, ",")
		if len(parts) > 100 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("一次最多比较 100 个版本"))
			return
		}
		for _, id := range parts {
			if id = strings.TrimSpace(id); id != "" {
				ids[id] = true
			}
		}
	}
	versions, err := registry.List(r.URL.Query().Get("run_id"), r.URL.Query().Get("content_id"), ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

// handleHarnessEvoVersion GET /api/harness-evo/versions/{version_id}。
func (s *Server) handleHarnessEvoVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/harness-evo/versions/"), "/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("path must be /api/harness-evo/versions/{version_id}"))
		return
	}
	mgr := sharedHarnessEvoManager(s.cfg.StateDir)
	registry, err := harnesspro.NewVersionRegistry(mgr.versionDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	version, err := registry.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}
