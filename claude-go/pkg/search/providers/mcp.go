package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

const (
	// mcpProtocolVersion 是本文客户端实现的 Streamable HTTP 协议版本。
	mcpProtocolVersion = "2025-03-26"
	// mcpClientName 是 initialize 时上报的客户端名。
	mcpClientName = "claude-go-search"
	// mcpClientVersion 是 initialize 时上报的客户端版本。
	mcpClientVersion = "1.0"
	// maxMCPToolRunes 是单条工具输出的摘要截断长度。
	maxMCPToolRunes = 400
)

// mcpRetryBackoff 是传输失败后的重连退避时长; 测试可临时调小。
var mcpRetryBackoff = 30 * time.Second

// mcpTool 是 tools/list 返回的工具描述。
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// rpcError 是 JSON-RPC 2.0 错误对象。
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// rpcResponse 是 JSON-RPC 2.0 响应。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// rpcRequest 是 JSON-RPC 2.0 请求。
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcCallError 保留服务端 JSON-RPC 错误码, 供参数回退判定使用。
type rpcCallError struct {
	Code    int
	Message string
}

// Error 实现 error 接口。
func (e *rpcCallError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// toolOutputError 表示 tools/call 以 isError=true 返回的业务错误。
type toolOutputError struct{ msg string }

// Error 实现 error 接口。
func (e *toolOutputError) Error() string { return e.msg }

// mcpClient 是一个最小的 MCP Streamable HTTP 客户端。
//
// 常驻 initialize 结果与工具清单; 传输失败后进入 30s 退避并清空会话,
// 退避结束后的首个请求重新握手。所有 RPC 由 mu 串行化, 避免并发
// initialize 与 session 争用。
type mcpClient struct {
	name     string
	endpoint string
	client   *http.Client
	ua       string

	mu          sync.Mutex
	nextID      int64
	sessionID   string
	protocol    string
	initialized bool
	tools       []mcpTool
	tool        string
	toolDef     mcpTool
	toolDone    bool
	openUntil   atomic.Int64
}

// newMCPClient 构造 MCP 客户端。
func newMCPClient(name, endpoint string, client *http.Client, ua string) *mcpClient {
	return &mcpClient{name: name, endpoint: endpoint, client: client, ua: ua}
}

// Healthy 返回是否已脱离退避窗口。
func (c *mcpClient) Healthy() bool {
	until := c.openUntil.Load()
	return until == 0 || time.Now().UnixNano() >= until
}

// Close 释放客户端持有的逻辑状态 (HTTP 连接由 Egress 统一管理)。
func (c *mcpClient) Close() error { return nil }

// failLocked 在传输类失败后进入退避并清空会话与工具缓存。
func (c *mcpClient) failLocked(err error) {
	switch search.KindOf(err) {
	case search.ErrTimeout, search.ErrRateLimited, search.ErrUnavailable, search.ErrBlocked:
		c.openUntil.Store(time.Now().Add(mcpRetryBackoff).UnixNano())
		c.initialized = false
		c.sessionID = ""
		c.toolDone = false
		c.tool = ""
		c.toolDef = mcpTool{}
		c.tools = nil
	}
}

// searchTool 返回用于检索的工具名; 首次调用执行 initialize + tools/list。
func (c *mcpClient) searchTool(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.Healthy() {
		return "", search.Wrap(c.name, search.ErrUnavailable, fmt.Errorf("MCP endpoint in %s reconnect backoff", mcpRetryBackoff))
	}
	if err := c.ensureLocked(ctx); err != nil {
		return "", err
	}
	if c.toolDone {
		if c.tool == "" {
			return "", search.Wrap(c.name, search.ErrUnsupported, errors.New("MCP server exposes no search tool"))
		}
		return c.tool, nil
	}

	raw, err := c.rpcLocked(ctx, "tools/list", map[string]any{})
	if err != nil {
		return "", err
	}
	var payload struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", search.Wrap(c.name, search.ErrUnsupported, fmt.Errorf("decode tools/list: %w", err))
	}
	c.tools = payload.Tools
	for _, t := range payload.Tools {
		lower := strings.ToLower(t.Name)
		// 只选检索工具; web_fetch / research 之类名字里也可能带 search 前缀。
		if strings.Contains(lower, "search") && !strings.Contains(lower, "fetch") {
			c.tool = t.Name
			c.toolDef = t
			break
		}
	}
	c.toolDone = true
	if c.tool == "" {
		return "", search.Wrap(c.name, search.ErrUnsupported, errors.New("MCP server exposes no search tool"))
	}
	return c.tool, nil
}

