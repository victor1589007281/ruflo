package search

// fetch.go —— 单页抓取、正文抽取与字符集处理。
//
// 抓取前先做 URL 校验 (只允许 http/https, 解析 DNS 后拒绝私网/环回/链路本地
// 地址), 防止 WebFetch 被当成 SSRF 跳板; 响应按 Content-Type 选择 HTML 抽取
// 或纯文本, 字符集从 header/meta 探测后转 UTF-8, 最终按 maxChars 截断。
//
// HTML 抽取不引第三方 DOM 依赖, 用一个小型容错解析器挑正文根节点, 再转成
// 带标题/列表/链接的 Markdown; 解析失败时退回原始文本而不是报错。
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

const (
	defaultFetchChars = 16000
	maxFetchBodyBytes = 5 << 20
	maxRedirects      = 10
	// minContentRunes 是候选正文容器的最低可读文本量; 低于它退回全树渲染,
	// 避免把只有一行的小页面误判成"选不出正文"。
	minContentRunes = 10
)

// resolveIPs 可在测试中替换, 用于在离线环境解析域名。
var resolveIPs = net.LookupIP

// urlGuard 是抓取 URL 的校验入口。生产恒指向 validateFetchURL;
// 同包测试可替换它以便对 httptest 的环回地址做端到端验证。
// 注意: 重定向逐跳校验也走同一个入口。
var urlGuard = validateFetchURL

// Fetch 抓取并提取一个 URL 的正文。
//
// 流程: SSRF 校验 -> 直连 F1 -> 失败且配置了代理时用代理再试一次。
// 只有 GET 幂等请求, 因此重试是安全的。返回值中的 Document.Via 固定
// 为 "http", 正文经过 readability-lite 提取并按 MaxChars 截断。
func (m *Manager) Fetch(ctx context.Context, req FetchRequest) (*FetchResponse, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if m.isClosed() {
		return nil, Wrap("", ErrUnavailable, errors.New("search manager closed"))
	}

	u, err := urlGuard(req.URL)
	if err != nil {
		return nil, err
	}
	maxChars := req.MaxChars
	if maxChars <= 0 {
		maxChars = defaultFetchChars
	}

	deadline := req.Deadline
	if deadline.IsZero() {
		timeout := m.cfg.FetchTimeout
		if timeout <= 0 {
			timeout = 8 * time.Second
		}
		deadline = time.Now().Add(timeout)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	modes := []Mode{ModeDirect}
	if m.egress != nil && m.egress.HasProxy() {
		modes = append(modes, ModeProxy)
	}

	var lastErr error
	var doc *Document
	for i, mode := range modes {
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				lastErr = Wrap("fetch", classifyError(err), err)
			}
			break
		}
		client := m.egress.Client(mode)
		m.metrics.Inc("egress_requests_total", map[string]string{
			"provider": "fetch",
			"egress":   string(mode),
		})
		doc, err = m.fetchWithClient(ctx, client, u, maxChars)
		if err == nil {
			if i > 0 {
				m.metrics.Inc("egress_fallback_total", map[string]string{
					"from": "direct", "to": "proxy", "reason": "error",
				})
			}
			break
		}
		lastErr = err
		// 输入非法 (SSRF/协议) 不因换出口而改变, 直接结束。
		if KindOf(err) == ErrInvalid {
			break
		}
	}

	resp := &FetchResponse{Document: doc, ElapsedMS: time.Since(start).Milliseconds()}
	if doc == nil {
		return resp, lastErr
	}
	m.metrics.Inc("fetch_success_total", map[string]string{"via": doc.Via})
	return resp, nil
}

