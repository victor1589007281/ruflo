// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
)

// SecurityScan runs gosec or falls back to regex-based security scanning.
type SecurityScan struct{ Base }

// NewSecurityScan creates a new SecurityScan skill.
func NewSecurityScan() *SecurityScan {
	return &SecurityScan{
		Base: NewBase(Meta{
			Name:           "security-scan",
			Description:    "Run gosec or fallback regex-based security scanning for Go code.",
			Version:        "1.0.0",
			ReadOnly:       true,
			SafeConcurrent: true,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root": {"type": "string"},
					"files":     {"type": "array", "items": {"type": "string"}}
				},
				"required": ["repo_root"]
			}`),
		}),
	}
}

// Execute runs the security scan.
func (s *SecurityScan) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot string   `json:"repo_root"`
		Files    []string `json:"files,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	// Try gosec first
	if out, err := s.runGosec(ctx, in.RepoRoot); err == nil {
		return s.parseGosecOutput(out)
	}

	// Fallback to regex scanning
	diagnostics := s.regexScan(in.RepoRoot, in.Files)
	return map[string]any{
		"status":      "completed",
		"method":      "regex_fallback",
		"diagnostics": diagnostics,
		"issues":      len(diagnostics),
	}, nil
}

func (s *SecurityScan) runGosec(ctx context.Context, repoRoot string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gosec", "-fmt", "json", "./...")
	cmd.Dir = repoRoot
	return cmd.CombinedOutput()
}

func (s *SecurityScan) parseGosecOutput(out []byte) (map[string]any, error) {
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		return map[string]any{
			"status":     "completed",
			"method":     "gosec",
			"raw_output": string(out),
		}, nil
	}
	result["status"] = "completed"
	result["method"] = "gosec"
	return result, nil
}

func (s *SecurityScan) regexScan(repoRoot string, files []string) []Diagnostic {
	var diagnostics []Diagnostic

	patterns := []struct {
		re       *regexp.Regexp
		category string
		severity string
		msg      string
	}{
		{
			re:       regexp.MustCompile(`"crypto/md5"|"crypto/sha1"`),
			category: "weak_crypto",
			severity: "error",
			msg:      "Weak cryptographic primitive (md5/sha1) detected. Use crypto/sha256 or bcrypt.",
		},
		{
			re:       regexp.MustCompile(`os\.Exec|exec\.Command.*\+.*%s|\+.*\$`),
			category: "command_injection",
			severity: "error",
			msg:      "Potential command injection via string concatenation in exec.Command.",
		},
		{
			re:       regexp.MustCompile(`sql\.Open\s*\(.*\+`),
			category: "sql_injection",
			severity: "error",
			msg:      "Potential SQL injection via string concatenation in sql.Open.",
		},
		{
			re:       regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
			category: "hardcoded_secret",
			severity: "error",
			msg:      "Hardcoded AWS access key detected.",
		},
		{
			re:       regexp.MustCompile(`ghp_[a-zA-Z0-9]{36}`),
			category: "hardcoded_secret",
			severity: "error",
			msg:      "Hardcoded GitHub personal access token detected.",
		},
	}

	for _, f := range files {
		content, err := exec.Command("cat", f).Output()
		if err != nil {
			continue
		}
		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			for _, p := range patterns {
				if p.re.MatchString(line) {
					diagnostics = append(diagnostics, Diagnostic{
						File:     f,
						Line:     i + 1,
						Message:  p.msg,
						Category: p.category,
						Severity: p.severity,
						Tool:     "security-scan",
					})
				}
			}
		}
	}

	return diagnostics
}
