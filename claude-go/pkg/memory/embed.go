// embed.go —— 记忆检索的可选向量化后端 (手册 7.0.6 缺口: 纯 BM25 → BM25+embedding 混合)。
//
// 设计约束 (手册纪律): **默认关**——它是可选后端而非替代; 不开时行为与现状完全一致。
// 开启: CLAUDE_GO_MEMORY_EMBEDDER=ollama:bge-m3 (+ CLAUDE_GO_OLLAMA_BASE 覆盖端点,
// 默认 http://127.0.0.1:11434)。所有 embedding 调用都是 best-effort:
// 失败静默降级为纯 BM25, 绝不阻断记忆读写 (本地 Ollama 未起时零代价)。
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Embedder 文本向量化端口。
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// EmbedderFromEnv 按 env 装配 Embedder; 未配置返回 nil (纯 BM25, 现状)。
//
//	CLAUDE_GO_MEMORY_EMBEDDER: 形如 "ollama:bge-m3" (仅此一档实现)
//	CLAUDE_GO_OLLAMA_BASE:     Ollama 端点, 缺省 http://127.0.0.1:11434
func EmbedderFromEnv(getenv func(string) string) Embedder {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	spec := strings.TrimSpace(getenv("CLAUDE_GO_MEMORY_EMBEDDER"))
	if spec == "" {
		return nil
	}
	provider, model, _ := strings.Cut(spec, ":")
	if provider != "ollama" || model == "" {
		return nil
	}
	base := strings.TrimSpace(getenv("CLAUDE_GO_OLLAMA_BASE"))
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	return &OllamaEmbedder{BaseURL: strings.TrimRight(base, "/"), Model: model}
}

// OllamaEmbedder 经 Ollama /api/embeddings 的本地向量化 (bge-m3 等)。
type OllamaEmbedder struct {
	BaseURL string
	Model   string
}

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed 单文本向量化 (10s 超时, 输入截 4000 字符防超长阻塞)。
func (e *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if e == nil || e.BaseURL == "" || e.Model == "" {
		return nil, fmt.Errorf("embedder 未配置")
	}
	if len(text) > 4000 {
		text = text[:4000]
	}
	body, _ := json.Marshal(embedRequest{Model: e.Model, Input: text})
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, e.BaseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings HTTP %d", resp.StatusCode)
	}
	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) == 0 || len(out.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embeddings 空响应")
	}
	return out.Embeddings[0], nil
}

// Cosine 余弦相似度 (零向量/维度不等返回 0)。
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
