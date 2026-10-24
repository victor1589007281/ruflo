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
//
// 已被打桩为恒返回 0, 即"确定性文本修复"这条能力当前不存在。
//
// 注意原注释称"修复逻辑已交给 LLM 驱动的 ValidationGate"——**该说法不成立**:
// ValidationGate/CodeExecutor 这条契约优先编码链从未接线 (零生产调用, 见
// executor.go 顶部的 Deprecated 说明), 所以打桩换来的替代品并不存在。
// 真实生效的编译修复在 teams.go 的 tryGateWithRemediation: 编译/测试门禁
// 失败后由 LLM 阶段重跑修复, 最多 2 次。此处保留桩函数只为不改调用方签名。
func applyGoCompileTextRepairs(root string) int {
	_ = root
	return 0
}

// applyGoCompileErrorRepairs applies deterministic error-driven repairs.
//
// 同 applyGoCompileTextRepairs: 已打桩恒返回 0, 原注释所称的替代品
// (Contract-First Agent Harness) 从未接线。真实修复环在
// teams.go 的 tryGateWithRemediation。
func applyGoCompileErrorRepairs(root, buildErrors string) int {
	_ = root
	_ = buildErrors
	return 0
}

// readGoModModuleAndRequires 读出 go.mod 的 module 路径与 require 的模块路径列表 (已去重)。
//
// 注意：手写行解析而非 x/mod/modfile —— 只认 `module`、`require (` 块与单行 `require` 三种形态;
// `replace` / `exclude` / `retract` 一律不解析 (它们不在 require 块内, 不会被误收)。
// 块内每行只取第一个字段, 因此行尾的 `// indirect` 注释不影响结果; 文件读不到时返回 ("", nil)。
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

// runTestCheckLang 按语言跑该语言的测试命令集, 返回汇总后的失败输出; 通过或无需测试时返回空串。
//
// 三种「无需测试」的短路: 项目文件不存在、Go 项目没有主源码文件、MySQL 项目的 C++ 单元测试
// 已被禁用 (WITH_UNIT_TESTS=OFF)。Go 项目额外设 GOFLAGS=-mod=readonly, 避免测试顺手改 go.mod。
//
// 注意：OOM 提示的判定条件里 `strings.Contains(errStr, "exited") && len(errStr) < 100`
// 因 && 优先级高于 || 而只约束最后一项 —— 前几个关键词 (killed / OOM 等) 只要出现就命中,
// 与输出长度无关; 读这段条件时容易误以为整条表达式都受长度限制。
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

// hasPrimarySourceFiles 判断目录下是否存在该语言的主源码文件 (决定要不要跑测试)。
//
// 遍历时剪掉 .git / .claude-go / vendor / node_modules / build, 并且遇到内嵌子项目
// (自身带 ProjectFile, 如子模块的 go.mod) 也跳过 —— 只回答「根目录这个项目有没有源码」,
// 不把 vendored 或子模块的源码算进来。
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

// isGoNoPackagesOutput 识别 go test 的「没有匹配到包」输出 —— 这不是测试失败, 应跳过而不是上报错误。
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

// MaterializeCode 把 LLM 输出里的代码块按文件路径落盘到 cwd 下, 返回实际写入的相对路径列表。
//
// 算法解读：
//
//	原理：LLM 的产出是 markdown 文本, 文件路径信息散落在三个地方 —— 围栏语言后的
//	      ````go:path/to/file`、代码块内的 `// File: path` 注释、以及块首行裸路径。
//	      主循环用一条大正则一次性抓出所有带语言标记的代码块, 再按这三种模式依次兜底推断路径;
//	      第二遍单独扫描 `### path/to/file.ext` 这种「标题给路径、下一行才是围栏」的写法。
//	关键不变量：路径必须先过 cleanMaterializeRelPathForCwd 才能拼到 cwd 下 ——
//	      它负责剥掉 cwd 前缀、剥掉重复的目录基名、并拒绝绝对路径与任何含 .. 的路径。
//	      这道闸一旦被绕过, LLM 输出里的 `../../etc/x` 就能写到工作区之外。
//	      另外 seen 保证同一路径只写一次 (先出现的代码块胜出)。
//	代价与边界：路径推断是启发式的, 无路径的代码块直接丢弃 (不猜文件名);
//	      正则不处理围栏嵌套; 非 Go 语言不做内容校验, 只有 .go 文件会经
//	      canMaterializeGoSource 拒绝「用空壳文件覆盖真实源码」的退化写入。
//	设计权衡：宁可少写 (丢弃无法定路径的块) 也不猜 —— 猜错路径会污染工作区,
//	      而漏写的代码块在下游的编译/测试闸门里会自然暴露。
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