// fetchWithClient 用指定客户端执行一次抓取与正文提取。
func (m *Manager) fetchWithClient(ctx context.Context, client *http.Client, u *url.URL, maxChars int) (*Document, error) {
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return Wrap("fetch", ErrBlocked, errors.New("too many redirects"))
		}
		_, err := urlGuard(req.URL.String())
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, Wrap("fetch", ErrInvalid, err)
	}
	req.Header.Set("User-Agent", m.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1")

	resp, err := c.Do(req)
	if err != nil {
		return nil, wrapFetchError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, Wrap("fetch", statusKind(resp.StatusCode), fmt.Errorf("http status %d", resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBodyBytes+1))
	if err != nil {
		return nil, Wrap("fetch", ErrUnavailable, err)
	}
	if len(body) > maxFetchBodyBytes {
		return nil, Wrap("fetch", ErrUnavailable, errors.New("response body exceeds 5MB"))
	}

	mediaType, params := parseMediaType(resp.Header.Get("Content-Type"))
	charset := params["charset"]
	if charset == "" {
		charset = detectMetaCharset(body)
	}
	// 二进制内容直接拒绝; 文本内容即使编码探测失败也尝试按 UTF-8 读。
	if strings.IndexByte(string(body), 0x00) >= 0 {
		return nil, Wrap("fetch", ErrUnsupported, errors.New("binary response body"))
	}
	text, decodeErr := decodeBytes(body, charset)
	if decodeErr != nil {
		text = string(body)
	}

	var title, byline, markdown string
	var published time.Time
	switch mediaType {
	case "", "text/html", "application/xhtml+xml":
		if mediaType == "" && !looksHTML(text) {
			markdown = normalizeMarkdown(text)
			title = firstLine(markdown)
			break
		}
		page := extractHTML(text)
		title, byline, published, markdown = page.Title, page.Byline, page.Published, page.Markdown
	case "text/plain", "text/markdown":
		markdown = normalizeMarkdown(text)
		title = firstLine(markdown)
	default:
		return nil, Wrap("fetch", ErrUnsupported, fmt.Errorf("unsupported content type %q", mediaType))
	}

	markdown, truncated := truncateRunes(strings.TrimSpace(markdown), maxChars)
	doc := &Document{
		URL:       resp.Request.URL.String(),
		Title:     title,
		Markdown:  markdown,
		Byline:    byline,
		Published: published,
		FetchedAt: time.Now(),
		Hash:      hashDocument(markdown),
		Truncated: truncated,
		Via:       "http",
	}
	return doc, nil
}

// validateFetchURL 校验抓取目标, 拒绝非 http(s)、环回、私网与链路本地地址。
//
// 解析出 IP 后再判断, 因此 "localhost" 这类别名同样会被拦截。重定向的
// 每一跳都由 CheckRedirect 调用本函数。
func validateFetchURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, Wrap("fetch", ErrInvalid, errors.New("empty URL"))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, Wrap("fetch", ErrInvalid, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, Wrap("fetch", ErrInvalid, fmt.Errorf("unsupported scheme %q", u.Scheme))
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, Wrap("fetch", ErrInvalid, errors.New("missing host"))
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, Wrap("fetch", ErrInvalid, fmt.Errorf("blocked host %q", host))
	}
	ips, err := resolveIPs(host)
	if err != nil {
		return nil, Wrap("fetch", ErrUnavailable, fmt.Errorf("resolve %s: %w", host, err))
	}
	if len(ips) == 0 {
		return nil, Wrap("fetch", ErrUnavailable, fmt.Errorf("resolve %s: no address", host))
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return nil, Wrap("fetch", ErrInvalid, fmt.Errorf("blocked address %s", ip))
		}
	}
	return u, nil
}

// isPublicIP 判断 IP 是否公网可路由地址。
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return true
}

// wrapFetchError 归一化抓取错误。
func wrapFetchError(err error) error {
	var se *Error
	if errors.As(err, &se) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Wrap("fetch", ErrTimeout, err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Wrap("fetch", ErrTimeout, err)
	}
	return Wrap("fetch", ErrUnavailable, err)
}

// statusKind 把 HTTP 状态码映射到错误类别。
func statusKind(code int) ErrorKind {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrBlocked
	case code == http.StatusTooManyRequests:
		return ErrRateLimited
	case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout:
		return ErrTimeout
	default:
		return ErrUnavailable
	}
}

// parseMediaType 解析 Content-Type; 缺失或非法时返回空 media type。
func parseMediaType(raw string) (string, map[string]string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", map[string]string{}
	}
	mt, params, err := mime.ParseMediaType(raw)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.Split(raw, ";")[0])), map[string]string{}
	}
	if params == nil {
		params = map[string]string{}
	}
	return strings.ToLower(mt), params
}

// decodeBytes 按 charset 解码; 未知 charset 时按原样返回。
func decodeBytes(b []byte, charset string) (string, error) {
	charset = strings.ToLower(strings.TrimSpace(charset))
	if charset == "" || charset == "utf-8" || charset == "utf8" {
		return string(b), nil
	}
	enc, err := htmlindex.Get(charset)
	if err != nil || enc == nil {
		return string(b), nil
	}
	out, _, err := transform.Bytes(enc.NewDecoder(), b)
	if err != nil {
		return string(b), err
	}
	return string(out), nil
}

