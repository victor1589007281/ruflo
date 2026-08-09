package agentdbsemantic

import (
	"context"
	"encoding/json"
	"fmt"
)

// RetrieveHit 一次检索命中的统一表示 (source + content + score)。
type RetrieveHit struct {
	Source  Source  `json:"source"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// RetrieveResult 多源检索聚合结果。
type RetrieveResult struct {
	Items        []RetrieveHit    `json:"items"`
	SourceCounts map[Source]int   `json:"source_counts"`
}

// RetrieveQuery 跨源检索 (POST /v1/retrieve/query)。sources 为空时默认
// 记忆+文件 (retrieve.Memories + retrieve.Files)。返回按 RRF 融合排序的结果。
func (c *Client) RetrieveQuery(ctx context.Context, query string, sources []Source, topK int) (*RetrieveResult, error) {
	req := map[string]any{"query": query, "top_k": topK}
	if len(sources) > 0 {
		names := make([]string, len(sources))
		for i, s := range sources {
			names[i] = string(s)
		}
		req["sources"] = names
	}
	resp, _, err := c.doJSON(ctx, "POST", "/v1/retrieve/query", req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []RetrieveHit `json:"items"`
		Count map[string]int `json:"source_counts"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("agentdbsemantic: 解析 retrieve/query 响应: %w", err)
	}
	res := &RetrieveResult{Items: out.Items}
	if out.Count != nil {
		res.SourceCounts = make(map[Source]int, len(out.Count))
		for k, n := range out.Count {
			res.SourceCounts[Source(k)] = n
		}
	}
	return res, nil
}

// AssembledContext 上下文组装结果 (RRF + token 预算裁剪)。
type AssembledContext struct {
	Body      string // 按相关性组装、受 token_budget 约束的注入文本
	Truncated bool   // 是否因预算裁剪掉了尾部
	Items     int    // 实际进入 Body 的条目数
}

// AssembleContext 对已检索条目做 token 预算感知的上下文组装
// (POST /v1/retrieve/context)。query 用于标题/引导, reserve 为给后续任务文本
// 预留的 token 数。
func (c *Client) AssembleContext(ctx context.Context, query string, tokenBudget, reserve int, items []RetrieveHit) (*AssembledContext, error) {
	in := make([]map[string]any, len(items))
	for i, it := range items {
		in[i] = map[string]any{"source": string(it.Source), "content": it.Content, "score": it.Score}
	}
	req := map[string]any{
		"query":        query,
		"token_budget": tokenBudget,
		"reserve":      reserve,
		"items":        in,
	}
	resp, _, err := c.doJSON(ctx, "POST", "/v1/retrieve/context", req)
	if err != nil {
		return nil, err
	}
	var out AssembledContext
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("agentdbsemantic: 解析 retrieve/context 响应: %w", err)
	}
	return &out, nil
}
