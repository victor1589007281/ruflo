package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/embeddings"
	"github.com/ruflo/ruflo-go/pkg/memory"
	nlp "github.com/ruflo/ruflo-go/pkg/neural"
)

var (
	ruvMu       sync.Mutex
	ruvIndex    *memory.HNSWIndex
	ruvLabels   = map[uint64]string{}
	ruvNext     uint64
	ruvLoraPath string
)

// ruvIndexLocked returns the HNSW index; caller must hold ruvMu.
func ruvIndexLocked() *memory.HNSWIndex {
	if ruvIndex == nil {
		ruvIndex = memory.NewHNSWIndex(embeddings.HashEmbeddingDim, memory.CosineDistance)
	}
	return ruvIndex
}

func ruvllmTools() []*mcp.MCPTool {
	ruvLoraPath = filepath.Join(resolveDataDir(), "ruvllm", "microlora.json")
	obj := map[string]any{"type": "object", "properties": map[string]any{}}
	return []*mcp.MCPTool{
		{Name: "ruvllm_status", Description: "RuVLLM HNSW + SONA snapshot", InputSchema: obj, Handler: toolHandler(handleRuvLLMStatus)},
		{Name: "ruvllm_hnsw_create", Description: "Reset or create local HNSW index", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"reset": map[string]any{"type": "boolean"}}}, Handler: toolHandler(handleRuvLLMHNSWCreate)},
		{Name: "ruvllm_hnsw_add", Description: "Add labeled vector from text (hash embed)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}}, "required": []string{"text"}}, Handler: toolHandler(handleRuvLLMHNSWAdd)},
		{Name: "ruvllm_hnsw_route", Description: "Nearest neighbors for query text", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "k": map[string]any{"type": "number"}}, "required": []string{"query"}}, Handler: toolHandler(handleRuvLLMHNSWRoute)},
		{Name: "ruvllm_sona_create", Description: "Begin SONA trajectory", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"task_id": map[string]any{"type": "string"}}}, Handler: toolHandler(handleRuvLLMSONACreate)},
		{Name: "ruvllm_sona_adapt", Description: "End trajectory with verdict (SONA)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"trajectory_id": map[string]any{"type": "string"}, "verdict": map[string]any{"type": "string"}}, "required": []string{"trajectory_id"}}, Handler: toolHandler(handleRuvLLMSONAAdapt)},
		{Name: "ruvllm_microlora_create", Description: "Register micro-LoRA adapter metadata", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}, Handler: toolHandler(handleRuvLLMMicroLoraCreate)},
		{Name: "ruvllm_microlora_adapt", Description: "Bump micro-LoRA usage counter", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}, Handler: toolHandler(handleRuvLLMMicroLoraAdapt)},
		{Name: "ruvllm_chat_format", Description: "Normalize chat messages to JSON layout", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"role": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}}, Handler: toolHandler(handleRuvLLMChatFormat)},
		{Name: "ruvllm_generate_config", Description: "Emit RuVLLM JSON config template", InputSchema: obj, Handler: toolHandler(handleRuvLLMGenerateConfig)},
	}
}

// RegisterRuvLLMTools registers neural / HNSW bridge tools.
func RegisterRuvLLMTools(reg *mcp.ToolRegistry) error {
	for _, t := range ruvllmTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleRuvLLMStatus(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	ruvMu.Lock()
	nLab := len(ruvLabels)
	has := ruvIndex != nil
	ruvMu.Unlock()
	pat := 0
	if globalState.sona != nil {
		pat = len(globalState.sona.Patterns())
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"hnsw_active": has, "vectors": nLab, "sona_patterns": pat,
	}}
}

func handleRuvLLMHNSWCreate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	reset := false
	if b, ok := m["reset"].(bool); ok {
		reset = b
	}
	ruvMu.Lock()
	if reset {
		ruvIndex = memory.NewHNSWIndex(embeddings.HashEmbeddingDim, memory.CosineDistance)
		ruvLabels = map[uint64]string{}
		ruvNext = 0
	} else {
		ruvIndexLocked()
	}
	ruvMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"reset": reset, "dim": embeddings.HashEmbeddingDim}}
}

