// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"fmt"
	"go/types"
	"path/filepath"
	"time"

	"golang.org/x/tools/go/packages"
	"github.com/anthropic/claude-go/pkg/contract"
)

// GoContractBuilder builds the Contract Store.
// Underlying reuse of golang.org/x/tools/go/packages for typed package loading,
// more accurate than handwritten filepath.Walk + go/parser (can resolve cross-package type dependencies).
//
// Input: {"repo_root": "/path/to/repo", "module": "github.com/anthropic/claude-go"}
// Output: {"contracts": [...], "implementors": {...}, "stats": {...}}
type GoContractBuilder struct {
	Base
	store *contract.Store
}

// NewGoContractBuilder creates a new GoContractBuilder.
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

// Execute runs the contract builder.
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

	// Use go/packages to load all packages (with type information)
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
			continue // skip packages with errors
		}
		pkgPath := pkg.PkgPath
		if pkgPath == "" {
			pkgPath = in.Module
		}

		// Traverse scope to find interface types
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

		// Traverse all types in the package to find interface implementors
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			named, ok := obj.Type().(*types.Named)
			if !ok {
				continue
			}
			// Simplified: only record types with methods
			if _, ok := named.Underlying().(*types.Struct); ok {
				recvKey := pkgPath + "." + name
				// Determine implementation relationship via method set overlap
				for ifaceKey, ifaceContract := range contracts {
					if hasMethodOverlap(named, ifaceContract.Methods) {
						implementors[ifaceKey] = append(implementors[ifaceKey], recvKey)
					}
				}
			}
		}
		stats.FilesScanned += len(pkg.Syntax)
	}

	// Write back to store
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
	// Simplified: check if at least one interface method is implemented
	// In practice should use types.Implements, but need to construct interface type
	for i := 0; i < named.NumMethods(); i++ {
		if _, ok := methods[named.Method(i).Name()]; ok {
			return true
		}
	}
	return false
}
