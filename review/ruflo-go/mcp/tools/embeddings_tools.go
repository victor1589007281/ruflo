package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/embeddings"
)

// 本文件：向量嵌入 MCP 工具，使用 pkg/embeddings.HashEmbed384 生成确定性伪嵌入并存入 JSON 仓库。
//
// 设计思路：generate/batch 追加条目并分配递增 id；search 对查询向量与库内向量做点积（与 hash 嵌入配合的相似度代理）；
// compare 计算两文本嵌入的点积；无外部模型调用，backend 标识为 hash384。

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

// embStoreFile 持久化嵌入仓库：条目列表、就绪标志与序号发生器。

var (
	embMu    sync.Mutex
	embStore = &embStoreFile{Entries: make([]embEntry, 0)}
)

// embStorePath 返回 embeddings/store.json 路径。
func embStorePath() string {
	return filepath.Join(resolveDataDir(), "embeddings", "store.json")
}

// embLoad 从磁盘加载嵌入仓库；Entries 为 nil 时初始化为空切片。
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

// embSave 拷贝当前 Entries/Ready/Seq 后写入 embStorePath。
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

// cosineSim 对等长向量计算点积（此处未做 L2 归一化，与 HashEmbed384 输出配合作为相似度分数）。
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

// embeddingsTools 先 embLoad，再注册生成、批量、搜索、初始化、比较、神经联动、双曲桩与状态工具。
func embeddingsTools() []*mcp.MCPTool {
	embLoad()
	return []*mcp.MCPTool{
		{Name: "embeddings_generate", Description: "Generate embedding for text", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}, Handler: toolHandler(handleEmbeddingsGenerate)},
		{Name: "embeddings_batch", Description: "Batch embed multiple texts", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"texts": map[string]any{"type": "array"}}}, Handler: toolHandler(handleEmbeddingsBatch)},
		{Name: "embeddings_search", Description: "Semantic search over stored embeddings", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "k": map[string]any{"type": "number"}}}, Handler: toolHandler(handleEmbeddingsSearch)},
		{Name: "embeddings_init", Description: "Initialize embedding service", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleEmbeddingsInit)},
		{Name: "embeddings_compare", Description: "Cosine similarity between two texts", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "string"}}, "required": []string{"a", "b"}}, Handler: toolHandler(handleEmbeddingsCompare)},
		{Name: "embeddings_neural", Description: "Link embedding store to neural pattern count", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleEmbeddingsNeural)},
		{Name: "embeddings_hyperbolic", Description: "Poincaré ball mapping and hyperbolic distance (hash embeddings)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleEmbeddingsHyperbolic)},
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

// handleEmbeddingsSearch 对 query 生成查询向量，与库内所有条目计算 cosineSim，按分数降序取前 k（默认 5）条。
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

// handleEmbeddingsNeural 聚合嵌入仓库、SONA 与统一 memory（若已初始化）的统计。
func handleEmbeddingsNeural(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	embMu.Lock()
	nEmb := len(embStore.Entries)
	seq := embStore.Seq
	embMu.Unlock()
	globalState.mu.RLock()
	nPat := len(globalState.neural.Patterns)
	globalState.mu.RUnlock()
	data := map[string]any{
		"embedding_store_entries": nEmb,
		"embedding_store_seq":     seq,
		"neural_patterns_mcp":     nPat,
	}
	if globalState.sona != nil {
		st := globalState.sona.GetStats()
		data["sona"] = map[string]any{
			"total_patterns":       st.TotalPatterns,
			"total_trajectories": st.TotalTrajectories,
			"active_trajectories": st.ActiveTrajectories,
			"avg_confidence":      st.AvgConfidence,
			"signal_count":        st.SignalCount,
		}
	}
	_ = InitDefaultMemory()
	if u := getUnifiedMemory(); u != nil {
		ms := u.Stats()
		if ms != nil {
			data["unified_memory"] = map[string]any{
				"total_entries": ms.TotalEntries,
				"index_size":    ms.IndexSize,
				"cache_hits":    ms.CacheHits,
				"cache_misses":  ms.CacheMisses,
			}
		}
	}
	return mcp.MCPToolResult{OK: true, Data: data}
}

// handleEmbeddingsHyperbolic 使用 hash 嵌入经 EuclideanToHyperbolic 映射并计算 HyperbolicDistance。
func handleEmbeddingsHyperbolic(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	const c = 1.0
	va := embeddings.HashEmbed384("hyperbolic-anchor-a")
	vb := embeddings.HashEmbed384("hyperbolic-anchor-b")
	ha := embeddings.EuclideanToHyperbolic(va, c)
	hb := embeddings.EuclideanToHyperbolic(vb, c)
	dist := embeddings.HyperbolicDistance(ha, hb)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"mode": "poincare", "available": true, "curvature": c,
		"dim": len(va),
		"hyperbolic_distance": dist,
		"distance_finite":     !math.IsNaN(dist) && !math.IsInf(dist, 0),
	}}
}

// handleEmbeddingsStatus 返回 Ready、条目数、序号与后端标识 hash384。
func handleEmbeddingsStatus(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	embMu.Lock()
	cp := embStore
	embMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"ready": cp.Ready, "entries": len(cp.Entries), "seq": cp.Seq, "backend": "hash384"}}
}

// handleEmbeddingsInit 将嵌入服务标记为就绪（Ready=true）并持久化，表示可对外提供嵌入能力。
func handleEmbeddingsInit(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	embMu.Lock()
	embStore.Ready = true
	embMu.Unlock()
	if err := embSave(); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"ready": true, "backend": "hash384"}}
}
