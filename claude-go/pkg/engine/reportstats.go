// reportstats.go —— 报告形态的轻量统计 (正文字数 / 证据行数 / 来源域名数)。
//
// 这三个数在评测脚本里一直是人工从报告文本里抠出来的, 而 agent 自己看不到。
// 结果是: 报告正文 985 字、证据表 25 行只来自 3 个 URL 也能"顺利完成" ——
// 质量约束写在提示词里, 但没有运行时事实可以对照。这里只在审计时统计并打日志,
// 不改文本 (改写正文比统计危险得多, 理由见 citation.go 文件头)。
//
// 口径必须与评测脚本一致, 否则日志说 1800 字、脚本量出 1200 字:
//   - 正文只数汉字 (U+3400–U+9FFF), 排除表格行与标题行;
//   - 证据行 = 以 `|` 开头、且不是 `|---|---|` 分隔行的行 (含表头);
//   - 域名只从证据表行里的 URL 取, 内联链接不算 —— 否则一条 20 条内联引用的
//     报告会掩盖"证据表只依赖单一 PDF"。
package engine

import (
	"net/url"
	"regexp"
	"strings"
)

// tableSeparatorRE 匹配 markdown 表格分隔行 (`|---|:--:|`)。
var tableSeparatorRE = regexp.MustCompile(`^\|[\s:|-]+\|$`)

// cjkCharRE 是评测脚本使用的同一区间 (CJK 扩展 A + 基本区)。
var cjkCharRE = regexp.MustCompile(`[\x{3400}-\x{9FFF}]`)

// reportShape 返回正文汉字数、证据表行数、证据表里的独立域名数。
//
// 空文本返回全 0; 没有表格时 rows/hosts 为 0。
func reportShape(text string) (bodyCJK, rows, hosts int) {
	if strings.TrimSpace(text) == "" {
		return 0, 0, 0
	}
	body := make([]string, 0, 64)
	hostSet := make(map[string]struct{})
	for _, line := range strings.Split(text, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "#") {
			continue
		}
		if strings.HasPrefix(s, "|") {
			if tableSeparatorRE.MatchString(s) {
				continue
			}
			rows++
			for _, raw := range citationURLRE.FindAllString(s, -1) {
				u := normalizeCitationURL(raw)
				if u == "" {
					continue
				}
				parsed, err := url.Parse(u)
				if err != nil || parsed.Hostname() == "" {
					continue
				}
				hostSet[strings.ToLower(parsed.Hostname())] = struct{}{}
			}
			continue
		}
		body = append(body, s)
	}
	bodyCJK = len(cjkCharRE.FindAllString(strings.Join(body, ""), -1))
	return bodyCJK, rows, len(hostSet)
}
