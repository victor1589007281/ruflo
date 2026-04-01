package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/ruflo/ruflo-go/mcp"
)

// 本文件：基于声明（claims）的轻量授权 MCP 工具，按 agent_id 维护字符串声明列表并持久化到 store.json。
//
// 设计思路：claimsData.ByAgent 为内存真源，claimsLoad/claimsSave 与磁盘同步；grant 去重追加，revoke 过滤移除，
// list 支持按 agent 或返回全体映射。

type claimsFile struct {
	ByAgent map[string][]string `json:"by_agent"`
}

// claimsFile 持久化结构：键为 agent_id，值为该代理持有的 claim 字符串列表。

var (
	claimsMu   sync.Mutex
	claimsData = &claimsFile{ByAgent: make(map[string][]string)}
)

// claimsStorePath 返回 dataDir/claims/store.json 路径。
func claimsStorePath() string {
	return filepath.Join(resolveDataDir(), "claims", "store.json")
}

// claimsLoad 从磁盘加载声明数据；解析成功且 ByAgent 非 nil 时替换 claimsData。
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

// claimsSave 深拷贝 ByAgent 后写入 claimsStorePath。
func claimsSave() error {
	claimsMu.Lock()
	cp := claimsFile{ByAgent: make(map[string][]string)}
	for k, v := range claimsData.ByAgent {
		cp.ByAgent[k] = append([]string(nil), v...)
	}
	claimsMu.Unlock()
	return writeJSONFile(claimsStorePath(), cp)
}

// claimsTools 先 claimsLoad，再注册 check/grant/revoke/list。
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

// handleClaimsRevoke 从 agent 的列表中移除指定 claim；revoked 表示是否确实移除过。
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

// handleClaimsList 若提供 agent_id 则返回该代理的 claims；否则返回 by_agent 全表拷贝。
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
