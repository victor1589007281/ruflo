package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/orchestrator"
	"github.com/anthropic/claude-go/pkg/toolskill"
)

// CodeTask 代码生成/修复任务
type CodeTask struct {
	ID          string   `json:"id"`
	Objective   string   `json:"objective"`
	TargetFiles []string `json:"target_files,omitempty"`
	Language    string   `json:"language"`
	RepoRoot    string   `json:"repo_root"`
	ModulePath  string   `json:"module_path,omitempty"`
}

// CodeResult 执行结果
type CodeResult struct {
	TaskID     string          `json:"task_id"`
	Status     string          `json:"status"` // success/failed/timeout
	Patches    []PatchSummary  `json:"patches"`
	Validation *GateResult     `json:"validation"`
	Trace      []ExecutionStep `json:"trace"`
	RoundCount int             `json:"round_count"`
	Duration   time.Duration   `json:"duration"`
	Error      string          `json:"error,omitempty"`
}

// PatchSummary 补丁摘要
type PatchSummary struct {
	File      string `json:"file"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Snippet   string `json:"snippet,omitempty"`
}

// ExecutionStep 执行步骤
type ExecutionStep struct {
	Round    int            `json:"round"`
	Action   string         `json:"action"`
	Duration time.Duration  `json:"duration"`
	Result   string         `json:"result"`
	Details  map[string]any `json:"details,omitempty"`
}

// CodeExecutor 代码执行器（实现 orchestrator.TaskRunner）
type CodeExecutor struct {
	contractStore    *ContractStore
	patchApplier     *PatchApplier
	validationGate   *ValidationGate
	contextEngine    *ContextEngine
	skillRuntime     *toolskill.Runtime
	llmRunner        orchestrator.LLMClient
	maxRounds        int
	promptSkills     []string
	constitutionPath string
}

func NewCodeExecutor(
	cs *ContractStore,
	pa *PatchApplier,
	vg *ValidationGate,
	ce *ContextEngine,
	sr *toolskill.Runtime,
	llm orchestrator.LLMClient,
) *CodeExecutor {
	return &CodeExecutor{
		contractStore:  cs,
		patchApplier:   pa,
		validationGate: vg,
		contextEngine:  ce,
		skillRuntime:   sr,
		llmRunner:      llm,
		maxRounds:      vg.maxRounds,
		promptSkills:   []string{"go-repair-strategy", "contract-first-coding"},
	}
}

func (e *CodeExecutor) Name() string { return "code-executor" }

func (e *CodeExecutor) Execute(
	ctx context.Context,
	task *orchestrator.Task,
	bb orchestrator.ReadOnlyBlackboard,
) (any, error) {
	codeTask := e.parseCodeTask(task, bb)
	start := time.Now()
	result := &CodeResult{
		TaskID: codeTask.ID,
		Status: "failed",
		Trace:  make([]ExecutionStep, 0),
	}

	if err := e.initContractStore(codeTask); err != nil {
		result.Error = err.Error()
		return result, err
	}

	if err := e.PreFlightCompileCheck(codeTask); err != nil {
		result.Error = err.Error()
		return result, err
	}

	e.validationGate.ResetRoundBudget()

	for round := 0; round < e.maxRounds; round++ {
		roundStart := time.Now()
		var snippets []EditSnippet
		var stepErr error

		if round == 0 {
			snippets, stepErr = e.generateCode(ctx, codeTask)
		} else {
			// Try self-refine first for auto-fixable blockers
			if result.Validation != nil && !result.Validation.Passed && len(result.Validation.Diagnostics) > 0 {
				autoFixable := true
				for _, d := range result.Validation.Diagnostics {
					if d.Severity == "blocking" {
						switch d.Category {
						case "undefined", "wrong_import", "format":
							// auto-fixable
						default:
							autoFixable = false
						}
					}
				}
				if autoFixable {
					refined, refineErr := e.SelfRefine(ctx, codeTask, result.Validation.Diagnostics)
					if refineErr == nil && len(refined) > 0 {
						snippets = refined
						stepErr = nil
						goto ApplySnippets
					}
				}
			}
			snippets, stepErr = e.repairFromDiagnostics(ctx, codeTask, result.Validation)
		}

	ApplySnippets:
		if stepErr != nil {
			action := "generate"
			if round > 0 {
				action = "repair"
			}
			result.Trace = append(result.Trace, ExecutionStep{
				Round:    round,
				Action:   action,
				Duration: time.Since(roundStart),
				Result:   "fail",
				Details:  map[string]any{"error": stepErr.Error()},
			})
			result.Error = stepErr.Error()
			return result, stepErr
		}

		applyStart := time.Now()
		applyResult, err := e.patchApplier.Apply(codeTask.RepoRoot, snippets)
		if err != nil {
			result.Trace = append(result.Trace, ExecutionStep{
				Round:    round,
				Action:   "apply",
				Duration: time.Since(applyStart),
				Result:   "fail",
				Details:  map[string]any{"error": err.Error()},
			})
			result.Error = err.Error()
			return result, err
		}

		result.Trace = append(result.Trace, ExecutionStep{
			Round:    round,
			Action:   "apply",
			Duration: time.Since(applyStart),
			Result:   "success",
			Details:  map[string]any{"changed": applyResult.Changed},
		})

		valStart := time.Now()
		valReq := toolskill.SkillRequest{
			RepoRoot:     codeTask.RepoRoot,
			ChangedFiles: applyResult.Changed,
			Scope:        "package",
			Language:     codeTask.Language,
		}
		gateResult, err := e.validationGate.Validate(ctx, e.skillRuntime, valReq)
		if err != nil {
			result.Error = err.Error()
			return result, err
		}

		result.Validation = gateResult
		result.RoundCount = round + 1

		result.Trace = append(result.Trace, ExecutionStep{
			Round:    round,
			Action:   "validate",
			Duration: time.Since(valStart),
			Result: func() string {
				if gateResult.Passed {
					return "pass"
				}
				return "fail"
			}(),
			Details: map[string]any{
				"stage":    gateResult.Stage,
				"blockers": len(gateResult.Blockers),
				"warnings": len(gateResult.Warnings),
			},
		})

		if gateResult.Passed {
			result.Status = "success"
			result.Duration = time.Since(start)
			result.Patches = e.buildPatchSummaries(codeTask.RepoRoot, snippets)
			return result, nil
		}

		if !gateResult.ShouldRetry() {
			result.Status = "failed"
			result.Duration = time.Since(start)
			result.Error = fmt.Sprintf("unrecoverable errors at stage %s", gateResult.Stage)
			return result, fmt.Errorf("%s", result.Error)
		}

		if !e.validationGate.CanProceed(gateResult) {
			result.Status = "failed"
			result.Duration = time.Since(start)
			result.Error = fmt.Sprintf("max rounds (%d) exceeded", e.maxRounds)
			return result, fmt.Errorf("%s", result.Error)
		}

		changedPkgs := e.contractStore.InferChangedPackages(applyResult.Changed)
		if len(changedPkgs) > 0 {
			e.contractStore.RefreshPackages(changedPkgs)
		}
	}

	result.Status = "failed"
	result.Duration = time.Since(start)
	result.Error = fmt.Sprintf("execution failed after %d rounds", result.RoundCount)
	return result, fmt.Errorf("%s", result.Error)
}

func (e *CodeExecutor) initContractStore(task CodeTask) error {
	if e.contractStore.GetModule() == "" && task.ModulePath != "" {
		e.contractStore.module = task.ModulePath
	}
	if len(e.contractStore.packages) == 0 {
		return e.contractStore.BuildFromRepo()
	}
	return nil
}

func (e *CodeExecutor) generateCode(ctx context.Context, task CodeTask) ([]EditSnippet, error) {
	prompt := e.buildGenerationPrompt(task)
	output, err := e.llmRunner.SimpleComplete(ctx, "", prompt)
	if err != nil {
		return nil, fmt.Errorf("llm generation failed: %w", err)
	}
	return e.parseEditSnippets(output)
}

// SelfRefine attempts to fix diagnostics internally before submitting to Validator.
func (e *CodeExecutor) SelfRefine(ctx context.Context, task CodeTask, diags []toolskill.Diagnostic) ([]EditSnippet, error) {
	var autoFixDiags []toolskill.Diagnostic
	for _, d := range diags {
		if d.Severity != "blocking" {
			continue
		}
		switch d.Category {
		case "undefined", "wrong_import", "format":
			autoFixDiags = append(autoFixDiags, d)
		}
	}
	if len(autoFixDiags) == 0 {
		return nil, fmt.Errorf("no auto-fixable diagnostics")
	}

	var diagBuilder strings.Builder
	for _, d := range autoFixDiags {
		diagBuilder.WriteString(fmt.Sprintf("- [%s] %s:%d: %s\n", d.Category, d.File, d.Line, d.Message))
	}

	prompt := fmt.Sprintf(`You are fixing Go compilation errors via self-refinement.

## Objective
%s

## Auto-fixable Diagnostics
%s

## Rules
1. Fix ONLY the listed diagnostics.
2. Minimal change: total changed lines <= 20.
3. Output changes as a JSON array of EditSnippet objects.
4. Use "//...existing code..." to preserve unchanged parts.
5. Do NOT output explanations outside the JSON.

## Output Format
Return ONLY a JSON array:
[
  {
    "file": "relative/path/to/file.go",
    "start_marker": "func main() {",
    "replacement": "func main() {\n    //...existing code...\n    newCode()\n}"
  }
]`, task.Objective, diagBuilder.String())

	output, err := e.llmRunner.SimpleComplete(ctx, "", prompt)
	if err != nil {
		return nil, fmt.Errorf("llm self-refine failed: %w", err)
	}
	return e.parseEditSnippets(output)
}

// PreFlightCompileCheck verifies that tests compile before generating implementation.
func (e *CodeExecutor) PreFlightCompileCheck(task CodeTask) error {
	if task.RepoRoot == "" {
		return nil
	}
	cmd := exec.Command("go", "test", "-c", "./...")
	cmd.Dir = task.RepoRoot
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	outStr := string(out)
	// Missing stubs or undefined symbols are expected before generation
	if strings.Contains(outStr, "undefined") || strings.Contains(outStr, "not declared") || strings.Contains(outStr, "missing") {
		return nil
	}
	// If no test files exist, also acceptable
	if strings.Contains(outStr, "no non-test Go files") || strings.Contains(outStr, "no test files") {
		return nil
	}
	return fmt.Errorf("pre-flight compile check failed: %s", outStr)
}

func (e *CodeExecutor) repairFromDiagnostics(
	ctx context.Context,
	task CodeTask,
	gate *GateResult,
) ([]EditSnippet, error) {
	if gate == nil || len(gate.Diagnostics) == 0 {
		return nil, fmt.Errorf("no diagnostics to repair from")
	}

	var allChunks []ContextChunk
	var allSuggestions []string

	for _, diag := range gate.Diagnostics {
		if diag.Severity != "blocking" {
			continue
		}
		ctxReq := ContextRequest{
			TaskType:   "fix",
			TargetFile: diag.File,
			TargetLine: diag.Line,
			Diagnostic: diag.Message,
			Category:   diag.Category,
			Symbol:     diag.Code,
		}
		ctxResult, err := e.contextEngine.Retrieve(ctxReq)
		if err != nil {
			continue
		}
		allChunks = append(allChunks, ctxResult.Chunks...)
		allSuggestions = append(allSuggestions, ctxResult.Suggestions...)
	}

	prompt := e.buildRepairPrompt(task, gate, allChunks, allSuggestions)
	output, err := e.llmRunner.SimpleComplete(ctx, "", prompt)
	if err != nil {
		return nil, fmt.Errorf("llm repair failed: %w", err)
	}

	return e.parseEditSnippets(output)
}

func (e *CodeExecutor) buildGenerationPrompt(task CodeTask) string {
	var contractCtx strings.Builder
	for _, file := range task.TargetFiles {
		dir := filepath.Dir(file)
		for pkgPath, pkg := range e.contractStore.packages {
			if pkg.Path == dir || strings.HasPrefix(dir, pkg.Path+"/") {
				contractCtx.WriteString(fmt.Sprintf("// Package: %s\n", pkgPath))
				for _, ti := range pkg.Types {
					if ti.Exported {
						contractCtx.WriteString(fmt.Sprintf("// %s\n", e.contextEngine.formatTypeDefinition(ti)))
					}
				}
			}
		}
	}

	strategy := e.loadPromptSkillContent("contract-first-coding")

	var constitution string
	if e.constitutionPath != "" {
		if data, err := os.ReadFile(e.constitutionPath); err == nil {
			constitution = string(data)
		}
	}

	var prompt strings.Builder
	if constitution != "" {
		prompt.WriteString("## GO CODE CONSTITUTION (Hard Rules - MUST NOT VIOLATE)\n")
		prompt.WriteString(constitution)
		prompt.WriteString("\n\n")
	}

	prompt.WriteString(fmt.Sprintf(`You are a Go code generator working in contract-first mode.

## Objective (WHAT, not HOW)
%s

## Repository
Root: %s
Module: %s

## Contract Context
%s

## Strategy
%s

## ── THINK PHASE (MANDATORY) ──
Before generating ANY code, you MUST think through the problem inside <think>...</think> tags.
Your think block MUST address ALL of the following:

1. **Goal**: Restate the objective in one sentence.
2. **Constraints**: List all interfaces/types from Contract Context that you plan to use.
3. **Change Scope**: Estimate: (a) files to modify, (b) lines to add, (c) lines to delete.
4. **Assumptions**: Declare any assumptions you are making (e.g., "Assuming interface X exists").
5. **Alternatives**: If multiple approaches exist, state why you chose yours.
6. **Stop Condition**: When would you STOP and ask for help instead of continuing?

## ── RULES (Simplicity + Surgical) ──
1. You can ONLY use interfaces/types/functions that exist in the Contract Context.
2. If you need a new interface, you MUST declare it in your output.
3. Constructor return types MUST match interface declarations.
4. **SIMPLICITY**: Do NOT add features beyond what was asked. Do NOT create abstractions for single-use code.
5. **SURGICAL**: Only modify files you declared in Change Scope. Do NOT "improve" adjacent code.
6. Output your changes as a JSON array of EditSnippet objects.
7. Each snippet MUST have: "file", "start_marker", "replacement".
8. Use "//...existing code..." to preserve unchanged parts.
9. Do NOT output explanations outside the JSON.

## ── SUCCESS CRITERIA (MUST ALL PASS) ──
- [ ] Code compiles: `+"`"+`go build ./...`+"`"+` returns zero errors
- [ ] Simplicity: Total changed lines ≤ 200 (generation) or ≤ 50 (repair)
- [ ] Surgical: Only declared files modified; no collateral changes
- [ ] Contract-compliant: All referenced types exist in Contract Context
- [ ] Think Phase present: Output contains <think>...</think> block

## Output Format
First output your <think> block, then return ONLY a JSON array:
[
  {
    "file": "relative/path/to/file.go",
    "start_marker": "func main() {",
    "replacement": "func main() {\n    //...existing code...\n    newCode()\n}"
  }
]`,
		task.Objective, task.RepoRoot, e.contractStore.GetModule(),
		contractCtx.String(), strategy))

	return prompt.String()
}

func (e *CodeExecutor) buildRepairPrompt(
	task CodeTask,
	gate *GateResult,
	chunks []ContextChunk,
	suggestions []string,
) string {
	var diagBuilder strings.Builder
	for _, b := range gate.Blockers {
		diagBuilder.WriteString(fmt.Sprintf("- [%s] %s:%d: %s\n", b.Rule, b.File, b.Line, b.Message))
	}

	var ctxBuilder strings.Builder
	for _, chunk := range chunks {
		if chunk.Relevance >= 0.7 {
			ctxBuilder.WriteString(fmt.Sprintf("// Source: %s (relevance: %.2f)\n%s\n\n",
				chunk.Source, chunk.Relevance, chunk.Content))
		}
	}

	strategy := e.loadPromptSkillContent("go-repair-strategy")

	return fmt.Sprintf(`You are fixing Go compilation errors.

## Objective (WHAT success looks like)
%s

## Validation Failures
%s

## Relevant Context
%s

## Repair Suggestions
%s

## Strategy
%s

## ── THINK PHASE (MANDATORY) ──
Before generating ANY fix, you MUST think inside <think>...</think> tags:

1. **Root Cause**: What is the ACTUAL root cause (not the symptom)?
2. **Minimal Fix**: What is the SMALLEST change that fixes this error?
3. **Change Scope**: Which exact file(s) and line(s) will you modify?
4. **Side Effects**: Could this fix break anything else? How do you know?
5. **Assumptions**: What are you assuming about the codebase?
6. **When to Stop**: If you cannot determine the root cause in 2 sentences, STOP and say so.

## ── RULES (Simplicity + Surgical) ──
1. Fix the ROOT CAUSE, not the symptom.
2. If an interface is missing a method, check the interface definition first.
3. If an import alias conflicts, rename the local import.
4. Do NOT add empty interfaces to bypass compilation.
5. Do NOT add panic("TODO") or empty stubs.
6. **SIMPLICITY**: One error = one change. Do NOT refactor unrelated code.
7. **SURGICAL**: Only modify files you declared in Change Scope.
8. Output changes as a JSON array of EditSnippet objects.
9. Use "//...existing code..." to preserve unchanged parts.

## ── SUCCESS CRITERIA (MUST ALL PASS) ──
- [ ] Root cause fixed: The specific compilation error is resolved
- [ ] Minimal change: Total changed lines ≤ 50
- [ ] No collateral damage: Only declared files modified
- [ ] No regressions: Existing tests still pass
- [ ] Think Phase present: Output contains <think>...</think> block

## Output Format
First output your <think> block, then return ONLY a JSON array of EditSnippet objects.`,
		task.Objective, diagBuilder.String(), ctxBuilder.String(),
		strings.Join(suggestions, "\n"), strategy)
}

func (e *CodeExecutor) parseEditSnippets(output string) ([]EditSnippet, error) {
	output = strings.TrimSpace(output)

	if strings.HasPrefix(output, "```json") {
		output = strings.TrimPrefix(output, "```json")
		output = strings.TrimPrefix(output, "```")
		if idx := strings.LastIndex(output, "```"); idx >= 0 {
			output = output[:idx]
		}
	} else if strings.HasPrefix(output, "```") {
		output = strings.TrimPrefix(output, "```")
		if idx := strings.LastIndex(output, "```"); idx >= 0 {
			output = output[:idx]
		}
	}
	output = strings.TrimSpace(output)

	var snippets []EditSnippet
	if err := json.Unmarshal([]byte(output), &snippets); err != nil {
		return nil, fmt.Errorf("parse snippets: %w (output: %.200s)", err, output)
	}

	for i, s := range snippets {
		if s.File == "" {
			return nil, fmt.Errorf("snippet %d: missing file", i)
		}
		if s.StartMarker == "" {
			return nil, fmt.Errorf("snippet %d: missing start_marker", i)
		}
		if s.Replacement == "" {
			return nil, fmt.Errorf("snippet %d: missing replacement", i)
		}
	}

	return snippets, nil
}

