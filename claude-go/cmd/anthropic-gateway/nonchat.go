package main

// nonchat.go — 非对话 wire 形状的透传入口: embeddings(向量) 与 systemone(决策)。
//
// 背景: 上游聚合器把"对话/向量/决策"三类模型放在三个不同的 base 下, 并且用
// api[shape].base_url 显式声明。模型条目因此在 config 里带 protocol 与
// upstreamBaseUrl 两个字段 (见 modelconfig.ModelConfig)。本文件只做一件事:
// 把客户端发来的同形状请求原样转给正确的上游路径, 注入凭据/会话头, 原样回传。
//
// 为什么不做"翻译": 这两类 wire 没有 Anthropic/OpenAI 的等价物 —— 向量请求没有
// messages, 决策返回的是概率分布而不是文本。任何翻译都会丢字段 (决策的
// probabilities/confidence 正是全部价值)。所以入口只接受**同形状**请求:
// 客户端要发 /v1/embeddings 就得按 OpenAI embeddings 形状发, 要发 /v1/systemone
// 就得按 systemone 形状发; 形状不符由上游用自己的 4xx 说清楚, 网关不猜。
//
// 与对话路径的一个刻意差异: **不按会话回退模型**。handleMessages 在模型未声明时会
// 回退到"这个会话上次用的模型", 因为子 agent 常填错名字, 回退比 404 更贴近事实。
// 但向量/决策请求换模型等于换语义 (一个 1536 维、一个 3072 维; 一个问题一个概率),
// 静默替换只会让调用方拿到"形状对但意思错"的结果 —— 这里必须 404。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/httpauth"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	// protocolEmbeddings: OpenAI 兼容的 /embeddings 向量接口。
	protocolEmbeddings = "embeddings"
	// protocolSystemOne: Jev/TypeSafe 决策线协议 (/systemone, 概率输出)。
	protocolSystemOne = "systemone"
)

// handleEmbeddings POST /v1/embeddings (及裸 /embeddings)。
func (g *gateway) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	g.proxyNonChat(w, r, protocolEmbeddings, "/embeddings")
}

// handleSystemOne POST /v1/systemone (及 /v1/decide、裸路径)。
// 两个入口名指向同一条上游路径: systemone 是线协议名, decide 是它做的事。
func (g *gateway) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	g.proxyNonChat(w, r, protocolSystemOne, "/systemone")
}

// nonChatProtocolPath 非对话协议唯一可用的入口路径 (对话入口的拒绝文案用它指路)。
func nonChatProtocolPath(protocol string) string {
	switch protocol {
	case protocolEmbeddings:
		return "/v1/embeddings"
	case protocolSystemOne:
		return "/v1/decide"
	}
	return ""
}

// rejectNonChatProtocol 对话入口 (messages / chat-completions / responses) 上的
// 非对话模型直接 400。
//
// 为什么必须在分派前拦住: 对话入口对未知协议一律按"翻译成 OpenAI 形状"处理,
// 于是 embeddings/systemone 模型会被拼上 {providerBase}/chat/completions 或
// /messages 去请求 —— 上游回一个与真实原因无关的错误 (404/400 说请求体不对),
// 排查时看起来像网关 bug。这里用一句话说清"这个模型不在对话面上, 该去哪个端点"。
func rejectNonChatProtocol(w http.ResponseWriter, rc modelconfig.ResolvedConfig) bool {
	target := nonChatProtocolPath(rc.Protocol)
	if target == "" {
		return false
	}
	writeError(w, 400, "invalid_request_error", fmt.Sprintf(
		"模型 %s 的协议是 %q，不属于对话面；请改用 %s 端点", rc.Alias, protoName(rc.Protocol), target))
	return true
}

