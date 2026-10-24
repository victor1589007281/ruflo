// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

// LanguageRegistry language configuration registry.
type LanguageRegistry struct {
	profiles map[string]*LanguageProfile
}

// NewLanguageRegistry creates a new LanguageRegistry with default profiles.
// 构造语言注册表并预置 Go profile。
// 注意：Python/Rust profile 虽已定义, 但**未在此注册** —— 需要时得显式 Register。
func NewLanguageRegistry() *LanguageRegistry {
	reg := &LanguageRegistry{profiles: make(map[string]*LanguageProfile)}
	reg.Register(GoProfile())
	return reg
}

// Register registers a language profile.
// 以 profile 的 Name 为键登记 (同名覆盖, 不做冲突检测)。
func (r *LanguageRegistry) Register(p *LanguageProfile) {
	r.profiles[p.Name] = p
}

// Get retrieves a language profile by name.
// 按名取语言 profile; 第二个返回值表示是否存在。
func (r *LanguageRegistry) Get(name string) (*LanguageProfile, bool) {
	p, ok := r.profiles[name]
	return p, ok
}

// LanguageProfile full language configuration.
type LanguageProfile struct {
	Name       string           `json:"name"`       // "go", "python", "rust", "typescript"
	Extensions []string         `json:"extensions"` // [".go"]
	BuildCmd   string           `json:"build_cmd"`  // "go build ./..."
	TestCmd    string           `json:"test_cmd"`   // "go test ./..."
	Tools      []ToolPreference `json:"tools"`      // tool priority list
}

// ToolPreference tool priority configuration.
type ToolPreference struct {
	Name       string   `json:"name"`        // "staticcheck"
	Priority   int      `json:"priority"`    // 1 = highest
	Cmd        string   `json:"cmd"`         // "staticcheck"
	Args       []string `json:"args"`        // ["-f", "json", "./..."]
	OutputFmt  string   `json:"output_fmt"`  // "json" | "text" | "checkstyle"
	FallbackTo string   `json:"fallback_to"` // which tool to fallback to when unavailable
}

// GoProfile Go language default configuration.
// 返回 Go 的默认语言配置。
//
// 注意：Tools 里有两组 Priority 都是 1 的条目 (staticcheck 与 gofmt/goimports) ——
// 它们是**不同类别**的工具 (检查器 vs 格式化器), Priority 只在本类别内排序,
// 不要跨类别比大小。
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

// PythonProfile Python language configuration (placeholder example).
// 返回 Python 的默认语言配置 (占位示例, 未在 NewLanguageRegistry 里注册)。
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

// RustProfile Rust language configuration (placeholder example).
// 返回 Rust 的默认语言配置 (占位示例, 未在 NewLanguageRegistry 里注册)。
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
