// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import "context"

// StaticcheckAdapter wraps the staticcheck tool.
type StaticcheckAdapter struct {
	cap *ToolCapability
}

func (a *StaticcheckAdapter) Name() string       { return "staticcheck" }
func (a *StaticcheckAdapter) Capabilities() []string { return []string{"static-check"} }

func (a *StaticcheckAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: a.cap.Path, outputFmt: "json"}
	parser := &StaticcheckJSONParser{}
	return base.ExecAndParse(ctx, []string{"-f", "json", "./..."}, args.RepoRoot, parser)
}

// GolangCILintAdapter wraps golangci-lint.
type GolangCILintAdapter struct {
	cap *ToolCapability
}

func (a *GolangCILintAdapter) Name() string       { return "golangci-lint" }
func (a *GolangCILintAdapter) Capabilities() []string { return []string{"static-check"} }

func (a *GolangCILintAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: a.cap.Path, outputFmt: "json"}
	parser := &GolangCILintParser{}
	return base.ExecAndParse(ctx, []string{"run", "--out-format=json", "./..."}, args.RepoRoot, parser)
}

// GoBuildAdapter wraps go build.
type GoBuildAdapter struct{}

func (a *GoBuildAdapter) Name() string       { return "go-build" }
func (a *GoBuildAdapter) Capabilities() []string { return []string{"build", "static-check"} }

func (a *GoBuildAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: "go", outputFmt: "text"}
	parser := &GoBuildTextParser{}
	return base.ExecAndParse(ctx, []string{"build", "./..."}, args.RepoRoot, parser)
}

// GoFormatAdapter wraps gofmt.
type GoFormatAdapter struct {
	detector *ToolDetector
}

func (a *GoFormatAdapter) Name() string       { return "go-format" }
func (a *GoFormatAdapter) Capabilities() []string { return []string{"format"} }

func (a *GoFormatAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: "gofmt", outputFmt: "text"}
	// gofmt -l . outputs unformatted file list
	cmdArgs := []string{"-l", "."}
	if len(args.Files) > 0 {
		cmdArgs = append([]string{"-l"}, args.Files...)
	}
	result, err := base.ExecAndParse(ctx, cmdArgs, args.RepoRoot, &GoBuildTextParser{})
	if result != nil {
		// gofmt success but has output = unformatted files exist
		result.CheckPassed = result.RawOutput == ""
	}
	return result, err
}

// GoImportsAdapter wraps goimports.
type GoImportsAdapter struct {
	detector *ToolDetector
}

func (a *GoImportsAdapter) Name() string       { return "goimports" }
func (a *GoImportsAdapter) Capabilities() []string { return []string{"import-fix", "format"} }

func (a *GoImportsAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	cmd := "goimports"
	if _, ok := a.detector.Get("goimports"); !ok {
		// fallback to gofmt (can only format, cannot fix imports)
		cmd = "gofmt"
	}
	base := BaseAdapter{cmd: cmd, outputFmt: "text"}

	cmdArgs := []string{"-w"} // write changes
	if len(args.Files) > 0 {
		cmdArgs = append(cmdArgs, args.Files...)
	} else {
		cmdArgs = append(cmdArgs, ".")
	}

	result, err := base.ExecAndParse(ctx, cmdArgs, args.RepoRoot, &GoBuildTextParser{})
	if result != nil {
		result.CheckPassed = err == nil
	}
	return result, err
}
