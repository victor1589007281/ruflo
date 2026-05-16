package agent

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// goModTidyMutex 串行化 go mod tidy，防止多个并行任务同时修改共享的 go.mod/go.sum。
var goModTidyMutex sync.Mutex

// applyGoCompileTextRepairs applies deterministic text-level repairs.
// DEPRECATED: Will be replaced by Contract-First Agent Harness (CodeExecutor).
// Stubbed to return 0 — all repair logic now handled by LLM-driven ValidationGate.
func applyGoCompileTextRepairs(root string) int {
	_ = root
	return 0
}

// applyGoCompileErrorRepairs applies deterministic error-driven repairs.
// DEPRECATED: Will be replaced by Contract-First Agent Harness (CodeExecutor).
// Stubbed to return 0 — all repair logic now handled by LLM-driven ValidationGate.
func applyGoCompileErrorRepairs(root, buildErrors string) int {
	_ = root
	_ = buildErrors
	return 0
}

func readGoModModuleAndRequires(path string) (string, []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	var modulePath string
	var requires []string
	inRequireBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "module ") {
			modulePath = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			continue
		}
		if strings.HasPrefix(line, "require (") {
			inRequireBlock = true
			continue
		}
		if inRequireBlock && line == ")" {
			inRequireBlock = false
			continue
		}
		if strings.HasPrefix(line, "require ") {
			fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "require ")))
			if len(fields) > 0 {
				requires = append(requires, fields[0])
			}
			continue
		}
		if inRequireBlock {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				requires = append(requires, fields[0])
			}
		}
	}
	return modulePath, uniqueStrings(requires)
}

func runTestCheckLang(cwd, lang string) string {
	if cwd == "" {
		return ""
	}
	tc := GetToolchain(lang)
	if _, err := os.Stat(filepath.Join(cwd, tc.ProjectFile)); err != nil {
		return ""
	}
	if tc.Language == "go" && !hasPrimarySourceFiles(cwd, tc) {
		return ""
	}
	// MySQL/C++ 项目: 单元测试已禁用 (WITH_UNIT_TESTS=OFF)
	if lang == "cpp" && isMySQLProject(cwd) {
		return ""
	}
	if len(tc.TestCmds) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), tc.Timeout)
	defer cancel()

	var errors []string
	for _, args := range tc.TestCmds {
		var env map[string]string
		if tc.Language == "go" {
			env = map[string]string{"GOFLAGS": "-mod=readonly"}
		}
		out, err := runLimitedCommandWithNetwork(ctx, cwd, args, tc.MemoryMaxMB, tc.CPUQuotaPercent, true, env)
		if err != nil {
			if tc.Language == "go" && isGoNoPackagesOutput(string(out)) {
				continue
			}
			errStr := string(out)
			label := fmt.Sprintf("%s 失败:", strings.Join(args, " "))
			// 检测内存限制/OOM 相关错误, 追加明确提示
			if tc.MemoryMaxMB > 0 && (strings.Contains(errStr, "killed") || strings.Contains(errStr, "Killed") ||
				strings.Contains(errStr, "signal: killed") || strings.Contains(errStr, "OOM") ||
				strings.Contains(errStr, "out of memory") || strings.Contains(errStr, "cannot allocate memory") ||
				strings.Contains(errStr, "exited") && len(errStr) < 100) {
				label = fmt.Sprintf("%s (⚠️ 疑似内存超限, 当前限制 %dMB。请检查测试代码是否存在无限循环或未限制的数据结构增长):", strings.Join(args, " "), tc.MemoryMaxMB)
			}
			errors = append(errors, fmt.Sprintf("%s\n%s", label, errStr))
		}
	}
	if len(errors) == 0 {
		return ""
	}
	result := strings.Join(errors, "\n\n")
	if len(result) > 3000 {
		result = result[:3000] + "\n...(截断)"
	}
	return result
}

