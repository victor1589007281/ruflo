package llmgw

// remote.go —— L1 网关的 **remote 那条腿** (design/02 §3.1 "实现两态"的第二态,
// 也是 §四 T1「网关分离」标着"断链"的那个根因)。
//
// ---------------------------------------------------------------------------
// 它与"把 BaseURL 改指网关"有什么不同 (这是最容易被当成重复工作的一点)
// ---------------------------------------------------------------------------
//
// 仓里**已经**有一条流量经网关的路: `modelconfig.ApplyGatewayOverride` 读
// `CLAUDE_GO_LLM_GATEWAY`, 把 `api.Client.BaseURL` (以及 fallback 端点) 改指网关。
// 那条路解决的是"字节走不走网关"; 它解决**不了**设计里 remote 态真正要的那件事:
//
//	local  档: 进程内仍是完整的 api.Client —— 重试、熔断三件套、FallbackModels
//	          与独立 fallback 端点、prompt cache 自适应关闭, 全部在**每个副本内**
//	          各跑一份。多副本时熔断状态互不可见 (design/02 §3.1「集中化的收益」
//	          明说这是当前的病灶)。
//	remote 档: 上层拿到的 LLMGateway 背后**没有 api.Client**。本进程不做重试、
//	          不开熔断、不持有 provider 凭据 —— 这些全归网关进程。这才是 T1
//	          「本机所有 claude-go 系进程与下游平台共用一个网关 (熔断/配额/记账
//	          集中)」这句话成立的形态。
//
// 两档共用**同一个地址来源** (`CLAUDE_GO_LLM_GATEWAY`)。刻意不造第二个地址变量:
// 那个变量已经写进 `deploy/k8s/distributed.yaml` 与 `k8s-job.yaml`, 再加一个
// 语义相近的开关, 迟早出现"设了 A 没设 B, 于是网关是装饰品"——那正是这个变量
// 自己刚被修过的病 (它曾是全仓零读取的死变量)。
//
// ---------------------------------------------------------------------------
// ⚠️ 默认 local, 且 remote 不可静默回落 —— 两条都是硬约束
// ---------------------------------------------------------------------------
//
// **默认 local**: `CLAUDE_GO_LLM_GATEWAY_MODE` 不设 = 行为逐字节不变。改默认会
// 影响 8+ 个在用 :18080 的下游平台 (storyloom/mediaforge/testforge/growring…),
// 而 remote 档去掉的恰是它们此刻正依赖的进程内重试与 fallback。
//
// **网关不可达或非 2xx 一律报错, 绝不回落到本进程直连**: 回落会让"流量经网关"
// 这个部署事实变成谎话, 而运维侧完全看不出来 —— access.jsonl 行数少了几条,
// 没有任何告警。本文件因此**结构上就不持有 `*api.Client`**: 不是"选择不回落",
// 而是回落的对象根本不在手上 (fail-closed 最可靠的形态是让错误路径不存在)。
//
// ---------------------------------------------------------------------------
// 为什么不在 remote 里再叠一层重试
// ---------------------------------------------------------------------------
//
// design/02 §4.3 的成文教训是**重试单层化**。网关进程内已经/将要承担重试与熔断,
// 客户端再叠一层的后果是重试次数相乘 (4×4=16), 而 429 场景下相乘的重试就是把
// 限流打得更死。remote 档的一次调用 = 一次 HTTP 往返, 成败如实上报。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// GatewayModeEnv 选择 LLMGateway 用哪个实现: 空/local = 进程内直连 (默认),
// remote = 把请求发给独立的 llm-gateway 进程。
//
// 地址仍取 CLAUDE_GO_LLM_GATEWAY (见本文件头): 这里只选实现, 不定义第二个地址。
const GatewayModeEnv = "CLAUDE_GO_LLM_GATEWAY_MODE"

// 实现档位取值。
const (
	ModeLocal  = "local"
	ModeRemote = "remote"
)

// remoteRawMaxTokens 是 Raw 在调用方未指定上限时的默认值,
// 与 api.Client.RawComplete 的默认值一致 (两档不能给同一次多模态调用不同的上限)。
const remoteRawMaxTokens = 8192

// remoteRespLimit 单次非流式响应的读取上限, 与网关侧 handleMessages 的入站上限同量级。
const remoteRespLimit = 64 << 20

