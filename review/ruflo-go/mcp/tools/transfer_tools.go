package tools

// 本文件实现 Transfer / IPFS 桥接类 MCP 工具（transfer_*）：状态持久化于数据目录 transfer/store.json，并提供 PII 检测与 Pinata 网关 URL 拼接。
//
// 设计思路：
//   - 目录数据为模板/插件元数据列表，缺失或空时 seedDefaultTransfer 注入示例项，便于演示与测试。
//   - transfer_store-download 仅记录意图并 save，不执行真实下载；IPFS 解析不访问网络，只构造 gateway URL。
//   - filterItems 对 name/description/tags 做子串包含过滤（大小写不敏感）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ruflo/ruflo-go/mcp"
)

// transferStoreFile 持久化目录：商店条目与插件条目分栏存储。
type transferStoreFile struct {
	StoreItems []transferItem `json:"store_items"`
	Plugins    []transferItem `json:"plugins"`
}

// transferItem 单条目录项：标识、名称、描述、标签与运营标记（精选/趋势/官方）及可选 CID。
type transferItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Featured    bool     `json:"featured,omitempty"`
	Trending    bool     `json:"trending,omitempty"`
	Official    bool     `json:"official,omitempty"`
	CID         string   `json:"cid,omitempty"`
}

var (
	transferMu   sync.Mutex
	transferData = &transferStoreFile{}
)

// transferStorePath 返回 transfer 目录 store.json 路径。
func transferStorePath() string {
	return filepath.Join(resolveDataDir(), "transfer", "store.json")
}

// transferLoad 加载 store.json；失败或空目录时调用 seedDefaultTransfer。
func transferLoad() {
	transferMu.Lock()
	defer transferMu.Unlock()
	b, err := os.ReadFile(transferStorePath())
	if err != nil {
		seedDefaultTransfer()
		return
	}
	var f transferStoreFile
	if json.Unmarshal(b, &f) != nil {
		seedDefaultTransfer()
		return
	}
	transferData = &f
	if len(transferData.StoreItems) == 0 && len(transferData.Plugins) == 0 {
		seedDefaultTransfer()
	}
}

// seedDefaultTransfer 写入内置示例商店项与插件项到内存。
func seedDefaultTransfer() {
	transferData = &transferStoreFile{
		StoreItems: []transferItem{
			{ID: "tpl-1", Name: "workflow-starter", Description: "Starter orchestration template", Tags: []string{"workflow", "core"}, Featured: true, Trending: true},
			{ID: "tpl-2", Name: "memory-hybrid", Description: "Hybrid memory preset", Tags: []string{"memory"}, Featured: false, Trending: true},
		},
		Plugins: []transferItem{
			{ID: "p-official-1", Name: "@ruflo/embeddings", Description: "Embeddings plugin", Tags: []string{"embeddings"}, Official: true, Featured: true},
			{ID: "p-2", Name: "@ruflo/security", Description: "Security validation", Tags: []string{"security"}, Official: true},
		},
	}
}

// transferSave 将 StoreItems 与 Plugins 快照写入磁盘。
func transferSave() error {
	transferMu.Lock()
	cp := transferStoreFile{
		StoreItems: append([]transferItem(nil), transferData.StoreItems...),
		Plugins:    append([]transferItem(nil), transferData.Plugins...),
	}
	transferMu.Unlock()
	return writeJSONFile(transferStorePath(), cp)
}

// transferTools 构造 transfer_* MCP 工具列表。
func transferTools() []*mcp.MCPTool {
	transferLoad()
	obj := map[string]any{"type": "object", "properties": map[string]any{}}
	return []*mcp.MCPTool{
		{Name: "transfer_detect-pii", Description: "Detect PII in text (heuristic)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}, Handler: toolHandler(handleTransferDetectPII)},
		{Name: "transfer_ipfs-resolve", Description: "Resolve IPFS CID to gateway URL", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"cid": map[string]any{"type": "string"}}, "required": []string{"cid"}}, Handler: toolHandler(handleTransferIPFSResolve)},
		{Name: "transfer_store-search", Description: "Search transfer store catalog", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}, Handler: toolHandler(handleTransferStoreSearch)},
		{Name: "transfer_store-info", Description: "Get store item by id", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}}, Handler: toolHandler(handleTransferStoreInfo)},
		{Name: "transfer_store-download", Description: "Record download intent for store item", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}}, Handler: toolHandler(handleTransferStoreDownload)},
		{Name: "transfer_store-featured", Description: "List featured store items", InputSchema: obj, Handler: toolHandler(handleTransferStoreFeatured)},
		{Name: "transfer_store-trending", Description: "List trending store items", InputSchema: obj, Handler: toolHandler(handleTransferStoreTrending)},
		{Name: "transfer_plugin-search", Description: "Search plugin catalog", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}, Handler: toolHandler(handleTransferPluginSearch)},
		{Name: "transfer_plugin-info", Description: "Get plugin by id", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}}, Handler: toolHandler(handleTransferPluginInfo)},
		{Name: "transfer_plugin-featured", Description: "List featured plugins", InputSchema: obj, Handler: toolHandler(handleTransferPluginFeatured)},
		{Name: "transfer_plugin-official", Description: "List official plugins", InputSchema: obj, Handler: toolHandler(handleTransferPluginOfficial)},
	}
}

