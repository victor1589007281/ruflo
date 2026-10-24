// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
)

// SecurityScan runs gosec or falls back to regex-based security scanning.
type SecurityScan struct{ Base }

// NewSecurityScan 构造安全扫描 Tool Skill (只读、可并发)。
//
// NewSecurityScan creates a new SecurityScan skill.
func NewSecurityScan() *SecurityScan {
	return &SecurityScan{
		Base: NewBase(Meta{
			Name:           "security-scan",
			Description:    "Run gosec or fallback regex-based security scanning for Go code.",
			Version:        "1.0.0",
			ReadOnly:       true,
			SafeConcurrent: true,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"repo_root": {"type": "string"},
					"files":     {"type": "array", "items": {"type": "string"}}
				},
				"required": ["repo_root"]
			}`),
		}),
	}
}

// Execute 先跑 gosec, 失败则退化成内置正则扫描。
//
// 算法解读：
//
//	原理：两级策略 —— gosec 可用且**退出码为 0** 时用它的结构化输出; 否则用一组
//		硬编码正则逐行扫文件。降级是静默的, 结果里用 method 字段区分两种来源。
//	关键不变量：降级判据是 `err == nil`, 而 gosec **发现问题时退出码非 0** ——
//		也就是说"扫出漏洞"和"工具跑不起来"在这里被归成同一类, 都会掉进正则兜底。
//		因此 method=="gosec" 只代表"没扫出问题", 不代表"扫过且干净"。
//	代价与边界：正则兜底只覆盖 files 里**显式列出**的文件 (不列文件 = 零结果),
//		且每条诊断不带修复建议。gosec 那一路走 ./... 全仓扫描, 不受 files 约束。
//	设计权衡：宁可多跑一遍廉价的兜底, 也不让"环境里没装 gosec"变成一次空结果 ——
//		但代价是上面那条判据歧义, 读结果时必须同时看 method。
//
// Execute runs the security scan.
func (s *SecurityScan) Execute(ctx context.Context, input json.RawMessage) (map[string]any, error) {
	var in struct {
		RepoRoot string   `json:"repo_root"`
		Files    []string `json:"files,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}

	// Try gosec first
	if out, err := s.runGosec(ctx, in.RepoRoot); err == nil {
		return s.parseGosecOutput(out)
	}

	// Fallback to regex scanning
	diagnostics := s.regexScan(in.RepoRoot, in.Files)
	return map[string]any{
		"status":      "completed",
		"method":      "regex_fallback",
		"diagnostics": diagnostics,
		"issues":      len(diagnostics),
	}, nil
}

// runGosec 在 repoRoot 下执行 `gosec -fmt json ./...`, 合并返回 stdout+stderr。
//
// 注意：用 CombinedOutput 会把 stderr 混进 stdout, 于是解析 JSON 失败时
// 原始输出里会同时含诊断与告警; 且 gosec 扫出问题即非零退出 (见 Execute 的解读)。
func (s *SecurityScan) runGosec(ctx context.Context, repoRoot string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gosec", "-fmt", "json", "./...")
	cmd.Dir = repoRoot
	return cmd.CombinedOutput()
}

// parseGosecOutput 解析 gosec 的 JSON 输出; 解析失败时原样回传文本, **不报错**。
func (s *SecurityScan) parseGosecOutput(out []byte) (map[string]any, error) {
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		return map[string]any{
			"status":     "completed",
			"method":     "gosec",
			"raw_output": string(out),
		}, nil
	}
	result["status"] = "completed"
	result["method"] = "gosec"
	return result, nil
}

// regexScan 用内置模式表逐行扫描 files 里的每个文件, 返回命中的诊断。
//
// 算法解读：
//
//	原理：5 条正则 (弱加密 / 命令注入 / SQL 注入 / AWS key / GitHub token) 对每一行
//		逐一试匹配, 一行可以命中多条, 每条各自产出一条诊断。
//	关键不变量：行号取的是 **split 后的下标 +1** —— 前提是文件按 '\n' 切分后下标与
//		物理行号一一对应; 这也意味着 CRLF 文件行尾会带上 '\r', 但不影响正则匹配。
//	代价与边界：files 为空时**恒返回 nil** (不扫全仓); 读文件走 `cat` 子进程而不是
//		os.ReadFile, 既没传 ctx 也没做路径校验; repoRoot 参数在函数体内**完全未使用**
//		(路径按当前工作目录解析), 所以传相对路径时会依赖进程 cwd。
//	设计权衡：宁可误报也要覆盖最常见的几类硬编码密钥 —— 模式表刻意保持小而精确,
//		避免用"看起来像密钥"的宽泛规则把结果淹没。
func (s *SecurityScan) regexScan(repoRoot string, files []string) []Diagnostic {
	var diagnostics []Diagnostic

	patterns := []struct {
		re       *regexp.Regexp
		category string
		severity string
		msg      string
	}{
		{
			re:       regexp.MustCompile(`"crypto/md5"|"crypto/sha1"`),
			category: "weak_crypto",
			severity: "error",
			msg:      "Weak cryptographic primitive (md5/sha1) detected. Use crypto/sha256 or bcrypt.",
		},
		{
			re:       regexp.MustCompile(`os\.Exec|exec\.Command.*\+.*%s|\+.*\$`),
			category: "command_injection",
			severity: "error",
			msg:      "Potential command injection via string concatenation in exec.Command.",
		},
		{
			re:       regexp.MustCompile(`sql\.Open\s*\(.*\+`),
			category: "sql_injection",
			severity: "error",
			msg:      "Potential SQL injection via string concatenation in sql.Open.",
		},
		{
			re:       regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
			category: "hardcoded_secret",
			severity: "error",
			msg:      "Hardcoded AWS access key detected.",
		},
		{
			re:       regexp.MustCompile(`ghp_[a-zA-Z0-9]{36}`),
			category: "hardcoded_secret",
			severity: "error",
			msg:      "Hardcoded GitHub personal access token detected.",
		},
	}

	for _, f := range files {
		content, err := exec.Command("cat", f).Output()
		if err != nil {
			continue
		}
		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			for _, p := range patterns {
				if p.re.MatchString(line) {
					diagnostics = append(diagnostics, Diagnostic{
						File:     f,
						Line:     i + 1,
						Message:  p.msg,
						Category: p.category,
						Severity: p.severity,
						Tool:     "security-scan",
					})
				}
			}
		}
	}

	return diagnostics
}
