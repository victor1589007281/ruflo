// 本文件实现编排边界的字符串与结构化输入校验：在入口处检测超长、Shell 元字符、控制字符、类 SQL/路径穿越片段等；
// SanitizeString/SanitizeHTML/SanitizePath 提供输出侧消毒；ValidateEmail 做轻量邮箱形态校验（非完整 RFC 解析器）。
package security

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// InputValidator 对编排相关的外部字符串做模式与长度检查；maxStringLen 限制单字段体积以缓解 DoS。
type InputValidator struct {
	maxStringLen int // 允许的最大字符串字节长度，默认 256KiB
}

// NewInputValidator 构造校验器，默认单字符串上限 256KiB。
func NewInputValidator() *InputValidator {
	return &InputValidator{maxStringLen: 256 * 1024}
}

var (
	shellMetas = regexp.MustCompile(`[;&|$\x60<>]`)
	ctrlChars  = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f]`)
	sqlish     = regexp.MustCompile(`(?i)(\bunion\b\s+\bselect\b|\bselect\b.+\bfrom\b|\bdrop\b\s+\btable\b|\binsert\b\s+\binto\b|\bdelete\b\s+\bfrom\b|\bexec\b\s*\()`)
	pathish    = regexp.MustCompile(`\.\./|\.\.\\`)
	reHTMLTags = regexp.MustCompile(`(?s)<[^>]*>`)
	reEmail    = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)
)

// ValidateString 按字段名聚合规则：先比长度，再依次用正则匹配 Shell 元字符、控制字符、类 SQL、路径片段等；
// 命中规则追加为 ValidationIssue，最后由 resultOf 根据是否存在 Block 决定 OK。
func (v *InputValidator) ValidateString(field, s string) ValidationResult {
	var issues []ValidationIssue
	if len(s) > v.maxStringLen {
		issues = append(issues, ValidationIssue{Field: field, Message: "string too long", Severity: SeverityBlock})
	}
	if shellMetas.MatchString(s) {
		issues = append(issues, ValidationIssue{Field: field, Message: "contains shell metacharacters", Severity: SeverityWarn})
	}
	if ctrlChars.MatchString(s) {
		issues = append(issues, ValidationIssue{Field: field, Message: "contains control characters", Severity: SeverityBlock})
	}
	if sqlish.MatchString(s) {
		issues = append(issues, ValidationIssue{Field: field, Message: "suspected SQL-like payload", Severity: SeverityWarn})
	}
	if pathish.MatchString(s) {
		issues = append(issues, ValidationIssue{Field: field, Message: "path traversal sequence", Severity: SeverityWarn})
	}
	return resultOf(issues)
}

// ValidateAgentSpawn 校验智能体注册相关字段：先对 name、type、namespace 各跑一遍 ValidateString（继承通用注入/长度规则），
// 再追加业务约束——name 与 type 去空白后非空（block），name 中不允许 Unicode 控制字符（block）。
func (v *InputValidator) ValidateAgentSpawn(name, agentType, namespace string) ValidationResult {
	var issues []ValidationIssue
	issues = append(issues, v.ValidateString("name", name).Issues...)
	issues = append(issues, v.ValidateString("type", agentType).Issues...)
	issues = append(issues, v.ValidateString("namespace", namespace).Issues...)
	if strings.TrimSpace(name) == "" {
		issues = append(issues, ValidationIssue{Field: "name", Message: "required", Severity: SeverityBlock})
	}
	if strings.TrimSpace(agentType) == "" {
		issues = append(issues, ValidationIssue{Field: "type", Message: "required", Severity: SeverityBlock})
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			issues = append(issues, ValidationIssue{Field: "name", Message: "invalid character", Severity: SeverityBlock})
			break
		}
	}
	return resultOf(issues)
}

// ValidateTaskInput 对任务标题与描述仅复用 ValidateString，无额外必填规则；适合通用任务载荷入口。
func (v *InputValidator) ValidateTaskInput(title, description string) ValidationResult {
	var issues []ValidationIssue
	issues = append(issues, v.ValidateString("title", title).Issues...)
	issues = append(issues, v.ValidateString("description", description).Issues...)
	return resultOf(issues)
}

// resultOf 线性扫描 issues：时间复杂度 O(n)；若存在任一 SeverityBlock 则 OK=false，否则 OK=true（warn/info 不阻断）。
func resultOf(issues []ValidationIssue) ValidationResult {
	block := false
	for _, i := range issues {
		if i.Severity == SeverityBlock {
			block = true
			break
		}
	}
	return ValidationResult{OK: !block, Issues: issues}
}

// SanitizeString 输出侧消毒：TrimSpace 后删除与 ctrlChars 一致的控制字符区间，减少终端与日志逃逸风险。
func SanitizeString(input string) string {
	s := strings.TrimSpace(input)
	s = ctrlChars.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// SanitizeHTML 基于正则删除形如 <...> 的片段，复杂度与输入长度线性相关；无法处理注释、脚本边界、属性事件等复杂 HTML，
// 不能替代解析器消毒与内容安全策略（CSP）。
func SanitizeHTML(input string) string {
	s := reHTMLTags.ReplaceAllString(input, "")
	return strings.TrimSpace(s)
}

// SanitizePath 将用户路径串规范为不含穿越语义的相对路径形式。
// 步骤：反斜杠→斜杠 → filepath.Clean → 去掉前导 "/" → 按 "/" 分段遍历：跳过 "" 与 "."；
// 遇 ".." 则弹栈（模拟路径解析）；若某段仍匹配 pathish 则丢弃该段。最终 Join 得到稳定相对路径字符串。
func SanitizePath(input string) string {
	s := strings.TrimSpace(strings.ReplaceAll(input, "\\", "/"))
	s = filepath.Clean(s)
	if strings.HasPrefix(s, "/") {
		s = strings.TrimPrefix(s, "/")
	}
	parts := strings.Split(s, "/")
	var out []string
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		if pathish.MatchString(p) {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "/")
}

// ValidateEmail 对 local@domain.tld 形态做正则匹配：local 侧允许字母数字与 ._%+-；domain 含点分标签。
// 空串返回 block；不匹配返回 block；匹配返回 OK=true 且无 Issues（与 ValidateString 风格略有不同，返回指针便于可选链）。
func ValidateEmail(email string) *ValidationResult {
	e := strings.TrimSpace(email)
	if e == "" {
		r := &ValidationResult{OK: false, Issues: []ValidationIssue{{Field: "email", Message: "required", Severity: SeverityBlock}}}
		return r
	}
	if !reEmail.MatchString(e) {
		r := &ValidationResult{OK: false, Issues: []ValidationIssue{{Field: "email", Message: "invalid email format", Severity: SeverityBlock}}}
		return r
	}
	return &ValidationResult{OK: true}
}