// cleanMaterializeRelPath 不带 cwd 上下文的路径清洗包装 (无前缀剥离与基名去重)。
func cleanMaterializeRelPath(path string) string {
	return cleanMaterializeRelPathForCwd("", path)
}

// cleanMaterializeRelPathForCwd 把 LLM 给的路径清洗成可安全拼到 cwd 下的相对路径; 不安全时返回空串。
//
// 算法解读：
//
//	原理：路径可能带 markdown 反引号/引号、`path=` 之类的前缀、中文句末标点,
//	      也可能是绝对路径或已经含 cwd 前缀 —— 逐层剥离后再判定安全性。
//	关键不变量：返回空串 = 调用方必须放弃写入。拒绝的形态包括绝对路径、
//	      任何含 `..` 的路径 (裸 `..`、`../` 开头、中间的 `/../`), 以及剥完前缀后为空的路径。
//	      这条不变量是 MaterializeCode 唯一的目录穿越防线。
//	代价与边界：只做字符串层面的清洗, 不解析符号链接 —— cwd 内若有指向外部的软链,
//	      写入仍会落到链接目标 (调用方需自行保证工作区里没有这种链接)。
//	设计权衡：额外做了一次「cwd 基名去重」(LLM 常写 `agentDBV7/file.go` 而 cwd 已经是该目录),
//	      这属于对模型习惯的迁就, 换来的是不产生 cwd/agentDBV7/file.go 这种嵌套目录。
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
	// 越界防线: 空路径、绝对路径、以及任何形式的 ".." 穿越一律判无效返回空串,
	// 由调用方丢弃 —— 这里不尝试修正, 因为无法区分「确实想写上级目录」与「模型幻觉路径」。
	if path == "" || strings.HasPrefix(path, "/") || path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
		return ""
	}
	if strings.HasPrefix(path, "path=/") || strings.HasPrefix(path, "path=") {
		return ""
	}
	return filepath.FromSlash(filepath.Clean(path))
}

var headerFilePathRe = regexp.MustCompile(`([A-Za-z0-9._/-]+\.[A-Za-z0-9][A-Za-z0-9_-]*)`)

// extractHeaderFilePathForCwd 从 markdown 标题行 (## 开头) 里抠出文件路径; 取不到返回空串。
//
// 只认带目录的路径 (清洗后必须含 "/") —— 裸文件名标题太像普通小标题, 认了会误判。
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

// isRelevantFileExt 判断路径是否属于该语言的源码或常见配置/文档扩展名。
//
// 注意：语言匹配用的是 strings.Contains 而不是后缀比较, 所以 ".go" 会命中 "foo.golden"
// 这类文件名 —— 宽松是有意的 (宁可多写一个配置产物也不要漏掉源码), 但它不是精确判定。
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

// canMaterializeSourceContent 落盘前的按语言内容校验; 只有 Go 源码会被实际检查, 其余一律放行。
//
// 注意：tc 为 nil (语言未知) 时同样放行 —— 校验是尽力而为的, 不是写入门禁。
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

