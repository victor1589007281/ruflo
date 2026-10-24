// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import "context"

// StaticcheckAdapter wraps the staticcheck tool.
type StaticcheckAdapter struct {
	cap *ToolCapability
}

// Name 返回适配器标识名 "staticcheck"。
func (a *StaticcheckAdapter) Name() string       { return "staticcheck" }
// Capabilities 返回该适配器提供的能力标签 (静态检查)。
func (a *StaticcheckAdapter) Capabilities() []string { return []string{"static-check"} }

// Execute 在 args.RepoRoot 下执行 staticcheck -f json ./..., 交给 StaticcheckJSONParser 解析成诊断项。
func (a *StaticcheckAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: a.cap.Path, outputFmt: "json"}
	parser := &StaticcheckJSONParser{}
	return base.ExecAndParse(ctx, []string{"-f", "json", "./..."}, args.RepoRoot, parser)
}

// GolangCILintAdapter wraps golangci-lint.
type GolangCILintAdapter struct {
	cap *ToolCapability
}

// Name 返回适配器标识名 "golangci-lint"。
func (a *GolangCILintAdapter) Name() string       { return "golangci-lint" }
// Capabilities 返回该适配器提供的能力标签 (静态检查)。
func (a *GolangCILintAdapter) Capabilities() []string { return []string{"static-check"} }

// Execute 在 args.RepoRoot 下执行 golangci-lint run --out-format=json ./..., 交给 GolangCILintParser 解析。
func (a *GolangCILintAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: a.cap.Path, outputFmt: "json"}
	parser := &GolangCILintParser{}
	return base.ExecAndParse(ctx, []string{"run", "--out-format=json", "./..."}, args.RepoRoot, parser)
}

// GoBuildAdapter wraps go build.
type GoBuildAdapter struct{}

// Name 返回适配器标识名 "go-build"。
func (a *GoBuildAdapter) Name() string       { return "go-build" }
// Capabilities 返回该适配器提供的能力标签 (编译 + 静态检查)。
func (a *GoBuildAdapter) Capabilities() []string { return []string{"build", "static-check"} }

// Execute 在 args.RepoRoot 下执行 go build ./..., 交给 GoBuildTextParser 解析文本诊断。
//
// 注意：命令行写死 "go" 而非 a.cap.Path —— 与其余适配器不同, 这里不读 ToolCapability
// 里探测到的路径, 故 --go 之类的自定义工具路径对 build 这条不生效。
func (a *GoBuildAdapter) Execute(ctx context.Context, args AdapterArgs) (*AdapterResult, error) {
	base := BaseAdapter{cmd: "go", outputFmt: "text"}
	parser := &GoBuildTextParser{}
	return base.ExecAndParse(ctx, []string{"build", "./..."}, args.RepoRoot, parser)
}

// GoFormatAdapter wraps gofmt.
type GoFormatAdapter struct {
	detector *ToolDetector
}

// Name 返回适配器标识名 "go-format"。
func (a *GoFormatAdapter) Name() string       { return "go-format" }
// Capabilities 返回该适配器提供的能力标签 (格式化)。
func (a *GoFormatAdapter) Capabilities() []string { return []string{"format"} }

// Execute 用 gofmt -l 列出未格式化文件; 有输出即判定检查未通过。
//
// 算法解读：
//
//	原理：gofmt -l 的语义是"只列出需要改动的文件、不改动任何文件", 因此它退 0 不代表
//	      通过 —— 退出码只说明 gofmt 自己跑成功, 真正的判据是 stdout 是否为空。
//	关键不变量：CheckPassed 必须由 RawOutput 是否为空决定, 不能由 err 决定。
//	      把 err==nil 当通过, 会让"仓库里全是未格式化文件"这条最该被拦的情况变成绿灯。
//	代价与边界：只覆盖 gofmt 能识别的格式问题 (缩进/对齐/换行), 不覆盖 goimports 的
//	      导入整理; args.Files 为空时退化为全目录扫描 (`.`), 大仓库会慢。
//	设计权衡：宁可依赖一个隐式约定 (gofmt 有输出=有问题) 也不额外跑一次 `gofmt -d`,
//	      因为判据只需要"有没有", 不需要"差在哪"。
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

// Name 返回适配器标识名 "goimports"。
func (a *GoImportsAdapter) Name() string       { return "goimports" }
// Capabilities 返回该适配器提供的能力标签 (导入修复 + 格式化)。
func (a *GoImportsAdapter) Capabilities() []string { return []string{"import-fix", "format"} }

// Execute 用 goimports -w 就地修复导入与格式; 探测不到 goimports 时降级用 gofmt。
//
// 算法解读：
//
//	原理：这是本包唯一一个**会改写工作区**的适配器 —— 它跑的是 -w (写回), 其余适配器
//	      都只读。判据因此不能沿用"有无输出"(写回模式没有输出), 改用 err 是否为空。
//	关键不变量：降级链必须保持"声明能力 ⊇ 实际能力": 回落到 gofmt 时只剩 format,
//	      而 Capabilities() 仍然照报 import-fix —— 调用方若据此认为导入已被修好,
//	      拿到的是**静默的假绿**。这是已知的契约缺口, 修复需要把探测结果反馈进
//	      Capabilities, 但那样会让能力随环境变化而不确定。
//	代价与边界：降级与否取决于 ToolDetector 的探测结果, 同一份代码在两台机器上
//	      可能走不同分支 —— 这正是它需要把实际命令写进日志的原因。
//	设计权衡：宁可降级也不直接失败 (gofmt 至少能把格式修对), 代价是"导入了但没修"
//	      这件事只能从命令行看出来。
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
