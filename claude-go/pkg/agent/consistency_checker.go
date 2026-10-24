package agent

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// CheckerConsistencyIssue represents a single consistency issue found during scanning.
type CheckerConsistencyIssue struct {
	Type     string `json:"type"`     // e.g., "missing_method", "broken_signature", "orphan_reference"
	Symbol   string `json:"symbol"`   // fully qualified symbol name
	File     string `json:"file"`     // file where the issue was found
	Line     int    `json:"line"`     // line number
	Message  string `json:"message"`  // human-readable description
	Severity string `json:"severity"` // "error" or "warning"
}

// CheckerInterfaceCheck represents the result of checking a single interface.
type CheckerInterfaceCheck struct {
	InterfaceName string                    `json:"interface_name"`
	Package       string                    `json:"package"`
	File          string                    `json:"file"`
	Line          int                       `json:"line"`
	Methods       int                       `json:"methods"`
	Implementors  []string                  `json:"implementors"`
	Issues        []CheckerConsistencyIssue `json:"issues,omitempty"`
}

// CheckerGlobalConsistencyReport aggregates all consistency checks across a codebase.
type CheckerGlobalConsistencyReport struct {
	ScannedAt    time.Time                 `json:"scanned_at"`
	Packages     int                       `json:"packages"`
	Interfaces   int                       `json:"interfaces"`
	Implementors int                       `json:"implementors"`
	Issues       []CheckerConsistencyIssue `json:"issues,omitempty"`
	Checks       []CheckerInterfaceCheck   `json:"checks,omitempty"`
}

// ConsistencyChecker scans Go files and verifies interface implementations.
type ConsistencyChecker struct {
	root   string
	module string
	mu     sync.RWMutex
	fset   *token.FileSet
}

// NewConsistencyChecker 为给定根目录创建一致性检查器 (module 为空时 Check 会自己读 go.mod)。
// NewConsistencyChecker creates a new ConsistencyChecker for the given root directory.
func NewConsistencyChecker(root, module string) *ConsistencyChecker {
	return &ConsistencyChecker{
		root:   root,
		module: module,
		fset:   token.NewFileSet(),
	}
}

// Check 扫描根目录下全部 Go 文件, 逐个校验接口实现, 返回汇总报告。
//
// 算法解读：
//
//	原理：两遍 AST 扫描。第一遍 Walk 全部 .go (跳过 vendor/.git/node_modules/.claude-go
//	      与 _test.go), 以 "importPath.TypeName" 为键建三张表 —— 接口、结构体、
//	      结构体的方法集; 第二遍对每个接口遍历全部结构体, 用"接口方法名是否都在
//	      该方法集里"判定它是不是实现者, 再对每个实现者查缺失方法与签名。
//	关键不变量：符号全名 = importPath + "." + TypeName, 而 importPath 是**目录相对
//	      根目录**拼出来的 (不走 go list)。于是根目录选错、或同一目录里混着不同
//	      package 名时, 全表一起错位 —— 表内自洽所以不会报错, 只会**漏配**实现者。
//	代价与边界：接口 × 结构体是 O(I×S) 全量比对, 每次 Check 都重新解析全仓 (无缓存);
//	      单文件解析失败被静默吞掉 (return nil), 解析不了的文件等同于不存在。
//	设计权衡：宁可漏报不误报 —— 签名只比参数/返回值的**个数**而不比类型, 因此
//	      "签名不匹配"仅能抓出参数个数不同的实现 (见 methodSignatureMatches)。
//
// Check scans all Go files under the root directory and verifies interface implementations.
// It returns a CheckerGlobalConsistencyReport containing all found issues.
func (cc *ConsistencyChecker) Check() (*CheckerGlobalConsistencyReport, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if cc.module == "" {
		cc.module = cc.readGoModModule()
	}

	// First pass: collect all interfaces and structs
	interfaces := make(map[string]*ast.InterfaceType)          // pkg.Name -> *ast.InterfaceType
	structs := make(map[string]*ast.StructType)                // pkg.Name -> *ast.StructType
	structMethods := make(map[string]map[string]*ast.FuncDecl) // pkg.Name -> methodName -> *ast.FuncDecl
	interfacePositions := make(map[string]token.Position)
	structPositions := make(map[string]token.Position)
	packageForSymbol := make(map[string]string)

	err := filepath.Walk(cc.root, func(path string, info os.FileInfo, err error) error {
		// 遍历错误 (权限不足、目录被并发删除) 一律吞掉: 本检查器是只读旁路,
		// 不该因为一个读不了的目录就让整份报告失败 —— 代价是该目录被静默漏扫。
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
		return cc.parseFileForCheck(path, interfaces, structs, structMethods, interfacePositions, structPositions, packageForSymbol)
	})
	if err != nil {
		return nil, err
	}

	report := &CheckerGlobalConsistencyReport{
		ScannedAt: time.Now(),
		Packages:  len(packageForSymbol),
	}

	// Second pass: verify each interface
	for ifaceQName, ifaceType := range interfaces {
		check := cc.checkInterface(ifaceQName, ifaceType, structs, structMethods, interfacePositions, structPositions, packageForSymbol)
		report.Interfaces++
		// 注意: 同一个结构体实现 N 个接口就被计 N 次, 所以 Implementors 是
		// "接口-实现者配对数"而不是去重后的结构体个数。
		report.Implementors += len(check.Implementors)
		if len(check.Issues) > 0 {
			report.Issues = append(report.Issues, check.Issues...)
		}
		report.Checks = append(report.Checks, check)
	}

	// Sort for deterministic output
	sort.Slice(report.Issues, func(i, j int) bool {
		// Severity 是自由字符串: 除 "error" 之外的一切取值 (含将来新增的级别) 都被
		// 归到同一档, 组内再按符号名排 —— 输出顺序稳定, 便于 diff 两次扫描结果。
		if report.Issues[i].Severity != report.Issues[j].Severity {
			return report.Issues[i].Severity == "error"
		}
		return report.Issues[i].Symbol < report.Issues[j].Symbol
	})

	return report, nil
}