// callSearch 调用搜索工具。
//
// 参数形状由 tools/list 返回的 inputSchema 决定, 而不是写死 {query,numResults}:
// Parallel 的 web_search 要求 {objective, search_queries[]}, 按旧形状调用会
// 被服务端以参数错误拒绝, 表现为整条供给线 degrade。参数被拒时再回退到
// 该工具的最小必填集合。
func (c *mcpClient) callSearch(ctx context.Context, tool, query string, limit int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.Healthy() {
		return "", search.Wrap(c.name, search.ErrUnavailable, fmt.Errorf("MCP endpoint in %s reconnect backoff", mcpRetryBackoff))
	}
	if err := c.ensureLocked(ctx); err != nil {
		return "", err
	}

	text, err := c.toolCallLocked(ctx, tool, argsForToolSchema(c.toolDef.InputSchema, query, limit))
	if err == nil {
		return text, nil
	}
	if !isArgumentFailure(err) {
		return "", err
	}
	return c.toolCallLocked(ctx, tool, argsForToolSchema(c.toolDef.InputSchema, query, 0))
}

// argsForToolSchema 按工具的 inputSchema 组装调用参数。
//
// 只发送 schema 里声明过的字段: 多发的字段在严格服务端 (additionalProperties=false)
// 会直接触发参数错误。limit<=0 表示不传数量偏好。
func argsForToolSchema(raw json.RawMessage, query string, limit int) map[string]any {
	props := schemaProperties(raw)
	if len(props) == 0 {
		// 服务端未声明 inputSchema: 用最常见的 query+numResults 探测,
		// 被拒后由调用方回退到仅 query。
		if limit > 0 {
			return map[string]any{"query": query, "numResults": limit}
		}
		return map[string]any{"query": query}
	}
	if props["search_queries"] {
		args := map[string]any{
			"objective":      query,
			"search_queries": []string{query},
		}
		if limit > 0 {
			for _, key := range []string{"max_results", "num_results", "numResults"} {
				if props[key] {
					args[key] = limit
					break
				}
			}
		}
		return args
	}
	if props["query"] {
		args := map[string]any{"query": query}
		if limit > 0 {
			for _, key := range []string{"numResults", "num_results", "max_results"} {
				if props[key] {
					args[key] = limit
					break
				}
			}
		}
		return args
	}
	// 未知 schema: 用最常见形状探测, 失败后由 isArgumentFailure 决定是否值得重试。
	return map[string]any{"query": query}
}

// schemaProperties 返回 inputSchema 的顶层属性名集合。
func schemaProperties(raw json.RawMessage) map[string]bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return nil
	}
	out := make(map[string]bool, len(s.Properties))
	for k := range s.Properties {
		out[k] = true
	}
	return out
}

// ensureLocked 在未初始化时执行一次 initialize。
func (c *mcpClient) ensureLocked(ctx context.Context) error {
	if c.initialized {
		return nil
	}
	raw, err := c.rpcLocked(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    mcpClientName,
			"version": mcpClientVersion,
		},
	})
	if err != nil {
		return err
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &res)
	if res.ProtocolVersion != "" {
		c.protocol = res.ProtocolVersion
	} else {
		c.protocol = mcpProtocolVersion
	}
	c.initialized = true
	c.openUntil.Store(0)
	return nil
}

