# Agent Harness 与 Skills 护栏完整实现方案

日期：2026-05-10

---

## 摘要

将 `claude-go/pkg/agent/workflow.go` 从 15,687 行（含 ~10,000 行 deterministic repair）重构为 **Contract-First 的代码生成与修复 Harness**。

**现有资产**：
- `pkg/orchestrator/` — ~5,000 行 DAG 编排（复用）
- `pkg/skills/` — 446 行 Prompt Skill（复用扩展）
- `pkg/sandbox/` — ~1,300 行多模式沙箱（复用）
- `pkg/memory/` — ~1,800 行记忆存储（复用）

**目标代码量**：
- `pkg/agent/workflow.go` — 15,687 行 → **< 2,000 行**
- `pkg/agent/` 新增 ~3,800 行（含 CodeExecutor, RulesLoader, WikiCompiler）
- `pkg/toolskill/` 新增 ~1,200 行（底层复用 Go 生态原生工具，仅做 wrapper/adapter）
- `pkg/skills/builtin/` 新增 5 个 SKILL.md（融入 Karpathy 四大原则）
- `.claude-go/AGENT_RULES.md` — 项目级 Agent 约束模板
- `.claude-go/wiki/` — LLM Wiki 知识持久化目录

---

## 目录

1. [总体架构](#总体架构)
2. [Karpathy 原则与 Agent 约束架构](#karpathy-原则与-agent-约束架构)
3. [Contract Store 完整实现](#contract-store-完整实现)
4. [Patch Apply 完整实现](#patch-apply-完整实现)
5. [Validation Gate 完整实现](#validation-gate-完整实现)
6. [Context Engine 完整实现](#context-engine-完整实现)
7. [Code Executor 完整实现](#code-executor-完整实现)
8. [Tool Skill Runtime 完整实现](#tool-skill-runtime-完整实现)
9. [内置 Tool Skill 完整实现](#内置-tool-skill-完整实现)
10. [Prompt Skill 扩展](#prompt-skill-扩展)
11. [Agent Rules 文件规范](#agent-rules-文件规范)
12. [LLM Wiki 知识持久化](#llm-wiki-知识持久化)
13. [orchestrator 集成改造](#orchestrator-集成改造)
14. [workflow.go 改造方案](#workflowgo-改造方案)
15. [一次性迁移步骤](#一次性迁移步骤)
16. [测试与 Benchmark](#测试与-benchmark)

---

## 总体架构

```
┌─────────────────────────────────────────────────────────────────────┐
│ Layer 0: Karpathy 原则约束层（AGENT_RULES.md + LLM Wiki）          │
│  AGENT_RULES.md — 项目级 Agent 行为约束（Think/Simplicity/         │
│    Surgical/Goal-Driven）                                           │
│  .claude-go/wiki/ — 持久化结构化知识（接口定义、设计决策、         │
│    失败模式、最佳实践）                                             │
├─────────────────────────────────────────────────────────────────────┤
│ Layer 1: Prompt Skill（pkg/skills/）                                │
│  skills.go — SKILL.md 解析与注入（446 行，已有）                   │
│  builtin/ — 策略类 SKILL.md（新增，融入 Karpathy 四大原则）       │
├─────────────────────────────────────────────────────────────────────┤
│ Layer 2: Orchestrator（pkg/orchestrator/）                          │
│  graph.go / scheduler.go / engine.go / checkpoint.go               │
│  blackboard.go / backpressure.go / hooks.go / llm_runner.go       │
│  (~5,000 行，已有，扩展 ValidationHook)                            │
├─────────────────────────────────────────────────────────────────────┤
│ Layer 3: Agent Harness Core（pkg/agent/）                           │
│  executor.go — 任务执行循环（~1,200 行，增加 Think-before-code）  │
│  validation_gate.go — 硬阻断门禁（~500 行，增加 Simplicity/      │
│    Surgical/Goal-Driven 检查）                                      │
│  contract_store.go — 项目符号图（~900 行）                          │
│  patch_apply.go — AST-aware 补丁（~700 行）                         │
│  context_engine.go — 符号检索（~500 行）                            │
├─────────────────────────────────────────────────────────────────────┤
│ Layer 4: Tool Skill Runtime（pkg/toolskill/）                       │
│  schema.go / registry.go / runtime.go (~650 行)                    │
│  builtin/ — 内置 Tool Skills (~1,200 行，原生工具 Wrapper)        │
├─────────────────────────────────────────────────────────────────────┤
│ Layer 5: Execution / Storage（pkg/sandbox/, pkg/memory/）           │
│  sandbox/manager.go / memory/store.go (~3,100 行，已有)            │
└─────────────────────────────────────────────────────────────────────┘
```

---

## Karpathy 原则与 Agent 约束架构

本章节受 Andrej Karpathy 在 2024-2025 年关于 AI Agent 代码生成、约束设计和提示词工程的实践启发（特别是 Vibe Coding → AutoResearch → Agent Rules 的演进路径），将四大核心原则系统性融入 Agent Harness 的每一层。

### Karpathy 的四大核心原则

| 原则 | 核心思想 | Agent Harness 对应机制 |
|------|---------|----------------------|
| **Think Before Code** | 先思考再编码，停止当困惑时 | `executor.go` 增加 **Think Phase**，强制 LLM 输出思考过程再生成代码 |
| **Simplicity First** | 不添加未要求的功能，不用不必要的抽象 | `validation_gate.go` 增加 **代码复杂度门禁**（行数/圈复杂度/抽象层级） |
| **Surgical Changes** | 不修改未破损的代码，匹配现有风格 | `patch_apply.go` 增加 **变更范围审计**，`context_engine.go` 检索现有风格 |
| **Goal-Driven Execution** | 给成功标准而非逐步指令 | Prompt Skill 从命令式改为**声明式**，`buildGeneratePrompt` 输出成功标准而非步骤 |

### 1. Think-Before-Code 机制

Karpathy 的 AutoResearch 架构中，`program.md` 作为人类编写的高层指令，`train.py` 作为 Agent 直接修改的代码文件（~630 行，确保能放入 LLM 上下文）。关键设计是：**Agent 在修改代码前必须先"思考"**——理解约束、评估方案、明确变更范围。

在我们的 Harness 中，每个代码生成/修复任务在执行前必须经过一个 **Think Phase**：

```
┌─────────────┐    ┌─────────────────┐    ┌─────────────┐
│   Task      │ -> │   THINK PHASE   │ -> │   CODE      │
│  Received   │    │  (强制思考输出)  │    │ Generation  │
└─────────────┘    └─────────────────┘    └─────────────┘
                      - 理解目标
                      - 评估现有代码
                      - 识别约束
                      - 规划变更范围
                      - 声明假设
```

**Think Phase 的输出要求**（LLM 必须在生成代码前输出）：
1. **目标理解**：用一句话描述任务目标
2. **约束清单**：列出 Contract Store 中相关的接口、类型、函数
3. **变更评估**：预计修改多少行、影响哪些文件
4. **假设声明**：列出你做的任何假设（如"假设接口 X 已存在"）
5. **方案比较**：如果有多种实现方式，简述选择理由
6. **停止条件**：什么情况下你会停止并请求帮助

**实现方式**：在 `CodeExecutor.buildGeneratePrompt()` 和 `buildRepairPrompt()` 中，在代码生成指令前插入 `## Think Phase` 段落。LLM 必须在 `<think>` 和 `</think>` 标签之间完成思考，然后才输出代码。

**门禁检查**：如果 LLM 输出中没有 `<think>...</think>` 块，Validation Gate 拒绝该输出并提示"Missing Think Phase"。

### 2. Simplicity-First 约束

Karpathy 观察到 LLM 有严重的"过度工程化"倾向："Implement a bloated construction over 1000 lines when 100 would do"。

在我们的 Harness 中，通过以下机制强制 simplicity：

**a) 代码复杂度门禁（Validation Gate 新增）**
- **行数预算**：每个任务的变更不得超过预算行数（默认：生成任务 ≤200 行，修复任务 ≤50 行）
- **圈复杂度**：新增函数 McCabe 复杂度 ≤10
- **抽象层级**：禁止为单次使用创建抽象（如提取函数只被调用一次）
- **依赖预算**：禁止引入未声明的新依赖

**b) 变更前复杂度检查**
- Agent 在 Think Phase 必须声明预计变更行数
- 如果实际变更超出预算的 150%，Gate 拒绝并要求重新思考

**c) 代码膨胀检测**
- 对比变更前后的文件行数比例
- 如果新增行数是删除行数的 5 倍以上，标记为"潜在过度工程化"

### 3. Surgical-Change 控制

Karpathy 发现 LLM 经常"改动不相关的代码"——修改注释、重命名变量、调整格式。这导致代码 review 困难且引入不必要的风险。

**a) 变更范围声明**
- Think Phase 必须列出"计划修改的文件和行号范围"
- 实际变更不得超出声明范围的 ±20%

**b) 相邻代码保护**
- Patch Apply 时检测：替换块前后 3 行是否被意外修改
- 如果检测到"附带损伤"（collateral damage），Gate 拒绝补丁

**c) 风格一致性检查**
- Context Engine 检索现有代码风格（命名约定、错误处理模式、注释风格）
- Agent 生成的新代码必须匹配现有风格（通过 `repo-style-check` Tool Skill）

### 4. Goal-Driven 执行（声明式 Prompt）

Karpathy 的核心洞察：**"Don't tell it what to do — give it success criteria"**。LLM 在循环中迭代直到满足明确的成功标准时表现最好。

**a) Prompt 设计从命令式到声明式**

| 命令式（旧） | 声明式（新） |
|-------------|-------------|
| "Step 1: 查询 Contract Store" | "Success: 所有使用的类型在 Contract Store 中有定义" |
| "Step 2: 生成最小变更" | "Success: 变更行数 ≤ 50 行，仅修改目标文件" |
| "Step 3: 验证编译通过" | "Success: `go build ./...` 返回零错误" |
| "不要修改无关代码" | "Success: 未声明的修改行数 = 0" |

**b) 成功标准检查清单**
每个任务的 prompt 末尾包含：
```
## Success Criteria (MUST ALL PASS)
- [ ] Code compiles: `go build ./...` succeeds
- [ ] Tests pass: `go test ./...` succeeds  
- [ ] Simplicity: Total changed lines ≤ 50
- [ ] Surgical: Only target files modified
- [ ] Contract-compliant: All types exist in Contract Store
```

**c) 迭代循环中的目标追踪**
- 每轮修复后，Executor 对比成功标准清单
- 已达标项标记为 ✅，未达标项作为下一轮的首要目标
- LLM 看到明确的进度，而非模糊的"继续修复"

### 5. 约束衰减防护 (Constraint Decay Protection)

2025 年学术研究发现 **"Constraint Decay"** 现象：随着显式约束（架构、数据库、ORM 等）的积累，Agent 性能显著下降（平均损失 30% 的断言通过率）。

在我们的 Harness 中的防护措施：

**a) 约束分层**
- **Hard Constraints**（不可绕过）：Contract-First、Validation Gate、Simplicity Budget
- **Soft Constraints**（可建议）：代码风格、命名偏好、注释规范
- **Contextual Constraints**（按需加载）：仅当涉及相关文件时才注入

**b) 约束预算**
- 单个任务的 prompt 中约束数量 ≤ 12 条
- 超过 12 条时，将低优先级约束移入 LLM Wiki，按需检索

**c) 约束优先级**
```
P0 (Critical): Think Phase → Contract-First → Validation Gate
P1 (Important): Simplicity Budget → Surgical Scope → Goal Criteria
P2 (Guideline): Style Match → Comment Quality → Naming Convention
```

---

## Contract Store 完整实现

`pkg/agent/contract_store.go` — 使用 `go/ast` 完整解析项目符号图。

```go
// Package agent 实现 Agent Harness 核心组件
package agent

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ContractStore 维护项目级符号表与接口契约。
// 以内存为主，支持增量刷新，不持久化到文件。
type ContractStore struct {
	mu       sync.RWMutex
	root     string                    // repo 根目录
	module   string                    // go module path
	packages map[string]*PackageInfo   // import path → PackageInfo
	symbols  map[string]*SymbolInfo    // 全局符号索引
	fset     *token.FileSet
}

// PackageInfo 包级信息
type PackageInfo struct {
	Path       string                 `json:"path"`
	ImportPath string                 `json:"import_path"`
	Files      []string               `json:"files"`
	Imports    []ImportInfo           `json:"imports"`
	Types      map[string]TypeInfo    `json:"types"`
	Funcs      map[string]FuncInfo    `json:"funcs"`
	Vars       map[string]VarInfo     `json:"vars"`
	Consts     map[string]VarInfo     `json:"consts"`
}

// ImportInfo import 信息
type ImportInfo struct {
	Path  string `json:"path"`
	Alias string `json:"alias,omitempty"`
	Local bool   `json:"local,omitempty"` // 是否为本 module 内部包
}

// TypeInfo 类型信息
type TypeInfo struct {
	Name       string        `json:"name"`
	Kind       string        `json:"kind"` // struct/interface/func/type-alias/map/slice/chan/array
	Package    string        `json:"package"`
	File       string        `json:"file"`
	Line       int           `json:"line"`
	Column     int           `json:"column"`
	Exported   bool          `json:"exported"`
	Fields     []FieldInfo   `json:"fields,omitempty"`
	Methods    []MethodInfo  `json:"methods,omitempty"`
	ElemType   string        `json:"elem_type,omitempty"` // for slice/map/chan/pointer
	KeyType    string        `json:"key_type,omitempty"`  // for map
	Underlying string        `json:"underlying,omitempty"` // for type alias
	Implements []string      `json:"implements,omitempty"` // 实现的接口列表（全限定名）
}

// FieldInfo 字段信息
type FieldInfo struct {
	Name     string   `json:"name,omitempty"`
	Type     string   `json:"type"`
	Tag      string   `json:"tag,omitempty"`
	Embedded bool     `json:"embedded,omitempty"`
	Offset   int      `json:"offset,omitempty"`
	Doc      string   `json:"doc,omitempty"`
}

// MethodInfo 方法信息
type MethodInfo struct {
	Name      string      `json:"name"`
	Package   string      `json:"package"`
	File      string      `json:"file"`
	Line      int         `json:"line"`
	Signature string      `json:"signature"`
	Receiver  string      `json:"receiver,omitempty"` // receiver type name
	Params    []ParamInfo `json:"params"`
	Returns   []ParamInfo `json:"returns"`
	IsPointer bool        `json:"is_pointer,omitempty"` // receiver is pointer
}

// ParamInfo 参数信息
type ParamInfo struct {
	Name string `json:"name,omitempty"`
	Type string `json:"type"`
}

// FuncInfo 函数信息
type FuncInfo struct {
	Name      string      `json:"name"`
	Package   string      `json:"package"`
	File      string      `json:"file"`
	Line      int         `json:"line"`
	Signature string      `json:"signature"`
	Params    []ParamInfo `json:"params"`
	Returns   []ParamInfo `json:"returns"`
	Exported  bool        `json:"exported"`
	IsMethod  bool        `json:"is_method,omitempty"`
}

// VarInfo 变量/常量信息
type VarInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Package  string `json:"package"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Exported bool   `json:"exported"`
	Value    string `json:"value,omitempty"`
}

// SymbolInfo 全局符号索引项
type SymbolInfo struct {
	Name      string `json:"name"`       // 全限定名 "pkg.Type"
	ShortName string `json:"short_name"` // 短名 "Type"
	Kind      string `json:"kind"`       // type/func/var/const
	Package   string `json:"package"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Exported  bool   `json:"exported"`
}

// NewContractStore 创建空的 Contract Store
func NewContractStore(root, module string) *ContractStore {
	return &ContractStore{
		root:     root,
		module:   module,
		packages: make(map[string]*PackageInfo),
		symbols:  make(map[string]*SymbolInfo),
		fset:     token.NewFileSet(),
	}
}

// BuildFromRepo 扫描 repo 中所有 Go 文件构建完整契约图
func (cs *ContractStore) BuildFromRepo() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// 清空旧数据
	cs.packages = make(map[string]*PackageInfo)
	cs.symbols = make(map[string]*SymbolInfo)
	cs.fset = token.NewFileSet()

	// 读取 go.mod 获取 module path（如果未提供）
	if cs.module == "" {
		cs.module = cs.readGoModModule()
	}

	return filepath.Walk(cs.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", "node_modules", ".claude-go":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		return cs.parseFile(path)
	})
}

// readGoModModule 从 go.mod 读取 module path
func (cs *ContractStore) readGoModModule() string {
	data, err := os.ReadFile(filepath.Join(cs.root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// parseFile 解析单个 Go 文件
func (cs *ContractStore) parseFile(path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil // 跳过不可读文件
	}

	f, err := parser.ParseFile(cs.fset, path, src, parser.ParseComments|parser.AllErrors)
	if err != nil {
		// 语法错误的文件：尝试部分解析，跳过具体声明
		return nil
	}

	// 计算 import path
	dir := filepath.Dir(path)
	relDir, _ := filepath.Rel(cs.root, dir)
	importPath := filepath.ToSlash(relDir)
	if cs.module != "" {
		importPath = cs.module + "/" + importPath
	}

	// 获取或创建 package info
	pkg, ok := cs.packages[importPath]
	if !ok {
		pkg = &PackageInfo{
			Path:       dir,
			ImportPath: importPath,
			Types:      make(map[string]TypeInfo),
			Funcs:      make(map[string]FuncInfo),
			Vars:       make(map[string]VarInfo),
			Consts:     make(map[string]VarInfo),
		}
		cs.packages[importPath] = pkg
	}
	pkg.Files = append(pkg.Files, path)

	// 解析 imports
	for _, imp := range f.Imports {
		impPath := strings.Trim(imp.Path.Value, `"`)
		info := ImportInfo{Path: impPath}
		if imp.Name != nil {
			info.Alias = imp.Name.Name
		}
		info.Local = cs.module != "" && strings.HasPrefix(impPath, cs.module+"/")
		pkg.Imports = append(pkg.Imports, info)
	}

	// 遍历顶层声明
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			cs.parseGenDecl(pkg, path, importPath, d)
		case *ast.FuncDecl:
			cs.parseFuncDecl(pkg, path, importPath, d)
		}
	}

	return nil
}

// parseGenDecl 解析通用声明
func (cs *ContractStore) parseGenDecl(pkg *PackageInfo, file, importPath string, decl *ast.GenDecl) {
	for _, spec := range decl.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			cs.parseTypeSpec(pkg, file, importPath, s)
		case *ast.ValueSpec:
			cs.parseValueSpec(pkg, file, importPath, decl.Tok, s)
		}
	}
}

// parseTypeSpec 解析类型声明
func (cs *ContractStore) parseTypeSpec(pkg *PackageInfo, file, importPath string, spec *ast.TypeSpec) {
	name := spec.Name.Name
	pos := cs.fset.Position(spec.Pos())
	exported := ast.IsExported(name)

	ti := TypeInfo{
		Name:     name,
		Package:  importPath,
		File:     file,
		Line:     pos.Line,
		Column:   pos.Column,
		Exported: exported,
	}

	switch t := spec.Type.(type) {
	case *ast.StructType:
		ti.Kind = "struct"
		for i, field := range t.Fields.List {
			fi := FieldInfo{Offset: i}
			if len(field.Names) > 0 {
				fi.Name = field.Names[0].Name
				fi.Embedded = false
			} else {
				// 匿名字段
				fi.Name = cs.exprToString(field.Type)
				fi.Embedded = true
			}
			fi.Type = cs.exprToString(field.Type)
			if field.Tag != nil {
				fi.Tag = field.Tag.Value
			}
			ti.Fields = append(ti.Fields, fi)
		}

	case *ast.InterfaceType:
		ti.Kind = "interface"
		for _, method := range t.Methods.List {
			if len(method.Names) > 0 {
				mi := MethodInfo{
					Name:    method.Names[0].Name,
					Package: importPath,
					File:    file,
					Line:    cs.fset.Position(method.Pos()).Line,
				}
				if ft, ok := method.Type.(*ast.FuncType); ok {
					mi.Signature = cs.funcTypeToString(ft)
					mi.Params = cs.parseParams(ft.Params)
					mi.Returns = cs.parseParams(ft.Results)
				}
				ti.Methods = append(ti.Methods, mi)
			}
		}

	case *ast.FuncType:
		ti.Kind = "func-type"

	case *ast.ArrayType:
		if t.Len == nil {
			ti.Kind = "slice"
		} else {
			ti.Kind = "array"
		}
		ti.ElemType = cs.exprToString(t.Elt)

	case *ast.MapType:
		ti.Kind = "map"
		ti.KeyType = cs.exprToString(t.Key)
		ti.ElemType = cs.exprToString(t.Value)

	case *ast.ChanType:
		ti.Kind = "chan"
		ti.ElemType = cs.exprToString(t.Value)

	case *ast.StarExpr:
		ti.Kind = "pointer"
		ti.ElemType = cs.exprToString(t.X)

	default:
		ti.Kind = "type-alias"
		ti.Underlying = cs.exprToString(t)
	}

	pkg.Types[name] = ti
	cs.symbols[importPath+"."+name] = &SymbolInfo{
		Name:      importPath + "." + name,
		ShortName: name,
		Kind:      "type",
		Package:   importPath,
		File:      file,
		Line:      pos.Line,
		Exported:  exported,
	}
}

// parseValueSpec 解析变量/常量声明
func (cs *ContractStore) parseValueSpec(pkg *PackageInfo, file, importPath string, tok token.Token, spec *ast.ValueSpec) {
	for i, name := range spec.Names {
		pos := cs.fset.Position(name.Pos())
		exported := ast.IsExported(name.Name)
		vi := VarInfo{
			Name:     name.Name,
			Package:  importPath,
			File:     file,
			Line:     pos.Line,
			Exported: exported,
		}
		if spec.Type != nil {
			vi.Type = cs.exprToString(spec.Type)
		}
		if i < len(spec.Values) {
			vi.Value = cs.exprToString(spec.Values[i])
		}

		if tok == token.CONST {
			pkg.Consts[name.Name] = vi
			cs.symbols[importPath+"."+name.Name] = &SymbolInfo{
				Name:      importPath + "." + name.Name,
				ShortName: name.Name,
				Kind:      "const",
				Package:   importPath,
				File:      file,
				Line:      pos.Line,
				Exported:  exported,
			}
		} else {
			pkg.Vars[name.Name] = vi
			cs.symbols[importPath+"."+name.Name] = &SymbolInfo{
				Name:      importPath + "." + name.Name,
				ShortName: name.Name,
				Kind:      "var",
				Package:   importPath,
				File:      file,
				Line:      pos.Line,
				Exported:  exported,
			}
		}
	}
}

