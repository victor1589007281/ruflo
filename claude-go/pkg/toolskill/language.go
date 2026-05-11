// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

// LanguageRegistry language configuration registry.
type LanguageRegistry struct {
	profiles map[string]*LanguageProfile
}

// NewLanguageRegistry creates a new LanguageRegistry with default profiles.
func NewLanguageRegistry() *LanguageRegistry {
	reg := &LanguageRegistry{profiles: make(map[string]*LanguageProfile)}
	reg.Register(GoProfile())
	return reg
}

// Register registers a language profile.
func (r *LanguageRegistry) Register(p *LanguageProfile) {
	r.profiles[p.Name] = p
}

// Get retrieves a language profile by name.
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
