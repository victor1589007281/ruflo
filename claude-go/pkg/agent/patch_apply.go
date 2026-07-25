package agent


import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EditSnippet 编辑片段。
// 使用 StartMarker 在 AST 中定位节点，Replacement 替换该节点。
// Replacement 中可包含 "//...existing code..." 占位符来保留原内容。
type EditSnippet struct {
	File        string `json:"file"`
	StartMarker string `json:"start_marker"` // 唯一标识定位点（精确匹配代码片段）
	EndMarker   string `json:"end_marker,omitempty"` // 可选结束标记
	Replacement string `json:"replacement"`  // 新代码
}

// ApplyResult 应用结果
type ApplyResult struct {
	Success      bool            `json:"success"`
	Changed      []string        `json:"changed"`
	Conflicts    []EditConflict  `json:"conflicts,omitempty"`
	SyntaxErrors []SyntaxError   `json:"syntax_errors,omitempty"`
}

// EditConflict 编辑冲突
type EditConflict struct {
	File1    string `json:"file1"`
	Marker1  string `json:"marker1"`
	File2    string `json:"file2"`
	Marker2  string `json:"marker2"`
	Reason   string `json:"reason"`
}

// SyntaxError 语法错误
type SyntaxError struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

// PatchApplier AST-aware 补丁应用器
type PatchApplier struct{}

// Deprecated: 本类型无任何生产调用方, 从未执行过。它属于"契约优先编码链"
// (CodeExecutor + PatchApplier + ValidationGate + ContextEngine, 约 1700 行,
// 零测试)。该链复制的编译/测试门禁在 teams.go 已有真实且更完善的实现
// (runGlobalCompileGate / runGlobalTestGate / runGlobalConsistencyCheck +
// tryGateWithRemediation 两次自动修复), 且它原本宿主在 pkg/orchestrator 之上,
// 而该包已于 design/01 M4 删除 (本链的签名因此改用本地 DeprecatedCodeTaskSpec)。链内已知阻塞缺陷见 design/PROGRESS.md 偏差记录。
// 勿在其上继续开发; 需要真实质量门禁请用 pkg/toolskill 接 pkg/graph 的 gate 节点。
func NewPatchApplier() *PatchApplier {
	return &PatchApplier{}
}

// Apply 应用一组编辑片段。
// 流程：1) 分组 2) 冲突检测 3) AST 替换 4) 语法验证 5) gofmt
func (pa *PatchApplier) Apply(root string, snippets []EditSnippet) (*ApplyResult, error) {
	result := &ApplyResult{Success: true}

	if len(snippets) == 0 {
		return result, nil
	}

	// Step 1: 按文件分组
	byFile := make(map[string][]*EditSnippet)
	for i := range snippets {
		s := &snippets[i]
		path := filepath.Join(root, s.File)
		byFile[path] = append(byFile[path], s)
	}

	// Step 2: 冲突检测
	for path, fileSnippets := range byFile {
		conflicts := pa.detectConflicts(path, fileSnippets)
		if len(conflicts) > 0 {
			result.Conflicts = append(result.Conflicts, conflicts...)
			result.Success = false
		}
	}
	if !result.Success {
		return result, fmt.Errorf("detected %d conflicts", len(result.Conflicts))
	}

	// Step 3: 对每个文件执行 AST 替换
	for path, fileSnippets := range byFile {
		if err := pa.applyToFile(path, fileSnippets); err != nil {
			result.Success = false
			return result, fmt.Errorf("apply to %s: %w", path, err)
		}
		result.Changed = append(result.Changed, path)
	}

	// Step 4: 语法验证
	for path := range byFile {
		if err := pa.validateSyntax(path); err != nil {
			result.SyntaxErrors = append(result.SyntaxErrors, SyntaxError{
				File:    path,
				Message: err.Error(),
			})
			result.Success = false
		}
	}
	if !result.Success {
		return result, fmt.Errorf("syntax errors in %d files", len(result.SyntaxErrors))
	}

	// Step 5: gofmt
	if err := pa.gofmtFiles(result.Changed); err != nil {
		// gofmt 失败是 warning，不影响 Success
	}

	return result, nil
}

