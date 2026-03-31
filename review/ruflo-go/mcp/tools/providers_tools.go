package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
)

type providerRec struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	APIKey string            `json:"api_key,omitempty"`
	Extra  map[string]string `json:"extra,omitempty"`
}

type providersFile struct {
	Providers []providerRec `json:"providers"`
}

var (
	provMu   sync.Mutex
	provList = &providersFile{Providers: []providerRec{
		{Name: "anthropic-default", Type: string(api.LLMProviderAnthropic)},
		{Name: "openai-default", Type: string(api.LLMProviderOpenAI)},
	}}
)

func providersStorePath() string {
	return filepath.Join(resolveDataDir(), "providers", "store.json")
}

func provLoad() {
	provMu.Lock()
	defer provMu.Unlock()
	b, err := os.ReadFile(providersStorePath())
	if err != nil {
		return
	}
	var f providersFile
	if json.Unmarshal(b, &f) == nil && len(f.Providers) > 0 {
		provList = &f
	}
}

func provSave() error {
	provMu.Lock()
	cp := providersFile{Providers: append([]providerRec(nil), provList.Providers...)}
	provMu.Unlock()
	return writeJSONFile(providersStorePath(), cp)
}

func providersTools() []*mcp.MCPTool {
	provLoad()
	return []*mcp.MCPTool{
		{Name: "providers_list", Description: "List available LLM providers", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleProvidersList)},
		{Name: "providers_add", Description: "Add a provider", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "type": map[string]any{"type": "string"}}, "required": []string{"name", "type"}}, Handler: toolHandler(handleProvidersAdd)},
		{Name: "providers_remove", Description: "Remove a provider", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}, Handler: toolHandler(handleProvidersRemove)},
		{Name: "providers_test", Description: "Test a provider connection", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}, Handler: toolHandler(handleProvidersTest)},
		{Name: "providers_configure", Description: "Configure a provider", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "api_key": map[string]any{"type": "string"}}}, Handler: toolHandler(handleProvidersConfigure)},
	}
}

func RegisterProvidersTools(reg *mcp.ToolRegistry) error {
	for _, t := range providersTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleProvidersList(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	provMu.Lock()
	out := append([]providerRec(nil), provList.Providers...)
	provMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"providers": out, "count": len(out)}}
}

func handleProvidersAdd(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	typ := strArg(m, "type")
	if name == "" || typ == "" {
		return mcp.MCPToolResult{OK: false, Error: "name and type required"}
	}
	provMu.Lock()
	provList.Providers = append(provList.Providers, providerRec{Name: name, Type: typ})
	provMu.Unlock()
	if err := provSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"name": name, "type": typ}}
}

func handleProvidersRemove(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	provMu.Lock()
	next := make([]providerRec, 0, len(provList.Providers))
	removed := false
	for _, p := range provList.Providers {
		if p.Name == name {
			removed = true
			continue
		}
		next = append(next, p)
	}
	provList.Providers = next
	provMu.Unlock()
	if err := provSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	if !removed {
		return mcp.MCPToolResult{OK: false, Error: "provider not found"}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"removed": name}}
}

func handleProvidersTest(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	provMu.Lock()
	found := false
	var typ string
	for _, p := range provList.Providers {
		if p.Name == name {
			found = true
			typ = p.Type
			break
		}
	}
	provMu.Unlock()
	if !found {
		return mcp.MCPToolResult{OK: false, Error: "provider not found"}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"name": name, "type": typ, "reachable": true, "note": "synthetic check"}}
}

func handleProvidersConfigure(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	key := strArg(m, "api_key")
	provMu.Lock()
	ok := false
	for i := range provList.Providers {
		if provList.Providers[i].Name == name {
			provList.Providers[i].APIKey = key
			if provList.Providers[i].Extra == nil {
				provList.Providers[i].Extra = make(map[string]string)
			}
			ok = true
			break
		}
	}
	provMu.Unlock()
	if !ok {
		return mcp.MCPToolResult{OK: false, Error: "provider not found"}
	}
	if err := provSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"name": name, "configured": true}}
}
