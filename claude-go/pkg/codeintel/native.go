// native.go — Native 原生兜底引擎（v3.1 恢复）。
//
// 职责: 当 GitNexus 返回 ambiguous/not_found 或 Graphify 无结果时，
//       降级到实时文本搜索（ripgrep + 文件读取）。
// 特性:
//   - 不依赖预建索引，零存储开销
//   - 支持精确单词匹配、定义位置发现、代码片段提取
//   - 与 GitNexus/Graphify 互补，填补结构查询的盲区
package codeintel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var ripgrepPath string // 缓存发现的 rg 路径

// NativeEngine 原生兜底引擎。
type NativeEngine struct {
	RepoPath string
}

// NewNativeEngine 创建原生引擎。
func NewNativeEngine(repoPath string) *NativeEngine {
	return &NativeEngine{RepoPath: repoPath}
}

// FindDefinitions 查找符号的所有定义位置（支持多种语言模式）。
func (e *NativeEngine) FindDefinitions(symbol string, maxResults int) (*QueryResult, error) {
	start := time.Now()
	if maxResults <= 0 {
		maxResults = 20
	}

	patterns := []string{
		// Go: func Symbol(, func (Receiver) Symbol(
		fmt.Sprintf(`func\s+(\([^)]*\)\s+)?%s\s*\(`, symbol),
		// Go: type Symbol struct/interface
		fmt.Sprintf(`type\s+%s\s+(struct|interface)`, symbol),
		// Go: const/var Symbol
		fmt.Sprintf(`(const|var)\s+%s\b`, symbol),
		// JS/TS: function Symbol(, class Symbol, const Symbol =
		fmt.Sprintf(`(function\s+%s|class\s+%s|const\s+%s\s*=)`, symbol, symbol, symbol),
		// Python: def Symbol(, class Symbol(
		fmt.Sprintf(`(def\s+%s|class\s+%s)\b`, symbol, symbol),
		// C/C++/Java/Rust: 类型+符号( 或 struct Symbol {
		fmt.Sprintf(`(struct|class|enum|union|typedef|impl)\s+%s\b`, symbol),
	}

	var allMatches []map[string]interface{}
	seen := make(map[string]bool)

	for _, pattern := range patterns {
		matches := e.rgSearch(pattern, maxResults*2)
		for _, m := range matches {
			key := fmt.Sprintf("%s:%v", m["file"], m["line"])
			if seen[key] {
				continue
			}
			seen[key] = true
			allMatches = append(allMatches, m)
			if len(allMatches) >= maxResults {
				break
			}
		}
		if len(allMatches) >= maxResults {
			break
		}
	}

	result := map[string]interface{}{
		"symbol":      symbol,
		"definitions": allMatches,
		"count":       len(allMatches),
		"engine":      "native_definitions",
	}
	return &QueryResult{
		QueryType: "native_definitions",
		Results:   result,
		Tokens:    len(mustJSON(result)) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// FindReferences 查找符号的文本引用（rg -w）。
func (e *NativeEngine) FindReferences(symbol string, maxResults int) (*QueryResult, error) {
	start := time.Now()
	if maxResults <= 0 {
		maxResults = 50
	}
	matches := e.rgSearchWord(symbol, maxResults)
	result := map[string]interface{}{
		"symbol":      symbol,
		"references":  matches,
		"count":       len(matches),
		"engine":      "native_references",
	}
	return &QueryResult{
		QueryType: "native_references",
		Results:   result,
		Tokens:    len(mustJSON(result)) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// ReadSnippet 读取符号定义附近的代码片段。
func (e *NativeEngine) ReadSnippet(filePath string, line int, contextLines int) (*QueryResult, error) {
	start := time.Now()
	if contextLines <= 0 {
		contextLines = 5
	}
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(e.RepoPath, filePath)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	var lines []string
	startLine := line - contextLines
	if startLine < 1 {
		startLine = 1
	}
	endLine := line + contextLines

	scanner := bufio.NewScanner(f)
	curr := 1
	for scanner.Scan() {
		if curr >= startLine && curr <= endLine {
			lines = append(lines, fmt.Sprintf("%4d | %s", curr, scanner.Text()))
		}
		if curr > endLine {
			break
		}
		curr++
	}

	result := map[string]interface{}{
		"file":        filePath,
		"target_line": line,
		"snippet":     lines,
		"engine":      "native_snippet",
	}
	return &QueryResult{
		QueryType: "native_snippet",
		Results:   result,
		Tokens:    len(mustJSON(result)) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// FallbackSearch 兜底搜索：先找定义，再读附近行，再找引用。
func (e *NativeEngine) FallbackSearch(symbol string) (*QueryResult, error) {
	start := time.Now()

	// 1. 找定义
	defs, _ := e.FindDefinitions(symbol, 10)
	var bestDef map[string]interface{}
	if defs != nil {
		if m, ok := defs.Results.(map[string]interface{}); ok {
			if dlist, ok := m["definitions"].([]map[string]interface{}); ok && len(dlist) > 0 {
				bestDef = dlist[0]
			}
		}
	}

	// 2. 读取定义附近
	var snippet []string
	if bestDef != nil {
		if f, ok := bestDef["file"].(string); ok {
			if ln, ok := bestDef["line"].(int); ok {
				snippetQR, _ := e.ReadSnippet(f, ln, 8)
				if snippetQR != nil {
					if m, ok := snippetQR.Results.(map[string]interface{}); ok {
						if s, ok := m["snippet"].([]string); ok {
							snippet = s
						}
					}
				}
			}
		}
	}

	// 3. 找引用
	refs, _ := e.FindReferences(symbol, 20)

	result := map[string]interface{}{
		"symbol":      symbol,
		"definitions": safeMapResult(defs),
		"references":  safeMapResult(refs),
		"snippet":     snippet,
		"engine":      "native_fallback",
		"note":        "GitNexus/Graphify returned ambiguous/not_found; native fallback activated",
	}
	return &QueryResult{
		QueryType: "native_fallback",
		Results:   result,
		Tokens:    len(mustJSON(result)) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// ============================================================================
// 私有辅助
// ============================================================================

func (e *NativeEngine) rgSearch(pattern string, maxResults int) []map[string]interface{} {
	rg := findRipgrep()
	if rg == "" {
		return e.grepSearch(pattern, maxResults)
	}
	cmd := exec.Command(rg, "-n", "--max-count", fmt.Sprintf("%d", maxResults), "-P", pattern, e.RepoPath)
	out, _ := cmd.Output()
	return parseRgLines(string(out))
}

func (e *NativeEngine) rgSearchWord(symbol string, maxResults int) []map[string]interface{} {
	rg := findRipgrep()
	if rg == "" {
		return e.grepSearchWord(symbol, maxResults)
	}
	cmd := exec.Command(rg, "-n", "-w", "--max-count", fmt.Sprintf("%d", maxResults), symbol, e.RepoPath)
	out, _ := cmd.Output()
	return parseRgLines(string(out))
}

func parseRgLines(output string) []map[string]interface{} {
	var results []map[string]interface{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) >= 3 {
			ln, _ := strconv.Atoi(parts[1])
			results = append(results, map[string]interface{}{
				"file":    parts[0],
				"line":    ln,
				"content": parts[2],
			})
		}
	}
	return results
}

// ============================================================================
// ripgrep 路径发现 + grep fallback
// ============================================================================

func findRipgrep() string {
	if ripgrepPath != "" {
		return ripgrepPath
	}
	// 1. PATH
	if p, err := exec.LookPath("rg"); err == nil {
		ripgrepPath = p
		return p
	}
	// 2. 常见系统路径
	candidates := []string{
		"/usr/local/bin/rg",
		"/usr/bin/rg",
	}
	// 3. 编辑器捆绑路径
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			"/usr/share/trae-cn/resources/app/node_modules/@vscode/ripgrep/bin/rg",
			"/usr/share/trae-cn/resources/app/node_modules/@byted-fe/ripgrep-linux-x64/bin/rg",
			"/usr/share/trae-cn/resources/app/node_modules/@byted-fe/ripgrep-linux-musl-x64/bin/rg",
			filepath.Join(home, ".cursor-server/bin/linux-x64/60faf7b51077ed1df1db718157bbfed740d2e160/resources/helpers/rg"),
			filepath.Join(home, ".cursor-server/bin/linux-x64/f3f5cec40024283013878b50c4f9be4002e0b580/resources/helpers/rg"),
			filepath.Join(home, ".cursor-server/bin/linux-x64/60faf7b51077ed1df1db718157bbfed740d2e160/node_modules/@vscode/ripgrep/bin/rg"),
			filepath.Join(home, ".cursor-server/bin/linux-x64/f3f5cec40024283013878b50c4f9be4002e0b580/node_modules/@vscode/ripgrep/bin/rg"),
			filepath.Join(home, ".local/share/opencode/bin/rg"),
			filepath.Join(home, ".ai_completion/.ripgrep/ripgrep-v13.0.0-10-x86_64-unknown-linux-musl"),
		)
		// 4. 动态搜索 cursor-server 下的 rg
		cursorBase := filepath.Join(home, ".cursor-server/bin/linux-x64")
		if entries, err := os.ReadDir(cursorBase); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					p := filepath.Join(cursorBase, e.Name(), "resources/helpers/rg")
					if _, err := os.Stat(p); err == nil {
						candidates = append(candidates, p)
					}
					p2 := filepath.Join(cursorBase, e.Name(), "node_modules/@vscode/ripgrep/bin/rg")
					if _, err := os.Stat(p2); err == nil {
						candidates = append(candidates, p2)
					}
				}
			}
		}
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			ripgrepPath = p
			return p
		}
	}
	ripgrepPath = "_NOT_FOUND_"
	return ""
}

// grep fallback（无 rg 时使用）
func (e *NativeEngine) grepSearch(pattern string, maxResults int) []map[string]interface{} {
	cmd := exec.Command("grep", "-r", "-n", "-E", "--max-count="+fmt.Sprintf("%d", maxResults), pattern, e.RepoPath)
	out, _ := cmd.Output()
	return parseGrepLines(string(out), e.RepoPath)
}

func (e *NativeEngine) grepSearchWord(symbol string, maxResults int) []map[string]interface{} {
	cmd := exec.Command("grep", "-r", "-n", "-w", "--max-count="+fmt.Sprintf("%d", maxResults), symbol, e.RepoPath)
	out, _ := cmd.Output()
	return parseGrepLines(string(out), e.RepoPath)
}

func parseGrepLines(output string, repoPath string) []map[string]interface{} {
	var results []map[string]interface{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// grep -r 输出: <file>:<line>:<content>
		parts := strings.SplitN(line, ":", 3)
		if len(parts) >= 3 {
			file := parts[0]
			// grep 输出的是相对路径或绝对路径，统一处理
			if !filepath.IsAbs(file) {
				file = filepath.Join(repoPath, file)
			}
			ln, _ := strconv.Atoi(parts[1])
			results = append(results, map[string]interface{}{
				"file":    file,
				"line":    ln,
				"content": parts[2],
			})
		}
	}
	return results
}

func safeMapResult(qr *QueryResult) interface{} {
	if qr == nil || qr.Results == nil {
		return nil
	}
	return qr.Results
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
