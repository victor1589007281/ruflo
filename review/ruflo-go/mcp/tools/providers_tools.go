package tools

// 本文件实现 LLM Provider 配置的 MCP 工具（providers_*），持久化至 providers/store.json。
//
// 设计思路：
//   - providerRec 与 api.LLMProvider* 类型字符串对齐，默认种子含 anthropic/openai 占位条目。
//   - Test 为合成检查（仅确认存在与类型），不发起真实网络探测；Configure 写入 api_key 字段（注意本地文件权限）。
//   - 列表返回含敏感字段的完整结构，调用方应避免日志泄露。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
)

// providerRec 单条 Provider 记录：展示名、类型、可选 API Key 与扩展键值。
type providerRec struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	APIKey string            `json:"api_key,omitempty"`
	Extra  map[string]string `json:"extra,omitempty"`
}

// providersFile 持久化文件顶层结构：Providers 数组。
type providersFile struct {
	Providers []providerRec `json:"providers"`
}

var (
	provMu sync.Mutex
	// provList 内存中的 Provider 列表；含两条默认 Provider。
	provList = &providersFile{Providers: []providerRec{
		{Name: "anthropic-default", Type: string(api.LLMProviderAnthropic)},
		{Name: "openai-default", Type: string(api.LLMProviderOpenAI)},
	}}
)

// providersStorePath 返回 Provider 存储 JSON 路径。
func providersStorePath() string {
	return filepath.Join(resolveDataDir(), "providers", "store.json")
}

// provLoad 从磁盘加载；仅当解析成功且 Providers 非空时覆盖内存。
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

// provSave 将当前 Providers 切片写入 store.json。
func provSave() error {
	provMu.Lock()
	cp := providersFile{Providers: append([]providerRec(nil), provList.Providers...)}
	provMu.Unlock()
	return writeJSONFile(providersStorePath(), cp)
}

// providersTools 构造 providers_* MCP 工具定义。
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

// RegisterProvidersTools 向注册表登记 LLM Provider 管理 MCP 工具。
func RegisterProvidersTools(reg *mcp.ToolRegistry) error {
	for _, t := range providersTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handleProvidersList 处理 providers_list：返回 providers 与 count。
func handleProvidersList(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	provMu.Lock()
	out := append([]providerRec(nil), provList.Providers...)
	provMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"providers": out, "count": len(out)}}
}

// handleProvidersAdd 处理 providers_add：name、type 必填；追加记录并保存。
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

// handleProvidersRemove 处理 providers_remove：按 name 删除；未找到返回错误。
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

// handleProvidersTest 处理 providers_test：按 name 查找；返回 reachable=true 与 note 标明为合成检查。
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

// handleProvidersConfigure 处理 providers_configure：按 name 匹配项写入 api_key，并确保 Extra 非 nil。
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