// canMaterializeGoSource 判断一份 Go 源码内容是否可以写盘 —— 拒绝空内容、缺 package 声明, 以及相对旧文件明显退化的版本。
//
// 算法解读：
//
//	原理：LLM 在修复环里经常回吐「只有 package 声明」或「几行注释」的骨架, 覆盖掉原本
//	      完整的源文件。这里把新内容与磁盘上的旧内容对比, 用三条启发式拦住退化:
//	      ① 旧文件 ≥240 字节而新内容 <120 字节 (缩水成骨架);
//	      ② 旧文件有声明而新内容一条声明都没有 (包体被清空);
//	      ③ 旧文件 ≥1000 字节、新内容不足其 1/4, 且声明数不到旧的一半 (整体塌缩)。
//	关键不变量：目标文件不存在或为空时直接放行 —— 这些规则只用于「防止改坏已有文件」,
//	      新建文件不该被它们拦下。另外声明数由 parser 统计, 解析失败返回 0,
//	      所以「新内容语法有错」会走 ② 被拒, 而不是被当成合法文件写入。
//	代价与边界：阈值是经验值; 合法的大幅重构 (把 1200 字节的文件拆成两个 400 字节的)
//	      会被 ③ 误拒, 只能靠调用方另想办法落盘。
//	设计权衡：宁误拒不误写 —— 拒了只是少一次写入, 后面还有编译闸门兜底;
//	      而写坏已有源码会让整个工作区退回到不可编译状态, 代价大得多。
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

// goSourceHasPackageDecl 判断源码里是否存在 package 声明 —— 只看第一处非空非注释行是否为 package。
//
// 注意：这是「首个有效行必须是 package」的近似判定, 而不是全文件搜索 ——
// 因此 package 声明前若夹了 //go:build 之外的语句 (如误置的 import), 会判为无声明。
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

// goSourceDeclCount 用 Go parser 统计顶层声明条数; 语法错误导致解析失败时返回 0。
//
// 注意：返回 0 有两种含义 —— 「文件里真没有声明」和「文件语法错到解析不了」。
// 调用方 (canMaterializeGoSource) 把 0 一律当作退化信号, 正是利用了这个二义性。
func goSourceDeclCount(src string) int {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments)
	if err != nil || f == nil {
		return 0
	}
	return len(f.Decls)
}

// outputContainsRelevantCodeBlock 判断 LLM 输出里是否含该语言的代码块 (语言未知时只要求有围栏)。
//
// 注意：判定整体先转小写再匹配关键词, 所以 `PACKAGE ` / `#Include` 这类写法也能命中;
// 反过来, 输出里只要出现一次 "package " 这个词 (哪怕在正文里) 就会判为有 Go 代码。
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

// filterParallel 从一批「依赖已满足」的阶段里挑出可以并发执行的那一组。
//
// 算法解读：
//
//	原理：三级降级 —— ① 显式标了 Parallel 且不止一个, 直接全取;
//	      ② 否则按 DependsOn 集合分组 (依赖集相同 ⇒ 互不依赖, 可并行), 取最大的一组;
//	      ③ 都不满足时返回全部输入 (调用方保证这批阶段的依赖已满足, 无顺序约束)。
//	关键不变量：返回的每个阶段必须来自入参 stages, 且返回非空 —— 调用方拿它当
//	      「本轮并行集」直接执行, 返回空集会让调度循环空转。
//	      分组键用 Join(DependsOn, ","), 依赖顺序不同的两组会被视作不同组 (例如
//	      [a,b] 与 [b,a] 分到两组), 因而少并行了两个本可并行的阶段 —— 是保守的偏差。
//	代价与边界：不做真正的拓扑分层, 只看依赖集字面是否相同; 依赖集是子集关系的阶段
//	      不会被并行。map 遍历顺序随机, 出现两个等大的最大组时选哪个不确定。
//	设计权衡：用「同依赖集」这个充分条件替代完整的并发安全性分析, 零解析成本;
//	      代价是并行度偏低, 但绝不会把有依赖关系的阶段并到一起。
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

