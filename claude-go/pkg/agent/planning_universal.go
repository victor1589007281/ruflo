package agent

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type planningLanguageAdapter struct {
	ID             string
	Display        string
	SourceExt      string
	TestSuffix     string
	ManifestFile   string
	VerifyCommand  string
	DirectorySpecs []planningDirectoryLeafSpec
}

type planningDirectoryLeafSpec struct {
	Suffix       string
	Title        string
	Acceptance   string
	Minutes      int
	WorkUnitType string
	Role         string
	IsTest       bool
}

type timeoutClassification struct {
	Kind       string
	Action     string
	Reason     string
	AllowSplit bool
}

func normalizePlanningID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	re := regexp.MustCompile(`[^a-z0-9._:-]+`)
	s = re.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func uniqueTrimmedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func normalizePlanFiles(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, f := range in {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		for _, expanded := range expandPlanBracePath(f) {
			cleaned := filepath.ToSlash(filepath.Clean(expanded))
			if cleaned == "." || strings.HasPrefix(cleaned, "../") || filepath.IsAbs(cleaned) {
				out = append(out, expanded)
				continue
			}
			out = append(out, cleaned)
		}
	}
	return uniqueTrimmedStrings(out)
}

func expandPlanBracePath(pathValue string) []string {
	pathValue = strings.TrimSpace(pathValue)
	if pathValue == "" || !strings.Contains(pathValue, "{") || !strings.Contains(pathValue, "}") {
		return []string{pathValue}
	}
	start := strings.Index(pathValue, "{")
	endRel := strings.Index(pathValue[start+1:], "}")
	if start < 0 || endRel < 0 {
		return []string{pathValue}
	}
	end := start + 1 + endRel
	body := pathValue[start+1 : end]
	if body == "" || strings.ContainsAny(body, "{}") {
		return []string{pathValue}
	}
	parts := strings.Split(body, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || strings.Contains(part, "/") || strings.Contains(part, "\\") {
			continue
		}
		out = append(out, pathValue[:start]+part+pathValue[end+1:])
	}
	if len(out) == 0 {
		return []string{pathValue}
	}
	return out
}

func rawTaskWriteFiles(rt rawTask) []string {
	if len(rt.writeFiles) > 0 {
		return rt.writeFiles
	}
	return rt.targetFiles
}

func rawTaskConflictKeys(rt rawTask) []string {
	var keys []string
	keys = append(keys, rt.conflictKeys...)
	return uniqueTrimmedStrings(keys)
}

func nonGlobalRawTaskConflictKeys(rt rawTask) []string {
	keys := rawTaskConflictKeys(rt)
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || strings.HasSuffix(strings.ToLower(key), ":global") {
			continue
		}
		out = append(out, key)
	}
	return uniqueTrimmedStrings(out)
}

func estimateRawTaskLOC(rt rawTask) int {
	files := len(rawTaskWriteFiles(rt))
	if files <= 0 {
		files = len(rt.targetFiles)
	}
	switch rt.taskType {
	case wbsTaskTypeVerification:
		return 20
	case wbsTaskTypeMacro:
		return 400
	}
	switch strings.ToLower(rt.complexity) {
	case "simple", "low":
		return max(60, files*60)
	case "complex", "high":
		return max(220, files*140)
	default:
		return max(120, files*90)
	}
}

func synthesizedObjectiveWBS(objective string) []rawTask {
	adapter := planningLanguageAdapterFor(inferPlanningLanguageID(objective, ""))
	targetRoot := inferObjectiveTargetRoot(objective)
	if targetRoot == "" {
		targetRoot = "app"
	}
	targetRoot = strings.Trim(strings.TrimSpace(filepath.ToSlash(targetRoot)), "/")
	if targetRoot == "" || strings.HasPrefix(targetRoot, "../") || filepath.IsAbs(targetRoot) {
		targetRoot = "app"
	}
	return synthesizedUniversalV1WBS(targetRoot, adapter)
}