// toolCallLocked 执行 tools/call 并取出文本内容。
func (c *mcpClient) toolCallLocked(ctx context.Context, tool string, args map[string]any) (string, error) {
	raw, err := c.rpcLocked(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", search.Wrap(c.name, search.ErrUnsupported, fmt.Errorf("decode tools/call: %w", err))
	}
	var texts []string
	for _, part := range out.Content {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	text := strings.Join(texts, "\n")
	if out.IsError {
		if text == "" {
			text = "tool reported an error"
		}
		return "", search.Wrap(c.name, classifyToolMessage(text), &toolOutputError{msg: text})
	}
	if text == "" && len(out.StructuredContent) > 0 && string(out.StructuredContent) != "null" {
		text = string(out.StructuredContent)
	}
	if strings.TrimSpace(text) == "" {
		return "", search.Wrap(c.name, search.ErrUnsupported, errors.New("tools/call returned no text content"))
	}
	return text, nil
}

// rpcLocked 发送一条 JSON-RPC 请求并返回 result。
func (c *mcpClient) rpcLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	rr, err := c.postLocked(ctx, method, params)
	if err != nil {
		c.failLocked(err)
		return nil, err
	}
	if rr.Error != nil {
		rpcErr := &rpcCallError{Code: rr.Error.Code, Message: rr.Error.Message}
		return nil, search.Wrap(c.name, classifyRPCCode(rpcErr), rpcErr)
	}
	if len(rr.Result) == 0 {
		return nil, search.Wrap(c.name, search.ErrUnavailable, fmt.Errorf("%s returned no result", method))
	}
	return rr.Result, nil
}

// postLocked 发送一条 JSON-RPC 消息并读取 JSON 或 SSE 响应。
func (c *mcpClient) postLocked(ctx context.Context, method string, params any) (*rpcResponse, error) {
	c.nextID++
	id := c.nextID
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, search.Wrap(c.name, search.ErrUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, search.Wrap(c.name, search.ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", c.ua)
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	if c.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", c.protocol)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, transportError(c.name, err)
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := readAllLimited(resp.Body)
		return nil, statusError(c.name, resp.StatusCode, resp.Header, body)
	}

	lr := newLimitedReader(resp.Body)
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		br := bufio.NewReader(lr)
		if peek, _ := br.Peek(6); bytes.HasPrefix(peek, []byte("event:")) || bytes.HasPrefix(peek, []byte("data:")) {
			ct = "text/event-stream"
		}
		lr = newLimitedReader(br)
	}
	if strings.Contains(ct, "text/event-stream") {
		rr, err := readSSE(lr, id)
		if err != nil {
			return nil, search.Wrap(c.name, search.ErrUnavailable, err)
		}
		return rr, nil
	}

	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, transportError(c.name, err)
	}
	if looksLikeChallenge(resp.Header.Get("Content-Type"), data) {
		return nil, search.Wrap(c.name, search.ErrBlocked, errors.New("MCP endpoint returned a challenge page"))
	}
	var rr rpcResponse
	if err := json.Unmarshal(data, &rr); err != nil {
		return nil, search.Wrap(c.name, search.ErrUnavailable, fmt.Errorf("decode JSON-RPC response: %w", err))
	}
	return &rr, nil
}

