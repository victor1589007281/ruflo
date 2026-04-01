package tools

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/security"
)

// 本文件：安全扫描、审计、输入校验与报告 MCP 工具；扫描/审计结果持久化到 data 目录下 JSON。
//
// 设计思路：secFile 在内存中聚合 scans 与 audits，secLoad/secSave 与磁盘 reports.json 同步；validate 调用
// pkg/security.InputValidator；report 将当前快照另存 summary.json。工具通过 toolHandler 适配 map 形参。

type securityReportFile struct {
	Scans   []map[string]any `json:"scans"`
	Audits  []map[string]any `json:"audits"`
	Updated time.Time        `json:"updated_at"`
}

// securityReportFile 为落盘的安全报告结构：多次扫描与审计记录及更新时间。

var (
	secMu   sync.Mutex
	secFile = &securityReportFile{Scans: make([]map[string]any, 0), Audits: make([]map[string]any, 0)}
)

// securityStorePath 返回 dataDir/security/reports.json 的绝对路径。
func securityStorePath() string {
	return filepath.Join(resolveDataDir(), "security", "reports.json")
}

// secLoad 在互斥锁下从 securityStorePath 读取 JSON 并覆盖 secFile，失败或解析失败则保持原状。
func secLoad() {
	secMu.Lock()
	defer secMu.Unlock()
	b, err := os.ReadFile(securityStorePath())
	if err != nil {
		return
	}
	var f securityReportFile
	if json.Unmarshal(b, &f) == nil {
		secFile = &f
		if secFile.Scans == nil {
			secFile.Scans = make([]map[string]any, 0)
		}
		if secFile.Audits == nil {
			secFile.Audits = make([]map[string]any, 0)
		}
	}
}

// secSave 拷贝当前 secFile 内容、刷新 Updated 时间戳后写入 securityStorePath。
func secSave() error {
	secMu.Lock()
	secFile.Updated = now()
	cp := securityReportFile{
		Scans:   append([]map[string]any(nil), secFile.Scans...),
		Audits:  append([]map[string]any(nil), secFile.Audits...),
		Updated: secFile.Updated,
	}
	secMu.Unlock()
	return writeJSONFile(securityStorePath(), cp)
}

// securityTools 先 secLoad，再注册 scan/audit/validate/report 四类工具。
func securityTools() []*mcp.MCPTool {
	secLoad()
	return []*mcp.MCPTool{
		{Name: "security_scan", Description: "Scan for vulnerabilities (heuristic)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"target": map[string]any{"type": "string"}, "path": map[string]any{"type": "string", "description": "Alias of target directory"}}}, Handler: toolHandler(handleSecurityScan)},
		{Name: "security_audit", Description: "Run security audit summary", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"scope": map[string]any{"type": "string"}}}, Handler: toolHandler(handleSecurityAudit)},
		{Name: "security_validate", Description: "Validate input string", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"field": map[string]any{"type": "string"}, "input": map[string]any{"type": "string"}}, "required": []string{"input"}}, Handler: toolHandler(handleSecurityValidate)},
		{Name: "security_report", Description: "Generate security report", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleSecurityReportTool)},
	}
}

// RegisterSecurityTools 将 securityTools 返回的全部工具注册到给定 ToolRegistry。
func RegisterSecurityTools(reg *mcp.ToolRegistry) error {
	for _, t := range securityTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func sensitiveBasename(name string) (string, bool) {
	b := strings.ToLower(filepath.Base(name))
	switch {
	case b == ".env" || strings.HasPrefix(b, ".env.") || b == ".env.sample":
		return "env_file", true
	case strings.Contains(b, "credential") || b == "secrets.json" || b == "credentials.json":
		return "credential_like_filename", true
	case b == "id_rsa" || b == "id_dsa" || b == "id_ecdsa" || b == "id_ed25519":
		return "private_key_filename", true
	case strings.HasSuffix(b, ".pem") && (strings.Contains(b, "key") || strings.Contains(b, "private")):
		return "pem_key_like", true
	default:
		return "", false
	}
}

// handleSecurityScan 在 target/path 目录下用 PathValidator、InputValidator 与敏感文件名启发式做真实扫描。
func handleSecurityScan(_ context.Context, m map[string]any) mcp.MCPToolResult {
	target := strArg(m, "path")
	if target == "" {
		target = strArg(m, "target")
	}
	if target == "" {
		target = "."
	}
	absRoot, err := filepath.Abs(target)
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	st, err := os.Stat(absRoot)
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	if !st.IsDir() {
		return mcp.MCPToolResult{OK: false, Error: "target must be a directory"}
	}
	pv := security.NewPathValidator(absRoot)
	iv := security.NewInputValidator()
	findings := make([]map[string]any, 0, 16)
	filesScanned := 0
	const maxFiles = 8000
	_ = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			findings = append(findings, map[string]any{"id": "S-WALK", "severity": "warn", "path": path, "message": werr.Error()})
			return nil
		}
		if filesScanned >= maxFiles {
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		filesScanned++
		rel, _ := filepath.Rel(absRoot, path)
		relSlash := filepath.ToSlash(rel)
		if _, err := pv.Validate(path, false); err != nil {
			findings = append(findings, map[string]any{"id": "S-PATH", "severity": "warn", "path": relSlash, "message": err.Error()})
		}
		if vr := iv.ValidateString("relpath", relSlash); !vr.OK {
			findings = append(findings, map[string]any{"id": "S-INJECT", "severity": "info", "path": relSlash, "message": "path pattern issues", "issues": vr.Issues})
		}
		if kind, hit := sensitiveBasename(path); hit {
			findings = append(findings, map[string]any{"id": "S-SENS", "severity": "warn", "path": relSlash, "message": "sensitive filename pattern", "kind": kind})
		}
		info, ierr := d.Info()
		if ierr == nil && info.Size() > 0 && info.Size() <= 96*1024 {
			ext := strings.ToLower(filepath.Ext(path))
			if ext == ".go" || ext == ".json" || ext == ".yaml" || ext == ".yml" || ext == ".env" || ext == ".txt" || ext == ".md" || ext == "" {
				buf := make([]byte, 768)
				f, oerr := os.Open(path)
				if oerr == nil {
					n, _ := f.Read(buf)
					_ = f.Close()
					snippet := string(buf[:n])
					if vr := iv.ValidateString("content", snippet); len(vr.Issues) > 0 {
						findings = append(findings, map[string]any{"id": "S-CONTENT", "severity": "info", "path": relSlash, "issues": vr.Issues})
					}
				}
			}
		}
		return nil
	})
	if len(findings) == 0 {
		findings = append(findings, map[string]any{"id": "S-000", "severity": "info", "message": "no heuristic findings in scanned files"})
	}
	rec := map[string]any{
		"target": target, "abs_root": absRoot, "at": now(),
		"findings": findings, "files_scanned": filesScanned,
	}
	secMu.Lock()
	secFile.Scans = append(secFile.Scans, rec)
	secMu.Unlock()
	if err := secSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: rec}
}