// RemoteConfig 远程网关客户端配置。
type RemoteConfig struct {
	// BaseURL 网关根地址 (如 http://claude-go-gateway:18081)。
	// 不带路径: 出站时拼 "/messages", 与 api.Client 的约定一致, 也正是
	// llmgw.Server 服务的两条路径之一。
	BaseURL string
	// Model 出站模型名 (可带 "provider:" 前缀, 网关自己剥)。
	Model string
	// APIKey 客户端→网关这一跳的凭据。可空: 网关是否校验由网关决定
	// (与 ApplyGatewayOverride 的"原样透传"口径一致)。
	APIKey string
	// Timeout 非流式调用的整体超时; <=0 表示不设。
	// **刻意不落在 http.Client.Timeout 上**: 那个超时把响应体读取也算进去,
	// 会在流式长连接上把正常的长回答掐断成"读到一半的流"。
	Timeout time.Duration
	// HTTPClient 允许注入 (测试/连接池调优); 为 nil 时用一个流式友好的默认值。
	HTTPClient *http.Client
}

// Remote 远程实现: 把请求转发到独立的 llm-gateway 进程。
//
// 注意字段里**没有** *api.Client —— 见本文件头"不可静默回落"。
type Remote struct {
	baseURL string
	model   string
	apiKey  string
	timeout time.Duration
	client  *http.Client
}

var _ LLMGateway = (*Remote)(nil)

// NewRemote 构建远程网关客户端。
//
// BaseURL / Model 缺一个就报错而不是"用个默认值先跑起来": 没有地址等于不知道
// 往哪发, 没有模型名等于让网关拿默认路由去猜 —— 两种猜法都会让"这次调用到底
// 打的是谁"在事后无法回答。
func NewRemote(cfg RemoteConfig) (*Remote, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("llmgw remote: 网关地址为空 (设 %s)", modelconfig.GatewayEnvName())
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llmgw remote: 未指定模型名 (网关按 model 路由 provider)")
	}
	cl := cfg.HTTPClient
	if cl == nil {
		cl = &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost: 16,
			// 只约束握手/首包, 不约束整体 —— 流式回答可以很长。
			ResponseHeaderTimeout: 120 * time.Second,
		}}
	}
	return &Remote{
		baseURL: base,
		model:   strings.TrimSpace(cfg.Model),
		apiKey:  cfg.APIKey,
		timeout: cfg.Timeout,
		client:  cl,
	}, nil
}

// NewFromEnv 按 CLAUDE_GO_LLM_GATEWAY_MODE 选实现, 默认 local。
//
// 这是生产装配点该调的那一个函数 (feishu/dashboard 各一处), 而不是各自
// os.Getenv 再自己分支 —— 档位判定散成多份就会出现"某个入口忘了跟", 表现为
// 一半流量经网关一半直连, 且两边日志长得一样。
//
// remote 档下 Model/APIKey 仍取自已解析好的 api.Client: 模型别名的解析
// (role>plan>global 优先级、providers 表) 是 modelconfig 的职责, 网关档位不该
// 把它重做一遍。**但 c 的 BaseURL/重试/熔断在 remote 档下全部不参与出站。**
func NewFromEnv(c *api.Client) (LLMGateway, error) {
	switch mode := strings.ToLower(strings.TrimSpace(os.Getenv(GatewayModeEnv))); mode {
	case "", ModeLocal:
		return NewLocal(c), nil
	case ModeRemote:
		if c == nil {
			return nil, fmt.Errorf("llmgw: %s=remote 但没有已解析的模型配置", GatewayModeEnv)
		}
		gw := modelconfig.GatewayBaseURL()
		if gw == "" {
			// fail-closed: 声明了 remote 却没给地址, 绝不"退回 local 先跑起来"——
			// 那正是让"流量经网关"变成谎话的那条边。
			return nil, fmt.Errorf("llmgw: %s=remote 但 %s 未设置 (无网关地址)",
				GatewayModeEnv, modelconfig.GatewayEnvName())
		}
		r, err := NewRemote(RemoteConfig{BaseURL: gw, Model: c.Model, APIKey: c.APIKey})
		if err != nil {
			return nil, err
		}
		log.Printf("[llmgw] L1 网关档位=remote, 出站经 %s (本进程不做重试/熔断/fallback)", gw)
		return r, nil
	default:
		// 拼错的档位名不能当 local 放过去: "设了以为生效其实没生效"是本仓反复
		// 出现的失效模式, 报错比静默正确得多。
		return nil, fmt.Errorf("llmgw: %s=%q 无效 (可选 %s|%s)", GatewayModeEnv, mode, ModeLocal, ModeRemote)
	}
}

