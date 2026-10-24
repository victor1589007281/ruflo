package agent

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ContextRequest 上下文检索请求
type ContextRequest struct {
	TaskType     string   `json:"task_type"` // implement/fix/refactor
	TargetFile   string   `json:"target_file"`
	TargetLine   int      `json:"target_line"`
	Diagnostic   string   `json:"diagnostic"` // 原始诊断文本
	Category     string   `json:"category"`   // missing_method/wrong_import/type_mismatch
	Symbol       string   `json:"symbol"`     // 涉及的符号
	RelatedFiles []string `json:"related_files,omitempty"`
}

// ContextChunk 上下文片段
type ContextChunk struct {
	Source    string  `json:"source"` // contract/import/test/doc
	File      string  `json:"file"`
	Line      int     `json:"line"`
	Content   string  `json:"content"`
	Relevance float64 `json:"relevance"` // 0.0-1.0
}

// ContextResult 检索结果
type ContextResult struct {
	Chunks      []ContextChunk `json:"chunks"`
	SymbolFound bool           `json:"symbol_found"`
	Suggestions []string       `json:"suggestions"`
}

// ContextEngine 上下文检索引擎
type ContextEngine struct {
	store *ContractStore
}

// Deprecated: 本类型无任何生产调用方, 从未执行过。它属于"契约优先编码链"
// (CodeExecutor + PatchApplier + ValidationGate + ContextEngine, 约 1700 行,
// 零测试)。该链复制的编译/测试门禁在 teams.go 已有真实且更完善的实现
// (runGlobalCompileGate / runGlobalTestGate / runGlobalConsistencyCheck +
// tryGateWithRemediation 两次自动修复), 且它原本宿主在 pkg/orchestrator 之上,
// 而该包已于 design/01 M4 删除 (本链的签名因此改用本地 DeprecatedCodeTaskSpec)。链内已知阻塞缺陷见 design/PROGRESS.md 偏差记录。
// 勿在其上继续开发; 需要真实质量门禁请用 pkg/toolskill 接 pkg/graph 的 gate 节点。
func NewContextEngine(store *ContractStore) *ContextEngine {
	return &ContextEngine{store: store}
}

// Retrieve 主入口：根据诊断类型分发到具体检索逻辑
func (ce *ContextEngine) Retrieve(req ContextRequest) (*ContextResult, error) {
	result := &ContextResult{}

	switch req.Category {
	case "missing_method", "interface_mismatch", "not_declared_by_package":
		ce.retrieveMissingMethod(req, result)
	case "wrong_import", "import_alias_conflict", "undefined_import":
		ce.retrieveImportConflict(req, result)
	case "wrong_struct_field", "undefined_field", "missing_field":
		ce.retrieveFieldError(req, result)
	case "type_mismatch", "mismatched_types":
		ce.retrieveTypeMismatch(req, result)
	case "constructor_stub", "fake_implementation", "undefined_type":
		ce.retrieveFakeOrUndefined(req, result)
	case "unused_import":
		ce.retrieveUnusedImport(req, result)
	case "format":
		result.Suggestions = append(result.Suggestions, "Run goimports or gofmt to fix formatting")
	case "data_race", "goroutine_leak", "channel_close", "mutex_deadlock":
		ce.retrieveConcurrencyIssue(req, result)
	default:
		ce.retrieveGeneric(req, result)
	}

	sortChunksByRelevance(result.Chunks)
	return result, nil
}