func synthesizedUniversalV1WBS(root string, adapter planningLanguageAdapter) []rawTask {
	if adapter.ID == "" {
		adapter = planningLanguageAdapterFor("")
	}
	manifest := filepath.ToSlash(filepath.Join(root, adapter.ManifestFile))
	mainFile := filepath.ToSlash(filepath.Join(root, universalMainFileForAdapter(adapter)))
	coreFile := filepath.ToSlash(filepath.Join(root, universalCoreFileForAdapter(adapter)))
	storeFile := filepath.ToSlash(filepath.Join(root, universalStoreFileForAdapter(adapter)))
	searchFile := filepath.ToSlash(filepath.Join(root, universalSearchFileForAdapter(adapter)))
	testFile := filepath.ToSlash(filepath.Join(root, universalTestFileForAdapter(adapter)))
	verify := universalVerifyCommandForAdapter(root, adapter)
	return []rawTask{
		{
			num:            "1",
			title:          "创建最小可编译项目骨架",
			role:           "coder",
			taskType:       wbsTaskTypeLeaf,
			workUnitType:   wbsWorkUnitContract,
			parentID:       "project",
			estimatedMin:   2,
			riskLevel:      wbsRiskLow,
			blockingPolicy: wbsBlockingFailBlocks,
			targetFiles:    []string{manifest},
			writeFiles:     []string{manifest},
			accept:         "只创建目标语言 manifest; 不引用未来模块; scoped build/check 通过",
			verifyCommand:  verify,
			splitReason:    "objective-fallback:planner-validation-exhausted",
		},
		{
			num:            "2",
			title:          "创建本地入口与空运行路径",
			role:           "coder",
			taskType:       wbsTaskTypeLeaf,
			workUnitType:   wbsWorkUnitImplementation,
			parentID:       "project",
			depNums:        []string{"1"},
			estimatedMin:   2,
			riskLevel:      wbsRiskLow,
			blockingPolicy: wbsBlockingFailBlocks,
			targetFiles:    []string{mainFile},
			writeFiles:     []string{mainFile},
			readFiles:      []string{manifest},
			accept:         "创建最小入口或导出文件; 不 import 未生成模块; scoped build/check 通过",
			verifyCommand:  verify,
			splitReason:    "objective-fallback:planner-validation-exhausted",
		},
		{
			num:            "3",
			title:          "定义核心 API 与数据类型",
			role:           "coder",
			taskType:       wbsTaskTypeLeaf,
			workUnitType:   wbsWorkUnitContract,
			parentID:       "core",
			depNums:        []string{"2"},
			estimatedMin:   3,
			riskLevel:      wbsRiskMedium,
			blockingPolicy: wbsBlockingFailBlocks,
			targetFiles:    []string{coreFile},
			writeFiles:     []string{coreFile},
			readFiles:      []string{mainFile},
			accept:         "定义通用核心接口、错误、配置和数据类型; 不实现高级内核/外部服务面; scoped build/check 通过",
			verifyCommand:  verify,
			splitReason:    "objective-fallback:planner-validation-exhausted",
			conflictKeys:   []string{"contract:" + root},
		},
		{
			num:            "4",
			title:          "实现存储构造与私有状态",
			role:           "coder",
			taskType:       wbsTaskTypeLeaf,
			workUnitType:   wbsWorkUnitImplementation,
			parentID:       "storage",
			depNums:        []string{"3"},
			estimatedMin:   2,
			riskLevel:      wbsRiskMedium,
			blockingPolicy: wbsBlockingFailBlocks,
			targetFiles:    []string{storeFile},
			writeFiles:     []string{storeFile},
			readFiles:      []string{coreFile},
			accept:         "只实现 store 类型、构造函数和私有 map/集合状态; 不实现查询/搜索/高级持久化; 不在非本地包类型上定义方法; scoped build/check 通过",
			verifyCommand:  verify,
			splitReason:    "objective-fallback:planner-validation-exhausted",
		},
		{
			num:            "5",
			title:          "实现存储 CRUD 闭环",
			role:           "coder",
			taskType:       wbsTaskTypeLeaf,
			workUnitType:   wbsWorkUnitImplementation,
			parentID:       "storage",
			depNums:        []string{"4"},
			estimatedMin:   2,
			riskLevel:      wbsRiskMedium,
			blockingPolicy: wbsBlockingFailBlocks,
			targetFiles:    []string{storeFile},
			writeFiles:     []string{storeFile},
			readFiles:      []string{coreFile, storeFile},
			accept:         "只补 Put/Get/Delete/List 或等价最小 CRUD; 使用已定义核心契约, 不创建重复接口/跨包方法; scoped build/check 通过",
			verifyCommand:  verify,
			splitReason:    "objective-fallback:planner-validation-exhausted",
		},
		{
			num:            "6",
			title:          "实现最小检索与查询能力",
			role:           "coder",
			taskType:       wbsTaskTypeLeaf,
			workUnitType:   wbsWorkUnitImplementation,
			parentID:       "query",
			depNums:        []string{"5"},
			estimatedMin:   2,
			riskLevel:      wbsRiskMedium,
			blockingPolicy: wbsBlockingFailBlocks,
			targetFiles:    []string{searchFile},
			writeFiles:     []string{searchFile},
			readFiles:      []string{coreFile, storeFile},
			accept:         "实现关键词/线性检索或等价最小查询闭环; 不实现高级索引生产引擎; scoped build/check 通过",
			verifyCommand:  verify,
			splitReason:    "objective-fallback:planner-validation-exhausted",
		},
		{
			num:            "7",
			title:          "补最小本地验证用例",
			role:           "tester",
			taskType:       wbsTaskTypeLeaf,
			workUnitType:   wbsWorkUnitVerification,
			parentID:       "verification",
			depNums:        []string{"6"},
			estimatedMin:   2,
			riskLevel:      wbsRiskLow,
			blockingPolicy: wbsBlockingFailBlocks,
			targetFiles:    []string{testFile},
			writeFiles:     []string{testFile},
			readFiles:      []string{coreFile, storeFile, searchFile},
			accept:         universalTestAcceptanceForAdapter(adapter),
			verifyCommand:  verify,
			splitReason:    "objective-fallback:planner-validation-exhausted",
		},
		{
			num:            "v-final",
			title:          "本地验证与回归检查",
			role:           "tester",
			taskType:       wbsTaskTypeVerification,
			workUnitType:   wbsWorkUnitVerification,
			parentID:       "verification",
			depNums:        []string{"7"},
			estimatedMin:   2,
			riskLevel:      wbsRiskLow,
			blockingPolicy: wbsBlockingFailBlocks,
			verifyCommand:  verify,
			accept:         "本地执行 build/test/TODO scan; 不调用 tester LLM",
			splitReason:    "objective-fallback:planner-validation-exhausted",
		},
	}
}

