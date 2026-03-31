// Package security provides boundary validation and safe execution helpers.
package security

// ValidationSeverity indicates how severe a finding is.
type ValidationSeverity string

const (
	SeverityInfo  ValidationSeverity = "info"
	SeverityWarn  ValidationSeverity = "warn"
	SeverityBlock ValidationSeverity = "block"
)

// ValidationIssue describes one failed check.
type ValidationIssue struct {
	Field    string             `json:"field"`
	Message  string             `json:"message"`
	Severity ValidationSeverity `json:"severity"`
}

// ValidationResult aggregates issues from a validation pass.
type ValidationResult struct {
	OK     bool              `json:"ok"`
	Issues []ValidationIssue `json:"issues,omitempty"`
}