func hasPrimarySourceFiles(cwd string, tc *LanguageToolchain) bool {
	if cwd == "" || tc == nil {
		return false
	}
	primaryExts := map[string]bool{tc.FileExt: true}
	switch tc.Language {
	case "cpp":
		primaryExts[".c"] = true
		primaryExts[".cc"] = true
		primaryExts[".h"] = true
		primaryExts[".hpp"] = true
	}

	found := false
	_ = filepath.WalkDir(cwd, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if d.IsDir() {
			if path != cwd {
				name := d.Name()
				if name == ".git" || name == ".claude-go" || name == "vendor" || name == "node_modules" || name == "build" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, tc.ProjectFile)); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if primaryExts[filepath.Ext(path)] {
			found = true
		}
		return nil
	})
	return found
}

func isGoNoPackagesOutput(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "matched no packages") || strings.Contains(lower, "no packages to")
}

// isMySQLProject 检测是否为 MySQL/Percona-Server 项目。
// 判断依据: 根目录存在 CMakeLists.txt 且包含 mysqld 相关配置。
func isMySQLProject(cwd string) bool {
	if cwd == "" {
		return false
	}
	// 检查特征文件
	features := []string{
		"sql/mysqld.cc",
		"sql/sql_parse.cc",
		"sql/sql_yacc.yy",
		"VERSION",
	}
	for _, f := range features {
		if _, err := os.Stat(filepath.Join(cwd, f)); err == nil {
			return true
		}
	}
	// 检查 CMakeCache 中是否有 mysqld 相关内容
	cacheFile := filepath.Join(cwd, "build", "CMakeCache.txt")
	if data, err := os.ReadFile(cacheFile); err == nil {
		return strings.Contains(string(data), "mysqld") ||
			strings.Contains(string(data), "MYSQLD")
	}
	return false
}

func MaterializeCode(cwd, output, lang string) []string {
	if cwd == "" || output == "" {
		return nil
	}
	tc := GetToolchain(lang)
	ext := tc.FileExt

	var written []string
	seen := make(map[string]bool)

	reBlock := regexp.MustCompile("(?s)```(?:go|cpp|c\\+\\+|rust|rs|python|py|typescript|ts|javascript|js|h|hpp|toml|cmake|mod|makefile|txt|md|markdown|json|yaml|yml)(?::([^\\n]+))?\\n(.*?)```")
	reFilePath := regexp.MustCompile(`(?m)^(?://|#|/\*)\s*(?:File|file|PATH|path|filename|Filename):\s*(.+?)(?:\s*\*/)?$`)

	for _, match := range reBlock.FindAllStringSubmatch(output, -1) {
		block := match[2]
		var filePath string

		// 模式 1: ```lang:path/to/file
		if match[1] != "" {
			filePath = strings.TrimSpace(match[1])
		}
		// 模式 2: 代码块内 // File: path 或 # File: path
		if filePath == "" {
			if fpMatch := reFilePath.FindStringSubmatch(block); len(fpMatch) > 1 {
				filePath = strings.TrimSpace(fpMatch[1])
			}
		}
		// 模式 3: 代码块第一行就是文件路径 (e.g. "src/main.rs" 或 "include/kv.h")
		if filePath == "" {
			firstLine := strings.TrimSpace(strings.SplitN(block, "\n", 2)[0])
			if strings.Contains(firstLine, "/") && strings.Contains(firstLine, ".") && len(firstLine) < 80 && !strings.Contains(firstLine, " ") {
				filePath = firstLine
				block = strings.SplitN(block, "\n", 2)[1]
			}
		}
		if filePath == "" {
			continue
		}
		filePath = cleanMaterializeRelPathForCwd(cwd, filePath)
		if filePath == "" {
			continue
		}

		if !isRelevantFileExt(filePath, ext) {
			continue
		}
		if seen[filePath] {
			continue
		}

		full := filepath.Join(cwd, filePath)
		dir := filepath.Dir(full)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		cleanBlock := reFilePath.ReplaceAllString(block, "")
		content := strings.TrimSpace(cleanBlock) + "\n"
		if !canMaterializeSourceContent(full, filePath, content, tc) {
			continue
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			continue
		}
		written = append(written, filePath)
		seen[filePath] = true
	}

	// 模式 4: markdown header "### path/to/file.ext" 后紧跟代码块
	lines := strings.Split(output, "\n")
	for i := 0; i < len(lines)-1; i++ {
		candidate := extractHeaderFilePathForCwd(cwd, lines[i], ext)
		if candidate == "" {
			continue
		}
		if !isRelevantFileExt(candidate, ext) || seen[candidate] {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, "```") {
				blockStart := j + 1
				for k := blockStart; k < len(lines); k++ {
					if strings.HasPrefix(strings.TrimSpace(lines[k]), "```") {
						block := strings.Join(lines[blockStart:k], "\n")
						full := filepath.Join(cwd, candidate)
						dir := filepath.Dir(full)
						if err := os.MkdirAll(dir, 0o755); err == nil {
							content := strings.TrimSpace(block) + "\n"
							if !canMaterializeSourceContent(full, candidate, content, tc) {
								break
							}
							if err := os.WriteFile(full, []byte(content), 0o644); err == nil {
								written = append(written, candidate)
								seen[candidate] = true
							}
						}
						break
					}
				}
			}
			break
		}
	}

	return written
}

