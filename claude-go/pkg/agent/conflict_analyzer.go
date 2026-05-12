// Package agent implements the ConflictAnalyzer for smart conflict key optimization.
// Phase 3.3: symbol-level dependency analysis to replace file-level dependencies.
package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

// ConflictAnalyzer analyzes source files to produce fine-grained symbol-level
// conflict keys, enabling higher parallelism than coarse file-level keys.
type ConflictAnalyzer struct{}

// NewConflictAnalyzer creates a new analyzer.
func NewConflictAnalyzer() *ConflictAnalyzer {
	return &ConflictAnalyzer{}
}

// SymbolRef represents a symbol reference within a file.
type SymbolRef struct {
	File      string
	Package   string
	Name      string
	Kind      string // "type", "func", "var", "const", "method", "interface"
	Position  int
}

// AnalyzeFile extracts symbol-level conflict keys from a Go source file.
// Returns a list of keys like "pkg.TypeName", "pkg.FuncName" that can be
// used instead of the coarse file path as conflict keys.
func (ca *ConflictAnalyzer) AnalyzeFile(filePath string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	pkgName := f.Name.Name
	var keys []string

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil {
				// Method: pkg.Type.Method
				recvType := extractReceiverType(d.Recv)
				if recvType != "" {
					keys = append(keys, pkgName+"."+recvType+"."+d.Name.Name)
					keys = append(keys, pkgName+"."+recvType) // type-level key
				}
			} else {
				keys = append(keys, pkgName+"."+d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					keys = append(keys, pkgName+"."+s.Name.Name)
				case *ast.ValueSpec:
					for _, name := range s.Names {
						keys = append(keys, pkgName+"."+name.Name)
					}
				}
			}
		}
	}

	return caUniqueStrings(keys), nil
}

// extractReceiverType extracts the type name from a receiver list.
func extractReceiverType(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	return exprToString(expr)
}

// exprToString converts an AST expression to its string representation.
func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return exprToString(e.X)
	case *ast.IndexExpr:
		return exprToString(e.X)
	case *ast.ArrayType:
		return exprToString(e.Elt)
	default:
		return ""
	}
}

// SmartConflictKeysForTask replaces coarse file-level conflict keys with
// symbol-level keys for read-only shared tasks.
// If a task only reads symbols from a file (does not write), it gets
// symbol-level keys, allowing parallel reads of different symbols in the same file.
func (ca *ConflictAnalyzer) SmartConflictKeysForTask(task rawTask, fileSymbols map[string][]string) []string {
	if !isReadOnlyRawTask(task) {
		// Write tasks keep file-level keys for safety
		return rawTaskConflictKeys(task)
	}

	var symbolKeys []string
	for _, f := range task.targetFiles {
		f = filepath.ToSlash(f)
		syms, ok := fileSymbols[f]
		if !ok || len(syms) == 0 {
			// Fallback to file-level key if we don't have symbols
			symbolKeys = append(symbolKeys, f)
			continue
		}
		// Read-only task gets all symbol keys from the file
		symbolKeys = append(symbolKeys, syms...)
	}
	for _, f := range task.readFiles {
		f = filepath.ToSlash(f)
		syms, ok := fileSymbols[f]
		if !ok || len(syms) == 0 {
			symbolKeys = append(symbolKeys, f)
			continue
		}
		symbolKeys = append(symbolKeys, syms...)
	}
	return caUniqueStrings(symbolKeys)
}

// BuildFileSymbolMap scans a set of Go files and builds a map from file path
// to symbol-level conflict keys.
func (ca *ConflictAnalyzer) BuildFileSymbolMap(files map[string][]byte) map[string][]string {
	result := make(map[string][]string)
	for path, src := range files {
		keys, err := ca.AnalyzeFile(path, src)
		if err != nil {
			continue
		}
		result[path] = keys
	}
	return result
}

// caUniqueStrings deduplicates strings.
func caUniqueStrings(ss []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// StripTestSuffix removes _test.go suffix for conflict key grouping.
func StripTestSuffix(path string) string {
	if strings.HasSuffix(path, "_test.go") {
		return strings.TrimSuffix(path, "_test.go") + ".go"
	}
	return path
}
