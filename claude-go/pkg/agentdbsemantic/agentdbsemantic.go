// Package agentdbsemantic implements Layer 2 (语义增强面) of 规划 14.1.4.3:
// claude-go 对接 agentDB 语义引擎的 HTTP 客户端 —— Memory (检索/衰减/写入)、
// Retrieve (RRF 上下文组装)、File / Vector / Graph 检索。
//
// 与 pkg/agentdbclient (层1+分布式数据面: KV/Log/Blob/CAS/租约) 互补, 本包只访问
// agentDB serve 的语义端点 (Memory / Retrieve / File)。两者指向同一个 agentDB
// serve 进程: 数据面管"状态", 语义面管"理解"。Embedding 由 serve 端执行
// (POST /v1/memory/store|search 的 query_text/content 走服务端嵌入), 客户端
// 无需自己算向量。
package agentdbsemantic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 是 agentDB 语义引擎的 HTTP 客户端。
type Client struct {
	baseURL string
	httpc   *http.Client
}

// New 创建指向 baseURL 的语义客户端 (如 http://127.0.0.1:18080)。
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpc:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Source 检索源 (与 agentDB retrieve.Source 对齐)。
type Source string

const (
	SourceMemory Source = "memory" // 记忆
	SourceFile   Source = "file"   // 文件 BM25
	SourceGraph  Source = "graph"  // 代码图谱
	SourceSQL    Source = "sql"    // SQL 检索
)

// ---- 内部请求工具 ----

// doJSON 发送 JSON 请求体并解码 JSON 响应; status 非 2xx 返回错误(带响应体摘要)。
func (c *Client) doJSON(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("agentdbsemantic: 序列化请求: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("agentdbsemantic: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("agentdbsemantic: 读响应: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("agentdbsemantic: %s %s: HTTP %d: %s",
			method, path, resp.StatusCode, truncate(string(data), 300))
	}
	return data, resp.StatusCode, nil
}

func pathEscape(s string) string { return url.PathEscape(s) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
