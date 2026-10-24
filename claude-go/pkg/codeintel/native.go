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

// rgSearch 用 ripgrep 做正则搜索 (PCRE2), 无 rg 时降级到 grep -E。
//
// 算法解读：
//
//	原理：native 引擎不依赖预建索引, 每次查询都是一次实时文本扫描 —— 用 rg 拿"文件:行号:内容"
//	      三元组, 再交给 parseRgLines 解析。这是 GitNexus/Graphify 给不出答案时的兜底通道。
//	关键不变量：rg 与 grep 两条路径必须产出**同构**的结果 (file/line/content), 否则上层按
//	      统一形状取字段就会在降级时静默拿到空值。这也是 grepSearch 要单独做路径归一化的原因。
//	代价与边界：扫描范围是整个 RepoPath, 无索引 ⇒ 复杂度 ≈ 仓库字节数, 大仓上比图查询慢一个量级;
//	      另外 `--max-count` 是**每文件**上限而非总量上限, 所以 maxResults 不是严格的结果总数上限。
//	设计权衡：宁可接受"结果可能多于 maxResults"也不做二次截断 —— 截断会让"定义在哪个文件"
//	      这类问题按目录序被偏置, 而调用方 (FindDefinitions) 自己有 seen 去重与总量裁剪。
//
// 注意: `-P` 要求 rg 编译了 PCRE2, 部分发行版/编辑器捆绑的 rg 没有; 此时 cmd.Output() 报错被
// 忽略, 结果是空列表而不是报错 —— 看起来像"没有匹配"。调用方只能靠"两条模式都空"间接察觉。
func (e *NativeEngine) rgSearch(pattern string, maxResults int) []map[string]interface{} {
	rg := findRipgrep()
	if rg == "" {
		return e.grepSearch(pattern, maxResults)
	}
	cmd := exec.Command(rg, "-n", "--max-count", fmt.Sprintf("%d", maxResults), "-P", pattern, e.RepoPath)
	// 执行错误被忽略: rg 不支持 PCRE2 或路径不存在时这里会失败, 结果退化成空列表而不是报错。
	out, _ := cmd.Output()
	return parseRgLines(string(out))
}

// rgSearchWord 用 rg -w 做整词匹配 (找引用), 无 rg 时降级到 grep -w。
func (e *NativeEngine) rgSearchWord(symbol string, maxResults int) []map[string]interface{} {
	rg := findRipgrep()
	if rg == "" {
		return e.grepSearchWord(symbol, maxResults)
	}
	cmd := exec.Command(rg, "-n", "-w", "--max-count", fmt.Sprintf("%d", maxResults), symbol, e.RepoPath)
	out, _ := cmd.Output()
	return parseRgLines(string(out))
}

// parseRgLines 把 rg 的 `文件:行号:内容` 输出解析成结果列表。
//
// 算法解读：
//
//	原理：rg 默认输出以 `:` 分隔的三段。用 SplitN(..., 3) 只切前两个冒号, 把剩下的一整段
//	      (可能自带冒号, 例如 Go 的 label、map 字面量) 原样留给 content。
//	关键不变量：行号解析失败时**不丢弃**该行, 而是留下 ln=0 —— 位置未知比整条消失更容易被发现,
//	      也让调用方的 seen 去重键 ("文件:0") 保持稳定。
//	代价与边界：按 ':' 切分对**含冒号的文件名** (Windows 盘符 C:\... 或 "a:b.go") 会切错段,
//	      把路径截断、把行号解析成 0。仓库路径不含冒号时无影响。
//	设计权衡：不做 shell 级的 `--null` 分隔 —— 那需要同时改 rg 与 grep 两侧的参数与解析,
//	      而这两条路径要保持同构, 改动面大于收益。
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

// findRipgrep 按 PATH → 常见系统路径 → 编辑器捆绑路径 → 动态扫描的顺序发现 rg, 结果进程内缓存。
//
// 算法解读：
//
//	原理：部署环境差异极大 —— rg 可能不在 PATH 上, 而是随 VSCode/Cursor/Trae 的扩展捆在
//	      node_modules 里。故按"可信度递减"的顺序探测, 命中即缓存返回。
//	关键不变量：未命中时缓存哨兵 "_NOT_FOUND_" 而不是留空 —— 空串是"还没查过"的初值,
//	      用空串表示未命中会让每次调用都重跑一遍整轮文件系统探测 (含一次 ReadDir)。
//	代价与边界：只在首次调用做 IO; 之后是纯内存。代价是**运行期新装的 rg 不会被发现**,
//	      要重启进程才生效。
//	设计权衡：不做后台异步刷新 —— 探测结果在整个进程生命周期内实际不变, 刷新只会引入竞态。
//
// 注意: cursor-server 的路径里写死了版本哈希 (60faf7b5…/f3f5cec4…), 这些是特定版本的目录名;
// 换版本后靠第 4 步的动态扫描兜底, 写死的那几条会退化成永远 stat 失败的死路径。
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
	// 缓存哨兵而不是留空: 空串是"还没查过"的初值, 用它表示未命中会让每次调用都重跑整轮探测。
	ripgrepPath = "_NOT_FOUND_"
	return ""
}

// grep fallback（无 rg 时使用）
func (e *NativeEngine) grepSearch(pattern string, maxResults int) []map[string]interface{} {
	cmd := exec.Command("grep", "-r", "-n", "-E", "--max-count="+fmt.Sprintf("%d", maxResults), pattern, e.RepoPath)
	out, _ := cmd.Output()
	return parseGrepLines(string(out), e.RepoPath)
}

// grepSearchWord 用 grep -w 做整词匹配 (rgSearchWord 的降级路径)。
func (e *NativeEngine) grepSearchWord(symbol string, maxResults int) []map[string]interface{} {
	cmd := exec.Command("grep", "-r", "-n", "-w", "--max-count="+fmt.Sprintf("%d", maxResults), symbol, e.RepoPath)
	out, _ := cmd.Output()
	return parseGrepLines(string(out), e.RepoPath)
}

// parseGrepLines 解析 grep 输出, 并把相对路径拼成基于 repoPath 的路径。
//
// 注意: 这里必须做相对→绝对的归一化, 因为 rg 给的是**相对 cwd** 的路径而 grep -r 给的是
// 传入参数的路径形态; 不归一化的话同一符号在两条路径下的 file 字段形状不同, 上层去重与展示都会错位。
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

// safeMapResult 取 QueryResult.Results; nil 查询结果返回 nil 而不是 panic。
func safeMapResult(qr *QueryResult) interface{} {
	if qr == nil || qr.Results == nil {
		return nil
	}
	return qr.Results
}

// mustJSON 序列化 v; 出错返回 nil (仅供估算 token 数, 不参与正确性)。
func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
