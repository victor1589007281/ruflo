// anthropic-gateway 本地 Anthropic 协议翻译网关。
//
// 让 claude (Claude Code CLI, 只讲 Anthropic /v1/messages) 也能用 opencode 上
// 的非 anthropic 协议模型:
//   - glm 系 (protocol=openai)        → 翻译成 OpenAI /chat/completions
//   - muse-spark (protocol=openai-responses) → 翻译成 Responses /v1/responses, 走东京 tinyproxy
//   - anthropic 系模型 (protocol 缺省) → 原样透传 /v1/messages (保真, 不重编码)
//
// 路由完全复用 pkg/api 的翻译器 (openai.go / responses.go) 与模型级 Proxy 隔离,
// 与 claude-go CLI 走同一份 config.json, 模型可用性、协议、代理语义完全一致。
//
// 用法:
//
//	anthropic-gateway --config /mnt/data/claude-go/config/config.json --addr 127.0.0.1:18989
//	ANTHROPIC_BASE_URL=http://127.0.0.1:18989 ANTHROPIC_API_KEY=<任意非空> claude --model opencode:glm-5.3-flash ...
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/hotreload"
	"github.com/anthropic/claude-go/pkg/types"
)

// resolveWatchPath 把 --config 入参解析成"确定存在"的文件路径, 供热加载监视器用。
// 入参留空时走 feishu 的自动发现 (含 /etc/claude-go/config.json, 正是 k8s ConfigMap
// 挂载点) —— 监视器拿不到真实路径等于没启用。
func resolveWatchPath(p string) string {
	if p != "" {
		return p
	}
	if r, err := feishu.ResolveJSONConfigPath(""); err == nil {
		return r
	}
	return ""
}

// upstreamTimeout 翻译路径单次上游调用上限。omen-alpha 等隐藏推理模型要先烧
// 数万 reasoning token 才吐正文, 单次可达 10+ 分钟; 原默认 10min/5min 的硬超时
// 会误掐 (502 context deadline exceeded)。per-model max_tokens 仍自限生成长度。
var upstreamTimeout = flag.Duration("upstream-timeout", 30*time.Minute, "翻译路径单次上游调用上限 (omen 长推理需 >10min)")

func main() {
	var (
		addr          = flag.String("addr", "127.0.0.1:18989", "监听地址")
		configPath    = flag.String("config", "", "config.json 路径 (留空走 feishu 自动发现)")
		defaultPrefix = flag.String("default-prefix", "opencode", "model 无 provider 前缀时的默认 provider")
	)
	flag.Parse()

	jsonCfg, err := feishu.LoadJSONConfig(*configPath)
	if err != nil {
		log.Fatalf("加载 config 失败: %v", err)
	}
	if jsonCfg == nil || len(jsonCfg.Providers) == 0 {
		log.Fatalf("config 无 providers (path=%s)", *configPath)
	}
	cfgJSON := jsonCfg.ToModelConfigJSON()
	registry, resolver, err := modelconfig.LoadFromConfig(cfgJSON)
	if err != nil {
		log.Fatalf("构建 model 解析器失败: %v", err)
	}

	// 默认 provider 注册表兜底: "glm-5.3-flash" 无前缀 → 先试 "opencode:glm-5.3-flash"。
	tryPrefix := *defaultPrefix

	gw := &gateway{registry: registry, resolver: resolver, tryPrefix: tryPrefix}

	// 配置热加载: providers / models / ai 改了立即生效, 不必重启网关。
	// 网关的 api.Client 本来就是每请求按 rc 重建的 (buildClient), 所以注册表与解析器
	// 一换, apiKey / baseUrl / proxy / protocol / 模型清单全部跟着换 —— 这是几个服务里
	// 最容易做成真热加载的一个, 不需要动任何并发结构 (Reload 自带 RWMutex)。
	hotreload.WatchConfig(resolveWatchPath(*configPath), 5*time.Second, "anthropic-gateway",
		func(path string) error {
			// 与启动期同一条路径: 原始 config.json 要先经 feishu 转换才能喂给
			// modelconfig —— 直接 ReloadFromJSON 形状对不上 (ai.plans.roles)。
			jc, err := feishu.LoadJSONConfig(path)
			if err != nil {
				return fmt.Errorf("加载配置失败: %w", err)
			}
			if jc == nil {
				return fmt.Errorf("配置为空")
			}
			return modelconfig.ReloadFromConfig(jc.ToModelConfigJSON(), registry, resolver)
		})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", gw.handleMessages)
	// 裸 /messages: claude-go 内部客户端 (api.Client endpointPath) 向 {网关}/messages
	// 发 Anthropic 协议请求 (CLAUDE_GO_LLM_GATEWAY 裸根地址 + "/messages")。部署里的
	// pathfix.py 虽会把 /messages 改写成 /v1/messages, 但直挂本进程 (无 pathfix) 的
	// 实例 (如宿主 18989) 之前会在 Go mux 上裸 404 "404 page not found", 这里原生
	// 注册双保险。
	mux.HandleFunc("/messages", gw.handleMessages)
	// OpenAI 协议透传: claude-go 内部客户端对 protocol=openai 模型 (omen-alpha 等)
	// 向 {网关}/chat/completions 发 OpenAI 格式请求体。此前未注册该路由, 全部裸
	// 404 (pathfix 只改写 /messages), 导致走本网关的团队作业 (interviewforge 增量
	// 出题等) 在"增量出题"阶段反复 404、重试 6 次后失败。
	mux.HandleFunc("/v1/chat/completions", gw.handleChatCompletions)
	mux.HandleFunc("/chat/completions", gw.handleChatCompletions)
	mux.HandleFunc("/v1/responses", gw.handleResponses)
	mux.HandleFunc("/responses", gw.handleResponses)
	mux.HandleFunc("/v1/models", gw.handleModels)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	srv := &http.Server{Addr: *addr, Handler: mux}
	log.Printf("[anthropic-gateway] 监听 %s (config=%s, default-prefix=%s)", *addr, orDefault(*configPath, "auto-discover"), tryPrefix)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("网关退出: %v", err)
	}
}

