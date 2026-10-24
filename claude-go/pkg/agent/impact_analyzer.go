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
)

// ImpactReport aggregates the results of an impact analysis.
type ImpactReport struct {
	ChangedFiles     []string          `json:"changed_files"`
	ModifiedSymbols  []string          `json:"modified_symbols"`
	References       []SymbolReference `json:"references"`
	AffectedPackages []string          `json:"affected_packages"`
	AffectedFiles    []string          `json:"affected_files"`
}

// SymbolReference represents a reference to a modified symbol.
type SymbolReference struct {
	Symbol  string `json:"symbol"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Context string `json:"context"` // e.g., "func_call", "type_use", "assignment"
}

// ImpactAnalyzer parses changed files to find modified symbols and searches for references.
type ImpactAnalyzer struct {
	root   string
	module string
	mu     sync.RWMutex
	fset   *token.FileSet
}

// NewImpactAnalyzer creates a new ImpactAnalyzer for the given root directory.
// NewImpactAnalyzer 构造 ImpactAnalyzer, 绑定仓库根目录与 Go module 名。
func NewImpactAnalyzer(root, module string) *ImpactAnalyzer {
	return &ImpactAnalyzer{
		root:   root,
		module: module,
		fset:   token.NewFileSet(),
	}
}

// Analyze parses the given changed files to find modified symbols and searches for references.
// It returns an ImpactReport containing all found references.
// Analyze 解析变更文件找出被修改的符号, 再全仓搜索引用, 汇总成影响面报告。
//
// 算法解读：
//
//	原理：两趟处理——第一趟解析每个变更文件, 用 AST 抽取「导入路径.接收者.符号名」形式的全限定名；
//	      第二趟从仓库根目录遍历全部 .go 文件, 把每个标识符与第一趟得到的符号集合求交。
//	关键不变量：符号全名必须由「导入路径 + 符号名」共同构成——只按短名匹配会把不同包下的同名符号混为一谈。
//	代价与边界：第二趟是 O(全部 .go 文件) 的全仓遍历加逐文件重新解析, 无缓存；单文件读取/解析失败会被静默跳过, 因此报告是「下界」而非完备集。
//	设计权衡：用全量重解析换取零索引维护成本, 适合变更文件数少的场景；仓库规模上来后需要换成持久化索引。
func (ia *ImpactAnalyzer) Analyze(changedFiles []string) (*ImpactReport, error) {
	ia.mu.Lock()
	defer ia.mu.Unlock()

	if ia.module == "" {
		ia.module = ia.readGoModModule()
	}

	report := &ImpactReport{
		ChangedFiles: changedFiles,
	}

	// Parse changed files to find modified symbols
	modifiedSymbols := make(map[string]bool)
	affectedPackages := make(map[string]bool)

	for _, file := range changedFiles {
		file = filepath.Clean(file)
		// 非 Go 文件与 _test.go 不参与影响面计算: 测试文件改动不构成对外影响。
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}

		symbols, pkg, err := ia.parseChangedFile(file)
		if err != nil {
			continue
		}
		for _, sym := range symbols {
			modifiedSymbols[sym] = true
		}
		if pkg != "" {
			affectedPackages[pkg] = true
		}
	}

	for sym := range modifiedSymbols {
		report.ModifiedSymbols = append(report.ModifiedSymbols, sym)
	}
	sort.Strings(report.ModifiedSymbols)

	// Search for references to modified symbols across the codebase
	// 只有确实找到被改符号时才发起全仓扫描, 否则直接返回空报告。
	if len(modifiedSymbols) > 0 {
		refs, affectedPkgs, affectedFiles := ia.findReferences(modifiedSymbols)
		report.References = refs
		for pkg := range affectedPkgs {
			affectedPackages[pkg] = true
		}
		report.AffectedPackages = make([]string, 0, len(affectedPackages))
		for pkg := range affectedPackages {
			report.AffectedPackages = append(report.AffectedPackages, pkg)
		}
		sort.Strings(report.AffectedPackages)
		report.AffectedFiles = affectedFiles
	}

	return report, nil
}

// readGoModModule 从 <root>/go.mod 读取 module 名; 读不到或没有 module 行时返回空串。
func (ia *ImpactAnalyzer) readGoModModule() string {
	data, err := os.ReadFile(filepath.Join(ia.root, "go.mod"))
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

// parseChangedFile parses a single changed file and extracts modified symbols.
// parseChangedFile 解析单个变更文件, 抽取其声明的符号全限定名列表, 并返回该文件所属的导入路径。
//
// 算法解读：
//
//	原理：遍历顶层声明——FuncDecl 取函数名(带接收者时拼成 接收者.方法名), GenDecl 取 TypeSpec/ValueSpec 里的名字, 再统一前置导入路径。
//	关键不变量：导入路径必须由「module 名 + 相对目录」拼出且用 / 归一化——少拼 module 前缀会让后续引用匹配全部落空。
//	代价与边界：只看顶层声明, 方法体内的局部符号与匿名结构体字段不会被登记；AllErrors 模式下语法有错仍尽力返回已解析部分。
//	设计权衡：不做类型检查、不解析跨文件符号表, 用最小代价拿到「变更文件声明了什么」这一层信息。
func (ia *ImpactAnalyzer) parseChangedFile(file string) ([]string, string, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, "", err
	}

	f, err := parser.ParseFile(ia.fset, file, src, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return nil, "", err
	}

	dir := filepath.Dir(file)
	relDir, _ := filepath.Rel(ia.root, dir)
	importPath := filepath.ToSlash(relDir)
	if ia.module != "" {
		importPath = ia.module + "/" + importPath
	}

	var symbols []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recvType := ia.exprToString(d.Recv.List[0].Type)
				recvType = strings.TrimPrefix(recvType, "*")
				symbols = append(symbols, importPath+"."+recvType+"."+d.Name.Name)
			} else {
				symbols = append(symbols, importPath+"."+d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					symbols = append(symbols, importPath+"."+ts.Name.Name)
				}
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, name := range vs.Names {
						symbols = append(symbols, importPath+"."+name.Name)
					}
				}
			}
		}
	}

	return symbols, importPath, nil
}

// findReferences searches the entire codebase for references to the given symbols.
// findReferences 全仓遍历 .go 文件, 查找对给定符号集合的引用, 返回引用明细、受影响包与受影响文件。
//
// 算法解读：
//
//	原理：先把全限定名按短名(最后一段)倒排成 short→[全限定名] 索引；遍历时对每个标识符做一次 O(1) 短名查表, 命中后再展开为该短名下的全部候选。
//	关键不变量：倒排表必须先于遍历构建完毕——它同时充当「哪些标识符值得记录」的过滤器, 否则遍历会退化成记录所有标识符。
//	代价与边界：倒排表按「同名符号数」放大结果, 一个常见短名(如 String)会产出大量候选引用, 精确性依赖调用方按全限定名二次过滤。
//	设计权衡：用「短名粗筛 + 全名展开」代替「解析每个文件的导入表再精确匹配」, 省掉类型检查但引入同名误报。
func (ia *ImpactAnalyzer) findReferences(symbols map[string]bool) ([]SymbolReference, map[string]bool, []string) {
	var refs []SymbolReference
	affectedPackages := make(map[string]bool)
	affectedFiles := make(map[string]bool)

	// Build a map of short names to full qualified names for faster lookup
	shortToFull := make(map[string][]string)
	for sym := range symbols {
		parts := strings.Split(sym, ".")
		if len(parts) > 0 {
			short := parts[len(parts)-1]
			shortToFull[short] = append(shortToFull[short], sym)
		}
	}

	// vendor/.git/node_modules 等目录不参与扫描, 避免把第三方代码计入影响面。
	_ = filepath.Walk(ia.root, func(path string, info os.FileInfo, err error) error {
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

		dir := filepath.Dir(path)
		relDir, _ := filepath.Rel(ia.root, dir)
		importPath := filepath.ToSlash(relDir)
		if ia.module != "" {
			importPath = ia.module + "/" + importPath
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		f, err := parser.ParseFile(ia.fset, path, src, parser.ParseComments|parser.AllErrors)
		if err != nil {
			return nil
		}

		// Walk the AST to find references
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.Ident:
				if candidates, ok := shortToFull[node.Name]; ok {
					pos := ia.fset.Position(node.Pos())
					// Determine context
					context := ia.determineContext(node)
					for _, candidate := range candidates {
						refs = append(refs, SymbolReference{
							Symbol:  candidate,
							File:    pos.Filename,
							Line:    pos.Line,
							Column:  pos.Column,
							Context: context,
						})
						affectedPackages[importPath] = true
						affectedFiles[pos.Filename] = true
					}
				}
			}
			return true
		})

		return nil
	})

	// Convert affectedFiles map to slice
	var files []string
	for f := range affectedFiles {
		files = append(files, f)
	}
	sort.Strings(files)

	// Sort references for deterministic output
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Symbol != refs[j].Symbol {
			return refs[i].Symbol < refs[j].Symbol
		}
		if refs[i].File != refs[j].File {
			return refs[i].File < refs[j].File
		}
		return refs[i].Line < refs[j].Line
	})

	return refs, affectedPackages, files
}

// determineContext tries to determine the usage context of an identifier.
// determineContext 判定标识符的使用场景; 注意：当前是桩实现, 恒返回 "reference", 并未真正回溯父节点。
func (ia *ImpactAnalyzer) determineContext(ident *ast.Ident) string {
	// This is a simplified context detection
	// In a full implementation, we'd walk up the AST to find the parent node
	return "reference"
}

// exprToString 把 AST 表达式节点还原成源码形式的类型字符串, 用于拼接符号全名。
func (ia *ImpactAnalyzer) exprToString(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + ia.exprToString(e.X)
	case *ast.SelectorExpr:
		return ia.exprToString(e.X) + "." + e.Sel.Name
	case *ast.ArrayType:
		if e.Len == nil {
			return "[]" + ia.exprToString(e.Elt)
		}
		return "[" + ia.exprToString(e.Len) + "]" + ia.exprToString(e.Elt)
	case *ast.MapType:
		return "map[" + ia.exprToString(e.Key) + "]" + ia.exprToString(e.Value)
	case *ast.FuncType:
		return "func" + ia.funcTypeToString(e)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct{}"
	case *ast.Ellipsis:
		return "..." + ia.exprToString(e.Elt)
	case *ast.ParenExpr:
		return "(" + ia.exprToString(e.X) + ")"
	case *ast.BasicLit:
		return e.Value
	default:
		return fmt.Sprintf("<%T>", expr)
	}
}

// funcTypeToString 还原函数签名的参数与返回值列表, 供 exprToString 拼出 func 类型字符串。
func (ia *ImpactAnalyzer) funcTypeToString(ft *ast.FuncType) string {
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
			typ := ia.exprToString(field.Type)
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
			typ := ia.exprToString(field.Type)
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