// handleSecurityAudit 审计 pkg/security 各组件与 SafeExecutor / PathValidator 的真实行为。
func handleSecurityAudit(_ context.Context, m map[string]any) mcp.MCPToolResult {
	scope := strArg(m, "scope")
	if scope == "" {
		scope = "orchestration"
	}
	checks := make([]map[string]any, 0, 8)
	passed := 0

	iv := security.NewInputValidator()
	if r := iv.ValidateString("audit_probe", "plain-safe-text"); r.OK {
		passed++
		checks = append(checks, map[string]any{"name": "input_validator", "ok": true})
	} else {
		checks = append(checks, map[string]any{"name": "input_validator", "ok": false, "issues": r.Issues})
	}

	td, err := os.MkdirTemp("", "ruflo-sec-audit-*")
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	defer func() { _ = os.RemoveAll(td) }()
	nested := filepath.Join(td, "allowed", "nested.txt")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	if err := os.WriteFile(nested, []byte("ok"), 0o644); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	pv := security.NewPathValidator(td)
	if _, err := pv.Validate(nested, false); err == nil {
		passed++
		checks = append(checks, map[string]any{"name": "path_validator_allowed_subpath", "ok": true})
	} else {
		checks = append(checks, map[string]any{"name": "path_validator_allowed_subpath", "ok": false, "error": err.Error()})
	}
	if _, err := pv.Validate("/nonexistent-ruflo-escape/etc/passwd", false); err != nil {
		passed++
		checks = append(checks, map[string]any{"name": "path_validator_rejects_outside", "ok": true})
	} else {
		checks = append(checks, map[string]any{"name": "path_validator_rejects_outside", "ok": false, "error": "expected rejection"})
	}

	trueExe, lookErr := exec.LookPath("true")
	if lookErr != nil {
		trueExe = "/usr/bin/true"
	}
	se := security.NewSafeExecutor(map[string]string{"true": trueExe})
	if _, err := se.Run(context.Background(), "missing-cmd", nil); err != nil {
		passed++
		checks = append(checks, map[string]any{"name": "safe_executor_unknown_rejected", "ok": true})
	} else {
		checks = append(checks, map[string]any{"name": "safe_executor_unknown_rejected", "ok": false})
	}
	if _, err := se.Run(context.Background(), "true", []string{"x;rm -rf /"}); err != nil {
		passed++
		checks = append(checks, map[string]any{"name": "safe_executor_blocked_metachar", "ok": true})
	} else {
		checks = append(checks, map[string]any{"name": "safe_executor_blocked_metachar", "ok": false})
	}
	if _, err := se.Run(context.Background(), "true", []string{}); err == nil {
		passed++
		checks = append(checks, map[string]any{"name": "safe_executor_allowlisted_ok", "ok": true})
	} else {
		checks = append(checks, map[string]any{"name": "safe_executor_allowlisted_ok", "ok": false, "error": err.Error()})
	}

	rec := map[string]any{
		"scope": scope, "at": now(),
		"checks": len(checks), "passed": passed,
		"results": checks,
	}
	secMu.Lock()
	secFile.Audits = append(secFile.Audits, rec)
	secMu.Unlock()
	if err := secSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: rec}
}

// handleSecurityValidate 使用 InputValidator 校验 input；field 可选（默认 "input"），返回 valid 与 issues 列表。
func handleSecurityValidate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	field := strArg(m, "field")
	if field == "" {
		field = "input"
	}
	in := strArg(m, "input")
	v := security.NewInputValidator()
	res := v.ValidateString(field, in)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"valid": res.OK, "issues": res.Issues,
	}}
}

// handleSecurityReportTool 将当前 scans/audits 快照写入 security/summary.json，返回路径与条数统计。
func handleSecurityReportTool(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	secMu.Lock()
	out := securityReportFile{
		Scans:   append([]map[string]any(nil), secFile.Scans...),
		Audits:  append([]map[string]any(nil), secFile.Audits...),
		Updated: now(),
	}
	secMu.Unlock()
	p := filepath.Join(resolveDataDir(), "security", "summary.json")
	if err := writeJSONFile(p, out); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"path": p, "scans": len(out.Scans), "audits": len(out.Audits)}}
}
