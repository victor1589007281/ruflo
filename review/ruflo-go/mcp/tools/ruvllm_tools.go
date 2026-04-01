package tools

// 本文件实现 RuVLLM 相关 MCP 工具：本地 HNSW 向量索引（哈希嵌入）、SONA 轨迹、micro-LoRA 元数据文件与聊天/配置辅助。
//
// 设计思路：
//   - HNSW 与标签映射驻留进程内存，用互斥锁保护；向量由 embeddings.HashEmbed384 生成，适合无外部 API 的快速原型。
//   - SONA 依赖 globalState.sona，未初始化时相关工具返回不可用错误。
//   - micro-LoRA 以 JSON 文件记录适配器使用计数，非真实训练管线。

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
	ruvLoraPath string // micro-LoRA JSON 路径，在 ruvllmTools 中赋值
)

// ruvIndexLocked 返回进程内 HNSW 索引；调用方必须已持有 ruvMu。若尚未创建则按哈希维度与余弦距离初始化。
func ruvIndexLocked() *memory.HNSWIndex {
	if ruvIndex == nil {
		ruvIndex = memory.NewHNSWIndex(embeddings.HashEmbeddingDim, memory.CosineDistance)
	}
	return ruvIndex
}

// ruvllmTools 构造 ruvllm_* MCP 工具并设置 ruvLoraPath。
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

// RegisterRuvLLMTools 向注册表登记神经检索 / HNSW 桥接类 MCP 工具。
func RegisterRuvLLMTools(reg *mcp.ToolRegistry) error {
	for _, t := range ruvllmTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handleRuvLLMStatus 处理 ruvllm_status：返回 HNSW 是否已分配、向量标签数、SONA 模式数量（若 globalState.sona 存在）。
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

// handleRuvLLMHNSWCreate 处理 ruvllm_hnsw_create：reset 为 true 时清空索引与标签并重置内部 ID；否则确保索引已创建。
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

// handleRuvLLMHNSWAdd 处理 ruvllm_hnsw_add：text 必填；可选 id 作为展示标签，否则自动生成；插入哈希向量并返回 internal_id。
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

// handleRuvLLMHNSWRoute 处理 ruvllm_hnsw_route：query 必填；k 为近邻数量（默认 5）；Search 的 ef 固定 64。
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

// handleRuvLLMSONACreate 处理 ruvllm_sona_create：可选 task_id；调用 SONA BeginTrajectory 返回 trajectory_id。
func handleRuvLLMSONACreate(_ context.Context, m map[string]any) mcp.MCPToolResult {
	tid := strArg(m, "task_id")
	if globalState.sona == nil {
		return mcp.MCPToolResult{OK: false, Error: "sona unavailable"}
	}
	tr := globalState.sona.BeginTrajectory(tid)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"trajectory_id": tr}}
}

// handleRuvLLMSONAAdapt 处理 ruvllm_sona_adapt：trajectory_id 必填；verdict 为轨迹结束判定字符串。
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

// microLoraFile 为 microlora.json 的磁盘结构：适配器名到使用次数。
type microLoraFile struct {
	Adapters map[string]int `json:"adapters"`
}

// handleRuvLLMMicroLoraCreate 处理 ruvllm_microlora_create：name 默认 default；在 JSON 中登记适配器计数初值 0。
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

// handleRuvLLMMicroLoraAdapt 处理 ruvllm_microlora_adapt：name 必填；将对应适配器计数自增并写回文件。
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

// handleRuvLLMChatFormat 处理 ruvllm_chat_format：role 默认 user；将单条 content 规范为 OpenAI 风格 messages 数组。
func handleRuvLLMChatFormat(_ context.Context, m map[string]any) mcp.MCPToolResult {
	role := strArg(m, "role")
	if role == "" {
		role = "user"
	}
	content := strArg(m, "content")
	msgs := []map[string]any{{"role": role, "content": content}}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"messages": msgs}}
}

// handleRuvLLMGenerateConfig 处理 ruvllm_generate_config：返回内置 HNSW 维度/度量/ef 与 SONA 默认学习率的 JSON 模板。
func handleRuvLLMGenerateConfig(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	cfg := map[string]any{
		"hnsw": map[string]any{"dim": embeddings.HashEmbeddingDim, "metric": "cosine", "ef": 64},
		"sona": map[string]any{"learning_rate": nlp.DefaultSONAConfig().LearningRate},
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"config": cfg}}
}
