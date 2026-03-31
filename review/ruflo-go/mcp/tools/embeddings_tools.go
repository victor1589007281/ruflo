package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/embeddings"
)

type embEntry struct {
	ID   string    `json:"id"`
	Text string    `json:"text"`
	Vec  []float32 `json:"vec"`
}

type embStoreFile struct {
	Entries []embEntry `json:"entries"`
	Ready   bool       `json:"ready"`
	Seq     int        `json:"seq"`
}

var (
	embMu    sync.Mutex
	embStore = &embStoreFile{Entries: make([]embEntry, 0)}
)

func embStorePath() string {
	return filepath.Join(resolveDataDir(), "embeddings", "store.json")
}

func embLoad() {
	embMu.Lock()
	defer embMu.Unlock()
	b, err := os.ReadFile(embStorePath())
	if err != nil {
		return
	}
	var f embStoreFile
	if json.Unmarshal(b, &f) == nil {
		embStore = &f
		if embStore.Entries == nil {
			embStore.Entries = make([]embEntry, 0)
		}
	}
}

func embSave() error {
	embMu.Lock()
	cp := embStoreFile{
		Ready:   embStore.Ready,
		Seq:     embStore.Seq,
		Entries: append([]embEntry(nil), embStore.Entries...),
	}
	embMu.Unlock()
	return writeJSONFile(embStorePath(), cp)
}

func cosineSim(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

func embeddingsTools() []*mcp.MCPTool {
	embLoad()
	return []*mcp.MCPTool{
		{Name: "embeddings_generate", Description: "Generate embedding for text", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}, Handler: toolHandler(handleEmbeddingsGenerate)},
		{Name: "embeddings_batch", Description: "Batch embed multiple texts", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"texts": map[string]any{"type": "array"}}}, Handler: toolHandler(handleEmbeddingsBatch)},
		{Name: "embeddings_search", Description: "Semantic search over stored embeddings", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "k": map[string]any{"type": "number"}}}, Handler: toolHandler(handleEmbeddingsSearch)},
		{Name: "embeddings_init", Description: "Initialize embedding service", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleEmbeddingsInit)},
		{Name: "embeddings_compare", Description: "Cosine similarity between two texts", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "string"}}, "required": []string{"a", "b"}}, Handler: toolHandler(handleEmbeddingsCompare)},
		{Name: "embeddings_neural", Description: "Link embedding store to neural pattern count", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleEmbeddingsNeural)},
		{Name: "embeddings_hyperbolic", Description: "Hyperbolic embedding mode (stub)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleEmbeddingsHyperbolic)},
		{Name: "embeddings_status", Description: "Embedding store status", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleEmbeddingsStatus)},
	}
}

func RegisterEmbeddingsTools(reg *mcp.ToolRegistry) error {
	for _, t := range embeddingsTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handleEmbeddingsGenerate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	text := strArg(m, "text")
	if text == "" {
		return mcp.MCPToolResult{OK: false, Error: "text required"}
	}
	vec := embeddings.HashEmbed384(text)
	embMu.Lock()
	embStore.Seq++
	id := "e-" + strconv.Itoa(embStore.Seq)
	embStore.Entries = append(embStore.Entries, embEntry{ID: id, Text: text, Vec: vec})
	embMu.Unlock()
	if err := embSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"id": id, "dim": len(vec), "preview": text}}
}

func handleEmbeddingsBatch(_ context.Context, m map[string]any) mcp.MCPToolResult {
	var texts []string
	if raw, ok := m["texts"]; ok && raw != nil {
		switch arr := raw.(type) {
		case []any:
			for _, x := range arr {
				texts = append(texts, fmt.Sprint(x))
			}
		case []string:
			texts = append(texts, arr...)
		}
	}
	if len(texts) == 0 {
		return mcp.MCPToolResult{OK: false, Error: "texts required"}
	}
	out := make([]map[string]any, 0, len(texts))
	embMu.Lock()
	for _, text := range texts {
		if text == "" {
			continue
		}
		vec := embeddings.HashEmbed384(text)
		embStore.Seq++
		id := "e-" + strconv.Itoa(embStore.Seq)
		embStore.Entries = append(embStore.Entries, embEntry{ID: id, Text: text, Vec: vec})
		out = append(out, map[string]any{"id": id, "dim": len(vec)})
	}
	embMu.Unlock()
	if err := embSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"count": len(out), "items": out}}
}

func handleEmbeddingsSearch(_ context.Context, m map[string]any) mcp.MCPToolResult {
	q := strArg(m, "query")
	if q == "" {
		return mcp.MCPToolResult{OK: false, Error: "query required"}
	}
	k := 5
	if v, ok := m["k"]; ok {
		switch t := v.(type) {
		case float64:
			k = int(t)
		case int:
			k = t
		}
	}
	if k <= 0 {
		k = 5
	}
	qv := embeddings.HashEmbed384(q)
	embMu.Lock()
	ents := append([]embEntry(nil), embStore.Entries...)
	embMu.Unlock()
	type hit struct {
		ID    string  `json:"id"`
		Text  string  `json:"text"`
		Score float64 `json:"score"`
	}
	var hits []hit
	for _, e := range ents {
		s := cosineSim(qv, e.Vec)
		hits = append(hits, hit{ID: e.ID, Text: e.Text, Score: s})
	}
	for i := 0; i < len(hits); i++ {
		for j := i + 1; j < len(hits); j++ {
			if hits[j].Score > hits[i].Score {
				hits[i], hits[j] = hits[j], hits[i]
			}
		}
	}
	if len(hits) > k {
		hits = hits[:k]
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"results": hits}}
}

func handleEmbeddingsCompare(_ context.Context, m map[string]any) mcp.MCPToolResult {
	a := strArg(m, "a")
	b := strArg(m, "b")
	if a == "" || b == "" {
		return mcp.MCPToolResult{OK: false, Error: "a and b required"}
	}
	va := embeddings.HashEmbed384(a)
	vb := embeddings.HashEmbed384(b)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"similarity": cosineSim(va, vb)}}
}

func handleEmbeddingsNeural(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	embMu.Lock()
	nEmb := len(embStore.Entries)
	embMu.Unlock()
	globalState.mu.RLock()
	nPat := len(globalState.neural.Patterns)
	globalState.mu.RUnlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"embeddings": nEmb, "neural_patterns": nPat}}
}

func handleEmbeddingsHyperbolic(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"mode": "poincare", "available": false, "note": "hyperbolic path not enabled in Go runtime"}}
}

func handleEmbeddingsStatus(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	embMu.Lock()
	cp := embStore
	embMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"ready": cp.Ready, "entries": len(cp.Entries), "seq": cp.Seq, "backend": "hash384"}}
}

func handleEmbeddingsInit(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	embMu.Lock()
	embStore.Ready = true
	embMu.Unlock()
	if err := embSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"ready": true, "backend": "hash384"}}
}