func universalMainFileForAdapter(adapter planningLanguageAdapter) string {
	switch adapter.ID {
	case "python":
		return filepath.ToSlash(filepath.Join("src", "__init__.py"))
	case "rust":
		return filepath.ToSlash(filepath.Join("src", "lib.rs"))
	case "typescript":
		return filepath.ToSlash(filepath.Join("src", "index.ts"))
	case "javascript":
		return filepath.ToSlash(filepath.Join("src", "index.js"))
	case "cpp":
		return filepath.ToSlash(filepath.Join("src", "main.cpp"))
	default:
		return filepath.ToSlash(filepath.Join("cmd", "app", "main.go"))
	}
}

func universalCoreFileForAdapter(adapter planningLanguageAdapter) string {
	switch adapter.ID {
	case "python":
		return filepath.ToSlash(filepath.Join("src", "core.py"))
	case "rust":
		return filepath.ToSlash(filepath.Join("src", "core.rs"))
	case "typescript":
		return filepath.ToSlash(filepath.Join("src", "core.ts"))
	case "javascript":
		return filepath.ToSlash(filepath.Join("src", "core.js"))
	case "cpp":
		return filepath.ToSlash(filepath.Join("src", "core.hpp"))
	default:
		return filepath.ToSlash(filepath.Join("internal", "core", "types.go"))
	}
}

func universalStoreFileForAdapter(adapter planningLanguageAdapter) string {
	switch adapter.ID {
	case "python":
		return filepath.ToSlash(filepath.Join("src", "store.py"))
	case "rust":
		return filepath.ToSlash(filepath.Join("src", "store.rs"))
	case "typescript":
		return filepath.ToSlash(filepath.Join("src", "store.ts"))
	case "javascript":
		return filepath.ToSlash(filepath.Join("src", "store.js"))
	case "cpp":
		return filepath.ToSlash(filepath.Join("src", "store.cpp"))
	default:
		return filepath.ToSlash(filepath.Join("internal", "store", "store.go"))
	}
}