// readGoModModule 从 root/go.mod 里读出 module 行; 读不到或没有 module 行时返回 ""。
//
// 注意：用的是最朴素的逐行前缀匹配, 因此块式 `module (\n ... \n)` 写法读不出来,
// 行尾注释也会被一起当作模块名 (不做任何清理)。
func (cc *ConsistencyChecker) readGoModModule() string {
	data, err := os.ReadFile(filepath.Join(cc.root, "go.mod"))
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

// parseFileForCheck 解析单个文件, 把其中的接口/结构体/方法登记进第一遍扫描的三张表。
//
// 符号键统一为 importPath + "." + TypeName; 接收者指针 (*T) 会被剥掉, 使 T 与 *T
// 的方法归到同一张方法集里。读文件或解析失败都返回 nil (静默跳过), 不中断整次扫描。
func (cc *ConsistencyChecker) parseFileForCheck(
	path string,
	interfaces map[string]*ast.InterfaceType,
	structs map[string]*ast.StructType,
	structMethods map[string]map[string]*ast.FuncDecl,
	interfacePositions map[string]token.Position,
	structPositions map[string]token.Position,
	packageForSymbol map[string]string,
) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	f, err := parser.ParseFile(cc.fset, path, src, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return nil
	}

	dir := filepath.Dir(path)
	relDir, _ := filepath.Rel(cc.root, dir)
	importPath := filepath.ToSlash(relDir)
	if cc.module != "" {
		importPath = cc.module + "/" + importPath
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					qname := importPath + "." + ts.Name.Name
					packageForSymbol[qname] = importPath
					pos := cc.fset.Position(ts.Pos())
					switch t := ts.Type.(type) {
					case *ast.InterfaceType:
						interfaces[qname] = t
						interfacePositions[qname] = pos
					case *ast.StructType:
						structs[qname] = t
						structPositions[qname] = pos
						if structMethods[qname] == nil {
							structMethods[qname] = make(map[string]*ast.FuncDecl)
						}
					}
				}
			}
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recvType := cc.exprToString(d.Recv.List[0].Type)
				// Strip pointer if present
				recvType = strings.TrimPrefix(recvType, "*")
				qname := importPath + "." + recvType
				if structMethods[qname] == nil {
					structMethods[qname] = make(map[string]*ast.FuncDecl)
				}
				structMethods[qname][d.Name.Name] = d
			}
		}
	}
	return nil
}

// checkInterface 校验单个接口: 找出实现者, 并逐条记录缺失方法与签名不符。
//
// 无方法的接口会被记成 orphan_reference (warning 级) —— 那是"空接口占位"的典型形态,
// 真正实现它的结构体不会被认出来 (任何结构体都满足空方法集)。
func (cc *ConsistencyChecker) checkInterface(
	ifaceQName string,
	ifaceType *ast.InterfaceType,
	structs map[string]*ast.StructType,
	structMethods map[string]map[string]*ast.FuncDecl,
	interfacePositions map[string]token.Position,
	structPositions map[string]token.Position,
	packageForSymbol map[string]string,
) CheckerInterfaceCheck {
	pos := interfacePositions[ifaceQName]
	check := CheckerInterfaceCheck{
		InterfaceName: ifaceQName,
		Package:       packageForSymbol[ifaceQName],
		File:          pos.Filename,
		Line:          pos.Line,
		Methods:       len(ifaceType.Methods.List),
	}

	// Build interface method set
	ifaceMethods := make(map[string]*ast.Field)
	for _, method := range ifaceType.Methods.List {
		if len(method.Names) > 0 {
			ifaceMethods[method.Names[0].Name] = method
		}
	}

	// Find implementors
	for structQName, st := range structs {
		if cc.implements(st, structMethods[structQName], ifaceType, ifaceMethods) {
			check.Implementors = append(check.Implementors, structQName)
		}
	}

	// Check each implementor for missing methods or broken signatures
	for _, implQName := range check.Implementors {
		implMethods := structMethods[implQName]
		for mName, ifaceMethod := range ifaceMethods {
			implMethod, ok := implMethods[mName]
			if !ok {
				check.Issues = append(check.Issues, CheckerConsistencyIssue{
					Type:     "missing_method",
					Symbol:   implQName + "." + mName,
					File:     structPositions[implQName].Filename,
					Line:     structPositions[implQName].Line,
					Message:  fmt.Sprintf("struct %s is missing method %s required by interface %s", implQName, mName, ifaceQName),
					Severity: "error",
				})
				continue
			}
			if !cc.methodSignatureMatches(implMethod, ifaceMethod) {
				check.Issues = append(check.Issues, CheckerConsistencyIssue{
					Type:     "broken_signature",
					Symbol:   implQName + "." + mName,
					File:     cc.fset.Position(implMethod.Pos()).Filename,
					Line:     cc.fset.Position(implMethod.Pos()).Line,
					Message:  fmt.Sprintf("method %s on %s has a different signature than interface %s", mName, implQName, ifaceQName),
					Severity: "error",
				})
			}
		}
	}

	// Orphan reference check
	if len(ifaceType.Methods.List) == 0 {
		check.Issues = append(check.Issues, CheckerConsistencyIssue{
			Type:     "orphan_reference",
			Symbol:   ifaceQName,
			File:     pos.Filename,
			Line:     pos.Line,
			Message:  fmt.Sprintf("interface %s has no methods; possible orphan reference", ifaceQName),
			Severity: "warning",
		})
	}

	return check
}

