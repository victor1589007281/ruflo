package tools

import "regexp"

var (
	reEmail   = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	reSSNLike = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	rePhone   = regexp.MustCompile(`\b\+?\d[\d\s\-().]{8,}\d\b`)
)

// detectPIIIssues returns coarse PII match labels (shared by transfer + aidefence tools).
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