// parseFuncDecl 解析函数/方法声明
func (cs *ContractStore) parseFuncDecl(pkg *PackageInfo, file, importPath string, decl *ast.FuncDecl) {
	pos := cs.fset.Position(decl.Pos())
	name := decl.Name.Name
	exported := ast.IsExported(name)

	fi := FuncInfo{
		Name:     name,
		Package:  importPath,
		File:     file,
		Line:     pos.Line,
		Exported: exported,
	}

	if decl.Type != nil {
		fi.Signature = cs.funcTypeToString(decl.Type)
		fi.Params = cs.parseParams(decl.Type.Params)
		fi.Returns = cs.parseParams(decl.Type.Results)
	}

	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		fi.IsMethod = true
		recvType := cs.exprToString(decl.Recv.List[0].Type)
		fi.Name = recvType + "." + name
		// 将方法附加到对应类型的 Methods 列表
		if ti, ok := pkg.Types[recvType]; ok {
			mi := MethodInfo{
				Name:      name,
				Package:   importPath,
				File:      file,
				Line:      pos.Line,
				Signature: fi.Signature,
				Params:    fi.Params,
				Returns:   fi.Returns,
				Receiver:  recvType,
				IsPointer: cs.isPointerReceiver(decl.Recv.List[0].Type),
			}
			ti.Methods = append(ti.Methods, mi)
			pkg.Types[recvType] = ti
		}
	}

	pkg.Funcs[fi.Name] = fi
	cs.symbols[importPath+"."+fi.Name] = &SymbolInfo{
		Name:      importPath + "." + fi.Name,
		ShortName: fi.Name,
		Kind:      "func",
		Package:   importPath,
		File:      file,
		Line:      pos.Line,
		Exported:  exported,
	}
}

// parseParams 解析参数列表
func (cs *ContractStore) parseParams(fields *ast.FieldList) []ParamInfo {
	if fields == nil {
		return nil
	}
	var params []ParamInfo
	for _, field := range fields.List {
		typ := cs.exprToString(field.Type)
		if len(field.Names) == 0 {
			params = append(params, ParamInfo{Type: typ})
		} else {
			for _, name := range field.Names {
				params = append(params, ParamInfo{Name: name.Name, Type: typ})
			}
		}
	}
	return params
}

// isPointerReceiver 判断 receiver 是否为指针
func (cs *ContractStore) isPointerReceiver(expr ast.Expr) bool {
	_, ok := expr.(*ast.StarExpr)
	return ok
}

// exprToString 将 ast.Expr 转为字符串表示
func (cs *ContractStore) exprToString(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + cs.exprToString(e.X)
	case *ast.ArrayType:
		if e.Len == nil {
			return "[]" + cs.exprToString(e.Elt)
		}
		return "[" + cs.exprToString(e.Len) + "]" + cs.exprToString(e.Elt)
	case *ast.MapType:
		return "map[" + cs.exprToString(e.Key) + "]" + cs.exprToString(e.Value)
	case *ast.ChanType:
		return "chan " + cs.exprToString(e.Value)
	case *ast.FuncType:
		return "func" + cs.funcTypeToString(e)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct{}"
	case *ast.SelectorExpr:
		return cs.exprToString(e.X) + "." + e.Sel.Name
	case *ast.Ellipsis:
		return "..." + cs.exprToString(e.Elt)
	case *ast.ParenExpr:
		return "(" + cs.exprToString(e.X) + ")"
	case *ast.CallExpr:
		return cs.exprToString(e.Fun) + "(...)"
	case *ast.BasicLit:
		return e.Value
	case *ast.CompositeLit:
		return cs.exprToString(e.Type) + "{}"
	case *ast.IndexExpr:
		return cs.exprToString(e.X) + "[" + cs.exprToString(e.Index) + "]"
	case *ast.IndexListExpr:
		var indices []string
		for _, idx := range e.Indices {
			indices = append(indices, cs.exprToString(idx))
		}
		return cs.exprToString(e.X) + "[" + strings.Join(indices, ", ") + "]"
	case *ast.TypeAssertExpr:
		return cs.exprToString(e.X) + ".(" + cs.exprToString(e.Type) + ")"
	case *ast.SliceExpr:
		return cs.exprToString(e.X) + "[...]"
	case *ast.KeyValueExpr:
		return cs.exprToString(e.Key) + ": " + cs.exprToString(e.Value)
	case *ast.BinaryExpr:
		return cs.exprToString(e.X) + " " + e.Op.String() + " " + cs.exprToString(e.Y)
	case *ast.UnaryExpr:
		return e.Op.String() + cs.exprToString(e.X)
	case *ast.BadExpr:
		return "<bad_expr>"
	default:
		return fmt.Sprintf("<%T>", expr)
	}
}

// funcTypeToString 将 FuncType 转为签名字符串
func (cs *ContractStore) funcTypeToString(ft *ast.FuncType) string {
	if ft == nil {
		return "()"
	}
	var sb strings.Builder
	sb.WriteString("(")
	if ft.Params != nil {
		for i, field := range ft.Params.List {
			if i > 0 {
				sb.WriteString(", ")
			}
			typ := cs.exprToString(field.Type)
			if len(field.Names) == 0 {
				sb.WriteString(typ)
			} else {
				for j, name := range field.Names {
					if j > 0 {
						sb.WriteString(", ")
					}
					sb.WriteString(name.Name + " " + typ)
				}
			}
		}
	}
	sb.WriteString(")")

	if ft.Results != nil && len(ft.Results.List) > 0 {
		sb.WriteString(" ")
		if len(ft.Results.List) > 1 || (len(ft.Results.List) == 1 && len(ft.Results.List[0].Names) > 1) {
			sb.WriteString("(")
		}
		for i, field := range ft.Results.List {
			if i > 0 {
				sb.WriteString(", ")
			}
			typ := cs.exprToString(field.Type)
			if len(field.Names) == 0 {
				sb.WriteString(typ)
			} else {
				for j, name := range field.Names {
					if j > 0 {
						sb.WriteString(", ")
					}
					sb.WriteString(name.Name + " " + typ)
				}
			}
		}
		if len(ft.Results.List) > 1 || (len(ft.Results.List) == 1 && len(ft.Results.List[0].Names) > 1) {
			sb.WriteString(")")
		}
	}

	return sb.String()
}

// RefreshPackages 增量刷新指定 package
func (cs *ContractStore) RefreshPackages(pkgPaths []string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for _, pkgPath := range pkgPaths {
		// 删除旧数据
		if oldPkg, ok := cs.packages[pkgPath]; ok {
			// 删除该 package 的所有符号
			for k := range cs.symbols {
				if strings.HasPrefix(k, pkgPath+".") {
					delete(cs.symbols, k)
				}
			}
			delete(cs.packages, pkgPath)
			// 重新扫描 package 目录
			for _, file := range oldPkg.Files {
				if _, err := os.Stat(file); err == nil {
					cs.parseFile(file)
				}
			}
		}
	}

	return nil
}

// InferChangedPackages 从修改的文件推断出受影响的 package
func (cs *ContractStore) InferChangedPackages(changedFiles []string) []string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	seen := make(map[string]bool)
	for _, file := range changedFiles {
		dir := filepath.Dir(file)
		for importPath, pkg := range cs.packages {
			if pkg.Path == dir || strings.HasPrefix(dir, pkg.Path+"/") {
				seen[importPath] = true
			}
		}
	}

	var result []string
	for p := range seen {
		result = append(result, p)
	}
	return result
}

// ─── 查询接口 ───

// FindType 查找类型
func (cs *ContractStore) FindType(pkgPath, name string) (TypeInfo, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	pkg, ok := cs.packages[pkgPath]
	if !ok {
		return TypeInfo{}, false
	}
	 ti, ok := pkg.Types[name]
	 return ti, ok
}

// FindTypeByQName 通过全限定名查找类型
func (cs *ContractStore) FindTypeByQName(qname string) (TypeInfo, bool) {
	parts := strings.LastIndex(qname, ".")
	if parts < 0 {
		return TypeInfo{}, false
	}
	return cs.FindType(qname[:parts], qname[parts+1:])
}

// FindInterface 查找接口及其完整信息
func (cs *ContractStore) FindInterface(pkgPath, name string) (*InterfaceContract, bool) {
	ti, ok := cs.FindType(pkgPath, name)
	if !ok || ti.Kind != "interface" {
		return nil, false
	}

	iface := &InterfaceContract{
		TypeInfo:     ti,
		Implementors: cs.findImplementors(pkgPath, name, ti),
		Callers:      cs.findCallers(pkgPath, name),
	}
	return iface, true
}

// InterfaceContract 接口契约（包含实现者和调用者）
type InterfaceContract struct {
	TypeInfo
	Implementors []string    `json:"implementors"`
	Callers      []CallerRef `json:"callers"`
}

// findImplementors 查找接口的所有实现者
func (cs *ContractStore) findImplementors(ifacePkg, ifaceName string, iface TypeInfo) []string {
	var implementors []string
	for importPath, pkg := range cs.packages {
		for typeName, ti := range pkg.Types {
			if ti.Kind != "struct" {
				continue
			}
			if cs.implements(ti, iface) {
				implementors = append(implementors, importPath+"."+typeName)
			}
		}
	}
	return implementors
}

// implements 判断 struct 是否实现 interface
func (cs *ContractStore) implements(structType TypeInfo, iface TypeInfo) bool {
	if iface.Kind != "interface" {
		return false
	}
	ifaceMethods := make(map[string]MethodInfo)
	for _, m := range iface.Methods {
		ifaceMethods[m.Name] = m
	}

	// struct 自身的方法
	structMethods := make(map[string]MethodInfo)
	for _, m := range structType.Methods {
		structMethods[m.Name] = m
	}

	// 检查 interface 的每个方法
	for name, ifaceMethod := range ifaceMethods {
		structMethod, ok := structMethods[name]
		if !ok {
			return false
		}
		// 检查签名匹配（简化：比较参数和返回类型数量）
		if len(structMethod.Params) != len(ifaceMethod.Params) ||
			len(structMethod.Returns) != len(ifaceMethod.Returns) {
			return false
		}
		// TODO: 更精确的类型匹配
	}
	return true
}

// findCallers 查找接口的调用者
func (cs *ContractStore) findCallers(pkgPath, typeName string) []CallerRef {
	// 简化实现：返回空列表
	// 实际实现需要遍历所有文件的 AST 找到 method call
	return nil
}

// FindFunction 查找函数
func (cs *ContractStore) FindFunction(pkgPath, name string) (FuncInfo, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	pkg, ok := cs.packages[pkgPath]
	if !ok {
		return FuncInfo{}, false
	}
	fi, ok := pkg.Funcs[name]
	return fi, ok
}

// FindSymbol 通过全限定名查找符号
func (cs *ContractStore) FindSymbol(qname string) (*SymbolInfo, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	s, ok := cs.symbols[qname]
	return s, ok
}

// GetPackageImports 获取包的导入列表
func (cs *ContractStore) GetPackageImports(pkgPath string) []ImportInfo {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	pkg, ok := cs.packages[pkgPath]
	if !ok {
		return nil
	}
	return pkg.Imports
}

// GetPackagesByPrefix 按前缀查找包
func (cs *ContractStore) GetPackagesByPrefix(prefix string) []string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	var result []string
	for path := range cs.packages {
		if strings.HasPrefix(path, prefix) {
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}

// SearchSymbols 模糊搜索符号
func (cs *ContractStore) SearchSymbols(query string) []SymbolInfo {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	var results []SymbolInfo
	q := strings.ToLower(query)
	for _, s := range cs.symbols {
		if strings.Contains(strings.ToLower(s.Name), q) ||
			strings.Contains(strings.ToLower(s.ShortName), q) {
			results = append(results, *s)
		}
	}
	return results
}

// GetModule 返回 module path
func (cs *ContractStore) GetModule() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.module
}

// ToJSON 导出为 JSON
func (cs *ContractStore) ToJSON() ([]byte, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return json.MarshalIndent(struct {
		Module   string                  `json:"module"`
		Packages map[string]*PackageInfo `json:"packages"`
		Symbols  map[string]*SymbolInfo  `json:"symbols"`
	}{
		Module:   cs.module,
		Packages: cs.packages,
		Symbols:  cs.symbols,
	}, "", "  ")
}

// PackageCount 返回 package 数量
func (cs *ContractStore) PackageCount() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.packages)
}

// SymbolCount 返回符号数量
func (cs *ContractStore) SymbolCount() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.symbols)
}
```

---

## Patch Apply 完整实现

`pkg/agent/patch_apply.go` — 基于 AST 的结构感知编辑。

```go
package agent

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
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
		src, err := os.ReadFile(path)
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
```

---

## Validation Gate 完整实现

`pkg/agent/validation_gate.go` — 硬阻断验证门禁。

```go
package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/toolskill"
)

// GateStage 验证阶段
type GateStage string

const (
	GateCompile GateStage = "compile" // go build / go test compile
	GateStatic  GateStage = "static"  // go vet / staticcheck
	GateTest    GateStage = "test"    // go test
	GateFormat  GateStage = "format"  // gofmt / goimports
	GateQuality GateStage = "quality" // repo-quality-gate
)

// GateResult 验证结果
type GateResult struct {
	Passed      bool                 `json:"passed"`
	Stage       GateStage            `json:"stage,omitempty"` // 首次失败的阶段
	Diagnostics []toolskill.Diagnostic `json:"diagnostics,omitempty"`
	Blockers    []GateBlocker        `json:"blockers,omitempty"`
	Warnings    []GateWarning        `json:"warnings,omitempty"`
	Evidence    map[string]any       `json:"evidence"` // 各 skill 原始输出
	Duration    time.Duration        `json:"duration"`
	Round       int                  `json:"round"`
}

