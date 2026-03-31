package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/memory"
)

func memoryTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{
			Name:        "memory_store",
			Description: "Store a memory entry",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":       map[string]any{"type": "string"},
					"value":     map[string]any{"type": "string"},
					"namespace": map[string]any{"type": "string"},
				},
				"required": []string{"key", "value"},
			},
			Handler: handleMemoryStore,
		},
		{
			Name:        "memory_retrieve",
			Description: "Retrieve memory by key",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":       map[string]any{"type": "string"},
					"namespace": map[string]any{"type": "string"},
				},
				"required": []string{"key"},
			},
			Handler: handleMemoryRetrieve,
		},
		{
			Name:        "memory_search",
			Description: "Search memory by text substring (lightweight)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":     map[string]any{"type": "string"},
					"namespace": map[string]any{"type": "string"},
					"limit":     map[string]any{"type": "number"},
					"threshold": map[string]any{"type": "number", "description": "min normalized score 0-1"},
				},
			},
			Handler: handleMemorySearch,
		},
		{
			Name:        "memory_delete",
			Description: "Delete a memory key in namespace",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":       map[string]any{"type": "string"},
					"namespace": map[string]any{"type": "string"},
				},
				"required": []string{"key"},
			},
			Handler: handleMemoryDelete,
		},
		{
			Name:        "memory_list",
			Description: "List keys in namespace",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace": map[string]any{"type": "string"},
					"limit":     map[string]any{"type": "number"},
				},
			},
			Handler: handleMemoryList,
		},
		{
			Name:        "memory_stats",
			Description: "Memory store statistics",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleMemoryStats,
		},
		{
			Name:        "memory_init",
			Description: "Initialize or reset in-memory memory store",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"force": map[string]any{"type": "boolean"},
				},
			},
			Handler: handleMemoryInit,
		},
		{
			Name:        "memory_migrate",
			Description: "Export in-proc memory map to JSON and optionally copy into unified memory",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"import_unified": map[string]any{"type": "boolean"},
				},
			},
			Handler: handleMemoryMigrate,
		},
	}
}

func nsKey(namespace string) string {
	if namespace == "" {
		return "default"
	}
	return namespace
}

type memStoreArgs struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Namespace string `json:"namespace"`
}

func handleMemoryStore(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a memStoreArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Key == "" {
		return nil, fmt.Errorf("key is required")
	}
	_ = InitDefaultMemory()
	if u := getUnifiedMemory(); u != nil {
		n := nsKey(a.Namespace)
		in := memory.MemoryEntryInput{Key: a.Key, Value: a.Value, Namespace: n}
		if err := u.Store(in); err != nil {
			return nil, err
		}
		ent, err := u.Retrieve(a.Key, n)
		if err != nil {
			return nil, err
		}
		return jsonOK(map[string]any{"ok": true, "entry": ent, "backend": "sqlite+hnsw"})
	}
	n := nsKey(a.Namespace)
	t := now()
	entry := &api.MemoryEntry{
		Key:       a.Key,
		Value:     a.Value,
		Namespace: n,
		CreatedAt: t,
		UpdatedAt: t,
	}
	globalState.mu.Lock()
	if globalState.memory[n] == nil {
		globalState.memory[n] = make(map[string]*api.MemoryEntry)
	}
	globalState.memory[n][a.Key] = entry
	globalState.mu.Unlock()
	return jsonOK(map[string]any{"ok": true, "entry": entry})
}

type memKeyArgs struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace"`
}

func handleMemoryRetrieve(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a memKeyArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Key == "" {
		return nil, fmt.Errorf("key is required")
	}
	_ = InitDefaultMemory()
	if u := getUnifiedMemory(); u != nil {
		n := nsKey(a.Namespace)
		e, err := u.Retrieve(a.Key, n)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("not found")
			}
			return nil, err
		}
		return jsonOK(map[string]any{"entry": e, "backend": "sqlite+hnsw"})
	}
	n := nsKey(a.Namespace)
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	ns := globalState.memory[n]
	if ns == nil {
		return nil, fmt.Errorf("not found")
	}
	e, ok := ns[a.Key]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return jsonOK(map[string]any{"entry": e})
}

type memSearchArgs struct {
	Query     string  `json:"query"`
	Namespace string  `json:"namespace"`
	Limit     int     `json:"limit"`
	Threshold float64 `json:"threshold"`
}

