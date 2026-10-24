// Package global 提供美股、加密与宏观的海外数据源适配器。
//
// 设计文档: /home/victor/base/gateway/opencode/search-agent-design.md §3.7.2–§3.7.4 / §7.4
//
// 约束:
//   - 每个源只声明自己真实具备的能力; 未声明的能力不路由, 被调用时返回 ErrUnsupported;
//   - 所有请求带 ctx、超时与 5MB 响应上限, 429/403/超时分别归类;
//   - 无法精确还原的口径 (如 Yahoo 复权) 显式写入 Bar.Adjust 并附带警告, 不伪造数字。
package global

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/market"
)

// gMaxBody 是单次响应体的上限。海外源偶发返回整页 HTML 或超大 JSON,
// 不设上限会让单次请求把内存打满。
const gMaxBody = 5 << 20

// gBrowserUA 是 Yahoo 等网页型接口要求的浏览器 UA。
// Yahoo 对非浏览器 UA 直接 403, 因此不能沿用 cfg.UserAgent。
const gBrowserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// gLimitedReader 返回最多可读 gMaxBody+1 字节的 reader。
// 多出的 1 字节用于让调用方区分 "刚好 5MB" 与 "被截断"。
func gLimitedReader(r io.Reader) io.Reader { return io.LimitReader(r, gMaxBody+1) }

// gGet 发送 GET 请求, 统一设置 UA 并返回响应体。
// 非 2xx 会按 429/403/404/超时等类别转换为 *market.Error。
func gGet(ctx context.Context, c *http.Client, provider, rawURL, ua string, hdr map[string]string) ([]byte, error) {
	if c == nil {
		c = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, market.Wrap(provider, market.ErrUnavailable, err)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	req.Header.Set("Accept", "application/json, text/csv, */*")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, gNetError(provider, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, gStatusError(provider, resp.StatusCode, snippet)
	}

	body, err := io.ReadAll(gLimitedReader(resp.Body))
	if err != nil {
		return nil, gNetError(provider, err)
	}
	if len(body) > gMaxBody {
		return nil, market.Wrap(provider, market.ErrUnavailable,
			fmt.Errorf("response exceeds %d bytes", gMaxBody))
	}
	return body, nil
}

// gDoJSON 发送 GET 请求并把响应体解码到 out。
func gDoJSON(ctx context.Context, c *http.Client, provider, rawURL, ua string, hdr map[string]string, out any) error {
	body, err := gGet(ctx, c, provider, rawURL, ua, hdr)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return market.Wrap(provider, market.ErrUnavailable, fmt.Errorf("decode json: %w", err))
	}
	return nil
}

// gStatusError 把 HTTP 状态码归类为数据面错误。
func gStatusError(provider string, status int, snippet []byte) error {
	msg := strings.TrimSpace(string(snippet))
	var cause error
	if msg != "" {
		cause = fmt.Errorf("http %d: %s", status, msg)
	} else {
		cause = fmt.Errorf("http %d", status)
	}
	switch status {
	case http.StatusTooManyRequests:
		return market.Wrap(provider, market.ErrRateLimited, cause)
	case http.StatusForbidden:
		return market.Wrap(provider, market.ErrBlocked, cause)
	case http.StatusNotFound:
		return market.Wrap(provider, market.ErrNotAvailable, cause)
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return market.Wrap(provider, market.ErrTimeout, cause)
	default:
		return market.Wrap(provider, market.ErrUnavailable, cause)
	}
}

// gNetError 把传输层错误归类为超时或不可用; 调用方主动取消的 ctx 原样返回。
func gNetError(provider string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return market.Wrap(provider, market.ErrTimeout, err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return market.Wrap(provider, market.ErrTimeout, err)
	}
	return market.Wrap(provider, market.ErrUnavailable, err)
}

// gCaps 由能力列表构造能力集合。
func gCaps(caps ...market.Capability) market.CapSet {
	m := make(market.CapSet, len(caps))
	for _, c := range caps {
		m[c] = true
	}
	return m
}

// gCode 从 Instrument 中取交易所代码; Code 为空时回退解析 Canonical。
func gCode(inst market.Instrument) string {
	code := strings.TrimSpace(inst.Code)
	if code == "" {
		if i := strings.LastIndex(inst.Canonical, ":"); i >= 0 {
			code = strings.TrimSpace(inst.Canonical[i+1:])
		}
	}
	return code
}

