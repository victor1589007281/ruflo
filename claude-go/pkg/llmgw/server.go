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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

// Route 一个 provider 的路由项。
type Route struct {
	Provider string // provider 名 (kimi/deepseek/...)
	BaseURL  string // 形如 https://api.kimi.com/coding/v1 (末尾无 /messages)
	APIKey   string
	Models   []string // 该 provider 可服务的模型名 (裸名, 不含 "provider:" 前缀)
	// Proxy 出站 HTTP 代理 URL。模型级配置: 只有 Proxy 非空的 route 才走代理
	// (如 muse-spark 大陆不可达, 须经东京 tailnet 代理出口), 直连组 Proxy 为空。
	// main.go 构建路由时同一 provider 按 Proxy 分组, 保证"没配置代理的模型绝不走代理"。
	Proxy string
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
	NodeID         string `json:"node_id,omitempty"`         // X-CG-Node-ID
	TurnID         string `json:"turn_id,omitempty"`         // X-CG-Turn-ID
	CallID         string `json:"call_id,omitempty"`         // X-CG-Call-ID
	Err            string `json:"err,omitempty"`
}

// Server 网关服务。
type Server struct {
	routes       []Route
	defaultRoute *Route
	client       *http.Client
	// proxyClients 按 (Provider+"\x00"+Proxy) 索引的代理客户端。
	// 只对配置了 proxy 的 route 构建, 未配置的 route 始终用 s.client 直连。
	proxyClients map[string]*http.Client
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
		proxyClients: map[string]*http.Client{},
	}
	for i := range routes {
		if routes[i].Provider == defaultProvider {
			s.defaultRoute = &routes[i]
		}
		if routes[i].Proxy == "" {
			continue
		}
		// 带代理的 route: 独立 http.Client, 注入 http.ProxyURL transport。
		// 只有这些 route 的出站请求经代理转发, 不污染直连组。
		if u, err := url.Parse(routes[i].Proxy); err == nil && u.Host != "" {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = http.ProxyURL(u)
			s.proxyClients[routes[i].Provider+"\x00"+routes[i].Proxy] = &http.Client{Transport: tr}
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
	// OpenAI 协议 (opencode 建议 mimo-v2.5 走 /v1/chat/completions, 见 pkg/api/openai.go)。
	// 与 /messages 一样按模型路由, 上游路径固定拼 /chat/completions (避免 baseURL 自带
	// /v1 时 double-v1)。
	mux.HandleFunc("/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	// OpenAI Responses API (opencode 对 muse-spark 等 Meta 模型推荐 /v1/responses,
	// 见 pkg/api/responses.go)。同样按模型路由, 上游路径拼 /responses。
	mux.HandleFunc("/responses", s.handleResponses)
	mux.HandleFunc("/v1/responses", s.handleResponses)
	return mux
}

// handleChatCompletions 与 handleMessages 同构, 转发到 baseURL + "/chat/completions"。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleMessagesWithPath(w, r, "/chat/completions")
}

// handleResponses 转发到 baseURL + "/responses" (OpenAI Responses API)。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handleMessagesWithPath(w, r, "/responses")
}

// route 按模型名选路由。同一 provider 可能拆成多个 route (按 proxy 分组),
// 因此必须先按 (provider + 裸模型名) 精确命中, 再回退 provider 前缀匹配。
// 这样 "opencode:muse-spark-1.2-contributor" 精确落到带 proxy 的 route,
// 不会因非 proxy 的 opencode route 在前而被误选。
func (s *Server) route(model string) *Route {
	bare, provider := model, ""
	if i := strings.IndexByte(model, ':'); i > 0 {
		provider, bare = model[:i], model[i+1:]
	}
	for j := range s.routes {
		for _, m := range s.routes[j].Models {
			if m == bare && (provider == "" || s.routes[j].Provider == provider) {
				return &s.routes[j]
			}
		}
	}
	// provider 前缀兜底 (裸模型名不在任何 Models 表时)
	if provider != "" {
		for j := range s.routes {
			if s.routes[j].Provider == provider {
				return &s.routes[j]
			}
		}
	}
	return s.defaultRoute
}

