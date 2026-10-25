// citation.go —— 引用 URL 账本与事后审计。
//
// 调研型 agent 最危险的一类错误不是"少写一条证据", 而是**编造一个看起来对的
// URL**: FDA accessdata 的 NDA 号路径、DOI 出版页、财报 PDF 路径。它们格式
// 完全合法, 点开却是 404 —— 报告因此比没有引用更糟。
//
// 这里做两件事, 都不改写最终文本:
//  1. 运行中记录"工具实际返回/请求过的 URL", 在强制收口时把这份清单交给模型
//     (证据表只许引用清单内的 URL);
//  2. 运行结束后审计最终文本: 引用了清单外 URL 时记 Warn, 含条数与样例.
//
// 为什么不在事后自动删改: 删除一行证据可能把结论变成无源断言, 而改写过的报告
// 会掩盖"模型曾经编造"这件事。审计痕迹留给上游判断, 比静默修补更有用。
package engine

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// citationNonURLRunes 是"URL 里不该出现、但中文报告里紧贴 URL"的字符区间。
//
// 只排除 ASCII 标点是不够的: 实测最终文本写成
// `https://www.fx168news.com/article/1075841）域名解析失败；中国经营网解读（http://...`
// 时, 旧的 `[^\s...]+` 会把后面整句中文一起吞进 URL —— 账本里于是多出一条
// 永远匹配不上的"URL", 审计样例和引用核验提示都变成乱码。URL 里的非 ASCII
// 内容按规范应 percent-encode, 所以这里直接从 CJK 区段截断是安全的。
const citationNonURLRunes = `\x{2E80}-\x{9FFF}\x{F900}-\x{FAFF}\x{FE00}-\x{FE6F}\x{FF00}-\x{FFEF}`

// citationURLRE 抓文本里的 http(s) URL。
//
// 排除空白、常见包裹字符与 CJK 字符; 尾部标点单独裁 (markdown 表格 `| url |`、
// 句末句号、`<url>` 都很常见)。
var citationURLRE = regexp.MustCompile(`(?i)https?://[^\s"'<>\[\]{}\x60` + citationNonURLRunes + `]+`)

// citationDOIRE 抓裸 DOI (10.xxxx/...)。
//
// 检索结果经常只给 DOI 号不给 URL, 而提示词要求统一写成 https://doi.org/<doi>。
// 只比对 URL 会把这种**有据可查的构造**误判成编造 —— 审计一旦开始误报, 真正
// 的编造就淹没在噪声里了。
var citationDOIRE = regexp.MustCompile(`(?i)10\.[0-9]{4,9}/[^\s"'<>\[\]{}\x60` + citationNonURLRunes + `]+`)

// citationLedger 一次 run 内见过的 URL / DOI 集合。
type citationLedger struct {
	urls map[string]struct{}
	dois map[string]struct{}
	// fetched 是"本轮真的成功抓取过"的子集。
	//
	// 为什么要单独记: 搜索结果里出现的 URL 也会进 urls, 于是一条只被 snippet
	// 扫到、从未打开的链接和一条 WebFetch 成功返回的原文在账本里没有区别 ——
	// 而报告要求的是"逐字摘录", 前者根本支撑不了。实测茅台任务 12 条引用里
	// 有 10 条只出现在搜索结果中 (新浪财经), 摘录却写成引文。
	fetched map[string]struct{}
	// templates 是 Bash 里带 shell 变量的 curl 模板 (按 ? 前的路径归一)。
	//
	// 实测 FRED 的取数命令写成 `for s in UNRATE PAYEMS; do curl "...csv?id=$s"; done`,
	// 命令里没有具体 URL, 结果里只有 CSV, 于是报告引用的
	// `...csv?id=UNRATE` 会被审计误判成"从未返回过"(疑似编造)。按路径记模板,
	// 只解决"同端点批量取数"这一类误报, 不把任意同域 URL 都放过。
	templates map[string]struct{}
}

// newCitationLedger 构造一次 run 的空账本。四张表都预分配, 避免在高频的
// tool_result 观察路径上反复初始化。
func newCitationLedger() *citationLedger {
	return &citationLedger{
		urls:      make(map[string]struct{}),
		dois:      make(map[string]struct{}),
		fetched:   make(map[string]struct{}),
		templates: make(map[string]struct{}),
	}
}

