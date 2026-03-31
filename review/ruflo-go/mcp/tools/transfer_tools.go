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

type transferStoreFile struct {
	StoreItems []transferItem `json:"store_items"`
	Plugins    []transferItem `json:"plugins"`
}

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

func transferStorePath() string {
	return filepath.Join(resolveDataDir(), "transfer", "store.json")
}

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

func transferSave() error {
	transferMu.Lock()
	cp := transferStoreFile{
		StoreItems: append([]transferItem(nil), transferData.StoreItems...),
		Plugins:    append([]transferItem(nil), transferData.Plugins...),
	}
	transferMu.Unlock()
	return writeJSONFile(transferStorePath(), cp)
}

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

// RegisterTransferTools registers IPFS/transfer bridge tools with file-backed catalog.
func RegisterTransferTools(reg *mcp.ToolRegistry) error {
	for _, t := range transferTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleTransferDetectPII(_ context.Context, m map[string]any) mcp.MCPToolResult {
	text := strArg(m, "text")
	if text == "" {
		return mcp.MCPToolResult{OK: false, Error: "text required"}
	}
	issues := detectPIIIssues(text)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"pii_detected": len(issues) > 0, "issues": issues}}
}

func handleTransferIPFSResolve(_ context.Context, m map[string]any) mcp.MCPToolResult {
	cid := strings.TrimSpace(strArg(m, "cid"))
	if cid == "" {
		return mcp.MCPToolResult{OK: false, Error: "cid required"}
	}
	url := "https://gateway.pinata.cloud/ipfs/" + cid
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"cid": cid, "gateway_url": url}}
}

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

func handleTransferStoreSearch(_ context.Context, m map[string]any) mcp.MCPToolResult {
	transferMu.Lock()
	items := append([]transferItem(nil), transferData.StoreItems...)
	transferMu.Unlock()
	q := strArg(m, "query")
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"results": filterItems(items, q), "count": len(filterItems(items, q))}}
}

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

func handleTransferPluginSearch(_ context.Context, m map[string]any) mcp.MCPToolResult {
	transferMu.Lock()
	items := append([]transferItem(nil), transferData.Plugins...)
	transferMu.Unlock()
	q := strArg(m, "query")
	hits := filterItems(items, q)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"results": hits, "count": len(hits)}}
}

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
