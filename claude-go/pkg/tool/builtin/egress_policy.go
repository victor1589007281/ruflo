// egress_policy.go 实现 WebFetch 的出网域名策略 (H-F2 安全门槛)。
//
// 解决的问题: SSRF 校验只拦"内网地址", 不回答"该不该访问这个公网域名"。
// 最终版的上下文里可能有私密文本, 而模型能抓任意网页 —— 两条攻击路径:
//
//  1. 网页指令劫持: 抓回来的内容直接进模型上下文, 没有 trust 标记就无法让
//     审核层/模型区分"用户说的话"与"网页让它做的事";
//  2. URL 外传: 私密文本被编码进 URL query, SSRF 不拦公网外传。
//
// 本文件负责第 2 条里的域名准入。它**不**替代 SSRF 逐跳校验, 也不做内容过滤:
// 策略只在请求发出之前回答"这个 host 允不允许 / 要不要人点头"。
//
// 零值语义 (向后兼容的关键): DomainPolicy{} = 允许所有 host、从不要求确认,
// 与改造前逐字节相同。策略是否启用完全由装配方决定。
package builtin

import (
	"encoding/json"
	"net"
	"net/url"
	"strings"

	"github.com/anthropic/claude-go/pkg/tool"
)

// DomainDecision 是域名策略对单个 host 的裁决。
type DomainDecision int

const (
	// DomainAllowed: 命中 allow 名单。
	DomainAllowed DomainDecision = iota
	// DomainDenied: 命中 deny 名单。deny 优先于 allow。
	DomainDenied
	// DomainUnknown: 两张名单都没命中。是否放行由 UnknownNeedsConfirmation 决定。
	DomainUnknown
)

// DomainPolicy 是 WebFetch 的出网域名策略。
//
// 匹配规则:
//   - host 与名单项都不区分大小写, 忽略末尾的点 (FQDN 写法);
//   - 名单项 "example.com" 同时匹配 "example.com" 与 "a.example.com" (含子域);
//   - 显式写 "*." 前缀等价于去掉前缀 (统一按含子域处理);
//   - 端口不参与匹配: 名单项可以带端口, 比较前会剥掉; 请求侧用 Hostname()。
//
// Denied 优先: 同一个 host 同时出现在 Allowed 与 Denied 时判 Denied。这是
// fail-closed 的方向 —— 一条误加的 deny 只会让某个站点不可用(可见), 而让 deny
// 被 allow 盖过会让一条误加的 allow 静默打开一个本该被封的出口。
type DomainPolicy struct {
	// Allowed 是允许直接抓取的域名 (含子域)。
	Allowed []string
	// Denied 是禁止抓取的域名 (含子域)。
	Denied []string
	// UnknownNeedsConfirmation 为 true 时, 两张名单都没命中的域名要求宿主
	// 显式确认后才能抓取。false = 未知域直接放行 (旧行为)。
	UnknownNeedsConfirmation bool
}

// Decide 返回到 host 的策略裁决。
func (p DomainPolicy) Decide(host string) DomainDecision {
	h := normalizeDomainHost(host)
	if h == "" {
		// 空 host 不可能是合法 URL; 交给 URL 校验报错, 不在这里判 deny。
		return DomainUnknown
	}
	for _, d := range p.Denied {
		if domainMatches(h, normalizeDomainHost(d)) {
			return DomainDenied
		}
	}
	for _, a := range p.Allowed {
		if domainMatches(h, normalizeDomainHost(a)) {
			return DomainAllowed
		}
	}
	return DomainUnknown
}

// NeedsConfirmationFor 回答"这个 host 在未知的情况下是否需要确认"。
// 非未知域恒为 false (deny 由 Call 直接拒绝, 不需要人点头; allow 已明确允许)。
func (p DomainPolicy) NeedsConfirmationFor(host string) bool {
	return p.UnknownNeedsConfirmation && p.Decide(host) == DomainUnknown
}

// normalizeDomainHost 把 host/名单项规范化成可比较的形式。
//
// 允许名单项写成 "https://example.com/path" 或 "example.com:443" 这类方便人
// 手写的形态; 解析失败时退回朴素的 lower/去尾点, 不让一条格式奇怪的名单项
// 导致 panic 或悄悄匹配空串。
func normalizeDomainHost(raw string) string {
	d := strings.TrimSpace(strings.ToLower(raw))
	if d == "" {
		return ""
	}
	// FQDN 末尾的点。
	d = strings.TrimSuffix(d, ".")
	// 允许手写完整 URL。
	if strings.Contains(d, "://") {
		if u, err := url.Parse(d); err == nil && u.Hostname() != "" {
			d = strings.ToLower(u.Hostname())
		}
	}
	// 剥端口 (IPv6 字面量带方括号时 Hostname() 已经处理过)。
	if h, _, err := net.SplitHostPort(d); err == nil {
		d = h
	}
	// 裸 IPv6 字面量: url.Hostname() 不带方括号, 但名单项可能手写成 "[::1]"。
	if strings.HasPrefix(d, "[") && strings.HasSuffix(d, "]") {
		d = d[1 : len(d)-1]
	}
	d = strings.TrimPrefix(d, "*.")
	return strings.TrimSuffix(strings.ToLower(d), ".")
}

// domainMatches 判断 host 是否等于 domain 或为其子域。
//
// 必须用 "."+domain 做后缀判据: 直接 HasSuffix(host, domain) 会让
// "notexample.com" 匹配 "example.com" —— 一个经典的越权写法。
func domainMatches(host, domain string) bool {
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// SetDomainPolicy 装配出网域名策略。零值 = 允许全部且不要求确认。
func (t *WebFetchTool) SetDomainPolicy(p DomainPolicy) {
	t.policy = p
}

// NeedsConfirmationFor 实现 tool.ToolMetaInputConfirmation (H-F2)。
//
// 只有"未知域 + 策略要求确认"才返回 true。入参解析失败时返回 false: 非法
// URL 会在 Call 里得到明确的 input error, 不需要先弹一次确认再报错。
func (t *WebFetchTool) NeedsConfirmationFor(input json.RawMessage) bool {
	if !t.policy.UnknownNeedsConfirmation {
		return false
	}
	var in webFetchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || u.Hostname() == "" {
		return false
	}
	return t.policy.NeedsConfirmationFor(u.Hostname())
}

// 编译期断言: WebFetchTool 实现了按入参的确认声明。
var _ tool.ToolMetaInputConfirmation = (*WebFetchTool)(nil)