// nonChatUpstreamBase 该模型的非对话出站根地址。
//
// 优先模型级 upstreamBaseUrl (聚合上游的 systemone/embeddings 与对话不同 base),
// 缺省回落 provider.baseUrl —— 后者与改造前一致, 自建单 base 的上游无需改配置。
func nonChatUpstreamBase(rc modelconfig.ResolvedConfig) string {
	if b := strings.TrimRight(strings.TrimSpace(rc.UpstreamBaseURL), "/"); b != "" {
		return b
	}
	return strings.TrimRight(rc.BaseURL, "/")
}

// proxyNonChat 是同形状透传的唯一实现: 解析模型 → 校验协议 → 改写 model 为裸名 →
// 注入凭据/会话头 → 转发到 {base}{upstreamPath} → 原样回传。
func (g *gateway) proxyNonChat(w http.ResponseWriter, r *http.Request, wantProtocol, upstreamPath string) {
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
	if err := json.Unmarshal(body, &probe); err != nil || strings.TrimSpace(probe.Model) == "" {
		writeError(w, 400, "invalid_request_error", "请求体缺少 model 字段")
		return
	}

	rc, err := g.resolveModel(probe.Model)
	if err != nil {
		// 刻意不回退: 见文件头注释。
		writeError(w, 404, "model_not_found", err.Error())
		return
	}
	if rc.Protocol != wantProtocol {
		writeError(w, 400, "invalid_request_error", fmt.Sprintf(
			"模型 %s 的协议是 %q，不能用于 %s（%s 协议）",
			rc.Alias, protoName(rc.Protocol), upstreamPath, wantProtocol))
		return
	}

	upstream := nonChatUpstreamBase(rc) + upstreamPath
	forwardBody := rewriteModelField(body, rc.ProviderName)

	// 超时与对话翻译路径同一档 (--upstream-timeout): 决策模型首次加载可能要十几秒,
	// 但绝不该无限挂住。
	client := &http.Client{Timeout: *upstreamTimeout}
	if rc.Proxy != "" {
		if pu, perr := neturl.Parse(rc.Proxy); perr == nil && pu.Host != "" && (pu.Scheme == "http" || pu.Scheme == "https") {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = http.ProxyURL(pu)
			client.Transport = tr
		}
	}

	policy := api.RetryPolicyFromConfig(rc.RetryCount, rc.RetryBaseSec, rc.RetryMaxSec, rc.RetryStatuses)
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream, bytes.NewReader(forwardBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", rc.APIKey)
		req.Header.Set("Authorization", "Bearer "+rc.APIKey)
		req.Header.Set("User-Agent", "anthropic-gateway/"+gatewayVersion)
		// opencode zen/go 的所有形状都强制 x-opencode-session (缺失 → 400)。
		if isOpenCodeUpstream(rc) {
			req.Header.Set("x-opencode-session", opencodeSessionID(r, ""))
		}
		if v := types.AppFromContext(r.Context()); v != "" && v != types.DefaultAppID {
			req.Header.Set(httpauth.AppHeader, v)
		}
		return req, nil
	}

	started := time.Now()
	resp, attempts, err := api.DoWithRetry(r.Context(), client, policy, build)
	if err != nil {
		if r.Context().Err() != nil {
			log.Printf("[anthropic-gateway] nonchat 已取消 (protocol=%s attempts=%d): %v", wantProtocol, attempts, err)
			return
		}
		log.Printf("[anthropic-gateway] nonchat 上游失败 (protocol=%s model=%s upstream=%s attempts=%d): %v",
			wantProtocol, rc.ProviderName, upstream, attempts, err)
		writeError(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	log.Printf("[anthropic-gateway] nonchat app=%s protocol=%s model=%s resolved=%s upstream=%s status=%d attempts=%d ms=%d proxy=%s",
		gatewayAppLabel(r), wantProtocol, probe.Model, rc.ProviderName, upstream,
		resp.StatusCode, attempts, time.Since(started).Milliseconds(), rc.Proxy)

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	// 向量响应可能很大 (1024+ 维浮点), 但都是单次完整 JSON, 无流式语义。
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				log.Printf("[anthropic-gateway] nonchat 回传写入失败: %v", werr)
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