func universalSearchFileForAdapter(adapter planningLanguageAdapter) string {
	switch adapter.ID {
	case "python":
		return filepath.ToSlash(filepath.Join("src", "search.py"))
	case "rust":
		return filepath.ToSlash(filepath.Join("src", "search.rs"))
	case "typescript":
		return filepath.ToSlash(filepath.Join("src", "search.ts"))
	case "javascript":
		return filepath.ToSlash(filepath.Join("src", "search.js"))
	case "cpp":
		return filepath.ToSlash(filepath.Join("src", "search.cpp"))
	default:
		return filepath.ToSlash(filepath.Join("internal", "search", "search.go"))
	}
}

func universalTestFileForAdapter(adapter planningLanguageAdapter) string {
	switch adapter.ID {
	case "python":
		return filepath.ToSlash(filepath.Join("tests", "test_core.py"))
	case "rust":
		return filepath.ToSlash(filepath.Join("tests", "core_test.rs"))
	case "typescript":
		return filepath.ToSlash(filepath.Join("src", "core.test.ts"))
	case "javascript":
		return filepath.ToSlash(filepath.Join("src", "core.test.js"))
	case "cpp":
		return filepath.ToSlash(filepath.Join("tests", "core_test.cpp"))
	default:
		return "agentdb_test.go"
	}
}

func universalTestAcceptanceForAdapter(adapter planningLanguageAdapter) string {
	switch adapter.ID {
	case "go":
		return "补覆盖创建、写入、读取、检索的最小本地测试; 测试文件放在项目根目录并使用外部测试包, 通过 import internal 子包验证, 禁止放入 internal/core 造成循环导入; 本地 go test ./... 通过"
	default:
		return "补覆盖创建、写入、读取、检索的最小本地测试; 本地 build/test 通过"
	}
}

func universalVerifyCommandForAdapter(root string, adapter planningLanguageAdapter) string {
	switch adapter.ID {
	case "python":
		return "cd " + root + " && python -m py_compile $(find . -name '*.py')"
	case "rust":
		return "cd " + root + " && cargo test"
	case "typescript", "javascript":
		return "cd " + root + " && npm test"
	case "cpp":
		return "cd " + root + " && cmake -S . -B build && cmake --build build"
	default:
		return "cd " + root + " && go test ./..."
	}
}

func inferPlanningLanguageID(objective, explicit string) string {
	explicit = strings.ToLower(strings.TrimSpace(explicit))
	switch explicit {
	case "go", "golang":
		return "go"
	case "python", "py":
		return "python"
	case "rust", "rs":
		return "rust"
	case "typescript", "ts":
		return "typescript"
	case "javascript", "js", "node":
		return "javascript"
	case "cpp", "c++", "cc":
		return "cpp"
	}

	lower := strings.ToLower(objective)
	switch {
	case strings.Contains(lower, "golang") || strings.Contains(lower, "go ") ||
		strings.Contains(lower, "go项目") || strings.Contains(lower, "go 项目") ||
		strings.Contains(lower, "go.mod"):
		return "go"
	case strings.Contains(lower, "python") || strings.Contains(lower, "pytest") || strings.Contains(lower, "pyproject"):
		return "python"
	case strings.Contains(lower, "rust") || strings.Contains(lower, "cargo.toml") || strings.Contains(lower, "cargo test"):
		return "rust"
	case strings.Contains(lower, "typescript") || strings.Contains(lower, "ts项目") || strings.Contains(lower, "ts 项目"):
		return "typescript"
	case strings.Contains(lower, "javascript") || strings.Contains(lower, "node.js") || strings.Contains(lower, "nodejs"):
		return "javascript"
	case strings.Contains(lower, "c++") || strings.Contains(lower, "cpp") || strings.Contains(lower, "cmake"):
		return "cpp"
	default:
		return ""
	}
}

