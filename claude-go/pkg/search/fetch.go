package search

// fetch.go —— 单页抓取、正文抽取与字符集处理。
//
// 抓取前先做 URL 校验 (只允许 http/https, 解析 DNS 后拒绝私网/环回/链路本地
// 地址), 防止 WebFetch 被当成 SSRF 跳板; 响应按 Content-Type 选择 HTML 抽取
// 或纯文本, 字符集从 header/meta 探测后转 UTF-8。网络层保留规范化全文，
// prompt 选段、max_content_tokens 和 offset 截断统一在缓存之后执行。
//
// HTML 抽取不引第三方 DOM 依赖, 用一个小型容错解析器挑正文根节点, 再转成
// 带标题/列表/链接的 Markdown; 解析失败时退回原始文本而不是报错。
import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
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

// Fetch 抓取一个 URL，并缓存规范化全文。
//
// 网络层仍执行初始 URL 与每跳重定向的 SSRF 校验、5MB 上限和出口降级；
// prompt 选段、token 预算和 continuation 都在全文缓存之后执行，避免同一页面
// 因不同问题重复打源。临时故障可复用过期全文，但必须显式标记 stale。
func (m *Manager) Fetch(ctx context.Context, req FetchRequest) (resp *FetchResponse, err error) {
	start := time.Now()
	// 使用记录挂在出口处: 所有 return (缓存命中/失败/成功) 都会走这里,
	// 不存在"某个分支忘了记"的分叉。
	defer func() { m.recordFetch(req, resp, err, time.Since(start)) }()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, Wrap("", classifyError(err), err)
	}
	if m.isClosed() {
		return nil, Wrap("", ErrUnavailable, errors.New("search manager closed"))
	}
	if err := validateFetchRequest(req); err != nil {
		return nil, err
	}

	u, err := urlGuard(req.URL)
	if err != nil {
		return nil, err
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	key := fetchCacheKey(u.String())
	cached, cacheHit, cacheDegraded := m.cachedDocument(ctx, key)
	if cacheHit {
		doc, degraded, err := prepareDocument(cached, req)
		if err != nil {
			return nil, err
		}
		doc.CacheHit = true
		return &FetchResponse{Document: doc, Degraded: degraded, ElapsedMS: time.Since(start).Milliseconds()}, nil
	}

	result, err, shared := m.fetchFlight.Do(ctx, key, func() (fetchFlightResult, error) {
		cached, ok, flightDegraded := m.cachedDocument(ctx, key)
		if ok {
			return fetchFlightResult{doc: cached, cacheHit: true}, nil
		}
		deadline := req.Deadline
		if deadline.IsZero() {
			timeout := m.cfg.FetchTimeout
			if timeout <= 0 {
				timeout = 8 * time.Second
			}
			deadline = time.Now().Add(timeout)
		}
		fctx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()

		modes := []Mode{ModeDirect}
		if m.egress != nil && m.egress.HasProxy() {
			modes = append(modes, ModeProxy)
		}
		var lastErr error
		var doc *Document
		for i, mode := range modes {
			if err := fctx.Err(); err != nil {
				if lastErr == nil {
					lastErr = Wrap("fetch", classifyError(err), err)
				}
				break
			}
			client := m.egress.Client(mode)
			m.metrics.Inc("egress_requests_total", map[string]string{"provider": "fetch", "egress": string(mode)})
			doc, err = m.fetchWithClient(fctx, client, u)
			// arXiv 2026 起把列表页从 YYMM 改成 YYYY-MM (/list/cs.CL/2611 现在
			// 404, /list/cs.CL/2026-11 才是有效地址)。模型和旧链接仍会给出旧格式,
			// 只在 404 时按这一种形状修一次, 不做通用 URL 猜测。
			if err != nil && KindOf(err) == ErrNotFound {
				if fixed, ok := repairArxivListURL(u); ok {
					if rdoc, rerr := m.fetchWithClient(fctx, client, fixed); rerr == nil {
						rdoc.URL = fixed.String()
						doc, err = rdoc, nil
						flightDegraded = append(flightDegraded, "url_repaired:arxiv_list")
						m.metrics.Inc("fetch_url_repaired_total", map[string]string{"kind": "arxiv_list"})
					}
				}
			}
			if err == nil {
				if i > 0 {
					m.metrics.Inc("egress_fallback_total", map[string]string{"from": "direct", "to": "proxy", "reason": "error"})
				}
				break
			}
			lastErr = err
			// ErrInvalid 是请求本身非法 (SSRF/协议), ErrNotFound 是资源
			// 已死 —— 换出口重试都没有意义, 交给后面的归档/降级分支。
			if k := KindOf(err); k == ErrInvalid || k == ErrNotFound {
				break
			}
		}
		if doc == nil && m.cfg.WaybackFallback && waybackRetryable(lastErr) {
			// 原站已死/超时/拒爬时回退 Wayback 最近快照: 深网检索里大量
			// 一手材料只在归档里还活着 (旧公告/下线文档/被墙页面)。快照
			// 内容按原 URL 缓存进同一 key, 后续命中不再打归档。
			if wdoc, werr := m.fetchWayback(fctx, u, modes); werr == nil {
				doc = wdoc
				flightDegraded = append(flightDegraded, "wayback_fallback:"+string(KindOf(lastErr)))
				m.metrics.Inc("fetch_wayback_fallback_total", map[string]string{"kind": string(KindOf(lastErr))})
			} else {
				flightDegraded = append(flightDegraded, "wayback_fallback_failed:"+string(KindOf(werr)))
			}
		}
		if doc == nil {
			// 只对临时故障使用 stale；SSRF、协议、403 和内容类型错误绝不绕过。
			switch KindOf(lastErr) {
			case ErrTimeout, ErrUnavailable, ErrRateLimited:
				// 网络超时后 fctx 已取消，但 stale 读取是受 Redis 客户端超时
				// 约束的独立短操作，不能因为原请求超时而失去降级机会。
				stale, ok, degraded := m.staleDocument(context.WithoutCancel(fctx), key)
				flightDegraded = append(flightDegraded, degraded...)
				if ok {
					stale.Stale = true
					return fetchFlightResult{
						doc: stale, cacheHit: true,
						degraded: append(flightDegraded, "stale_if_error:"+string(KindOf(lastErr))),
					}, nil
				}
			}
			return fetchFlightResult{}, lastErr
		}
		flightDegraded = append(flightDegraded, m.putDocumentCache(fctx, key, doc, m.cfg.CacheTTL)...)
		m.metrics.Inc("fetch_success_total", map[string]string{"via": doc.Via})
		return fetchFlightResult{doc: doc, degraded: flightDegraded}, nil
	})
	if err != nil {
		return &FetchResponse{ElapsedMS: time.Since(start).Milliseconds()}, err
	}
	doc, degraded, err := prepareDocument(result.doc, req)
	if err != nil {
		return nil, err
	}
	doc.CacheHit = result.cacheHit || shared
	degraded = append(degraded, cacheDegraded...)
	degraded = append(degraded, result.degraded...)
	return &FetchResponse{Document: doc, Degraded: dedupeStrings(degraded), ElapsedMS: time.Since(start).Milliseconds()}, nil
}