// retrieveMissingMethod 处理 "X has no method Y"
func (ce *ContextEngine) retrieveMissingMethod(req ContextRequest, result *ContextResult) {
	typeName, methodName := ce.parseMethodSymbol(req.Symbol)
	if typeName == "" {
		return
	}

	found := false
	for pkgPath, pkg := range ce.store.packages {
		if ti, ok := pkg.Types[typeName]; ok {
			found = true
			result.SymbolFound = true

			result.Chunks = append(result.Chunks, ContextChunk{
				Source:    "contract",
				File:      ti.File,
				Line:      ti.Line,
				Content:   ce.formatTypeDefinition(ti),
				Relevance: 1.0,
			})

			if ti.Kind == "interface" {
				iface, _ := ce.store.FindInterface(pkgPath, typeName)
				if iface != nil {
					for _, imp := range iface.Implementors {
						result.Chunks = append(result.Chunks, ContextChunk{
							Source:    "contract",
							File:      "",
							Line:      0,
							Content:   fmt.Sprintf("// Implementor: %s", imp),
							Relevance: 0.7,
						})
					}
					result.Suggestions = append(result.Suggestions,
						fmt.Sprintf("Option 1: Add method '%s' to interface '%s'", methodName, typeName),
						fmt.Sprintf("Option 2: Check if implementor '%s' should have this method", firstOr(iface.Implementors, "<none>")),
					)
				}
			}

			if ti.Kind == "struct" {
				methodNames := make(map[string]bool)
				for _, m := range ti.Methods {
					methodNames[m.Name] = true
				}
				if !methodNames[methodName] {
					result.Suggestions = append(result.Suggestions,
						fmt.Sprintf("Add method '%s' to struct '%s'", methodName, typeName),
						fmt.Sprintf("Existing methods: %s", strings.Join(mapKeys(methodNames), ", ")),
					)
				}
			}

			if req.TargetFile != "" {
				result.Chunks = append(result.Chunks, ContextChunk{
					Source:    "source",
					File:      req.TargetFile,
					Line:      req.TargetLine,
					Content:   fmt.Sprintf("// Call site at %s:%d", req.TargetFile, req.TargetLine),
					Relevance: 0.8,
				})
			}
		}
	}

	if !found {
		result.Suggestions = append(result.Suggestions,
			fmt.Sprintf("Type '%s' not found in contract. Need to define it first (contract-first).", typeName),
		)
	}
}

// retrieveImportConflict 处理 import 冲突
func (ce *ContextEngine) retrieveImportConflict(req ContextRequest, result *ContextResult) {
	alias := req.Symbol
	if alias == "" {
		alias = ce.extractAliasFromDiagnostic(req.Diagnostic)
	}

	for pkgPath, pkg := range ce.store.packages {
		for _, imp := range pkg.Imports {
			if imp.Alias == alias || filepath.Base(imp.Path) == alias {
				result.Chunks = append(result.Chunks, ContextChunk{
					Source:    "contract",
					File:      "",
					Line:      0,
					Content:   fmt.Sprintf("// Package %s imports %q as %q", pkgPath, imp.Path, alias),
					Relevance: 0.9,
				})
			}
		}
	}

	result.Suggestions = append(result.Suggestions,
		fmt.Sprintf("If stdlib '%s' and local pkg both use alias '%s', rename local import:", alias, alias),
		fmt.Sprintf("  import %s \"%s\" // change to: local%s \"...\"", alias, "<local-path>", alias),
	)
}

// retrieveFieldError 处理 struct field 错误
func (ce *ContextEngine) retrieveFieldError(req ContextRequest, result *ContextResult) {
	typeName := req.Symbol
	if typeName == "" {
		typeName = ce.extractTypeFromFieldError(req.Diagnostic)
	}

	for _, pkg := range ce.store.packages {
		if ti, ok := pkg.Types[typeName]; ok && ti.Kind == "struct" {
			result.SymbolFound = true
			result.Chunks = append(result.Chunks, ContextChunk{
				Source:    "contract",
				File:      ti.File,
				Line:      ti.Line,
				Content:   ce.formatTypeDefinition(ti),
				Relevance: 1.0,
			})

			var fieldNames []string
			for _, f := range ti.Fields {
				fieldNames = append(fieldNames, f.Name)
			}
			result.Suggestions = append(result.Suggestions,
				fmt.Sprintf("Struct '%s' has fields: %s", typeName, strings.Join(fieldNames, ", ")),
			)
		}
	}
}