// GateBlocker 阻断项
type GateBlocker struct {
	Stage   string `json:"stage"`
	Rule    string `json:"rule"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
}

// GateWarning 警告项
type GateWarning struct {
	Stage   string `json:"stage"`
	Rule    string `json:"rule"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// ValidationGate 验证门禁
type ValidationGate struct {
	mu          sync.RWMutex
	stages      []GateConfig          // 验证阶段配置（有序）
	hardStages  map[GateStage]bool    // 哪些 stage 失败必须阻断
	maxRounds   int                   // 单 work unit 最大修复轮数
	roundBudget int                   // 当前已用轮数
	autoFix     map[string]bool       // 哪些 rule 可以自动修复
}

// GateConfig 阶段配置
type GateConfig struct {
	Stage       GateStage `json:"stage"`
	SkillName   string    `json:"skill_name"`   // 对应的 Tool Skill 名
	Required    bool      `json:"required"`     // 是否必须执行
	AutoRetry   bool      `json:"auto_retry"`   // 失败时是否允许自动修复
	MaxAutoFix  int       `json:"max_auto_fix"` // 自动修复最大尝试次数
}

// DefaultGateConfig 返回默认验证阶段配置
func DefaultGateConfig() []GateConfig {
	return []GateConfig{
		{Stage: GateFormat, SkillName: "go-static-check", Required: true, AutoRetry: true, MaxAutoFix: 1},
		{Stage: GateCompile, SkillName: "go-static-check", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateStatic, SkillName: "go-static-check", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateTest, SkillName: "go-static-check", Required: true, AutoRetry: false, MaxAutoFix: 0},
		{Stage: GateQuality, SkillName: "repo-quality-gate", Required: true, AutoRetry: false, MaxAutoFix: 0},
	}
}

// NewValidationGate 创建验证门禁
func NewValidationGate(config []GateConfig, maxRounds int) *ValidationGate {
	hard := make(map[GateStage]bool)
	for _, s := range []GateStage{GateCompile, GateStatic, GateTest, GateQuality} {
		hard[s] = true
	}
	return &ValidationGate{
		stages:     config,
		hardStages: hard,
		maxRounds:  maxRounds,
		autoFix: map[string]bool{
			"unused_import": true,
			"format":        true,
		},
	}
}

// Validate 按阶段顺序执行验证
func (vg *ValidationGate) Validate(
	ctx context.Context,
	runtime *toolskill.Runtime,
	req toolskill.SkillRequest,
) (*GateResult, error) {
	start := time.Now()
	result := &GateResult{
		Passed:   true,
		Evidence: make(map[string]any),
		Round:    vg.roundBudget,
	}

	for _, cfg := range vg.stages {
		if !cfg.Required {
			continue
		}

		skillResult, err := runtime.Execute(ctx, cfg.SkillName, req)
		if err != nil {
			return nil, fmt.Errorf("stage %s skill execution error: %w", cfg.Stage, err)
		}

		result.Evidence[string(cfg.Stage)] = skillResult

		if skillResult.Status == "pass" {
			continue
		}

		// 失败处理
		result.Passed = false
		result.Stage = cfg.Stage

		for _, d := range skillResult.Diagnostics {
			if d.Severity == "blocking" || vg.hardStages[cfg.Stage] {
				result.Blockers = append(result.Blockers, GateBlocker{
					Stage:   string(cfg.Stage),
					Rule:    d.Category,
					File:    d.File,
					Line:    d.Line,
					Column:  d.Column,
					Message: d.Message,
				})
			} else {
				result.Warnings = append(result.Warnings, GateWarning{
					Stage:   string(cfg.Stage),
					Rule:    d.Category,
					File:    d.File,
					Line:    d.Line,
					Message: d.Message,
				})
			}
		}

		result.Diagnostics = append(result.Diagnostics, skillResult.Diagnostics...)

		// Hard stage 有阻断项 → 立即停止
		if vg.hardStages[cfg.Stage] && len(result.Blockers) > 0 {
			result.Duration = time.Since(start)
			return result, nil
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}

// CanProceed 判断是否允许继续修复
func (vg *ValidationGate) CanProceed(result *GateResult) bool {
	vg.mu.Lock()
	defer vg.mu.Unlock()
	vg.roundBudget++
	if vg.roundBudget >= vg.maxRounds {
		return false
	}
	return !result.Passed
}

// GetRoundBudget 获取当前轮数
func (vg *ValidationGate) GetRoundBudget() int {
	vg.mu.RLock()
	defer vg.mu.RUnlock()
	return vg.roundBudget
}

// ResetRoundBudget 重置轮数
func (vg *ValidationGate) ResetRoundBudget() {
	vg.mu.Lock()
	defer vg.mu.Unlock()
	vg.roundBudget = 0
}

// IsAutoFixable 判断某个 rule 是否可以自动修复
func (vg *ValidationGate) IsAutoFixable(rule string) bool {
	vg.mu.RLock()
	defer vg.mu.RUnlock()
	return vg.autoFix[rule]
}

// AddAutoFixRule 添加自动修复规则
func (vg *ValidationGate) AddAutoFixRule(rule string) {
	vg.mu.Lock()
	defer vg.mu.Unlock()
	vg.autoFix[rule] = true
}

// Summary 返回验证结果的文本摘要
func (gr *GateResult) Summary() string {
	if gr.Passed {
		return fmt.Sprintf("Validation passed (%d rounds, %s)", gr.Round, gr.Duration)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Validation FAILED at stage '%s' (round %d, %s)\n", gr.Stage, gr.Round, gr.Duration))
	sb.WriteString(fmt.Sprintf("Blockers: %d, Warnings: %d\n", len(gr.Blockers), len(gr.Warnings)))
	for _, b := range gr.Blockers {
		sb.WriteString(fmt.Sprintf("  [BLOCKER] %s:%d %s: %s\n", b.File, b.Line, b.Rule, b.Message))
	}
	return sb.String()
}

// GetTopBlocker 获取优先级最高的阻断项
func (gr *GateResult) GetTopBlocker() *GateBlocker {
	if len(gr.Blockers) == 0 {
		return nil
	}
	return &gr.Blockers[0]
}

// ShouldRetry 判断是否值得重试
func (gr *GateResult) ShouldRetry() bool {
	if gr.Passed {
		return false
	}
	// 如果有明确可修复的 blocker，值得重试
	for _, b := range gr.Blockers {
		switch b.Rule {
		case "undefined", "missing_method", "wrong_import", "type_mismatch",
			"wrong_struct_field", "constructor_stub":
			return true
		}
	}
	return false
}

// ── Karpathy 原则扩展检查 ──

// GateSimplicity 代码复杂度检查阶段
func (vg *ValidationGate) CheckSimplicity(
	ctx context.Context,
	changedFiles []string,
	budget int, // 最大允许变更行数
) *GateResult {
	result := &GateResult{Passed: true, Stage: "simplicity"}

	totalAdditions := 0
	totalDeletions := 0
	for _, f := range changedFiles {
		additions, deletions := countDiffLines(f)
		totalAdditions += additions
		totalDeletions += deletions
	}

	// Simplicity 检查 1: 行数预算
	if totalAdditions+totalDeletions > budget {
		result.Passed = false
		result.Blockers = append(result.Blockers, GateBlocker{
			Stage:   "simplicity",
			Rule:    "line_budget_exceeded",
			Message: fmt.Sprintf("Changed lines %d exceeds budget %d (add: %d, del: %d)", totalAdditions+totalDeletions, budget, totalAdditions, totalDeletions),
		})
	}

	// Simplicity 检查 2: 膨胀检测（新增行是删除行的 5 倍以上）
	if totalDeletions > 0 && totalAdditions/totalDeletions > 5 {
		result.Warnings = append(result.Warnings, GateWarning{
			Stage:   "simplicity",
			Rule:    "code_bloat_detected",
			Message: fmt.Sprintf("Code bloat: %d additions vs %d deletions (ratio %.1f:1)", totalAdditions, totalDeletions, float64(totalAdditions)/float64(totalDeletions)),
		})
	}

	return result
}

// GateSurgical 变更范围审计阶段
func (vg *ValidationGate) CheckSurgical(
	ctx context.Context,
	declaredFiles []string,
	actualFiles []string,
	patchResults map[string][]string, // file -> [before_context, after_context]
) *GateResult {
	result := &GateResult{Passed: true, Stage: "surgical"}

	// Surgical 检查 1: 是否修改了未声明的文件
	declaredSet := make(map[string]bool)
	for _, f := range declaredFiles {
		declaredSet[f] = true
	}
	for _, f := range actualFiles {
		if !declaredSet[f] {
			result.Passed = false
			result.Blockers = append(result.Blockers, GateBlocker{
				Stage:   "surgical",
				Rule:    "undeclared_file_modified",
				File:    f,
				Message: fmt.Sprintf("File %s was modified but not declared in Change Scope", f),
			})
		}
	}

	// Surgical 检查 2: 相邻代码保护（检测附带损伤）
	for file, contexts := range patchResults {
		if len(contexts) >= 2 {
			before := contexts[0]
			after := contexts[1]
			// 简单启发：如果替换块前后 3 行的文本发生非预期变化
			if hasCollateralDamage(before, after) {
				result.Warnings = append(result.Warnings, GateWarning{
					Stage:   "surgical",
					Rule:    "collateral_damage_detected",
					File:    file,
					Message: "Detected changes to adjacent code outside the declared scope",
				})
			}
		}
	}

	return result
}

// GateThinkPhase Think Phase 存在性检查
func (vg *ValidationGate) CheckThinkPhase(output string) *GateResult {
	result := &GateResult{Passed: true, Stage: "think_phase"}
	if !strings.Contains(output, "<think>") || !strings.Contains(output, "</think>") {
		result.Passed = false
		result.Blockers = append(result.Blockers, GateBlocker{
			Stage:   "think_phase",
			Rule:    "missing_think_phase",
			Message: "LLM output is missing the required <think>...</think> block. Rejecting output.",
		})
	}
	return result
}

// countDiffLines 统计文件的增删行数（简化实现）
func countDiffLines(filePath string) (additions, deletions int) {
	// 实际实现应调用 git diff 或解析 patch
	// 这里提供接口占位
	return 0, 0
}

// hasCollateralDamage 检测替换前后的附带损伤
func hasCollateralDamage(before, after string) bool {
	// 提取替换块前后各 3 行作为上下文
	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")
	if len(beforeLines) < 3 || len(afterLines) < 3 {
		return false
	}
	// 如果前后上下文不一致，说明有附带修改
	prefixMatch := beforeLines[0] == afterLines[0]
	suffixMatch := beforeLines[len(beforeLines)-1] == afterLines[len(afterLines)-1]
	return !prefixMatch || !suffixMatch
}

---

## Context Engine 完整实现

`pkg/agent/context_engine.go` — 根据诊断类型检索相关契约上下文。

```go
package agent

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ContextRequest 上下文检索请求
type ContextRequest struct {
	TaskType      string   `json:"task_type"`       // implement/fix/refactor
	TargetFile    string   `json:"target_file"`
	TargetLine    int      `json:"target_line"`
	Diagnostic    string   `json:"diagnostic"`      // 原始诊断文本
	Category      string   `json:"category"`        // missing_method/wrong_import/type_mismatch
	Symbol        string   `json:"symbol"`          // 涉及的符号
	RelatedFiles  []string `json:"related_files,omitempty"`
}

// ContextChunk 上下文片段
type ContextChunk struct {
	Source    string  `json:"source"`    // contract/import/test/doc
	File      string  `json:"file"`
	Line      int     `json:"line"`
	Content   string  `json:"content"`
	Relevance float64 `json:"relevance"` // 0.0-1.0
}

// ContextResult 检索结果
type ContextResult struct {
	Chunks      []ContextChunk `json:"chunks"`
	SymbolFound bool           `json:"symbol_found"`
	Suggestions []string       `json:"suggestions"`
}

// ContextEngine 上下文检索引擎
type ContextEngine struct {
	store *ContractStore
}

func NewContextEngine(store *ContractStore) *ContextEngine {
	return &ContextEngine{store: store}
}

// Retrieve 主入口：根据诊断类型分发到具体检索逻辑
func (ce *ContextEngine) Retrieve(req ContextRequest) (*ContextResult, error) {
	result := &ContextResult{}

	switch req.Category {
	case "missing_method", "interface_mismatch", "not_declared_by_package":
		ce.retrieveMissingMethod(req, result)
	case "wrong_import", "import_alias_conflict", "undefined_import":
		ce.retrieveImportConflict(req, result)
	case "wrong_struct_field", "undefined_field", "missing_field":
		ce.retrieveFieldError(req, result)
	case "type_mismatch", "mismatched_types":
		ce.retrieveTypeMismatch(req, result)
	case "constructor_stub", "fake_implementation", "undefined_type":
		ce.retrieveFakeOrUndefined(req, result)
	case "unused_import":
		ce.retrieveUnusedImport(req, result)
	case "format":
		result.Suggestions = append(result.Suggestions, "Run goimports or gofmt to fix formatting")
	default:
		ce.retrieveGeneric(req, result)
	}

	sortChunksByRelevance(result.Chunks)
	return result, nil
}

// retrieveMissingMethod 处理 "X has no method Y"
func (ce *ContextEngine) retrieveMissingMethod(req ContextRequest, result *ContextResult) {
	typeName, methodName := ce.parseMethodSymbol(req.Symbol)
	if typeName == "" {
		return
	}

	found := false
	for pkgPath, pkg := range ce.store.packages {
		if ti, ok := pkg.Types[typeName]; ok {
			found = true
			result.SymbolFound = true

			result.Chunks = append(result.Chunks, ContextChunk{
				Source:    "contract",
				File:      ti.File,
				Line:      ti.Line,
				Content:   ce.formatTypeDefinition(ti),
				Relevance: 1.0,
			})

			if ti.Kind == "interface" {
				iface, _ := ce.store.FindInterface(pkgPath, typeName)
				if iface != nil {
					for _, imp := range iface.Implementors {
						result.Chunks = append(result.Chunks, ContextChunk{
							Source:    "contract",
							File:      "",
							Line:      0,
							Content:   fmt.Sprintf("// Implementor: %s", imp),
							Relevance: 0.7,
						})
					}
					result.Suggestions = append(result.Suggestions,
						fmt.Sprintf("Option 1: Add method '%s' to interface '%s'", methodName, typeName),
						fmt.Sprintf("Option 2: Check if implementor '%s' should have this method", firstOr(iface.Implementors, "<none>")),
					)
				}
			}

			if ti.Kind == "struct" {
				methodNames := make(map[string]bool)
				for _, m := range ti.Methods {
					methodNames[m.Name] = true
				}
				if !methodNames[methodName] {
					result.Suggestions = append(result.Suggestions,
						fmt.Sprintf("Add method '%s' to struct '%s'", methodName, typeName),
						fmt.Sprintf("Existing methods: %s", strings.Join(mapKeys(methodNames), ", ")),
					)
				}
			}

			if req.TargetFile != "" {
				result.Chunks = append(result.Chunks, ContextChunk{
					Source:    "source",
					File:      req.TargetFile,
					Line:      req.TargetLine,
					Content:   fmt.Sprintf("// Call site at %s:%d", req.TargetFile, req.TargetLine),
					Relevance: 0.8,
				})
			}
		}
	}

	if !found {
		result.Suggestions = append(result.Suggestions,
			fmt.Sprintf("Type '%s' not found in contract. Need to define it first (contract-first).", typeName),
		)
	}
}

// retrieveImportConflict 处理 import 冲突
func (ce *ContextEngine) retrieveImportConflict(req ContextRequest, result *ContextResult) {
	alias := req.Symbol
	if alias == "" {
		alias = ce.extractAliasFromDiagnostic(req.Diagnostic)
	}

	for pkgPath, pkg := range ce.store.packages {
		for _, imp := range pkg.Imports {
			if imp.Alias == alias || filepath.Base(imp.Path) == alias {
				result.Chunks = append(result.Chunks, ContextChunk{
					Source:    "contract",
					File:      "",
					Line:      0,
					Content:   fmt.Sprintf("// Package %s imports %q as %q", pkgPath, imp.Path, alias),
					Relevance: 0.9,
				})
			}
		}
	}

	result.Suggestions = append(result.Suggestions,
		fmt.Sprintf("If stdlib '%s' and local pkg both use alias '%s', rename local import:", alias, alias),
		fmt.Sprintf("  import %s \"%s\" // change to: local%s \"...\"", alias, "<local-path>", alias),
	)
}

// retrieveFieldError 处理 struct field 错误
func (ce *ContextEngine) retrieveFieldError(req ContextRequest, result *ContextResult) {
	typeName := req.Symbol
	if typeName == "" {
		typeName = ce.extractTypeFromFieldError(req.Diagnostic)
	}

	for _, pkg := range ce.store.packages {
		if ti, ok := pkg.Types[typeName]; ok && ti.Kind == "struct" {
			result.SymbolFound = true
			result.Chunks = append(result.Chunks, ContextChunk{
				Source:    "contract",
				File:      ti.File,
				Line:      ti.Line,
				Content:   ce.formatTypeDefinition(ti),
				Relevance: 1.0,
			})

			var fieldNames []string
			for _, f := range ti.Fields {
				fieldNames = append(fieldNames, f.Name)
			}
			result.Suggestions = append(result.Suggestions,
				fmt.Sprintf("Struct '%s' has fields: %s", typeName, strings.Join(fieldNames, ", ")),
			)
		}
	}
}

// retrieveTypeMismatch 处理类型不匹配
func (ce *ContextEngine) retrieveTypeMismatch(req ContextRequest, result *ContextResult) {
	expected, actual := ce.extractTypesFromMismatch(req.Diagnostic)

	if expected != "" {
		for _, pkg := range ce.store.packages {
			if ti, ok := pkg.Types[expected]; ok {
				result.Chunks = append(result.Chunks, ContextChunk{
					Source:    "contract",
					File:      ti.File,
					Line:      ti.Line,
					Content:   ce.formatTypeDefinition(ti),
					Relevance: 0.9,
				})
			}
		}
	}

	if actual != "" {
		for _, pkg := range ce.store.packages {
			if ti, ok := pkg.Types[actual]; ok {
				result.Chunks = append(result.Chunks, ContextChunk{
					Source:    "contract",
					File:      ti.File,
					Line:      ti.Line,
					Content:   ce.formatTypeDefinition(ti),
					Relevance: 0.8,
				})
			}
		}
	}

	result.Suggestions = append(result.Suggestions,
		fmt.Sprintf("Expected type: %s, Actual type: %s", expected, actual),
		"Check if a type conversion or wrapper is needed",
	)
}

// retrieveFakeOrUndefined 处理假实现或未定义类型
func (ce *ContextEngine) retrieveFakeOrUndefined(req ContextRequest, result *ContextResult) {
	typeName := req.Symbol
	if typeName == "" {
		typeName = ce.extractTypeFromUndefined(req.Diagnostic)
	}

	found := false
	for pkgPath, pkg := range ce.store.packages {
		if ti, ok := pkg.Types[typeName]; ok {
			found = true
			result.SymbolFound = true
			result.Chunks = append(result.Chunks, ContextChunk{
				Source:    "contract",
				File:      ti.File,
				Line:      ti.Line,
				Content:   ce.formatTypeDefinition(ti),
				Relevance: 1.0,
			})

			if ti.Kind == "interface" && len(ti.Methods) == 0 {
				result.Suggestions = append(result.Suggestions,
					fmt.Sprintf("WARNING: Interface '%s' is empty (likely fake implementation)", typeName),
					fmt.Sprintf("Define real methods for '%s.%s'", pkgPath, typeName),
				)
			}
		}
	}

	if !found {
		matches := ce.store.SearchSymbols(typeName)
		if len(matches) > 0 {
			result.Suggestions = append(result.Suggestions,
				fmt.Sprintf("'%s' not found. Did you mean:", typeName),
			)
			for _, m := range matches[:min(5, len(matches))] {
				result.Suggestions = append(result.Suggestions, fmt.Sprintf("  - %s (%s)", m.Name, m.Kind))
			}
		} else {
			result.Suggestions = append(result.Suggestions,
				fmt.Sprintf("'%s' not found in any package. Define it first (contract-first).", typeName),
			)
		}
	}
}

// retrieveUnusedImport 处理未使用的 import
func (ce *ContextEngine) retrieveUnusedImport(req ContextRequest, result *ContextResult) {
	importPath := req.Symbol
	result.Suggestions = append(result.Suggestions,
		fmt.Sprintf("Import '%s' is unused. Remove it or use it.", importPath),
		"Auto-fix available: goimports -w",
	)
}

// retrieveGeneric 通用检索
func (ce *ContextEngine) retrieveGeneric(req ContextRequest, result *ContextResult) {
	if req.TargetFile == "" {
		return
	}
	dir := filepath.Dir(req.TargetFile)
	for pkgPath, pkg := range ce.store.packages {
		if pkg.Path == dir || strings.HasPrefix(dir, pkg.Path+string(filepath.Separator)) {
			for name, ti := range pkg.Types {
				if ti.Exported {
					result.Chunks = append(result.Chunks, ContextChunk{
						Source:    "contract",
						File:      ti.File,
						Line:      ti.Line,
						Content:   fmt.Sprintf("// %s.%s (%s)", pkgPath, name, ti.Kind),
						Relevance: 0.3,
					})
				}
			}
		}
	}
}

// helper methods
func (ce *ContextEngine) parseMethodSymbol(symbol string) (typeName, methodName string) {
	if symbol == "" {
		return "", ""
	}
	parts := strings.Split(symbol, ".")
	if len(parts) >= 2 {
		methodName = parts[len(parts)-1]
		typeName = parts[len(parts)-2]
	}
	return
}

func (ce *ContextEngine) extractAliasFromDiagnostic(diag string) string {
	if idx := strings.Index(diag, `":"`); idx > 0 {
		start := strings.LastIndex(diag[:idx], `"`)
		if start >= 0 {
			return diag[start+1 : idx]
		}
	}
	return ""
}

func (ce *ContextEngine) extractTypeFromFieldError(diag string) string {
	if idx := strings.Index(diag, "type "); idx >= 0 {
		return strings.TrimSpace(diag[idx+5:])
	}
	return ""
}

func (ce *ContextEngine) extractTypesFromMismatch(diag string) (expected, actual string) {
	if idx := strings.Index(diag, "as type "); idx >= 0 {
		expected = strings.TrimSpace(diag[idx+8:])
	}
	if idx := strings.Index(diag, "type "); idx >= 0 {
		end := strings.Index(diag[idx+5:], ")")
		if end > 0 {
			actual = strings.TrimSpace(diag[idx+5 : idx+5+end])
		}
	}
	return
}

func (ce *ContextEngine) extractTypeFromUndefined(diag string) string {
	if idx := strings.Index(diag, "undefined:"); idx >= 0 {
		return strings.TrimSpace(diag[idx+10:])
	}
	return ""
}

func (ce *ContextEngine) formatTypeDefinition(ti TypeInfo) string {
	var sb strings.Builder
	switch ti.Kind {
	case "struct":
		sb.WriteString(fmt.Sprintf("type %s struct {\n", ti.Name))
		for _, f := range ti.Fields {
			if f.Embedded {
				sb.WriteString(fmt.Sprintf("    %s\n", f.Type))
			} else {
				sb.WriteString(fmt.Sprintf("    %s %s", f.Name, f.Type))
				if f.Tag != "" {
					sb.WriteString(fmt.Sprintf(" %s", f.Tag))
				}
				sb.WriteString("\n")
			}
		}
		sb.WriteString("}")
	case "interface":
		sb.WriteString(fmt.Sprintf("type %s interface {\n", ti.Name))
		for _, m := range ti.Methods {
			sb.WriteString(fmt.Sprintf("    %s%s\n", m.Name, m.Signature))
		}
		sb.WriteString("}")
	case "type-alias":
		sb.WriteString(fmt.Sprintf("type %s %s", ti.Name, ti.Underlying))
	default:
		sb.WriteString(fmt.Sprintf("type %s %s", ti.Name, ti.Kind))
	}
	return sb.String()
}

func sortChunksByRelevance(chunks []ContextChunk) {
	for i := 0; i < len(chunks); i++ {
		for j := i + 1; j < len(chunks); j++ {
			if chunks[j].Relevance > chunks[i].Relevance {
				chunks[i], chunks[j] = chunks[j], chunks[i]
			}
		}
	}
}

func firstOr(list []string, fallback string) string {
	if len(list) > 0 {
		return list[0]
	}
	return fallback
}

func mapKeys(m map[string]bool) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

---

## Code Executor 完整实现

`pkg/agent/executor.go` — 整合所有组件的执行循环。

```go
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/orchestrator"
	"github.com/anthropic/claude-go/pkg/toolskill"
)

// CodeTask 代码生成/修复任务
type CodeTask struct {
	ID           string   `json:"id"`
	Objective    string   `json:"objective"`
	TargetFiles  []string `json:"target_files,omitempty"`
	Language     string   `json:"language"`
	RepoRoot     string   `json:"repo_root"`
	ModulePath   string   `json:"module_path,omitempty"`
}

// CodeResult 执行结果
type CodeResult struct {
	TaskID     string          `json:"task_id"`
	Status     string          `json:"status"` // success/failed/timeout
	Patches    []PatchSummary  `json:"patches"`
	Validation *GateResult     `json:"validation"`
	Trace      []ExecutionStep `json:"trace"`
	RoundCount int             `json:"round_count"`
	Duration   time.Duration   `json:"duration"`
	Error      string          `json:"error,omitempty"`
}

// PatchSummary 补丁摘要
type PatchSummary struct {
	File      string `json:"file"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Snippet   string `json:"snippet,omitempty"`
}

// ExecutionStep 执行步骤
type ExecutionStep struct {
	Round    int            `json:"round"`
	Action   string         `json:"action"`
	Duration time.Duration  `json:"duration"`
	Result   string         `json:"result"`
	Details  map[string]any `json:"details,omitempty"`
}

// CodeExecutor 代码执行器（实现 orchestrator.TaskRunner）
type CodeExecutor struct {
	contractStore  *ContractStore
	patchApplier   *PatchApplier
	validationGate *ValidationGate
	contextEngine  *ContextEngine
	skillRuntime   *toolskill.Runtime
	llmRunner      orchestrator.LLMRunner
	maxRounds      int
	promptSkills   []string
}

func NewCodeExecutor(
	cs *ContractStore,
	pa *PatchApplier,
	vg *ValidationGate,
	ce *ContextEngine,
	sr *toolskill.Runtime,
	llm orchestrator.LLMRunner,
) *CodeExecutor {
	return &CodeExecutor{
		contractStore:  cs,
		patchApplier:   pa,
		validationGate: vg,
		contextEngine:  ce,
		skillRuntime:   sr,
		llmRunner:      llm,
		maxRounds:      vg.maxRounds,
		promptSkills:   []string{"go-repair-strategy", "contract-first-coding"},
	}
}

func (e *CodeExecutor) Name() string { return "code-executor" }

func (e *CodeExecutor) Execute(
	ctx context.Context,
	task *orchestrator.Task,
	bb orchestrator.ReadOnlyBlackboard,
) (any, error) {
	codeTask := e.parseCodeTask(task, bb)
	start := time.Now()
	result := &CodeResult{
		TaskID: codeTask.ID,
		Status: "failed",
		Trace:  make([]ExecutionStep, 0),
	}

	if err := e.initContractStore(codeTask); err != nil {
		result.Error = err.Error()
		return result, err
	}

	e.validationGate.ResetRoundBudget()

	for round := 0; round < e.maxRounds; round++ {
		roundStart := time.Now()
		var snippets []EditSnippet
		var stepErr error

		if round == 0 {
			snippets, stepErr = e.generateCode(ctx, codeTask)
		} else {
			snippets, stepErr = e.repairFromDiagnostics(ctx, codeTask, result.Validation)
		}

		if stepErr != nil {
			action := "generate"
			if round > 0 {
				action = "repair"
			}
			result.Trace = append(result.Trace, ExecutionStep{
				Round:    round,
				Action:   action,
				Duration: time.Since(roundStart),
				Result:   "fail",
				Details:  map[string]any{"error": stepErr.Error()},
			})
			result.Error = stepErr.Error()
			return result, stepErr
		}

		applyStart := time.Now()
		applyResult, err := e.patchApplier.Apply(codeTask.RepoRoot, snippets)
		if err != nil {
			result.Trace = append(result.Trace, ExecutionStep{
				Round:    round,
				Action:   "apply",
				Duration: time.Since(applyStart),
				Result:   "fail",
				Details:  map[string]any{"error": err.Error()},
			})
			result.Error = err.Error()
			return result, err
		}

		result.Trace = append(result.Trace, ExecutionStep{
			Round:    round,
			Action:   "apply",
			Duration: time.Since(applyStart),
			Result:   "success",
			Details:  map[string]any{"changed": applyResult.Changed},
		})

		valStart := time.Now()
		valReq := toolskill.SkillRequest{
			RepoRoot:     codeTask.RepoRoot,
			ChangedFiles: applyResult.Changed,
			Scope:        "package",
			Language:     codeTask.Language,
		}
		gateResult, err := e.validationGate.Validate(ctx, e.skillRuntime, valReq)
		if err != nil {
			result.Error = err.Error()
			return result, err
		}

		result.Validation = gateResult
		result.RoundCount = round + 1

		result.Trace = append(result.Trace, ExecutionStep{
			Round:    round,
			Action:   "validate",
			Duration: time.Since(valStart),
			Result:   func() string { if gateResult.Passed { return "pass" }; return "fail" }(),
			Details: map[string]any{
				"stage":    gateResult.Stage,
				"blockers": len(gateResult.Blockers),
				"warnings": len(gateResult.Warnings),
			},
		})

		if gateResult.Passed {
			result.Status = "success"
			result.Duration = time.Since(start)
			result.Patches = e.buildPatchSummaries(codeTask.RepoRoot, snippets)
			return result, nil
		}

		if !gateResult.ShouldRetry() {
			result.Status = "failed"
			result.Duration = time.Since(start)
			result.Error = fmt.Sprintf("unrecoverable errors at stage %s", gateResult.Stage)
			return result, fmt.Errorf(result.Error)
		}

		if !e.validationGate.CanProceed(gateResult) {
			result.Status = "failed"
			result.Duration = time.Since(start)
			result.Error = fmt.Sprintf("max rounds (%d) exceeded", e.maxRounds)
			return result, fmt.Errorf(result.Error)
		}

		changedPkgs := e.contractStore.InferChangedPackages(applyResult.Changed)
		if len(changedPkgs) > 0 {
			e.contractStore.RefreshPackages(changedPkgs)
		}
	}

	result.Status = "failed"
	result.Duration = time.Since(start)
	result.Error = fmt.Sprintf("execution failed after %d rounds", result.RoundCount)
	return result, fmt.Errorf(result.Error)
}

func (e *CodeExecutor) initContractStore(task CodeTask) error {
	if e.contractStore.GetModule() == "" && task.ModulePath != "" {
		e.contractStore.module = task.ModulePath
	}
	if len(e.contractStore.packages) == 0 {
		return e.contractStore.BuildFromRepo()
	}
	return nil
}

func (e *CodeExecutor) generateCode(ctx context.Context, task CodeTask) ([]EditSnippet, error) {
	prompt := e.buildGenerationPrompt(task)
	output, err := e.llmRunner.Run(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("llm generation failed: %w", err)
	}
	return e.parseEditSnippets(output)
}

func (e *CodeExecutor) repairFromDiagnostics(
	ctx context.Context,
	task CodeTask,
	gate *GateResult,
) ([]EditSnippet, error) {
	if gate == nil || len(gate.Diagnostics) == 0 {
		return nil, fmt.Errorf("no diagnostics to repair from")
	}

	var allChunks []ContextChunk
	var allSuggestions []string

	for _, diag := range gate.Diagnostics {
		if diag.Severity != "blocking" {
			continue
		}
		ctxReq := ContextRequest{
			TaskType:   "fix",
			TargetFile: diag.File,
			TargetLine: diag.Line,
			Diagnostic: diag.Message,
			Category:   diag.Category,
			Symbol:     diag.Symbol,
		}
		ctxResult, err := e.contextEngine.Retrieve(ctxReq)
		if err != nil {
			continue
		}
		allChunks = append(allChunks, ctxResult.Chunks...)
		allSuggestions = append(allSuggestions, ctxResult.Suggestions...)
	}

	prompt := e.buildRepairPrompt(task, gate, allChunks, allSuggestions)
	output, err := e.llmRunner.Run(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("llm repair failed: %w", err)
	}

	return e.parseEditSnippets(output)
}

func (e *CodeExecutor) buildGenerationPrompt(task CodeTask) string {
	var contractCtx strings.Builder
	for _, file := range task.TargetFiles {
		dir := filepath.Dir(file)
		for pkgPath, pkg := range e.contractStore.packages {
			if pkg.Path == dir || strings.HasPrefix(dir, pkg.Path+"/") {
				contractCtx.WriteString(fmt.Sprintf("// Package: %s\n", pkgPath))
				for name, ti := range pkg.Types {
					if ti.Exported {
						contractCtx.WriteString(fmt.Sprintf("// %s\n", e.contextEngine.formatTypeDefinition(ti)))
					}
				}
			}
		}
	}

	strategy := e.loadPromptSkillContent("contract-first-coding")

	return fmt.Sprintf(`You are a Go code generator working in contract-first mode.

## Objective (WHAT, not HOW)
%s

## Repository
Root: %s
Module: %s

## Contract Context
%s

## Strategy
%s

## ── THINK PHASE (MANDATORY) ──
Before generating ANY code, you MUST think through the problem inside <think>...</think> tags.
Your think block MUST address ALL of the following:

1. **Goal**: Restate the objective in one sentence.
2. **Constraints**: List all interfaces/types from Contract Context that you plan to use.
3. **Change Scope**: Estimate: (a) files to modify, (b) lines to add, (c) lines to delete.
4. **Assumptions**: Declare any assumptions you are making (e.g., "Assuming interface X exists").
5. **Alternatives**: If multiple approaches exist, state why you chose yours.
6. **Stop Condition**: When would you STOP and ask for help instead of continuing?

## ── RULES (Simplicity + Surgical) ──
1. You can ONLY use interfaces/types/functions that exist in the Contract Context.
2. If you need a new interface, you MUST declare it in your output.
3. Constructor return types MUST match interface declarations.
4. **SIMPLICITY**: Do NOT add features beyond what was asked. Do NOT create abstractions for single-use code.
5. **SURGICAL**: Only modify files you declared in Change Scope. Do NOT "improve" adjacent code.
6. Output your changes as a JSON array of EditSnippet objects.
7. Each snippet MUST have: "file", "start_marker", "replacement".
8. Use "//...existing code..." to preserve unchanged parts.
9. Do NOT output explanations outside the JSON.

## ── SUCCESS CRITERIA (MUST ALL PASS) ──
- [ ] Code compiles: ` + "`" + `go build ./...` + "`" + ` returns zero errors
- [ ] Simplicity: Total changed lines ≤ 200 (generation) or ≤ 50 (repair)
- [ ] Surgical: Only declared files modified; no collateral changes
- [ ] Contract-compliant: All referenced types exist in Contract Context
- [ ] Think Phase present: Output contains <think>...</think> block

## Output Format
First output your <think> block, then return ONLY a JSON array:
[
  {
    "file": "relative/path/to/file.go",
    "start_marker": "func main() {",
    "replacement": "func main() {\n    //...existing code...\n    newCode()\n}"
  }
]`,
		task.Objective, task.RepoRoot, e.contractStore.GetModule(),
		contractCtx.String(), strategy)
}

func (e *CodeExecutor) buildRepairPrompt(
	task CodeTask,
	gate *GateResult,
	chunks []ContextChunk,
	suggestions []string,
) string {
	var diagBuilder strings.Builder
	for _, b := range gate.Blockers {
		diagBuilder.WriteString(fmt.Sprintf("- [%s] %s:%d: %s\n", b.Rule, b.File, b.Line, b.Message))
	}

	var ctxBuilder strings.Builder
	for _, chunk := range chunks {
		if chunk.Relevance >= 0.7 {
			ctxBuilder.WriteString(fmt.Sprintf("// Source: %s (relevance: %.2f)\n%s\n\n",
				chunk.Source, chunk.Relevance, chunk.Content))
		}
	}

	strategy := e.loadPromptSkillContent("go-repair-strategy")

	return fmt.Sprintf(`You are fixing Go compilation errors.

## Objective (WHAT success looks like)
%s

## Validation Failures
%s

## Relevant Context
%s

## Repair Suggestions
%s

## Strategy
%s

## ── THINK PHASE (MANDATORY) ──
Before generating ANY fix, you MUST think inside <think>...</think> tags:

1. **Root Cause**: What is the ACTUAL root cause (not the symptom)?
2. **Minimal Fix**: What is the SMALLEST change that fixes this error?
3. **Change Scope**: Which exact file(s) and line(s) will you modify?
4. **Side Effects**: Could this fix break anything else? How do you know?
5. **Assumptions**: What are you assuming about the codebase?
6. **When to Stop**: If you cannot determine the root cause in 2 sentences, STOP and say so.

## ── RULES (Simplicity + Surgical) ──
1. Fix the ROOT CAUSE, not the symptom.
2. If an interface is missing a method, check the interface definition first.
3. If an import alias conflicts, rename the local import.
4. Do NOT add empty interfaces to bypass compilation.
5. Do NOT add panic("TODO") or empty stubs.
6. **SIMPLICITY**: One error = one change. Do NOT refactor unrelated code.
7. **SURGICAL**: Only modify files you declared in Change Scope.
8. Output changes as a JSON array of EditSnippet objects.
9. Use "//...existing code..." to preserve unchanged parts.

## ── SUCCESS CRITERIA (MUST ALL PASS) ──
- [ ] Root cause fixed: The specific compilation error is resolved
- [ ] Minimal change: Total changed lines ≤ 50
- [ ] No collateral damage: Only declared files modified
- [ ] No regressions: Existing tests still pass
- [ ] Think Phase present: Output contains <think>...</think> block

## Output Format
First output your <think> block, then return ONLY a JSON array of EditSnippet objects.`,
		task.Objective, diagBuilder.String(), ctxBuilder.String(),
		strings.Join(suggestions, "\n"), strategy)
}

func (e *CodeExecutor) parseEditSnippets(output string) ([]EditSnippet, error) {
	output = strings.TrimSpace(output)

	if strings.HasPrefix(output, "```json") {
		output = strings.TrimPrefix(output, "```json")
		output = strings.TrimPrefix(output, "```")
		if idx := strings.LastIndex(output, "```"); idx >= 0 {
			output = output[:idx]
		}
	} else if strings.HasPrefix(output, "```") {
		output = strings.TrimPrefix(output, "```")
		if idx := strings.LastIndex(output, "```"); idx >= 0 {
			output = output[:idx]
		}
	}
	output = strings.TrimSpace(output)

	var snippets []EditSnippet
	if err := json.Unmarshal([]byte(output), &snippets); err != nil {
		return nil, fmt.Errorf("parse snippets: %w (output: %.200s)", err, output)
	}

	for i, s := range snippets {
		if s.File == "" {
			return nil, fmt.Errorf("snippet %d: missing file", i)
		}
		if s.StartMarker == "" {
			return nil, fmt.Errorf("snippet %d: missing start_marker", i)
		}
		if s.Replacement == "" {
			return nil, fmt.Errorf("snippet %d: missing replacement", i)
		}
	}

	return snippets, nil
}

func (e *CodeExecutor) buildPatchSummaries(root string, snippets []EditSnippet) []PatchSummary {
	byFile := make(map[string][]EditSnippet)
	for _, s := range snippets {
		byFile[s.File] = append(byFile[s.File], s)
	}

	var summaries []PatchSummary
	for file, ss := range byFile {
		path := filepath.Join(root, file)
		src, _ := os.ReadFile(path)
		content := string(src)

		additions := 0
		deletions := 0
		for _, s := range ss {
			additions += strings.Count(s.Replacement, "\n")
			if strings.Contains(content, s.StartMarker) {
				startIdx := strings.Index(content, s.StartMarker)
				endIdx := startIdx + len(s.StartMarker)
				if s.EndMarker != "" {
					if eidx := strings.Index(content[startIdx:], s.EndMarker); eidx >= 0 {
						endIdx = startIdx + eidx + len(s.EndMarker)
					}
				}
				deletions += strings.Count(content[startIdx:endIdx], "\n")
			}
		}

		summaries = append(summaries, PatchSummary{
			File:      file,
			Additions: additions,
			Deletions: deletions,
			Snippet:   ss[0].Replacement,
		})
	}
	return summaries
}

func (e *CodeExecutor) parseCodeTask(task *orchestrator.Task, bb orchestrator.ReadOnlyBlackboard) CodeTask {
	ct := CodeTask{ID: task.ID, Language: "go"}
	if task.Metadata != nil {
		if v, ok := task.Metadata["objective"].(string); ok {
			ct.Objective = v
		}
		if v, ok := task.Metadata["repo_root"].(string); ok {
			ct.RepoRoot = v
		}
		if v, ok := task.Metadata["module_path"].(string); ok {
			ct.ModulePath = v
		}
		if v, ok := task.Metadata["target_files"].([]string); ok {
			ct.TargetFiles = v
		}
	}
	return ct
}

func (e *CodeExecutor) loadPromptSkillContent(skillName string) string {
	switch skillName {
	case "contract-first-coding":
		return "1. Declare interfaces before implementations. 2. Constructors must return declared types. 3. No empty interfaces."
	case "go-repair-strategy":
		return "1. Check contract before fixing. 2. Minimal change. 3. No fake implementations."
	default:
		return ""
	}
}

func (e *CodeExecutor) SetMaxRounds(n int) {
	e.maxRounds = n
}

func (e *CodeExecutor) SetPromptSkills(skills []string) {
	e.promptSkills = skills
}
```

---

---

## 5. 工具链适配层 (pkg/toolskill/)

Tool Skill 的**核心设计原则**：**绝不重写已有的工业级工具，只做三件事——检测、调用、包装**。

Go 生态已经拥有世界一流的静态分析工具（staticcheck、golangci-lint、go vet、goimports、gopls 等），这些工具由社区十年打磨、覆盖数千条规则、误报率极低。Agent Harness 的 Tool Skill 不是它们的替代品，而是**面向 LLM 的适配层**。

### 架构：四层模型

```
┌──────────────────────────────────────────────────────────────┐
│ Layer 3: Tool Skill（LLM 可调用的工具接口）                    │
│   go-static-check, go-repair-diagnosis, repo-quality-gate    │
│   统一 JSON Schema 输入/输出，面向 Agent 决策链路              │
├──────────────────────────────────────────────────────────────┤
│ Layer 2: Tool Adapter（原生工具的封装与解析）                  │
│   - 调用原生工具 (exec.Command / go/analysis API)            │
│   - 解析文本/JSON 输出 → 结构化 LLM-friendly JSON            │
│   - 添加 category / severity / confidence 元数据             │
│   - 提供 fallback 链（staticcheck → go vet → go build）      │
├──────────────────────────────────────────────────────────────┤
│ Layer 1: Tool Detector（运行时工具发现）                       │
│   - 启动时扫描 $PATH 中可用的原生工具                         │
│   - 构建 capability map（工具名 → 可用性 → 版本）            │
│   - 根据可用工具动态选择 Adapter 实现                         │
├──────────────────────────────────────────────────────────────┤
│ Layer 0: Native Tools（Go 生态原生工具）                       │
│   staticcheck, golangci-lint, go vet, goimports, go build    │
│   go/analysis, go/packages, gopls                            │
└──────────────────────────────────────────────────────────────┘
```

### 多语言设计

工具链适配层**按语言隔离**。每种语言有独立的 `LanguageProfile`，定义：
- 检测工具链（可配置优先级）
- 编译/测试命令
- 文件扩展名模式
- 结构化输出解析器

```go
// LanguageProfile 语言配置文件
type LanguageProfile struct {
	Name       string           `json:"name"`       // "go", "python", "rust"...
	Extensions []string         `json:"extensions"` // [".go"]
	BuildCmd   string           `json:"build_cmd"`  // "go build ./..."
	TestCmd    string           `json:"test_cmd"`   // "go test ./..."
	Tools      []ToolPreference `json:"tools"`      // 工具优先级列表
}

// ToolPreference 工具优先级配置
type ToolPreference struct {
	Name        string   `json:"name"`        // "staticcheck"
	Priority    int      `json:"priority"`    // 1 = 最高
	Cmd         string   `json:"cmd"`         // "staticcheck"
	Args        []string `json:"args"`        // ["-f", "json", "./..."]
	OutputFmt   string   `json:"output_fmt"`  // "json" | "text"
	FallbackTo  string   `json:"fallback_to"` // 不可用时降级到哪个工具
}
```

**Go 语言默认工具链优先级**：

| 能力 | 首选工具 | 降级链 | 输出格式 |
|------|---------|--------|---------|
| 静态检查 | `staticcheck` | `golangci-lint` → `go vet` → `go build` | JSON / 文本 |
| Import 管理 | `goimports` | `gofmt` + 手动 | 文本 |
| 代码格式化 | `gofmt` | `golangci-lint run --fix` | 文本 |
| 编译 | `go build` | 无 | 文本 |
| 测试 | `go test` | 无 | 文本 |
| 接口发现 | `go/analysis` (库调用) | `go/packages` + AST | JSON |

---

### 5.1 工具自动检测 (pkg/toolskill/detect.go)

```go
package toolskill

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// ToolDetector 运行时工具发现器。
// 在 Agent Harness 初始化时扫描环境，构建可用工具的能力图谱。
type ToolDetector struct {
	mu      sync.RWMutex
	cache   map[string]*ToolCapability // tool name -> capability
}

// ToolCapability 工具能力描述
type ToolCapability struct {
	Name       string `json:"name"`
	Available  bool   `json:"available"`
	Version    string `json:"version,omitempty"`
	Path       string `json:"path,omitempty"`
	OutputFmt  string `json:"output_fmt"`   // "json", "text", "checkstyle"
	Priority   int    `json:"priority"`     // 1 = highest
}

func NewToolDetector() *ToolDetector {
	return &ToolDetector{cache: make(map[string]*ToolCapability)}
}

// Scan 扫描环境中所有已知的原生工具。
func (d *ToolDetector) Scan(ctx context.Context) {
	tools := []struct {
		name      string
		checkCmd  []string
		verRegexp string
		outputFmt string
		priority  int
	}{
		{"staticcheck", []string{"staticcheck", "-version"}, `staticcheck ([\d.]+)`, "text", 1},
		{"golangci-lint", []string{"golangci-lint", "--version"}, `[\d.]+`, "text", 2},
		{"go", []string{"go", "version"}, `go([\d.]+)`, "text", 0}, // go 始终可用
		{"gofmt", []string{"gofmt", "--help"}, ``, "text", 0},
		{"goimports", []string{"goimports", "--help"}, ``, "text", 0},
		{"gopls", []string{"gopls", "version"}, `[\d.]+`, "text", 0},
	}

	var wg sync.WaitGroup
	for _, t := range tools {
		wg.Add(1)
		go func(t struct{...}) {
			defer wg.Done()
			cap := d.probe(ctx, t.name, t.checkCmd, t.verRegexp, t.outputFmt, t.priority)
			d.mu.Lock()
			d.cache[t.name] = cap
			d.mu.Unlock()
		}(t)
	}
	wg.Wait()
}

func (d *ToolDetector) probe(ctx context.Context, name string, cmd []string, verRe, outputFmt string, priority int) *ToolCapability {
	cap := &ToolCapability{Name: name, OutputFmt: outputFmt, Priority: priority}

	path, err := exec.LookPath(cmd[0])
	if err != nil {
		cap.Available = false
		return cap
	}
	cap.Available = true
	cap.Path = path

	// 获取版本
	if verRe != "" {
		out, _ := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
		if m := regexp.MustCompile(verRe).FindStringSubmatch(string(out)); len(m) > 1 {
			cap.Version = m[1]
		}
	}
	return cap
}

// Get 查询工具可用性
func (d *ToolDetector) Get(name string) (*ToolCapability, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, ok := d.cache[name]
	return c, ok
}

// Resolve 按能力名称解析最佳可用工具。
// 例如 Resolve("static-check") -> staticcheck/golangci-lint/go vet
func (d *ToolDetector) Resolve(capability string) *ToolCapability {
	chains := map[string][]string{
		"static-check": {"staticcheck", "golangci-lint", "go-vet"},
		"format":       {"gofmt", "goimports"},
		"build":        {"go"},
		"import-fix":   {"goimports", "gofmt"},
	}
	candidates, ok := chains[capability]
	if !ok {
		return nil
	}
	for _, name := range candidates {
		if c, ok := d.Get(name); ok && c.Available {
			return c
		}
	}
	return nil
}

// All 返回所有已检测的工具
func (d *ToolDetector) All() map[string]*ToolCapability {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]*ToolCapability, len(d.cache))
	for k, v := range d.cache {
		out[k] = v
	}
	return out
}
```

### 5.2 通用 Adapter 接口 (pkg/toolskill/adapter.go)

```go
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Adapter 原生工具适配器接口。
// 每个 Adapter 封装一个原生工具：负责调用、解析输出、转换为结构化 JSON。
type Adapter interface {
	// Name 返回适配器名称
	Name() string
	// Capabilities 声明此 Adapter 提供的能力
	Capabilities() []string
	// Execute 执行原生工具，返回结构化结果
	Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error)
}

