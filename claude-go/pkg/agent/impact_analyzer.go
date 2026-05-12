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
	ChangedFiles   []string          `json:"changed_files"`
	ModifiedSymbols []string         `json:"modified_symbols"`
	References     []SymbolReference `json:"references"`
	AffectedPackages []string        `json:"affected_packages"`
	AffectedFiles  []string          `json:"affected_files"`
}

// SymbolReference represents a reference to a modified symbol.
type SymbolReference struct {
	Symbol   string `json:"symbol"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Context  string `json:"context"` // e.g., "func_call", "type_use", "assignment"
}

// ImpactAnalyzer parses changed files to find modified symbols and searches for references.
type ImpactAnalyzer struct {
	root   string
	module string
	mu     sync.RWMutex
	fset   *token.FileSet
}

// NewImpactAnalyzer creates a new ImpactAnalyzer for the given root directory.
func NewImpactAnalyzer(root, module string) *ImpactAnalyzer {
	return &ImpactAnalyzer{
		root:   root,
		module: module,
		fset:   token.NewFileSet(),
	}
}

// Analyze parses the given changed files to find modified symbols and searches for references.
// It returns an ImpactReport containing all found references.
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
func (ia *ImpactAnalyzer) determineContext(ident *ast.Ident) string {
	// This is a simplified context detection
	// In a full implementation, we'd walk up the AST to find the parent node
	return "reference"
}

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
