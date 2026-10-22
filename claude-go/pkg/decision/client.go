// Package decision 提供一个面向 Ollaya（TypeSafe 线协议兼容）决策模型的 HTTP 客户端。
//
// 背景（2026-10-21 落地，docforge 第 19 章判断层）：claude-go 已有多处消费置信度
// 的地方，但置信度的来源是长度先验 / 关键词命中 / 硬编码常量（见 pkg/agent/swarm.go
// qualityGate、pkg/agent/intent.go）。本包把"判断"从生成模型里剥出来，改由本地决策
// 模型（Ollaya，127.0.0.1:11435）给出**校准过的概率**。
//
// 设计纪律（与 Jev 三档决策制一致）：
//   - 判断只返回结构化概率，不生成文本；
//   - 调用方拿到完整概率分布，自己决定阈值（阈值由 pkg/swarm_intel 的
//     ConformalCalibrator 在线标定，不拍死）；
//   - 任何失败都按 fail-open 回退到原策略，绝不阻断主路径（与 statestore/dual 一致）。
//
// 协议要点（Ollaya /api/decide，与 /v1/systemone 线协议一致）：
//   - 一次请求可携带多个 question，模型一次前向算完（单模型串行，多问题合并成一次往返）；
//   - answers 按 question id 键控返回；
//   - QUEUE_FULL 返回 503 + Retry-After:1，可退避重试；
//   - STATE_TRUNCATED / INPUT_TOO_LONG 返回 422，不可重试（调用方必须先截断）。
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 默认端点与模型。可被 Client 的字段覆盖，便于测试与切换。
const (
	// DefaultBaseURL 是 Ollaya 的默认监听地址（只绑 loopback）。
	DefaultBaseURL = "http://127.0.0.1:11435"

	// ModelRecall 是热路径召回判断用的小模型（P0 RetrieveFor 精判）。
	// 选型依据（Ollaya 2026-10-20 bench）：snap:2b 在 2B 档里 ECE 最好（0.049）、
	// 延迟最低（GPU 20ms）、准确率 0.702，适合高频低 stakes 的召回判断。
	ModelRecall = "snap:2b"

	// ModelScore 是低频高 stakes 打分用的主力模型（P1 swarm qualityGate）。
	// 选型依据：jeb:4b 在 4B 档里质量/速度最优（准确率 0.759、ECE 0.048、GPU 27ms）。
	ModelScore = "jeb:4b"
)

// QuestionType 是 Jev 三种问题原语之一。
type QuestionType string

const (
	// TypeChoice 从 2–255 个选项中选一个，返回 choice + confidence + probabilities。
	TypeChoice QuestionType = "choice"
	// TypeScore 在 2–10 级有序量表上打分，返回 score + confidence + legend + probabilities。
	TypeScore QuestionType = "score"
	// TypeNoul 返回 0..1 的"该命题成立"概率，无 confidence。
	TypeNoul QuestionType = "noul"
)

// Question 是一次决策调用里的单个问题。
//
// 线协议（v0.15.0 实测 + preset show 拿到权威形状）：questions 是 map[id]question，
// question 平铺 {type, instructions, criteria}，criteria 的形状随 type 变：
//   - choice：map[label]description（2-255 个）
//   - score： []string（2-10 级，0 在前）
//   - noul：  无 criteria
// 响应的 answer 是 internally-tagged enum（带 "type" 字段），此处 Go 侧用平铺 struct 接。
type Question struct {
	// ID 是问题的键（不进 JSON body；由 Decide 用来组装 map[id]question）。
	ID string `json:"-"`
	// Type 是问题原语。
	Type QuestionType `json:"type"`
	// Instructions 是问题的人话描述。
	Instructions string `json:"instructions,omitempty"`
	// Options 仅 choice：label → description。
	Options map[string]string `json:"-"`
	// Legend 仅 score：各级描述，level 0 在前。
	Legend []string `json:"-"`
}

// MarshalJSON 把 Question 拍成线协议形状：{type, instructions, criteria}。
func (q Question) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": string(q.Type)}
	if q.Instructions != "" {
		m["instructions"] = q.Instructions
	}
	switch q.Type {
	case TypeChoice:
		m["criteria"] = q.Options
	case TypeScore:
		m["criteria"] = q.Legend
	case TypeNoul:
		// noul 无 criteria
	}
	return json.Marshal(m)
}

// Answer 是单个问题的决策结果（按 Type 填充对应字段）。
type Answer struct {
	// choice 原语
	Choice string `json:"choice,omitempty"`
	// score 原语
	Score  float64  `json:"score,omitempty"`
	Legend map[string]string `json:"legend,omitempty"`
	// noul 原语
	Noul float64 `json:"noul,omitempty"`
	// choice/score 共有：校准置信度与完整概率分布。
	Confidence   float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// Response 是 /api/decide 的响应。Answers 按 Question.ID 键控。
type Response struct {
	Answers        map[string]Answer `json:"answers"`
	Model          string            `json:"model,omitempty"`
	StateTruncated bool              `json:"state_truncated,omitempty"`
}

// apiError 是 Ollaya 统一错误体。
type apiError struct {
	Err  string `json:"error"`
	Code string `json:"code"`
}

// Error 是带错误码的决策错误，调用方可用 Code 判断是否可重试。
type Error struct {
	Code       string
	Message    string
	StatusCode int
	Retryable  bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("decision: %s (code=%s http=%d)", e.Message, e.Code, e.StatusCode)
}

// Client 是 Ollaya 决策客户端。零值可用（用 DefaultBaseURL）。
type Client struct {
	// BaseURL 覆盖默认端点（含 scheme，无尾斜杠）。
	BaseURL string
	// HTTPClient 可注入自定义 http.Client（测试 / 超时控制）。
	HTTPClient *http.Client
	// Timeout 是单次请求超时（不含重试）。默认 8s——无 GPU 机器上 CPU 推理可能到秒级。
	Timeout time.Duration
	// MaxRetries 是对可重试错误（503 QUEUE_FULL、网络抖动、5xx）的最大重试次数。默认 2。
	MaxRetries int
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	to := c.Timeout
	if to <= 0 {
		to = 8 * time.Second
	}
	return &http.Client{Timeout: to}
}

func (c *Client) maxRetries() int {
	if c.MaxRetries < 0 {
		return 0
	}
	if c.MaxRetries == 0 {
		return 2
	}
	return c.MaxRetries
}

// decideRequest 是 /api/decide 的请求体。Questions 是 map[id]question。
// KeepAlive 是 Ollaya 的续租参数（"24h"/"1h30m"/"300ms" 等）：本次调用会把该
// 模型的过期窗口推后该时长。默认 24h——决策模型一旦加载，几乎都希望常驻
// （CPU 冷加载 10–17s，按需加载会击穿 qualityGate 的 20s 预算）；要按需释放
// 由 WarmModels 的反向调用方显式设置 0。
type decideRequest struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
	KeepAlive string              `json:"keep_alive,omitempty"`
}

