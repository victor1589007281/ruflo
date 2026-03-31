package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ruflo/ruflo-go/mcp"
)

var (
	configMu     sync.Mutex
	configLoaded bool
	configVals   = map[string]string{
		"log_level":      "info",
		"memory_backend": "hybrid",
		"mcp_transport":  "stdio",
		"data_dir":       ".claude-flow",
	}
)

func configStorePath() string {
	return filepath.Join(resolveDataDir(), "config", "mcp-config.json")
}

type configFile struct {
	Values map[string]string `json:"values"`
}

func loadMCPConfig() {
	configMu.Lock()
	defer configMu.Unlock()
	if configLoaded {
		return
	}
	configLoaded = true
	b, err := os.ReadFile(configStorePath())
	if err != nil {
		return
	}
	var f configFile
	if json.Unmarshal(b, &f) != nil || f.Values == nil {
		return
	}
	for k, v := range f.Values {
		configVals[k] = v
	}
}

func saveMCPConfig() error {
	configMu.Lock()
	cp := make(map[string]string, len(configVals))
	for k, v := range configVals {
		cp[k] = v
	}
	configMu.Unlock()
	f := configFile{Values: cp}
	return writeJSONFile(configStorePath(), f)
}

func configTools() []*mcp.MCPTool {
	loadMCPConfig()
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"key": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}},
	}
	return []*mcp.MCPTool{
		{Name: "config_get", Description: "Get a config value by key", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}, "required": []string{"key"}}, Handler: toolHandler(handleConfigGet)},
		{Name: "config_set", Description: "Set a config value", InputSchema: schema, Handler: toolHandler(handleConfigSet)},
		{Name: "config_list", Description: "List all config entries", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleConfigList)},
		{Name: "config_reset", Description: "Reset config to defaults", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleConfigReset)},
		{Name: "config_export", Description: "Export MCP config JSON to path", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}, Handler: toolHandler(handleConfigExport)},
		{Name: "config_import", Description: "Import MCP config JSON from path", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}, Handler: toolHandler(handleConfigImport)},
	}
}

func RegisterConfigTools(reg *mcp.ToolRegistry) error {
	for _, t := range configTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleConfigGet(_ context.Context, m map[string]any) mcp.MCPToolResult {
	k := strArg(m, "key")
	if k == "" {
		return mcp.MCPToolResult{OK: false, Error: "key required"}
	}
	configMu.Lock()
	v, ok := configVals[k]
	configMu.Unlock()
	if !ok {
		return mcp.MCPToolResult{OK: true, Data: map[string]any{"key": k, "value": nil, "found": false}}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"key": k, "value": v, "found": true}}
}

func handleConfigSet(_ context.Context, m map[string]any) mcp.MCPToolResult {
	k := strArg(m, "key")
	if k == "" {
		return mcp.MCPToolResult{OK: false, Error: "key required"}
	}
	val := strArg(m, "value")
	configMu.Lock()
	configVals[k] = val
	configMu.Unlock()
	if err := saveMCPConfig(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"key": k, "value": val}}
}

func handleConfigList(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	configMu.Lock()
	cp := make(map[string]string, len(configVals))
	for k, v := range configVals {
		cp[k] = v
	}
	configMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"config": cp}}
}

func handleConfigExport(_ context.Context, m map[string]any) mcp.MCPToolResult {
	p := strArg(m, "path")
	if p == "" {
		p = filepath.Join(resolveDataDir(), "config", "export.json")
	}
	configMu.Lock()
	cp := make(map[string]string, len(configVals))
	for k, v := range configVals {
		cp[k] = v
	}
	configMu.Unlock()
	f := configFile{Values: cp}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"path": p}}
}

func handleConfigImport(_ context.Context, m map[string]any) mcp.MCPToolResult {
	p := strArg(m, "path")
	if p == "" {
		return mcp.MCPToolResult{OK: false, Error: "path required"}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	var f configFile
	if err := json.Unmarshal(b, &f); err != nil || f.Values == nil {
		return mcp.MCPToolResult{OK: false, Error: "invalid config file"}
	}
	configMu.Lock()
	for k, v := range f.Values {
		k = strings.TrimSpace(k)
		if k != "" {
			configVals[k] = v
		}
	}
	configMu.Unlock()
	if err := saveMCPConfig(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"imported": true, "keys": len(f.Values)}}
}

func handleConfigReset(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	configMu.Lock()
	configVals = map[string]string{
		"log_level":      "info",
		"memory_backend": "hybrid",
		"mcp_transport":  "stdio",
		"data_dir":       ".claude-flow",
	}
	configMu.Unlock()
	if err := saveMCPConfig(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"reset": true}}
}
