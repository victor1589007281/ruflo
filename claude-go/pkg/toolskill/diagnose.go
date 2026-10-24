// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/anthropic/claude-go/pkg/contract"
	"github.com/anthropic/claude-go/pkg/textutil"
)

// GoRepairDiagnosis diagnoses compilation errors and outputs repair strategies.
// Does not depend on external tools — directly parses compiler error text and combines with Contract Store context to produce actionable repair instructions.
type GoRepairDiagnosis struct {
	Base
	store *contract.Store
}

// NewGoRepairDiagnosis 构造 Go 编译错误诊断技能: 纯文本分析, 不依赖外部工具。
//
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

// Execute 解析输入并返回诊断建议, 同时把 category/confidence 提到顶层便于调用方分流。
//
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

	// 注意：in.RepoRoot 解析了却从未参与诊断 (diagnose 不接收它), 形同虚设。
	suggestion := t.diagnose(in.ErrorText, in.FilePath, in.ContractHint, in.SourceSnippet)

	return map[string]any{
		"suggestion":     suggestion,
		"error_category": suggestion.Category,
		"confidence":     suggestion.Confidence,
	}, nil
}

// diagnose 用有序正则规则表匹配错误文本, 命中即返回该规则的分类/动作/置信度。
//
// 算法解读：
//
//	原理：把编译错误、竞态报告等文本按一张**有序规则表**逐条正则匹配, 第一条命中的规则
//	      决定 Category/Action/Confidence/Instructions; 一条都不命中则降级为 other。
//	关键不变量：规则顺序必须**从具体到宽泛**, 且命中即 return —— 例如 `undefined: X`
//	      (第 1 条) 必须排在 `X.Y undefined` (第 5 条) 之前, 否则字段错误会被误判成
//	      未定义符号; 调换两条的顺序会静默改变分类结果, 不会有任何报错。
//	代价与边界：8 条正则写在函数体内, **每次调用都重新 MustCompile** (固定编译开销);
//	      置信度是写死的经验值 (0.3~0.95) 而非统计得出, 只表达"这类错误有多好修"。
//	设计权衡：只输出**单条**最优建议, 不做多假设排序 —— 上游要的是可直接执行的修复指令,
//	      而不是一组待评估的候选; 代价是命中靠前规则后, 后面的信息 (如同时存在竞态)
//	      会被完全丢弃。
func (t *GoRepairDiagnosis) diagnose(errorText, filePath, contractHint, snippet string) RepairSuggestion {
	// 初始 0.7 只是占位值: 命中规则会被覆盖, 未命中则被 fallback 覆盖成 0.3 —— 它不会出现在返回值里。
	// 注意：TargetLine 从未被赋值, 恒为 0, 调用方不能拿它定位行号。
	// 注意：形参 snippet 在本函数内从未被读取 —— Execute 有传, 但规则表不看源码片段。
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
			// 只有带捕获组的规则才能把符号名填进模板; 不带组的模板本身就是完整句子。
			if len(m) > 1 {
				s.Diagnosis = fmt.Sprintf(p.msgTemplate, m[1])
				s.Instructions = fmt.Sprintf(p.instrTmpl, m[1])
				// 第二个及以后的捕获组拼成 details, 避免丢掉 (如 does not implement 的接口名)。
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
	// 未识别错误置信度压到 0.3 —— 让调用方知道这条基本不可信, 应转人工或 Context Engine。
	s.Category = "other"
	s.Diagnosis = fmt.Sprintf("Unrecognized error. Analyze manually: %s", truncate(errorText, 200))
	s.Action = "consult_contract"
	s.Instructions = "No automatic diagnosis available. Use Context Engine to retrieve relevant definitions, then craft minimal fix."
	s.Confidence = 0.3
	return s
}

// truncate 把文本按字符 (rune) 截断到 n 个字符, 超出部分以 "..." 结尾。
func truncate(s string, n int) string {
	return textutil.Truncate(s, n, "...")
}
