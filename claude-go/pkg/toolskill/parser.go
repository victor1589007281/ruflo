// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// ----------------------------------------------
// staticcheck JSON Parser
// staticcheck -f json ./... output format:
// {"checker":"SA4000","code":"SA4000","severity":"error"
//  "location":{"file":"x.go","line":10,"column":5}
//  "message":"..."}
// ----------------------------------------------
type StaticcheckJSONParser struct{}

// Parse 解析 staticcheck -f json 的逐行 JSON 输出, 把检查项 code 映射成诊断类别。
func (p *StaticcheckJSONParser) Parse(raw []byte, repoRoot string) (*AdapterResult, error) {
	var result AdapterResult
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			Checker  string `json:"checker"`
			Code     string `json:"code"`
			Severity string `json:"severity"`
			Location struct {
				File   string `json:"file"`
				Line   int    `json:"line"`
				Column int    `json:"column"`
			} `json:"location"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			File:     entry.Location.File,
			Line:     entry.Location.Line,
			Column:   entry.Location.Column,
			Message:  entry.Message,
			Category: mapStaticcheckCode(entry.Code),
			Severity: entry.Severity,
			Code:     entry.Code,
			Tool:     "staticcheck",
		})
	}
	return &result, nil
}

// mapStaticcheckCode 按 staticcheck 检查项前缀归类: SA=bug, S1/ST=style, QF=suggestion, 其余 warning。
func mapStaticcheckCode(code string) string {
	prefix := ""
	if len(code) >= 2 {
		prefix = code[:2]
	}
	switch prefix {
	case "SA":
		return "bug" // correctness issues
	case "S1":
		return "style"
	case "ST":
		return "style"
	case "QF":
		return "suggestion" // quickfix
	default:
		return "warning"
	}
}

// ----------------------------------------------
// go vet / go build text Parser
// Format: file.go:10:5: error message
// ----------------------------------------------
type GoBuildTextParser struct{}

var goBuildPattern = regexp.MustCompile(`^(.+?):(\d+):(?:(\d+):)?\s*(.+)$`)

// Parse 用正则解析 "file:line:col: message" 形态的 go build / go vet 文本输出。
func (p *GoBuildTextParser) Parse(raw []byte, repoRoot string) (*AdapterResult, error) {
	var result AdapterResult
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		if m := goBuildPattern.FindStringSubmatch(line); m != nil {
			lineNum, _ := strconv.Atoi(m[2])
			colNum, _ := strconv.Atoi(m[3])
			msg := m[4]
			result.Diagnostics = append(result.Diagnostics, Diagnostic{
				File:     m[1],
				Line:     lineNum,
				Column:   colNum,
				Message:  msg,
				Category: classifyGoError(msg),
				Severity: "error",
				Tool:     "go-build",
			})
		}
	}
	return &result, nil
}

// classifyGoError 按错误文本里的关键词给 Go 编译错误分类。
func classifyGoError(msg string) string {
	switch {
	case strings.Contains(msg, "undefined"):
		return "undefined_symbol"
	case strings.Contains(msg, "cannot use"):
		return "type_mismatch"
	case strings.Contains(msg, "does not implement"):
		return "interface_mismatch"
	case strings.Contains(msg, "too few") || strings.Contains(msg, "too many"):
		return "signature_mismatch"
	case strings.Contains(msg, "import"):
		return "import_error"
	case strings.Contains(msg, "undefined field") || strings.Contains(msg, "unknown field"):
		return "field_error"
	default:
		return "syntax_error"
	}
}

// ----------------------------------------------
// golangci-lint JSON Parser
// golangci-lint run --out-format=json ./...
// ----------------------------------------------
type GolangCILintParser struct{}

// Parse 解析 golangci-lint --out-format=json 的整体 JSON 输出; 解析失败直接返回错误, 不降级。
func (p *GolangCILintParser) Parse(raw []byte, repoRoot string) (*AdapterResult, error) {
	var payload struct {
		Issues []struct {
			FromLinter string `json:"FromLinter"`
			Text       string `json:"Text"`
			Severity   string `json:"Severity"`
			Pos        struct {
				Filename string `json:"Filename"`
				Line     int    `json:"Line"`
				Column   int    `json:"Column"`
			} `json:"Pos"`
		} `json:"Issues"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}

	var result AdapterResult
	for _, issue := range payload.Issues {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			File:     issue.Pos.Filename,
			Line:     issue.Pos.Line,
			Column:   issue.Pos.Column,
			Message:  issue.Text,
			Category: mapLinterName(issue.FromLinter),
			Severity: issue.Severity,
			Code:     issue.FromLinter,
			Tool:     "golangci-lint:" + issue.FromLinter,
		})
	}
	return &result, nil
}

// mapLinterName 把 linter 名映射成诊断类别: errcheck/govet/staticcheck=bug, 其余 style。
func mapLinterName(name string) string {
	switch name {
	case "errcheck", "govet", "staticcheck":
		return "bug"
	case "gosimple", "ineffassign", "unused":
		return "suggestion"
	default:
		return "style"
	}
}

// ----------------------------------------------
// goimports Parser
// ----------------------------------------------
type GoImportsParser struct{}

// Parse 解析 goimports -l 的文件名列表, 每个文件名产出一条 import/格式告警。
func (p *GoImportsParser) Parse(raw []byte, repoRoot string) (*AdapterResult, error) {
	// goimports -l outputs unformatted file names
	var result AdapterResult
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			File:     line,
			Line:     0,
			Column:   0,
			Message:  "import/format issues detected",
			Category: "import_error",
			Severity: "warning",
			Tool:     "goimports",
		})
	}
	return &result, nil
}

// ----------------------------------------------
// go vet Parser
// ----------------------------------------------
type GoVetParser struct{}

// Parse 复用 go build 文本解析器 (go vet 的输出同格式)。
func (p *GoVetParser) Parse(raw []byte, repoRoot string) (*AdapterResult, error) {
	// go vet outputs text in the same format as go build
	return (&GoBuildTextParser{}).Parse(raw, repoRoot)
}
