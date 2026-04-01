package tools

import "regexp"

// 本文件：PII（个人可识别信息）粗粒度正则检测，供 transfer、aidefence 等工具复用。
//
// 设计思路：仅做模式匹配标签（email、类 SSN、类电话），不保证零误报/漏报；正则预编译为包级变量避免重复编译。

// 预编译正则：邮箱、三段式社保号形态、含区号/分隔符的电话形态。
var (
	reEmail   = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	reSSNLike = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	rePhone   = regexp.MustCompile(`\b\+?\d[\d\s\-().]{8,}\d\b`)
)

// detectPIIIssues 扫描 text，按命中顺序追加标签："email"、"ssn_like"、"phone_like"；无命中返回 nil 切片。
func detectPIIIssues(text string) []string {
	var issues []string
	if reEmail.FindString(text) != "" {
		issues = append(issues, "email")
	}
	if reSSNLike.FindString(text) != "" {
		issues = append(issues, "ssn_like")
	}
	if rePhone.FindString(text) != "" {
		issues = append(issues, "phone_like")
	}
	return issues
}