type gateway struct {
	registry  *modelconfig.ProviderRegistry
	resolver  *modelconfig.ConfigResolver
	tryPrefix string
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// resolveModel 解析 model 字符串为 ResolvedConfig。
// 规则: 先按完整别名 (provider:model) 精确查找; 未命中且无冒号时, 用 default-prefix 拼接重试。
func (g *gateway) resolveModel(model string) (modelconfig.ResolvedConfig, error) {
	if model == "" {
		return modelconfig.ResolvedConfig{}, fmt.Errorf("model 为空")
	}
	rc := g.resolver.ResolveAlias(model)
	if rc.BaseURL != "" && rc.APIKey != "" && rc.ProviderName != "" {
		return rc, nil
	}
	if !strings.Contains(model, ":") {
		// 裸模型名: claude-go 客户端走 ProviderName(后半段) 路径时只发裸名。
		// 先全表反查 alias (如 "k3" -> "kimi:k3"), 再退 default-prefix;
		// 否则非默认前缀 provider (kimi 等) 的模型会被 404/误路由到默认上游。
		if alias := g.registry.LookupAliasByModelName(model); alias != model {
			if rc3 := g.resolver.ResolveAlias(alias); rc3.BaseURL != "" && rc3.APIKey != "" && rc3.ProviderName != "" {
				return rc3, nil
			}
		}
		if g.tryPrefix != "" {
			rc2 := g.resolver.ResolveAlias(g.tryPrefix + ":" + model)
			if rc2.BaseURL != "" && rc2.APIKey != "" && rc2.ProviderName != "" {
				return rc2, nil
			}
		}
	}
	return modelconfig.ResolvedConfig{}, fmt.Errorf("model %q 在 providers 中未找到 (baseURL/apiKey/providerName 不全)", model)
}

// buildClient 按 resolved 配置构造 api.Client (协议 + 模型级代理)。
// 与 cmd/claude-go/main.go 的 client 构造逻辑保持一致。
func buildClient(rc modelconfig.ResolvedConfig) *api.Client {
	baseURL := strings.TrimRight(rc.BaseURL, "/")
	if strings.HasSuffix(baseURL, "/anthropic") || strings.HasSuffix(baseURL, "/compatible-mode") {
		baseURL += "/v1"
	}
	var c *api.Client
	if api.IsLocalEndpoint(baseURL) {
		c = api.NewOllamaClient(baseURL, rc.ProviderName)
		c.APIKey = rc.APIKey
	} else {
		c = api.NewClient(baseURL, rc.APIKey, rc.ProviderName)
	}
	c.Protocol = rc.Protocol
	c.SetProxy(rc.Proxy)
	c.Tag = "anthropic-gateway"
	return c
}

func (g *gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error())
		return
	}
	var req types.APIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, 400, "invalid_request_error", "解析请求体失败: "+err.Error())
		return
	}

	rc, err := g.resolveModel(req.Model)
	if err != nil {
		writeError(w, 404, "model_not_found", err.Error())
		return
	}
	log.Printf("[anthropic-gateway] model=%s resolved=%s protocol=%s proxy=%s stream=%v messages=%d tools=%d",
		req.Model, rc.ProviderName, protoName(rc.Protocol), rc.Proxy, req.Stream, len(req.Messages), len(req.Tools))

	// anthropic 协议模型 → 原样透传 (保真, 避免重编码丢 metadata/tool_choice/stop_sequences 等)。
	if rc.Protocol == "" || rc.Protocol == "anthropic" {
		// 裸名请求 (客户端 ProviderName 路径) 须把 model 改写为限定别名再透传:
		// opencode/kimi 这类聚合上游按 "provider:model" 限定名路由, 裸名会被拒
		// ("Model k3 is not supported")。已是限定名时改写为同值, 无副作用。
		body = rewriteModelField(body, rc.Provider+":"+rc.ProviderName)
		g.proxyPassthrough(w, r, body, rc)
		return
	}

	client := buildClient(rc)
	// NewClient 默认把 http.Client.Timeout 设为 5min (300s), 会先于上面 ctx 上限
	// 误掐长推理 (omen 可达 7+ min 才首 token); 归一到 --upstream-timeout。
	// WithToolChoiceAny 只是 Client 值拷贝、共享同一个 *http.Client 指针, 此覆盖在克隆后仍生效。
	if *upstreamTimeout > 0 && client.Client != nil {
		client.Client.Timeout = *upstreamTimeout
	}
	if req.ToolChoice != nil && req.ToolChoice.Type == "any" {
		client = client.WithToolChoiceAny()
	}

	// opencode 上游 (zen/go) 要求每个请求带 x-opencode-session (会话级稳定 ID) 与
	// 自有 User-Agent, 缺失会被拒 400 MissingSessionID。Claude Code CLI 出站请求自带
	// X-Claude-Code-Session-Id (每对话稳定), 原样透传最贴合 opencode 的会话语义;
	// 该头缺失时兜底生成随机 ID (仍能消除 400, 代价仅是跨请求缓存/路由不共享)。
	if isOpenCodeUpstream(rc) {
		client.ExtraHeaders = http.Header{
			"x-opencode-session": {opencodeSessionID(r)},
			"User-Agent":         {"anthropic-gateway/" + gatewayVersion},
		}
	}

	systemPrompt := extractSystem(req.System)
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16384
	}

	// 翻译路径单次调用的硬上限用 --upstream-timeout (默认 30min)。omen-alpha 等
	// 隐藏推理模型先烧数万 reasoning token 才吐正文, 原来 10min 的 ctx 会误掐 (502)。
	ctx, cancel := context.WithTimeout(r.Context(), *upstreamTimeout)
	defer cancel()

	if req.Stream {
		g.streamMessages(ctx, w, client, req.Messages, systemPrompt, req.Tools, maxTokens)
		return
	}
	resp, err := client.SendMessage(ctx, req.Messages, systemPrompt, req.Tools, maxTokens)
	if err != nil {
		writeError(w, 502, "upstream_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// streamMessages 把 api.Client.StreamMessage 返回的 Anthropic StreamDelta 事件
// 以 SSE 原样转发给 claude CLI, 并补发 message_stop (翻译器只发到 message_delta)。
func (g *gateway) streamMessages(ctx context.Context, w http.ResponseWriter, client *api.Client, messages []types.APIMessage, systemPrompt []string, tools []types.APITool, maxTokens int) {
	eventCh, errCh := client.StreamMessage(ctx, messages, systemPrompt, tools, maxTokens)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	bw := bufio.NewWriter(w)
	seenStop := false
	writeEvent := func(delta types.StreamDelta) error {
		data, err := json.Marshal(delta)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(bw, "event: %s\ndata: %s\n\n", delta.Type, data); err != nil {
			return err
		}
		if delta.Type == "message_stop" {
			seenStop = true
		}
		bw.Flush()
		flusher.Flush()
		return nil
	}

	upstreamErr := ""
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() != nil {
				upstreamErr = ctx.Err().Error()
			}
		case err, ok := <-errCh:
			if ok && err != nil {
				upstreamErr = err.Error()
			}
		case delta, ok := <-eventCh:
			if !ok {
				// 事件流结束
				if upstreamErr != "" {
					// 流中途失败: 尽力给 claude 一个 message_stop 以便正常收尾
				}
				if !seenStop {
					fmt.Fprintf(bw, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					bw.Flush()
					flusher.Flush()
				}
				fmt.Fprintf(bw, "data: [DONE]\n\n")
				bw.Flush()
				flusher.Flush()
				return
			}
			if err := writeEvent(delta); err != nil {
				log.Printf("[anthropic-gateway] SSE 写入失败: %v", err)
				return
			}
		}
	}
}

// proxyPassthrough anthropic 协议模型原样转发上游 /v1/messages, 响应逐字节回传
// (流式/非流式都保持上游原样, 不重编码)。唯一改写: model 字段剥掉 provider 前缀
// (opencode 上游只认裸 ID, "opencode:minimax-m3" 会被拒 401 ModelError)。
func (g *gateway) proxyPassthrough(w http.ResponseWriter, r *http.Request, body []byte, rc modelconfig.ResolvedConfig) {
	baseURL := strings.TrimRight(rc.BaseURL, "/")
	upstream := baseURL + "/messages"

	forwardBody := rewriteModelField(body, rc.ProviderName)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream, strings.NewReader(string(forwardBody)))
	if err != nil {
		writeError(w, 502, "upstream_error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", rc.APIKey)
	req.Header.Set("Authorization", "Bearer "+rc.APIKey)
	req.Header.Set("User-Agent", "anthropic-gateway/"+gatewayVersion)
	if v := r.Header.Get("anthropic-version"); v != "" {
		req.Header.Set("anthropic-version", v)
	}
	// opencode zen/go 原生 /v1/messages 同样强制要求 x-opencode-session (缺失 → 400)。
	if isOpenCodeUpstream(rc) {
		req.Header.Set("x-opencode-session", opencodeSessionID(r))
	}

	// 模型级代理: 复用与翻译路径相同的代理语义 (http://host:port)。
	client := &http.Client{Timeout: 10 * time.Minute}
	if rc.Proxy != "" {
		if pu, perr := neturl.Parse(rc.Proxy); perr == nil && pu.Host != "" && (pu.Scheme == "http" || pu.Scheme == "https") {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = http.ProxyURL(pu)
			client.Transport = tr
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		writeError(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode >= 400 {
		io.Copy(w, resp.Body)
		return
	}
	// 流式回传: 逐块写 + flush, 保证 SSE 增量到达 claude CLI (整块 io.Copy 会让客户端等完整响应)。
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				log.Printf("[anthropic-gateway] passthrough 回传写入失败: %v", werr)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// handleChatCompletions OpenAI Chat Completions 透传入口: claude-go 内部客户端
// (api.Client protocol=openai, 如 omen-alpha) 向 {网关}/chat/completions 发的是
// OpenAI 格式请求体, 网关无需翻译, 按模型解析上游后原样转发即可。
func (g *gateway) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	g.proxyOpenAIFormat(w, r, "/chat/completions")
}

// handleResponses OpenAI Responses API 透传入口 (muse-spark 等 protocol=openai-responses 模型)。
func (g *gateway) handleResponses(w http.ResponseWriter, r *http.Request) {
	g.proxyOpenAIFormat(w, r, "/responses")
}

// proxyOpenAIFormat OpenAI 系协议原样透传。入站已是 OpenAI 格式请求体 (与 Claude CLI
// 的 Anthropic 格式入口不同), 网关职责只有: 解析模型 → 解析上游 → 剥 provider 前缀 →
// 注入鉴权/会话头 → 转发到 {base}{upstreamPath} → 原样回传 (流式逐块 flush)。
func (g *gateway) proxyOpenAIFormat(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error())
		return
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Model == "" {
		writeError(w, 400, "invalid_request_error", "请求体缺少 model 字段")
		return
	}
	rc, err := g.resolveModel(probe.Model)
	if err != nil {
		writeError(w, 404, "model_not_found", err.Error())
		return
	}
	log.Printf("[anthropic-gateway] openai-passthrough model=%s resolved=%s path=%s",
		probe.Model, rc.ProviderName, upstreamPath)

	upstream := strings.TrimRight(rc.BaseURL, "/") + upstreamPath
	forwardBody := rewriteModelField(body, rc.ProviderName)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream, bytes.NewReader(forwardBody))
	if err != nil {
		writeError(w, 502, "upstream_error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", rc.APIKey)
	req.Header.Set("Authorization", "Bearer "+rc.APIKey)
	req.Header.Set("User-Agent", "anthropic-gateway/"+gatewayVersion)
	// opencode zen/go 上游强制 x-opencode-session (缺失 → 400 MissingSessionID)。
	if isOpenCodeUpstream(rc) {
		req.Header.Set("x-opencode-session", opencodeSessionID(r))
	}
	// omen-alpha 等隐藏推理模型首 token 可达 7+ 分钟: 用 --upstream-timeout (默认 30min),
	// 不能用 proxyPassthrough 那样的 10min 短超时 (会中途 502 context deadline exceeded)。
	client := &http.Client{Timeout: *upstreamTimeout}
	if rc.Proxy != "" {
		if pu, perr := neturl.Parse(rc.Proxy); perr == nil && pu.Host != "" && (pu.Scheme == "http" || pu.Scheme == "https") {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = http.ProxyURL(pu)
			client.Transport = tr
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		writeError(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode >= 400 {
		io.Copy(w, resp.Body)
		return
	}
	// 流式/非流式都原样回传, 逐块 flush, 不让客户端等完整响应。
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				log.Printf("[anthropic-gateway] openai-passthrough 回传写入失败: %v", werr)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
}

func (g *gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	ids := g.registry.AllAliases()
	out := map[string]any{
		"object": "list",
		"data":   ids,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ---- helpers ----

func protoName(p string) string {
	if p == "" {
		return "anthropic"
	}
	return p
}

// gatewayVersion 出站 User-Agent 标识, 便于 opencode 等上游按客户端维度计量/限流。
const gatewayVersion = "1.0"

// claudeSessionHeader Claude Code CLI 的原生会话头 (每对话稳定), opencode 文档中
// "Go recognizes its native session header" 即指它; 网关转译时将其映射为上游要求的
// x-opencode-session。
const claudeSessionHeader = "X-Claude-Code-Session-Id"

// isOpenCodeUpstream 判断该 ResolvedConfig 是否指向 opencode zen/go 上游。
// 判定依据: provider 名为 "opencode", 或 baseUrl 主机是 opencode.ai (含子域)。
func isOpenCodeUpstream(rc modelconfig.ResolvedConfig) bool {
	if strings.EqualFold(strings.TrimSpace(rc.ProviderName), "opencode") {
		return true
	}
	u, err := neturl.Parse(strings.TrimSpace(rc.BaseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}

// opencodeSessionID 取本次入站请求对应的 opencode 会话 ID。
// 优先透传 Claude Code 原生 X-Claude-Code-Session-Id (跨请求稳定, 让 opencode 的
// 路由/提示缓存命中同会话); 无该头 (curl/其他客户端) 时生成随机 UUID 兜底。
func opencodeSessionID(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(claudeSessionHeader)); v != "" {
		return v
	}
	return newSessionID()
}

// newSessionID 生成一个 v4 风格随机 UUID 字符串。
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("gw-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

// rewriteModelField 把请求体里的 model 字段替换为上游裸模型 ID。
// 先按紧凑/格式化 JSON 的精确 "model":"..." 字节模式替换 (快路径, 保留原字段顺序);
// 失败时回退解析 map 重建 (鲁棒, 处理任意空白格式)。
func rewriteModelField(body []byte, bareModel string) []byte {
	if bareModel == "" {
		return body
	}
	// 快路径: "model":"opencode:minimax-m3" → "model":"minimax-m3" (JSON 键无空格)
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil || m.Model == "" || m.Model == bareModel {
		return body
	}
	old := fmt.Sprintf(`"model":"%s"`, m.Model)
	new := fmt.Sprintf(`"model":"%s"`, bareModel)
	if out := bytes.Replace(body, []byte(old), []byte(new), 1); !bytes.Equal(out, body) {
		return out
	}
	// 回退: 解析重建 (容忍 "model" : "..." 等变体)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	raw["model"] = json.RawMessage(`"` + bareModel + `"`)
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// extractSystem 把 Anthropic system 字段 (string | text 块数组) 展平为 []string。
func extractSystem(system interface{}) []string {
	switch v := system.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []interface{}:
		var out []string
		for _, item := range v {
			switch it := item.(type) {
			case string:
				if strings.TrimSpace(it) != "" {
					out = append(out, it)
				}
			case map[string]interface{}:
				if txt, ok := it["text"].(string); ok && strings.TrimSpace(txt) != "" {
					out = append(out, txt)
				}
			}
		}
		return out
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, etype, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type": etype,
		"error": map[string]any{
			"type":    etype,
			"message": msg,
		},
	})
}
