// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/anthropic/claude-go/pkg/contract"
)

// GoRepairDiagnosis diagnoses compilation errors and outputs repair strategies.
// Does not depend on external tools — directly parses compiler error text and combines with Contract Store context to produce actionable repair instructions.
type GoRepairDiagnosis struct {
	Base
	store *contract.Store
}

// NewGoRepairDiagnosis creates a new GoRepairDiagnosis.
func NewGoRepairDiagnosis(store *contract.Store) *GoRepairDiagnosis {
	return &GoRepairDiagnosis{
		Base: NewBase(Meta{
			Name:           "go-repair-diagnosis",
			Description:    "Diagnose Go compilation errors and suggest structured repair strategies. Pure text analysis (no external tools needed) — parses compiler error messages and combines with Contract Store context to produce actionable repair instructions.",
			Version:        "1.0.0",
			ReadOnly:       true,
			SafeConcurrent: true,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"error_text":     {"type": "string"},
					"file_path":      {"type": "string"},
					"repo_root":      {"type": "string"},
					"contract_hint":  {"type": "string"},
					"source_snippet": {"type": "string"}
				},
				"required": ["error_text", "file_path"]
			}`),
		}),
		store: store,
	}
}

// RepairSuggestion represents a repair suggestion.
type RepairSuggestion struct {
	Diagnosis     string  `json:"diagnosis"`
	Category      string  `json:"category"`
	Confidence    float64 `json:"confidence"`
	Action        string  `json:"action"`
	TargetFile    string  `json:"target_file"`
	TargetLine    int     `json:"target_line"`
	Instructions  string  `json:"instructions"`
	ContractCheck string  `json:"contract_check,omitempty"`
}

// Execute runs the diagnosis.
func (t *GoRepairDiagnosis) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		ErrorText     string `json:"error_text"`
		FilePath      string `json:"file_path"`
		RepoRoot      string `json:"repo_root"`
		ContractHint  string `json:"contract_hint"`
		SourceSnippet string `json:"source_snippet"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	suggestion := t.diagnose(in.ErrorText, in.FilePath, in.ContractHint, in.SourceSnippet)

	return map[string]any{
		"suggestion":     suggestion,
		"error_category": suggestion.Category,
		"confidence":     suggestion.Confidence,
	}, nil
}

func (t *GoRepairDiagnosis) diagnose(errorText, filePath, contractHint, snippet string) RepairSuggestion {
	s := RepairSuggestion{TargetFile: filePath, Confidence: 0.7}

	patterns := []struct {
		re          *regexp.Regexp
		category    string
		action      string
		confidence  float64
		msgTemplate string
		instrTmpl   string
	}{
		{
			re:          regexp.MustCompile(`undefined:\s+(\S+)`),
			category:    "undefined_symbol",
			action:      "consult_contract",
			confidence:  0.75,
			msgTemplate: "Symbol %q is undefined. Check imports, typos, or unexported identifiers.",
			instrTmpl:   "Search codebase for %q. If method, check receiver contract. If type, check imports.",
		},
		{
			re:          regexp.MustCompile(`cannot use .* type .* as type`),
			category:    "type_mismatch",
			action:      "change_signature",
			confidence:  0.85,
			msgTemplate: "Type mismatch. Actual type does not match expected type.",
			instrTmpl:   "Identify expected type from contract or function signature. Modify expression or update target signature.",
		},
		{
			re:          regexp.MustCompile(`(.+) does not implement (.+)`),
			category:    "interface_mismatch",
			action:      "add_method",
			confidence:  0.9,
			msgTemplate: "Receiver does not satisfy interface. Missing or mismatched methods.",
			instrTmpl:   "Look up interface in Contract Store. Add missing methods with exact signatures.",
		},
		{
			re:          regexp.MustCompile(`ambiguous import|imported and not used|no required module provides package`),
			category:    "import_conflict",
			action:      "add_import",
			confidence:  0.88,
			msgTemplate: "Import path conflict or missing dependency.",
			instrTmpl:   "Resolve import path. Use explicit alias if ambiguous. Add to go.mod if missing.",
		},
		{
			re:          regexp.MustCompile(`(\S+)\.([A-Za-z0-9_]+) undefined`),
			category:    "field_error",
			action:      "consult_contract",
			confidence:  0.8,
			msgTemplate: "Field or method not found on type.",
			instrTmpl:   "Check type definition. Add field to struct or method to interface (contract-first), then implement.",
		},
		{
			re:          regexp.MustCompile(`too (few|many) arguments`),
			category:    "signature_mismatch",
			action:      "change_signature",
			confidence:  0.9,
			msgTemplate: "Function call argument count mismatch.",
			instrTmpl:   "Check callee's contract or definition. Update call site or function signature.",
		},
		{
			re:          regexp.MustCompile(`DATA RACE|WARNING: DATA RACE`),
			category:    "data_race",
			action:      "add_sync",
			confidence:  0.95,
			msgTemplate: "Data race detected. Shared mutable state accessed without synchronization.",
			instrTmpl:   "Use sync.Mutex, sync.RWMutex, sync.Map, or channels to protect shared state. Prefer channel-based communication over shared memory.",
		},
		{
			re:          regexp.MustCompile(`Goroutine leak|leaked goroutine|goroutine .* leaked`),
			category:    "goroutine_leak",
			action:      "fix_lifecycle",
			confidence:  0.85,
			msgTemplate: "Goroutine leak detected. Goroutine started but never terminates.",
			instrTmpl:   "Ensure goroutines have a cancellation path (context.Done(), close channel, or sync.WaitGroup).",
		},
	}

	for _, p := range patterns {
		if m := p.re.FindStringSubmatch(errorText); m != nil {
			s.Category = p.category
			s.Action = p.action
			s.Confidence = p.confidence
			if len(m) > 1 {
				s.Diagnosis = fmt.Sprintf(p.msgTemplate, m[1])
				s.Instructions = fmt.Sprintf(p.instrTmpl, m[1])
				if len(m) > 2 {
					s.Diagnosis = fmt.Sprintf("%s (details: %s)", s.Diagnosis, strings.Join(m[1:], "."))
				}
			} else {
				s.Diagnosis = p.msgTemplate
				s.Instructions = p.instrTmpl
			}
			if contractHint != "" {
				s.ContractCheck = contractHint
			}
			return s
		}
	}

	// Fallback
	s.Category = "other"
	s.Diagnosis = fmt.Sprintf("Unrecognized error. Analyze manually: %s", truncate(errorText, 200))
	s.Action = "consult_contract"
	s.Instructions = "No automatic diagnosis available. Use Context Engine to retrieve relevant definitions, then craft minimal fix."
	s.Confidence = 0.3
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