func planningLanguageAdapterFor(language string) planningLanguageAdapter {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "python", "py":
		return planningLanguageAdapter{
			ID: "python", Display: "Python", SourceExt: ".py", TestSuffix: "_test.py",
			ManifestFile: "pyproject.toml", VerifyCommand: "pytest",
			DirectorySpecs: []planningDirectoryLeafSpec{
				{Suffix: "__init__.py", Title: "包边界与公开 API", Acceptance: "定义该目录 package 边界、公开类型和最小导出; Python 语法检查通过", Minutes: 2, WorkUnitType: wbsWorkUnitContract, Role: "coder"},
				{Suffix: "{stem}.py", Title: "核心行为最小实现", Acceptance: "实现该目录一个核心行为闭环; 不扩展其它目录; Python 语法检查通过", Minutes: 3, WorkUnitType: wbsWorkUnitImplementation, Role: "coder"},
				{Suffix: "test_{stem}.py", Title: "本地单元测试", Acceptance: "为该目录核心行为补最小 pytest 用例; 本地测试通过", Minutes: 2, WorkUnitType: wbsWorkUnitVerification, Role: "tester", IsTest: true},
			},
		}
	case "rust", "rs":
		return planningLanguageAdapter{
			ID: "rust", Display: "Rust", SourceExt: ".rs", TestSuffix: "_test.rs",
			ManifestFile: "Cargo.toml", VerifyCommand: "cargo test",
			DirectorySpecs: []planningDirectoryLeafSpec{
				{Suffix: "mod.rs", Title: "模块边界与公开类型", Acceptance: "定义该模块公开类型、trait 和错误边界; cargo check 通过", Minutes: 2, WorkUnitType: wbsWorkUnitContract, Role: "coder"},
				{Suffix: "{stem}.rs", Title: "核心行为最小实现", Acceptance: "实现该模块一个核心行为闭环; cargo check 通过", Minutes: 3, WorkUnitType: wbsWorkUnitImplementation, Role: "coder"},
				{Suffix: "{stem}_test.rs", Title: "本地单元测试", Acceptance: "为该模块核心行为补最小 Rust 测试; cargo test 通过", Minutes: 2, WorkUnitType: wbsWorkUnitVerification, Role: "tester", IsTest: true},
			},
		}
	case "typescript", "ts":
		return planningLanguageAdapter{
			ID: "typescript", Display: "TypeScript", SourceExt: ".ts", TestSuffix: ".test.ts",
			ManifestFile: "package.json", VerifyCommand: "npm test",
			DirectorySpecs: []planningDirectoryLeafSpec{
				{Suffix: "types.ts", Title: "类型与接口边界", Acceptance: "定义该目录类型、接口和错误边界; TypeScript 语义保持一致", Minutes: 2, WorkUnitType: wbsWorkUnitContract, Role: "coder"},
				{Suffix: "{stem}.ts", Title: "核心行为最小实现", Acceptance: "实现该目录一个核心行为闭环; 不扩展其它目录", Minutes: 3, WorkUnitType: wbsWorkUnitImplementation, Role: "coder"},
				{Suffix: "{stem}.test.ts", Title: "本地单元测试", Acceptance: "为该目录核心行为补最小测试; npm test 通过或测试命令明确不可用", Minutes: 2, WorkUnitType: wbsWorkUnitVerification, Role: "tester", IsTest: true},
			},
		}
	case "javascript", "js", "node":
		return planningLanguageAdapter{
			ID: "javascript", Display: "JavaScript", SourceExt: ".js", TestSuffix: ".test.js",
			ManifestFile: "package.json", VerifyCommand: "npm test",
			DirectorySpecs: []planningDirectoryLeafSpec{
				{Suffix: "index.js", Title: "模块边界与导出", Acceptance: "定义该目录模块边界、公开导出和错误边界; Node 语法检查通过", Minutes: 2, WorkUnitType: wbsWorkUnitContract, Role: "coder"},
				{Suffix: "{stem}.js", Title: "核心行为最小实现", Acceptance: "实现该目录一个核心行为闭环; 不扩展其它目录", Minutes: 3, WorkUnitType: wbsWorkUnitImplementation, Role: "coder"},
				{Suffix: "{stem}.test.js", Title: "本地单元测试", Acceptance: "为该目录核心行为补最小测试; npm test 通过或测试命令明确不可用", Minutes: 2, WorkUnitType: wbsWorkUnitVerification, Role: "tester", IsTest: true},
			},
		}
	case "cpp", "c++", "cc":
		return planningLanguageAdapter{
			ID: "cpp", Display: "C++", SourceExt: ".cpp", TestSuffix: "_test.cpp",
			ManifestFile: "CMakeLists.txt", VerifyCommand: "cmake --build build && ctest --test-dir build",
			DirectorySpecs: []planningDirectoryLeafSpec{
				{Suffix: "{stem}.hpp", Title: "头文件接口边界", Acceptance: "定义该目录公开类型、接口和错误边界; 不实现跨模块逻辑", Minutes: 2, WorkUnitType: wbsWorkUnitContract, Role: "coder"},
				{Suffix: "{stem}.cpp", Title: "核心行为最小实现", Acceptance: "实现该目录一个核心行为闭环; 不扩展其它目录", Minutes: 3, WorkUnitType: wbsWorkUnitImplementation, Role: "coder"},
				{Suffix: "{stem}_test.cpp", Title: "本地单元测试", Acceptance: "为该目录核心行为补最小测试; 本地构建/测试通过", Minutes: 2, WorkUnitType: wbsWorkUnitVerification, Role: "tester", IsTest: true},
			},
		}
	default:
		return planningLanguageAdapter{
			ID: "go", Display: "Go", SourceExt: ".go", TestSuffix: "_test.go",
			ManifestFile: "go.mod", VerifyCommand: "go test ./...",
			DirectorySpecs: []planningDirectoryLeafSpec{
				{Suffix: "types.go", Title: "接口与类型边界", Acceptance: "只定义该目录的公开类型、接口、错误和最小构造函数; scoped build 通过", Minutes: 2, WorkUnitType: wbsWorkUnitContract, Role: "coder"},
				{Suffix: "{stem}.go", Title: "核心行为最小实现", Acceptance: "实现该目录一个核心行为闭环; 不扩展其它目录; scoped build 通过", Minutes: 3, WorkUnitType: wbsWorkUnitImplementation, Role: "coder"},
				{Suffix: "{stem}_test.go", Title: "本地单元测试", Acceptance: "为该目录核心行为补最小单元测试; scoped build/test 通过", Minutes: 2, WorkUnitType: wbsWorkUnitVerification, Role: "tester", IsTest: true},
			},
		}
	}
}