// BrowserStyleUA 是发送给"拒爬虫 UA 但匿名直读公开内容"站点的浏览器形态
// UA —— 与 providers 包 douban/weibo 同一口径: 保留 claude-go-search 身份
// 标识, 不伪装成真实浏览器, 不做轮换。使用方两处:
//   - fetch 侧 browserUAHosts 命中的域名 (目前只有公众号文章域);
//   - providers 包构造期钉给对应源。
const BrowserStyleUA = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 claude-go-search/1.0"

// browserUAHosts 列出抓取时必须用浏览器形态 UA 的域名。这些站对爬虫 UA
// 直接拒绝 (公众号文章 2026-11-15 实测: 爬虫 UA 拿不到正文, 匿名桌面 UA
// 可直读 25k 字全文), 但内容本身匿名公开、无挑战页 —— 不是反爬绕过。
var browserUAHosts = map[string]bool{
	"mp.weixin.qq.com": true,
}

// fetchUserAgent 按目标 host 选择 UA。命中 browserUAHosts 用浏览器形态,
// 其余保持 cfg 里配置的诚实爬虫 UA (带项目地址)。
func fetchUserAgent(cfgUserAgent, host string) string {
	if browserUAHosts[host] {
		return BrowserStyleUA
	}
	return cfgUserAgent
}

