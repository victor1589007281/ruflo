package agent

import (
	"fmt"
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

// normalizePlanningID 把任意标识符归一为小写、仅含 [a-z0-9._:-] 的 ID, 其余字符折成连字符。
func normalizePlanningID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	re := regexp.MustCompile(`[^a-z0-9._:-]+`)
	s = re.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// uniqueTrimmedStrings 去首尾空白、丢空串、保序去重。
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

// normalizePlanFiles 归一 WBS 里声明的文件路径: 展开花括号、转 slash、Clean; 越界 (绝对/上跳) 路径原样保留。
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

// expandPlanBracePath 展开单个花括号的路径 (如 src/{a,b}.go); 无花括号或括号体不合法时原样返回。
//
// 算法解读：
//
//	原理：WBS 里声明的常常是"一组文件", 而落盘与冲突检测需要逐个具体路径, 这一步负责把
//	      声明展开成路径集合。
//	关键不变量：只展开**第一个**花括号, 且括号体内不得含 `/`、`\` 或嵌套花括号 —— 任一
//	      条不满足就原样返回。宁可少展开也不猜: 把 `{a/b,c}` 展开成带目录的路径会让写入
//	      落到从未声明过的目录里。
//	代价与边界：不递归, 多个花括号只展开最左边那个; 展开为空时回退原串, 保证"输入非空则
//	      输出非空"。
//	设计权衡：这是 glob 的十几行子集, 换来不引依赖且行为完全可预测 —— 路径展开的意外行为
//	      会直接变成"文件写错地方"。
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

// rawTaskWriteFiles 返回任务实际会写的文件: writeFiles 优先, 为空则回退 targetFiles。
func rawTaskWriteFiles(rt rawTask) []string {
	if len(rt.writeFiles) > 0 {
		return rt.writeFiles
	}
	return rt.targetFiles
}

// rawTaskConflictKeys 返回归一 (去空白/去重) 后的冲突键。
func rawTaskConflictKeys(rt rawTask) []string {
	var keys []string
	keys = append(keys, rt.conflictKeys...)
	return uniqueTrimmedStrings(keys)
}

// nonGlobalRawTaskConflictKeys 过滤掉 ":global" 结尾的冲突键 —— 全局键不参与文件级互斥。
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

// estimateRawTaskLOC 粗估任务的改动规模 (行数): 先按任务类型定档, 再按文件数与复杂度加成。
//
// 算法解读：
//
//	原理：在真正写代码之前给出改动规模的量级估计, 供排期与"是否该拆"参考。
//	关键不变量：verification 类型恒返回 20 行 —— 验证任务的成本与它验证的代码量无关;
//	      若跟着文件数放大, 终验任务会被系统性误判成超载而反复被拆。
//	代价与边界：这是**量级**估计 (60/90/140 行每文件), 不是预测; 文件数为 0 时按单文件
//	      下限取值, 避免估出 0。
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

// synthesizedObjectiveWBS 在规划器产出不可用 (校验耗尽) 时, 用 objective 兜底合成一份通用 WBS。
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

// synthesizedUniversalV1WBS 生成语言无关的兜底 WBS (8 个节点): 骨架 → 入口 → 契约 → 存储构造 →
// 存储 CRUD → 检索 → 本地用例 → 终验。
//
// 算法解读：
//
//	原理：规划器校验耗尽时不能把任务判死。这里用一份与目标语言无关的最小可编译骨架接管,
//	      让"造一个 X"这类空泛 objective 至少产出一个能 build 的项目。
//	关键不变量：每个叶子只写一个文件、只依赖前一个叶子的产出 (depNums 构成一条链), 且每个
//	      叶子的 accept 都写明"不做什么"。粒度一旦越界 (一个叶子同时改存储与检索), 失败
//	      就无法归因到具体一步, 拆分也就失去了意义。
//	代价与边界：产出的是通用骨架而非贴合 objective 的设计, 只保证可编译可测; 因此每个节点
//	      的 splitReason 一律标 objective-fallback:planner-validation-exhausted, 让下游能把
//	      它与真实规划区分开。
//	设计权衡：宁愿给一条"窄但一定能跑通"的链, 也不给一张"宽但可能互相冲突"的图 ——
//	      并行的存储/检索叶子在无人协调时极易写出重复接口 (见 accept 里的禁令)。
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

// universalMainFileForAdapter 返回该语言的项目入口文件 (Python/Rust/TS/JS/C++ 各一条, 默认 Go)。
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

// universalCoreFileForAdapter 返回该语言的核心契约文件路径 (默认 internal/core/types.go)。
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

// universalStoreFileForAdapter 返回该语言的存储实现文件路径 (默认 internal/store/store.go)。
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

// universalSearchFileForAdapter 返回该语言的检索实现文件路径 (默认 internal/search/search.go)。
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

// universalTestFileForAdapter 返回该语言的测试文件路径 (Go 用根目录外部测试包, 规避循环导入)。
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

// universalTestAcceptanceForAdapter 返回该语言测试叶子的验收措辞 (Go 额外禁止把测试放进 internal/core)。
func universalTestAcceptanceForAdapter(adapter planningLanguageAdapter) string {
	switch adapter.ID {
	case "go":
		return "补覆盖创建、写入、读取、检索的最小本地测试; 测试文件放在项目根目录并使用外部测试包, 通过 import internal 子包验证, 禁止放入 internal/core 造成循环导入; 本地 go test ./... 通过"
	default:
		return "补覆盖创建、写入、读取、检索的最小本地测试; 本地 build/test 通过"
	}
}

// universalVerifyCommandForAdapter 返回该语言的本地验证命令 (在 root 下执行)。
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

// inferPlanningLanguageID 推断目标语言: 显式声明优先, 否则从 objective 里认关键词; 认不出返回空串。
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

// planningLanguageAdapterFor 返回某语言的规划适配器: 清单文件、测试后缀、验证命令与目录叶子规格。
// 未知语言回退 Go。
//
// 每个目录固定产出三个叶子 (契约 → 实现 → 测试), 这是 WBS 粒度的最小可归因单元:
// 契约叶子只定义类型与错误边界, 实现叶子只做一个行为闭环, 测试叶子只补最小用例。
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

// renderDirectoryLeafFile 把目录叶子规格渲染成具体文件路径 ({stem} 替换为目录名/文件主名)。
func (a planningLanguageAdapter) renderDirectoryLeafFile(dir, stem string, spec planningDirectoryLeafSpec) string {
	suffix := strings.ReplaceAll(spec.Suffix, "{stem}", stem)
	return filepath.ToSlash(filepath.Join(dir, suffix))
}

// planningErrorText 取错误文本; err 为 nil 时返回空串。
func planningErrorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// cognitiveLoadScore 计算任务的动态认知负载分 (0~10+).
// 综合考虑文件耦合度、任务类型、逻辑密度关键词、风险等级等。
func cognitiveLoadScore(node *TaskNode) float64 {
	if node == nil {
		return 0
	}
	load := 0.0

	// 文件数基础负载 (读比写轻量)
	writeFiles := len(node.WriteFiles)
	if writeFiles == 0 {
		writeFiles = len(node.TargetFiles)
	}
	load += float64(writeFiles) * 0.4
	load += float64(len(node.ReadFiles)) * 0.25

	// 任务类型权重: contract/verification 密度低, implementation 密度中等
	switch node.WorkUnitType {
	case wbsWorkUnitContract:
		load += 0.5
	case wbsWorkUnitImplementation:
		load += 1.0
	case wbsWorkUnitVerification:
		load += 0.3
	default:
		// WorkUnitType 未明确时, coder 角色默认按 implementation 密度估算
		if strings.Contains(strings.ToLower(node.Role), "coder") {
			load += 1.0
		}
	}

	// 逻辑密度关键词检测 (高认知负载任务)
	text := strings.ToLower(node.Title + " " + node.AcceptCriteria)
	highDensityTerms := []string{
		"编码", "序列化", "协议", "索引", "crc", "wal", "sstable",
		"并发", "锁", "事务", "mvcc", "一致性", "调度", "状态机",
		"编译器", "解析器", "lexer", "parser", "ast",
		"权限", "安全边界", "加密", "签名",
		"二进制", "wire format", "endian", "checksum",
		// 扩充: 存储/引擎/查询/执行/调度等核心系统组件
		"存储", "引擎", "计划器", "执行器", "内核", "管理器",
		"数据结构", "接口", "模块", "系统", "框架", "服务",
		"编解码", "压缩", "哈希", "内存", "缓存", "gc",
		"网络", "数据库", "db", "类型系统", "编译", "反序列化",
		"goroutine", "channel", "context", "allocator", "arena", "mmap",
		"rpc", "grpc", "http", "mq", "消息队列", "event", "pubsub",
		"fsm", "pipeline", "workflow", "orchestrator", "middleware",
		"gateway", "proxy", "负载均衡", "路由", "分片", "shard",
		"复制", "replica", "同步", "备份", "恢复", "迁移",
		"优化", "profile", "benchmark", "调优",
		"embedding", "vector", "hnsw", "ann", "cluster",
		"model", "train", "inference", "attention", "transformer", "llm",
	}
	for _, term := range highDensityTerms {
		if strings.Contains(text, term) {
			load += 1.5
			break // 只加一次，避免同一类密度反复累加
		}
	}

	// 标题语义密度兜底: 当结构化元数据稀疏时，从标题中提取技术概念密度
	load += titleSemanticLoad(node.Title)

	// coder leaf 保底负载: 即使 WBS 未声明目标文件，agent 执行时仍会自行推断并生成代码
	if writeFiles == 0 && len(node.TargetFiles) == 0 &&
		node.TaskType == wbsTaskTypeLeaf && strings.Contains(strings.ToLower(node.Role), "coder") {
		load += 2.5
	}

	// 子目标数
	load += float64(len(node.SubGoals)) * 0.35

	// 风险等级
	switch node.RiskLevel {
	case wbsRiskHigh:
		load += 2.5
	case wbsRiskMedium:
		load += 0.8
	}

	// 预估时间偏差
	if node.EstimatedMin > 4 {
		load += 1.5
	}

	return load
}

// titleSemanticLoad 从标题中提取技术概念密度 (0~4.0)。
// 当 WBS 未提供结构化元数据时，用标题语义作为负载兜底信号。
func titleSemanticLoad(title string) float64 {
	if title == "" {
		return 0
	}
	text := strings.ToLower(title)

	// 核心技术概念 (每个匹配 +0.8)
	coreTerms := []string{
		"存储", "引擎", "计划器", "执行器", "调度器", "管理器", "内核",
		"数据结构", "算法", "协议", "接口", "模块", "系统", "框架",
		"编解码", "序列化", "压缩", "加密", "哈希", "索引", "缓存",
		"内存", "gc", "垃圾回收", "allocator", "arena", "mmap",
		"并发", "锁", "事务", "mvcc", "一致性", "共识", "raft",
		"网络", "数据库", "db", "sql", "nosql", "rpc", "grpc", "http",
		"消息队列", "mq", "event", "pubsub", "watcher", "observer",
		"fsm", "pipeline", "workflow", "orchestrator", "middleware",
		"gateway", "proxy", "负载均衡", "路由", "分片", "shard",
		"复制", "replica", "同步", "备份", "恢复", "迁移",
		"优化", "profile", "benchmark", "调优",
		"编译", "解析", "lexer", "parser", "ast", "类型系统",
		"b+", "btree", "skiplist", "hashtable", "bloom", "bitmap",
		"lsm", "wal", "sstable", "日志", "checkpoint",
		"embedding", "vector", "hnsw", "ann", "cluster",
		"model", "train", "inference", "attention", "transformer", "llm",
	}

	// 实现动作词 (每个匹配 +0.5)
	actionTerms := []string{
		"实现", "设计", "重构", "优化", "集成", "适配",
		"编码", "开发", "构建", "组装", "封装", "抽象",
	}

	score := 0.0
	for _, term := range coreTerms {
		if strings.Contains(text, term) {
			score += 0.8
		}
	}
	for _, term := range actionTerms {
		if strings.Contains(text, term) {
			score += 0.5
		}
	}
	if score > 4.0 {
		score = 4.0
	}
	return score
}

// classifyAgentTimeout 把一次 agent 超时归因: 限流 / prompt 膨胀 / 真·超载 / provider 停摆 / 未知。
//
// 算法解读：
//
//	原理：超时不等于"任务太大"。先排除两个与任务规模无关的成因 (provider 限流、prompt 本身
//	      超长), 再用认知负载评分决定"该拆"还是"该重试"。
//	关键不变量：限流与 prompt 膨胀**先于**负载评分判定, 且都禁止拆分 —— 这两类超时重试就好,
//	      拆任务只会把一个非任务问题变成两个。
//	代价与边界：负载分是启发式 (关键词 + 文件数 + 风险等级), 不是测量值; 6~8 分是"允许拆"
//	      的灰区, 判错的方向是拆得过细而不是漏拆。
//	设计权衡：把 defaultCoderCallTimeout 写死在这里, 是为了让"小负载叶子却超时"这条唯一能
//	      指向 provider 停摆的证据不被配置漂移带走。
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

	load := cognitiveLoadScore(node)
	// 阈值是**连续评分**而不是二值: 6~8 分允许拆 (拆了大概率更顺), >8 分明确该拆。

	// 连续评分: >8 分明确拆分, 6~8 分允许拆分, <6 分倾向 provider stall
	if load > 8.0 {
		return timeoutClassification{Kind: wbsTimeoutKindTrueOversize, Action: wbsTimeoutActionSplit, Reason: fmt.Sprintf("cognitive load %.1f exceeds high threshold", load), AllowSplit: true}
	}
	if load >= 6.0 {
		return timeoutClassification{Kind: wbsTimeoutKindTrueOversize, Action: wbsTimeoutActionSplit, Reason: fmt.Sprintf("cognitive load %.1f exceeds medium threshold", load), AllowSplit: true}
	}

	// 小负载任务超时 → provider stall 可能性大
	const defaultCoderCallTimeout = 10 * time.Minute
	if node.TaskType == wbsTaskTypeLeaf && elapsed >= defaultCoderCallTimeout {
		return timeoutClassification{Kind: wbsTimeoutKindProviderStall, Action: wbsTimeoutActionRetryOrFail, Reason: fmt.Sprintf("small load (%.1f) leaf timed out, likely provider/tool stall", load), AllowSplit: false}
	}

	return timeoutClassification{Kind: wbsTimeoutKindUnknown, Action: wbsTimeoutActionRetryOrFail, Reason: fmt.Sprintf("timeout cause ambiguous (load %.1f)", load), AllowSplit: false}
}