func cleanMaterializeRelPath(path string) string {
	return cleanMaterializeRelPathForCwd("", path)
}

func cleanMaterializeRelPathForCwd(cwd, path string) string {
	path = strings.TrimSpace(strings.Trim(path, "`\"'"))
	path = strings.TrimPrefix(path, "path=")
	path = strings.TrimPrefix(path, "file=")
	path = strings.TrimPrefix(path, "filepath=")
	path = strings.TrimPrefix(path, "file_path=")
	path = strings.TrimSpace(strings.Trim(path, "`\"'"))
	path = strings.TrimRight(path, "。:：,，)")
	path = filepath.ToSlash(path)
	if cwd != "" && strings.HasPrefix(path, "/") {
		cleanCwd := filepath.ToSlash(filepath.Clean(cwd))
		cleanPath := filepath.ToSlash(filepath.Clean(path))
		if cleanPath == cleanCwd {
			return ""
		}
		if strings.HasPrefix(cleanPath, cleanCwd+"/") {
			path = strings.TrimPrefix(cleanPath, cleanCwd+"/")
		}
	}
	path = strings.TrimPrefix(path, "./")
	// 关键修复: 当 LLM 输出 "<basename(cwd)>/file.ext", 而 cwd 已经指向该目录时, 去掉前缀避免嵌套
	// 例如: cwd=/path/to/agentDBV7, path="agentDBV7/file.go" → 应该写入 cwd/file.go, 而非 cwd/agentDBV7/file.go
	if cwd != "" {
		cwdBase := filepath.Base(filepath.Clean(cwd))
		if cwdBase != "" && cwdBase != "." && cwdBase != "/" {
			prefix := cwdBase + "/"
			if strings.HasPrefix(path, prefix) {
				path = strings.TrimPrefix(path, prefix)
			}
		}
	}
	if path == "" || strings.HasPrefix(path, "/") || path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
		return ""
	}
	if strings.HasPrefix(path, "path=/") || strings.HasPrefix(path, "path=") {
		return ""
	}
	return filepath.FromSlash(filepath.Clean(path))
}

var headerFilePathRe = regexp.MustCompile(`([A-Za-z0-9._/-]+\.[A-Za-z0-9][A-Za-z0-9_-]*)`)

func extractHeaderFilePathForCwd(cwd, line, langExt string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "##") {
		return ""
	}
	matches := headerFilePathRe.FindAllString(trimmed, -1)
	for _, match := range matches {
		candidate := cleanMaterializeRelPathForCwd(cwd, match)
		if candidate == "" || !strings.Contains(filepath.ToSlash(candidate), "/") {
			continue
		}
		if isRelevantFileExt(candidate, langExt) {
			return candidate
		}
	}
	return ""
}

func isRelevantFileExt(filePath, langExt string) bool {
	commonExts := []string{".toml", ".cmake", ".txt", ".md", ".json", ".yaml", ".yml", ".cfg", ".ini", ".mod"}
	if strings.Contains(filePath, langExt) {
		return true
	}
	for _, ce := range commonExts {
		if strings.HasSuffix(filePath, ce) {
			return true
		}
	}
	lp := strings.ToLower(filePath)
	return strings.HasSuffix(lp, ".h") || strings.HasSuffix(lp, ".hpp") ||
		strings.HasSuffix(lp, ".c") || strings.HasSuffix(lp, ".cc")
}