func handleRuvLLMHNSWAdd(_ context.Context, m map[string]any) mcp.MCPToolResult {
	text := strArg(m, "text")
	if text == "" {
		return mcp.MCPToolResult{OK: false, Error: "text required"}
	}
	idStr := strArg(m, "id")
	vec := embeddings.HashEmbed384(text)
	ruvMu.Lock()
	idx := ruvIndexLocked()
	nid := atomic.AddUint64(&ruvNext, 1)
	if idStr != "" {
		ruvLabels[nid] = idStr
	} else {
		ruvLabels[nid] = "v-" + strconv.FormatUint(nid, 10)
	}
	err := idx.Insert(nid, vec)
	lbl := ruvLabels[nid]
	ruvMu.Unlock()
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"internal_id": nid, "label": lbl, "dim": len(vec)}}
}

func handleRuvLLMHNSWRoute(_ context.Context, m map[string]any) mcp.MCPToolResult {
	q := strArg(m, "query")
	if q == "" {
		return mcp.MCPToolResult{OK: false, Error: "query required"}
	}
	k := 5
	if v, ok := m["k"].(float64); ok {
		k = int(v)
	}
	if k <= 0 {
		k = 5
	}
	vec := embeddings.HashEmbed384(q)
	ruvMu.Lock()
	idx := ruvIndexLocked()
	hits := idx.Search(vec, k, 64)
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"id": h.ID, "distance": h.Distance, "label": ruvLabels[h.ID],
		})
	}
	ruvMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"neighbors": out}}
}

func handleRuvLLMSONACreate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	tid := strArg(m, "task_id")
	if globalState.sona == nil {
		return mcp.MCPToolResult{OK: false, Error: "sona unavailable"}
	}
	tr := globalState.sona.BeginTrajectory(tid)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"trajectory_id": tr}}
}

func handleRuvLLMSONAAdapt(_ context.Context, m map[string]any) mcp.MCPToolResult {
	tr := strArg(m, "trajectory_id")
	verdict := strArg(m, "verdict")
	if tr == "" {
		return mcp.MCPToolResult{OK: false, Error: "trajectory_id required"}
	}
	if globalState.sona == nil {
		return mcp.MCPToolResult{OK: false, Error: "sona unavailable"}
	}
	if err := globalState.sona.EndTrajectory(tr, verdict); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"trajectory_id": tr, "verdict": verdict}}
}

type microLoraFile struct {
	Adapters map[string]int `json:"adapters"`
}

func handleRuvLLMMicroLoraCreate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if name == "" {
		name = "default"
	}
	_ = os.MkdirAll(filepath.Dir(ruvLoraPath), 0o755)
	f := microLoraFile{Adapters: map[string]int{}}
	if b, err := os.ReadFile(ruvLoraPath); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	if f.Adapters == nil {
		f.Adapters = map[string]int{}
	}
	f.Adapters[name] = 0
	raw, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(ruvLoraPath, raw, 0o644); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"name": name}}
}

func handleRuvLLMMicroLoraAdapt(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if name == "" {
		return mcp.MCPToolResult{OK: false, Error: "name required"}
	}
	f := microLoraFile{Adapters: map[string]int{}}
	if b, err := os.ReadFile(ruvLoraPath); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	if f.Adapters == nil {
		f.Adapters = map[string]int{}
	}
	f.Adapters[name]++
	raw, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(ruvLoraPath, raw, 0o644); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"name": name, "steps": f.Adapters[name]}}
}

func handleRuvLLMChatFormat(_ context.Context, m map[string]any) mcp.MCPToolResult {
	role := strArg(m, "role")
	if role == "" {
		role = "user"
	}
	content := strArg(m, "content")
	msgs := []map[string]any{{"role": role, "content": content}}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"messages": msgs}}
}

func handleRuvLLMGenerateConfig(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	cfg := map[string]any{
		"hnsw": map[string]any{"dim": embeddings.HashEmbeddingDim, "metric": "cosine", "ef": 64},
		"sona": map[string]any{"learning_rate": nlp.DefaultSONAConfig().LearningRate},
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"config": cfg}}
}
