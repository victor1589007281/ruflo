// native.go — Native 原生兜底引擎。
//
// 职责: 当 GitNexus（索引缺失/未命中）或 Graphify（图数据缺失）无法回答查询时，
//       降级到实时文本搜索（Grep + Read）。
// 特性:
//   - 不依赖预建索引，零存储开销
//   - 适合新文件、未分片目录、索引过期场景
//   - 与内置 Grep/Read 工具语义等价，但封装为统一查询接口
//
// 零 LLM：纯操作系统命令 + 文件 IO。
package codeintel

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// NativeEngine 原生兜底引擎。
type NativeEngine struct {
	RepoPath string
}

// NewNativeEngine 创建原生引擎。
func NewNativeEngine(repoPath string) *NativeEngine {
	return &NativeEngine{RepoPath: repoPath}
}

// Grep 在仓库中执行文本搜索（类 rg/grep）。
func (e *NativeEngine) Grep(pattern string, opts GrepOptions) (*QueryResult, error) {
	start := time.Now()

	var matches []map[string]interface{}
	var cmd *exec.Cmd

	// 优先尝试 rg (ripgrep)，次选 grep
	if _, err := exec.LookPath("rg"); err == nil {
		args := []string{"--json", "--max-count", fmt.Sprintf("%d", opts.MaxResults)}
		if opts.Dir != "" {
			args = append(args, opts.Dir)
		} else {
			args = append(args, e.RepoPath)
		}
		args = append(args, pattern)
		cmd = exec.Command("rg", args...)
	} else {
		args := []string{"-r", "-n", "--include", "*.go", "--include", "*.c", "--include", "*.h", "--include", "*.cpp", "--include", "*.py", "--include", "*.js", "--include", "*.ts", "--include", "*.java", "--include", "*.rs"}
		if opts.Dir != "" {
			args = append(args, opts.Dir)
		} else {
			args = append(args, e.RepoPath)
		}
		args = append(args, "-e", pattern)
		cmd = exec.Command("grep", args...)
	}

	out, err := cmd.Output()
	if err != nil {
		// grep 无匹配时 exit 1，不算错误
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// no matches
		} else {
			return nil, fmt.Errorf("grep error: %w", err)
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	lineNum := 0
	for scanner.Scan() && lineNum < opts.MaxResults {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// 简单解析 "file:line:content" 或 "file-line content"
		file, lineno, content := parseGrepLine(line)
		matches = append(matches, map[string]interface{}{
			"file":    file,
			"line":    lineno,
			"content": content,
		})
		lineNum++
	}

	result := map[string]interface{}{
		"pattern": pattern,
		"matches": matches,
		"count":   len(matches),
		"engine":  "native_grep",
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "native_grep",
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// ReadFile 读取文件内容（带行范围）。
func (e *NativeEngine) ReadFile(filePath string, opts ReadOptions) (*QueryResult, error) {
	start := time.Now()

	// 如果路径是相对的，基于 RepoPath 解析
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(e.RepoPath, filePath)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var lines []string
	lineNo := 1
	for scanner.Scan() {
		if opts.Offset > 0 && lineNo < opts.Offset {
			lineNo++
			continue
		}
		if opts.Limit > 0 && len(lines) >= opts.Limit {
			break
		}
		lines = append(lines, scanner.Text())
		lineNo++
	}

	result := map[string]interface{}{
		"file":    filePath,
		"lines":   lines,
		"total":   lineNo - 1,
		"offset":  opts.Offset,
		"engine":  "native_read",
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "native_read",
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// FallbackSearch 兜底搜索：先 Grep 找定义，再 Read 附近行。
func (e *NativeEngine) FallbackSearch(symbol string) (*QueryResult, error) {
	start := time.Now()

	// 1. Grep 找符号出现的位置
	grepResult, err := e.Grep(symbol, GrepOptions{MaxResults: 20})
	if err != nil {
		return nil, err
	}
	matches, _ := grepResult.Results.(map[string]interface{})["matches"].([]map[string]interface{})

	// 2. 提取最可能的定义位置（第一个匹配）
	var bestFile string
	var bestLine int
	for _, m := range matches {
		if file, ok := m["file"].(string); ok && file != "" {
			bestFile = file
			if ln, ok := m["line"].(int); ok {
				bestLine = ln
			}
			break
		}
	}

	// 3. 读取定义附近
	var snippet []string
	if bestFile != "" {
		readResult, err := e.ReadFile(bestFile, ReadOptions{Offset: bestLine - 5, Limit: 20})
		if err == nil {
			if r, ok := readResult.Results.(map[string]interface{}); ok {
				if lines, ok := r["lines"].([]string); ok {
					snippet = lines
				}
			}
		}
	}

	result := map[string]interface{}{
		"symbol":  symbol,
		"matches": matches,
		"snippet": map[string]interface{}{
			"file":   bestFile,
			"line":   bestLine,
			"lines":  snippet,
		},
		"engine": "native_fallback",
	}
	content, _ := json.MarshalIndent(result, "", "  ")
	return &QueryResult{
		QueryType: "native_fallback",
		Results:   result,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// ============================================================================
// 参数类型
// ============================================================================

// GrepOptions Grep 查询选项。
type GrepOptions struct {
	Dir        string // 搜索目录（空表示整个仓库）
	MaxResults int    // 最大结果数
}

// ReadOptions 文件读取选项。
type ReadOptions struct {
	Offset int // 起始行（1-based）
	Limit  int // 最大行数
}

// ============================================================================
// 私有辅助
// ============================================================================

func parseGrepLine(line string) (file string, lineno int, content string) {
	// 尝试解析 "file:line:content" 格式
	parts := strings.SplitN(line, ":", 3)
	if len(parts) >= 3 {
		file = parts[0]
		fmt.Sscanf(parts[1], "%d", &lineno)
		content = parts[2]
		return
	}
	// 回退：整行作为内容
	content = line
	return
}