// observeText 记录一段文本里出现的 URL (工具结果 / 用户 prompt)。
func (l *citationLedger) observeText(s string) {
	if l == nil || s == "" {
		return
	}
	for _, raw := range citationURLRE.FindAllString(s, -1) {
		// shell 变量片段 ($s / ${id}) 不是可引用 URL, 别让它进账本。
		if strings.Contains(raw, "$") {
			continue
		}
		if u := normalizeCitationURL(raw); u != "" {
			l.urls[u] = struct{}{}
		}
	}
	for _, raw := range citationDOIRE.FindAllString(s, -1) {
		if d := normalizeDOI(raw); d != "" {
			l.dois[d] = struct{}{}
		}
	}
}

// normalizeDOI 归一 DOI: 小写、裁尾部标点。
func normalizeDOI(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRightFunc(raw, func(r rune) bool {
		switch r {
		case '.', ',', ';', ':', '!', '?', ')', ']', '}', '>', '|', '"', '\'':
			return true
		case '。', '，', '；', '：', '！', '？', '）', '】', '》', '」', '』':
			return true
		default:
			return false
		}
	})
	return strings.ToLower(raw)
}

// doiOfURL 从 doi.org / dx.doi.org 链接里取出 DOI (其他 URL 返回空)。
func doiOfURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "doi.org" && host != "dx.doi.org" {
		return ""
	}
	return normalizeDOI(strings.TrimPrefix(parsed.Path, "/"))
}

// observeToolUses 记录 tool_use 入参里的 URL。
//
// WebFetch 的 URL 只出现在入参, 不在结果里 —— 漏掉这一步会把"真的打开过"
// 的页面判成未验证。
func (l *citationLedger) observeToolUses(blocks []types.ContentBlock) {
	if l == nil {
		return
	}
	for _, b := range blocks {
		if b.Type != types.ContentBlockToolUse || len(b.Input) == 0 {
			continue
		}
		l.observeText(string(b.Input))
	}
}

// observeMessages 记录 tool_result 消息里的 URL。
func (l *citationLedger) observeMessages(msgs []types.Message) {
	if l == nil {
		return
	}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == types.ContentBlockToolResult {
				l.observeText(b.Content)
			}
		}
	}
}

// observeFetches 记录"成功抓取"的 URL: WebFetch 的成功 tool_result, 以及
// Bash 里 curl/wget 的成功调用。
//
// 判据用 tool_use_id 把入参与结果配对, 且只认 IsError=false 的那条。不能只看
// 入参: 403/404 的 WebFetch 同样带 URL, 把它算成已抓取会让审计把"打不开的页面"
// 当成合格证据 —— 那正是要拦的那类错误。
func (l *citationLedger) observeFetches(uses []types.ContentBlock, results []types.Message) {
	if l == nil {
		return
	}
	byID := make(map[string]types.ContentBlock, len(uses))
	for _, b := range uses {
		if b.Type == types.ContentBlockToolUse && b.ID != "" {
			byID[b.ID] = b
		}
	}
	if len(byID) == 0 {
		return
	}
	for _, m := range results {
		for _, r := range m.Content {
			if r.Type != types.ContentBlockToolResult || r.IsError {
				continue
			}
			use, ok := byID[r.ToolUseID]
			if !ok {
				continue
			}
			switch use.Name {
			case "WebFetch":
				var in struct {
					URL string `json:"url"`
				}
				if json.Unmarshal(use.Input, &in) == nil && in.URL != "" {
					l.observeFetched(in.URL)
				}
			case "Bash":
				var in struct {
					Command string `json:"command"`
				}
				if json.Unmarshal(use.Input, &in) != nil {
					continue
				}
				if !strings.Contains(in.Command, "curl") && !strings.Contains(in.Command, "wget") {
					continue
				}
				for _, raw := range citationURLRE.FindAllString(in.Command, -1) {
					u := normalizeCitationURL(raw)
					if u == "" {
						continue
					}
					// 变量模板 (`?id=$s` / `?id={s}`) 无法逐条核对参数, 记路径。
					if strings.Contains(raw, "$") || strings.HasSuffix(u, "=") {
						l.observeTemplate(u)
						continue
					}
					l.observeFetched(u)
				}
			}
		}
	}
}

// observeTemplate 记录带变量的取数端点 (按 ? 前路径归一)。
func (l *citationLedger) observeTemplate(raw string) {
	u := normalizeCitationURL(raw)
	if u == "" {
		return
	}
	key := u
	if i := strings.IndexByte(key, '?'); i >= 0 {
		key = key[:i]
	}
	if key != "" {
		l.templates[key] = struct{}{}
	}
}

// templateHit 判断引用是否落在"同端点变量取数"的模板里。
func (l *citationLedger) templateHit(u string) bool {
	if l == nil || len(l.templates) == 0 {
		return false
	}
	if _, ok := l.templates[u]; ok {
		return true
	}
	if i := strings.IndexByte(u, '?'); i >= 0 {
		_, ok := l.templates[u[:i]]
		return ok
	}
	return false
}

