package llmgw

// LLM Gateway 独立进程模式 (design/02 §3.1 R1): 反向代理式网关。
//
// 设计取舍: 不重组装请求/响应, 而是**原文透传** Anthropic Messages 协议
// (含 SSE 流) —— 协议保真零损耗, 网关只做四件事:
//  1. 按 model 路由到 provider (换 BaseURL + 注入该 provider 的鉴权头)
//  2. token 双边记账: 非流式解析 usage; 流式 tee 解析 message_start/message_delta;
//     input 缺失时按请求字节估算并打 estimated 标记
//     (修复 design/02 §1.4 "token 记账只计输出": Kimi 类网关 SSE 常不回 input_tokens)
//  3. 访问日志 access.jsonl (含 trace 头透传)
//  4. /healthz
//
// 多副本/多平台共享同一网关即共享配额观测 (集中化收益, design/02 §3.1)。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Route 一个 provider 的路由项。
type Route struct {
	Provider string // provider 名 (kimi/deepseek/...)
	BaseURL  string // 形如 https://api.kimi.com/coding/v1 (末尾无 /messages)
	APIKey   string
	Models   []string // 该 provider 可服务的模型名 (裸名, 不含 "provider:" 前缀)
}

// AccessRecord 网关访问日志行 (access.jsonl)。
type AccessRecord struct {
	TS             int64  `json:"ts"`
	Model          string `json:"model"`
	Provider       string `json:"provider"`
	Status         int    `json:"status"`
	DurMS          int64  `json:"dur_ms"`
	Stream         bool   `json:"stream"`
	InputTokens    int    `json:"input_tokens"`
	OutputTokens   int    `json:"output_tokens"`
	InputEstimated bool   `json:"input_estimated,omitempty"` // input 来自请求字节估算 (len/4)
	RunID          string `json:"run_id,omitempty"`          // X-CG-Run-ID 透传 (trace 四元组)
	Err            string `json:"err,omitempty"`
}

// Server 网关服务。
type Server struct {
	routes       []Route
	defaultRoute *Route
	client       *http.Client
	logMu        sync.Mutex
	logPath      string
}

// NewServer 构建网关。defaultProvider 为空时取第一个 route。
func NewServer(routes []Route, defaultProvider, stateDir string) (*Server, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("llm-gateway: 至少需要一个 provider 路由")
	}
	s := &Server{
		routes: routes,
		// 流式长连接: 不设整体超时, 只设握手/空闲级超时
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost:   16,
			ResponseHeaderTimeout: 120 * time.Second,
		}},
	}
	for i := range routes {
		if routes[i].Provider == defaultProvider {
			s.defaultRoute = &routes[i]
		}
	}
	if s.defaultRoute == nil {
		s.defaultRoute = &s.routes[0]
	}
	if stateDir != "" {
		_ = os.MkdirAll(stateDir, 0o755)
		s.logPath = filepath.Join(stateDir, "access.jsonl")
	}
	return s, nil
}

// Handler 返回 http.Handler (便于测试与挂载)。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"providers":%d}`, len(s.routes))
	})
	mux.HandleFunc("/messages", s.handleMessages)
	mux.HandleFunc("/v1/messages", s.handleMessages)
	return mux
}

// route 按模型名选 provider: 精确命中该 provider 的模型表, 否则 default。
func (s *Server) route(model string) *Route {
	// "provider:model" 别名形态直接取 provider 段
	if i := strings.IndexByte(model, ':'); i > 0 {
		name := model[:i]
		for j := range s.routes {
			if s.routes[j].Provider == name {
				return &s.routes[j]
			}
		}
	}
	for j := range s.routes {
		for _, m := range s.routes[j].Models {
			if m == model {
				return &s.routes[j]
			}
		}
	}
	return s.defaultRoute
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, `{"error":"read body failed"}`, http.StatusBadRequest)
		return
	}
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	rt := s.route(probe.Model)

	rec := AccessRecord{
		TS: start.UnixMilli(), Model: probe.Model, Provider: rt.Provider,
		Stream: probe.Stream, RunID: r.Header.Get("X-CG-Run-ID"),
	}

	// 模型名若带 "provider:" 前缀, 出站前剥掉 (上游只认裸模型名)
	outBody := body
	if i := strings.IndexByte(probe.Model, ':'); i > 0 {
		bare := probe.Model[i+1:]
		outBody = bytes.Replace(body,
			[]byte(`"model":"`+probe.Model+`"`),
			[]byte(`"model":"`+bare+`"`), 1)
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimRight(rt.BaseURL, "/")+"/messages", bytes.NewReader(outBody))
	if err != nil {
		s.fail(w, &rec, http.StatusBadGateway, "build request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", rt.APIKey)
	req.Header.Set("Authorization", "Bearer "+rt.APIKey)
	if v := r.Header.Get("anthropic-version"); v != "" {
		req.Header.Set("anthropic-version", v)
	} else {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if probe.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		s.fail(w, &rec, http.StatusBadGateway, "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()
	rec.Status = resp.StatusCode

	// 响应头透传
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if probe.Stream && resp.StatusCode == http.StatusOK {
		s.pipeStream(w, resp.Body, &rec)
	} else {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_, _ = w.Write(respBody)
		// 非流式: 直接解析 usage
		var ur struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBody, &ur) == nil {
			rec.InputTokens, rec.OutputTokens = ur.Usage.InputTokens, ur.Usage.OutputTokens
		}
	}

	// token 双边兜底: input 缺失按请求字节 /4 估算 (chars≈bytes 对英文, CJK 偏保守)
	if rec.InputTokens == 0 {
		rec.InputTokens = len(body) / 4
		rec.InputEstimated = true
	}
	rec.DurMS = time.Since(start).Milliseconds()
	s.writeLog(&rec)
}

// pipeStream 边转发边 tee 解析 SSE usage。逐行 flush 保证流式体验。
func (s *Server) pipeStream(w http.ResponseWriter, body io.Reader, rec *AccessRecord) {
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil && len(line) == 0 { // SSE 事件以空行结束, 按事件粒度 flush
			flusher.Flush()
		}
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			var ev struct {
				Type    string `json:"type"`
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
					InputTokens  int `json:"input_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &ev) == nil {
				switch ev.Type {
				case "message_start":
					if ev.Message.Usage.InputTokens > 0 {
						rec.InputTokens = ev.Message.Usage.InputTokens
					}
				case "message_delta":
					if ev.Usage.OutputTokens > 0 {
						rec.OutputTokens = ev.Usage.OutputTokens
					}
					if ev.Usage.InputTokens > 0 { // 部分网关在末帧才给 input
						rec.InputTokens = ev.Usage.InputTokens
					}
				}
			}
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) fail(w http.ResponseWriter, rec *AccessRecord, status int, msg string) {
	rec.Status = status
	rec.Err = msg
	rec.DurMS = time.Since(time.UnixMilli(rec.TS)).Milliseconds()
	s.writeLog(rec)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "gateway_error", "message": msg}})
}

func (s *Server) writeLog(rec *AccessRecord) {
	if s.logPath == "" {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("[llm-gateway] 访问日志写入失败: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