// detectMetaCharset 从 HTML 头部嗅探 charset 声明。
func detectMetaCharset(b []byte) string {
	n := len(b)
	if n > 4096 {
		n = 4096
	}
	s := strings.ToLower(string(b[:n]))
	for i := 0; i < len(s); {
		idx := strings.Index(s[i:], "charset")
		if idx < 0 {
			break
		}
		p := i + idx + len("charset")
		for p < len(s) && (s[p] == ' ' || s[p] == '\t' || s[p] == '\n' || s[p] == '\r') {
			p++
		}
		if p >= len(s) || s[p] != '=' {
			i = p
			continue
		}
		p++
		for p < len(s) && (s[p] == ' ' || s[p] == '\t' || s[p] == '\n' || s[p] == '\r') {
			p++
		}
		if p >= len(s) {
			break
		}
		var q byte
		if s[p] == '"' || s[p] == '\'' {
			q = s[p]
			p++
		}
		start := p
		for p < len(s) {
			c := s[p]
			if q != 0 {
				if c == q {
					break
				}
			} else if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ';' || c == '>' || c == '"' || c == '\'' {
				break
			}
			p++
		}
		if p > start {
			return s[start:p]
		}
		i = start
	}
	return ""
}

// looksHTML 判断无 Content-Type 时内容是否像 HTML。
func looksHTML(s string) bool {
	head := strings.ToLower(s)
	if len(head) > 512 {
		head = head[:512]
	}
	return strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html") ||
		strings.Contains(head, "<head") || strings.Contains(head, "<body") ||
		strings.Contains(head, "<article") || strings.Contains(head, "<p")
}

// firstLine 返回文本第一行非空内容, 用于纯文本标题。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if utf8.RuneCountInString(line) > 120 {
				line, _ = truncateRunes(line, 120)
			}
			return line
		}
	}
	return ""
}

// truncateRunes 按字符截断, 返回是否发生截断。
func truncateRunes(s string, max int) (string, bool) {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s, false
	}
	r := []rune(s)
	return string(r[:max]), true
}

// ---------------------------------------------------------------------------
// readability-lite: 轻量 HTML 解析与正文提取
// ---------------------------------------------------------------------------

// htmlNode 是极简 DOM 节点。tag 为空表示文本节点。
type htmlNode struct {
	tag      string
	attrs    map[string]string
	text     string
	children []*htmlNode
	parent   *htmlNode
}

// extractedPage 是正文提取结果。
type extractedPage struct {
	Title     string
	Byline    string
	Published time.Time
	Markdown  string
}

// voidTags 是无闭合标签的元素。
var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

// extractHTML 解析 HTML 并输出 Markdown 化正文。
func extractHTML(raw string) extractedPage {
	root := parseHTML(raw)
	meta := collectHTMLMeta(root)
	content := pickContentRoot(root)
	markdown := renderMarkdown(content)
	if strings.TrimSpace(markdown) == "" {
		markdown = renderMarkdown(root)
	}
	return extractedPage{
		Title:     meta.title,
		Byline:    meta.byline,
		Published: meta.published,
		Markdown:  markdown,
	}
}