// detectConflicts 检测同一文件内的编辑冲突
func (pa *PatchApplier) detectConflicts(path string, snippets []*EditSnippet) []EditConflict {
	var conflicts []EditConflict
	for i := 0; i < len(snippets); i++ {
		for j := i + 1; j < len(snippets); j++ {
			s1, s2 := snippets[i], snippets[j]
			if s1.StartMarker == s2.StartMarker {
				conflicts = append(conflicts, EditConflict{
					File1:   path,
					Marker1: s1.StartMarker,
					File2:   path,
					Marker2: s2.StartMarker,
					Reason:  "same start marker",
				})
			}
			// 检查文本范围重叠
			if pa.markersOverlap(s1.StartMarker, s1.EndMarker, s2.StartMarker, s2.EndMarker) {
				conflicts = append(conflicts, EditConflict{
					File1:   path,
					Marker1: s1.StartMarker,
					File2:   path,
					Marker2: s2.StartMarker,
					Reason:  "overlapping ranges",
				})
			}
		}
	}
	return conflicts
}

// markersOverlap 检查两个标记范围是否重叠
func (pa *PatchApplier) markersOverlap(s1, e1, s2, e2 string) bool {
	// 简化：如果 start 相同则重叠
	if s1 == s2 {
		return true
	}
	// 如果 end1 == start2 或 end2 == start1，也算重叠（边界接触）
	if e1 == s2 || e2 == s1 {
		return true
	}
	return false
}

// applyToFile 对单个文件应用编辑（AST 方式）
func (pa *PatchApplier) applyToFile(path string, snippets []*EditSnippet) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	original := string(src)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		// AST 解析失败：回退到文本替换
		return pa.applyTextFallback(path, original, snippets)
	}

	// 尝试 AST 替换
	modified := false
	content := original
	for _, snippet := range snippets {
		// 在 AST 中查找匹配的节点
		node := pa.findNodeByMarker(f, fset, content, snippet.StartMarker, snippet.EndMarker)
		if node == nil {
			// AST 找不到，回退到文本替换
			newContent, ok := pa.applyTextEdit(content, snippet)
			if !ok {
				return fmt.Errorf("start marker not found: %s", snippet.StartMarker)
			}
			content = newContent
			modified = true
			continue
		}

		// 获取节点的文本范围
		start, end := pa.nodeRange(fset, node)
		if start < 0 || end > len(content) || start > end {
			return fmt.Errorf("invalid node range for marker: %s", snippet.StartMarker)
		}

		// 处理占位符
		replacement := snippet.Replacement
		existing := content[start:end]
		replacement = strings.ReplaceAll(replacement, "//...existing code...", existing)
		replacement = strings.ReplaceAll(replacement, "// ... existing code ...", existing)
		replacement = strings.ReplaceAll(replacement, "/*...existing code...*/", existing)

		// 替换
		content = content[:start] + replacement + content[end:]
		modified = true

		// 重新解析 AST（因为文本已改变）
		fset = token.NewFileSet()
		f, err = parser.ParseFile(fset, path, []byte(content), parser.ParseComments)
		if err != nil {
			// 如果替换后语法错误，回退
			return fmt.Errorf("replacement caused syntax error: %w", err)
		}
	}

	if modified {
		return os.WriteFile(path, []byte(content), 0644)
	}
	return nil
}

// findNodeByMarker 在 AST 中查找匹配的节点
func (pa *PatchApplier) findNodeByMarker(f *ast.File, fset *token.FileSet, content, startMarker, endMarker string) ast.Node {
	var found ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		start, end := pa.nodeRange(fset, n)
		if start < 0 || end > len(content) {
			return true
		}
		nodeText := content[start:end]
		if strings.Contains(nodeText, startMarker) {
			// 如果提供了 endMarker，检查是否也包含
			if endMarker == "" || strings.Contains(nodeText, endMarker) {
				found = n
				return false // 停止搜索
			}
		}
		return true
	})
	return found
}

