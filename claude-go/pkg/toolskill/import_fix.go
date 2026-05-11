// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// GoImportResolution resolves import issues.
// Uses goimports as the primary tool (fallback to go get + manual analysis).
type GoImportResolution struct {
	Base
	runtime *Runtime
}

// NewGoImportResolution creates a new GoImportResolution.
func NewGoImportResolution(rt *Runtime) *GoImportResolution {
	return &GoImportResolution{
		Base: NewBase(Meta{
			Name:           "go-import-resolution",
			Description:    "Resolve Go import conflicts, missing packages, and unused imports. Uses goimports as primary tool (fallback to go get + manual analysis). Returns structured import changes.",
			Version:        "1.0.0",
			ReadOnly:       false,
			SafeConcurrent: false,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root":       {"type": "string"},
					"problem":         {"type": "string"},
					"affected_files":  {"type": "array", "items": {"type": "string"}},
					"preferred_alias": {"type": "string"}
				},
				"required": ["repo_root", "problem"]
			}`),
		}),
		runtime: rt,
	}
}

// ImportChange represents an import change.
type ImportChange struct {
	File      string `json:"file"`
	OldImport string `json:"old_import,omitempty"`
	NewImport string `json:"new_import"`
	Alias     string `json:"alias,omitempty"`
}

// ModChange represents a go.mod change.
type ModChange struct {
	Action  string `json:"action"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}

// Execute runs the import resolution.
func (t *GoImportResolution) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot       string   `json:"repo_root"`
		Problem        string   `json:"problem"`
		AffectedFiles  []string `json:"affected_files"`
		PreferredAlias string   `json:"preferred_alias"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	var importChanges []ImportChange
	var modChanges []ModChange
	resolution := ""

	// Strategy 1: Try goimports auto-fix for all affected files
	if adapter, ok := t.runtime.adapters["import-fix"]; ok {
		for _, f := range in.AffectedFiles {
			path := filepath.Join(in.RepoRoot, f)
			_, _ = adapter.Execute(ctx, AdapterArgs{RepoRoot: in.RepoRoot, Files: []string{path}})
		}
	}

	// Strategy 2: Missing package -> go get
	if m := regexp.MustCompile(`no required module provides package (\S+)`).FindStringSubmatch(in.Problem); len(m) > 1 {
		pkgPath := m[1]
		resolution = fmt.Sprintf("Add dependency %s", pkgPath)
		modChanges = append(modChanges, ModChange{Action: "add_require", Path: pkgPath})
		cmd := exec.CommandContext(ctx, "go", "get", pkgPath)
		cmd.Dir = in.RepoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("go get %s failed: %w\n%s", pkgPath, err, string(out))
		}
	}

	// Strategy 3: Ambiguous import
	if regexp.MustCompile(`ambiguous import`).MatchString(in.Problem) {
		resolution = "Use explicit alias for conflicting import"
		for _, f := range in.AffectedFiles {
			content, _ := os.ReadFile(filepath.Join(in.RepoRoot, f))
			lines := strings.Split(string(content), "\n")
			for i, line := range lines {
				if strings.Contains(line, `"`) && strings.HasPrefix(strings.TrimSpace(line), `"`) {
					alias := in.PreferredAlias
					if alias == "" {
						alias = "pkg" + fmt.Sprintf("%d", i)
					}
					lines[i] = fmt.Sprintf("%s %s", alias, line)
					importChanges = append(importChanges, ImportChange{
						File: f, NewImport: strings.TrimSpace(line), Alias: alias,
					})
					break
				}
			}
		}
	}

	// Strategy 4: imported and not used
	if m := regexp.MustCompile(`imported and not used: "([^"]+)"`).FindStringSubmatch(in.Problem); len(m) > 1 {
		resolution = fmt.Sprintf("Remove unused import %s", m[1])
		for _, f := range in.AffectedFiles {
			importChanges = append(importChanges, ImportChange{File: f, OldImport: m[1]})
		}
	}

	return map[string]any{
		"resolution":     resolution,
		"import_changes": importChanges,
		"mod_changes":    modChanges,
		"manual_review":  len(importChanges) == 0 && len(modChanges) == 0,
	}, nil
}