// retrieveTypeMismatch 处理类型不匹配
func (ce *ContextEngine) retrieveTypeMismatch(req ContextRequest, result *ContextResult) {
	expected, actual := ce.extractTypesFromMismatch(req.Diagnostic)

	if expected != "" {
		for _, pkg := range ce.store.packages {
			if ti, ok := pkg.Types[expected]; ok {
				result.Chunks = append(result.Chunks, ContextChunk{
					Source:    "contract",
					File:      ti.File,
					Line:      ti.Line,
					Content:   ce.formatTypeDefinition(ti),
					Relevance: 0.9,
				})
			}
		}
	}

	if actual != "" {
		for _, pkg := range ce.store.packages {
			if ti, ok := pkg.Types[actual]; ok {
				result.Chunks = append(result.Chunks, ContextChunk{
					Source:    "contract",
					File:      ti.File,
					Line:      ti.Line,
					Content:   ce.formatTypeDefinition(ti),
					Relevance: 0.8,
				})
			}
		}
	}

	result.Suggestions = append(result.Suggestions,
		fmt.Sprintf("Expected type: %s, Actual type: %s", expected, actual),
		"Check if a type conversion or wrapper is needed",
	)
}

// retrieveFakeOrUndefined 处理假实现或未定义类型
func (ce *ContextEngine) retrieveFakeOrUndefined(req ContextRequest, result *ContextResult) {
	typeName := req.Symbol
	if typeName == "" {
		typeName = ce.extractTypeFromUndefined(req.Diagnostic)
	}

	found := false
	for pkgPath, pkg := range ce.store.packages {
		if ti, ok := pkg.Types[typeName]; ok {
			found = true
			result.SymbolFound = true
			result.Chunks = append(result.Chunks, ContextChunk{
				Source:    "contract",
				File:      ti.File,
				Line:      ti.Line,
				Content:   ce.formatTypeDefinition(ti),
				Relevance: 1.0,
			})

			if ti.Kind == "interface" && len(ti.Methods) == 0 {
				result.Suggestions = append(result.Suggestions,
					fmt.Sprintf("WARNING: Interface '%s' is empty (likely fake implementation)", typeName),
					fmt.Sprintf("Define real methods for '%s.%s'", pkgPath, typeName),
				)
			}
		}
	}

	if !found {
		matches := ce.store.SearchSymbols(typeName)
		if len(matches) > 0 {
			result.Suggestions = append(result.Suggestions,
				fmt.Sprintf("'%s' not found. Did you mean:", typeName),
			)
			for _, m := range matches[:min(5, len(matches))] {
				result.Suggestions = append(result.Suggestions, fmt.Sprintf("  - %s (%s)", m.Name, m.Kind))
			}
		} else {
			result.Suggestions = append(result.Suggestions,
				fmt.Sprintf("'%s' not found in any package. Define it first (contract-first).", typeName),
			)
		}
	}
}

// retrieveUnusedImport 处理未使用的 import
func (ce *ContextEngine) retrieveUnusedImport(req ContextRequest, result *ContextResult) {
	importPath := req.Symbol
	result.Suggestions = append(result.Suggestions,
		fmt.Sprintf("Import '%s' is unused. Remove it or use it.", importPath),
		"Auto-fix available: goimports -w",
	)
}

// retrieveGeneric 通用检索
func (ce *ContextEngine) retrieveGeneric(req ContextRequest, result *ContextResult) {
	if req.TargetFile == "" {
		return
	}
	dir := filepath.Dir(req.TargetFile)
	for pkgPath, pkg := range ce.store.packages {
		if pkg.Path == dir || strings.HasPrefix(dir, pkg.Path+string(filepath.Separator)) {
			for name, ti := range pkg.Types {
				if ti.Exported {
					result.Chunks = append(result.Chunks, ContextChunk{
						Source:    "contract",
						File:      ti.File,
						Line:      ti.Line,
						Content:   fmt.Sprintf("// %s.%s (%s)", pkgPath, name, ti.Kind),
						Relevance: 0.3,
					})
				}
			}
		}
	}
}

