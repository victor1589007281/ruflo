package agent


import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/toolskill"
)

// GateStage 验证阶段
type GateStage string

const (
	GateCompile     GateStage = "compile"     // go build / go test compile
	GateStatic      GateStage = "static"      // go vet / staticcheck
	GateTest        GateStage = "test"        // go test
	GateFormat      GateStage = "format"      // gofmt / goimports
	GateQuality     GateStage = "quality"     // repo-quality-gate
	GateSecurity    GateStage = "security"    // security scan
	GateConcurrency GateStage = "concurrency" // concurrency check
	GatePerformance GateStage = "performance" // benchmark gate
)

// ConstitutionViolation tracks violations of the Go Code Constitution.
type ConstitutionViolation struct {
	Rule    string `json:"rule"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// GateResult 验证结果
type GateResult struct {
	Passed      bool                   `json:"passed"`
	Stage       GateStage              `json:"stage,omitempty"` // 首次失败的阶段
	Diagnostics []toolskill.Diagnostic `json:"diagnostics,omitempty"`
	Blockers    []GateBlocker          `json:"blockers,omitempty"`
	Warnings    []GateWarning          `json:"warnings,omitempty"`
	Evidence    map[string]any         `json:"evidence"` // 各 skill 原始输出
	Duration    time.Duration          `json:"duration"`
	Round       int                    `json:"round"`
}

// GateBlocker 阻断项
type GateBlocker struct {
	Stage   string `json:"stage"`
	Rule    string `json:"rule"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
}

