// ast_parser.go — Tree-sitter AST 解析器。
//
// 支持语言: C, C++, Go, Java, JavaScript, TypeScript, Python, Rust
// 提取内容: 符号定义(函数/类型/变量)、调用关系、类型继承
//
// 零 LLM: 纯 tree-sitter 静态分析。
package codeintel

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	tstypescript "github.com/smacker/go-tree-sitter/typescript/typescript"
)

// Lang 支持的语言枚举。
type Lang string

const (
	LangC          Lang = "c"
	LangCpp        Lang = "cpp"
	LangGo         Lang = "go"
	LangJava       Lang = "java"
	LangJavaScript Lang = "javascript"
	LangTypeScript Lang = "typescript"
	LangPython     Lang = "python"
	LangRust       Lang = "rust"
)

// DetectLang 根据文件扩展名检测语言。
func DetectLang(path string) Lang {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".c":
		return LangC
	case ".cc", ".cpp", ".cxx", ".c++", ".hpp", ".hxx":
		return LangCpp
	case ".go":
		return LangGo
	case ".java":
		return LangJava
	case ".js", ".mjs", ".cjs":
		return LangJavaScript
	case ".ts", ".tsx", ".mts":
		return LangTypeScript
	case ".py", ".pyi":
		return LangPython
	case ".rs":
		return LangRust
	default:
		return ""
	}
}

func (l Lang) parser() *sitter.Parser {
	p := sitter.NewParser()
	switch l {
	case LangC:
		p.SetLanguage(c.GetLanguage())
	case LangCpp:
		p.SetLanguage(cpp.GetLanguage())
	case LangGo:
		p.SetLanguage(golang.GetLanguage())
	case LangJava:
		p.SetLanguage(java.GetLanguage())
	case LangJavaScript:
		p.SetLanguage(javascript.GetLanguage())
	case LangTypeScript:
		p.SetLanguage(tstypescript.GetLanguage())
	case LangPython:
		p.SetLanguage(python.GetLanguage())
	case LangRust:
		p.SetLanguage(rust.GetLanguage())
	default:
		return nil
	}
	return p
}

// ParsedFile 单个文件的解析结果。
type ParsedFile struct {
	Path     string      `json:"path"`
	Lang     Lang        `json:"lang"`
	Symbols  []SymbolRef `json:"symbols"`
	Calls    []CallRef   `json:"calls"`
	Types    []TypeRef   `json:"types"`
	Imports  []string    `json:"imports"`
}

