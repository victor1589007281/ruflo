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
	ScannedAt    time.Time               `json:"scanned_at"`
	Packages     int                     `json:"packages"`
	Interfaces   int                     `json:"interfaces"`
	Implementors int                     `json:"implementors"`
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

// NewConsistencyChecker creates a new ConsistencyChecker for the given root directory.
func NewConsistencyChecker(root, module string) *ConsistencyChecker {
	return &ConsistencyChecker{
		root:   root,
		module: module,
		fset:   token.NewFileSet(),
	}
}

// Check scans all Go files under the root directory and verifies interface implementations.
// It returns a CheckerGlobalConsistencyReport containing all found issues.
func (cc *ConsistencyChecker) Check() (*CheckerGlobalConsistencyReport, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if cc.module == "" {
		cc.module = cc.readGoModModule()
	}

	// First pass: collect all interfaces and structs
	interfaces := make(map[string]*ast.InterfaceType) // pkg.Name -> *ast.InterfaceType
	structs := make(map[string]*ast.StructType)       // pkg.Name -> *ast.StructType
	structMethods := make(map[string]map[string]*ast.FuncDecl) // pkg.Name -> methodName -> *ast.FuncDecl
	interfacePositions := make(map[string]token.Position)
	structPositions := make(map[string]token.Position)
	packageForSymbol := make(map[string]string)

	err := filepath.Walk(cc.root, func(path string, info os.FileInfo, err error) error {
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
		report.Implementors += len(check.Implementors)
		if len(check.Issues) > 0 {
			report.Issues = append(report.Issues, check.Issues...)
		}
		report.Checks = append(report.Checks, check)
	}

	// Sort for deterministic output
	sort.Slice(report.Issues, func(i, j int) bool {
		if report.Issues[i].Severity != report.Issues[j].Severity {
			return report.Issues[i].Severity == "error"
		}
		return report.Issues[i].Symbol < report.Issues[j].Symbol
	})

	return report, nil
}

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