// goExternalImports 扫描 cwd 下所有 .go 文件的 import, 返回去重排序后的「外部」依赖路径。
//
// 外部 = 非标准库、且不属于本模块 (modulePath 及其子包)。vendor / node_modules /
// .git / .claude-go 下的文件不参与扫描; 解析失败的文件静默跳过。
//
// 注意：只做 parser.ImportsOnly 解析, 拿不到 build tag 的约束 —— 被 `//go:build ignore`
// 排除的文件里的 import 同样会被算进来, 从而可能报出「未声明依赖」的假阳性。
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

// isStdlibImport 判断 import 路径是否属于标准库 —— 首段不含 "." 即视为标准库。
//
// 依据是 Go 官方约定: 域名形式的模块路径首段必带点 (如 github.com/...), 标准库不带。
// 注意：这条启发式对 `gopkg.in/yaml.v3` 之外的少数老式路径有效, 但会把
// 任何首段无点的第三方路径误判为标准库。
func isStdlibImport(importPath string) bool {
	first := importPath
	if idx := strings.Index(first, "/"); idx >= 0 {
		first = first[:idx]
	}
	return !strings.Contains(first, ".")
}

// isImportDeclaredByRequire 判断 import 路径是否被某个 require 模块覆盖 (同路径或其子包)。
func isImportDeclaredByRequire(importPath string, requires []string) bool {
	for _, req := range requires {
		if importPath == req || strings.HasPrefix(importPath, req+"/") {
			return true
		}
	}
	return false
}

// undeclaredGoExternalImports 返回代码里用到、但 go.mod 的 require 中没有声明的外部依赖 (去重排序)。
//
// 注意：go.mod 里的间接依赖 (indirect) 同样算「已声明」, 所以这里判出的都是真正缺失的模块;
// 但反向不成立 —— 声明了却没用到的依赖不会被本函数发现 (它只做单向差集)。
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

// ensureGoModuleDependencies 校验并补齐 Go 模块依赖: 有未声明依赖则直接返回提示 (拒绝自动引入), 否则跑 go mod tidy。
//
// 算法解读：
//
//	原理：先做「未声明依赖」检查, 有则短路返回而不是让 tidy 去下载 ——
//	      目的是不让 LLM 随手引入第三方库, 强制它优先用标准库或本仓实现。
//	关键不变量：go mod tidy 必须被 goModTidyMutex 串行化 —— 多个并行阶段同时跑 tidy
//	      会并发改写同一份 go.mod/go.sum, 轻则失败重试, 重则写坏依赖声明。
//	      另外 tidy 用 runLimitedCommandWithNetwork 的 network=false 调用, 即禁止联网。
//	代价与边界：tidy 超时 45 秒且结果截断到 3000 字符; 离线环境下需要下载新依赖的
//	      场景必然失败 —— 这正是「默认禁止自动引入」的预期表现。
//	设计权衡：用「先拒绝、再 tidy」两步代替「直接 tidy 让它自己补」, 牺牲了便利性,
//	      换来依赖面的可控 —— 是否引入新依赖由人在同一 Leaf 里显式决定。
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

// buildPrevResultsSummary 把上游各阶段的产出压缩成 prompt 片段 (每段 1000 字摘要 + 原始产物的引用路径)。
//
// 注意：map 遍历顺序随机, 因此拼出的片段顺序每次都可能不同 —— 内容等价但字节不同,
// 会影响 prompt 缓存的命中。摘要里同时给出 prevResults 键与 blackboard key, 让模型需要全文时能按引用回查。
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

// getSystemMemoryMB 读取系统总内存 (MB); 取不到时兜底为 16384。
//
// 注意：走的是 `sh -c "free -m | awk ..."` 而不是直接读 /proc/meminfo ——
// 依赖 free 与 awk 存在, 且在容器里拿到的是宿主/节点总量而非 cgroup 限额,
// 因此容器内可能严重高估可用内存。
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