// helper methods
//
// parseMethodSymbol 从 "pkg.Type.Method" 形状的符号串里切出类型名与方法名 (取倒数第二、最后一段)。
func (ce *ContextEngine) parseMethodSymbol(symbol string) (typeName, methodName string) {
	if symbol == "" {
		return "", ""
	}
	parts := strings.Split(symbol, ".")
	if len(parts) >= 2 {
		methodName = parts[len(parts)-1]
		typeName = parts[len(parts)-2]
	}
	return
}

// extractAliasFromDiagnostic 从诊断文本里刮出 import 别名 (取 `":"` 之前那段引号里的内容)。
func (ce *ContextEngine) extractAliasFromDiagnostic(diag string) string {
	if idx := strings.Index(diag, `":"`); idx > 0 {
		start := strings.LastIndex(diag[:idx], `"`)
		if start >= 0 {
			return diag[start+1 : idx]
		}
	}
	return ""
}

// extractTypeFromFieldError 从 "type X has no field Y" 形状的诊断里取 "type " 之后的类型名。
func (ce *ContextEngine) extractTypeFromFieldError(diag string) string {
	if idx := strings.Index(diag, "type "); idx >= 0 {
		return strings.TrimSpace(diag[idx+5:])
	}
	return ""
}

// extractTypesFromMismatch 从类型不匹配诊断里取期望类型 ("as type X") 与实际类型 ("type X)")。
func (ce *ContextEngine) extractTypesFromMismatch(diag string) (expected, actual string) {
	if idx := strings.Index(diag, "as type "); idx >= 0 {
		expected = strings.TrimSpace(diag[idx+8:])
	}
	if idx := strings.Index(diag, "type "); idx >= 0 {
		end := strings.Index(diag[idx+5:], ")")
		if end > 0 {
			actual = strings.TrimSpace(diag[idx+5 : idx+5+end])
		}
	}
	return
}

// extractTypeFromUndefined 从 "undefined: X" 诊断里取未定义的符号名。
func (ce *ContextEngine) extractTypeFromUndefined(diag string) string {
	if idx := strings.Index(diag, "undefined:"); idx >= 0 {
		return strings.TrimSpace(diag[idx+10:])
	}
	return ""
}

// formatTypeDefinition 把契约里的类型信息渲染成可直接塞进提示词的 Go 源码片段
// (struct / interface / 类型别名各一套模板)。
func (ce *ContextEngine) formatTypeDefinition(ti TypeInfo) string {
	var sb strings.Builder
	switch ti.Kind {
	case "struct":
		sb.WriteString(fmt.Sprintf("type %s struct {\n", ti.Name))
		for _, f := range ti.Fields {
			if f.Embedded {
				sb.WriteString(fmt.Sprintf("    %s\n", f.Type))
			} else {
				sb.WriteString(fmt.Sprintf("    %s %s", f.Name, f.Type))
				if f.Tag != "" {
					sb.WriteString(fmt.Sprintf(" %s", f.Tag))
				}
				sb.WriteString("\n")
			}
		}
		sb.WriteString("}")
	case "interface":
		sb.WriteString(fmt.Sprintf("type %s interface {\n", ti.Name))
		for _, m := range ti.Methods {
			sb.WriteString(fmt.Sprintf("    %s%s\n", m.Name, m.Signature))
		}
		sb.WriteString("}")
	case "type-alias":
		sb.WriteString(fmt.Sprintf("type %s %s", ti.Name, ti.Underlying))
	default:
		sb.WriteString(fmt.Sprintf("type %s %s", ti.Name, ti.Kind))
	}
	return sb.String()
}