func handleMemorySearch(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a memSearchArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}
	if a.Threshold <= 0 {
		a.Threshold = 0.01
	}
	_ = InitDefaultMemory()
	if u := getUnifiedMemory(); u != nil {
		n := nsKey(a.Namespace)
		opts := api.SearchOptions{
			Namespace: n,
			K:         a.Limit,
			EF:        0,
			MinScore:  a.Threshold,
		}
		res, err := u.Search(a.Query, opts)
		if err != nil {
			return nil, err
		}
		return jsonOK(map[string]any{"results": res, "count": len(res), "backend": "sqlite+hnsw"})
	}
	q := strings.ToLower(strings.TrimSpace(a.Query))
	n := nsKey(a.Namespace)
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	type hit struct {
		Entry *api.MemoryEntry `json:"entry"`
		Score float64          `json:"score"`
	}
	var out []hit
	m := globalState.memory[n]
	if m != nil {
		for _, e := range m {
			score := substringScore(q, strings.ToLower(e.Key+" "+e.Value))
			if score >= a.Threshold {
				out = append(out, hit{Entry: e, Score: score})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > a.Limit {
		out = out[:a.Limit]
	}
	res := make([]api.SearchResult, 0, len(out))
	for _, h := range out {
		res = append(res, api.SearchResult{Entry: h.Entry, Score: h.Score})
	}
	return jsonOK(map[string]any{"results": res, "count": len(res)})
}

func substringScore(query, hay string) float64 {
	if query == "" {
		return 1
	}
	if strings.Contains(hay, query) {
		return 1
	}
	parts := strings.Fields(query)
	matched := 0
	for _, p := range parts {
		if p != "" && strings.Contains(hay, p) {
			matched++
		}
	}
	if len(parts) == 0 {
		return 0
	}
	return float64(matched) / float64(len(parts))
}

func handleMemoryDelete(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a memKeyArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Key == "" {
		return nil, fmt.Errorf("key is required")
	}
	_ = InitDefaultMemory()
	if u := getUnifiedMemory(); u != nil {
		n := nsKey(a.Namespace)
		if err := u.Delete(a.Key, n); err != nil {
			return nil, err
		}
		return jsonOK(map[string]any{"ok": true, "deleted": true, "backend": "sqlite+hnsw"})
	}
	n := nsKey(a.Namespace)
	globalState.mu.Lock()
	defer globalState.mu.Unlock()
	ns := globalState.memory[n]
	if ns == nil {
		return jsonOK(map[string]any{"ok": false, "deleted": false})
	}
	_, ok := ns[a.Key]
	if ok {
		delete(ns, a.Key)
	}
	return jsonOK(map[string]any{"ok": ok, "deleted": ok})
}

type memListArgs struct {
	Namespace string `json:"namespace"`
	Limit     int    `json:"limit"`
}

func handleMemoryList(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a memListArgs
	_ = parseArgs(args, &a)
	if a.Limit <= 0 {
		a.Limit = 100
	}
	_ = InitDefaultMemory()
	if u := getUnifiedMemory(); u != nil {
		n := nsKey(a.Namespace)
		entries, err := u.List(n, a.Limit, 0)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(entries))
		for _, e := range entries {
			keys = append(keys, e.Key)
		}
		return jsonOK(map[string]any{"keys": keys, "count": len(keys), "backend": "sqlite+hnsw"})
	}
	n := nsKey(a.Namespace)
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	keys := make([]string, 0)
	if ns := globalState.memory[n]; ns != nil {
		for k := range ns {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > a.Limit {
		keys = keys[:a.Limit]
	}
	return jsonOK(map[string]any{"keys": keys, "count": len(keys)})
}

func handleMemoryStats(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	_ = InitDefaultMemory()
	if u := getUnifiedMemory(); u != nil {
		st := u.Stats()
		return jsonOK(map[string]any{
			"backend":      "sqlite+hnsw",
			"entries":      st.TotalEntries,
			"index_size":   st.IndexSize,
			"cache_hits":   st.CacheHits,
			"cache_misses": st.CacheMisses,
			"initialized":  true,
		})
	}
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	total := 0
	for _, m := range globalState.memory {
		total += len(m)
	}
	return jsonOK(map[string]any{
		"namespaces":  len(globalState.memory),
		"entries":     total,
		"initialized": globalState.memoryInitialized,
		"backend":     "memory",
	})
}

type memInitArgs struct {
	Force bool `json:"force"`
}

func handleMemoryInit(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a memInitArgs
	_ = parseArgs(args, &a)
	if err := ReopenMemory(a.Force); err != nil {
		return nil, err
	}
	globalState.mu.Lock()
	globalState.memory = make(map[string]map[string]*api.MemoryEntry)
	globalState.memoryInitialized = true
	globalState.mu.Unlock()
	return jsonOK(map[string]any{"ok": true, "initialized": true, "force": a.Force, "backend": "sqlite+hnsw"})
}

type memMigrateArgs struct {
	ImportUnified bool `json:"import_unified"`
}

func handleMemoryMigrate(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a memMigrateArgs
	_ = parseArgs(args, &a)
	globalState.mu.RLock()
	export := make(map[string]map[string]*api.MemoryEntry)
	for ns, m := range globalState.memory {
		export[ns] = make(map[string]*api.MemoryEntry)
		for k, v := range m {
			if v != nil {
				cp := *v
				export[ns][k] = &cp
			}
		}
	}
	globalState.mu.RUnlock()
	p := filepath.Join(resolveDataDir(), "memory", "migrate-export.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return nil, err
	}
	imported := 0
	if a.ImportUnified {
		_ = InitDefaultMemory()
		if u := getUnifiedMemory(); u != nil {
			for ns, m := range export {
				for k, e := range m {
					if e == nil {
						continue
					}
					in := memory.MemoryEntryInput{Key: k, Value: e.Value, Namespace: ns}
					if err := u.Store(in); err == nil {
						imported++
					}
				}
			}
		}
	}
	return jsonOK(map[string]any{"ok": true, "path": p, "unified_imported": imported})
}