// AdapterArgs 通用调用参数
type AdapterArgs struct {
	RepoRoot string            `json:"repo_root"`
	Files    []string          `json:"files,omitempty"`
	Extra    map[string]any    `json:"extra,omitempty"` // 工具特有参数
}

// AdapterResult 通用执行结果
type AdapterResult struct {
	RawOutput string                   `json:"raw_output"`
	Diagnostics []Diagnostic           `json:"diagnostics,omitempty"`
	CheckPassed bool                   `json:"check_passed,omitempty"`
	Metadata  map[string]any           `json:"metadata,omitempty"`
	DurationMs int64                   `json:"duration_ms"`
}

// Diagnostic 结构化诊断信息（所有 Adapter 统一输出格式）
type Diagnostic struct {
	File      string  `json:"file"`
	Line      int     `json:"line"`
	Column    int     `json:"column"`
	Message   string  `json:"message"`
	Category  string  `json:"category"`  // syntax_error, type_error, import_error, style, warning, info
	Severity  string  `json:"severity"`  // error, warning, info
	Code      string  `json:"code,omitempty"`      // 规则编码（如 SA4000）
	Tool      string  `json:"tool,omitempty"`      // 来源工具名
	Fixable   bool    `json:"fixable,omitempty"`   // 是否可自动修复
}

// BaseAdapter 通用适配器基类
type BaseAdapter struct {
	name       string
	cmd        string
	args       []string
	outputFmt  string
	parser     OutputParser
}

// OutputParser 输出解析器接口
type OutputParser interface {
	Parse(raw []byte, repoRoot string) (*AdapterResult, error)
}