// retrieveConcurrencyIssue 针对 data_race / goroutine_leak / channel_close / mutex_deadlock
// 四类并发问题给出固定的排查建议, 并尝试附上 race 报告里那个类型的契约定义。
//
// 注意：建议是**静态模板**, 不读本次涉及的现场代码 —— 它只按类别回一份清单, 逐案分析
// 要靠模型自己。extractTypeFromRaceDiagnostic 的提取同样是启发式的 (见该函数)。
func (ce *ContextEngine) retrieveConcurrencyIssue(req ContextRequest, result *ContextResult) {
	// Provide suggestions based on the concurrency issue type
	switch req.Category {
	case "data_race":
		result.Suggestions = append(result.Suggestions,
			"Use sync.Mutex or sync.RWMutex to protect shared state",
			"Consider sync.Map for map-only shared state",
			"Consider channel-based communication instead of shared memory",
			"Use atomic.Value for single-value shared state",
		)
		// Find the type definition for the raced variable
		typeName := ce.extractTypeFromRaceDiagnostic(req.Diagnostic)
		if typeName != "" {
			for _, pkg := range ce.store.packages {
				if ti, ok := pkg.Types[typeName]; ok {
					result.Chunks = append(result.Chunks, ContextChunk{
						Source: "contract", File: ti.File, Line: ti.Line,
						Content: ce.formatTypeDefinition(ti), Relevance: 1.0,
					})
				}
			}
		}
	case "goroutine_leak":
		result.Suggestions = append(result.Suggestions,
			"Pass context.Context to goroutine and check ctx.Done()",
			"Use sync.WaitGroup to track goroutine completion",
			"Use errgroup.Group for structured goroutine management",
			"Add timeout or cancellation to long-running goroutines",
		)
	case "channel_close":
		result.Suggestions = append(result.Suggestions,
			"Only the sender should close the channel",
			"Use sync.Once to ensure channel is closed exactly once",
			"Check if channel is closed before sending with select + default",
		)
	case "mutex_deadlock":
		result.Suggestions = append(result.Suggestions,
			"Ensure mutex unlock happens in defer or on all return paths",
			"Avoid holding multiple mutexes; if necessary, always acquire in the same order",
			"Use sync.RWMutex when reads dominate",
		)
	}
}

// extractTypeFromRaceDiagnostic 从 race detector 输出里刮出疑似类型/变量名。
//
// 注意：取的是第一个含 ".go:" 的行的**最后一个空白分隔字段** (通常是行号或列号), 未必是
// 类型名 —— 这个提取是启发式的, 命中率取决于 race 报告的排版。这里只记录, 不改代码。
func (ce *ContextEngine) extractTypeFromRaceDiagnostic(diag string) string {
	// Extract type/variable name from race detector output
	// e.g., "Previous write at 0x00c0000 by goroutine 8:"
	// Try to find the variable name on subsequent lines
	lines := strings.Split(diag, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, ".go:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[len(parts)-1]
			}
		}
	}
	return ""
}

// sortChunksByRelevance 按 Relevance 从高到低就地重排片段 (选择排序; 片段数量很小,
// 且这样无需引入 sort 依赖)。
func sortChunksByRelevance(chunks []ContextChunk) {
	for i := 0; i < len(chunks); i++ {
		for j := i + 1; j < len(chunks); j++ {
			if chunks[j].Relevance > chunks[i].Relevance {
				chunks[i], chunks[j] = chunks[j], chunks[i]
			}
		}
	}
}

// firstOr 取列表首元素; 列表为空时返回 fallback。
func firstOr(list []string, fallback string) string {
	if len(list) > 0 {
		return list[0]
	}
	return fallback
}

// mapKeys 取出 map 的全部键; 顺序不定, 调用方需要稳定输出时应自行排序。
func mapKeys(m map[string]bool) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// minInt 返回两数中的较小者。
//
// 注意：本文件里真正的截断用的是 Go 1.21 的内建 min (见 retrieveFakeOrUndefined 的
// matches[:min(5, len(matches))]), 这个函数全仓没有任何调用方 —— 是死代码。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