// CallRef 调用关系。
type CallRef struct {
	Caller   string `json:"caller"`
	Callee   string `json:"callee"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
}

// TypeRef 类型关系。
type TypeRef struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // struct, class, interface, enum, typedef
	File     string `json:"file"`
	Line     int    `json:"line"`
	Parent   string `json:"parent,omitempty"` // 继承/实现的父类型
}

// ParseFiles 批量解析文件，返回每个文件的符号和调用关系。
func ParseFiles(files []string) ([]ParsedFile, error) {
	var results []ParsedFile
	for _, f := range files {
		pf, err := ParseFile(f)
		if err != nil {
			// 解析失败不阻断，记录错误继续
			continue
		}
		results = append(results, *pf)
	}
	return results, nil
}

// ParseFile 解析单个文件。
func ParseFile(path string) (*ParsedFile, error) {
	lang := DetectLang(path)
	if lang == "" {
		return nil, fmt.Errorf("unsupported language: %s", path)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	p := lang.parser()
	if p == nil {
		return nil, fmt.Errorf("no parser for language: %s", lang)
	}
	defer p.Close()

	tree, err := p.ParseCtx(nil, nil, content)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	if root == nil {
		return nil, fmt.Errorf("empty AST")
	}

	ctx := &parseContext{
		file:    path,
		lang:    lang,
		content: content,
	}
	walkNode(ctx, root)

	return &ParsedFile{
		Path:    path,
		Lang:    lang,
		Symbols: ctx.symbols,
		Calls:   ctx.calls,
		Types:   ctx.types,
		Imports: ctx.imports,
	}, nil
}

type parseContext struct {
	file     string
	lang     Lang
	content  []byte
	symbols  []SymbolRef
	calls    []CallRef
	types    []TypeRef
	imports  []string
	// 当前所在的函数/方法作用域
	scopeStack []string
}

func (ctx *parseContext) currentScope() string {
	if len(ctx.scopeStack) == 0 {
		return ""
	}
	return ctx.scopeStack[len(ctx.scopeStack)-1]
}

func (ctx *parseContext) pushScope(name string) {
	ctx.scopeStack = append(ctx.scopeStack, name)
}

func (ctx *parseContext) popScope() {
	if len(ctx.scopeStack) > 0 {
		ctx.scopeStack = ctx.scopeStack[:len(ctx.scopeStack)-1]
	}
}

func walkNode(ctx *parseContext, node *sitter.Node) {
	if node == nil {
		return
	}
	typ := node.Type()

	switch ctx.lang {
	case LangGo:
		walkGo(ctx, node, typ)
	case LangC, LangCpp:
		walkC(ctx, node, typ)
	case LangJava:
		walkJava(ctx, node, typ)
	case LangJavaScript, LangTypeScript:
		walkJS(ctx, node, typ)
	case LangPython:
		walkPython(ctx, node, typ)
	case LangRust:
		walkRust(ctx, node, typ)
	}

	// 递归遍历子节点
	for i := 0; i < int(node.ChildCount()); i++ {
		walkNode(ctx, node.Child(i))
	}
}

// ============================================================================
// Go
// ============================================================================

func walkGo(ctx *parseContext, node *sitter.Node, typ string) {
	switch typ {
	case "function_declaration", "method_declaration":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			name := nameNode.Content(ctx.content)
			ctx.symbols = append(ctx.symbols, SymbolRef{
				Name: name,
				Kind: "function",
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
			ctx.pushScope(name)
			defer ctx.popScope()
		}
	case "call_expression":
		funcNode := node.ChildByFieldName("function")
		if funcNode != nil {
			callee := funcNode.Content(ctx.content)
			ctx.calls = append(ctx.calls, CallRef{
				Caller: ctx.currentScope(),
				Callee: callee,
				File:   ctx.file,
				Line:   int(node.StartPoint().Row) + 1,
				Col:    int(node.StartPoint().Column),
			})
		}
	case "type_spec":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			ctx.types = append(ctx.types, TypeRef{
				Name: nameNode.Content(ctx.content),
				Kind: "type",
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
		}
	case "import_declaration":
		pathNode := node.ChildByFieldName("path")
		if pathNode != nil {
			imp := strings.Trim(pathNode.Content(ctx.content), `"`)
			ctx.imports = append(ctx.imports, imp)
		}
	}
}

// ============================================================================
// C / C++
// ============================================================================

func walkC(ctx *parseContext, node *sitter.Node, typ string) {
	switch typ {
	case "function_definition":
		declarator := node.ChildByFieldName("declarator")
		if declarator != nil {
			name := extractDeclaratorName(ctx, declarator)
			if name != "" {
				ctx.symbols = append(ctx.symbols, SymbolRef{
					Name: name,
					Kind: "function",
					File: ctx.file,
					Line: int(declarator.StartPoint().Row) + 1,
				})
				ctx.pushScope(name)
				defer ctx.popScope()
			}
		}
	case "call_expression":
		funcNode := node.ChildByFieldName("function")
		if funcNode != nil {
			callee := funcNode.Content(ctx.content)
			ctx.calls = append(ctx.calls, CallRef{
				Caller: ctx.currentScope(),
				Callee: callee,
				File:   ctx.file,
				Line:   int(node.StartPoint().Row) + 1,
				Col:    int(node.StartPoint().Column),
			})
		}
	case "struct_specifier", "class_specifier":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			kind := "struct"
			if typ == "class_specifier" {
				kind = "class"
			}
			ctx.types = append(ctx.types, TypeRef{
				Name: nameNode.Content(ctx.content),
				Kind: kind,
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
		}
	case "preproc_include":
		// #include "..."
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child != nil && child.Type() == "string_literal" {
				imp := strings.Trim(child.Content(ctx.content), `"<>`)
				ctx.imports = append(ctx.imports, imp)
			}
		}
	}
}

func extractDeclaratorName(ctx *parseContext, node *sitter.Node) string {
	if node == nil {
		return ""
	}
	if node.Type() == "identifier" {
		return node.Content(ctx.content)
	}
	// 递归查找 identifier
	for i := 0; i < int(node.ChildCount()); i++ {
		name := extractDeclaratorName(ctx, node.Child(i))
		if name != "" {
			return name
		}
	}
	return ""
}

// ============================================================================
// Java
// ============================================================================

