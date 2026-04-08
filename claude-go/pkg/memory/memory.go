// Package memory 实现记忆系统 (CLAUDE.md + memdir)。
// 对应 TS 源码: review/claude/src/utils/claudemd.ts + review/claude/src/memdir/memdir.ts
//
// Claude Code 的记忆系统由多层组成:
//
//   1. Managed memory: /etc/claude-code/CLAUDE.md (管理员级)
//   2. User memory: ~/.claude/CLAUDE.md (用户级)
//   3. Project memory: 从当前目录向上遍历:
//      - CLAUDE.md
//      - .claude/CLAUDE.md
//      - .claude/rules/*.md
//   4. Local memory: CLAUDE.local.md (本地, 不提交)
//
// 加载顺序: managed → user → project (远→近) → local
// 优先级: local > project(近) > project(远) > user > managed
//
// @include 指令:
//   记忆文件中可以包含 @path 引用其他文件,
//   被引用的文件内容会被内联到记忆提示中。
//   支持: @./relative, @~/home, @/absolute
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/types"
)

// MaxMemoryCharCount 单个记忆文件最大字符数
const MaxMemoryCharCount = 40000

// MaxEntrypointLines MEMORY.md 最大行数
const MaxEntrypointLines = 200

// MaxEntrypointBytes MEMORY.md 最大字节数 (UTF-8 截断)
const MaxEntrypointBytes = 25000

// memoryTypeEntrypoint cwd 下 MEMORY.md 的类型标签 (非 types 包常量，避免改动类型定义)
const memoryTypeEntrypoint types.MemoryType = "entrypoint"

// htmlCommentRe 匹配 HTML 注释 <!-- ... --> (非贪婪)
var htmlCommentRe = regexp.MustCompile(`<!--[\s\S]*?-->`)

// MaxIncludeDepth @include 最大递归深度 (防止循环引用)
// 对应 TS: utils/claudemd.ts 中的 MAX_INCLUDE_DEPTH = 5
const MaxIncludeDepth = 5

// Loader 记忆加载器
type Loader struct {
	Cwd         string
	HomeDir     string
	processedPaths map[string]bool // 防止循环引用
}

// NewLoader 创建记忆加载器
func NewLoader(cwd string) *Loader {
	home, _ := os.UserHomeDir()
	return &Loader{
		Cwd:            cwd,
		HomeDir:        home,
		processedPaths: make(map[string]bool),
	}
}

// LoadAll 加载所有记忆文件。
// 对应 TS: utils/claudemd.ts 中的记忆加载逻辑
//
// 加载算法:
//   1. 加载 managed memory (/etc/claude-code/CLAUDE.md)
//   2. 加载 user memory (~/.claude/CLAUDE.md, ~/.claude/rules/*.md)
//   3. 从 cwd 向上遍历到 / , 在每个目录中检查:
//      - CLAUDE.md
//      - .claude/CLAUDE.md
//      - .claude/rules/*.md
//   4. 加载 local memory (CLAUDE.local.md)
//   5. 处理 @include 指令
//   6. 按优先级排序 (后加载的优先级更高)
func (l *Loader) LoadAll() []types.MemoryFile {
	var files []types.MemoryFile
	priority := 0

	// 1. Managed memory
	managedPaths := []string{"/etc/claude-code/CLAUDE.md"}
	for _, p := range managedPaths {
		if f := l.loadFile(p, types.MemoryTypeManaged, priority); f != nil {
			files = append(files, *f)
			priority++
		}
	}

	// 2. User memory
	if l.HomeDir != "" {
		userPaths := []string{
			filepath.Join(l.HomeDir, ".claude", "CLAUDE.md"),
		}
		for _, p := range userPaths {
			if f := l.loadFile(p, types.MemoryTypeUser, priority); f != nil {
				files = append(files, *f)
				priority++
			}
		}
		// 加载 ~/.claude/rules/*.md
		rulesDir := filepath.Join(l.HomeDir, ".claude", "rules")
		files = append(files, l.loadRulesDir(rulesDir, types.MemoryTypeUser, &priority)...)
	}

	// 3. Project memory (从 cwd 向上遍历)
	dirs := l.getAncestorDirs(l.Cwd)
	// 从远到近遍历 (根目录最先加载，优先级最低)
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]

		// CLAUDE.md
		if f := l.loadFile(filepath.Join(dir, "CLAUDE.md"), types.MemoryTypeProject, priority); f != nil {
			files = append(files, *f)
			priority++
		}

		// .claude/CLAUDE.md
		if f := l.loadFile(filepath.Join(dir, ".claude", "CLAUDE.md"), types.MemoryTypeProject, priority); f != nil {
			files = append(files, *f)
			priority++
		}

		// .claude/rules/*.md
		rulesDir := filepath.Join(dir, ".claude", "rules")
		files = append(files, l.loadRulesDir(rulesDir, types.MemoryTypeProject, &priority)...)
	}

	// 4. Local memory
	localPaths := l.getAncestorDirs(l.Cwd)
	for i := len(localPaths) - 1; i >= 0; i-- {
		p := filepath.Join(localPaths[i], "CLAUDE.local.md")
		if f := l.loadFile(p, types.MemoryTypeLocal, priority); f != nil {
			files = append(files, *f)
			priority++
		}
	}

	// 4b. Auto memory: cwd/MEMORY.md (截断规则见 truncateEntrypoint)
	if mem := l.loadMemoryEntrypoint(priority); mem != nil {
		files = append(files, *mem)
		priority++
	}

	// 5. 处理 @include 指令 (带深度限制)
	for i := range files {
		files[i].Content = l.processIncludes(files[i].Content, filepath.Dir(files[i].Path), 0)
	}

	return files
}