// parseHTML 用容错扫描器构建节点树。
//
// 不引入第三方 HTML 解析器: 这里只需要"去噪 + 取正文", 不要求严格
// 符合 HTML5 树构建算法。未闭合的 p/li 会按常见写法自动收口。
func parseHTML(s string) *htmlNode {
	root := &htmlNode{tag: "#root"}
	stack := []*htmlNode{root}
	i := 0
	for i < len(s) {
		if s[i] != '<' {
			j := strings.IndexByte(s[i:], '<')
			if j < 0 {
				j = len(s) - i
			}
			addTextNode(stack[len(stack)-1], s[i:i+j])
			i += j
			continue
		}
		if strings.HasPrefix(s[i:], "<!--") {
			end := strings.Index(s[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		if strings.HasPrefix(s[i:], "<![CDATA[") {
			end := strings.Index(s[i+9:], "]]>")
			if end < 0 {
				break
			}
			addTextNode(stack[len(stack)-1], s[i+9:i+9+end])
			i += 9 + end + 3
			continue
		}
		if strings.HasPrefix(s[i:], "<!") {
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				break
			}
			i += end + 1
			continue
		}
		if strings.HasPrefix(s[i:], "</") {
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				break
			}
			name := strings.ToLower(strings.TrimSpace(s[i+2 : i+end]))
			for k := len(stack) - 1; k > 0; k-- {
				if stack[k].tag == name {
					stack = stack[:k]
					break
				}
			}
			i += end + 1
			continue
		}

		// 起始标签: 找 '>' 时尊重引号, 避免属性值里的 '>' 提前截断。
		j := i + 1
		var quote byte
		for j < len(s) {
			c := s[j]
			if quote != 0 {
				if c == quote {
					quote = 0
				}
			} else if c == '"' || c == '\'' {
				quote = c
			} else if c == '>' {
				break
			}
			j++
		}
		if j >= len(s) {
			break
		}
		inner := s[i+1 : j]
		i = j + 1

		selfClose := strings.HasSuffix(strings.TrimSpace(inner), "/")
		inner = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(inner), "/"))
		tag, attrs := parseTag(inner)
		if tag == "" {
			continue
		}

		// 常见省略闭合标签的自动收口: 必须在挂新节点之前弹栈。
		if !voidTags[tag] && !selfClose {
			if tag == "p" && len(stack) > 1 && stack[len(stack)-1].tag == "p" {
				stack = stack[:len(stack)-1]
			}
			if tag == "li" {
				for len(stack) > 1 && stack[len(stack)-1].tag == "li" {
					stack = stack[:len(stack)-1]
				}
			}
		}

		node := &htmlNode{tag: tag, attrs: attrs, parent: stack[len(stack)-1]}
		parent := stack[len(stack)-1]
		parent.children = append(parent.children, node)

		if voidTags[tag] || selfClose {
			continue
		}
		stack = append(stack, node)

		// script/style/title/textarea 的内容按原始文本读到闭合标签。
		if tag == "script" || tag == "style" || tag == "title" || tag == "textarea" {
			lowerRest := strings.ToLower(s[i:])
			closeTag := "</" + tag
			k := strings.Index(lowerRest, closeTag)
			var content string
			if k < 0 {
				content = s[i:]
				i = len(s)
			} else {
				content = s[i : i+k]
				rest := s[i+k:]
				end := strings.IndexByte(rest, '>')
				if end < 0 {
					i = len(s)
				} else {
					i += k + end + 1
				}
			}
			if content != "" {
				addTextNode(node, content)
			}
			stack = stack[:len(stack)-1]
			continue
		}
	}
	return root
}

// addTextNode 追加文本子节点。
func addTextNode(parent *htmlNode, text string) {
	if parent == nil || text == "" {
		return
	}
	parent.children = append(parent.children, &htmlNode{text: text, parent: parent})
}

// parseTag 解析标签名与属性。
func parseTag(inner string) (string, map[string]string) {
	i := 0
	for i < len(inner) && isSpaceByte(inner[i]) {
		i++
	}
	start := i
	for i < len(inner) && isTagNameByte(inner[i]) {
		i++
	}
	tag := strings.ToLower(inner[start:i])
	attrs := make(map[string]string)
	for i < len(inner) {
		for i < len(inner) && isSpaceByte(inner[i]) {
			i++
		}
		if i < len(inner) && inner[i] == '/' {
			i++
			continue
		}
		nameStart := i
		for i < len(inner) && !isSpaceByte(inner[i]) && inner[i] != '=' && inner[i] != '/' {
			i++
		}
		name := strings.ToLower(inner[nameStart:i])
		for i < len(inner) && isSpaceByte(inner[i]) {
			i++
		}
		value := ""
		if i < len(inner) && inner[i] == '=' {
			i++
			for i < len(inner) && isSpaceByte(inner[i]) {
				i++
			}
			if i < len(inner) && (inner[i] == '"' || inner[i] == '\'') {
				q := inner[i]
				i++
				vs := i
				for i < len(inner) && inner[i] != q {
					i++
				}
				value = inner[vs:i]
				if i < len(inner) {
					i++
				}
			} else {
				vs := i
				for i < len(inner) && !isSpaceByte(inner[i]) {
					i++
				}
				value = inner[vs:i]
			}
			value = stdhtml.UnescapeString(value)
		}
		if name != "" {
			if _, exists := attrs[name]; !exists {
				attrs[name] = value
			}
		}
	}
	return tag, attrs
}

// isSpaceByte 判断 ASCII 空白。
func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// isTagNameByte 判断标签名字符。
func isTagNameByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '-' || c == '_' || c == ':'
}

// walkNodes 先序遍历节点树。
func walkNodes(n *htmlNode, fn func(*htmlNode)) {
	if n == nil {
		return
	}
	fn(n)
	for _, c := range n.children {
		walkNodes(c, fn)
	}
}