func walkJava(ctx *parseContext, node *sitter.Node, typ string) {
	switch typ {
	case "method_declaration":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			name := nameNode.Content(ctx.content)
			ctx.symbols = append(ctx.symbols, SymbolRef{
				Name: name,
				Kind: "method",
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
			ctx.pushScope(name)
			defer ctx.popScope()
		}
	case "class_declaration":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			ctx.types = append(ctx.types, TypeRef{
				Name: nameNode.Content(ctx.content),
				Kind: "class",
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
		}
	case "method_invocation":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			ctx.calls = append(ctx.calls, CallRef{
				Caller: ctx.currentScope(),
				Callee: nameNode.Content(ctx.content),
				File:   ctx.file,
				Line:   int(node.StartPoint().Row) + 1,
				Col:    int(node.StartPoint().Column),
			})
		}
	}
}

// ============================================================================
// JavaScript / TypeScript
// ============================================================================

func walkJS(ctx *parseContext, node *sitter.Node, typ string) {
	switch typ {
	case "function_declaration", "method_definition":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			name := nameNode.Content(ctx.content)
			ctx.symbols = append(ctx.symbols, SymbolRef{
				Name: name,
				Kind: "function",
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
			ctx.pushScope(name)
			defer ctx.popScope()
		}
	case "call_expression":
		funcNode := node.ChildByFieldName("function")
		if funcNode != nil {
			ctx.calls = append(ctx.calls, CallRef{
				Caller: ctx.currentScope(),
				Callee: funcNode.Content(ctx.content),
				File:   ctx.file,
				Line:   int(node.StartPoint().Row) + 1,
				Col:    int(node.StartPoint().Column),
			})
		}
	case "class_declaration", "class":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			ctx.types = append(ctx.types, TypeRef{
				Name: nameNode.Content(ctx.content),
				Kind: "class",
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
		}
	case "import_statement":
		spec := node.ChildByFieldName("source")
		if spec != nil {
			imp := strings.Trim(spec.Content(ctx.content), `"'`)
			ctx.imports = append(ctx.imports, imp)
		}
	}
}

// ============================================================================
// Python
// ============================================================================

func walkPython(ctx *parseContext, node *sitter.Node, typ string) {
	switch typ {
	case "function_definition":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			name := nameNode.Content(ctx.content)
			ctx.symbols = append(ctx.symbols, SymbolRef{
				Name: name,
				Kind: "function",
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
			ctx.pushScope(name)
			defer ctx.popScope()
		}
	case "call":
		funcNode := node.ChildByFieldName("function")
		if funcNode != nil {
			ctx.calls = append(ctx.calls, CallRef{
				Caller: ctx.currentScope(),
				Callee: funcNode.Content(ctx.content),
				File:   ctx.file,
				Line:   int(node.StartPoint().Row) + 1,
				Col:    int(node.StartPoint().Column),
			})
		}
	case "class_definition":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			ctx.types = append(ctx.types, TypeRef{
				Name: nameNode.Content(ctx.content),
				Kind: "class",
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
		}
	case "import_statement", "import_from_statement":
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child != nil && child.Type() == "dotted_name" {
				ctx.imports = append(ctx.imports, child.Content(ctx.content))
			}
		}
	}
}

// ============================================================================
// Rust
// ============================================================================

func walkRust(ctx *parseContext, node *sitter.Node, typ string) {
	switch typ {
	case "function_item":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			name := nameNode.Content(ctx.content)
			ctx.symbols = append(ctx.symbols, SymbolRef{
				Name: name,
				Kind: "function",
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
			ctx.pushScope(name)
			defer ctx.popScope()
		}
	case "call_expression":
		funcNode := node.ChildByFieldName("function")
		if funcNode != nil {
			ctx.calls = append(ctx.calls, CallRef{
				Caller: ctx.currentScope(),
				Callee: funcNode.Content(ctx.content),
				File:   ctx.file,
				Line:   int(node.StartPoint().Row) + 1,
				Col:    int(node.StartPoint().Column),
			})
		}
	case "struct_item", "enum_item", "trait_item":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			kind := "struct"
			if typ == "enum_item" {
				kind = "enum"
			} else if typ == "trait_item" {
				kind = "trait"
			}
			ctx.types = append(ctx.types, TypeRef{
				Name: nameNode.Content(ctx.content),
				Kind: kind,
				File: ctx.file,
				Line: int(nameNode.StartPoint().Row) + 1,
			})
		}
	case "use_declaration":
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child != nil && (child.Type() == "scoped_identifier" || child.Type() == "identifier") {
				ctx.imports = append(ctx.imports, child.Content(ctx.content))
			}
		}
	}
}