// fetchWithClient 用指定客户端执行一次抓取与正文提取。
func (m *Manager) fetchWithClient(ctx context.Context, client *http.Client, u *url.URL) (*Document, error) {
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
	req.Header.Set("User-Agent", fetchUserAgent(m.cfg.UserAgent, u.Hostname()))
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf,text/plain;q=0.9,*/*;q=0.1")

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
	// application/*+json (hal+json / ld+json / vnd.github+json ...) 与裸 JSON
	// 同等对待, 走同一个代码块分支。
	if strings.HasSuffix(mediaType, "+json") {
		mediaType = "application/json"
	}
	// PDF 必须先于 NUL/字符集处理分流。部分服务器把 PDF 错标为
	// application/octet-stream，因此同时接受 Content-Type 与文件 magic。
	if mediaType == "application/pdf" || bytes.HasPrefix(body, []byte("%PDF-")) {
		return ExtractPDF(ctx, resp.Request.URL.String(), body)
	}
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
		if needsJSRender(text, markdown) {
			// 静态响应已经完成逐跳 SSRF 校验；渲染从最终 URL 开始，并沿用
			// 本次选定的 direct/proxy client，而不是另开一条隐式直连。
			return m.renderJS(ctx, client, resp.Request.URL.String())
		}
	case "text/plain", "text/markdown":
		markdown = normalizeMarkdown(text)
		title = firstLine(markdown)
	case "application/atom+xml", "application/rss+xml", "application/rdf+xml",
		"text/xml", "application/xml":
		// 学术站点大量用 feed 输出列表 (arXiv listing、期刊 TOC、会议通知),
		// 之前落在 default 被判 unsupported。feed 结构化成标题+条目链接清单,
		// 比裸 XML 文本对检索/阅读都更有用。
		feed, ferr := parseFeedXML(text)
		if ferr == nil && (feed.title() != "" || len(feed.items()) > 0) {
			title = feed.title()
			items := feed.items()
			markdown = feedMarkdown(items)
			if len(items) > 0 && !items[0].published().IsZero() {
				published = items[0].published()
			}
		} else if sm, serr := parseSitemapXML(text); serr == nil && len(sm.entries()) > 0 {
			// 不是 feed 还可能是 sitemap: 模型抓 sitemap 是为了发现站点页面
			// (博客归档/文档目录), 渲染成可顺藤抓取的链接清单, 比一句
			// unsupported 更能引导下一步 (2026-11-26 实测 doris/duckdb
			// sitemap.xml 两次撞 "unparseable feed")。
			title = fmt.Sprintf("Sitemap %s (%d URLs)", u.Host, len(sm.entries()))
			markdown = sitemapMarkdown(sm)
		} else {
			return nil, Wrap("fetch", ErrUnsupported, fmt.Errorf("unparseable feed %q", mediaType))
		}
	case "application/json":
		// REST API 响应 (api.github.com 的 releases/commits 等) 是查版本与
		// 元数据的一手来源, 之前落在 default 判 unsupported —— agent 只能
		// 绕路猜 HTML 页, 猜错就是一次臆造 URL 404。JSON 不做正文提取,
		// 规整后包进代码块; 体量超上限的部分截断。
		markdown = jsonMarkdown(text)
	default:
		return nil, Wrap("fetch", ErrUnsupported, fmt.Errorf("unsupported content type %q", mediaType))
	}

	markdown = strings.TrimSpace(markdown)
	doc := &Document{
		URL:       resp.Request.URL.String(),
		Title:     title,
		Markdown:  markdown,
		Byline:    byline,
		Published: published,
		FetchedAt: time.Now(),
		Hash:      hashDocument(markdown),
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
	case code == http.StatusNotFound || code == http.StatusGone:
		// 资源已死 (不是请求非法): 不配用旧缓存伪装成功, 但值得回退
		// Wayback 归档快照 —— 与 SSRF/协议类的 ErrInvalid 区分开。
		return ErrNotFound
	case code >= 400 && code < 500:
		// 其它 4xx 是永久性的目标/权限/资源错误，不允许用旧缓存伪装本次成功。
		return ErrInvalid
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

// feedDoc/feedEntry 以 union 结构同时兼容 Atom (feed/entry) 与 RSS
// (rss/channel/item) —— 两者字段名不同但语义一一对应, 一次解码全取。
type feedDoc struct {
	Title   string      `xml:"title"`
	Entries []feedEntry `xml:"entry"`
	Channel struct {
		Title string      `xml:"title"`
		Items []feedEntry `xml:"item"`
	} `xml:"channel"`
}

// feedEntry 是一条 Atom entry 或 RSS item。Summary/Content/Desc 三选一,
// 谁非空用谁; 时间字段同理 (Atom 用 published/updated, RSS 用 pubDate)。
type feedEntry struct {
	Title string `xml:"title"`
	Link  struct {
		Href string `xml:"href,attr"` // Atom: <link href="..."/>
		Text string `xml:",chardata"` // RSS: <link>http://...</link>
	} `xml:"link"`
	Summary   string `xml:"summary"`
	Content   string `xml:"content"`
	Desc      string `xml:"description"`
	Published string `xml:"published"`
	Updated   string `xml:"updated"`
	PubDate   string `xml:"pubDate"`
}

// published 返回条目时间 (按 Atom→RSS 字段序取第一个能解析的)。
func (e feedEntry) published() time.Time {
	return parseFeedTime(firstNonEmpty(e.Published, e.Updated, e.PubDate))
}

// feedLink 返回条目链接 (Atom 属性优先, RSS 文本兜底)。
func (e feedEntry) link() string {
	return strings.TrimSpace(firstNonEmpty(e.Link.Href, e.Link.Text))
}

func (d *feedDoc) title() string {
	return strings.TrimSpace(firstNonEmpty(d.Title, d.Channel.Title))
}

// items 返回条目列表 (Atom 的 entry 或 RSS channel 的 item)。
func (d *feedDoc) items() []feedEntry {
	if len(d.Entries) > 0 {
		return d.Entries
	}
	return d.Channel.Items
}

// parseFeedTime 尝试常见 feed 日期格式; 全部失败返回零值 (调用方降级展示原文)。
func parseFeedTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
		time.RFC1123Z, // RSS pubDate: Mon, 02 Jan 2006 15:04:05 -0700
		time.RFC1123,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseFeedXML 解码 Atom/RSS feed, 容忍未知扩展标签 (encoding/xml 跳过)。
func parseFeedXML(raw string) (*feedDoc, error) {
	var doc feedDoc
	if err := xml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// maxJSONDocChars 限制包进文档的 JSON 文本量: API 响应可能整包 MB 级,
// 全量进 markdown 会撑爆全文缓存与下游摘录窗口。
const maxJSONDocChars = 200_000

// jsonMarkdown 把 JSON 响应渲染成文档正文: 能解析就重新缩进 (便于阅读与
// 摘录窗口定位), 不能解析 (JSONP/ndjson) 保留原文; 超上限截断并在代码块
// 外注明。
func jsonMarkdown(text string) string {
	trimmed := strings.TrimSpace(text)
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(trimmed), "", "  "); err == nil && buf.Len() <= maxJSONDocChars {
		trimmed = buf.String()
	}
	truncated := false
	if len(trimmed) > maxJSONDocChars {
		trimmed = trimmed[:maxJSONDocChars]
		truncated = true
	}
	var sb strings.Builder
	sb.WriteString("```json\n")
	sb.WriteString(trimmed)
	sb.WriteString("\n```")
	if truncated {
		sb.WriteString("\n\n[JSON 已截断: 原始响应超过 ")
		sb.WriteString(strconv.Itoa(maxJSONDocChars))
		sb.WriteString(" 字符]")
	}
	return sb.String()
}

// feedMarkdown 把 feed 条目渲染成清单式 Markdown: 每条 = 链接标题 + 日期 +
// 摘要正文 (摘要是 HTML 片段, 走 extractHTML 转纯文本)。没有可展示内容的
// 条目直接跳过, 全空时返回空串由调用方判 unsupported。
func feedMarkdown(items []feedEntry) string {
	var sb strings.Builder
	for _, it := range items {
		title := strings.TrimSpace(it.Title)
		link := it.link()
		if title == "" && link == "" {
			continue
		}
		head := title
		if head == "" {
			head = link
		}
		if link != "" && title != "" {
			head = "[" + title + "](" + link + ")"
		}
		sb.WriteString("- ")
		sb.WriteString(head)
		if t := it.published(); !t.IsZero() {
			sb.WriteString(" — ")
			sb.WriteString(t.Format("2006-01-02"))
		}
		if body := strings.TrimSpace(firstNonEmpty(it.Summary, it.Content, it.Desc)); body != "" {
			if md := strings.TrimSpace(extractHTML(body).Markdown); md != "" {
				sb.WriteString("\n  ")
				sb.WriteString(strings.ReplaceAll(md, "\n", "\n  "))
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// maxSitemapEntries 限制渲染进文档的 sitemap 条目数: 全量 sitemap 可达数万条,
// 保留头部 (站点一般按栏目/更新聚簇排序, 头部对"发现页面"最有用), 截断注明总数。
const maxSitemapEntries = 200

// sitemapDoc 同时兼容 <urlset> (页面清单) 与 <sitemapindex> (子 sitemap 清单),
// 两者 entry 结构相同 (loc + lastmod), 一次解码全取; 命名空间前缀由
// encoding/xml 按本地名匹配, 不用显式处理。
type sitemapDoc struct {
	URLs []sitemapEntry `xml:"url"`
	Subs []sitemapEntry `xml:"sitemap"`
}

// sitemapEntry 是 urlset 里的一条 <url> 或 sitemapindex 里的一条 <sitemap>。
type sitemapEntry struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod"`
}

// entries 返回清单条目 (urlset 的 url 优先, 空则取 sitemapindex 的 sitemap)。
func (d *sitemapDoc) entries() []sitemapEntry {
	if len(d.URLs) > 0 {
		return d.URLs
	}
	return d.Subs
}

// parseSitemapXML 解码 sitemap, 容忍未知扩展标签 (同 parseFeedXML)。
func parseSitemapXML(raw string) (*sitemapDoc, error) {
	var doc sitemapDoc
	if err := xml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// sitemapMarkdown 把 sitemap 渲染成链接清单: 每条 loc 都是站点声明真实存在的
// URL, 模型可顺藤抓取 (这些链接经 seen-URL 登记后, 后续 WebFetch 不会触发
// 臆造提示); lastmod 原样附上帮助挑选。lastmod 不设为文档 published ——
// 很多站点全量刷成当天, 语义不可靠。
func sitemapMarkdown(doc *sitemapDoc) string {
	entries := doc.entries()
	var sb strings.Builder
	if len(doc.URLs) == 0 && len(doc.Subs) > 0 {
		sb.WriteString("子 sitemap 清单 (逐条抓取可继续发现页面):\n\n")
	}
	shown := entries
	truncated := len(shown) > maxSitemapEntries
	if truncated {
		shown = shown[:maxSitemapEntries]
	}
	for _, e := range shown {
		loc := strings.TrimSpace(e.Loc)
		if loc == "" {
			continue
		}
		sb.WriteString("- [")
		sb.WriteString(loc)
		sb.WriteString("](")
		sb.WriteString(loc)
		sb.WriteString(")")
		if lm := strings.TrimSpace(e.LastMod); lm != "" {
			sb.WriteString(" — ")
			sb.WriteString(lm)
		}
		sb.WriteString("\n")
	}
	if truncated {
		fmt.Fprintf(&sb, "\n[已截断: 共 %d 条, 仅列前 %d 条]", len(entries), maxSitemapEntries)
	}
	return sb.String()
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

// repairArxivListURL 把 arXiv 旧格式的列表页 (/list/<cat>/<YYMM>) 改写成
// 现行的 YYYY-MM 格式。
//
// 只在 host=arxiv.org、路径恰好是 list/<cat>/<4 位数字> 时生效; 月份不合法
// (00/13+) 不猜。返回值 ok=false 表示"不是这个形状", 调用方保持原样。
func repairArxivListURL(u *url.URL) (*url.URL, bool) {
	if u == nil {
		return nil, false
	}
	host := strings.ToLower(u.Hostname())
	if host != "arxiv.org" && host != "www.arxiv.org" {
		return nil, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "list" || parts[1] == "" {
		return nil, false
	}
	ym := parts[2]
	if len(ym) != 4 {
		return nil, false
	}
	for _, c := range ym {
		if c < '0' || c > '9' {
			return nil, false
		}
	}
	mm := ym[2:]
	if mm < "01" || mm > "12" {
		return nil, false
	}
	fixed := *u
	fixed.Path = "/list/" + parts[1] + "/20" + ym[:2] + "-" + mm
	fixed.RawPath = ""
	return &fixed, true
}

// waybackRetryable 判断哪类原站失败值得回退归档。
//
// 除 ErrInvalid (SSRF/协议校验, 归档侧同样会拒) 与 ErrUnsupported (内容
// 类型不支持, 归档里的同一 URL 还是同一类型) 外都回退: 404/410 (页面已死,
// 归档是唯一活口)、5xx/超时 (原站故障)、403/429 (拒爬 —— 读快照不等于
// 绕过反爬, 快照是公开归档的既往公开内容)。
func waybackRetryable(err error) bool {
	if err == nil {
		return false
	}
	switch KindOf(err) {
	case ErrInvalid, ErrUnsupported:
		return false
	}
	return true
}

// minWaybackChars 是归档快照可用的最小正文字数。低于它的快照没有任何研究
// 价值 (占位页/反爬页/跳转页), 宁可报失败也不当成功返回。
const minWaybackChars = 64

// waybackBase 是 Wayback Machine 的抓取前缀; "2" 取最近快照, "id_" 修饰
// 要求返回未改写的原始响应 (不归一化链接/不注入归档工具条)。
const waybackBase = "https://web.archive.org/web/2id_/"

// waybackSnapshotURL 把原 URL 改写成 Wayback 最近快照地址。var (而非内部
// 直连) 以便测试把归档端点指向 httptest, 与 urlGuard 的同包覆写惯例一致。
var waybackSnapshotURL = func(u *url.URL) (*url.URL, error) {
	return url.Parse(waybackBase + u.String())
}

// fetchWayback 经 Wayback Machine 抓原 URL 的最近快照。
//
// 逐出口模式重试 (与原抓取同一 modes 序列), 成功时把文档归回**原 URL**
// (缓存 key 从一开始就基于原 URL, 这样后续命中不再走归档), Via 标记
// wayback 让面板/质量门能看出这不是原站实时内容。快照不存在时 archive
// 返回 404, 与原站失败同向合并为 unavailable。
func (m *Manager) fetchWayback(ctx context.Context, u *url.URL, modes []Mode) (*Document, error) {
	snapshot, err := waybackSnapshotURL(u)
	if err != nil {
		return nil, Wrap("wayback", ErrInvalid, err)
	}
	var lastErr error
	for _, mode := range modes {
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				lastErr = Wrap("wayback", classifyError(err), err)
			}
			break
		}
		doc, err := m.fetchWithClient(ctx, m.egress.Client(mode), snapshot)
		if err != nil {
			lastErr = err
			if k := KindOf(err); k == ErrInvalid || k == ErrNotFound {
				break
			}
			continue
		}
		// 归档快照本身也可能是反爬/占位页 (实测 Reddit 的 "快照" 只有 6 字节),
		// 当成成功返回会让上层以为拿到了正文。太薄的快照按不可用处理 —— 与
		// "归档里没有这篇" 同向, 调用方会看到 wayback_fallback_failed 并如实
		// 报抓取失败, 而不是拿 6 字节去写报告。
		if n := len([]rune(strings.TrimSpace(doc.Markdown))); n < minWaybackChars {
			lastErr = Wrap("wayback", ErrUnavailable, fmt.Errorf("snapshot too thin: %d chars", n))
			continue
		}
		doc.URL = u.String()
		doc.Via = "wayback"
		return doc, nil
	}
	if lastErr == nil {
		lastErr = Wrap("wayback", ErrUnavailable, errors.New("no egress mode attempted"))
	}
	return nil, lastErr
}
