package security

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// InputValidator performs string and structured input checks for orchestration boundaries.
type InputValidator struct {
	maxStringLen int
}

// NewInputValidator returns a validator with default max string length 256KiB.
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

// ValidateString checks for injection-ish patterns and excessive length.
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

// ValidateAgentSpawn checks agent registration parameters.
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

// ValidateTaskInput validates arbitrary task description / payload strings.
func (v *InputValidator) ValidateTaskInput(title, description string) ValidationResult {
	var issues []ValidationIssue
	issues = append(issues, v.ValidateString("title", title).Issues...)
	issues = append(issues, v.ValidateString("description", description).Issues...)
	return resultOf(issues)
}

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

// SanitizeString trims whitespace and strips control characters.
func SanitizeString(input string) string {
	s := strings.TrimSpace(input)
	s = ctrlChars.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// SanitizeHTML removes angle-bracket tags (best-effort, not a full HTML parser).
func SanitizeHTML(input string) string {
	s := reHTMLTags.ReplaceAllString(input, "")
	return strings.TrimSpace(s)
}

// SanitizePath normalizes a path and collapses traversal segments.
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

// ValidateEmail checks a minimal RFC-like shape for an email address.
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