// clientFor 返回该 route 的出站 http.Client: 配置了 proxy 的 route 用代理客户端,
// 否则用共享直连客户端 (不消耗代理出口流量)。
func (s *Server) clientFor(rt *Route) *http.Client {
	if rt == nil || rt.Proxy == "" {
		return s.client
	}
	if c, ok := s.proxyClients[rt.Provider+"\x00"+rt.Proxy]; ok {
		return c
	}
	return s.client
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleMessagesWithPath(w, r, "/messages")
}

// handleMessagesWithPath 把请求转发到 rt.BaseURL + upstreamPath (原样透传 body)。
func (s *Server) handleMessagesWithPath(w http.ResponseWriter, r *http.Request, upstreamPath string) {
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
		Stream: probe.Stream,
		// trace 四元组: 头名用 pkg/api 的常量, 与出站侧 (api.setTraceHeaders) 同一份定义。
		RunID:  r.Header.Get(api.TraceHeaderRunID),
		NodeID: r.Header.Get(api.TraceHeaderNodeID),
		TurnID: r.Header.Get(api.TraceHeaderTurnID),
		CallID: r.Header.Get(api.TraceHeaderCallID),
	}

	// 模型名若带 "provider:" 前缀, 出站前剥掉 (上游只认裸模型名)。
	// 注意只剥 provider 前缀: 冒号后段本身可含冒号 —— ollama 等本地模型的
	// 裸名就是 "家族:tag" 形态 (如 "gemma4:26b-a4b-it-qat"), 此时冒号是
	// 模型名的一部分而非 provider 分隔符。若无条件按第一个冒号剥壳,
	// 会把 "gemma4:" 误剥成 "26b-a4b-it-qat", 上游返回 404。
	// 判定: 第一个冒号前段必须 == 已路由的 provider 名, 才是前缀形态。
	outBody := body
	if i := strings.IndexByte(probe.Model, ':'); i > 0 && probe.Model[:i] == rt.Provider {
		bare := probe.Model[i+1:]
		outBody = bytes.Replace(body,
			[]byte(`"model":"`+probe.Model+`"`),
			[]byte(`"model":"`+bare+`"`), 1)
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimRight(rt.BaseURL, "/")+upstreamPath, bytes.NewReader(outBody))
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

	resp, err := s.clientFor(rt).Do(req)
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
		// 非流式: 直接解析 usage (兼容 Anthropic input_tokens/output_tokens 与
		// OpenAI prompt_tokens/completion_tokens 两套字段名)
		var ur struct {
			Usage struct {
				InputTokens     int `json:"input_tokens"`
				OutputTokens    int `json:"output_tokens"`
				PromptTokens    int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBody, &ur) == nil {
			if ur.Usage.InputTokens > 0 {
				rec.InputTokens = ur.Usage.InputTokens
			}
			if ur.Usage.OutputTokens > 0 {
				rec.OutputTokens = ur.Usage.OutputTokens
			}
			if ur.Usage.PromptTokens > 0 {
				rec.InputTokens = ur.Usage.PromptTokens
			}
			if ur.Usage.CompletionTokens > 0 {
				rec.OutputTokens = ur.Usage.CompletionTokens
			}
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
			// OpenAI 协议流式末帧 (stream_options.include_usage) 无 event.type,
			// 直接带 usage.prompt_tokens / completion_tokens。
			var ou struct {
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &ou) == nil && ou.Usage != nil {
				if ou.Usage.PromptTokens > 0 {
					rec.InputTokens = ou.Usage.PromptTokens
				}
				if ou.Usage.CompletionTokens > 0 {
					rec.OutputTokens = ou.Usage.CompletionTokens
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