// nodeDepth 返回节点深度, 根为 0。
func nodeDepth(n *htmlNode) int {
	d := 0
	for p := n.parent; p != nil; p = p.parent {
		d++
	}
	return d
}

// skipNode 判断节点是否属于噪声, 正文提取与统计都会跳过。
func skipNode(n *htmlNode) bool {
	if n == nil || n.tag == "" {
		return false
	}
	switch n.tag {
	case "script", "style", "noscript", "iframe", "svg", "canvas", "form",
		"button", "select", "option", "input", "nav", "footer", "link", "meta":
		return true
	}
	marker := strings.ToLower(n.attrs["class"] + " " + n.attrs["id"] + " " + n.attrs["role"])
	if marker == " " {
		return false
	}
	for _, token := range []string{
		"cookie", "consent", "gdpr", "newsletter", "subscribe", "advertisement",
		"advert", "banner", "popup", "modal", "breadcrumb", "share-bar",
		"social-share", "related-posts",
	} {
		if strings.Contains(marker, token) {
			return true
		}
	}
	for _, field := range strings.Fields(marker) {
		if field == "ad" || field == "ads" || strings.HasPrefix(field, "ad-") || strings.HasPrefix(field, "ads-") {
			return true
		}
	}
	return false
}

// rawText 返回节点下全部原始文本 (保留空白, 解码实体)。
func rawText(n *htmlNode) string {
	if n == nil {
		return ""
	}
	if n.tag == "" {
		return stdhtml.UnescapeString(n.text)
	}
	var b strings.Builder
	for _, c := range n.children {
		b.WriteString(rawText(c))
	}
	return b.String()
}

// collapseInline 压缩空白但保留首尾单个空格, 防止相邻行内标签粘连。
func collapseInline(s string) string {
	if s == "" {
		return ""
	}
	lead := isSpaceByte(s[0])
	trail := isSpaceByte(s[len(s)-1])
	core := strings.Join(strings.Fields(s), " ")
	if core == "" {
		if lead || trail {
			return " "
		}
		return ""
	}
	if lead {
		core = " " + core
	}
	if trail {
		core += " "
	}
	return core
}

// nodeTextStats 统计节点可读文本量与其中链接文本量。
func nodeTextStats(n *htmlNode, inLink bool) (text, link int) {
	if n == nil {
		return 0, 0
	}
	if n.tag != "" && n.tag != "#root" && skipNode(n) {
		return 0, 0
	}
	if n.tag == "" {
		count := utf8.RuneCountInString(collapseInline(stdhtml.UnescapeString(n.text)))
		if inLink {
			return 0, count
		}
		return count, 0
	}
	isLink := inLink || n.tag == "a"
	for _, c := range n.children {
		a, b := nodeTextStats(c, isLink)
		text += a
		link += b
	}
	return text, link
}

// pickContentRoot 选择正文容器。
//
// 优先 <article>/<main>; 否则在 article/main/div/section/body 里按
// "文本量 - 2×链接文本量" 选分数最高者, 同分取更深节点, 避免选到包住
// 整页的外层 wrapper。
func pickContentRoot(root *htmlNode) *htmlNode {
	var best, article *htmlNode
	bestScore, articleScore := -1.0, -1.0
	bestDepth, articleDepth := -1, -1

	walkNodes(root, func(n *htmlNode) {
		switch n.tag {
		case "article", "main", "div", "section", "body":
		default:
			return
		}
		text, link := nodeTextStats(n, false)
		if text < minContentRunes {
			return
		}
		score := float64(text - 2*link)
		if score <= 0 {
			score = float64(text) * 0.01
		}
		depth := nodeDepth(n)
		if score > bestScore || (score == bestScore && depth > bestDepth) {
			best, bestScore, bestDepth = n, score, depth
		}
		if n.tag == "article" || n.tag == "main" {
			if score > articleScore || (score == articleScore && depth > articleDepth) {
				article, articleScore, articleDepth = n, score, depth
			}
		}
	})

	if article != nil && (best == nil || articleScore >= bestScore*0.4) {
		return article
	}
	if best != nil {
		return best
	}
	return root
}

// mdBuf 是带尾部换行计数的 Markdown 缓冲, 让 ensureBlank 为 O(1)。
type mdBuf struct {
	b       strings.Builder
	trailNL int
}