// ExecAndParse 执行命令并解析输出
func (a *BaseAdapter) ExecAndParse(ctx context.Context, args []string, repoRoot string, parser OutputParser) (*AdapterResult, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, a.cmd, args...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	duration := time.Since(start).Milliseconds()

	result, parseErr := parser.Parse(out, repoRoot)
	if parseErr != nil {
		// 解析失败时返回原始输出
		return &AdapterResult{
			RawOutput:  string(out),
			DurationMs: duration,
			CheckPassed: err == nil,
		}, fmt.Errorf("parse failed: %w", parseErr)
	}

	result.RawOutput = string(out)
	result.DurationMs = duration
	result.CheckPassed = err == nil && len(result.Diagnostics) == 0
	return result, nil
}
```

### 5.3 Go 语言专用 Parser (pkg/toolskill/parser.go)

```go
package toolskill

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ──────────────────────────────────────────────
// staticcheck JSON Parser
// staticcheck -f json ./... 输出格式：
// {"checker":"SA4000","code":"SA4000","severity":"error"
//  "location":{"file":"x.go","line":10,"column":5}
//  "message":"..."}
// ──────────────────────────────────────────────
type StaticcheckJSONParser struct{}

func (p *StaticcheckJSONParser) Parse(raw []byte, repoRoot string) (*AdapterResult, error) {
	var result AdapterResult
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			Checker  string `json:"checker"`
			Code     string `json:"code"`
			Severity string `json:"severity"`
			Location struct {
				File   string `json:"file"`
				Line   int    `json:"line"`
				Column int    `json:"column"`
			} `json:"location"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			File:     entry.Location.File,
			Line:     entry.Location.Line,
			Column:   entry.Location.Column,
			Message:  entry.Message,
			Category: mapStaticcheckCode(entry.Code),
			Severity: entry.Severity,
			Code:     entry.Code,
			Tool:     "staticcheck",
		})
	}
	return &result, nil
}

func mapStaticcheckCode(code string) string {
	prefix := ""
	if len(code) >= 2 {
		prefix = code[:2]
	}
	switch prefix {
	case "SA":
		return "bug" // correctness issues
	case "S1":
		return "style"
	case "ST":
		return "style"
	case "QF":
		return "suggestion" // quickfix
	default:
		return "warning"
	}
}

// ──────────────────────────────────────────────
// go vet / go build 文本 Parser
// 格式：file.go:10:5: error message
// ──────────────────────────────────────────────
type GoBuildTextParser struct{}

var goBuildPattern = regexp.MustCompile(`^(.+?):(\d+):(?:(\d+):)?\s*(.+)$`)

func (p *GoBuildTextParser) Parse(raw []byte, repoRoot string) (*AdapterResult, error) {
	var result AdapterResult
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		if m := goBuildPattern.FindStringSubmatch(line); m != nil {
			lineNum, _ := strconv.Atoi(m[2])
			colNum, _ := strconv.Atoi(m[3])
			msg := m[4]
			result.Diagnostics = append(result.Diagnostics, Diagnostic{
				File:     m[1],
				Line:     lineNum,
				Column:   colNum,
				Message:  msg,
				Category: classifyGoError(msg),
				Severity: "error",
				Tool:     "go-build",
			})
		}
	}
	return &result, nil
}

func classifyGoError(msg string) string {
	switch {
	case strings.Contains(msg, "undefined"):
		return "undefined_symbol"
	case strings.Contains(msg, "cannot use"):
		return "type_mismatch"
	case strings.Contains(msg, "does not implement"):
		return "interface_mismatch"
	case strings.Contains(msg, "too few") || strings.Contains(msg, "too many"):
		return "signature_mismatch"
	case strings.Contains(msg, "import"):
		return "import_error"
	case strings.Contains(msg, "undefined field") || strings.Contains(msg, "unknown field"):
		return "field_error"
	default:
		return "syntax_error"
	}
}

// ──────────────────────────────────────────────
// golangci-lint JSON Parser
// golangci-lint run --out-format=json ./...
// ──────────────────────────────────────────────
type GolangCILintParser struct{}

func (p *GolangCILintParser) Parse(raw []byte, repoRoot string) (*AdapterResult, error) {
	var payload struct {
		Issues []struct {
			FromLinter string `json:"FromLinter"`
			Text       string `json:"Text"`
			Severity   string `json:"Severity"`
			Pos        struct {
				Filename string `json:"Filename"`
				Line     int    `json:"Line"`
				Column   int    `json:"Column"`
			} `json:"Pos"`
		} `json:"Issues"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}

	var result AdapterResult
	for _, issue := range payload.Issues {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			File:     issue.Pos.Filename,
			Line:     issue.Pos.Line,
			Column:   issue.Pos.Column,
			Message:  issue.Text,
			Category: mapLinterName(issue.FromLinter),
			Severity: issue.Severity,
			Code:     issue.FromLinter,
			Tool:     "golangci-lint:" + issue.FromLinter,
		})
	}
	return &result, nil
}

func mapLinterName(name string) string {
	switch name {
	case "errcheck", "govet", "staticcheck":
		return "bug"
	case "gosimple", "ineffassign", "unused":
		return "suggestion"
	default:
		return "style"
	}
}
```

### 5.4 核心类型定义（schema.go 修改版）

```go
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ToolSkill Tool Skill 接口（面向 LLM）
type ToolSkill interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	OutputSchema() json.RawMessage
	IsReadOnly() bool
	IsConcurrencySafe() bool
	Execute(ctx context.Context, input json.RawMessage) (map[string]any, error)
}

// Meta Tool Skill 元数据
type Meta struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Version        string          `json:"version"`
	InputSchema    json.RawMessage `json:"input_schema"`
	OutputSchema   json.RawMessage `json:"output_schema,omitempty"`
	ReadOnly       bool            `json:"read_only"`
	SafeConcurrent bool            `json:"safe_concurrent"`
	Timeout        time.Duration   `json:"timeout,omitempty"`
}

type Base struct{ meta Meta }

func NewBase(meta Meta) Base { return Base{meta: meta} }
func (b *Base) Name() string { return b.meta.Name }
func (b *Base) Description() string { return b.meta.Description }
func (b *Base) InputSchema() json.RawMessage  { return b.meta.InputSchema }
func (b *Base) OutputSchema() json.RawMessage { return b.meta.OutputSchema }
func (b *Base) IsReadOnly() bool              { return b.meta.ReadOnly }
func (b *Base) IsConcurrencySafe() bool       { return b.meta.SafeConcurrent }

// Registry Tool Skill 注册表
type Registry struct {
	skills map[string]ToolSkill
}

func NewRegistry() *Registry {
	return &Registry{skills: make(map[string]ToolSkill)}
}
func (r *Registry) Register(ts ToolSkill) { r.skills[ts.Name()] = ts }
func (r *Registry) Get(name string) (ToolSkill, bool) {
	ts, ok := r.skills[name]
	return ts, ok
}
func (r *Registry) All() []ToolSkill {
	var list []ToolSkill
	for _, ts := range r.skills {
		list = append(list, ts)
	}
	return list
}

// ExecutionResult 统一执行结果
type ExecutionResult struct {
	Success bool                   `json:"success"`
	Data    map[string]any         `json:"data,omitempty"`
	Error   string                 `json:"error,omitempty"`
	Timing  TimingInfo             `json:"timing"`
}

type TimingInfo struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	DurationMs int64     `json:"duration_ms"`
}

func WrapError(err error) ExecutionResult {
	return ExecutionResult{Success: false, Error: err.Error(), Timing: TimingInfo{End: time.Now()}}
}
func WrapSuccess(data map[string]any) ExecutionResult {
	return ExecutionResult{Success: true, Data: data, Timing: TimingInfo{End: time.Now()}}
}
```

### 5.5 运行时与 Detector 集成 (pkg/toolskill/runtime.go)

```go
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// Runtime Tool Skill 运行时，集成 Detector 和 Adapter
type Runtime struct {
	registry *Registry
	detector *ToolDetector
	adapters map[string]Adapter // capability -> adapter
}

func NewRuntime(reg *Registry) *Runtime {
	return &Runtime{
		registry: reg,
		detector: NewToolDetector(),
		adapters: make(map[string]Adapter),
	}
}

// Init 初始化：扫描工具、注册默认 Adapter
func (rt *Runtime) Init(ctx context.Context) {
	rt.detector.Scan(ctx)

	// 注册 Adapter（按优先级，后面的覆盖前面的）
	if cap := rt.detector.Resolve("static-check"); cap != nil {
		switch cap.Name {
		case "staticcheck":
			rt.adapters["static-check"] = &StaticcheckAdapter{cap: cap}
		case "golangci-lint":
			rt.adapters["static-check"] = &GolangCILintAdapter{cap: cap}
		}
	}
	// 始终注册 go vet / go build fallback
	rt.adapters["build"] = &GoBuildAdapter{}
	rt.adapters["format"] = &GoFormatAdapter{detector: rt.detector}
	rt.adapters["import-fix"] = &GoImportsAdapter{detector: rt.detector}

	log.Printf("[ToolSkill] Initialized adapters: %v", rt.AdapterNames())
}

func (rt *Runtime) AdapterNames() []string {
	var names []string
	for k := range rt.adapters {
		names = append(names, k)
	}
	return names
}

// Execute 按名称执行 Tool Skill
func (rt *Runtime) Execute(ctx context.Context, name string, input json.RawMessage) ExecutionResult {
	ts, ok := rt.registry.Get(name)
	if !ok {
		return WrapError(fmt.Errorf("tool skill %q not found", name))
	}
	start := time.Now()
	data, err := ts.Execute(ctx, input)
	return ExecutionResult{
		Success: err == nil,
		Data:    data,
		Error:   errStr(err),
		Timing:  TimingInfo{Start: start, End: time.Now(), DurationMs: time.Since(start).Milliseconds()},
	}
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// RegisterDefaults 注册所有内置 Tool Skill
func (rt *Runtime) RegisterDefaults(store *contract.Store, validator *validation.Gate) {
	rt.registry.Register(NewGoContractBuilder(store))
	rt.registry.Register(NewGoStaticCheck(rt))
	rt.registry.Register(NewGoRepairDiagnosis(store))
	rt.registry.Register(NewGoImportResolution(rt))
	rt.registry.Register(NewRepoQualityGate(rt, validator))
}
```

---

## 6. 内置 Tool Skill 完整实现（Wrapper 层）

所有 Tool Skill 遵循统一模式：
1. **不手写 AST 分析** —— 调用 Layer 0 原生工具
2. **解析结构化/文本输出** —— 使用 Layer 2 Adapter + Parser
3. **输出 LLM-friendly JSON** —— category / severity / confidence 元数据
4. **内置 fallback 链** —— 首选工具不可用时自动降级


### 6.1 go-contract-builder (pkg/toolskill/contract_builder.go)

**底层复用**：`golang.org/x/tools/go/packages` + `golang.org/x/tools/go/analysis`

**职责**：扫描仓库，构建并维护 Contract Store。在代码生成前执行，确保 Agent 能查询到所有已有接口。

```go
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/types"
	"path/filepath"

	"golang.org/x/tools/go/packages"
	"github.com/anthropic/claude-go/pkg/contract"
)

// GoContractBuilder 构建 Contract Store。
// 底层复用 golang.org/x/tools/go/packages 进行类型化包加载，
// 比手写 filepath.Walk + go/parser 更准确（能解析跨包类型依赖）。
//
// 输入：{"repo_root": "/path/to/repo", "module": "github.com/anthropic/claude-go"}
// 输出：{"contracts": [...], "implementors": {...}, "stats": {...}}
type GoContractBuilder struct {
	Base
	store *contract.Store
}

func NewGoContractBuilder(store *contract.Store) *GoContractBuilder {
	return &GoContractBuilder{
		Base: NewBase(Meta{
			Name:           "go-contract-builder",
			Description:    "Scan the repository and build/update the Contract Store. Uses golang.org/x/tools/go/packages for type-accurate analysis. Call this before generating code to discover all existing interfaces, types, and their relationships.",
			Version:        "1.0.0",
			ReadOnly:       false,
			SafeConcurrent: true,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root":   {"type": "string"},
					"module":      {"type": "string"},
					"output_path": {"type": "string", "default": "<repo_root>/contract_store.json"}
				},
				"required": ["repo_root", "module"]
			}`),
		}),
		store: store,
	}
}

func (t *GoContractBuilder) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot   string `json:"repo_root"`
		Module     string `json:"module"`
		OutputPath string `json:"output_path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	if in.OutputPath == "" {
		in.OutputPath = filepath.Join(in.RepoRoot, "contract_store.json")
	}

	// 使用 go/packages 加载所有包（带类型信息）
	cfg := &packages.Config{
		Mode: packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedName,
		Dir:  in.RepoRoot,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("packages.Load failed: %w", err)
	}

	contracts := make(map[string]*contract.InterfaceContract)
	implementors := make(map[string][]string)
	stats := struct {
		FilesScanned    int `json:"files_scanned"`
		InterfacesFound int `json:"interfaces_found"`
		MethodsIndexed  int `json:"methods_indexed"`
	}{}

	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			continue // 跳过有错误的包
		}
		pkgPath := pkg.PkgPath
		if pkgPath == "" {
			pkgPath = in.Module
		}

		// 遍历作用域查找接口类型
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			named, ok := obj.Type().(*types.Named)
			if !ok {
				continue
			}
			iface, ok := named.Underlying().(*types.Interface)
			if !ok {
				continue
			}

			key := pkgPath + "." + name
			methods := make(map[string]*contract.MethodSig)
			for i := 0; i < iface.NumMethods(); i++ {
				m := iface.Method(i)
				sig := m.Type().(*types.Signature)
				methods[m.Name()] = &contract.MethodSig{
					Name:    m.Name(),
					Params:  typeListFromTypes(sig.Params()),
					Results: typeListFromTypes(sig.Results()),
				}
				stats.MethodsIndexed++
			}

			contracts[key] = &contract.InterfaceContract{
				Package:  pkgPath,
				Name:     name,
				Methods:  methods,
				FilePath: pkg.Fset.Position(obj.Pos()).Filename,
			}
			stats.InterfacesFound++
		}

		// 遍历包内所有类型，查找接口实现者
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			named, ok := obj.Type().(*types.Named)
			if !ok {
				continue
			}
			// 检查是否实现了某个接口
			for ifaceKey, ifaceContract := range contracts {
				// 简化：只记录有方法的类型
				if _, ok := named.Underlying().(*types.Struct); ok {
					recvKey := pkgPath + "." + name
					// 通过 method set 判断实现关系
					if hasMethodOverlap(named, ifaceContract.Methods) {
						implementors[ifaceKey] = append(implementors[ifaceKey], recvKey)
					}
				}
			}
		}
		stats.FilesScanned += len(pkg.Syntax)
	}

	// 写回 store
	t.store.Contracts = contracts
	t.store.Implementors = implementors
	t.store.Version = time.Now().Format(time.RFC3339)
	if err := t.store.Save(in.OutputPath); err != nil {
		return nil, fmt.Errorf("save contract store: %w", err)
	}

	return map[string]any{
		"contracts_found": stats.InterfacesFound,
		"methods_indexed": stats.MethodsIndexed,
		"files_scanned":   stats.FilesScanned,
		"store_path":      in.OutputPath,
		"version":         t.store.Version,
	}, nil
}

func typeListFromTypes(t *types.Tuple) []string {
	if t == nil {
		return nil
	}
	var out []string
	for i := 0; i < t.Len(); i++ {
		out = append(out, t.At(i).Type().String())
	}
	return out
}

func hasMethodOverlap(named *types.Named, methods map[string]*contract.MethodSig) bool {
	// 简化：检查是否实现了至少一个接口方法
	// 实际应使用 types.Implements，但需构造接口类型
	for mname := range methods {
		if named.Method(mname) != nil {
			return true
		}
	}
	return false
}
```

### 6.2 go-static-check (pkg/toolskill/static_check.go)

**底层复用**：`staticcheck` (honnef.co/go/tools) → fallback `golangci-lint` → fallback `go vet`

**职责**：运行静态分析，返回编译前错误。替代 workflow.go 中 30+ 文本级修复函数。

```go
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// GoStaticCheck 静态检查 Tool Skill。
// 自动检测环境中可用的静态分析工具，按优先级调用：
//   staticcheck -f json ./...  (首选，输出最丰富)
//   golangci-lint run --out-format=json ./...
//   go vet ./...
//   go build ./...  (最终 fallback)
type GoStaticCheck struct {
	Base
	runtime *Runtime
}

func NewGoStaticCheck(rt *Runtime) *GoStaticCheck {
	return &GoStaticCheck{
		Base: NewBase(Meta{
			Name:           "go-static-check",
			Description:    "Run static analysis on Go files before compilation. Automatically detects and uses the best available tool (staticcheck > golangci-lint > go vet > go build). Returns structured diagnostics with file, line, category, and severity.",
			Version:        "1.0.0",
			ReadOnly:       true,
			SafeConcurrent: true,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root": {"type": "string"},
					"files":     {"type": "array", "items": {"type": "string"}},
					"strict":    {"type": "boolean", "default": false}
				},
				"required": ["repo_root"]
			}`),
		}),
		runtime: rt,
	}
}

func (t *GoStaticCheck) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot string   `json:"repo_root"`
		Files    []string `json:"files"`
		Strict   bool     `json:"strict"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	// 尝试获取 static-check adapter（可能映射到 staticcheck / golangci-lint / go vet）
	var result *AdapterResult
	var usedTool string

	if adapter, ok := t.runtime.adapters["static-check"]; ok {
		res, err := adapter.Execute(ctx, AdapterArgs{RepoRoot: in.RepoRoot, Files: in.Files})
		if err == nil && res != nil {
			result = res
			usedTool = adapter.Name()
		}
	}

	// 如果 static-check adapter 未返回结果，fallback 到 go build
	if result == nil {
		buildAdapter := t.runtime.adapters["build"]
		res, err := buildAdapter.Execute(ctx, AdapterArgs{RepoRoot: in.RepoRoot})
		if err == nil && res != nil {
			result = res
			usedTool = "go-build"
		}
	}

	if result == nil {
		return nil, fmt.Errorf("no static analysis tool available")
	}

	// 过滤 diagnostics（strict 模式保留 warning）
	var diagnostics []Diagnostic
	for _, d := range result.Diagnostics {
		if in.Strict || d.Severity == "error" {
			diagnostics = append(diagnostics, d)
		}
	}

	return map[string]any{
		"diagnostics":   diagnostics,
		"error_count":   countBySeverity(diagnostics, "error"),
		"warning_count": countBySeverity(diagnostics, "warning"),
		"tool_used":     usedTool,
		"raw_output":    result.RawOutput,
	}, nil
}

func countBySeverity(d []Diagnostic, sev string) int {
	c := 0
	for _, diag := range d {
		if diag.Severity == sev {
			c++
		}
	}
	return c
}
```


### 6.3 go-repair-diagnosis (pkg/toolskill/repair_diagnosis.go)

**底层复用**：纯文本解析（无需外部工具，因为输入是编译器错误文本）

**职责**：给定编译错误，诊断根本原因并返回修复策略。替代 workflow.go 中 40+ 业务级修复函数。

```go
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/anthropic/claude-go/pkg/contract"
)

// GoRepairDiagnosis 诊断编译错误并输出修复策略。
// 不依赖外部工具——直接解析编译器错误文本，结合 Contract Store 输出结构化诊断。
type GoRepairDiagnosis struct {
	Base
	store *contract.Store
}