func (e *CodeExecutor) buildPatchSummaries(root string, snippets []EditSnippet) []PatchSummary {
	byFile := make(map[string][]EditSnippet)
	for _, s := range snippets {
		byFile[s.File] = append(byFile[s.File], s)
	}

	var summaries []PatchSummary
	for file, ss := range byFile {
		path := filepath.Join(root, file)
		src, _ := os.ReadFile(path)
		content := string(src)

		additions := 0
		deletions := 0
		for _, s := range ss {
			additions += strings.Count(s.Replacement, "\n")
			if strings.Contains(content, s.StartMarker) {
				startIdx := strings.Index(content, s.StartMarker)
				endIdx := startIdx + len(s.StartMarker)
				if s.EndMarker != "" {
					if eidx := strings.Index(content[startIdx:], s.EndMarker); eidx >= 0 {
						endIdx = startIdx + eidx + len(s.EndMarker)
					}
				}
				deletions += strings.Count(content[startIdx:endIdx], "\n")
			}
		}

		summaries = append(summaries, PatchSummary{
			File:      file,
			Additions: additions,
			Deletions: deletions,
			Snippet:   ss[0].Replacement,
		})
	}
	return summaries
}

func (e *CodeExecutor) parseCodeTask(task *orchestrator.Task, bb orchestrator.ReadOnlyBlackboard) CodeTask {
	ct := CodeTask{ID: task.ID, Language: "go"}
	if task.Config != nil {
		if v, ok := task.Config["objective"].(string); ok {
			ct.Objective = v
		}
		if v, ok := task.Config["repo_root"].(string); ok {
			ct.RepoRoot = v
		}
		if v, ok := task.Config["module_path"].(string); ok {
			ct.ModulePath = v
		}
		if v, ok := task.Config["target_files"].([]string); ok {
			ct.TargetFiles = v
		}
	}
	return ct
}

func (e *CodeExecutor) loadPromptSkillContent(skillName string) string {
	// Try loading from skills/ subdirectory if available
	if e.skillRuntime != nil {
		if data, err := os.ReadFile(filepath.Join("skills", skillName+".md")); err == nil {
			return string(data)
		}
	}
	switch skillName {
	case "contract-first-coding":
		return "1. Declare interfaces before implementations. 2. Constructors must return declared types. 3. No empty interfaces."
	case "go-repair-strategy":
		return "1. Check contract before fixing. 2. Minimal change. 3. No fake implementations."
	default:
		return ""
	}
}

func (e *CodeExecutor) SetMaxRounds(n int) {
	e.maxRounds = n
}

func (e *CodeExecutor) SetPromptSkills(skills []string) {
	e.promptSkills = skills
}

func (e *CodeExecutor) SetConstitutionPath(path string) {
	e.constitutionPath = path
}
