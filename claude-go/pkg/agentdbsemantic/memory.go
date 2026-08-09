package agentdbsemantic

import (
	"context"
	"encoding/json"
	"fmt"
)

// MemoryType 记忆类型 (与 agentDB memory.Type 对齐)。
type MemoryType string

const (
	MemorySemantic   MemoryType = "semantic"   // 语义记忆
	MemoryProcedural MemoryType = "procedural" // 程序性记忆 (技能/流程)
	MemoryWorking    MemoryType = "working"    // 工作记忆 (会话内)
)

// Memory 一条待写入/已写入 agentDB 的记忆。
type Memory struct {
	ID        string     `json:"id,omitempty"` // 空则 serve 端生成
	Content   string     `json:"content"`
	Embedding []float32  `json:"embedding,omitempty"` // 空则 serve 端嵌入 content
	Type      MemoryType `json:"type,omitempty"`
	ProjectID uint64     `json:"project_id,omitempty"`
	SessionID string     `json:"session_id,omitempty"`
	Tags      []string   `json:"tags,omitempty"`
}

// MemoryHit 记忆检索命中。
type MemoryHit struct {
	ID      string  `json:"id"`
	Content string  `json:"content"`
	Score   float32 `json:"score"`
}

// StoreMemory 写入一条记忆 (POST /v1/memory/store)。返回服务端记忆 ID。
// content 非空且未提供 Embedding 时, serve 端自动嵌入 (M1)。
func (c *Client) StoreMemory(ctx context.Context, m Memory) (string, error) {
	resp, _, err := c.doJSON(ctx, "POST", "/v1/memory/store", m)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return "", fmt.Errorf("agentdbsemantic: 解析 memory/store 响应: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("agentdbsemantic: memory/store 未返回 id")
	}
	return out.ID, nil
}

// SearchMemory 按文本检索记忆 (POST /v1/memory/search, query_text 由 serve 端嵌入)。
// decayLambda > 0 时按时间衰减融合进排序 (R4-3), 冷记忆权重随时间指数下降。
func (c *Client) SearchMemory(ctx context.Context, query string, topK int, decayLambda float64) ([]MemoryHit, error) {
	return c.search(ctx, memorySearchReq{
		QueryText: query,
		TopK:      topK,
		Decay:     decayLambda,
	})
}

// SearchMemoryVec 按向量检索记忆 (query 由调用方提供, 需与写入同一向量空间)。
func (c *Client) SearchMemoryVec(ctx context.Context, query []float32, topK int, decayLambda float64) ([]MemoryHit, error) {
	return c.search(ctx, memorySearchReq{
		Query: query,
		TopK:  topK,
		Decay: decayLambda,
	})
}

type memorySearchReq struct {
	Query     []float32 `json:"query,omitempty"`
	QueryText string    `json:"query_text,omitempty"`
	TopK      int       `json:"top_k"`
	Decay     float64   `json:"decay,omitempty"`
}

func (c *Client) search(ctx context.Context, req memorySearchReq) ([]MemoryHit, error) {
	resp, _, err := c.doJSON(ctx, "POST", "/v1/memory/search", req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []MemoryHit `json:"results"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("agentdbsemantic: 解析 memory/search 响应: %w", err)
	}
	return out.Results, nil
}