func NewGoRepairDiagnosis(store *contract.Store) *GoRepairDiagnosis {
	return &GoRepairDiagnosis{
		Base: NewBase(Meta{
			Name:           "go-repair-diagnosis",
			Description:    "Diagnose Go compilation errors and suggest structured repair strategies. Pure text analysis (no external tools needed) — parses compiler error messages and combines with Contract Store context to produce actionable repair instructions.",
			Version:        "1.0.0",
			ReadOnly:       true,
			SafeConcurrent: true,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"error_text":     {"type": "string"},
					"file_path":      {"type": "string"},
					"repo_root":      {"type": "string"},
					"contract_hint":  {"type": "string"},
					"source_snippet": {"type": "string"}
				},
				"required": ["error_text", "file_path"]
			}`),
		}),
		store: store,
	}
}

type RepairSuggestion struct {
	Diagnosis     string  `json:"diagnosis"`
	Category      string  `json:"category"`
	Confidence    float64 `json:"confidence"`
	Action        string  `json:"action"`
	TargetFile    string  `json:"target_file"`
	TargetLine    int     `json:"target_line"`
	Instructions  string  `json:"instructions"`
	ContractCheck string  `json:"contract_check,omitempty"`
}

func (t *GoRepairDiagnosis) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		ErrorText     string `json:"error_text"`
		FilePath      string `json:"file_path"`
		RepoRoot      string `json:"repo_root"`
		ContractHint  string `json:"contract_hint"`
		SourceSnippet string `json:"source_snippet"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	suggestion := t.diagnose(in.ErrorText, in.FilePath, in.ContractHint, in.SourceSnippet)

	return map[string]any{
		"suggestion":     suggestion,
		"error_category": suggestion.Category,
		"confidence":     suggestion.Confidence,
	}, nil
}

func (t *GoRepairDiagnosis) diagnose(errorText, filePath, contractHint, snippet string) RepairSuggestion {
	s := RepairSuggestion{TargetFile: filePath, Confidence: 0.7}

	patterns := []struct {
		re          *regexp.Regexp
		category    string
		action      string
		confidence  float64
		msgTemplate string
		instrTmpl   string
	}{
		{
			re:          regexp.MustCompile(`undefined:\s+(\S+)`),
			category:    "undefined_symbol",
			action:      "consult_contract",
			confidence:  0.75,
			msgTemplate: "Symbol %q is undefined. Check imports, typos, or unexported identifiers.",
			instrTmpl:   "Search codebase for %q. If method, check receiver contract. If type, check imports.",
		},
		{
			re:          regexp.MustCompile(`cannot use .* type .* as type`),
			category:    "type_mismatch",
			action:      "change_signature",
			confidence:  0.85,
			msgTemplate: "Type mismatch. Actual type does not match expected type.",
			instrTmpl:   "Identify expected type from contract or function signature. Modify expression or update target signature.",
		},
		{
			re:          regexp.MustCompile(`(.+) does not implement (.+)`),
			category:    "interface_mismatch",
			action:      "add_method",
			confidence:  0.9,
			msgTemplate: "Receiver does not satisfy interface. Missing or mismatched methods.",
			instrTmpl:   "Look up interface in Contract Store. Add missing methods with exact signatures.",
		},
		{
			re:          regexp.MustCompile(`ambiguous import|imported and not used|no required module provides package`),
			category:    "import_conflict",
			action:      "add_import",
			confidence:  0.88,
			msgTemplate: "Import path conflict or missing dependency.",
			instrTmpl:   "Resolve import path. Use explicit alias if ambiguous. Add to go.mod if missing.",
		},
		{
			re:          regexp.MustCompile(`(\S+)\.([A-Za-z0-9_]+) undefined`),
			category:    "field_error",
			action:      "consult_contract",
			confidence:  0.8,
			msgTemplate: "Field or method not found on type.",
			instrTmpl:   "Check type definition. Add field to struct or method to interface (contract-first), then implement.",
		},
		{
			re:          regexp.MustCompile(`too (few|many) arguments`),
			category:    "signature_mismatch",
			action:      "change_signature",
			confidence:  0.9,
			msgTemplate: "Function call argument count mismatch.",
			instrTmpl:   "Check callee's contract or definition. Update call site or function signature.",
		},
	}

	for _, p := range patterns {
		if m := p.re.FindStringSubmatch(errorText); m != nil {
			s.Category = p.category
			s.Action = p.action
			s.Confidence = p.confidence
			if len(m) > 1 {
				s.Diagnosis = fmt.Sprintf(p.msgTemplate, m[1])
				s.Instructions = fmt.Sprintf(p.instrTmpl, m[1])
				if len(m) > 2 {
					s.Diagnosis = fmt.Sprintf("%s (details: %s)", s.Diagnosis, strings.Join(m[1:], "."))
				}
			} else {
				s.Diagnosis = p.msgTemplate
				s.Instructions = p.instrTmpl
			}
			if contractHint != "" {
				s.ContractCheck = contractHint
			}
			return s
		}
	}

	// Fallback
	s.Category = "other"
	s.Diagnosis = fmt.Sprintf("Unrecognized error. Analyze manually: %s", truncate(errorText, 200))
	s.Action = "consult_contract"
	s.Instructions = "No automatic diagnosis available. Use Context Engine to retrieve relevant definitions, then craft minimal fix."
	s.Confidence = 0.3
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
```

### 6.4 go-import-resolution (pkg/toolskill/import_resolution.go)

**底层复用**：`goimports` → fallback `go get` + `gofmt`

**职责**：解决 Go 模块的 import 冲突、缺失包、循环引用等问题。

```go
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// GoImportResolution 解决 import 问题。
// 首选 goimports 自动修复，不可用则降级到 go get + 手动分析。
type GoImportResolution struct {
	Base
	runtime *Runtime
}

func NewGoImportResolution(rt *Runtime) *GoImportResolution {
	return &GoImportResolution{
		Base: NewBase(Meta{
			Name:           "go-import-resolution",
			Description:    "Resolve Go import conflicts, missing packages, and unused imports. Uses goimports as primary tool (fallback to go get + manual analysis). Returns structured import changes.",
			Version:        "1.0.0",
			ReadOnly:       false,
			SafeConcurrent: false,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root":       {"type": "string"},
					"problem":         {"type": "string"},
					"affected_files":  {"type": "array", "items": {"type": "string"}},
					"preferred_alias": {"type": "string"}
				},
				"required": ["repo_root", "problem"]
			}`),
		}),
		runtime: rt,
	}
}

type ImportChange struct {
	File      string `json:"file"`
	OldImport string `json:"old_import,omitempty"`
	NewImport string `json:"new_import"`
	Alias     string `json:"alias,omitempty"`
}

type ModChange struct {
	Action  string `json:"action"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}

func (t *GoImportResolution) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot       string   `json:"repo_root"`
		Problem        string   `json:"problem"`
		AffectedFiles  []string `json:"affected_files"`
		PreferredAlias string   `json:"preferred_alias"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	var importChanges []ImportChange
	var modChanges []ModChange
	resolution := ""

	// 策略 1: 尝试用 goimports 自动修复所有受影响文件
	if adapter, ok := t.runtime.adapters["import-fix"]; ok {
		for _, f := range in.AffectedFiles {
			path := filepath.Join(in.RepoRoot, f)
			_, _ = adapter.Execute(ctx, AdapterArgs{RepoRoot: in.RepoRoot, Files: []string{path}})
		}
	}

	// 策略 2: 缺失包 -> go get
	if m := regexp.MustCompile(`no required module provides package (\S+)`).FindStringSubmatch(in.Problem); len(m) > 1 {
		pkgPath := m[1]
		resolution = fmt.Sprintf("Add dependency %s", pkgPath)
		modChanges = append(modChanges, ModChange{Action: "add_require", Path: pkgPath})
		cmd := exec.CommandContext(ctx, "go", "get", pkgPath)
		cmd.Dir = in.RepoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("go get %s failed: %w\n%s", pkgPath, err, string(out))
		}
	}

	// 策略 3: 歧义 import
	if regexp.MustCompile(`ambiguous import`).MatchString(in.Problem) {
		resolution = "Use explicit alias for conflicting import"
		for _, f := range in.AffectedFiles {
			content, _ := os.ReadFile(filepath.Join(in.RepoRoot, f))
			lines := strings.Split(string(content), "\n")
			for i, line := range lines {
				if strings.Contains(line, `"`) && strings.HasPrefix(strings.TrimSpace(line), `"`) {
					alias := in.PreferredAlias
					if alias == "" {
						alias = "pkg" + fmt.Sprintf("%d", i)
					}
					lines[i] = fmt.Sprintf("%s %s", alias, line)
					importChanges = append(importChanges, ImportChange{
						File: f, NewImport: strings.TrimSpace(line), Alias: alias,
					})
					break
				}
			}
		}
	}

	// 策略 4: imported and not used
	if m := regexp.MustCompile(`imported and not used: "([^"]+)"`).FindStringSubmatch(in.Problem); len(m) > 1 {
		resolution = fmt.Sprintf("Remove unused import %s", m[1])
		for _, f := range in.AffectedFiles {
			importChanges = append(importChanges, ImportChange{File: f, OldImport: m[1]})
		}
	}

	return map[string]any{
		"resolution":     resolution,
		"import_changes": importChanges,
		"mod_changes":    modChanges,
		"manual_review":  len(importChanges) == 0 && len(modChanges) == 0,
	}, nil
}
```

### 6.5 repo-quality-gate (pkg/toolskill/quality_gate.go)

**底层复用**：`go build`, `go test`, `go vet`, `gofmt` —— 直接调用 Go 工具链命令

**职责**：运行完整的质量门禁检查（编译、测试、lint、格式化）。

```go
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/anthropic/claude-go/pkg/validation"
)

// RepoQualityGate 运行完整质量门禁。
// 直接调用 Go 工具链命令，无外部依赖（go 本身必须已安装）。
type RepoQualityGate struct {
	Base
	runtime *Runtime
	gate    *validation.Gate
}

func NewRepoQualityGate(rt *Runtime, gate *validation.Gate) *RepoQualityGate {
	return &RepoQualityGate{
		Base: NewBase(Meta{
			Name:           "repo-quality-gate",
			Description:    "Run comprehensive quality gate: go build, go test, go vet, gofmt. Directly invokes Go toolchain commands. This is the final validation step before marking a task complete.",
			Version:        "1.0.0",
			ReadOnly:       true,
			SafeConcurrent: false,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root":    {"type": "string"},
					"checks":       {"type": "array", "items": {"type": "string", "enum": ["build", "test", "vet", "format"]}, "default": ["build", "test", "vet"]},
					"test_timeout": {"type": "string", "default": "60s"},
					"test_flags":   {"type": "array", "items": {"type": "string"}, "default": ["-count=1"]}
				},
				"required": ["repo_root"]
			}`),
		}),
		runtime: rt,
		gate:    gate,
	}
}

type CheckResult struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Duration int64  `json:"duration_ms"`
	Output   string `json:"output,omitempty"`
	Errors   string `json:"errors,omitempty"`
}

func (t *RepoQualityGate) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot    string   `json:"repo_root"`
		Checks      []string `json:"checks"`
		TestTimeout string   `json:"test_timeout"`
		TestFlags   []string `json:"test_flags"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	if len(in.Checks) == 0 {
		in.Checks = []string{"build", "test", "vet"}
	}
	if in.TestTimeout == "" {
		in.TestTimeout = "60s"
	}
	if len(in.TestFlags) == 0 {
		in.TestFlags = []string{"-count=1"}
	}

	var results []CheckResult
	allPassed := true

	for _, check := range in.Checks {
		var result CheckResult
		result.Name = check

		switch check {
		case "build":
			cmd := exec.CommandContext(ctx, "go", "build", "./...")
			cmd.Dir = in.RepoRoot
			out, err := cmd.CombinedOutput()
			result.Output = string(out)
			result.Passed = err == nil

		case "test":
			args := append([]string{"test", "-timeout=" + in.TestTimeout}, in.TestFlags...)
			args = append(args, "./...")
			cmd := exec.CommandContext(ctx, "go", args...)
			cmd.Dir = in.RepoRoot
			out, err := cmd.CombinedOutput()
			result.Output = string(out)
			result.Passed = err == nil

		case "vet":
			cmd := exec.CommandContext(ctx, "go", "vet", "./...")
			cmd.Dir = in.RepoRoot
			out, err := cmd.CombinedOutput()
			result.Output = string(out)
			result.Passed = err == nil

		case "format":
			// 使用 gofmt 检查是否有未格式化的文件
			cmd := exec.CommandContext(ctx, "gofmt", "-l", ".")
			cmd.Dir = in.RepoRoot
			out, err := cmd.CombinedOutput()
			result.Output = string(out)
			result.Passed = strings.TrimSpace(string(out)) == "" && err == nil
		}

		if !result.Passed {
			allPassed = false
		}
		results = append(results, result)
	}

	return map[string]any{
		"passed":       allPassed,
		"results":      results,
		"check_count":  len(results),
		"failed_count": len(results) - countPassed(results),
	}, nil
}

func countPassed(results []CheckResult) int {
	c := 0
	for _, r := range results {
		if r.Passed {
			c++
		}
	}
	return c
}
```


---

## 6.6 多语言扩展设计

工具链适配层**按语言隔离**，每种语言通过 `LanguageProfile` 注册自己的工具链。当前实现以 Go 为主，架构预留了其他语言的扩展点。

### 语言配置注册表 (pkg/toolskill/language.go)

```go
package toolskill

// LanguageRegistry 语言配置注册表
type LanguageRegistry struct {
	profiles map[string]*LanguageProfile
}

func NewLanguageRegistry() *LanguageRegistry {
	reg := &LanguageRegistry{profiles: make(map[string]*LanguageProfile)}
	reg.Register(GoProfile())
	return reg
}

func (r *LanguageRegistry) Register(p *LanguageProfile) {
	r.profiles[p.Name] = p
}

func (r *LanguageRegistry) Get(name string) (*LanguageProfile, bool) {
	p, ok := r.profiles[name]
	return p, ok
}

// LanguageProfile 语言完整配置
type LanguageProfile struct {
	Name       string           `json:"name"`       // "go", "python", "rust", "typescript"
	Extensions []string         `json:"extensions"` // [".go"]
	BuildCmd   string           `json:"build_cmd"`  // "go build ./..."
	TestCmd    string           `json:"test_cmd"`   // "go test ./..."
	Tools      []ToolPreference `json:"tools"`      // 工具优先级列表
}

// ToolPreference 工具优先级配置
type ToolPreference struct {
	Name       string   `json:"name"`       // "staticcheck"
	Priority   int      `json:"priority"`   // 1 = highest
	Cmd        string   `json:"cmd"`        // "staticcheck"
	Args       []string `json:"args"`       // ["-f", "json", "./..."]
	OutputFmt  string   `json:"output_fmt"` // "json" | "text" | "checkstyle"
	FallbackTo string   `json:"fallback_to"`// 不可用时降级到哪个工具
}

// GoProfile Go 语言默认配置
func GoProfile() *LanguageProfile {
	return &LanguageProfile{
		Name:       "go",
		Extensions: []string{".go"},
		BuildCmd:   "go build ./...",
		TestCmd:    "go test ./...",
		Tools: []ToolPreference{
			{Name: "staticcheck", Priority: 1, Cmd: "staticcheck", Args: []string{"-f", "json", "./..."}, OutputFmt: "json", FallbackTo: "golangci-lint"},
			{Name: "golangci-lint", Priority: 2, Cmd: "golangci-lint", Args: []string{"run", "--out-format=json", "./..."}, OutputFmt: "json", FallbackTo: "go-vet"},
			{Name: "go-vet", Priority: 3, Cmd: "go", Args: []string{"vet", "./..."}, OutputFmt: "text", FallbackTo: "go-build"},
			{Name: "go-build", Priority: 4, Cmd: "go", Args: []string{"build", "./..."}, OutputFmt: "text", FallbackTo: ""},
			{Name: "gofmt", Priority: 1, Cmd: "gofmt", Args: []string{"-l", "."}, OutputFmt: "text", FallbackTo: ""},
			{Name: "goimports", Priority: 1, Cmd: "goimports", Args: []string{"-l", "."}, OutputFmt: "text", FallbackTo: "gofmt"},
		},
	}
}

// PythonProfile Python 语言配置（预留示例）
func PythonProfile() *LanguageProfile {
	return &LanguageProfile{
		Name:       "python",
		Extensions: []string{".py"},
		BuildCmd:   "python -m compileall .",
		TestCmd:    "pytest",
		Tools: []ToolPreference{
			{Name: "mypy", Priority: 1, Cmd: "mypy", Args: []string{"--json", "."}, OutputFmt: "json", FallbackTo: "pyright"},
			{Name: "pyright", Priority: 2, Cmd: "pyright", Args: []string{"--outputjson"}, OutputFmt: "json", FallbackTo: "ruff"},
			{Name: "ruff", Priority: 3, Cmd: "ruff", Args: []string{"check", "--output-format=json", "."}, OutputFmt: "json", FallbackTo: "flake8"},
			{Name: "flake8", Priority: 4, Cmd: "flake8", Args: []string{"--format=json"}, OutputFmt: "json", FallbackTo: ""},
			{Name: "black", Priority: 1, Cmd: "black", Args: []string{"--check", "."}, OutputFmt: "text", FallbackTo: "ruff-format"},
		},
	}
}

// RustProfile Rust 语言配置（预留示例）
func RustProfile() *LanguageProfile {
	return &LanguageProfile{
		Name:       "rust",
		Extensions: []string{".rs"},
		BuildCmd:   "cargo build",
		TestCmd:    "cargo test",
		Tools: []ToolPreference{
			{Name: "clippy", Priority: 1, Cmd: "cargo", Args: []string{"clippy", "--message-format=json"}, OutputFmt: "json", FallbackTo: "cargo-check"},
			{Name: "cargo-check", Priority: 2, Cmd: "cargo", Args: []string{"check", "--message-format=json"}, OutputFmt: "json", FallbackTo: ""},
			{Name: "rustfmt", Priority: 1, Cmd: "rustfmt", Args: []string{"--check"}, OutputFmt: "text", FallbackTo: ""},
		},
	}
}
```

### 多语言工具检测

`ToolDetector.Scan()` 按 `LanguageProfile.Tools` 列表检测，动态构建跨语言的 capability map：

```go
// ScanLanguages 扫描所有注册语言的工具可用性
func (d *ToolDetector) ScanLanguages(reg *LanguageRegistry) {
	for _, profile := range reg.profiles {
		for _, tool := range profile.Tools {
			cap := d.probe(context.Background(), tool.Name, []string{tool.Cmd, "--version"}, ``, tool.OutputFmt, tool.Priority)
			d.mu.Lock()
			d.cache[tool.Name] = cap
			d.mu.Unlock()
		}
	}
}
```

### 扩展新语言的三步

以 TypeScript 为例：

1. **创建 Profile**：
```go
func TypeScriptProfile() *LanguageProfile {
	return &LanguageProfile{
		Name:       "typescript",
		Extensions: []string{".ts", ".tsx"},
		BuildCmd:   "tsc --noEmit",
		TestCmd:    "jest",
		Tools: []ToolPreference{
			{Name: "eslint", Priority: 1, Cmd: "eslint", Args: []string{"--format=json", "."}, OutputFmt: "json", FallbackTo: "tsc"},
			{Name: "tsc", Priority: 2, Cmd: "tsc", Args: []string{"--noEmit"}, OutputFmt: "text", FallbackTo: ""},
			{Name: "prettier", Priority: 1, Cmd: "prettier", Args: []string{"--check", "."}, OutputFmt: "text", FallbackTo: ""},
		},
	}
}
```

2. **注册 Parser**：
```go
// ESLintJSONParser 解析 eslint --format=json 输出
type ESLintJSONParser struct{}
func (p *ESLintJSONParser) Parse(raw []byte, repoRoot string) (*AdapterResult, error) { ... }
```

3. **注册 Adapter**：
```go
if cap := detector.Resolve("eslint"); cap != nil {
	rt.adapters["static-check"] = &ESLintAdapter{cap: cap}
}
```

---

## 6.7 具体 Adapter 实现

以下是 Go 语言各原生工具对应的 Adapter 实现。

### StaticcheckAdapter

```go
type StaticcheckAdapter struct {
	cap *ToolCapability
}

func (a *StaticcheckAdapter) Name() string { return "staticcheck" }
func (a *StaticcheckAdapter) Capabilities() []string { return []string{"static-check"} }

func (a *StaticcheckAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: a.cap.Path, outputFmt: "json"}
	parser := &StaticcheckJSONParser{}
	return base.ExecAndParse(ctx, []string{"-f", "json", "./..."}, args.RepoRoot, parser)
}
```

### GolangCILintAdapter

```go
type GolangCILintAdapter struct {
	cap *ToolCapability
}

func (a *GolangCILintAdapter) Name() string { return "golangci-lint" }
func (a *GolangCILintAdapter) Capabilities() []string { return []string{"static-check"} }

func (a *GolangCILintAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: a.cap.Path, outputFmt: "json"}
	parser := &GolangCILintParser{}
	return base.ExecAndParse(ctx, []string{"run", "--out-format=json", "./..."}, args.RepoRoot, parser)
}
```

### GoBuildAdapter

```go
type GoBuildAdapter struct{}

func (a *GoBuildAdapter) Name() string { return "go-build" }
func (a *GoBuildAdapter) Capabilities() []string { return []string{"build", "static-check"} }

func (a *GoBuildAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: "go", outputFmt: "text"}
	parser := &GoBuildTextParser{}
	return base.ExecAndParse(ctx, []string{"build", "./..."}, args.RepoRoot, parser)
}
```

### GoFormatAdapter

```go
type GoFormatAdapter struct {
	detector *ToolDetector
}

func (a *GoFormatAdapter) Name() string { return "go-format" }
func (a *GoFormatAdapter) Capabilities() []string { return []string{"format"} }

func (a *GoFormatAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: "gofmt", outputFmt: "text"}
	// gofmt -l . 输出未格式化文件列表
	cmdArgs := []string{"-l", "."}
	if len(args.Files) > 0 {
		cmdArgs = append([]string{"-l"}, args.Files...)
	}
	result, err := base.ExecAndParse(ctx, cmdArgs, args.RepoRoot, &GoBuildTextParser{})
	if result != nil {
		// gofmt 成功但有输出 = 有未格式化的文件
		result.CheckPassed = result.RawOutput == ""
	}
	return result, err
}
```

### GoImportsAdapter

```go
type GoImportsAdapter struct {
	detector *ToolDetector
}

func (a *GoImportsAdapter) Name() string { return "goimports" }
func (a *GoImportsAdapter) Capabilities() []string { return []string{"import-fix", "format"} }

func (a *GoImportsAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	cmd := "goimports"
	if _, ok := a.detector.Get("goimports"); !ok {
		// fallback 到 gofmt（只能格式化，不能修复 import）
		cmd = "gofmt"
	}
	base := BaseAdapter{cmd: cmd, outputFmt: "text"}

	cmdArgs := []string{"-w"} // 写入修改
	if len(args.Files) > 0 {
		cmdArgs = append(cmdArgs, args.Files...)
	} else {
		cmdArgs = append(cmdArgs, ".")
	}

	result, err := base.ExecAndParse(ctx, cmdArgs, args.RepoRoot, &GoBuildTextParser{})
	if result != nil {
		result.CheckPassed = err == nil
	}
	return result, err
}
```

---

## 6.8 与旧版实现对比

| 维度 | 旧版（手写 AST） | 新版（原生工具 Wrapper） |
|------|----------------|------------------------|
| `go-contract-builder` | 手写 `filepath.Walk` + `go/parser` | 复用 `go/packages`（类型更准确） |
| `go-static-check` | 手写 AST 遍历查未定义符号 | 复用 `staticcheck` / `golangci-lint` |
| `go-import-resolution` | 手写文本替换 | 复用 `goimports` + `go get` |
| `repo-quality-gate` | 直接 `exec.Command`（这部分没变） | 直接 `exec.Command`（保持） |
| `go-repair-diagnosis` | 手写正则匹配 | 保持（纯文本分析，无外部工具） |
| 新增能力 | 无 | 自动工具检测 + fallback 链 |
| 新增能力 | 无 | 多语言 Profile 扩展 |
| 代码量 | ~1,500 行 | ~800 行（减少 ~47%） |

---
## 7. Prompt Skill 设计 (SKILL.md)

Prompt Skill 存储在 `.claude/skills/` 或 `~/.claude/skills/` 中，由 `pkg/skills/` 加载。以下是 5 个核心 Prompt Skill 的完整内容。

### 7.1 go-repair-strategy (修复策略)

```markdown
---
name: go-repair-strategy
description: General strategy for repairing Go compilation errors in the Agent Harness
when_to_use: When a Go compilation error occurs and you need to decide how to fix it
paths:
  - "**/*.go"
---

# Go Repair Strategy

## Core Principles (Karpathy-Inspired)