// implements 判定方法集是否覆盖接口的全部方法名 (只看名字, 不看签名)。
//
// 注意：st 与 ifaceType 两个入参在函数体里**完全没有被使用** —— 判定只依赖
// methods 与 ifaceMethods, 保留它们是为了与调用点的形状一致。
func (cc *ConsistencyChecker) implements(
	st *ast.StructType,
	methods map[string]*ast.FuncDecl,
	ifaceType *ast.InterfaceType,
	ifaceMethods map[string]*ast.Field,
) bool {
	for mName := range ifaceMethods {
		if _, ok := methods[mName]; !ok {
			return false
		}
	}
	return true
}

// methodSignatureMatches 比较实现方法与接口方法的签名。
//
// 注意：这里**只比参数与返回值的个数**, 不比类型、不比名字、不比变参 ——
// `F(int) error` 与 `F(string) error` 会被判为匹配。要真比类型得引入类型解析
// (go/types), 代价远超本检查器的定位, 所以这条检查只当"粗筛"用。
func (cc *ConsistencyChecker) methodSignatureMatches(implMethod *ast.FuncDecl, ifaceMethod *ast.Field) bool {
	implType := implMethod.Type
	ifaceType, ok := ifaceMethod.Type.(*ast.FuncType)
	if !ok {
		return false
	}

	// Compare parameter count
	implParams := cc.countFields(implType.Params)
	ifaceParams := cc.countFields(ifaceType.Params)
	if implParams != ifaceParams {
		return false
	}

	// Compare result count
	implResults := cc.countFields(implType.Results)
	ifaceResults := cc.countFields(ifaceType.Results)
	if implResults != ifaceResults {
		return false
	}

	return true
}

// countFields 数一个参数/结果列表里的**变量个数**: `a, b int` 记 2, 匿名类型记 1。
func (cc *ConsistencyChecker) countFields(fl *ast.FieldList) int {
	if fl == nil {
		return 0
	}
	count := 0
	for _, field := range fl.List {
		if len(field.Names) == 0 {
			count++
		} else {
			count += len(field.Names)
		}
	}
	return count
}

// exprToString 把类型表达式还原成可读字符串 (仅用于诊断输出, 不参与判定)。
//
// 认不出的节点降级成 `<*ast.XXX>` 而不是空串 —— 空串会让两条不同的类型在日志里
// 长得一模一样。
func (cc *ConsistencyChecker) exprToString(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + cc.exprToString(e.X)
	case *ast.SelectorExpr:
		return cc.exprToString(e.X) + "." + e.Sel.Name
	case *ast.ArrayType:
		if e.Len == nil {
			return "[]" + cc.exprToString(e.Elt)
		}
		return "[" + cc.exprToString(e.Len) + "]" + cc.exprToString(e.Elt)
	case *ast.MapType:
		return "map[" + cc.exprToString(e.Key) + "]" + cc.exprToString(e.Value)
	case *ast.FuncType:
		return "func" + cc.funcTypeToString(e)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct{}"
	case *ast.Ellipsis:
		return "..." + cc.exprToString(e.Elt)
	case *ast.ParenExpr:
		return "(" + cc.exprToString(e.X) + ")"
	case *ast.BasicLit:
		return e.Value
	default:
		return fmt.Sprintf("<%T>", expr)
	}
}

// funcTypeToString 把函数类型渲染成 `(a int, b string) (int, error)` 形式。
//
// 多返回值或多命名的返回值会补一层括号, 与 Go 源码里的写法保持一致 (单值不补)。
func (cc *ConsistencyChecker) funcTypeToString(ft *ast.FuncType) string {
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
			typ := cc.exprToString(field.Type)
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
			typ := cc.exprToString(field.Type)
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