// readSSE 逐行读取 SSE, 返回 id 匹配的那条 JSON-RPC 响应。
func readSSE(r io.Reader, wantID int64) (*rpcResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxBodyBytes)

	var data []string
	flush := func() (*rpcResponse, bool) {
		if len(data) == 0 {
			return nil, false
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		if strings.TrimSpace(payload) == "[DONE]" {
			return nil, false
		}
		var rr rpcResponse
		if err := json.Unmarshal([]byte(payload), &rr); err != nil {
			return nil, false
		}
		if rr.ID == nil || !rpcIDMatches(rr.ID, wantID) {
			return nil, false
		}
		return &rr, true
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if rr, ok := flush(); ok {
				return rr, nil
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		v := strings.TrimPrefix(line, "data:")
		v = strings.TrimPrefix(v, " ")
		data = append(data, v)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if rr, ok := flush(); ok {
		return rr, nil
	}
	return nil, errors.New("SSE stream ended without a matching JSON-RPC response")
}

// rpcIDMatches 判断响应 id 是否与请求 id 相同 (兼容数字与字符串形式)。
func rpcIDMatches(raw json.RawMessage, want int64) bool {
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	return s == strconv.FormatInt(want, 10)
}

// classifyRPCCode 把 JSON-RPC 错误码映射为归一化类别。
func classifyRPCCode(e *rpcCallError) search.ErrorKind {
	switch e.Code {
	case -32601: // method not found
		return search.ErrUnsupported
	case -32602: // invalid params
		return search.ErrUnsupported
	}
	msg := strings.ToLower(e.Message)
	switch {
	case strings.Contains(msg, "rate limit"), strings.Contains(msg, "too many requests"), strings.Contains(msg, "quota"):
		return search.ErrRateLimited
	case strings.Contains(msg, "unauthor"), strings.Contains(msg, "authenticat"), strings.Contains(msg, "forbidden"):
		return search.ErrUnavailable
	case strings.Contains(msg, "argument"), strings.Contains(msg, "parameter"), strings.Contains(msg, "invalid"):
		return search.ErrUnsupported
	default:
		return search.ErrUnavailable
	}
}

// classifyToolMessage 把 isError 的工具输出映射为归一化类别。
func classifyToolMessage(msg string) search.ErrorKind {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "rate limit"), strings.Contains(low, "too many requests"), strings.Contains(low, "quota"):
		return search.ErrRateLimited
	case strings.Contains(low, "argument"), strings.Contains(low, "parameter"), strings.Contains(low, "numresults"),
		strings.Contains(low, "num_results"), strings.Contains(low, "unknown field"), strings.Contains(low, "unexpected field"):
		return search.ErrUnsupported
	default:
		return search.ErrUnavailable
	}
}

// isArgumentFailure 判断一次调用是否因参数形状不被接受而值得降级重试。
func isArgumentFailure(err error) bool {
	var rpcErr *rpcCallError
	if errors.As(err, &rpcErr) {
		if rpcErr.Code == -32602 {
			return true
		}
		low := strings.ToLower(rpcErr.Message)
		return containsAny(low, "argument", "parameter", "numresults", "num_results", "unknown field", "unexpected field")
	}
	var toolErr *toolOutputError
	if errors.As(err, &toolErr) {
		low := strings.ToLower(toolErr.msg)
		return containsAny(low, "argument", "parameter", "numresults", "num_results", "unknown field", "unexpected field", "not supported", "unsupported")
	}
	return false
}

// containsAny 判断 s 是否包含任一子串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// mcpResultItem 是 MCP 搜索工具输出里的单条结果; 兼容常见字段别名。
type mcpResultItem struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	Link          string   `json:"link"`
	Snippet       string   `json:"snippet"`
	Description   string   `json:"description"`
	Text          string   `json:"text"`
	Content       string   `json:"content"`
	Excerpts      []string `json:"excerpts"`
	PublishedDate string   `json:"publishedDate"`
	Published     string   `json:"published"`
	PublishDate   string   `json:"publish_date"`
	Score         float64  `json:"score"`
}

