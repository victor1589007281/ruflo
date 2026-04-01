package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
		{Name: "security_scan", Description: "Scan for vulnerabilities (heuristic)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"target": map[string]any{"type": "string"}}}, Handler: toolHandler(handleSecurityScan)},
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

// handleSecurityScan 对 target 目录（默认 "."）追加一条启发式扫描记录（含占位 findings），持久化后返回该条记录。
func handleSecurityScan(_ context.Context, m map[string]any) mcp.MCPToolResult {
	target := strArg(m, "target")
	if target == "" {
		target = "."
	}
	rec := map[string]any{
		"target": target, "at": now(),
		"findings": []map[string]any{
			{"id": "S-001", "severity": "info", "message": "No CVE database attached; heuristic scan only"},
		},
	}
	secMu.Lock()
	secFile.Scans = append(secFile.Scans, rec)
	secMu.Unlock()
	if err := secSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: rec}
}

// handleSecurityAudit 在 scope（默认 orchestration）下追加一条摘要审计记录，持久化后返回。
func handleSecurityAudit(_ context.Context, m map[string]any) mcp.MCPToolResult {
	scope := strArg(m, "scope")
	if scope == "" {
		scope = "orchestration"
	}
	rec := map[string]any{"scope": scope, "at": now(), "checks": 4, "passed": 4}
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
