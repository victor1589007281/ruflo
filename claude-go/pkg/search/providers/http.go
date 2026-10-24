// Package providers 实现 pkg/search 的检索源适配器。
//
// 设计文档: /home/victor/base/gateway/opencode/search-agent-design.md §3.3–3.6。
//
// 本包的每个源只声明它真正实现的能力: 全部只做 Search, 未实现的 Fetch
// 一律返回 search.ErrUnsupported。错误统一用 search.Wrap 归一化, 调度器
// 据此决定重试、熔断与降级。
package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/search"
)

const (
	// defaultUserAgent 在配置未给 User-Agent 时使用。
	defaultUserAgent = "claude-go-search/1.0 (+https://github.com/victor1589007281/ruflo)"
	// maxBodyBytes 是单个响应体的硬上限, 防止异常源拖垮内存。
	maxBodyBytes = 5 << 20
	// defaultTimeout 在配置未给超时时使用。
	defaultTimeout = 10 * time.Second
	// defaultResultLimit 在请求与配置都没给 limit 时使用。
	defaultResultLimit = 8
	// maxResultLimit 是单源返回条数上限。
	maxResultLimit = 20
	// circuitThreshold 是连续失败多少次后本地熔断。
	circuitThreshold = 3
	// circuitOpenFor 是本地熔断的打开时长。
	circuitOpenFor = 30 * time.Second
	// maxSnippetRunes 是摘要的最大字符数。
	maxSnippetRunes = 400
)

// errBodyTooLarge 表示响应体超过 maxBodyBytes。
var errBodyTooLarge = errors.New("response body exceeds 5MB limit")

// limitedReader 是带 5MB 上限的读取器。
//
// 超出上限时返回 errBodyTooLarge, 而不是静默截断, 避免把半截 JSON
// 当成合法响应。
type limitedReader struct {
	r     io.Reader
	total int64
}

// newLimitedReader 构造一个带 5MB 上限的读取器。
func newLimitedReader(r io.Reader) *limitedReader { return &limitedReader{r: r} }

// Read 实现 io.Reader, 累计读取量并在超限时返回 errBodyTooLarge。
func (l *limitedReader) Read(p []byte) (int, error) {
	if l.total > maxBodyBytes {
		return 0, errBodyTooLarge
	}
	if remain := maxBodyBytes + 1 - l.total; int64(len(p)) > remain {
		p = p[:remain]
	}
	n, err := l.r.Read(p)
	l.total += int64(n)
	if l.total > maxBodyBytes {
		return n, errBodyTooLarge
	}
	return n, err
}

// readAllLimited 读出至多 5MB 的响应体。
func readAllLimited(r io.Reader) ([]byte, error) {
	return io.ReadAll(newLimitedReader(r))
}

// base 是全部 HTTP 源的共享状态: 出口、权重、超时与本地熔断。
type base struct {
	name         string
	tier         search.Tier
	kinds        search.KindSet
	mode         search.Mode
	weight       float64
	ua           string
	timeout      time.Duration
	defaultLimit int
	eg           *search.Egress

	fails     atomic.Int32
	openUntil atomic.Int64
}