// observeFetched 记录一条成功抓取的 URL (同时算已见)。
func (l *citationLedger) observeFetched(raw string) {
	u := normalizeCitationURL(raw)
	if u == "" {
		return
	}
	l.urls[u] = struct{}{}
	l.fetched[u] = struct{}{}
}

// sample 返回至多 n 条 URL (排序后), 供收口提示使用。
//
// 排序是为了让同一份上下文在重跑时稳定 —— 随机顺序会让前缀缓存失效, 也会让
// "为什么这次模型引用了别的 URL"无法归因。
func (l *citationLedger) sample(n int) []string {
	if l == nil || len(l.urls) == 0 || n <= 0 {
		return nil
	}
	out := make([]string, 0, len(l.urls))
	for u := range l.urls {
		out = append(out, u)
	}
	sort.Strings(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// sampleFetched 返回至多 n 条**成功抓取过**的 URL (排序后)。
//
// 收口提示优先用这份清单: 它比"搜索结果里出现过"强得多 —— 模型照着它写证据表,
// 至少链接真的打开过。
func (l *citationLedger) sampleFetched(n int) []string {
	if l == nil || len(l.fetched) == 0 || n <= 0 {
		return nil
	}
	out := make([]string, 0, len(l.fetched))
	for u := range l.fetched {
		out = append(out, u)
	}
	sort.Strings(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// searchEngineHosts 是绝不能出现在证据表里的"来源"。
//
// 实测 CPI 任务把 html.duckduckgo.com 的结果页直接当成 3 条证据的来源。
// 结果页的 URL 会随查询变化, 也不是一手材料; 它只能用来发现下一跳。
var searchEngineHosts = map[string]struct{}{
	"duckduckgo.com": {}, "html.duckduckgo.com": {},
	"bing.com": {}, "www.bing.com": {},
	"google.com": {}, "www.google.com": {},
	"baidu.com": {}, "www.baidu.com": {},
	"search.yahoo.com": {},
}

// searchEngineCitations 返回最终文本里以搜索引擎结果页充当来源的 URL。
func (l *citationLedger) searchEngineCitations(finalText string) []string {
	cited := make(map[string]struct{})
	for _, raw := range citationURLRE.FindAllString(finalText, -1) {
		u := normalizeCitationURL(raw)
		if u == "" {
			continue
		}
		parsed, err := url.Parse(u)
		if err != nil {
			continue
		}
		if _, ok := searchEngineHosts[strings.ToLower(parsed.Hostname())]; ok {
			cited[u] = struct{}{}
		}
	}
	out := make([]string, 0, len(cited))
	for u := range cited {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// audit 比对最终文本与账本, 列出未在本次工具输出中出现过的 URL。
//
// 只记日志, 不改文本 (理由见文件头)。样本上限 5 条: 日志是给人看的, 不是
// 第二份证据表。
//
// 四个计数刻意分开, 因为它们对应不同的处置:
//   - unverified: 账本里完全没有的 URL, 疑似编造;
//   - seen_not_fetched: 搜索结果里见过但没打开, 只能支撑"搜索摘要"而非引文;
//   - search_engine: 拿结果页当来源, 必须替换成下一跳;
//   - body_cjk/evidence_rows/distinct_hosts: 报告形态, 用来解释质量波动。
//
// 汇总日志在 cited>0 或 rows>=5 时无条件打: 只在告警时打会让"审计通过"与
// "审计没跑"在日志上无法区分。
func (l *citationLedger) audit(finalText string) {
	missing, cited := l.unverified(finalText)
	// 第二档: 搜索结果里见过、但从未成功抓取。它不是"编造", 但支撑不了
	// "逐字摘录" —— 单独报出来, 免得和编造混在同一条计数里。
	unfetched, _ := l.unfetched(finalText)
	seen := l.seenOnly(unfetched)
	se := l.searchEngineCitations(finalText)
	bodyCJK, rows, hosts := reportShape(finalText)
	// 无论干不干净都打一条汇总。只在有告警时打日志会让"审计通过"与
	// "审计根本没跑"在日志上长得一模一样, 事后无法区分。
	if cited > 0 || rows >= 5 {
		logging.For("engine").Info("引用审计汇总",
			"cited", cited, "fetched_cited", cited-len(unfetched),
			"unverified", len(missing), "seen_not_fetched", len(seen),
			"search_engine", len(se),
			"body_cjk", bodyCJK, "evidence_rows", rows, "distinct_hosts", hosts)
	}
	// 来源过度集中不判失败 (年报/法规天然单源), 但要让人一眼看见:
	// cmb 曾出现 25 行证据只来自 3 个 URL。
	if rows >= 15 && hosts > 0 && hosts <= 3 {
		logging.For("engine").Warn("证据表来源过度集中",
			"evidence_rows", rows, "distinct_hosts", hosts, "cited", cited)
	}
	if len(missing) > 0 {
		logging.For("engine").Warn("最终报告引用了本轮工具未返回过的 URL",
			"cited", cited, "unverified", len(missing), "sample", strings.Join(firstNStrings(missing, 5), ", "))
	}
	if len(seen) > 0 {
		logging.For("engine").Warn("最终报告引用了只在搜索结果中出现、未成功抓取的 URL",
			"cited", cited, "seen_not_fetched", len(seen), "sample", strings.Join(firstNStrings(seen, 5), ", "))
	}
	if len(se) > 0 {
		logging.For("engine").Warn("最终报告把搜索引擎结果页当作来源",
			"count", len(se), "sample", strings.Join(firstNStrings(se, 5), ", "))
	}
}

// seenOnly 从"未成功抓取"集合里留下**账本见过**的那些。
//
// unfetched 还包含从未在任何工具输出/入参里出现过的 URL (编造); 把它们
// 算进"只在搜索结果中出现"会让这一档的计数失去含义。
func (l *citationLedger) seenOnly(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if _, ok := l.urls[u]; ok {
			out = append(out, u)
		}
	}
	return out
}

// unfetched 返回"已见但未成功抓取"的引用 URL (排序) 与引用总数。
func (l *citationLedger) unfetched(finalText string) ([]string, int) {
	if l == nil || strings.TrimSpace(finalText) == "" {
		return nil, 0
	}
	cited := make(map[string]struct{})
	for _, raw := range citationURLRE.FindAllString(finalText, -1) {
		if u := normalizeCitationURL(raw); u != "" {
			cited[u] = struct{}{}
		}
	}
	var out []string
	for u := range cited {
		if _, ok := l.fetched[u]; ok {
			continue
		}
		// 同端点变量取数: 具体参数没出现在命令里, 但端点确实被 curl 成功
		// 调用过。算"已抓取"会略宽, 但比把真实取数误报成编造更可信。
		if l.templateHit(u) {
			continue
		}
		out = append(out, u)
	}
	sort.Strings(out)
	return out, len(cited)
}

// unverified 返回最终文本里不在账本中的 URL (排序) 与引用总数。
//
// 抽成独立方法是为了让"哪些算未验证"可单独断言 —— 直接测 audit 只能看日志,
// 而日志格式一改用例就假绿。
func (l *citationLedger) unverified(finalText string) ([]string, int) {
	if l == nil || strings.TrimSpace(finalText) == "" {
		return nil, 0
	}
	cited := make(map[string]struct{})
	for _, raw := range citationURLRE.FindAllString(finalText, -1) {
		if u := normalizeCitationURL(raw); u != "" {
			cited[u] = struct{}{}
		}
	}
	if len(cited) == 0 {
		return nil, 0
	}
	var missing []string
	for u := range cited {
		if l.verified(u) {
			continue
		}
		missing = append(missing, u)
	}
	sort.Strings(missing)
	return missing, len(cited)
}

// verified 判断一个引用 URL 是否在本轮工具输出里有依据。
//
// doi.org/<doi> 额外按 DOI 号比对: 检索结果给的是 DOI 号, 提示词要求统一写成
// doi.org 链接, 两者必须能对上。
func (l *citationLedger) verified(u string) bool {
	if _, ok := l.urls[u]; ok {
		return true
	}
	if l.templateHit(u) {
		return true
	}
	if doi := doiOfURL(u); doi != "" {
		_, ok := l.dois[doi]
		return ok
	}
	return false
}

// normalizeCitationURL 归一 URL 以便跨"工具输出"与"最终文本"比对。
//
// 归一 scheme/host 大小写、去掉 fragment 与尾部斜杠、裁掉 markdown/句末标点。
// 不丢 query: 带参数的 URL 往往是不同资源 (带签名的下载链接尤其如此)。
func normalizeCitationURL(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRightFunc(raw, func(r rune) bool {
		switch r {
		case '.', ',', ';', ':', '!', '?', ')', ']', '}', '>', '|', '"', '\'':
			return true
		// 中文标点: 报告里 URL 后面直接跟句号/括号很常见.
		case '。', '，', '；', '：', '！', '？', '）', '】', '》', '」', '』':
			return true
		default:
			return false
		}
	})
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if u.Path != "/" {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	return u.String()
}

// firstNStrings 返回前 n 项 (仅用于日志样例)。
func firstNStrings(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