// gDay 把时间归一到 UTC 当日零点, 用于日线 Date 与区间过滤。
func gDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// gFloat 解析字符串数值; 空串或非法值返回 0。
func gFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// gRawFloat 解析 JSON 原始值中的数值, 同时兼容字符串与数字两种编码。
func gRawFloat(raw json.RawMessage) float64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	if s[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			return gFloat(str)
		}
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// gRawInt64 解析 JSON 原始值中的整数, 同时兼容字符串与数字两种编码。
func gRawInt64(raw json.RawMessage) int64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	if s[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			v, err := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
			if err == nil {
				return v
			}
		}
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// gRoundInt 把浮点数量四舍五入为 int64 (市场数据里的成交量可能带小数)。
func gRoundInt(v float64) int64 {
	if v >= 0 {
		return int64(v + 0.5)
	}
	return int64(v - 0.5)
}

// gBase 是各源共享的适配器基类, 提供名称、市场、能力、健康状态与请求封装。
type gBase struct {
	name     string
	cfg      market.Config
	pc       market.ProviderConfig
	client   *http.Client
	fallback *http.Client // 可选: 主出口失败时的一次性回落客户端 (目前仅 Stooq)
	ua       string
	base     string
	markets  []market.Market
	caps     market.CapSet
	healthy  atomic.Bool
}

// newBase 构造共享基类; baseURL 为空时使用 defBase。
func newBase(name string, pc market.ProviderConfig, cfg market.Config, client *http.Client,
	ua string, markets []market.Market, caps market.CapSet, defBase string) *gBase {
	if client == nil {
		client = http.DefaultClient
	}
	if ua == "" {
		ua = cfg.UserAgent
	}
	if ua == "" {
		ua = "claude-go-market/1.0"
	}
	base := strings.TrimRight(pc.BaseURL, "/")
	if base == "" {
		base = defBase
	}
	b := &gBase{
		name:    name,
		cfg:     cfg,
		pc:      pc,
		client:  client,
		ua:      ua,
		base:    base,
		markets: append([]market.Market(nil), markets...),
		caps:    caps,
	}
	b.healthy.Store(true)
	return b
}

// Name 返回数据源名称。
func (b *gBase) Name() string { return b.name }

// Markets 返回数据源覆盖的市场。
func (b *gBase) Markets() []market.Market {
	return append([]market.Market(nil), b.markets...)
}

// Capabilities 返回数据源声明支持的能力。
func (b *gBase) Capabilities() market.CapSet { return b.caps }

// Healthy 返回本地熔断状态; 它不做网络探测。
func (b *gBase) Healthy() bool { return b.healthy.Load() }

// timeout 返回单请求超时: 优先 provider 级, 其次全局, 最后 8s 兜底。
func (b *gBase) timeout() time.Duration {
	if b.pc.Timeout > 0 {
		return b.pc.Timeout
	}
	if b.cfg.Timeout > 0 {
		return b.cfg.Timeout
	}
	return 8 * time.Second
}

// gShouldFallback 判断主出口失败后是否值得换出口重试一次。
// 只对网络类失败回落; 4xx 中的 404 (标的不存在) 不重试。
func gShouldFallback(err error) bool {
	switch market.KindOf(err) {
	case market.ErrBlocked, market.ErrTimeout, market.ErrUnavailable, market.ErrRateLimited:
		return true
	default:
		return false
	}
}

// get 带超时执行 GET, 并把结果记入本地健康状态。
// 配置了 fallback 时 (Stooq: direct 失败回落 proxy) 只重试一次, 避免放大限流。
func (b *gBase) get(ctx context.Context, rawURL string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, b.timeout())
	defer cancel()
	body, err := gGet(cctx, b.client, b.name, rawURL, b.ua, nil)
	if err != nil && b.fallback != nil && b.fallback != b.client && gShouldFallback(err) {
		body, err = gGet(cctx, b.fallback, b.name, rawURL, b.ua, nil)
	}
	b.record(err)
	return body, err
}

// getJSON 带超时执行 GET 并解码 JSON, 结果记入本地健康状态。
func (b *gBase) getJSON(ctx context.Context, rawURL string, out any) error {
	cctx, cancel := context.WithTimeout(ctx, b.timeout())
	defer cancel()
	err := gDoJSON(cctx, b.client, b.name, rawURL, b.ua, nil, out)
	if err != nil && b.fallback != nil && b.fallback != b.client && gShouldFallback(err) {
		err = gDoJSON(cctx, b.fallback, b.name, rawURL, b.ua, nil, out)
	}
	b.record(err)
	return err
}

// record 更新本地健康状态: 被拒/超时/不可用记为不健康, 成功即恢复。
func (b *gBase) record(err error) {
	if err == nil {
		b.healthy.Store(true)
		return
	}
	switch market.KindOf(err) {
	case market.ErrBlocked, market.ErrTimeout, market.ErrUnavailable:
		b.healthy.Store(false)
	}
}