// ---------------------------------------------------------------------------
// LLMGateway 实现
// ---------------------------------------------------------------------------

// Complete 非流式补全 (语义同 api.Client.SendMessage)。
func (r *Remote) Complete(ctx context.Context, req ChatRequest) (*types.APIResponse, error) {
	body, err := r.buildBody(req, false)
	if err != nil {
		return nil, err
	}
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	resp, err := r.do(ctx, body, req.Trace, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, remoteRespLimit))
	if err != nil {
		return nil, fmt.Errorf("llmgw remote: 读网关响应失败: %w", err)
	}
	var out types.APIResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("llmgw remote: 网关响应不是 Messages JSON (%d 字节): %w", len(raw), err)
	}
	return &out, nil
}

// Stream 流式补全 (语义同 api.Client.StreamMessage: 事件通道 + 错误通道, 两者都会被关闭)。
func (r *Remote) Stream(ctx context.Context, req ChatRequest) (<-chan types.StreamDelta, <-chan error) {
	// 缓冲量与 api.Client 一致: 换实现不该改变消费方看到的背压形态。
	eventCh := make(chan types.StreamDelta, 100)
	errCh := make(chan error, 1)

	go func() {
		defer close(eventCh)
		defer close(errCh)

		body, err := r.buildBody(req, true)
		if err != nil {
			errCh <- err
			return
		}
		resp, err := r.do(ctx, body, req.Trace, true)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			// 兼容 "data: " 与 "data:" 两种前缀 (DashScope 类网关无空格), 与
			// api.Client 的解析逐条对齐。
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var delta types.StreamDelta
			if json.Unmarshal([]byte(data), &delta) != nil {
				continue
			}
			select {
			case eventCh <- delta:
			case <-ctx.Done():
				return
			}
		}
		// **中途断流必须报错**: 扫描器静默结束会让消费方读成"空产出 + nil 错误",
		// 也就是把网络中断伪装成"模型什么都没说"。本仓在 worker 事件流上吃过
		// 同一个坑 (design/02 §3.3「终态只认队列」那段)。
		if err := sc.Err(); err != nil {
			errCh <- fmt.Errorf("llmgw remote: 读网关流中断: %w", err)
		}
	}()

	return eventCh, errCh
}

// Simple 单轮文本补全 (语义同 api.Client.SimpleComplete)。
func (r *Remote) Simple(ctx context.Context, system, user string) (string, error) {
	resp, err := r.Complete(ctx, r.simpleReq(ctx, system, user))
	if err != nil {
		return "", err
	}
	return joinText(resp), nil
}

// Raw 以 content 数组直接发一次请求 (语义同 api.Client.RawComplete)。
func (r *Remote) Raw(ctx context.Context, contentJSON json.RawMessage, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = remoteRawMaxTokens
	}
	resp, err := r.Complete(ctx, ChatRequest{
		Messages:  []types.APIMessage{{Role: "user", Content: contentJSON}},
		MaxTokens: maxTokens,
		Trace:     traceFromCtx(ctx),
	})
	if err != nil {
		return "", err
	}
	return joinText(resp), nil
}

