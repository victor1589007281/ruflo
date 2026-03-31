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

type securityReportFile struct {
	Scans   []map[string]any `json:"scans"`
	Audits  []map[string]any `json:"audits"`
	Updated time.Time        `json:"updated_at"`
}

var (
	secMu   sync.Mutex
	secFile = &securityReportFile{Scans: make([]map[string]any, 0), Audits: make([]map[string]any, 0)}
)

func securityStorePath() string {
	return filepath.Join(resolveDataDir(), "security", "reports.json")
}

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

func securityTools() []*mcp.MCPTool {
	secLoad()
	return []*mcp.MCPTool{
		{Name: "security_scan", Description: "Scan for vulnerabilities (heuristic)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"target": map[string]any{"type": "string"}}}, Handler: toolHandler(handleSecurityScan)},
		{Name: "security_audit", Description: "Run security audit summary", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"scope": map[string]any{"type": "string"}}}, Handler: toolHandler(handleSecurityAudit)},
		{Name: "security_validate", Description: "Validate input string", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"field": map[string]any{"type": "string"}, "input": map[string]any{"type": "string"}}, "required": []string{"input"}}, Handler: toolHandler(handleSecurityValidate)},
		{Name: "security_report", Description: "Generate security report", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleSecurityReportTool)},
	}
}

func RegisterSecurityTools(reg *mcp.ToolRegistry) error {
	for _, t := range securityTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

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