// nodeRange 返回 AST 节点在源文件中的字节范围
func (pa *PatchApplier) nodeRange(fset *token.FileSet, node ast.Node) (start, end int) {
	if node == nil {
		return -1, -1
	}
	pos := node.Pos()
	if !pos.IsValid() {
		return -1, -1
	}
	file := fset.File(pos)
	if file == nil {
		return -1, -1
	}
	start = file.Offset(pos)
	end = file.Offset(node.End())
	return start, end
}

// applyTextFallback 文本级回退替换
func (pa *PatchApplier) applyTextFallback(path, content string, snippets []*EditSnippet) error {
	modified := false
	for _, snippet := range snippets {
		newContent, ok := pa.applyTextEdit(content, snippet)
		if !ok {
			return fmt.Errorf("fallback: start marker not found: %s", snippet.StartMarker)
		}
		content = newContent
		modified = true
	}
	if modified {
		return os.WriteFile(path, []byte(content), 0644)
	}
	return nil
}

// applyTextEdit 对单个 snippet 执行文本替换
func (pa *PatchApplier) applyTextEdit(content string, snippet *EditSnippet) (string, bool) {
	startIdx := strings.Index(content, snippet.StartMarker)
	if startIdx == -1 {
		return content, false
	}

	endIdx := len(content)
	if snippet.EndMarker != "" {
		eidx := strings.Index(content[startIdx:], snippet.EndMarker)
		if eidx == -1 {
			return content, false
		}
		endIdx = startIdx + eidx + len(snippet.EndMarker)
	} else {
		// 找到 StartMarker 所在声明/语句的结束
		endIdx = pa.findStatementEnd(content, startIdx)
	}

	replacement := snippet.Replacement
	existing := content[startIdx:endIdx]
	replacement = strings.ReplaceAll(replacement, "//...existing code...", existing)
	replacement = strings.ReplaceAll(replacement, "// ... existing code ...", existing)
	replacement = strings.ReplaceAll(replacement, "/*...existing code...*/", existing)

	return content[:startIdx] + replacement + content[endIdx:], true
}

// findStatementEnd 找到声明/语句的结束位置
func (pa *PatchApplier) findStatementEnd(content string, startIdx int) int {
	// 简单策略：从 startIdx 开始找第一个顶层匹配的 } 或 ;
	depth := 0
	inString := false
	stringChar := byte(0)
	for i := startIdx; i < len(content); i++ {
		c := content[i]
		if inString {
			if c == stringChar && (i == 0 || content[i-1] != '\\') {
				inString = false
			}
			continue
		}
		if c == '"' || c == '`' {
			inString = true
			stringChar = c
			continue
		}
		if c == '/' && i+1 < len(content) && content[i+1] == '/' {
			// 跳过单行注释
			for i < len(content) && content[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(content) && content[i+1] == '*' {
			// 跳过多行注释
			i += 2
			for i+1 < len(content) && !(content[i] == '*' && content[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		switch c {
		case '{', '(':
			depth++
		case '}', ')':
			depth--
			if depth <= 0 {
				return i + 1
			}
		case ';':
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(content)
}

// validateSyntax 语法验证
func (pa *PatchApplier) validateSyntax(path string) error {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	return err
}

// gofmtFiles 运行 gofmt
func (pa *PatchApplier) gofmtFiles(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"-w"}, paths...)
	cmd := exec.Command("gofmt", args...)
	return cmd.Run()
}

// DiffPreview 生成 diff 预览
func (pa *PatchApplier) DiffPreview(root string, snippets []EditSnippet) (map[string]string, error) {
	result := make(map[string]string)
	byFile := make(map[string][]*EditSnippet)
	for i := range snippets {
		s := &snippets[i]
		byFile[s.File] = append(byFile[s.File], s)
	}
	for file, ss := range byFile {
		path := filepath.Join(root, file)
		_, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var buf strings.Builder
		buf.WriteString(fmt.Sprintf("--- a/%s\n+++ b/%s\n", file, file))
		for _, s := range ss {
			buf.WriteString(fmt.Sprintf("@@ %s @@\n", s.StartMarker))
			buf.WriteString(fmt.Sprintf("+ %s\n", s.Replacement))
		}
		result[file] = buf.String()
	}
	return result, nil
}