func (a planningLanguageAdapter) renderDirectoryLeafFile(dir, stem string, spec planningDirectoryLeafSpec) string {
	suffix := strings.ReplaceAll(spec.Suffix, "{stem}", stem)
	return filepath.ToSlash(filepath.Join(dir, suffix))
}

func planningErrorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func classifyAgentTimeout(err error, node *TaskNode, promptChars int, elapsed time.Duration) timeoutClassification {
	lowerErr := strings.ToLower(planningErrorText(err))
	if strings.Contains(lowerErr, "rate limit") || strings.Contains(lowerErr, "429") ||
		strings.Contains(lowerErr, "overloaded") || strings.Contains(lowerErr, "503") ||
		strings.Contains(lowerErr, "quota") {
		return timeoutClassification{Kind: wbsTimeoutKindRateLimit, Action: wbsTimeoutActionRetryOrFail, Reason: "provider returned rate/overload signal", AllowSplit: false}
	}
	if promptChars > 160000 {
		return timeoutClassification{Kind: wbsTimeoutKindPromptBloat, Action: wbsTimeoutActionRetryOrFail, Reason: "prompt exceeds prompt-bloat threshold", AllowSplit: false}
	}
	if node == nil {
		return timeoutClassification{Kind: wbsTimeoutKindUnknown, Action: wbsTimeoutActionRetryOrFail, Reason: "missing task node", AllowSplit: false}
	}
	writeFiles := len(node.WriteFiles)
	if writeFiles == 0 {
		writeFiles = len(node.TargetFiles)
	}
	if node.EstimatedMin > 4 || node.RiskLevel == wbsRiskHigh || writeFiles > 3 || node.EstimatedLOC > 250 {
		return timeoutClassification{Kind: wbsTimeoutKindTrueOversize, Action: wbsTimeoutActionSplit, Reason: "leaf exceeds declared readiness budget", AllowSplit: true}
	}
	if node.TaskType == wbsTaskTypeLeaf && node.EstimatedMin > 0 && node.EstimatedMin <= 4 && elapsed >= coderCallTimeout {
		return timeoutClassification{Kind: wbsTimeoutKindProviderStall, Action: wbsTimeoutActionRetryOrFail, Reason: "small ready leaf timed out, likely provider/tool stall rather than granularity", AllowSplit: false}
	}
	return timeoutClassification{Kind: wbsTimeoutKindUnknown, Action: wbsTimeoutActionRetryOrFail, Reason: "timeout cause is ambiguous", AllowSplit: false}
}