func canMaterializeSourceContent(fullPath, relPath, content string, tc *LanguageToolchain) bool {
	if tc == nil {
		return true
	}
	ext := strings.ToLower(filepath.Ext(relPath))
	switch tc.Language {
	case "go":
		if ext != ".go" {
			return true
		}
		return canMaterializeGoSource(fullPath, content)
	default:
		return true
	}
}

func canMaterializeGoSource(fullPath, content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	if !goSourceHasPackageDecl(trimmed) {
		return false
	}
	newDecls := goSourceDeclCount(trimmed)
	if old, err := os.ReadFile(fullPath); err == nil {
		oldTrimmed := strings.TrimSpace(string(old))
		if oldTrimmed == "" {
			return true
		}
		oldDecls := goSourceDeclCount(oldTrimmed)
		// E2E/repair outputs must provide full file contents. Refuse obvious
		// degradations such as replacing a real source file with package-only
		// scaffolding or a tiny comment shell.
		if len(oldTrimmed) >= 240 && len(trimmed) < 120 && oldDecls > 0 {
			return false
		}
		if oldDecls > 0 && newDecls == 0 {
			return false
		}
		if len(oldTrimmed) >= 1000 && len(trimmed)*4 < len(oldTrimmed) && newDecls < oldDecls/2 {
			return false
		}
	}
	return true
}

func goSourceHasPackageDecl(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		return strings.HasPrefix(trimmed, "package ")
	}
	return false
}

func goSourceDeclCount(src string) int {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments)
	if err != nil || f == nil {
		return 0
	}
	return len(f.Decls)
}

func outputContainsRelevantCodeBlock(output, lang string) bool {
	if output == "" {
		return false
	}
	tc := GetToolchain(lang)
	lower := strings.ToLower(output)
	switch tc.Language {
	case "go":
		return strings.Contains(lower, "```go") || strings.Contains(lower, "package ")
	case "rust":
		return strings.Contains(lower, "```rust") || strings.Contains(lower, "```rs") || strings.Contains(lower, "fn ")
	case "python":
		return strings.Contains(lower, "```python") || strings.Contains(lower, "```py")
	case "typescript":
		return strings.Contains(lower, "```typescript") || strings.Contains(lower, "```ts") || strings.Contains(lower, "export ")
	case "javascript":
		return strings.Contains(lower, "```javascript") || strings.Contains(lower, "```js") || strings.Contains(lower, "export ")
	case "cpp":
		return strings.Contains(lower, "```cpp") || strings.Contains(lower, "```c++") || strings.Contains(lower, "#include")
	default:
		return strings.Contains(lower, "```")
	}
}

func filterParallel(stages []StageDef) []StageDef {
	if len(stages) <= 1 {
		return stages
	}

	// 优先: 如果有显式标记 Parallel 的, 全部并行
	var explicit []StageDef
	for _, s := range stages {
		if s.Parallel {
			explicit = append(explicit, s)
		}
	}
	if len(explicit) > 1 {
		return explicit
	}

	// 所有 ready 阶段的依赖已满足 → 按依赖集分组, 同组可并行
	groups := make(map[string][]StageDef)
	for _, s := range stages {
		key := strings.Join(s.DependsOn, ",")
		groups[key] = append(groups[key], s)
	}

	// 选最大的同依赖组
	var best []StageDef
	for _, g := range groups {
		if len(g) > len(best) {
			best = g
		}
	}
	if len(best) > 1 {
		return best
	}

	// 兜底: 所有 ready 阶段依赖已满足, 无执行顺序约束, 全部并行
	return stages
}

