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
	"github.com/anthropic/claude-go/pkg/types"
)

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

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", gw.handleMessages)
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
	if g.tryPrefix != "" && !strings.Contains(model, ":") {
		rc2 := g.resolver.ResolveAlias(g.tryPrefix + ":" + model)
		if rc2.BaseURL != "" && rc2.APIKey != "" && rc2.ProviderName != "" {
			return rc2, nil
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
		g.proxyPassthrough(w, r, body, rc)
		return
	}

	client := buildClient(rc)
	if req.ToolChoice != nil && req.ToolChoice.Type == "any" {
		client = client.WithToolChoiceAny()
	}

	systemPrompt := extractSystem(req.System)
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16384
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
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
	if v := r.Header.Get("anthropic-version"); v != "" {
		req.Header.Set("anthropic-version", v)
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