// parseMCPResults 解析 MCP 搜索工具返回的文本。
//
// 支持 JSON 数组、{"results":[...]}、{"web":{"results":[...]}} 三种形状;
// 都不是时退化为逐行标题/URL 解析。仍解析不出则返回 ErrUnsupported。
func parseMCPResults(name, text string) ([]search.Result, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, search.Wrap(name, search.ErrUnsupported, errors.New("empty MCP search output"))
	}

	if strings.HasPrefix(trimmed, "[") {
		var items []mcpResultItem
		if err := json.Unmarshal([]byte(trimmed), &items); err == nil {
			return mcpItemsToResults(items), nil
		}
	}
	if strings.HasPrefix(trimmed, "{") {
		var wrapped struct {
			Results []mcpResultItem `json:"results"`
			Web     struct {
				Results []mcpResultItem `json:"results"`
			} `json:"web"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapped); err == nil {
			if wrapped.Results != nil {
				return mcpItemsToResults(wrapped.Results), nil
			}
			if wrapped.Web.Results != nil {
				return mcpItemsToResults(wrapped.Web.Results), nil
			}
		}
	}

	// Exa 等源返回 "Title:/URL:/Highlights:" 块文本; 必须在逐行 URL 兜底之前
	// 解析, 否则 "URL: https://..." 这一行会被当成标题 "URL"。
	if blocks := parseMCPBlocks(trimmed); len(blocks) > 0 {
		return blocks, nil
	}
	if lines := parseMCPLines(trimmed); len(lines) > 0 {
		return lines, nil
	}
	return nil, search.Wrap(name, search.ErrUnsupported, errors.New("unrecognised MCP search output shape"))
}

// mcpItemsToResults 把结构化条目转成 search.Result。
func mcpItemsToResults(items []mcpResultItem) []search.Result {
	out := make([]search.Result, 0, len(items))
	for _, it := range items {
		url := strings.TrimSpace(it.URL)
		if url == "" {
			url = strings.TrimSpace(it.Link)
		}
		if url == "" {
			continue
		}
		title := strings.TrimSpace(it.Title)
		if title == "" {
			title = url
		}
		snippet := firstNonEmpty(it.Snippet, it.Description, strings.Join(it.Excerpts, " "), it.Text, it.Content)
		// publish_date 是 Parallel 的字段名; 漏掉它会让新闻结果全部没有时间,
		// 质量门的 fresh 维度因此恒不过。
		published := firstNonEmpty(it.PublishedDate, it.Published, it.PublishDate)
		out = append(out, search.Result{
			URL:       url,
			Title:     title,
			Snippet:   truncateRunes(stripHTML(snippet), maxMCPToolRunes),
			Score:     it.Score,
			Published: parseTimeAny(published),
		})
	}
	return out
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// parseMCPBlocks 解析 "Title:/URL:/Published:/Highlights:" 形式的块文本。
//
// 块以行首 "Title:" 开始; URL 是同一个块里的第一行 URL。Snippet 取 Highlights
// 之后的前若干行 (Exa 的摘要就在那里), 没有 Highlights 块就不猜正文。
func parseMCPBlocks(text string) []search.Result {
	if !strings.Contains(text, "Title:") {
		return nil
	}
	lines := strings.Split(text, "\n")
	var out []search.Result
	var cur *mcpResultItem
	var snippet strings.Builder
	inHighlights := false

	flush := func() {
		if cur == nil || !isHTTPURL(strings.TrimSpace(cur.URL)) {
			cur, snippet = nil, strings.Builder{}
			return
		}
		cur.Snippet = truncateRunes(strings.TrimSpace(snippet.String()), maxMCPToolRunes)
		out = append(out, search.Result{
			URL:       strings.TrimSpace(cur.URL),
			Title:     strings.TrimSpace(cur.Title),
			Snippet:   stripHTML(cur.Snippet),
			Published: parseTimeAny(firstNonEmpty(cur.PublishedDate, cur.Published, cur.PublishDate)),
			Score:     cur.Score,
		})
		cur, snippet = nil, strings.Builder{}
		inHighlights = false
	}

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "Title:"):
			flush()
			cur = &mcpResultItem{Title: strings.TrimSpace(strings.TrimPrefix(line, "Title:"))}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "URL:"):
			if cur.URL == "" {
				cur.URL = strings.TrimSpace(strings.TrimPrefix(line, "URL:"))
			}
			inHighlights = false
		case strings.HasPrefix(line, "Published:"):
			cur.Published = strings.TrimSpace(strings.TrimPrefix(line, "Published:"))
		case strings.HasPrefix(line, "Highlights:"):
			inHighlights = true
		case strings.HasPrefix(line, "Author:"), strings.HasPrefix(line, "Text:"):
			inHighlights = false
		case inHighlights && line != "" && snippet.Len() < maxMCPToolRunes:
			if snippet.Len() > 0 {
				snippet.WriteByte(' ')
			}
			snippet.WriteString(line)
		}
	}
	flush()
	if len(out) == 0 {
		return nil
	}
	return out
}

var (
	// httpURLRE 从自由文本里提取 URL。
	httpURLRE = regexp.MustCompile(`https?://[^\s<>"'()\[\]]+`)
	// listPrefixRE 去掉 "1. " / "2) " / "- " 这类列表前缀。
	listPrefixRE = regexp.MustCompile(`^\s*(?:\d+[.)]|[-*•])\s*`)
)

// parseMCPLines 逐行拆出 "标题 + URL" 形式的工具输出。
func parseMCPLines(text string) []search.Result {
	var out []search.Result
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// Markdown 链接: [标题](https://...)
		if i := strings.Index(line, "]("); i > 0 {
			if start := strings.LastIndex(line[:i], "["); start >= 0 {
				if end := strings.Index(line[i+2:], ")"); end >= 0 {
					u := strings.TrimSpace(line[i+2 : i+2+end])
					if isHTTPURL(u) {
						out = append(out, search.Result{URL: u, Title: strings.TrimSpace(line[start+1 : i])})
						continue
					}
				}
			}
		}
		u := httpURLRE.FindString(line)
		if u == "" {
			continue
		}
		u = strings.TrimRight(u, ".,;:!?")
		title := strings.TrimSpace(strings.Replace(line, u, "", 1))
		title = listPrefixRE.ReplaceAllString(title, "")
		title = strings.Trim(title, " \t-–—:：|()[]{}<>\"'*#•")
		if title == "" {
			title = u
		}
		out = append(out, search.Result{URL: u, Title: title})
	}
	return out
}

