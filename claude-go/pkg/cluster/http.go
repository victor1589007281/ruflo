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
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
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

// writeJSON 以 JSON 形式写响应: 先设 Content-Type 再写状态码。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// enqueue POST /cluster/enqueue: 解码 Task 入队并返回任务 id。
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

// pull POST /cluster/pull: worker 按 kinds/caps 拉取任务; 无任务返回 204 + task:null。
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

// complete POST /cluster/complete: worker 回报成功结果。
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

// fail POST /cluster/fail: worker 回报失败原因。
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

// extend POST /cluster/extend: worker 续租, 避免长任务在租约到期后被重新派发。
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

// heartbeat POST /cluster/heartbeat: worker 注册/续约并上报能力标签与池快照。
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

// workers GET /cluster/workers: 列出存活 worker (观测用)。
func (h *httpHandler) workers(w http.ResponseWriter, r *http.Request) {
	alive, err := h.reg.Alive()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"workers": alive, "count": len(alive)})
}

// tasks GET /cluster/tasks: 列出任务 (观测用)。
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
	token  string
	http   *http.Client
}

// NewClient control 为控制面基址 (如 http://claude-go-control:18080)。
func NewClient(base, worker string) *Client {
	return &Client{base: base, worker: worker, http: &http.Client{Timeout: 30 * time.Second}}
}

// WithToken 注入 Bearer token。
//
// 为什么必须有: httpauth.DefaultProtectPrefixes 含 `/cluster/`, 所以配了
// wiki.apiSecret 的部署里, 不带 token 的 worker **每次拉取都是 401**。改造前
// post 又不检查状态码(见下), 于是这个 401 表现为"一直拉不到活"而不是报错 ——
// 最难归因的一类故障。
func (c *Client) WithToken(token string) *Client {
	c.token = token
	return c
}

// post 向控制面发一次 JSON POST; 非 2xx 一律视为错误并带上响应片段。
//
// 算法解读：
//
//	原理：worker 侧唯一的出站通道。把"HTTP 层成功"与"控制面接受了这次上报"严格分开,
//	      后者才是调用方关心的。
//	关键不变量：非 2xx 必须返回 error。上报终态被拒却当成功, 任务会永远停在 leased 直到
//	      租约过期, 且没有任何日志。
//	代价与边界：2xx 全算成功 —— 204 是 /cluster/pull 的合法"无任务"应答; 响应体解析失败
//	      被忽略, 因为此时状态码已经给出了结论。
//	设计权衡：返回 (code, error) 而不是只返回 error, 是因为 pull 要用 204 与 200 区分
//	      "无任务"和"有任务", 这个区别塞不进 error 里。
func (c *Client) post(path string, body, out any) (int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", c.base+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// **状态码必须判**。改造前这里拿到 code 却只是返回它, 而 Heartbeat/Complete/Fail
	// 三个调用方都写成 `_, err := c.post(...)` —— 于是 401(没配 token)、400("任务不在
	// 你的租约内")在 worker 侧全是**静默成功**。fail-open 用在这条路上代价极大:
	// 上报终态被拒却当成功, 任务就永远停在 leased 直到租约过期。
	//
	// 2xx 全算成功: 204 是 /cluster/pull 的合法"无任务"应答(见 PullWithCaps)。
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("cluster: POST %s 返回 %d: %s",
			path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// Heartbeat 注册/续约。pool 摘要可选 (nil = 未观测到池)。
func (c *Client) Heartbeat(caps, kinds []string, pool *tool.PoolSnapshot) error {
	_, err := c.post("/cluster/heartbeat", WorkerInfo{Name: c.worker, Caps: caps, Kinds: kinds, Pool: pool}, nil)
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