// write 追加字符串并维护尾部换行数。
func (m *mdBuf) write(s string) {
	if s == "" {
		return
	}
	m.b.WriteString(s)
	if s[len(s)-1] != '\n' {
		m.trailNL = 0
		return
	}
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\n'; i-- {
		n++
	}
	if n == len(s) {
		m.trailNL += n
	} else {
		m.trailNL = n
	}
	if m.trailNL > 3 {
		m.trailNL = 3
	}
}

// writeByte 追加单字节。
func (m *mdBuf) writeByte(c byte) {
	m.b.WriteByte(c)
	if c == '\n' {
		m.trailNL++
		if m.trailNL > 3 {
			m.trailNL = 3
		}
	} else {
		m.trailNL = 0
	}
}

// ensureBlank 保证当前输出至少有一个空行。
func (m *mdBuf) ensureBlank() {
	if m.b.Len() == 0 {
		return
	}
	if m.trailNL == 0 {
		m.write("\n\n")
	} else if m.trailNL == 1 {
		m.write("\n")
	}
}

// String 返回缓冲内容。
func (m *mdBuf) String() string { return m.b.String() }

// renderMarkdown 把节点树渲染成 Markdown 化纯文本。
func renderMarkdown(root *htmlNode) string {
	var b mdBuf
	if root == nil {
		return ""
	}
	if root.tag != "#root" && root.tag != "" {
		renderBlock(root, &b, 0)
	} else {
		for _, c := range root.children {
			renderBlock(c, &b, 0)
		}
	}
	return normalizeMarkdown(b.String())
}

// renderChildren 把 n 的子节点渲染成 Markdown, 供需要"把子树当独立文档"的
// 块级元素 (目前是 blockquote) 使用。
//
// 不能直接调用 renderMarkdown(n): 后者对非根节点会先 renderBlock(n) 一次,
// 于是 blockquote 会再次进入本分支 —— 页面里只要有一个 <blockquote>,
// WebFetch 就会递归到栈溢出 (整个进程 fatal, 不是返回错误)。
func renderChildren(n *htmlNode) string {
	var b mdBuf
	if n == nil {
		return ""
	}
	for _, c := range n.children {
		renderBlock(c, &b, 0)
	}
	return normalizeMarkdown(b.String())
}

// renderBlock 渲染块级节点。
func renderBlock(n *htmlNode, b *mdBuf, depth int) {
	if n == nil {
		return
	}
	if n.tag != "" && n.tag != "#root" && skipNode(n) {
		return
	}
	if n.tag == "" {
		s := collapseInline(stdhtml.UnescapeString(n.text))
		if s != "" && s != " " {
			b.write(s)
		}
		return
	}

	switch n.tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(n.tag[1] - '0')
		b.ensureBlank()
		b.write(strings.Repeat("#", level))
		b.writeByte(' ')
		b.write(strings.TrimSpace(inlineText(n)))
		b.ensureBlank()
	case "p":
		b.ensureBlank()
		b.write(strings.TrimSpace(inlineText(n)))
		b.ensureBlank()
	case "pre":
		b.ensureBlank()
		b.write("```")
		b.write(codeLanguage(n))
		b.writeByte('\n')
		b.write(strings.TrimSpace(rawText(n)))
		b.writeByte('\n')
		b.write("```")
		b.ensureBlank()
	case "blockquote":
		inner := renderChildren(n)
		b.ensureBlank()
		for _, line := range strings.Split(inner, "\n") {
			b.write("> ")
			b.write(line)
			b.writeByte('\n')
		}
		b.ensureBlank()
	case "ul", "ol":
		b.ensureBlank()
		renderList(n, b, depth)
		b.ensureBlank()
	case "li":
		b.ensureBlank()
		b.write("- ")
		b.write(strings.TrimSpace(inlineText(n)))
		b.ensureBlank()
	case "br":
		b.writeByte('\n')
	case "hr":
		b.ensureBlank()
		b.write("---")
		b.ensureBlank()
	case "table":
		renderTable(n, b)
	case "div", "section", "article", "main", "body", "header", "aside", "figure", "figcaption", "#root":
		for _, c := range n.children {
			renderBlock(c, b, depth)
		}
	default:
		if isInlineTag(n.tag) {
			b.write(inlineText(n))
			return
		}
		for _, c := range n.children {
			renderBlock(c, b, depth)
		}
	}
}