// stripFrontmatter 去除文件开头的 YAML frontmatter (首行与闭合行均为 ---)。
// 若无合法 frontmatter 则原样返回。
func stripFrontmatter(content string) string {
	s := strings.TrimPrefix(content, "\uFEFF")
	lines := strings.Split(s, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return content
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		}
	}
	return content
}

// stripHTMLComments 使用正则删除 HTML 注释块 <!-- ... -->。
func stripHTMLComments(content string) string {
	return htmlCommentRe.ReplaceAllString(content, "")
}

// truncateEntrypoint 对 MEMORY.md 应用行数与字节上限 (先限行再限字节，UTF-8 安全截断)。
func truncateEntrypoint(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) > MaxEntrypointLines {
		lines = lines[:MaxEntrypointLines]
		content = strings.Join(lines, "\n")
		content += fmt.Sprintf("\n\n> NOTE: Truncated to %d lines (MaxEntrypointLines)", MaxEntrypointLines)
	}
	if len(content) <= MaxEntrypointBytes {
		return content
	}
	// UTF-8 安全截断至 MaxEntrypointBytes 字节
	b := []byte(content)
	if len(b) <= MaxEntrypointBytes {
		return content
	}
	b = b[:MaxEntrypointBytes]
	for len(b) > 0 && !utf8.FullRune(b) {
		b = b[:len(b)-1]
	}
	return string(b) + fmt.Sprintf("\n\n> NOTE: Truncated to %d bytes (MaxEntrypointBytes)", MaxEntrypointBytes)
}

// isOutsideCwd 判断绝对路径 path 是否落在 cwd 目录树之外 (含解析后的相对关系)。
func isOutsideCwd(path, cwd string) bool {
	absPath, err1 := filepath.Abs(path)
	absCwd, err2 := filepath.Abs(cwd)
	if err1 != nil || err2 != nil {
		return true
	}
	rel, err := filepath.Rel(absCwd, absPath)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// loadMemoryEntrypoint 加载 cwd 下的 MEMORY.md，应用 strip/truncate，并登记 processedPaths。
func (l *Loader) loadMemoryEntrypoint(priority int) *types.MemoryFile {
	path := filepath.Join(l.Cwd, "MEMORY.md")
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	if l.processedPaths[absPath] {
		return nil
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	raw := string(data)
	raw = stripFrontmatter(raw)
	raw = stripHTMLComments(raw)
	content := strings.TrimSpace(raw)
	if content == "" {
		return nil
	}
	content = truncateEntrypoint(content)
	if len(content) > MaxMemoryCharCount {
		content = content[:MaxMemoryCharCount] + "\n\n> WARNING: File truncated (exceeded character limit)"
	}
	l.processedPaths[absPath] = true
	return &types.MemoryFile{
		Path:     absPath,
		Content:  content,
		Type:     memoryTypeEntrypoint,
		Priority: priority,
	}
}

// loadFile 加载单个文件
func (l *Loader) loadFile(path string, memType types.MemoryType, priority int) *types.MemoryFile {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil
	}

	if l.processedPaths[absPath] {
		return nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}

	raw := string(data)
	raw = stripFrontmatter(raw)
	raw = stripHTMLComments(raw)
	content := strings.TrimSpace(raw)
	if content == "" {
		return nil
	}

	if len(content) > MaxMemoryCharCount {
		content = content[:MaxMemoryCharCount] + "\n\n> WARNING: File truncated (exceeded character limit)"
	}

	l.processedPaths[absPath] = true

	return &types.MemoryFile{
		Path:     absPath,
		Content:  content,
		Type:     memType,
		Priority: priority,
	}
}

// loadRulesDir 加载 rules 目录下的所有 .md 文件
func (l *Loader) loadRulesDir(dir string, memType types.MemoryType, priority *int) []types.MemoryFile {
	var files []types.MemoryFile
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if f := l.loadFile(path, memType, *priority); f != nil {
			files = append(files, *f)
			*priority++
		}
	}
	return files
}

