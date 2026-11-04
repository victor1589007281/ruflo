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

	// 网关层重试与退避 (模型级可配置; 语义同 api.RetryPolicyFromConfig):
	// RetryCount 0 = 网关默认 2 次, -1 = 显式禁用, >0 = 该次数;
	// RetryBaseSec/RetryMaxSec 0 = 1s/8s; RetryStatuses 空 = 默认集合 (含 400)。
	//
	// 为什么放在 Route 而不是只留 Server 级 SetRetryPolicy: 部署里网关读的是
	// 配置文件, 而 SetRetryPolicy 只有代码调用点 —— 放进 Route 才能让
	// retryCount/retryStatuses 这类字段真正生效。同一 provider 下重试配置不同的
	// 模型会在 buildRoutes 里拆成多条 route (与 Proxy 分组同一套做法)。
	RetryCount    int
	RetryBaseSec  int
	RetryMaxSec   int
	RetryStatuses []int
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
	// Attempts 是本次请求实际发往上游的次数 (含首次)。>1 表示发生过重试;
	// 与 Status 合起来才能区分"上游一次就 500"和"重试 2 次后仍 500"。
	Attempts int    `json:"attempts,omitempty"`
	RunID    string `json:"run_id,omitempty"`  // X-CG-Run-ID 透传 (trace 四元组)
	NodeID   string `json:"node_id,omitempty"` // X-CG-Node-ID
	TurnID   string `json:"turn_id,omitempty"` // X-CG-Turn-ID
	CallID   string `json:"call_id,omitempty"` // X-CG-Call-ID
	Err      string `json:"err,omitempty"`
}

// Server 网关服务。
type Server struct {
	mu           sync.RWMutex // 保护 routes/defaultRoute —— 热加载会原地替换 (见 SetRoutes)
	routes       []Route
	defaultRoute *Route
	client       *http.Client
	// proxyClients 按 (Provider+"\x00"+Proxy) 索引的代理客户端。
	// 只对配置了 proxy 的 route 构建, 未配置的 route 始终用 s.client 直连。
	proxyClients map[string]*http.Client
	logMu        sync.Mutex
	logPath      string

	// retryPolicy 控制写响应前的上游重试 (受 s.mu 保护, 热加载可通过
	// SetRetryPolicy 替换)。默认 DefaultGatewayRetryPolicy: 含 400, 重试 2 次。
	retryPolicy api.RetryPolicy
}

