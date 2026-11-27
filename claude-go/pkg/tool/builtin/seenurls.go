package builtin

// seenurls.go —— 会话级"见过的 URL"登记簿, 服务 WebFetch 的臆造地址提示。
//
// 背景: 行研类 agent 会凭训练记忆拼 release/tag 这类"看起来像"的 URL
// (vllm v0.35.0、sglang v1.0.0 ...), 404 之后浪费一轮抓取。2026-11 遥测里
// 这类 404 占 fetch 错误的三分之一。这里的判据是行为性的: URL 若从未在
// 本会话的 WebSearch 结果或已成功抓取的页面里出现过, 404 时附一句提示,
// 把"换个真实链接"的选择权交回模型 —— 软约束, 不拒绝执行。
//
// 只按 SessionID 记, 进程级共享: 同一进程内并发的多个工具调用 (并行
// WebFetch) 都落在同一把锁上; 会话结束不主动清理, 用容量上限做淘汰。

import (
	"net/url"
	"regexp"
	"strings"
	"sync"
)

const (
	// maxSeenSessions 同时保留的会话数; 超出的按最早登记的淘汰。
	maxSeenSessions = 256
	// maxSeenURLsPerSession 单会话登记的 URL 上限, 防止长会话无限增长。
	maxSeenURLsPerSession = 4096
)

var (
	seenURLsMu   sync.Mutex
	seenURLsByID = make(map[string]map[string]struct{})
	seenURLsFIFO []string // 会话登记顺序, 用于淘汰

	// seenURLRe 从工具输出文本里抽取 http(s) 链接; 终止字符集覆盖
	// markdown/JSON 里常见的包裹符号。
	seenURLRe = regexp.MustCompile(`https?://[^\s"'\)\]>\},` + "`" + `]+`)
)

// normalizeSeenURL 归一化 URL 供集合比较: 小写 host、去 www.、去 fragment、
// 去末尾斜杠。不做 query 排序 —— 臆造 URL 与真实 URL 的差别通常在路径,
// 过度归一反而把不同页面判成同一个。
func normalizeSeenURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	key := host + strings.TrimRight(u.Path, "/")
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key
}

// recordSeenURLs 把一批 URL 登记到会话名下。sessionID 为空 (无会话上下文
// 的调用方) 时静默不记。
func recordSeenURLs(sessionID string, urls []string) {
	if sessionID == "" || len(urls) == 0 {
		return
	}
	seenURLsMu.Lock()
	defer seenURLsMu.Unlock()
	set := seenURLsByID[sessionID]
	if set == nil {
		if len(seenURLsFIFO) >= maxSeenSessions {
			delete(seenURLsByID, seenURLsFIFO[0])
			seenURLsFIFO = seenURLsFIFO[1:]
		}
		set = make(map[string]struct{})
		seenURLsByID[sessionID] = set
		seenURLsFIFO = append(seenURLsFIFO, sessionID)
	}
	for _, raw := range urls {
		if len(set) >= maxSeenURLsPerSession {
			break
		}
		if key := normalizeSeenURL(raw); key != "" {
			set[key] = struct{}{}
		}
	}
}

// recordSeenURLsFromText 从工具输出文本抽取链接并登记 —— WebSearch 结果与
// WebFetch 成功的正文 (feed 条目、页面内链) 都算"见过的", 避免把从页面里
// 顺藤摸瓜得到的真实链接误报成臆造。
func recordSeenURLsFromText(sessionID, text string) {
	if sessionID == "" || text == "" {
		return
	}
	recordSeenURLs(sessionID, seenURLRe.FindAllString(text, -1))
}

// seenURL 报告该 URL 是否在本会话出现过。sessionID 为空时返回 true ——
// 没有登记簿就无从判"没见过", 宁可不提示也不误报。
func seenURL(sessionID, raw string) bool {
	if sessionID == "" {
		return true
	}
	key := normalizeSeenURL(raw)
	if key == "" {
		return true
	}
	seenURLsMu.Lock()
	defer seenURLsMu.Unlock()
	_, ok := seenURLsByID[sessionID][key]
	return ok
}
