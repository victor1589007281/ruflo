// fileread.go 实现 FileRead 工具 (只读/并发安全)。
// 对应 TS 源码: review/claude/src/tools/FileReadTool/FileReadTool.ts
//
// 功能:
//   - 读取文件内容，支持行号范围 (offset + limit)
//   - 添加行号前缀 (LINE_NUMBER|LINE_CONTENT)
//   - 图片 / PDF / Notebook 占位提示
//   - 大文件截断 (字节与近似 token 上限)
package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const FileReadToolName = "Read"

const (
	maxReadTokens = 25000
	maxReadBytes  = 1048576 // 1 MiB
)

// maxApproxCharsFromTokens 粗略按 1 token ≈ 4 字符限制输出长度
const maxApproxCharsFromTokens = maxReadTokens * 4

// fileReadInput 解析 LLM 传入的工具输入
type fileReadInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"` // 1-indexed 起始行; 负数从末尾计
	Limit  int    `json:"limit,omitempty"`  // 读取行数
}

// FileReadTool 文件读取工具
type FileReadTool struct {
	mu             sync.Mutex
	lastReadHashes map[string]string // path -> content hash (不含内容，仅去重)
}

func NewFileReadTool() *FileReadTool {
	return &FileReadTool{
		lastReadHashes: make(map[string]string),
	}
}

func (t *FileReadTool) Name() string { return FileReadToolName }

func (t *FileReadTool) Description() string {
	return `Reads a file from the local filesystem. You can access any file directly by using this tool.
If the User provides a path to a file assume that path is valid.

Usage:
- You can optionally specify a line offset and limit (especially handy for long files)
- Lines in the output are numbered starting at 1, using following format: LINE_NUMBER|LINE_CONTENT
- If you read a file that exists but has empty contents you will receive 'File is empty.'`
}

func (t *FileReadTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "The absolute path of the file to read."},
			"offset": {"type": "integer", "description": "The line number to start reading from. 1-indexed. Negative values count from end."},
			"limit": {"type": "integer", "description": "The number of lines to read."}
		},
		"required": ["path"]
	}`)
}

func (t *FileReadTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (t *FileReadTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *FileReadTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func fileReadKindMessage(filePath string) (msg string, skip bool) {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg":
		return fmt.Sprintf("[Image file: %s] - Image reading requires multimodal support", filePath), true
	case ".pdf":
		return fmt.Sprintf("[PDF file: %s] - PDF reading requires external parser", filePath), true
	case ".ipynb":
		return fmt.Sprintf("[Notebook file: %s] - Use NotebookEdit tool to read notebooks", filePath), true
	default:
		return "", false
	}
}

func readFileLimited(path string, maxBytes int64) (data []byte, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	buf, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > maxBytes {
		return buf[:maxBytes], true, nil
	}
	return buf, false, nil
}

func truncateApproxTokens(s string) string {
	if len(s) <= maxApproxCharsFromTokens {
		return s
	}
	return s[:maxApproxCharsFromTokens] + "\n... (output truncated: approx token limit)"
}

// Call 执行文件读取。
func (t *FileReadTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	_ = ctx

	var in fileReadInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	filePath := expandPath(in.Path, tctx.Cwd)

	if msg, skip := fileReadKindMessage(filePath); skip {
		return &tool.ToolResult{Content: msg}, nil
	}

	data, truncatedBytes, err := readFileLimited(filePath, maxReadBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return &tool.ToolResult{
				Content: fmt.Sprintf("Error: file not found: %s\n\n%s", filePath, fileFindHint(filePath)),
				IsError: true,
			}, nil
		}
		return &tool.ToolResult{Content: fmt.Sprintf("读取文件失败: %v", err), IsError: true}, nil
	}

	content := string(data)
	if content == "" {
		return &tool.ToolResult{Content: "File is empty."}, nil
	}

	// 优化2: Read 工具 hash 缓存 — 只存 hash，不含内容
	currentHash := hashBytes(data)
	t.mu.Lock()
	lastHash, exists := t.lastReadHashes[filePath]
	t.lastReadHashes[filePath] = currentHash
	t.mu.Unlock()
	if exists && lastHash == currentHash {
		return &tool.ToolResult{Content: fmt.Sprintf("<file %s unchanged since last read (hash: %s)>", filePath, currentHash[:8])}, nil
	}

	lines := strings.Split(content, "\n")
	totalLines := len(lines)

	startLine := 0
	endLine := totalLines

	if in.Offset != 0 {
		if in.Offset > 0 {
			startLine = in.Offset - 1
		} else {
			startLine = totalLines + in.Offset
		}
		if startLine < 0 {
			startLine = 0
		}
		if startLine > totalLines {
			startLine = totalLines
		}
	}

	if in.Limit > 0 {
		endLine = startLine + in.Limit
		if endLine > totalLines {
			endLine = totalLines
		}
	}

	var sb strings.Builder
	if truncatedBytes {
		sb.WriteString(fmt.Sprintf("(File read truncated to first %d bytes)\n", maxReadBytes))
	}
	maxLineNumWidth := len(fmt.Sprintf("%d", endLine))
	for i := startLine; i < endLine; i++ {
		lineNum := i + 1
		sb.WriteString(fmt.Sprintf("%*d|%s\n", maxLineNumWidth, lineNum, lines[i]))
	}

	out := truncateApproxTokens(sb.String())
	return &tool.ToolResult{Content: out}, nil
}

// expandPath 展开路径 (支持 ~ 和相对路径)
func expandPath(p string, cwd string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

func fileFindHint(path string) string {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Sprintf("The directory %s does not exist.", dir)
	}
	return "If this is a relative path, the file may be in a different directory. Try using an absolute path."
}

// hashBytes 计算字节切片的短 SHA256 哈希。
func hashBytes(b []byte) string {
	if len(b) == 0 {
		return "empty"
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}