// NewServer 构建网关。defaultProvider 为空时取第一个 route。
func NewServer(routes []Route, defaultProvider, stateDir string) (*Server, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("llm-gateway: 至少需要一个 provider 路由")
	}
	s := &Server{
		routes: routes,
		// 默认重试策略: 写响应前的 4xx/5xx 重试 (含 400), 额外 2 次。
		// 旧调用点不传策略也自动获得与 anthropic-gateway 一致的行为。
		retryPolicy: api.DefaultGatewayRetryPolicy(),
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

// SetRetryPolicy 替换网关的上游重试策略 (热加载/测试用)。
//
// 不放进 NewServer 的签名: 旧调用点遍布部署与测试, 加参数会逼着所有调用方
// 改一遍, 而默认策略已经覆盖绝大多数场景。
func (s *Server) SetRetryPolicy(p api.RetryPolicy) {
	s.mu.Lock()
	s.retryPolicy = p
	s.mu.Unlock()
}

// currentRetryPolicy 取一份策略快照 (可能是热加载替换后的值)。
func (s *Server) currentRetryPolicy() api.RetryPolicy {
	s.mu.RLock()
	p := s.retryPolicy
	s.mu.RUnlock()
	return p
}

// retryPolicyFor 返回本次请求实际使用的策略。
//
// 优先级: route 显式配置 > Server 级策略 (SetRetryPolicy / 默认)。
// route 上一个字段都没配时原样返回 Server 策略 —— 这样 SetRetryPolicy 的
// 全局关闭 (MaxRetries=0) 不会被 route 的零值悄悄改回默认 2 次。
func (s *Server) retryPolicyFor(rt *Route) api.RetryPolicy {
	if rt != nil && (rt.RetryCount != 0 || rt.RetryBaseSec != 0 ||
		rt.RetryMaxSec != 0 || len(rt.RetryStatuses) != 0) {
		return api.RetryPolicyFromConfig(rt.RetryCount, rt.RetryBaseSec, rt.RetryMaxSec, rt.RetryStatuses)
	}
	return s.currentRetryPolicy()
}

// Handler 返回 http.Handler (便于测试与挂载)。
// SetRoutes 原地替换路由表 (配置热加载用)。
//
// 与 NewServer 的区别只在"不动已建好的 http.Client": 直连池与已有代理池复用,
// 不为一次热加载丢掉连接池; 只有新出现的 proxy 才补建 client。
// 加 RWMutex 是因为 handleMessages 在读 routes 的同时可能有热加载在写。
func (s *Server) SetRoutes(routes []Route, defaultProvider string) error {
	if len(routes) == 0 {
		return fmt.Errorf("llm-gateway: 至少需要一个 provider 路由")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = routes
	s.defaultRoute = nil
	for i := range routes {
		if routes[i].Provider == defaultProvider {
			s.defaultRoute = &routes[i]
		}
		if routes[i].Proxy == "" {
			continue
		}
		key := routes[i].Provider + "\x00" + routes[i].Proxy
		if _, ok := s.proxyClients[key]; ok {
			continue
		}
		if u, err := url.Parse(routes[i].Proxy); err == nil && u.Host != "" {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = http.ProxyURL(u)
			s.proxyClients[key] = &http.Client{Transport: tr}
		}
	}
	if s.defaultRoute == nil {
		s.defaultRoute = &s.routes[0]
	}
	return nil
}

// Handler 构造网关的 HTTP 路由: /healthz + 三套协议 (Anthropic /messages、OpenAI
// /chat/completions 与 /responses), 每套都同时挂带与不带 /v1 前缀的两个路径。
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
	// 取一次快照再查 —— 热加载可能在任意时刻替换 routes
	s.mu.RLock()
	routes, def := s.routes, s.defaultRoute
	s.mu.RUnlock()

	bare, provider := model, ""
	if i := strings.IndexByte(model, ':'); i > 0 {
		provider, bare = model[:i], model[i+1:]
	}
	for j := range routes {
		for _, m := range routes[j].Models {
			if m == bare && (provider == "" || routes[j].Provider == provider) {
				return &routes[j]
			}
		}
	}
	// provider 前缀兜底 (裸模型名不在任何 Models 表时)
	if provider != "" {
		for j := range routes {
			if routes[j].Provider == provider {
				return &routes[j]
			}
		}
	}
	return def
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

// stripProviderPrefix 把请求体里 model 字段的 "provider:" 前缀剥掉, 其余字节不动。
//
// # 为什么不能只用 bytes.Replace
//
// 常见的快路径是按 `"model":"<alias>"` 做一次字节替换 —— 它要求发布方**不带空格**。
// 但 JSON 的冒号两侧允许任意空白, 而不同客户端各写各的: Python 的 json.dumps 默认
// 产出 `"model": "opencode:deepseek-v4.1-flash"` (冒号后一个空格)。快路径匹配不上,
// 替换**静默不发生**, 于是整个别名被原样发给上游 —— 上游回 "Model is unavailable",
// 一个看起来像模型下线、实际是本地字符串没替换掉的错误。
//
// 这个缺陷在本轮 K8s 集成测试里真实撞到过: 同一份请求体, 紧凑写法 200, 带空格 400。
// 它不说话、不报错, 只是让上游拒掉一半的客户端。
//
// 所以与 cmd/anthropic-gateway.rewriteModelField 采用同一套两级策略: 快路径保原字节
// (不重排字段顺序、不影响提示缓存的字节前缀), 匹配失败时回退到解析重建。
func stripProviderPrefix(body []byte, alias, bare string) []byte {
	// 快路径: 紧凑写法, 保住原始字节顺序 (cache_control 的字节前缀会因重排而失效)。
	if out := bytes.Replace(body,
		[]byte(`"model":"`+alias+`"`),
		[]byte(`"model":"`+bare+`"`), 1); !bytes.Equal(out, body) {
		return out
	}
	// 回退: 解析重建。map 会重排键序, 但"上游收到裸模型名"优先于"字节顺序不变" ——
	// 前者错是 400, 后者错只是少命中一次缓存。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		// 解析失败就原样转发: 让上游给出它自己的诊断, 好过在这里造一个假错误。
		return body
	}
	quoted, err := json.Marshal(bare)
	if err != nil {
		return body
	}
	raw["model"] = json.RawMessage(quoted)
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// handleMessages 处理 Anthropic Messages 协议 (/messages 与 /v1/messages)。
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
		outBody = stripProviderPrefix(body, probe.Model, probe.Model[i+1:])
	}

	upstreamURL := strings.TrimRight(rt.BaseURL, "/") + upstreamPath
	// build 每次重建 request + body reader (http.Request.Body 是一次性的)。
	// 重试发生在写任何响应头/字节之前 —— 2xx 流一旦开始回传就不再重试。
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
			upstreamURL, bytes.NewReader(outBody))
		if err != nil {
			return nil, err
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
		// opencode zen/go 上游强制要求 x-opencode-session, 缺失直接 400 MissingSessionID
		// ("cannot be routed efficiently")。
		//
		// 这一行是本轮 K8s 集成测试卡住的**根因**: 部署里 claude-go-gateway 跑的是
		// `llm-gateway` 子命令 (即本包), 而会话头注入原先只写在 cmd/anthropic-gateway。
		// 真实链路是 控制面/worker → 本网关 → opencode, 本网关是唯一出站跳, 它不补头
		// 就没人补 —— 每一次 LLM 调用都 400, 与模型、凭据、推理参数全都无关。
		//
		// 同样的错误在 anthropic-gateway 侧也有一份, 所以判定逻辑收进 pkg/api 共用
		// (见 api.IsOpenCodeUpstream / api.OpencodeSessionID), 不在此处另写一套。
		if api.IsOpenCodeUpstream(rt.Provider, rt.BaseURL) {
			req.Header.Set(api.OpencodeSessionHeader, api.OpencodeSessionID(r, rt.Provider))
		}
		return req, nil
	}

	resp, attempts, err := api.DoWithRetry(r.Context(), s.clientFor(rt), s.retryPolicyFor(rt), build)
	rec.Attempts = attempts
	if err != nil {
		// 客户端已取消: 只落访问日志, 不再写第二次响应。
		if r.Context().Err() != nil {
			rec.Status = 499
			rec.Err = "canceled: " + err.Error()
			rec.DurMS = time.Since(start).Milliseconds()
			s.writeLog(&rec)
			return
		}
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
				InputTokens      int `json:"input_tokens"`
				OutputTokens     int `json:"output_tokens"`
				PromptTokens     int `json:"prompt_tokens"`
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

// fail 以网关自身的错误信封回应 (Anthropic 风格 {error:{type,message}}), 并先落一条访问日志。
//
// 落日志在写响应之前: 客户端拿到 5xx 时日志里必须已经有这条记录, 否则"请求挂了但
// 日志里查不到"就成了排查时的第一道墙。
func (s *Server) fail(w http.ResponseWriter, rec *AccessRecord, status int, msg string) {
	rec.Status = status
	rec.Err = msg
	rec.DurMS = time.Since(time.UnixMilli(rec.TS)).Milliseconds()
	s.writeLog(rec)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "gateway_error", "message": msg}})
}

// writeLog 把一条访问记录以 JSONL 追加到日志文件; 未配置路径时静默跳过。
//
// 每条记录独立 open/append/close (不持有长驻文件句柄), 用 logMu 串行化 —— 换来的是
// 日志文件可被外部工具随时轮转/删除, 代价是每次调用多两次系统调用。
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