// Diag 等价 Simple 但多回一行诊断元数据 (语义同 api.Client.CompleteDiag)。
//
// diag 的字段与格式**逐字**照抄 api.Client.CompleteDiag: 合议扇出靠这行区分
// "空响应 / 被 max_tokens 截断 / 超时"三种长得一样的失败, 换个格式等于让那套
// 归因在 remote 档下失效, 而失效的表现只是"日志里少了点东西"。
func (r *Remote) Diag(ctx context.Context, system, user string) (string, string, error) {
	resp, err := r.Complete(ctx, r.simpleReq(ctx, system, user))
	if err != nil {
		return "", fmt.Sprintf("SendMessage 出错: %v", err), err
	}
	var sb strings.Builder
	blockTypes := make([]string, 0, len(resp.Content))
	for _, b := range resp.Content {
		blockTypes = append(blockTypes, string(b.Type))
		if b.Type == types.ContentBlockText {
			sb.WriteString(b.Text)
		}
	}
	text := sb.String()
	outTok := 0
	if resp.Usage != nil {
		outTok = resp.Usage.OutputTokens
	}
	tail := text
	if len(tail) > 160 {
		tail = "…" + tail[len(tail)-160:]
	}
	diag := fmt.Sprintf("stop=%s outTok=%d blocks=%d%v textLen=%d tail=%q",
		resp.StopReason, outTok, len(resp.Content), blockTypes, len(text), tail)
	return text, diag, nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// simpleReq 构造 Simple/Diag 用的请求体, 与 api.Client 的两个同名方法逐字段对齐
// (含 user 侧的 content 块形态与 max_tokens 上限)。
func (r *Remote) simpleReq(ctx context.Context, system, user string) ChatRequest {
	userJSON, _ := json.Marshal(user)
	return ChatRequest{
		Messages: []types.APIMessage{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"text","text":` + string(userJSON) + `}]`),
		}},
		System: []string{system},
		// 上限取 pkg/api 导出的同一个常量, 不在这里写第二个 65536:
		// 两处各写一份的表现是"同一段 prompt 在 local 档出完整 JSON, 换 remote
		// 档就被截断在半途"。
		MaxTokens: api.SimpleCompleteMaxTokens,
		Trace:     traceFromCtx(ctx),
	}
}

// buildBody 组装 Anthropic Messages 请求体。
//
// **不加 cache_control**: prompt cache 按 design/02 §3.1 归网关集中处理
// (连"因 API 报错自适应关闭"那个状态位也该在网关那一份)。客户端各自贴
// cache_control 就回到了"每进程一套缓存策略"的现状。
func (r *Remote) buildBody(req ChatRequest, stream bool) ([]byte, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("llmgw remote: messages 为空")
	}
	out := types.APIRequest{
		Model:     r.model,
		Messages:  req.Messages,
		MaxTokens: req.MaxTokens,
		Stream:    stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = remoteRawMaxTokens
	}
	switch len(req.System) {
	case 0:
	case 1:
		out.System = req.System[0]
	default:
		blocks := make([]map[string]any, len(req.System))
		for i, s := range req.System {
			blocks[i] = map[string]any{"type": "text", "text": s}
		}
		out.System = blocks
	}
	if len(req.Tools) > 0 {
		out.Tools = req.Tools
	}
	return json.Marshal(out)
}

// do 发一次请求并**只在 2xx 时**返回响应; 其余一律 error (fail-closed)。
func (r *Remote) do(ctx context.Context, body []byte, t Trace, stream bool) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llmgw remote: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	if r.apiKey != "" {
		httpReq.Header.Set("x-api-key", r.apiKey)
		httpReq.Header.Set("Authorization", "Bearer "+r.apiKey)
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	setRemoteTraceHeaders(httpReq, effectiveTrace(ctx, t))

	resp, err := r.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llmgw remote: 网关 %s 不可达: %w", r.baseURL, err)
	}
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("llmgw remote: 网关返回 %d: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return resp, nil
}

// effectiveTrace 请求结构体里带了 trace 就以它为准, 否则取 ctx 里的
// (与 Local 的 withReqTrace 同一优先级, 免得两档的归因规则不一样)。
func effectiveTrace(ctx context.Context, t Trace) Trace {
	if !t.IsZero() {
		return t
	}
	return traceFromCtx(ctx)
}

func traceFromCtx(ctx context.Context) Trace { return api.TraceFromContext(ctx) }

// setRemoteTraceHeaders 把 trace 四元组落成出站请求头。
//
// 为什么 remote 也必须自己落头: local 档是 api.Client 落的 (api/trace.go 的
// setTraceHeaders, 未导出)。remote 档绕开了 api.Client, 若不在这里补, 网关侧
// server.go 那四个 `r.Header.Get(api.TraceHeaderRunID)` 就又读回空值 ——
// 也就是把刚修好的"两头都写了、中间没接"在新一条腿上重演一遍。
//
// 头名取 pkg/api 的常量而不是字面量: 名字只有一份, 改名时编译器会找到这里。
func setRemoteTraceHeaders(req *http.Request, t Trace) {
	if req == nil || t.IsZero() {
		return // 未设 trace 时一个头都不多发, 与直连形态逐字节一致
	}
	for h, v := range map[string]string{
		api.TraceHeaderRunID:  t.RunID,
		api.TraceHeaderNodeID: t.NodeID,
		api.TraceHeaderTurnID: t.TurnID,
		api.TraceHeaderCallID: t.CallID,
	} {
		if v != "" {
			req.Header.Set(h, v)
		}
	}
}

// joinText 拼接响应里的 text 块 (与 api.Client 的三个 *Complete 取法一致)。
func joinText(resp *types.APIResponse) string {
	if resp == nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range resp.Content {
		if b.Type == types.ContentBlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
