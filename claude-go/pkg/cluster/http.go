package cluster

// cluster HTTP 层 (design/02 §3.2/3.3 R3): 控制面把 Queue+Registry 经 HTTP 暴露,
// worker 经 Client 长轮询拉取/回报/心跳。控制面无状态 (状态在 StateStore),
// 多副本可共享同一后端。
//
// 端点:
//   POST /cluster/enqueue          入队 (控制面内部/编排层调用)
//   POST /cluster/pull             worker 拉取 (body: {worker, kinds})
//   POST /cluster/complete         worker 回报成功 (body: {id, worker, result})
//   POST /cluster/fail             worker 回报失败 (body: {id, worker, err})
//   POST /cluster/extend           worker 续租
//   POST /cluster/heartbeat        worker 心跳注册
//   GET  /cluster/workers          存活 worker 列表 (观测)
//   GET  /cluster/tasks            任务列表 (观测)

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

// Mount 把 cluster 端点挂到 mux。
func Mount(mux *http.ServeMux, q *Queue, reg *Registry) {
	h := &httpHandler{q: q, reg: reg}
	mux.HandleFunc("/cluster/enqueue", h.enqueue)
	mux.HandleFunc("/cluster/pull", h.pull)
	mux.HandleFunc("/cluster/complete", h.complete)
	mux.HandleFunc("/cluster/fail", h.fail)
	mux.HandleFunc("/cluster/extend", h.extend)
	mux.HandleFunc("/cluster/heartbeat", h.heartbeat)
	mux.HandleFunc("/cluster/workers", h.workers)
	mux.HandleFunc("/cluster/tasks", h.tasks)
}

type httpHandler struct {
	q   *Queue
	reg *Registry
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *httpHandler) enqueue(w http.ResponseWriter, r *http.Request) {
	var t Task
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	id, err := h.q.Enqueue(t)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"id": id})
}

func (h *httpHandler) pull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Worker string   `json:"worker"`
		Kinds  []string `json:"kinds"`
		Caps   []string `json:"caps"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	task, ok, err := h.q.PullFor(req.Worker, req.Kinds, req.Caps)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, 204, map[string]any{"task": nil}) // 无任务
		return
	}
	writeJSON(w, 200, map[string]any{"task": task})
}

func (h *httpHandler) complete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string          `json:"id"`
		Worker string          `json:"worker"`
		Result json.RawMessage `json:"result"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.q.Complete(req.ID, req.Worker, req.Result); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *httpHandler) fail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Worker string `json:"worker"`
		Err    string `json:"err"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.q.Fail(req.ID, req.Worker, req.Err); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *httpHandler) extend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Worker string `json:"worker"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.q.Extend(req.ID, req.Worker); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *httpHandler) heartbeat(w http.ResponseWriter, r *http.Request) {
	var info WorkerInfo
	if err := json.NewDecoder(r.Body).Decode(&info); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := h.reg.Heartbeat(info); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *httpHandler) workers(w http.ResponseWriter, r *http.Request) {
	alive, err := h.reg.Alive()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"workers": alive, "count": len(alive)})
}

func (h *httpHandler) tasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := h.q.List()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"tasks": tasks, "count": len(tasks)})
}

// --- Client: worker 侧 ---

// Client worker 连接控制面的 HTTP 客户端。
type Client struct {
	base   string
	worker string
	http   *http.Client
}

// NewClient control 为控制面基址 (如 http://claude-go-control:18080)。
func NewClient(base, worker string) *Client {
	return &Client{base: base, worker: worker, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) post(path string, body, out any) (int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", c.base+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == 200 {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// Heartbeat 注册/续约。
func (c *Client) Heartbeat(caps, kinds []string) error {
	_, err := c.post("/cluster/heartbeat", WorkerInfo{Name: c.worker, Caps: caps, Kinds: kinds}, nil)
	return err
}

// Pull 拉取任务 (无任务返回 nil,nil)。caps 为本 worker 的能力标签, 供控制面路由。
func (c *Client) Pull(kinds []string) (*Task, error) {
	return c.PullWithCaps(kinds, nil)
}

// PullWithCaps 携带能力标签拉取 (control 侧按 RequireCaps ⊆ caps 过滤)。
func (c *Client) PullWithCaps(kinds, caps []string) (*Task, error) {
	var out struct {
		Task *Task `json:"task"`
	}
	code, err := c.post("/cluster/pull", map[string]any{"worker": c.worker, "kinds": kinds, "caps": caps}, &out)
	if err != nil {
		return nil, err
	}
	if code == 204 {
		return nil, nil
	}
	return out.Task, nil
}

// Complete 回报成功。
func (c *Client) Complete(id string, result json.RawMessage) error {
	_, err := c.post("/cluster/complete", map[string]any{"id": id, "worker": c.worker, "result": result}, nil)
	return err
}

// Fail 回报失败。
func (c *Client) Fail(id, errMsg string) error {
	_, err := c.post("/cluster/fail", map[string]any{"id": id, "worker": c.worker, "err": errMsg}, nil)
	return err
}