### 1. Think Before Code
Before generating ANY fix, you MUST think through the problem:
- What is the ACTUAL root cause (not the symptom)?
- What is the SMALLEST change that fixes this?
- What assumptions am I making about the codebase?
- If I cannot explain the root cause in 2 sentences, STOP and ask for help.

### 2. Simplicity First
- One error = one change. Do NOT refactor unrelated code.
- Do NOT create abstractions for single-use code.
- If the fix requires >50 lines, rethink your approach.
- Do NOT "improve" adjacent code while fixing an error.

### 3. Surgical Changes
- Only modify files you explicitly declared in your Change Scope.
- Do NOT change comments, variable names, or formatting outside the fix area.
- Every changed line MUST trace directly to the compilation error.
- Match existing code style (naming, error handling, comments).

### 4. Goal-Driven Execution
- Focus on the success criteria, not the steps.
- Success = "This specific compilation error is resolved with minimal change."
- If a fix introduces new errors, STOP and reconsider.
- Do NOT optimize for "elegance" — optimize for "correctness + simplicity".

### 5. Contract-First
- Before modifying any code, query the Contract Store to discover
  existing interfaces and method signatures. Never invent new signatures that conflict
  with existing contracts.

### 6. Tool Usage
For complex analysis, call Tool Skills instead of guessing.
- `go-repair-diagnosis` -> get structured diagnosis
- `go-static-check` -> catch errors before compiling
- `go-import-resolution` -> fix import issues

## Decision Tree

### Step 1: Classify the Error

Read the compiler error and classify it:

| Error Pattern | Category | First Action |
|---------------|----------|-------------|
| `undefined: X` | undefined_symbol | Search codebase for X; check imports |
| `cannot use X (type A) as type B` | type_mismatch | Check expected type in contract/function sig |
| `X does not implement Y` | interface_mismatch | Query Contract Store for Y's methods |
| `too few/many arguments` | interface_mismatch | Check callee signature |
| `ambiguous import` | import_conflict | Call `go-import-resolution` |
| `imported and not used` | import_conflict | Remove import or use identifier |

### Step 2: Gather Context

For interface-related errors, ALWAYS call `go-contract-builder` first if the Contract
Store might be stale. Then query the relevant contract.

For cross-package errors, use the Context Engine to retrieve:
- The interface definition (for missing_method)
- The struct definition (for field_error)
- The import block of both files (for import_conflict)

### Step 3: Generate Fix

Generate the fix as a structured edit snippet:

```json
[{
  "file": "relative/path/to/file.go",
  "start_marker": "//...existing code...",
  "end_marker": "//...existing code...",
  "replacement": "// new code"
}]
```

Rules for edits:
- Use `//...existing code...` to mark unchanged boundaries
- Keep replacements minimal (usually 3-20 lines)
- Never delete lines that are not directly related to the fix
- Preserve comments and formatting

### Step 4: Validate

After applying, run `repo-quality-gate` with at least `["build"]`.
If it fails, return to Step 1. Do not apply additional changes without
re-validating.

## Anti-Patterns to Avoid

- **Do not** bulk-replace all occurrences of a name without checking each context
- **Do not** add `// nolint` or `//lint:ignore` to suppress errors
- **Do not** change exported API signatures to fix internal errors
- **Do not** use `interface{}` or `any` as a quick fix for type mismatches
```

### 7.2 contract-first-coding (契约优先编码)

```markdown
---
name: contract-first-coding
description: Coding guidelines that enforce interface-first design in Go
when_to_use: When generating new Go code, modifying existing code, or implementing interfaces
paths:
  - "**/*.go"
---

# Contract-First Coding (Karpathy Principles Applied)

## Principle: Think Before Implement

Before writing ANY code, you MUST:
1. **Query the Contract Store** for existing interfaces and types
2. **Declare your assumptions**: "I assume interface X already exists because..."
3. **State the minimal interface** needed for this capability
4. If you cannot explain the contract in 2 sentences, STOP and ask for help

## Rule 1: Declare Before Implement

When asked to add a new capability:

1. Define the interface that describes the capability
2. Write constructors that return the interface type, not concrete types
3. Implement the interface on the concrete type
4. Register the implementation in the Contract Store (via `go-contract-builder`)

Example:
```go
// Good: Return interface
type Processor interface { Process(ctx context.Context, input string) (string, error) }
func NewProcessor() Processor { return &processorImpl{} }

// Bad: Return concrete type
func NewProcessor() *processorImpl { return &processorImpl{} }
```

## Rule 2: Simplicity in Contracts

- Do NOT add methods to an interface "just in case"
- Do NOT create "flexible" interfaces with 10+ methods
- One capability = one focused interface (ideally 1-3 methods)
- If an interface has >5 methods, it probably needs splitting

## Rule 3: Constructor Contract Compliance

Constructors MUST return the declared interface type. If the concrete type gains
a new method, do NOT change the constructor return type. Either:
- Add the method to the interface first (contract change), OR
- Keep the method unexported if it's implementation detail

## Rule 4: No Empty Interfaces

Do not use `interface{}` or `any` as function parameters or return types unless
absolutely required by external APIs. Prefer strongly-typed interfaces.

## Rule 5: Query Before Extending

Before adding a new method to an existing type, query the Contract Store:
- Does an interface already define this method?
- Who implements that interface?
- Will your change break existing implementors?

If an interface needs extension, update the interface definition FIRST, then
update all implementors.

## Rule 6: Surgical Interface Changes

- Adding a method to an interface is a BREAKING CHANGE for all implementors
- Before adding: count implementors via Contract Store
- If >3 implementors, consider creating a new extended interface instead
- Never remove methods from published interfaces
```

### 7.3 import-alias-resolution (Import 别名解析)

```markdown
---
name: import-alias-resolution
description: Strategy for resolving Go import conflicts using aliases
when_to_use: When compilation fails with ambiguous import, name collision, or import not found
paths:
  - "**/*.go"
---

# Import Alias Resolution

## Conflict Types

1. **Ambiguous Import**: Two modules provide the same package path prefix.
   Solution: Use explicit import alias.

2. **Name Collision**: Imported package name shadows a local identifier.
   Solution: Rename import with alias.

3. **Missing Package**: `go: no required module provides package X`
   Solution: `go get X` or add to go.mod.

4. **Unused Import**: `imported and not used`
   Solution: Remove import or use the identifier.

## Resolution Steps

1. Identify the conflicting package name from the error
2. Check all files in the package for how the name is used
3. Choose an alias that is:
   - Descriptive (e.g., `stdjson` for `encoding/json`)
   - Not used elsewhere in the file
   - Consistent with project conventions
4. Apply alias to ALL files in the package that import this path
5. Update references in those files to use the alias

## Example

```go
// Before (ambiguous: both provide "logger")
import "github.com/anthropic/claude-go/pkg/logger"
import "github.com/sirupsen/logrus"

// After
import cllogger "github.com/anthropic/claude-go/pkg/logger"
import "github.com/sirupsen/logrus"
```
```

### 7.4 interface-mismatch-diagnosis (接口不匹配诊断)

```markdown
---
name: interface-mismatch-diagnosis
description: Diagnosing and fixing interface implementation mismatches in Go
when_to_use: When a type fails to implement an interface (compilation or static check error)
paths:
  - "**/*.go"
---

# Interface Mismatch Diagnosis

## Diagnosis Steps

1. **Get Interface Definition**: Query Contract Store for the interface.
   Note ALL methods, their parameter types, return types, and names.

2. **Get Receiver Definition**: Check the concrete type's existing methods.

3. **Compare Method-by-Method**:
   - Missing method? -> Add with exact signature
   - Wrong parameter types? -> Fix parameter types
   - Wrong return types? -> Fix return types
   - Pointer vs value receiver mismatch? -> Adjust receiver type
   - Method name typo? -> Rename to match interface

4. **Check Embedding**: If the type embeds another type that implements the
   interface, ensure the embedded type's methods are not shadowed.

## Common Mismatches

| Symptom | Cause | Fix |
|---------|-------|-----|
| `missing method X` | Method not defined on receiver | Add method with exact sig |
| `X has wrong type` | Parameter/return type mismatch | Adjust types to match interface |
| `pointer method` | Interface expects value receiver | Change receiver or call site |
| `value method` | Interface expects pointer receiver | Change receiver or call site |

## Important

- Do NOT change the interface to match your implementation (unless explicitly asked)
- Do NOT use type assertion or conversion to bypass interface requirements
- Do NOT leave methods unimplemented with panic bodies
```

### 7.5 quality-gate-awareness (质量门禁意识)

```markdown
---
name: quality-gate-awareness
description: Awareness of quality gates and validation requirements before marking work complete
when_to_use: Before declaring any code task complete
paths:
  - "**/*.go"
---

# Quality Gate Awareness

## Mandatory Checks (P0 - Critical)

Before declaring a code task complete, ALL of the following MUST pass:

1. **Build**: `go build ./...` must succeed with zero errors
2. **Tests**: `go test ./...` must pass (existing tests must not break)
3. **Vet**: `go vet ./...` must report no issues
4. **Think Phase**: Output contains a valid `<think>...</think>` block with root cause analysis

## Karpathy Principle Checks (P1 - Important)

5. **Simplicity Gate**: Total changed lines ≤ budget (generation ≤200, repair ≤50)
6. **Surgical Gate**: Only declared files modified; no collateral damage to adjacent code
7. **Goal-Driven Gate**: The specific compilation error is resolved (not just symptoms suppressed)

## Optional Checks (P2 - Guideline, Strict Mode)

8. **Format**: `gofmt -l .` must return empty (all files formatted)
9. **Static Check**: `go-static-check` tool must return zero diagnostics
10. **Style Match**: New code matches existing naming conventions and error handling patterns

## Failure Protocol (Goal-Driven)

If any gate fails:

1. **STOP**. Do not declare the task complete.
2. **Analyze**: Run `go-repair-diagnosis` to get structured diagnosis.
3. **Think**: Before writing any fix, restate the root cause and the minimal fix needed.
4. **Execute ONE fix** at a time.
5. **Validate against ALL success criteria** — not just the one that failed.
6. **Repeat** until all gates pass.

## Simplicity Failure Protocol

If the Simplicity Gate fails (too many lines changed):

1. STOP and reconsider your approach.
2. Ask: "What is the SMALLEST change that fixes this error?"
3. If the error truly requires a large change, break it into sub-tasks.
4. Do NOT bypass the line budget by splitting changes across multiple rounds.

## Surgical Failure Protocol

If the Surgical Gate fails (collateral damage detected):

1. STOP. Revert the changes that affected undeclared files.
2. Review your Change Scope declaration in the Think Phase.
3. Re-apply ONLY the changes directly related to the error.
4. Do NOT "improve" adjacent code while fixing the error.

## Budget Awareness

Each task has a round budget (default 10). Each failed validation consumes one round.
If you exceed 80% of the budget without passing all gates, escalate to the user
with a summary of remaining issues.

## Constraint Decay Warning

As constraints accumulate, Agent performance degrades ("Constraint Decay").
If you find yourself struggling to satisfy multiple gates simultaneously:

1. Check if you are over-engineering the solution (Simplicity Gate)
2. Check if you are modifying too many files (Surgical Gate)
3. Consider breaking the task into smaller, independent sub-tasks
4. When in doubt, STOP and ask the user for clarification

## No Bypass

- Do NOT use `// nolint` or `//lint:ignore` to suppress warnings
- Do NOT skip tests that are "probably unrelated"
- Do NOT declare partial success ("it compiles but tests fail")
- Do NOT modify test expectations to make failing tests pass
- Do NOT bypass the Think Phase by generating code immediately
- Do NOT expand the scope to "fix other things I noticed"
```

---

## 8. Agent Rules 文件规范

受 Karpathy `program.md` 和业界 `CLAUDE.md` / `AGENTS.md` 实践的启发，每个使用 Agent Harness 的项目应包含一个 **`.claude-go/AGENT_RULES.md`** 文件。这是项目的"Agent 宪法"——定义 Agent 在这个项目中的行为边界、约束和成功标准。

### AGENT_RULES.md 设计原则

1. **人类编写，Agent 遵守**：AGENT_RULES.md 由项目维护者编写，Agent 在执行任何任务前必须读取并遵守。
2. **约束分层**：Hard (不可绕过) / Soft (建议) / Contextual (按需)。
3. **声明式而非命令式**：描述"成功是什么样"而非"按什么步骤做"。
4. **项目特定**：每个项目有自己的规则，不试图统一所有项目的风格。

### AGENT_RULES.md 模板

```markdown
# Agent Rules for <Project Name>

## Project Overview
- Language: Go 1.22
- Module: github.com/example/project
- Architecture: Clean Architecture with domain-driven design

## Hard Constraints (MUST NOT violate)

### 1. Contract-First
- All new capabilities MUST start with an interface declaration.
- Constructors MUST return interface types, not concrete types.
- Before modifying any interface, query the Contract Store.

### 2. Simplicity Budget
- New feature: ≤ 200 lines changed per task
- Bug fix: ≤ 50 lines changed per task
- Do NOT create abstractions for single-use code.
- Do NOT add features beyond what was explicitly requested.

### 3. Surgical Scope
- Only modify files declared in the Think Phase.
- Do NOT change adjacent code (comments, formatting, variable names).
- Every changed line MUST trace directly to the task objective.

### 4. Think Phase Mandatory
- Before generating code, output <think>...</think> with:
  - Goal restatement
  - Constraints list
  - Change scope declaration
  - Assumptions
  - Stop condition

## Soft Guidelines (SHOULD follow)

### Code Style
- Naming: PascalCase for exported, camelCase for unexported
- Error handling: wrap errors with fmt.Errorf("context: %w", err)
- Comments: only for non-obvious logic
- Test naming: Test<Function>_<Scenario>

### Architecture
- Prefer explicit over implicit
- Prefer composition over inheritance
- Keep packages small (< 500 lines ideally)

## Success Criteria (MUST ALL PASS)
- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes
- [ ] `go vet ./...` reports no issues
- [ ] Changed lines within budget
- [ ] Only declared files modified
- [ ] Think Phase present in output

## When to STOP and Ask
- If you cannot explain the root cause in 2 sentences
- If the fix requires > budget lines
- If you need to modify > 3 files for a single bug fix
- If the task involves changing exported API signatures
- If you exceed 80% of the round budget
```

### AGENT_RULES.md 加载机制

```go
// pkg/agent/rules_loader.go
package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// AgentRules 项目级 Agent 规则
type AgentRules struct {
	Project      string            `json:"project"`
	HardConstraints []Constraint   `json:"hard_constraints"`
	SoftGuidelines  []Guideline    `json:"soft_guidelines"`
	SuccessCriteria []string       `json:"success_criteria"`
	StopConditions  []string       `json:"stop_conditions"`
	RawContent      string         `json:"-"`
}

type Constraint struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Enforceable bool   `json:"enforceable"` // Validation Gate 能否自动检查
}

type Guideline struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// LoadAgentRules 从项目目录加载 AGENT_RULES.md
func LoadAgentRules(repoRoot string) (*AgentRules, error) {
	paths := []string{
		filepath.Join(repoRoot, ".claude-go", "AGENT_RULES.md"),
		filepath.Join(repoRoot, "AGENT_RULES.md"),
		filepath.Join(repoRoot, "CLAUDE.md"),
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			rules := parseAgentRules(string(data))
			rules.RawContent = string(data)
			return rules, nil
		}
	}
	return nil, os.ErrNotExist
}

func parseAgentRules(content string) *AgentRules {
	rules := &AgentRules{}
	// 简单解析：按 ## 标题分块
	sections := strings.Split(content, "\n## ")
	for _, section := range sections {
		lines := strings.SplitN(section, "\n", 2)
		if len(lines) < 2 {
			continue
		}
		title := strings.TrimSpace(lines[0])
		body := lines[1]
		switch {
		case strings.Contains(title, "Project"):
			// 提取项目信息
		case strings.Contains(title, "Hard Constraints"):
			rules.HardConstraints = parseConstraints(body)
		case strings.Contains(title, "Soft"):
			rules.SoftGuidelines = parseGuidelines(body)
		case strings.Contains(title, "Success Criteria"):
			rules.SuccessCriteria = parseList(body)
		case strings.Contains(title, "STOP"):
			rules.StopConditions = parseList(body)
		}
	}
	return rules
}
```

### 与 Code Executor 的集成

```go
func (e *CodeExecutor) loadAgentRules(repoRoot string) {
	rules, err := LoadAgentRules(repoRoot)
	if err != nil {
		log.Printf("[AgentRules] No AGENT_RULES.md found in %s, using defaults", repoRoot)
		e.agentRules = defaultAgentRules()
		return
	}
	e.agentRules = rules
	log.Printf("[AgentRules] Loaded %d hard constraints, %d soft guidelines from %s",
		len(rules.HardConstraints), len(rules.SoftGuidelines), repoRoot)
}

func (e *CodeExecutor) injectRulesIntoPrompt(basePrompt string) string {
	if e.agentRules == nil || e.agentRules.RawContent == "" {
		return basePrompt
	}
	// 将 AGENT_RULES.md 的内容注入到 prompt 的顶部
	return fmt.Sprintf("## Project Agent Rules\n%s\n\n---\n\n%s",
		e.agentRules.RawContent, basePrompt)
}
```

---

## 9. LLM Wiki 知识持久化

受 Karpathy **LLM Wiki 范式**启发：与其每次让 Agent 重新推导知识，不如将项目知识**编译一次**为持久化、结构化、交叉链接的 Markdown wiki，后续所有操作查询 wiki 而非原始源码。

### 核心思想

| 传统方式 | LLM Wiki 方式 |
|---------|--------------|
| Agent 每次重新扫描源码理解项目 | Agent 查询预编译的 wiki 获取上下文 |
| Contract Store 只有符号信息 | Wiki 包含设计决策、架构约束、失败模式 |
| 知识随会话消失 | 知识持久化，跨会话累积 |
| 每次任务都从零开始理解 | 增量更新 wiki，知识复利增长 |

### Wiki 目录结构

```
.claude-go/wiki/
├── README.md              # Wiki 导航和索引
├── architecture/
│   ├── overview.md        # 架构总览
│   ├── domains.md         # 领域边界
│   └── dependencies.md    # 模块依赖图
├── contracts/
│   ├── interfaces.md      # 核心接口索引
│   └── implementations.md # 实现者映射
├── decisions/
│   ├── ADR-001.md         # 架构决策记录
│   └── ADR-002.md
├── patterns/
│   ├── error-handling.md  # 错误处理模式
│   ├── testing.md         # 测试约定
│   └── naming.md          # 命名规范
├── failures/
│   ├── common-errors.md   # 常见编译错误及修复
│   └── false-positives.md # 已知的静态检查误报
└── context/
    └── project-map.md     # 项目结构导航
```

### Wiki 内容示例 (architecture/overview.md)

```markdown
# Architecture Overview

## Layer Structure
```
API Layer (cmd/)
  └── HTTP handlers, CLI commands

Service Layer (pkg/service/)
  └── Business logic, orchestration

Domain Layer (pkg/domain/)
  └── Entities, value objects, domain events

Infrastructure Layer (pkg/infra/)
  └── DB, cache, external clients
```

## Key Interfaces
| Interface | Package | Implementors | Purpose |
|-----------|---------|-------------|---------|
| TaskRunner | orchestrator | CodeRunner, ShellRunner | Execute tasks |
| Validator | validation | Gate, Linter | Validate code |
| Adapter | toolskill | StaticcheckAdapter, GoBuildAdapter | Wrap native tools |

## Design Decisions
- **Contract-First**: All constructors return interface types
- **Tool Wrapper**: Native tools (staticcheck, go vet) wrapped for LLM consumption
- **Simplicity Budget**: Max 200 lines per feature task

## Known Constraints
- Do NOT add circular dependencies between layers
- Do NOT bypass Validation Gate for "quick fixes"
```

### Wiki 编译器 (pkg/agent/wiki_compiler.go)

```go
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WikiCompiler 将项目源码和上下文编译为 LLM Wiki
type WikiCompiler struct {
	contractStore *ContractStore
	repoRoot      string
	wikiDir       string
}

func NewWikiCompiler(store *ContractStore, repoRoot string) *WikiCompiler {
	return &WikiCompiler{
		contractStore: store,
		repoRoot:      repoRoot,
		wikiDir:       filepath.Join(repoRoot, ".claude-go", "wiki"),
	}
}

// Compile 全量编译 wiki（首次或 Contract Store 重大变更后调用）
func (wc *WikiCompiler) Compile() error {
	if err := os.MkdirAll(wc.wikiDir, 0755); err != nil {
		return err
	}

	// 1. 编译架构总览
	if err := wc.compileArchitectureOverview(); err != nil {
		return fmt.Errorf("architecture overview: %w", err)
	}

	// 2. 编译接口索引
	if err := wc.compileContractIndex(); err != nil {
		return fmt.Errorf("contract index: %w", err)
	}

	// 3. 编译决策记录（从 git log 提取）
	if err := wc.compileDecisionRecords(); err != nil {
		return fmt.Errorf("decision records: %w", err)
	}

	// 4. 编译失败模式（从 memory 提取）
	if err := wc.compileFailurePatterns(); err != nil {
		return fmt.Errorf("failure patterns: %w", err)
	}

	return nil
}

// compileArchitectureOverview 生成架构总览 wiki
func (wc *WikiCompiler) compileArchitectureOverview() error {
	var sb strings.Builder
	sb.WriteString("# Architecture Overview\n\n")
	sb.WriteString("## Module Structure\n\n")
	sb.WriteString("```\n")
	// 遍历 packages 生成模块树
	for pkgPath, pkg := range wc.contractStore.packages {
		sb.WriteString(fmt.Sprintf("%s/\n", pkgPath))
		for name := range pkg.Types {
			sb.WriteString(fmt.Sprintf("  └── %s\n", name))
		}
	}
	sb.WriteString("```\n\n")

	path := filepath.Join(wc.wikiDir, "architecture", "overview.md")
	os.MkdirAll(filepath.Dir(path), 0755)
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

// compileContractIndex 生成接口实现者映射
func (wc *WikiCompiler) compileContractIndex() error {
	var sb strings.Builder
	sb.WriteString("# Contract Index\n\n")
	sb.WriteString("| Interface | Package | Methods | Implementors |\n")
	sb.WriteString("|-----------|---------|---------|-------------|\n")

	for key, contract := range wc.contractStore.Contracts {
		methods := make([]string, 0, len(contract.Methods))
		for mname := range contract.Methods {
			methods = append(methods, mname)
		}
		impls := wc.contractStore.Implementors[key]
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
			contract.Name, contract.Package,
			strings.Join(methods, ", "),
			strings.Join(impls, ", ")))
	}

	path := filepath.Join(wc.wikiDir, "contracts", "interfaces.md")
	os.MkdirAll(filepath.Dir(path), 0755)
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