// newBase 按配置归一化出共享字段。pc 的零值字段回落到源自身默认值。
func newBase(name string, cfg search.Config, pc search.ProviderConfig, eg *search.Egress, defMode search.Mode, defTier search.Tier, defWeight float64) *base {
	tier := defTier
	if pc.Tier != 0 {
		tier = pc.Tier
	}
	weight := defWeight
	if pc.Weight > 0 {
		weight = pc.Weight
	}
	mode := defMode
	switch pc.Egress {
	case search.ModeDirect, search.ModeProxy, search.ModeAuto:
		mode = pc.Egress
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	timeout := pc.Timeout
	if timeout <= 0 {
		timeout = cfg.SearchTimeout
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	limit := cfg.DefaultLimit
	if limit <= 0 {
		limit = defaultResultLimit
	}
	return &base{
		name:         name,
		tier:         tier,
		mode:         mode,
		weight:       weight,
		ua:           ua,
		timeout:      timeout,
		defaultLimit: limit,
		eg:           eg,
	}
}

// Name 返回 provider 名。
func (b *base) Name() string { return b.name }

// Tier 返回供给层级。
func (b *base) Tier() search.Tier { return b.tier }

// Kinds 返回能力位集合。
func (b *base) Kinds() search.KindSet { return b.kinds }

// EgressMode 返回该源钉死的出口。
func (b *base) EgressMode() search.Mode { return b.mode }

// Weight 返回 RRF 融合权重。
func (b *base) Weight() float64 { return b.weight }

// Healthy 只反映本地熔断状态; 网络可达性由 Egress 与调度器负责。
func (b *base) Healthy() bool {
	until := b.openUntil.Load()
	return until == 0 || time.Now().UnixNano() >= until
}

// Fetch 声明本包所有源都不支持抓取。
func (b *base) Fetch(_ context.Context, _ search.FetchRequest) (*search.Document, error) {
	return nil, search.Wrap(b.name, search.ErrUnsupported, errors.New("fetch is not supported"))
}

// record 记录一次请求结果并按连续失败数维护熔断状态。
func (b *base) record(err error) error {
	if err == nil {
		b.fails.Store(0)
		b.openUntil.Store(0)
		return nil
	}
	switch search.KindOf(err) {
	case search.ErrTimeout, search.ErrRateLimited, search.ErrUnavailable, search.ErrBlocked:
		if b.fails.Add(1) >= circuitThreshold {
			b.openUntil.Store(time.Now().Add(circuitOpenFor).UnixNano())
		}
	}
	return err
}

// client 返回该源钉死出口对应的 HTTP 客户端。
func (b *base) client() *http.Client {
	if b.eg != nil {
		return b.eg.Client(b.mode)
	}
	return fallbackClient()
}

// requestContext 按请求 Deadline 或配置超时派生子 context。
func (b *base) requestContext(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithTimeout(ctx, b.timeout)
	}
	if d, ok := ctx.Deadline(); ok && !deadline.Before(d) {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

// limitOf 归一化返回条数: 0 取默认值, 超过 maxResultLimit 截断。
func (b *base) limitOf(n int) int {
	if n <= 0 {
		n = b.defaultLimit
	}
	if n > maxResultLimit {
		n = maxResultLimit
	}
	return n
}

var (
	bareOnce   sync.Once
	bareClient *http.Client
)

// fallbackClient 在调用方未提供 Egress 时给出一个不经环境代理的直连客户端。
func fallbackClient() *http.Client {
	bareOnce.Do(func() {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		bareClient = &http.Client{Transport: tr}
	})
	return bareClient
}

// endpointURL 校验并归一化一个绝对端点。
func endpointURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse endpoint %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("endpoint %q must use http/https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("endpoint %q has no host", raw)
	}
	return u.String(), nil
}

// withQuery 在保留端点已有参数的前提上追加查询参数。
func withQuery(raw string, params url.Values) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, vs := range params {
		q.Del(k)
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// doRequest 执行一次 GET/POST 并返回响应体、状态码与 Content-Type。
//
// 非 2xx、挑战页与超限响应都归一化为 *search.Error。
func doRequest(ctx context.Context, client *http.Client, name, ua, method, rawURL string, header http.Header, body io.Reader) ([]byte, int, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, 0, "", search.Wrap(name, search.ErrUnavailable, err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		if ua == "" {
			ua = defaultUserAgent
		}
		req.Header.Set("User-Agent", ua)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, "", transportError(name, err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	data, readErr := readAllLimited(resp.Body)
	if readErr != nil {
		return nil, resp.StatusCode, ct, transportError(name, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, resp.StatusCode, ct, statusError(name, resp.StatusCode, resp.Header, data)
	}
	if looksLikeChallenge(ct, data) {
		return data, resp.StatusCode, ct, search.Wrap(name, search.ErrBlocked, fmt.Errorf("challenge page (HTTP %d)", resp.StatusCode))
	}
	return data, resp.StatusCode, ct, nil
}

// doJSON 执行一次请求并把 JSON 响应解码到 out。
func doJSON(ctx context.Context, client *http.Client, name, ua, method, rawURL string, header http.Header, body io.Reader, out any) (int, error) {
	data, status, ct, err := doRequest(ctx, client, name, ua, method, rawURL, header, body)
	if err != nil {
		return status, err
	}
	if strings.Contains(strings.ToLower(ct), "text/html") {
		return status, search.Wrap(name, search.ErrUnavailable, fmt.Errorf("expected JSON, got %s", ct))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return status, search.Wrap(name, search.ErrUnavailable, errors.New("empty JSON body"))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return status, search.Wrap(name, search.ErrUnavailable, fmt.Errorf("decode JSON: %w", err))
	}
	return status, nil
}

// statusError 把非 2xx 状态码归一化为错误类别。
func statusError(name string, status int, h http.Header, body []byte) error {
	msg := snippetText(string(body), 200)
	switch {
	case status == http.StatusTooManyRequests:
		return search.Wrap(name, search.ErrRateLimited, fmt.Errorf("HTTP %d: %s", status, msg))
	case status == http.StatusForbidden:
		if isRateLimitSignal(h, body) {
			return search.Wrap(name, search.ErrRateLimited, fmt.Errorf("HTTP %d: %s", status, msg))
		}
		return search.Wrap(name, search.ErrBlocked, fmt.Errorf("HTTP %d: %s", status, msg))
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return search.Wrap(name, search.ErrTimeout, fmt.Errorf("HTTP %d: %s", status, msg))
	case status == http.StatusUnauthorized || status == http.StatusPaymentRequired || status == http.StatusProxyAuthRequired:
		return search.Wrap(name, search.ErrUnavailable, fmt.Errorf("HTTP %d: %s", status, msg))
	case status >= 500:
		return search.Wrap(name, search.ErrUnavailable, fmt.Errorf("HTTP %d: %s", status, msg))
	default:
		return search.Wrap(name, search.ErrUnavailable, fmt.Errorf("HTTP %d: %s", status, msg))
	}
}

// isRateLimitSignal 判断 403 是否其实是配额耗尽而非封锁。
func isRateLimitSignal(h http.Header, body []byte) bool {
	if h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != "" {
		return true
	}
	low := strings.ToLower(string(body))
	for _, marker := range []string{"rate limit", "rate-limit", "too many requests", "quota exceeded", "api rate"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// transportError 把网络/上下文错误归一化为 timeout 或 unavailable。
func transportError(name string, err error) error {
	var se *search.Error
	if errors.As(err, &se) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return search.Wrap(name, search.ErrTimeout, err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return search.Wrap(name, search.ErrTimeout, err)
	}
	return search.Wrap(name, search.ErrUnavailable, err)
}

// looksLikeChallenge 识别 200 状态下的 HTML 反爬挑战页。
func looksLikeChallenge(contentType string, body []byte) bool {
	if !strings.Contains(strings.ToLower(contentType), "text/html") {
		return false
	}
	low := strings.ToLower(string(body))
	for _, marker := range []string{"captcha", "challenge-platform", "cf-chl", "anomaly", "just a moment"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// rssFeed 是 RSS 2.0 的最小结构。
type rssFeed struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

// rssItem 是 RSS 单条条目。
type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	GUID        string `xml:"guid"`
}

// parseRSS 解析 RSS XML。
func parseRSS(name string, data []byte) ([]rssItem, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = charsetReader
	var feed rssFeed
	if err := dec.Decode(&feed); err != nil {
		return nil, search.Wrap(name, search.ErrUnavailable, fmt.Errorf("decode RSS: %w", err))
	}
	return feed.Channel.Items, nil
}

// charsetReader 支持 RSS 里常见的 UTF-8/ASCII/Latin-1 声明。
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "latin1", "latin-1", "windows-1252":
		// RSS 响应此时已被 readAllLimited 读入内存, 最多 5MB;
		// 逐字节按 Latin-1 映射为 UTF-8, 避免 xml.Decoder 报未知字符集。
		data, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 0, len(data))
		for _, b := range data {
			out = utf8.AppendRune(out, rune(b))
		}
		return bytes.NewReader(out), nil
	default:
		return nil, fmt.Errorf("unsupported charset %q", label)
	}
}

var (
	htmlTagRE = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRE   = regexp.MustCompile(`\s+`)
	yearRE    = regexp.MustCompile(`\b(19|20)\d{2}\b`)
)

// stripHTML 去掉标签、反转义实体并压缩空白。
func stripHTML(s string) string {
	s = htmlTagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(spaceRE.ReplaceAllString(s, " "))
}

// snippetText 清洗并截断摘要文本。
func snippetText(s string, limit int) string {
	return truncateRunes(stripHTML(s), limit)
}

// truncateRunes 按字符数截断, 不会切坏多字节字符。
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// timeLayouts 是各源日期字段可能采用的格式。
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	time.RFC1123Z,
	time.RFC1123,
	time.RFC822Z,
	time.RFC822,
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"2006/01/02",
	"2006 Jan 2",
	"2006 Jan",
	"Jan 2, 2006",
	"02 Jan 2006",
}

// parseTimeAny 尽力解析各源的时间字符串; 失败时返回零值。
func parseTimeAny(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	// PubMed 的 "2024 Jan-Feb" 之类只保留年份。
	if y := yearRE.FindString(s); y != "" {
		if n, err := strconv.Atoi(y); err == nil {
			return time.Date(n, time.January, 1, 0, 0, 0, 0, time.UTC)
		}
	}
	return time.Time{}
}

// finishResults 补全 provider/rank/lang 字段并按 limit 截断。
func finishResults(name string, req search.SearchRequest, in []search.Result, limit int) []search.Result {
	out := make([]search.Result, 0, len(in))
	for _, r := range in {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		r.Provider = name
		r.Rank = len(out) + 1
		if r.Lang == "" {
			r.Lang = req.Language
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// parseDateParts 解析 Crossref 的 date-parts 结构。
func parseDateParts(parts [][]int) time.Time {
	if len(parts) == 0 || len(parts[0]) == 0 {
		return time.Time{}
	}
	p := parts[0]
	year := p[0]
	month, day := 1, 1
	if len(p) > 1 && p[1] > 0 {
		month = p[1]
	}
	if len(p) > 2 && p[2] > 0 {
		day = p[2]
	}
	if year <= 0 {
		return time.Time{}
	}
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}