// Decide 用 model 对 state 做一次决策，返回按问题 id 键控的答案。
//
// 失败语义（fail-open 的关键）：返回的 error 一定是 *Error，调用方据 Retryable 决定是否
// 自行降级；本包不在这里吞错，因为"回退到原策略"是调用方的职责（它知道原策略是什么）。
// 但本包保证：任何网络/服务端/解析失败都返回 error 而不是 panic 或部分结果。
func (c *Client) Decide(ctx context.Context, model string, state any, questions []Question) (*Response, error) {
	if model == "" {
		return nil, &Error{Code: "INVALID_REQUEST", Message: "model is empty", Retryable: false}
	}
	if len(questions) == 0 {
		return nil, &Error{Code: "INVALID_REQUEST", Message: "no questions", Retryable: false}
	}

	qm := make(map[string]Question, len(questions))
	for _, q := range questions {
		if q.ID == "" {
			return nil, &Error{Code: "INVALID_REQUEST", Message: "question with empty ID", Retryable: false}
		}
		qm[q.ID] = q
	}
	reqBody := decideRequest{
		Model:     model,
		State:     state,
		Questions: qm,
		// 默认 24h 续租：每次真实调用都把模型过期窗口推后一天，
		// 配合 WarmModels 的 3 分钟心跳，模型在常驻 worker 上实际不会卸载。
		KeepAlive: "24h",
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, &Error{Code: "INVALID_REQUEST", Message: err.Error(), Retryable: false}
	}

	var lastErr error
	attempts := 1 + c.maxRetries()
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// 退避：QUEUE_FULL 的 Retry-After 是 1s，这里用指数退避（200ms 起），避免惊群。
			backoff := time.Duration(200*(1<<(attempt-1))) * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, &Error{Code: "CANCELLED", Message: ctx.Err().Error(), Retryable: false}
			case <-time.After(backoff):
			}
		}

		resp, derr := c.doOnce(ctx, payload)
		if derr == nil {
			return resp, nil
		}
		lastErr = derr
		var de *Error
		if ok := errorAs(derr, &de); ok && !de.Retryable {
			return nil, derr // 不可重试（422 等）：立即返回
		}
	}
	return nil, lastErr
}

// errorAs 是 errors.As 的局部包装，避免在不必要处暴露标准库依赖细节。
func errorAs(err error, target **Error) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*Error); ok {
		*target = e
		return true
	}
	return false
}

// doOnce 执行单次 HTTP 调用并解析响应。
func (c *Client) doOnce(ctx context.Context, payload []byte) (*Response, error) {
	url := c.base() + "/api/decide"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, &Error{Code: "INVALID_REQUEST", Message: err.Error(), Retryable: false}
	}
	req.Header.Set("Content-Type", "application/json")

	httpResp, err := c.httpClient().Do(req)
	if err != nil {
		// 网络/超时错误：可重试。
		return nil, &Error{Code: "NETWORK", Message: err.Error(), Retryable: true}
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 16<<20))
	if err != nil {
		return nil, &Error{Code: "READ", Message: err.Error(), Retryable: true}
	}

	if httpResp.StatusCode != http.StatusOK {
		var ae apiError
		_ = json.Unmarshal(body, &ae) // 尽力解析；解析失败也能用 status 判断
		code := ae.Code
		if code == "" {
			code = fmt.Sprintf("HTTP_%d", httpResp.StatusCode)
		}
		return nil, &Error{
			Code:       code,
			Message:    ae.Err,
			StatusCode: httpResp.StatusCode,
			Retryable:  isRetryable(httpResp.StatusCode, code),
		}
	}

	var out Response
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &Error{Code: "PARSE", Message: err.Error(), Retryable: false}
	}
	return &out, nil
}

// isRetryable 判定哪些错误值得退避重试。
// 422（INVALID_REQUEST / TOO_MANY_OPTIONS / INPUT_TOO_LONG / STATE_TRUNCATED）不可重试——
// 重试同样的输入只会再撞一次；503 QUEUE_FULL 与 5xx 服务端错误可重试。
func isRetryable(status int, code string) bool {
	switch code {
	case "INVALID_REQUEST", "TOO_MANY_OPTIONS", "INPUT_TOO_LONG", "STATE_TRUNCATED",
		"INVALID_JSON", "MODEL_NOT_FOUND", "UNAUTHORIZED", "FORBIDDEN":
		return false
	}
	if status == http.StatusServiceUnavailable || status == http.StatusConflict || status >= 500 {
		return true
	}
	return false
}