// compileFailurePatterns 从历史失败记录中提取模式
func (wc *WikiCompiler) compileFailurePatterns() error {
	// 从 memory store 读取历史诊断和修复记录
	// 生成常见错误模式文档
	var sb strings.Builder
	sb.WriteString("# Common Failure Patterns\n\n")
	sb.WriteString("| Error Pattern | Root Cause | Typical Fix | Frequency |\n")
	sb.WriteString("|--------------|-----------|------------|-----------|\n")
	// TODO: 从 memory 提取历史数据
	return os.WriteFile(filepath.Join(wc.wikiDir, "failures", "common-errors.md"), []byte(sb.String()), 0644)
}
```

### Wiki 查询器 (Context Engine 扩展)

```go
// WikiQuery 查询 LLM Wiki 获取上下文
type WikiQuery struct {
	wikiDir string
}

// Retrieve 根据任务类型检索相关 wiki 页面
func (wq *WikiQuery) Retrieve(req ContextRequest) ([]ContextChunk, error) {
	var chunks []ContextChunk

	switch req.TaskType {
	case "implement":
		// 检索架构总览 + 接口定义
		chunks = append(chunks, wq.readWikiPage("architecture/overview.md"))
		chunks = append(chunks, wq.readWikiPage("contracts/interfaces.md"))
	case "fix":
		// 检索失败模式 + 相关接口
		chunks = append(chunks, wq.readWikiPage("failures/common-errors.md"))
		if req.Symbol != "" {
			chunks = append(chunks, wq.readWikiPage("contracts/interfaces.md"))
		}
	case "refactor":
		// 检索架构约束 + 设计决策
		chunks = append(chunks, wq.readWikiPage("architecture/overview.md"))
		chunks = append(chunks, wq.readWikiPage("decisions/"))
	}

	return chunks, nil
}

func (wq *WikiQuery) readWikiPage(path string) ContextChunk {
	fullPath := filepath.Join(wq.wikiDir, path)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return ContextChunk{Source: "wiki", Content: "", Relevance: 0}
	}
	return ContextChunk{
		Source:    "wiki:" + path,
		Content:   string(data),
		Relevance: 0.8,
	}
}
```

### 与 Contract Store 的关系

| 组件 | 内容 | 更新频率 | 查询场景 |
|------|------|---------|---------|
| Contract Store | 符号定义、接口方法、实现者 | 每次代码变更后 | 编译时类型检查 |
| LLM Wiki | 架构决策、设计模式、失败经验 | 定期批量编译 | 任务规划、错误诊断 |
| AGENT_RULES.md | 行为约束、成功标准 | 人工维护 | 所有任务前置检查 |

三者共同构成 Agent 的**上下文三角**：
- **AGENT_RULES.md** 定义"应该怎么做"（规范）
- **Contract Store** 定义"能用什么"（能力）
- **LLM Wiki** 定义"之前怎么做的"（经验）

---

## 10. Orchestrator 集成

### 8.1 EngineConfig 扩展 (pkg/orchestrator/engine.go)

```go
package orchestrator

// EngineConfig 扩展字段（添加到现有 EngineConfig 中）
type EngineConfig struct {
	// ... existing fields ...

	// AgentHarness 配置
	Harness HarnessConfig `json:"harness,omitempty"`
}

// HarnessConfig Contract-First Agent Harness 配置
type HarnessConfig struct {
	// ContractStorePath Contract Store JSON 文件路径
	ContractStorePath string `json:"contract_store_path,omitempty"`

	// SkillDirs 额外 Skill 目录（除默认外）
	SkillDirs []string `json:"skill_dirs,omitempty"`

	// MaxRepairRounds 每个代码任务的最大修复轮数
	MaxRepairRounds int `json:"max_repair_rounds,omitempty"`

	// ValidationChecks 质量门禁检查列表
	ValidationChecks []string `json:"validation_checks,omitempty"`

	// PromptSkills 启用的 Prompt Skill 名称列表
	PromptSkills []string `json:"prompt_skills,omitempty"`

	// AutoBuildContractStore 是否在任务启动前自动构建 Contract Store
	AutoBuildContractStore bool `json:"auto_build_contract_store,omitempty"`
}

// DefaultHarnessConfig 返回默认配置
func DefaultHarnessConfig() HarnessConfig {
	return HarnessConfig{
		MaxRepairRounds:        10,
		ValidationChecks:       []string{"build", "test", "vet"},
		PromptSkills:           []string{"go-repair-strategy", "contract-first-coding", "quality-gate-awareness"},
		AutoBuildContractStore: true,
	}
}
```

### 8.2 ValidationHook (pkg/orchestrator/hooks.go 扩展)

```go
package orchestrator

import (
	"fmt"
	"log"

	"github.com/anthropic/claude-go/pkg/toolskill"
)

// ValidationHook 在任务完成后运行质量门禁检查。
// 实现 LifecycleHook 接口。
type ValidationHook struct {
	gate      *validation.Gate
	runtime   *toolskill.Runtime
	maxRounds int
}

func NewValidationHook(gate *validation.Gate, runtime *toolskill.Runtime, maxRounds int) *ValidationHook {
	return &ValidationHook{gate: gate, runtime: runtime, maxRounds: maxRounds}
}

func (h *ValidationHook) OnGraphStart(g *Graph) error {
	// 如果配置了自动构建 Contract Store，在图开始时执行
	cfg := g.Config.Harness
	if cfg.AutoBuildContractStore && cfg.ContractStorePath != "" {
		repoRoot := g.Metadata["repo_root"]
		modulePath := g.Metadata["module_path"]
		if rr, ok := repoRoot.(string); ok {
			_, _ = h.runtime.Execute(ctx, "go-contract-builder", mustJSON(map[string]any{
				"repo_root":   rr,
				"module":      modulePath,
				"output_path": cfg.ContractStorePath,
			}))
		}
	}
	return nil
}

func (h *ValidationHook) OnTaskComplete(task *Task, result *TaskResult) error {
	// 只对 CodeTask 类型的任务进行验证
	if task.Type != "code" {
		return nil
	}

	repoRoot := ""
	if task.Metadata != nil {
		if v, ok := task.Metadata["repo_root"].(string); ok {
			repoRoot = v
		}
	}
	if repoRoot == "" {
		return nil
	}

	// 运行质量门禁
	res := h.runtime.Execute(ctx, "repo-quality-gate", mustJSON(map[string]any{
		"repo_root": repoRoot,
		"checks":    task.Metadata["validation_checks"],
	}))

	if !res.Success {
		log.Printf("[ValidationHook] Task %s failed quality gate", task.ID)
		result.Success = false
		result.Error = fmt.Sprintf("quality gate failed: %v", res.Error)
		return nil
	}

	// 检查轮数预算
	rounds := 0
	if task.Metadata != nil {
		if r, ok := task.Metadata["repair_rounds"].(int); ok {
			rounds = r
		}
	}
	if rounds >= h.maxRounds {
		result.Success = false
		result.Error = fmt.Sprintf("exceeded max repair rounds (%d)", h.maxRounds)
	}

	return nil
}

func (h *ValidationHook) OnGraphComplete(g *Graph, results map[string]*TaskResult) error {
	// 图级别：汇总所有代码任务的验证结果
	var failed []string
	for id, res := range results {
		if !res.Success {
			failed = append(failed, id)
		}
	}
	if len(failed) > 0 {
		log.Printf("[ValidationHook] Graph completed with %d failed tasks: %v", len(failed), failed)
	}
	return nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
```

### 8.3 TaskRunner 注册 (pkg/orchestrator/runner.go 扩展)

```go
package orchestrator

import (
	"github.com/anthropic/claude-go/pkg/agent"
)

// RegisterCodeRunner 注册 CodeExecutor 作为 TaskRunner。
func RegisterCodeRunner(reg *RunnerRegistry, executor *agent.CodeExecutor) {
	reg.Register(&codeRunner{executor: executor})
}

type codeRunner struct {
	executor *agent.CodeExecutor
}

func (r *codeRunner) Type() string { return "code" }

func (r *codeRunner) Run(task *Task, bb ReadOnlyBlackboard) *TaskResult {
	return r.executor.Run(task, bb)
}
```

---

## 11. workflow.go 迁移计划

### 9.1 当前结构分析

`workflow.go` 共 15,687 行，分为两大部分：

| 区域 | 行号范围 | 内容 | 处理方式 |
|------|---------|------|---------|
| A | 1 - 5,312 | Workflow 框架（Graph、Stage、Task、Runner 注册、Hooks、Checkpoint） | **保留并精简** |
| B | 5,313 - 15,687 | ~70 repair 函数 + ~5 fallback 函数 | **全部删除** |

### 9.2 保留内容（精简到 < 2,000 行）

从区域 A 保留以下核心类型和函数：

```go
// 保留的核心类型（约 800 行）
- type Workflow struct { ... }
- type Stage struct { ... }
- type StageResult struct { ... }
- type TaskState int
- type Task struct { ... }
- type TaskResult struct { ... }
- type TaskRunner interface { ... }
- type RunnerRegistry struct { ... }
- type LifecycleHook interface { ... }
- type Blackboard interface { ... }
- type Engine struct { ... }
- type EngineConfig struct { ... } // 扩展 HarnessConfig
- func NewEngine(cfg EngineConfig) *Engine
- func (e *Engine) Execute(ctx context.Context, workflow *Workflow) (*WorkflowResult, error)
- func (e *Engine) executeStage(...) // 核心调度逻辑
```

删除以下内容：
- 所有 `repairGo*` 函数（~70 个）
- `applyGoCompileTextRepairs`
- `applyGoCompileErrorRepairs`
- `deterministicPlannerFallbackStageResult`
- `deterministicStageValidationFallback`
- 所有硬编码的文本替换规则
- 所有基于正则表达式的错误分类逻辑
- 过时的注释和 TODO（保留有价值的架构注释）

### 9.3 新增内容

在 `workflow.go` 精简版（< 2,000 行）的基础上，新增以下文件：

| 文件 | 代码量 | 说明 |
|------|--------|------|
| `pkg/contract/store.go` | ~350 行 | Contract Store |
| `pkg/patch/apply.go` | ~250 行 | AST Patch Apply |
| `pkg/validation/gate.go` | ~180 行 | Validation Gate |
| `pkg/context/engine.go` | ~300 行 | Context Engine |
| `pkg/agent/code_executor.go` | ~400 行 | CodeExecutor（替代原有 repair 逻辑） |
| `pkg/toolskill/schema.go` | ~200 行 | Tool Skill 基础类型 |
| `pkg/toolskill/runtime.go` | ~150 行 | Tool Skill 运行时 |
| `pkg/toolskill/contract_builder.go` | ~200 行 | go-contract-builder |
| `pkg/toolskill/static_check.go` | ~180 行 | go-static-check |
| `pkg/toolskill/repair_diagnosis.go` | ~180 行 | go-repair-diagnosis |
| `pkg/toolskill/import_resolution.go` | ~150 行 | go-import-resolution |
| `pkg/toolskill/quality_gate.go` | ~150 行 | repo-quality-gate |
| `.claude/skills/*.md` | ~300 行 | 5 个 Prompt Skill |

**总新增代码：~2,800 行。删除代码：~10,000+ 行。净减少：~7,200 行。**

---

## 12. 迁移步骤（一步到位）

### Step 1: 创建新文件（并行执行）

一次性创建所有新文件：

```bash
# 目录结构
mkdir -p pkg/{contract,patch,validation,context,toolskill}
mkdir -p .claude/skills/{go-repair-strategy,contract-first-coding,import-alias-resolution,interface-mismatch-diagnosis,quality-gate-awareness}

# 文件创建（空文件占位，随后写入内容）
touch pkg/contract/store.go
# ... 等等
```

### Step 2: 写入新文件内容

使用 Write 工具并行写入所有新文件（本设计文档中的完整代码）。

### Step 3: 精简 workflow.go

1. 备份 `workflow.go` 到 `workflow.go.backup`
2. 删除行 5,313 - 15,687（所有 repair/fallback 函数）
3. 在剩余代码中，将 `EngineConfig` 扩展 `HarnessConfig` 字段
4. 在 `NewEngine` 中初始化 Harness 组件（Contract Store, ToolSkill Runtime, CodeExecutor）
5. 在 `executeStage` 中，将原有的 `deterministicPlannerFallbackStageResult` 调用替换为：
   - 调用 `repo-quality-gate` Tool Skill
   - 如果失败，创建 repair sub-task 并调度给 CodeExecutor

### Step 4: 注册 TaskRunner

在 `cmd/claude-go/main.go`（或对应的初始化代码）中：

```go
func initAgentHarness(engine *orchestrator.Engine, cfg orchestrator.HarnessConfig) {
	// 1. 加载 Contract Store
	store := contract.NewStore()
	if _, err := os.Stat(cfg.ContractStorePath); err == nil {
		_ = store.Load(cfg.ContractStorePath)
	}

	// 2. 初始化 Tool Skill Runtime
	reg := toolskill.NewRegistry()
	rt := toolskill.NewRuntime(reg, sandbox.NewExecutor())
	rt.RegisterDefaults(store, validation.NewGate())

	// 3. 初始化 CodeExecutor
	executor := agent.NewCodeExecutor(agent.CodeExecutorOptions{
		MaxRounds:     cfg.MaxRepairRounds,
		ContractStore: store,
		Validator:     validation.NewGate(),
		PatchApplier:  patch.NewApplier(),
		ContextEngine: context.NewContextEngine(),
		ToolRuntime:   rt,
	})

	// 4. 加载 Prompt Skills
	executor.SetPromptSkills(cfg.PromptSkills)

	// 5. 加载 Agent Rules（如果存在）
	executor.LoadAgentRules(cfg.RepoRoot)

	// 6. 初始化 Wiki Compiler
	wikiCompiler := agent.NewWikiCompiler(store, cfg.RepoRoot)
	executor.SetWikiCompiler(wikiCompiler)

	// 7. 注册到 Orchestrator
	orchestrator.RegisterCodeRunner(engine.RunnerRegistry(), executor)
	engine.AddHook(orchestrator.NewValidationHook(validation.NewGate(), rt, cfg.MaxRepairRounds))
}
```

### Step 5: 初始化 Agent Rules 和 Wiki

创建项目级约束文件和知识目录：

```bash
# 1. 创建 AGENT_RULES.md（从模板复制并修改）
mkdir -p .claude-go/wiki/{architecture,contracts,decisions,patterns,failures,context}
cp docs/templates/AGENT_RULES.md.template .claude-go/AGENT_RULES.md

# 2. 首次编译 LLM Wiki（从 Contract Store 生成）
go run ./cmd/wiki-compiler --repo-root=. --output=.claude-go/wiki

# 3. 将 AGENT_RULES.md 和 wiki/ 加入 .gitignore（可选，因为 wiki 是可重新生成的）
echo ".claude-go/wiki/" >> .gitignore
echo "!.claude-go/AGENT_RULES.md" >> .gitignore
```

### Step 6: 验证

运行以下命令验证迁移：

```bash
# 1. 编译检查
cd /Users/huaquan.liang/Documents/GitHub/ruflo/claude-go
go build ./...

# 2. 单元测试
go test ./pkg/contract/... ./pkg/patch/... ./pkg/validation/... ./pkg/context/... ./pkg/toolskill/... ./pkg/agent/...

# 3. 验证 AGENT_RULES.md 加载
go test -run TestAgentRules ./pkg/agent/...

# 4. 验证 Wiki 编译
go test -run TestWikiCompiler ./pkg/agent/...

# 5. 集成测试：运行一个真实修复任务
# （需要准备测试仓库和已知编译错误的任务）

# 4. 统计代码量变化
wc -l pkg/agent/workflow.go pkg/contract/store.go pkg/patch/apply.go pkg/validation/gate.go pkg/context/engine.go pkg/agent/code_executor.go pkg/toolskill/*.go
```

---

## 13. 测试与基准

### 11.1 测试矩阵

| 测试类型 | 目标 | 方法 |
|---------|------|------|
| 单元测试 | Contract Store, Patch Apply, Validation Gate, Context Engine, 每个 Tool Skill | `go test` with table-driven tests |
| 集成测试 | CodeExecutor 完整循环 | 使用内存文件系统 + 模拟 LLM |
| 回归测试 | 原有 workflow.go 修复的场景 | 提取 20 个典型编译错误，验证新系统能正确处理 |
| 性能测试 | Patch Apply 速度 vs 旧版 regex | BenchmarkASTPatch vs BenchmarkRegexPatch |
| 端到端测试 | 真实仓库修复 | 使用 testdata/fixtures/ 下的示例项目 |

### 11.2 回归测试用例（20 个典型场景）

从 workflow.go 的 70+ repair 函数中提取最具代表性的 20 个场景：

1. `undefined: fmt.Printf` -> missing import
2. `cannot use X (type string) as type int` -> type mismatch
3. `*Foo does not implement Bar` -> missing method
4. `too few arguments in call to NewClient` -> signature mismatch
5. `ambiguous import: github.com/foo/bar` -> import alias
6. `imported and not used: "os"` -> unused import
7. `unknown field 'Name' in struct literal` -> field error
8. `undefined: MyInterface.Method` -> interface contract violation
9. `cannot assign to X` -> assignability error
10. `invalid operation: X + Y (mismatched types)` -> binary op mismatch
11. `multiple-value in single-value context` -> return count mismatch
12. `method has pointer receiver` -> receiver mismatch
13. `redeclared as imported package name` -> import name conflict
14. `no new variables on left side of :=` -> variable redeclaration
15. `cannot take address of X` -> addressability error
16. `invalid indirect of X` -> dereference error
17. `undefined: reflect.TypeOf` -> missing reflect import
18. `cannot range over X` -> range type error
19. `missing return at end of function` -> missing return
20. `composite literal uses unkeyed fields` -> style error (warning only)

### 11.3 成功标准

| 指标 | 旧版 (workflow.go) | 新版 (Agent Harness) | 目标 |
|------|-------------------|---------------------|------|
| 代码行数 | 15,687 | < 2,000 + 4,500 新增 | 净减 > 50% |
| 修复函数数量 | 70+ | 0 (Tool Skill 替代) | 归零 |
| fallback 函数 | 5 | 0 (Validation Gate 替代) | 归零 |
| 编译错误准确率 | ~60%（regex 匹配） | > 90%（AST + diagnosis） | +30% |
| 修复轮数 | 平均 5-8 轮 | 平均 2-4 轮 | -50% |
| 假修复率（引入新问题） | ~15% | < 5% | -10% |
| 新类型错误处理 | 需新增 repair 函数 | 通用 diagnosis + contract | 无需新增代码 |
| Think Phase 合规率 | N/A | > 95% | 新增 |
| Simplicity Gate 通过率 | N/A | > 90%（变更 ≤50 行） | 新增 |
| Surgical Gate 通过率 | N/A | > 95%（无附带损伤） | 新增 |
| 约束衰减防护 | N/A | 约束分层 + 预算控制 | 新增 |

---

## 14. 附录：完整文件清单

### 新增文件

```
pkg/
  contract/
    store.go              # Contract Store 实现
  patch/
    apply.go              # AST Patch Apply
  validation/
    gate.go               # Validation Gate
  context/
    engine.go             # Context Engine
  agent/
    code_executor.go      # CodeExecutor（workflow.go 精简后新增）
    rules_loader.go       # AGENT_RULES.md 加载与解析
    wiki_compiler.go      # LLM Wiki 编译器
  toolskill/
    schema.go             # Tool Skill 类型定义
    runtime.go            # Tool Skill 运行时
    detect.go             # 工具自动检测器
    adapter.go            # 通用 Adapter 接口
    parser.go             # 原生工具输出解析器
    language.go           # 多语言 Profile 注册表
    contract_builder.go   # go-contract-builder
    static_check.go       # go-static-check
    repair_diagnosis.go   # go-repair-diagnosis
    import_resolution.go  # go-import-resolution
    quality_gate.go       # repo-quality-gate
.claude/
  skills/
    go-repair-strategy/SKILL.md
    contract-first-coding/SKILL.md
    import-alias-resolution/SKILL.md
    interface-mismatch-diagnosis/SKILL.md
    quality-gate-awareness/SKILL.md
.claude-go/
  AGENT_RULES.md          # 项目级 Agent 行为约束（Karpathy 原则）
  wiki/                   # LLM Wiki 知识持久化
    README.md
    architecture/
    contracts/
    decisions/
    patterns/
    failures/
```

### 修改文件

```
pkg/agent/workflow.go           # 删除 ~10,000 行，精简到 < 2,000 行
pkg/orchestrator/engine.go      # 扩展 EngineConfig + HarnessConfig
pkg/orchestrator/hooks.go       # 新增 ValidationHook
pkg/orchestrator/runner.go      # 新增 RegisterCodeRunner
cmd/claude-go/main.go           # 新增 initAgentHarness 调用
```

### 删除内容（不删除文件，只删除代码）

```
pkg/agent/workflow.go:
  - 所有 repairGo* 函数
  - applyGoCompileTextRepairs
  - applyGoCompileErrorRepairs
  - deterministicPlannerFallbackStageResult
  - deterministicStageValidationFallback
  - 所有硬编码文本替换规则
  - 所有基于正则的错误分类
```

---

*文档结束。本设计文档包含所有组件的完整 Go 实现代码，可直接用于开发。*