// processIncludes 处理 @include 指令。
// 对应 TS: utils/claudemd.ts 中的 processMemoryFile()
//
// 语法:
//   @path       → 相对于记忆文件所在目录
//   @./path     → 明确相对路径
//   @~/path     → 相对于 HOME
//   @/path      → 绝对路径
//
// 在 markdown leaf 文本节点中查找 @引用，
// 不在代码块中处理 (避免误解析代码中的 @ 符号)。
//
// depth 参数用于限制递归深度，防止无限循环。
// 对应 TS: MAX_INCLUDE_DEPTH = 5 和 processedPaths 去重。
func (l *Loader) processIncludes(content string, baseDir string, depth int) string {
	if depth >= MaxIncludeDepth {
		return content
	}

	lines := strings.Split(content, "\n")
	inCodeBlock := false
	var result []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
		}

		if inCodeBlock {
			result = append(result, line)
			continue
		}

		// 检查 @include 引用
		if strings.HasPrefix(trimmed, "@") && !strings.Contains(trimmed, " ") {
			ref := strings.TrimSuffix(trimmed[1:], "#") // 去除 #fragment
			includePath := l.resolveIncludePath(ref, baseDir)
			if includePath != "" {
				absInclude, _ := filepath.Abs(includePath)
				if isOutsideCwd(absInclude, l.Cwd) {
					result = append(result, fmt.Sprintf("<!-- @include blocked: %q is outside working directory (cwd=%s) -->", ref, l.Cwd))
					continue
				}
				// 循环检测 (对应 TS: processedPaths)
				if !l.processedPaths[absInclude] {
					l.processedPaths[absInclude] = true
					if data, err := os.ReadFile(includePath); err == nil {
						included := string(data)
						included = stripFrontmatter(included)
						included = stripHTMLComments(included)
						included = strings.TrimSpace(included)
						if len(included) > MaxMemoryCharCount/2 {
							included = included[:MaxMemoryCharCount/2] + "\n... (included file truncated)"
						}
						// 递归处理嵌套的 @include
						included = l.processIncludes(included, filepath.Dir(includePath), depth+1)
						result = append(result, fmt.Sprintf("<!-- included from %s -->", ref))
						result = append(result, included)
						continue
					}
				}
			}
		}

		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

// resolveIncludePath 解析 @include 路径
func (l *Loader) resolveIncludePath(ref string, baseDir string) string {
	if strings.HasPrefix(ref, "~/") {
		return filepath.Join(l.HomeDir, ref[2:])
	}
	if strings.HasPrefix(ref, "/") {
		return ref
	}
	if strings.HasPrefix(ref, "./") {
		return filepath.Join(baseDir, ref[2:])
	}
	return filepath.Join(baseDir, ref)
}

// getAncestorDirs 获取从 dir 到根目录的所有祖先目录
func (l *Loader) getAncestorDirs(dir string) []string {
	var dirs []string
	current, _ := filepath.Abs(dir)
	for {
		dirs = append(dirs, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return dirs
}

// BuildMemoryPrompt 将加载的记忆文件组装成提示词文本。
// 对应 TS: memdir/memdir.ts 中的 loadMemoryPrompt()
func BuildMemoryPrompt(files []types.MemoryFile) string {
	if len(files) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Codebase and user instructions are shown below. " +
		"Be sure to adhere to these instructions. " +
		"IMPORTANT: These instructions OVERRIDE any default behavior.\n\n")

	for _, f := range files {
		label := string(f.Type)
		sb.WriteString(fmt.Sprintf("--- [%s] %s ---\n%s\n\n", label, f.Path, f.Content))
	}

	return sb.String()
}