// renderList 渲染 ul/ol; 嵌套列表按两层空格缩进。
func renderList(n *htmlNode, b *mdBuf, depth int) {
	ordered := n.tag == "ol"
	index := 1
	for _, c := range n.children {
		if c.tag != "li" {
			continue
		}
		indent := strings.Repeat("  ", depth)
		b.write(indent)
		if ordered {
			b.write(strconv.Itoa(index))
			b.write(". ")
			index++
		} else {
			b.write("- ")
		}
		for _, cc := range c.children {
			if cc.tag == "ul" || cc.tag == "ol" {
				continue
			}
			b.write(strings.TrimSpace(inlineText(cc)))
		}
		b.writeByte('\n')
		for _, cc := range c.children {
			if cc.tag == "ul" || cc.tag == "ol" {
				renderList(cc, b, depth+1)
			}
		}
	}
}

// renderTable 渲染简化表格, 单元格用 " | " 分隔。
func renderTable(n *htmlNode, b *mdBuf) {
	b.ensureBlank()
	walkNodes(n, func(row *htmlNode) {
		if row.tag != "tr" {
			return
		}
		var cells []string
		for _, cell := range row.children {
			if cell.tag != "td" && cell.tag != "th" {
				continue
			}
			cells = append(cells, strings.TrimSpace(inlineText(cell)))
		}
		if len(cells) == 0 {
			return
		}
		b.write("| ")
		b.write(strings.Join(cells, " | "))
		b.write(" |")
		b.writeByte('\n')
	})
	b.ensureBlank()
}

// inlineText 收集行内文本; 块级子节点退化为空格。
func inlineText(n *htmlNode) string {
	if n == nil {
		return ""
	}
	if n.tag != "" && skipNode(n) {
		return ""
	}
	if n.tag == "" {
		return collapseInline(stdhtml.UnescapeString(n.text))
	}
	switch n.tag {
	case "br":
		return "\n"
	case "img":
		return strings.TrimSpace(n.attrs["alt"])
	case "code":
		if raw := strings.TrimSpace(rawText(n)); raw != "" {
			return "`" + raw + "`"
		}
		return ""
	}
	var b strings.Builder
	for _, c := range n.children {
		if c.tag != "" && isBlockTag(c.tag) {
			b.WriteByte(' ')
			continue
		}
		b.WriteString(inlineText(c))
	}
	return b.String()
}

// isInlineTag 判断常见的行内标签。
func isInlineTag(tag string) bool {
	switch tag {
	case "a", "span", "strong", "b", "em", "i", "u", "code", "small",
		"sub", "sup", "mark", "abbr", "time", "label", "img", "br", "font":
		return true
	default:
		return false
	}
}

// isBlockTag 判断常见的块级标签。
func isBlockTag(tag string) bool {
	switch tag {
	case "p", "div", "section", "article", "main", "header", "footer",
		"ul", "ol", "li", "pre", "blockquote", "table", "tr", "td", "th",
		"h1", "h2", "h3", "h4", "h5", "h6", "hr", "br", "figure",
		"figcaption", "dl", "dt", "dd", "address":
		return true
	default:
		return false
	}
}

// codeLanguage 从 class="language-go" / "lang-go" 提取代码块语言。
func codeLanguage(n *htmlNode) string {
	classes := n.attrs["class"]
	for _, c := range n.children {
		if c.tag == "code" {
			classes += " " + c.attrs["class"]
		}
	}
	for _, field := range strings.Fields(classes) {
		lower := strings.ToLower(field)
		for _, prefix := range []string{"language-", "lang-"} {
			if strings.HasPrefix(lower, prefix) {
				lang := strings.TrimPrefix(lower, prefix)
				if lang != "" && isSafeToken(lang) {
					return lang
				}
			}
		}
	}
	return ""
}

// isSafeToken 限制代码语言只能含字母/数字/加号/井号/减号。
func isSafeToken(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' && r != '#' && r != '-' {
			return false
		}
	}
	return true
}