// RegisterTransferTools 向注册表登记带本地目录文件的 IPFS/Transfer 桥接 MCP 工具。
func RegisterTransferTools(reg *mcp.ToolRegistry) error {
	for _, t := range transferTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handleTransferDetectPII 处理 transfer_detect-pii：text 必填；复用 detectPIIIssues。
func handleTransferDetectPII(_ context.Context, m map[string]any) mcp.MCPToolResult {
	text := strArg(m, "text")
	if text == "" {
		return mcp.MCPToolResult{OK: false, Error: "text required"}
	}
	issues := detectPIIIssues(text)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"pii_detected": len(issues) > 0, "issues": issues}}
}

// handleTransferIPFSResolve 处理 transfer_ipfs-resolve：cid 必填；返回 Pinata 公共网关 URL。
func handleTransferIPFSResolve(_ context.Context, m map[string]any) mcp.MCPToolResult {
	cid := strings.TrimSpace(strArg(m, "cid"))
	if cid == "" {
		return mcp.MCPToolResult{OK: false, Error: "cid required"}
	}
	url := "https://gateway.pinata.cloud/ipfs/" + cid
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"cid": cid, "gateway_url": url}}
}

// filterItems 按 query 在名称/描述/标签中做包含匹配；query 为空则返回 items 的拷贝。
func filterItems(items []transferItem, query string) []transferItem {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return append([]transferItem(nil), items...)
	}
	var out []transferItem
	for _, it := range items {
		hay := strings.ToLower(it.Name + " " + it.Description + " " + strings.Join(it.Tags, " "))
		if strings.Contains(hay, q) {
			out = append(out, it)
		}
	}
	return out
}

// handleTransferStoreSearch 处理 transfer_store-search：可选 query；搜索 StoreItems。
func handleTransferStoreSearch(_ context.Context, m map[string]any) mcp.MCPToolResult {
	transferMu.Lock()
	items := append([]transferItem(nil), transferData.StoreItems...)
	transferMu.Unlock()
	q := strArg(m, "query")
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"results": filterItems(items, q), "count": len(filterItems(items, q))}}
}

// handleTransferStoreInfo 处理 transfer_store-info：id 必填；按 ID 查找商店项。
func handleTransferStoreInfo(_ context.Context, m map[string]any) mcp.MCPToolResult {
	id := strArg(m, "id")
	transferMu.Lock()
	defer transferMu.Unlock()
	for _, it := range transferData.StoreItems {
		if it.ID == id {
			return mcp.MCPToolResult{OK: true, Data: map[string]any{"item": it}}
		}
	}
	return mcp.MCPToolResult{OK: false, Error: "not found"}
}

// handleTransferStoreDownload 处理 transfer_store-download：校验 id 存在后调用 transferSave 记录本地下载意图（非真实拉取）。
func handleTransferStoreDownload(_ context.Context, m map[string]any) mcp.MCPToolResult {
	id := strArg(m, "id")
	transferMu.Lock()
	found := false
	for _, it := range transferData.StoreItems {
		if it.ID == id {
			found = true
			break
		}
	}
	transferMu.Unlock()
	if !found {
		return mcp.MCPToolResult{OK: false, Error: "not found"}
	}
	_ = transferSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"id": id, "recorded": true, "note": "download logged locally"}}
}

// handleTransferStoreFeatured 处理 transfer_store-featured：列出 Featured=true 的商店项。
func handleTransferStoreFeatured(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	transferMu.Lock()
	var out []transferItem
	for _, it := range transferData.StoreItems {
		if it.Featured {
			out = append(out, it)
		}
	}
	transferMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"items": out}}
}

// handleTransferStoreTrending 处理 transfer_store-trending：列出 Trending=true 的商店项。
func handleTransferStoreTrending(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	transferMu.Lock()
	var out []transferItem
	for _, it := range transferData.StoreItems {
		if it.Trending {
			out = append(out, it)
		}
	}
	transferMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"items": out}}
}

// handleTransferPluginSearch 处理 transfer_plugin-search：可选 query；在 Plugins 目录中搜索。
func handleTransferPluginSearch(_ context.Context, m map[string]any) mcp.MCPToolResult {
	transferMu.Lock()
	items := append([]transferItem(nil), transferData.Plugins...)
	transferMu.Unlock()
	q := strArg(m, "query")
	hits := filterItems(items, q)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"results": hits, "count": len(hits)}}
}

// handleTransferPluginInfo 处理 transfer_plugin-info：id 必填；按 ID 查找插件项。
func handleTransferPluginInfo(_ context.Context, m map[string]any) mcp.MCPToolResult {
	id := strArg(m, "id")
	transferMu.Lock()
	defer transferMu.Unlock()
	for _, it := range transferData.Plugins {
		if it.ID == id {
			return mcp.MCPToolResult{OK: true, Data: map[string]any{"plugin": it}}
		}
	}
	return mcp.MCPToolResult{OK: false, Error: "not found"}
}

// handleTransferPluginFeatured 处理 transfer_plugin-featured：列出精选插件。
func handleTransferPluginFeatured(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	transferMu.Lock()
	var out []transferItem
	for _, it := range transferData.Plugins {
		if it.Featured {
			out = append(out, it)
		}
	}
	transferMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"plugins": out}}
}

// handleTransferPluginOfficial 处理 transfer_plugin-official：列出 Official=true 的插件。
func handleTransferPluginOfficial(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	transferMu.Lock()
	var out []transferItem
	for _, it := range transferData.Plugins {
		if it.Official {
			out = append(out, it)
		}
	}
	transferMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"plugins": out}}
}
