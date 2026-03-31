package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/ruflo/ruflo-go/mcp"
)

type claimsFile struct {
	ByAgent map[string][]string `json:"by_agent"`
}

var (
	claimsMu   sync.Mutex
	claimsData = &claimsFile{ByAgent: make(map[string][]string)}
)

func claimsStorePath() string {
	return filepath.Join(resolveDataDir(), "claims", "store.json")
}

func claimsLoad() {
	claimsMu.Lock()
	defer claimsMu.Unlock()
	b, err := os.ReadFile(claimsStorePath())
	if err != nil {
		return
	}
	var f claimsFile
	if json.Unmarshal(b, &f) == nil && f.ByAgent != nil {
		claimsData = &f
	}
}

func claimsSave() error {
	claimsMu.Lock()
	cp := claimsFile{ByAgent: make(map[string][]string)}
	for k, v := range claimsData.ByAgent {
		cp.ByAgent[k] = append([]string(nil), v...)
	}
	claimsMu.Unlock()
	return writeJSONFile(claimsStorePath(), cp)
}

func claimsTools() []*mcp.MCPTool {
	claimsLoad()
	return []*mcp.MCPTool{
		{Name: "claims_check", Description: "Check if agent has claim", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"agent_id": map[string]any{"type": "string"}, "claim": map[string]any{"type": "string"}}, "required": []string{"agent_id", "claim"}}, Handler: toolHandler(handleClaimsCheck)},
		{Name: "claims_grant", Description: "Grant claim to agent", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"agent_id": map[string]any{"type": "string"}, "claim": map[string]any{"type": "string"}}, "required": []string{"agent_id", "claim"}}, Handler: toolHandler(handleClaimsGrant)},
		{Name: "claims_revoke", Description: "Revoke claim from agent", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"agent_id": map[string]any{"type": "string"}, "claim": map[string]any{"type": "string"}}, "required": []string{"agent_id", "claim"}}, Handler: toolHandler(handleClaimsRevoke)},
		{Name: "claims_list", Description: "List claims for agent or all", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"agent_id": map[string]any{"type": "string"}}}, Handler: toolHandler(handleClaimsList)},
	}
}

func RegisterClaimsTools(reg *mcp.ToolRegistry) error {
	for _, t := range claimsTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleClaimsCheck(_ context.Context, m map[string]any) mcp.MCPToolResult {
	agent := strArg(m, "agent_id")
	claim := strArg(m, "claim")
	claimsMu.Lock()
	list := append([]string(nil), claimsData.ByAgent[agent]...)
	claimsMu.Unlock()
	has := false
	for _, c := range list {
		if c == claim {
			has = true
			break
		}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"agent_id": agent, "claim": claim, "has": has}}
}

func handleClaimsGrant(_ context.Context, m map[string]any) mcp.MCPToolResult {
	agent := strArg(m, "agent_id")
	claim := strArg(m, "claim")
	claimsMu.Lock()
	found := false
	for _, c := range claimsData.ByAgent[agent] {
		if c == claim {
			found = true
			break
		}
	}
	if !found {
		claimsData.ByAgent[agent] = append(claimsData.ByAgent[agent], claim)
	}
	claimsMu.Unlock()
	if err := claimsSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"agent_id": agent, "claim": claim, "granted": !found}}
}

func handleClaimsRevoke(_ context.Context, m map[string]any) mcp.MCPToolResult {
	agent := strArg(m, "agent_id")
	claim := strArg(m, "claim")
	claimsMu.Lock()
	cur := claimsData.ByAgent[agent]
	next := make([]string, 0, len(cur))
	removed := false
	for _, c := range cur {
		if c == claim {
			removed = true
			continue
		}
		next = append(next, c)
	}
	claimsData.ByAgent[agent] = next
	claimsMu.Unlock()
	if err := claimsSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"agent_id": agent, "claim": claim, "revoked": removed}}
}

func handleClaimsList(_ context.Context, m map[string]any) mcp.MCPToolResult {
	agent := strArg(m, "agent_id")
	claimsMu.Lock()
	defer claimsMu.Unlock()
	if agent != "" {
		return mcp.MCPToolResult{OK: true, Data: map[string]any{"agent_id": agent, "claims": append([]string(nil), claimsData.ByAgent[agent]...)}}
	}
	out := make(map[string][]string)
	for k, v := range claimsData.ByAgent {
		out[k] = append([]string(nil), v...)
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"by_agent": out}}
}