func goExternalImports(cwd string) []string {
	modulePath, _ := readGoModModuleAndRequires(filepath.Join(cwd, "go.mod"))
	seen := make(map[string]bool)
	_ = filepath.Walk(cwd, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", ".claude-go", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		f, parseErr := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if parseErr != nil {
			return nil
		}
		for _, imp := range f.Imports {
			pathValue, unquoteErr := strconv.Unquote(imp.Path.Value)
			if unquoteErr != nil || pathValue == "" {
				continue
			}
			if isStdlibImport(pathValue) {
				continue
			}
			if modulePath != "" && (pathValue == modulePath || strings.HasPrefix(pathValue, modulePath+"/")) {
				continue
			}
			seen[pathValue] = true
		}
		return nil
	})
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for dep := range seen {
		out = append(out, dep)
	}
	sort.Strings(out)
	return out
}

func isStdlibImport(importPath string) bool {
	first := importPath
	if idx := strings.Index(first, "/"); idx >= 0 {
		first = first[:idx]
	}
	return !strings.Contains(first, ".")
}

func isImportDeclaredByRequire(importPath string, requires []string) bool {
	for _, req := range requires {
		if importPath == req || strings.HasPrefix(importPath, req+"/") {
			return true
		}
	}
	return false
}

func undeclaredGoExternalImports(cwd string) []string {
	_, requires := readGoModModuleAndRequires(filepath.Join(cwd, "go.mod"))
	seen := make(map[string]bool)
	for _, importPath := range goExternalImports(cwd) {
		if isImportDeclaredByRequire(importPath, requires) {
			continue
		}
		seen[importPath] = true
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for dep := range seen {
		out = append(out, dep)
	}
	sort.Strings(out)
	return out
}

func ensureGoModuleDependencies(cwd, lang string, tc *LanguageToolchain) string {
	if cwd == "" || tc == nil || tc.Language != "go" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err != nil {
		return ""
	}
	if undeclared := undeclaredGoExternalImports(cwd); len(undeclared) > 0 {
		return "Go 外部依赖未声明且默认禁止自动引入。请优先改为标准库/本项目内实现；如确需第三方依赖，必须在同一 Leaf 明确修改 go.mod 并说明原因。\n未声明依赖: " + strings.Join(undeclared, ", ")
	}
	goModTidyMutex.Lock()
	defer goModTidyMutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	out, err := runLimitedCommandWithNetwork(ctx, cwd, []string{"go", "mod", "tidy"}, tc.MemoryMaxMB, tc.CPUQuotaPercent, false, nil)
	if err == nil {
		return ""
	}
	result := "go mod tidy 失败:\n" + string(out)
	if len(result) > 3000 {
		result = result[:3000] + "\n...(截断)"
	}
	return result
}

func buildPrevResultsSummary(prevResults map[string]string) string {
	var b strings.Builder
	for name, output := range prevResults {
		summary := SummarizeOldOutput(output, 1000)
		b.WriteString(fmt.Sprintf("### %s:\n%s\n\nFull artifact/ref: prevResults[%q] / blackboard key `%s-result`.\n\n", name, summary, name, name))
	}
	return b.String()
}

// CalcSafeMakeJobs 根据系统内存计算安全的编译并行度。
func CalcSafeMakeJobs() int {
	totalMemMB := getSystemMemoryMB()

	// 保留 4GB 给系统
	reservedMB := int64(4096)
	availableMB := totalMemMB - reservedMB
	if availableMB < 2048 {
		availableMB = 2048 // 最小 2GB
	}

	// 每 job 约 2GB
	perJobMB := int64(2048)
	jobs := int(availableMB / perJobMB)

	// 上限: CPU 核心数和 8 的较小值
	cpuCount := runtime.NumCPU()
	if jobs > cpuCount {
		jobs = cpuCount
	}
	if jobs > 8 {
		jobs = 8
	}
	if jobs < 1 {
		jobs = 1
	}
	return jobs
}

func getSystemMemoryMB() int64 {
	// Linux: 读取 /proc/meminfo
	out, err := exec.Command("sh", "-c", "free -m | awk '/^Mem:/{print $2}'").Output()
	if err != nil {
		return 16384 // 兜底: 假设 16GB
	}
	mb, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if mb <= 0 {
		return 16384
	}
	return mb
}

// ── Test support stubs (functions referenced by workflow_materialize_test.go) ──
// NOTE: real implementations now live in workflow_adversarial_dev.go
