package tools

// 本文件实现 LLM Provider 配置的 MCP 工具（providers_*），持久化至 providers/store.json。
//
// 设计思路：
//   - providerRec 与 api.LLMProvider* 类型字符串对齐，默认种子含 anthropic/openai 占位条目。
//   - providers_test：OpenAI 等走 ProviderManager.HealthCheck；Anthropic 使用 JSON 中的 api_key 对 Models 端点 GET 探活。
//   - OpenAI 且配置了 api_key 时同步 Register 到 globalState.providerMgr；列表合并 JSON 与 manager_registered。
//   - Configure 写入 api_key 字段（注意本地文件权限）；列表含敏感字段，调用方应避免日志泄露。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/providers"
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

// managerProviderNameForType 将 store 中的 type 字符串映射为 ProviderManager 注册名（与 LLMProvider.Name 一致）。
func managerProviderNameForType(typ string) string {
	t := strings.ToLower(strings.TrimSpace(typ))
	switch t {
	case string(api.LLMProviderAnthropic):
		return "anthropic"
	case string(api.LLMProviderOpenAI):
		return string(api.LLMProviderOpenAI)
	case string(api.LLMProviderGoogle):
		return string(api.LLMProviderGoogle)
	case string(api.LLMProviderCohere):
		return string(api.LLMProviderCohere)
	case string(api.LLMProviderRuvector):
		return string(api.LLMProviderRuvector)
	case string(api.LLMProviderOllama):
		return string(api.LLMProviderOllama)
	default:
		return t
	}
}

// applyJSONProvidersToManager 将 store 中带 API Key 的 OpenAI 条目注册进 ProviderManager（Anthropic 走独立探活，不经过 Register）。
func applyJSONProvidersToManager() {
	mgr := globalState.providerMgr
	if mgr == nil {
		return
	}
	provMu.Lock()
	defer provMu.Unlock()
	for _, p := range provList.Providers {
		typ := strings.ToLower(strings.TrimSpace(p.Type))
		key := strings.TrimSpace(p.APIKey)
		if typ == string(api.LLMProviderOpenAI) && key != "" {
			mgr.RegisterProvider(&providers.OpenAIProvider{APIKey: key})
		}
	}
}

// anthropicHealthFromStore 使用 store 中的 key 调用 Anthropic Models API（与 AnthropicProvider.HealthCheck 行为一致）。
func anthropicHealthFromStore(ctx context.Context, apiKey string) error {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return fmt.Errorf("anthropic: no api key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("anthropic: server error %s", resp.Status)
	}
	return nil
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

// handleProvidersList 处理 providers_list：返回 JSON 配置中的 providers、ProviderManager 已注册名与 count。
func handleProvidersList(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	applyJSONProvidersToManager()
	provMu.Lock()
	out := append([]providerRec(nil), provList.Providers...)
	provMu.Unlock()
	mgrNames := []string(nil)
	if globalState.providerMgr != nil {
		mgrNames = globalState.providerMgr.ListProviders()
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"providers":            out,
		"count":                len(out),
		"manager_registered":   mgrNames,
		"manager_register_count": len(mgrNames),
	}}
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

// handleProvidersTest 处理 providers_test：同步 JSON 后按类型执行 HealthCheck（Anthropic 使用 JSON key 直连探活）。
func handleProvidersTest(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	applyJSONProvidersToManager()
	provMu.Lock()
	found := false
	var typ string
	var apiKey string
	for _, p := range provList.Providers {
		if p.Name == name {
			found = true
			typ = p.Type
			apiKey = p.APIKey
			break
		}
	}
	provMu.Unlock()
	if !found {
		return mcp.MCPToolResult{OK: false, Error: "provider not found"}
	}
	mgr := globalState.providerMgr
	if mgr == nil {
		return mcp.MCPToolResult{OK: false, Error: "provider manager unavailable"}
	}
	pn := managerProviderNameForType(typ)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if pn == "anthropic" {
		err := anthropicHealthFromStore(ctx, apiKey)
		reachable := err == nil
		data := map[string]any{
			"name": name, "type": typ, "manager_name": pn, "reachable": reachable,
			"probe": "anthropic_models",
		}
		if err != nil {
			data["error"] = err.Error()
		}
		return mcp.MCPToolResult{OK: true, Data: data}
	}

	p, ok := mgr.GetProvider(pn)
	if !ok {
		return mcp.MCPToolResult{OK: true, Data: map[string]any{
			"name": name, "type": typ, "manager_name": pn,
			"reachable": false,
			"note":      "no provider registered for this type (configure api_key for openai in store, or set provider env vars)",
		}}
	}
	err := p.HealthCheck(ctx)
	reachable := err == nil
	data := map[string]any{
		"name": name, "type": typ, "manager_name": pn, "reachable": reachable,
	}
	if err != nil {
		data["error"] = err.Error()
	}
	return mcp.MCPToolResult{OK: true, Data: data}
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