// normalizeMarkdown 统一换行、压缩空白并去掉多余空行。
func normalizeMarkdown(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	blank := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			line = strings.TrimRight(line, " \t")
			blank = false
			out = append(out, line)
			continue
		}
		if inFence {
			out = append(out, strings.TrimRight(line, " \t"))
			continue
		}
		line = collapseSpacesLine(strings.TrimRight(line, " \t"))
		if strings.TrimSpace(line) == "" {
			if len(out) == 0 || blank {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// collapseSpacesLine 把行内连续空格压成一个, 保留行首缩进。
func collapseSpacesLine(line string) string {
	if line == "" {
		return ""
	}
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	indent := line[:i]
	rest := line[i:]
	var b strings.Builder
	space := false
	for _, r := range rest {
		if r == ' ' || r == '\t' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return indent + b.String()
}

// ---------------------------------------------------------------------------
// 元数据提取
// ---------------------------------------------------------------------------

// pageMeta 收集标题/作者/发布时间, 用优先级解决 meta / title / JSON-LD 冲突。
type pageMeta struct {
	title      string
	titlePri   int
	byline     string
	bylinePri  int
	published  time.Time
	publishedP int
}

// setTitle 按优先级写入标题。
func (m *pageMeta) setTitle(v string, pri int) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	if m.title == "" || pri < m.titlePri {
		m.title, m.titlePri = v, pri
	}
}

// setByline 按优先级写入作者。
func (m *pageMeta) setByline(v string, pri int) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	if m.byline == "" || pri < m.bylinePri {
		m.byline, m.bylinePri = v, pri
	}
}

// setPublished 按优先级写入发布时间。
func (m *pageMeta) setPublished(v time.Time, pri int) {
	if v.IsZero() {
		return
	}
	if m.published.IsZero() || pri < m.publishedP {
		m.published, m.publishedP = v.UTC(), pri
	}
}

// collectHTMLMeta 从 meta/time/title/JSON-LD 提取元数据。
func collectHTMLMeta(root *htmlNode) pageMeta {
	var m pageMeta
	var h1 string
	var timeAttr string
	var jsonld []string

	walkNodes(root, func(n *htmlNode) {
		switch n.tag {
		case "title":
			m.setTitle(rawText(n), 2)
		case "h1":
			if h1 == "" {
				h1 = strings.TrimSpace(collapseInline(rawText(n)))
			}
		case "time":
			if timeAttr == "" {
				timeAttr = n.attrs["datetime"]
			}
		case "meta":
			key := strings.ToLower(strings.TrimSpace(firstNonEmpty(
				n.attrs["property"], n.attrs["name"], n.attrs["itemprop"],
			)))
			content := strings.TrimSpace(n.attrs["content"])
			switch key {
			case "og:title", "twitter:title":
				m.setTitle(content, 1)
			case "author", "article:author", "twitter:creator":
				m.setByline(content, 1)
			case "article:published_time", "datepublished", "pubdate", "date",
				"article:modified_time", "og:updated_time":
				if t, ok := parsePublished(content); ok {
					m.setPublished(t, 1)
				}
			}
		case "script":
			if strings.EqualFold(strings.TrimSpace(n.attrs["type"]), "application/ld+json") {
				jsonld = append(jsonld, rawText(n))
			}
		}
	})

	if h1 != "" {
		m.setTitle(h1, 3)
	}
	for _, raw := range jsonld {
		parseJSONLD(raw, &m)
	}
	if m.published.IsZero() && timeAttr != "" {
		if t, ok := parsePublished(timeAttr); ok {
			m.setPublished(t, 2)
		}
	}
	return m
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

// parseJSONLD 解析一段 JSON-LD, 递归寻找 headline/author/datePublished。
func parseJSONLD(raw string, m *pageMeta) {
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return
	}
	walkJSONLD(v, m, 0)
}

// walkJSONLD 递归遍历 JSON-LD 值。
func walkJSONLD(v any, m *pageMeta, depth int) {
	if depth > 8 {
		return
	}
	switch x := v.(type) {
	case map[string]any:
		if s, ok := x["headline"].(string); ok {
			m.setTitle(s, 1)
		}
		if s, ok := x["datePublished"].(string); ok {
			if t, ok2 := parsePublished(s); ok2 {
				m.setPublished(t, 1)
			}
		}
		if a, ok := x["author"]; ok {
			if name := jsonAuthorName(a); name != "" {
				m.setByline(name, 2)
			}
		}
		for _, val := range x {
			walkJSONLD(val, m, depth+1)
		}
	case []any:
		for _, item := range x {
			walkJSONLD(item, m, depth+1)
		}
	}
}

// jsonAuthorName 从 JSON-LD author 字段取名字。
func jsonAuthorName(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
		if s, ok := x["name"].(string); ok {
			return strings.TrimSpace(s)
		}
	case []any:
		for _, item := range x {
			if name := jsonAuthorName(item); name != "" {
				return name
			}
		}
	}
	return ""
}

// parsePublished 兼容常见发布时间格式。
func parsePublished(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 100_000_000 && n < 100_000_000_000 {
		return time.Unix(n, 0).UTC(), true
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05-0700",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2006/01/02",
		"01/02/2006",
		time.RFC1123Z,
		time.RFC1123,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
