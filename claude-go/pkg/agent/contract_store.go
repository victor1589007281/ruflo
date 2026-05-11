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

// CallerRef 调用者引用
type CallerRef struct {
	Package string `json:"package"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Code    string `json:"code,omitempty"`
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
