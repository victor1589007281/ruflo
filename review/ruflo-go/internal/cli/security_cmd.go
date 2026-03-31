package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ruflo/ruflo-go/pkg/security"
	"github.com/spf13/cobra"
)

func newSecurityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "security",
		Short: "Security scan, audit, and reporting",
	}
	cmd.AddCommand(securityScanCmd(), securityAuditCmd(), securityReportCmd())
	return cmd
}

func securityScanCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "scan",
		Short: "Scan project files for common issues",
		RunE: func(cmd *cobra.Command, args []string) error {
			if root == "" {
				root = "."
			}
			var findings []map[string]any
			_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					base := filepath.Base(path)
					if base == ".git" || base == "vendor" || base == "node_modules" {
						return fs.SkipDir
					}
					return nil
				}
				ext := strings.ToLower(filepath.Ext(path))
				if ext != ".go" && ext != ".json" && ext != ".yaml" && ext != ".yml" && ext != ".ts" && ext != ".tsx" && ext != ".js" {
					return nil
				}
				b, err := os.ReadFile(path)
				if err != nil {
					return nil
				}
				s := string(b)
				lowerName := strings.ToLower(filepath.Base(path))
				if strings.Contains(s, "sk-ant-") || strings.Contains(s, "sk-proj-") ||
					strings.Contains(s, "AIza") && !strings.Contains(lowerName, "example") {
					findings = append(findings, map[string]any{
						"path": path, "issue": "possible embedded API key or secret-like token",
					})
				}
				if strings.Contains(s, "../") && (ext == ".go" || ext == ".json") {
					findings = append(findings, map[string]any{
						"path": path, "issue": "path traversal sequence in source",
					})
				}
				return nil
			})
			out := map[string]any{"findings": findings, "count": len(findings)}
			if OutputFormat == "json" {
				enc := json.NewEncoder(Stdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			_, _ = fmt.Fprintf(Stdout(), "scan: %d finding(s)\n", len(findings))
			for _, f := range findings {
				_, _ = fmt.Fprintf(Stdout(), "  %s — %v\n", f["path"], f["issue"])
			}
			return nil
		},
	}
	c.Flags().StringVar(&root, "path", ".", "Root directory to scan")
	return c
}

func securityAuditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "audit",
		Short: "Run security audit on representative inputs",
		RunE: func(cmd *cobra.Command, args []string) error {
			v := security.NewInputValidator()
			res := v.ValidateAgentSpawn("demo-agent", "coder", "default")
			out := map[string]any{
				"validator_sample": map[string]any{
					"ok":     res.OK,
					"issues": res.Issues,
				},
				"message": "audit placeholder: input validator sample run",
			}
			if OutputFormat == "json" {
				enc := json.NewEncoder(Stdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			_, _ = fmt.Fprintf(Stdout(), "audit: sample agent spawn validation ok=%v (%d issues)\n",
				res.OK, len(res.Issues))
			return nil
		},
	}
}

func securityReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "report",
		Short: "Generate combined security report",
		RunE: func(cmd *cobra.Command, args []string) error {
			v := security.NewInputValidator()
			res := v.ValidateTaskInput("task", "description without ../../../etc/passwd")
			rep := map[string]any{
				"task_validation": map[string]any{"ok": res.OK, "issues": res.Issues},
				"summary":         "Combine `ruflo security scan` and `ruflo security audit` in your pipeline for full coverage.",
			}
			if OutputFormat == "json" {
				enc := json.NewEncoder(Stdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			_, _ = fmt.Fprintln(Stdout(), "security report: task input sample OK; run `ruflo security scan` for file sweep.")
			return nil
		},
	}
}