// isHTTPURL 判断字符串是否是 http(s) 绝对 URL。
func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// mcpProvider 是 Parallel / Exa 匿名 MCP 的共用实现。
type mcpProvider struct {
	*base
	mcp *mcpClient
}

// newParallel 构造 Parallel 匿名 MCP 源 (无鉴权, 直连)。
func newParallel(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	return newMCPProvider("parallel", cfg, pc, eg, "https://search.parallel.ai/mcp", 1.0)
}

// newExa 构造 Exa 匿名 MCP 源 (无鉴权, 直连)。
func newExa(cfg search.Config, pc search.ProviderConfig, eg *search.Egress) (search.Provider, error) {
	return newMCPProvider("exa_anon", cfg, pc, eg, "https://mcp.exa.ai/mcp", 0.9)
}

// newMCPProvider 构造 MCP 搜索源。
func newMCPProvider(name string, cfg search.Config, pc search.ProviderConfig, eg *search.Egress, defURL string, defWeight float64) (search.Provider, error) {
	raw := pc.BaseURL
	if strings.TrimSpace(raw) == "" {
		raw = defURL
	}
	endpoint, err := endpointURL(raw)
	if err != nil {
		return nil, err
	}
	b := newBase(name, cfg, pc, eg, search.ModeDirect, search.TierL0, defWeight)
	b.kinds = search.KindSet(search.KindSearch)
	return &mcpProvider{
		base: b,
		mcp:  newMCPClient(name, endpoint, b.client(), b.ua),
	}, nil
}

// Close 释放 MCP 客户端状态。
func (p *mcpProvider) Close() error { return p.mcp.Close() }

// Healthy 综合本地熔断与 MCP 退避状态。
func (p *mcpProvider) Healthy() bool { return p.base.Healthy() && p.mcp.Healthy() }

// Search 通过 MCP 工具执行检索。
func (p *mcpProvider) Search(ctx context.Context, req search.SearchRequest) (out []search.Result, err error) {
	defer func() { err = p.record(err) }()

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, search.Wrap(p.name, search.ErrInvalid, errors.New("empty query"))
	}
	query = searchQueryWithDomainHints(query, req)
	ctx, cancel := p.requestContext(ctx, req.Deadline)
	defer cancel()

	tool, err := p.mcp.searchTool(ctx)
	if err != nil {
		return nil, err
	}
	text, err := p.mcp.callSearch(ctx, tool, query, p.limitOf(req.Limit))
	if err != nil {
		return nil, err
	}
	results, err := parseMCPResults(p.name, text)
	if err != nil {
		return nil, err
	}
	return finishResults(p.name, req, results, p.limitOf(req.Limit)), nil
}