// GateWarning 警告项
type GateWarning struct {
	Stage   string `json:"stage"`
	Rule    string `json:"rule"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// ValidationGate 验证门禁
type ValidationGate struct {
	mu          sync.RWMutex
	stages      []GateConfig          // 验证阶段配置（有序）
	hardStages  map[GateStage]bool    // 哪些 stage 失败必须阻断
	maxRounds   int                   // 单 work unit 最大修复轮数
	roundBudget int                   // 当前已用轮数
	autoFix     map[string]bool       // 哪些 rule 可以自动修复
}

// GateConfig 阶段配置
type GateConfig struct {
	Stage       GateStage `json:"stage"`
	SkillName   string    `json:"skill_name"`   // 对应的 Tool Skill 名
	Required    bool      `json:"required"`     // 是否必须执行
	AutoRetry   bool      `json:"auto_retry"`   // 失败时是否允许自动修复
	MaxAutoFix  int       `json:"max_auto_fix"` // 自动修复最大尝试次数
}

// DefaultGateConfig 返回默认验证阶段配置（5 阶段，保留向后兼容）
func DefaultGateConfig() []GateConfig {
	return []GateConfig{
		{Stage: GateFormat, SkillName: "go-static-check", Required: true, AutoRetry: true, MaxAutoFix: 1},
		{Stage: GateCompile, SkillName: "go-static-check", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateStatic, SkillName: "go-static-check", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateTest, SkillName: "go-static-check", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateQuality, SkillName: "repo-quality-gate", Required: true, AutoRetry: false, MaxAutoFix: 0},
	}
}

// DefaultGateConfigV2 返回 8 阶段验证配置
func DefaultGateConfigV2() []GateConfig {
	return []GateConfig{
		{Stage: GateFormat, SkillName: "go-format", Required: true, AutoRetry: true, MaxAutoFix: 1},
		{Stage: GateCompile, SkillName: "go-static-check", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateStatic, SkillName: "go-static-check", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateSecurity, SkillName: "security-scan", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateConcurrency, SkillName: "concurrency-check", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateTest, SkillName: "repo-quality-gate", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GatePerformance, SkillName: "benchmark-gate", Required: false, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateQuality, SkillName: "repo-quality-gate", Required: true, AutoRetry: false, MaxAutoFix: 0},
	}
}

// NewValidationGate 创建验证门禁
// Deprecated: 本类型无任何生产调用方, 从未执行过。它属于"契约优先编码链"
// (CodeExecutor + PatchApplier + ValidationGate + ContextEngine, 约 1700 行,
// 零测试)。该链复制的编译/测试门禁在 teams.go 已有真实且更完善的实现
// (runGlobalCompileGate / runGlobalTestGate / runGlobalConsistencyCheck +
// tryGateWithRemediation 两次自动修复), 且它原本宿主在 pkg/orchestrator 之上,
// 而该包已于 design/01 M4 删除 (本链的签名因此改用本地 DeprecatedCodeTaskSpec)。链内已知阻塞缺陷见 design/PROGRESS.md 偏差记录。
// 勿在其上继续开发; 需要真实质量门禁请用 pkg/toolskill 接 pkg/graph 的 gate 节点。
func NewValidationGate(config []GateConfig, maxRounds int) *ValidationGate {
	hard := make(map[GateStage]bool)
	for _, s := range []GateStage{GateCompile, GateStatic, GateTest, GateQuality, GateSecurity, GateConcurrency} {
		hard[s] = true
	}
	return &ValidationGate{
		stages:     config,
		hardStages: hard,
		maxRounds:  maxRounds,
		autoFix: map[string]bool{
			"unused_import": true,
			"format":        true,
		},
	}
}

// Validate 按阶段顺序执行验证
func (vg *ValidationGate) Validate(
	ctx context.Context,
	runtime *toolskill.Runtime,
	req toolskill.SkillRequest,
) (*GateResult, error) {
	start := time.Now()
	result := &GateResult{
		Passed:   true,
		Evidence: make(map[string]any),
		Round:    vg.roundBudget,
	}

	for _, cfg := range vg.stages {
		if !cfg.Required {
			continue
		}

		reqJSON, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("stage %s marshal request error: %w", cfg.Stage, err)
		}
		skillResult := runtime.Execute(ctx, cfg.SkillName, json.RawMessage(reqJSON))

		result.Evidence[string(cfg.Stage)] = skillResult

		if skillResult.Status == "pass" {
			continue
		}

		// 失败处理
		result.Passed = false
		result.Stage = cfg.Stage

		for _, d := range skillResult.Diagnostics {
			if d.Severity == "blocking" || vg.hardStages[cfg.Stage] {
				result.Blockers = append(result.Blockers, GateBlocker{
					Stage:   string(cfg.Stage),
					Rule:    d.Category,
					File:    d.File,
					Line:    d.Line,
					Column:  d.Column,
					Message: d.Message,
				})
			} else {
				result.Warnings = append(result.Warnings, GateWarning{
					Stage:   string(cfg.Stage),
					Rule:    d.Category,
					File:    d.File,
					Line:    d.Line,
					Message: d.Message,
				})
			}
		}

		result.Diagnostics = append(result.Diagnostics, skillResult.Diagnostics...)

		// Hard stage 有阻断项 → 立即停止
		if vg.hardStages[cfg.Stage] && len(result.Blockers) > 0 {
			result.Duration = time.Since(start)
			return result, nil
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}

// CanProceed 判断是否允许继续修复
func (vg *ValidationGate) CanProceed(result *GateResult) bool {
	vg.mu.Lock()
	defer vg.mu.Unlock()
	vg.roundBudget++
	if vg.roundBudget >= vg.maxRounds {
		return false
	}
	return !result.Passed
}

// GetRoundBudget 获取当前轮数
func (vg *ValidationGate) GetRoundBudget() int {
	vg.mu.RLock()
	defer vg.mu.RUnlock()
	return vg.roundBudget
}

// ResetRoundBudget 重置轮数
func (vg *ValidationGate) ResetRoundBudget() {
	vg.mu.Lock()
	defer vg.mu.Unlock()
	vg.roundBudget = 0
}

// IsAutoFixable 判断某个 rule 是否可以自动修复
func (vg *ValidationGate) IsAutoFixable(rule string) bool {
	vg.mu.RLock()
	defer vg.mu.RUnlock()
	return vg.autoFix[rule]
}

// AddAutoFixRule 添加自动修复规则
func (vg *ValidationGate) AddAutoFixRule(rule string) {
	vg.mu.Lock()
	defer vg.mu.Unlock()
	vg.autoFix[rule] = true
}

// HasPerformanceRegression returns true if the performance gate detected regressions.
func (gr *GateResult) HasPerformanceRegression() bool {
	if gr == nil || gr.Evidence == nil {
		return false
	}
	perf, ok := gr.Evidence[string(GatePerformance)]
	if !ok {
		return false
	}
	m, ok := perf.(map[string]any)
	if !ok {
		return false
	}
	v, ok := m["performance_regression"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// Summary 返回验证结果的文本摘要
func (gr *GateResult) Summary() string {
	if gr.Passed {
		return fmt.Sprintf("Validation passed (%d rounds, %s)", gr.Round, gr.Duration)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Validation FAILED at stage '%s' (round %d, %s)\n", gr.Stage, gr.Round, gr.Duration))
	sb.WriteString(fmt.Sprintf("Blockers: %d, Warnings: %d\n", len(gr.Blockers), len(gr.Warnings)))
	for _, b := range gr.Blockers {
		sb.WriteString(fmt.Sprintf("  [BLOCKER] %s:%d %s: %s\n", b.File, b.Line, b.Rule, b.Message))
	}
	return sb.String()
}

// GetTopBlocker 获取优先级最高的阻断项
func (gr *GateResult) GetTopBlocker() *GateBlocker {
	if len(gr.Blockers) == 0 {
		return nil
	}
	return &gr.Blockers[0]
}

// ShouldRetry 判断是否值得重试
func (gr *GateResult) ShouldRetry() bool {
	if gr.Passed {
		return false
	}
	// 如果有明确可修复的 blocker，值得重试
	for _, b := range gr.Blockers {
		switch b.Rule {
		case "undefined", "missing_method", "wrong_import", "type_mismatch",
			"wrong_struct_field", "constructor_stub":
			return true
		}
	}
	return false
}

// ── Karpathy 原则扩展检查 ──

// GateSimplicity 代码复杂度检查阶段
func (vg *ValidationGate) CheckSimplicity(
	ctx context.Context,
	changedFiles []string,
	budget int, // 最大允许变更行数
) *GateResult {
	result := &GateResult{Passed: true, Stage: "simplicity"}

	totalAdditions := 0
	totalDeletions := 0
	for _, f := range changedFiles {
		additions, deletions := countDiffLines(f)
		totalAdditions += additions
		totalDeletions += deletions
	}

	// Simplicity 检查 1: 行数预算
	if totalAdditions+totalDeletions > budget {
		result.Passed = false
		result.Blockers = append(result.Blockers, GateBlocker{
			Stage:   "simplicity",
			Rule:    "line_budget_exceeded",
			Message: fmt.Sprintf("Changed lines %d exceeds budget %d (add: %d, del: %d)", totalAdditions+totalDeletions, budget, totalAdditions, totalDeletions),
		})
	}

	// Simplicity 检查 2: 膨胀检测（新增行是删除行的 5 倍以上）
	if totalDeletions > 0 && totalAdditions/totalDeletions > 5 {
		result.Warnings = append(result.Warnings, GateWarning{
			Stage:   "simplicity",
			Rule:    "code_bloat_detected",
			Message: fmt.Sprintf("Code bloat: %d additions vs %d deletions (ratio %.1f:1)", totalAdditions, totalDeletions, float64(totalAdditions)/float64(totalDeletions)),
		})
	}

	return result
}

// GateSurgical 变更范围审计阶段
func (vg *ValidationGate) CheckSurgical(
	ctx context.Context,
	declaredFiles []string,
	actualFiles []string,
	patchResults map[string][]string, // file -> [before_context, after_context]
) *GateResult {
	result := &GateResult{Passed: true, Stage: "surgical"}

	// Surgical 检查 1: 是否修改了未声明的文件
	declaredSet := make(map[string]bool)
	for _, f := range declaredFiles {
		declaredSet[f] = true
	}
	for _, f := range actualFiles {
		if !declaredSet[f] {
			result.Passed = false
			result.Blockers = append(result.Blockers, GateBlocker{
				Stage:   "surgical",
				Rule:    "undeclared_file_modified",
				File:    f,
				Message: fmt.Sprintf("File %s was modified but not declared in Change Scope", f),
			})
		}
	}

	// Surgical 检查 2: 相邻代码保护（检测附带损伤）
	for file, contexts := range patchResults {
		if len(contexts) >= 2 {
			before := contexts[0]
			after := contexts[1]
			// 简单启发：如果替换块前后 3 行的文本发生非预期变化
			if hasCollateralDamage(before, after) {
				result.Warnings = append(result.Warnings, GateWarning{
					Stage:   "surgical",
					Rule:    "collateral_damage_detected",
					File:    file,
					Message: "Detected changes to adjacent code outside the declared scope",
				})
			}
		}
	}

	return result
}

// GateThinkPhase Think Phase 存在性检查
func (vg *ValidationGate) CheckThinkPhase(output string) *GateResult {
	result := &GateResult{Passed: true, Stage: "think_phase"}
	if !strings.Contains(output, "<think>") || !strings.Contains(output, "</think>") {
		result.Passed = false
		result.Blockers = append(result.Blockers, GateBlocker{
			Stage:   "think_phase",
			Rule:    "missing_think_phase",
			Message: "LLM output is missing the required <think>...</think> block. Rejecting output.",
		})
	}
	return result
}

// countDiffLines 统计文件的增删行数（简化实现）
func countDiffLines(filePath string) (additions, deletions int) {
	// 实际实现应调用 git diff 或解析 patch
	// 这里提供接口占位
	return 0, 0
}

// CheckConstitution scans code for constitutional violations (hard-coded crypto/md5, bare map access, context.Background in business code).
func (vg *ValidationGate) CheckConstitution(files []string) ([]ConstitutionViolation, error) {
	var violations []ConstitutionViolation

	weakCryptoRe := regexp.MustCompile(`"crypto/md5"|"crypto/sha1"`)
	bareMapRe := regexp.MustCompile(`map\[\S+\]\S+\s*\[`)
	backgroundRe := regexp.MustCompile(`context\.Background\(\)`)
	syncMutexRe := regexp.MustCompile(`sync\.Mutex|sync\.RWMutex|sync\.Map`)

	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			continue // skip unreadable files
		}
		lines := strings.Split(string(content), "\n")
		base := filepath.Base(f)

		for i, line := range lines {
			lineNo := i + 1

			if weakCryptoRe.MatchString(line) {
				violations = append(violations, ConstitutionViolation{
					Rule:    "weak_crypto",
					File:    f,
					Line:    lineNo,
					Message: fmt.Sprintf("Weak cryptographic import detected in %s", base),
				})
			}

			if bareMapRe.MatchString(line) {
				// Heuristic: look for sync.Mutex anywhere in the file
				hasSync := syncMutexRe.Match(content)
				if !hasSync {
					violations = append(violations, ConstitutionViolation{
						Rule:    "bare_map_access",
						File:    f,
						Line:    lineNo,
						Message: fmt.Sprintf("Bare map access without synchronization in %s", base),
					})
				}
			}

			if backgroundRe.MatchString(line) {
				if !strings.Contains(base, "_test.go") && base != "main.go" {
					violations = append(violations, ConstitutionViolation{
						Rule:    "hardcoded_background",
						File:    f,
						Line:    lineNo,
						Message: fmt.Sprintf("context.Background() used outside main/init/test in %s", base),
					})
				}
			}
		}
	}

	return violations, nil
}

// hasCollateralDamage 检测替换前后的附带损伤
func hasCollateralDamage(before, after string) bool {
	// 提取替换块前后各 3 行作为上下文
	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")
	if len(beforeLines) < 3 || len(afterLines) < 3 {
		return false
	}
	// 如果前后上下文不一致，说明有附带修改
	prefixMatch := beforeLines[0] == afterLines[0]
	suffixMatch := beforeLines[len(beforeLines)-1] == afterLines[len(afterLines)-1]
	return !prefixMatch || !suffixMatch
}
