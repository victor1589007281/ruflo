package agentdbsemantic

import (
	"context"
	"encoding/json"
	"fmt"
)

// FileMeta 文件元信息 (与 agentDB pkg/file.FileMeta 对齐, serve 端 Go 默认 JSON 字段名)。
type FileMeta struct {
	Path     string `json:"Path"`
	Hash     string `json:"Hash"`
	Size     int64  `json:"Size"`
	Language string `json:"Language"`
}

// FileHit BM25 文件检索命中。
type FileHit struct {
	DocID string  `json:"DocID"`
	Score float64 `json:"Score"`
	Meta  FileMeta `json:"Meta"`
}

// IndexFile 索引一个文件 (POST /v1/files/index), 供 BM25 检索与内容重组读取。
func (c *Client) IndexFile(ctx context.Context, path string, content []byte) (*FileMeta, error) {
	req := map[string]any{"path": path, "content": string(content)}
	resp, _, err := c.doJSON(ctx, "POST", "/v1/files/index", req)
	if err != nil {
		return nil, err
	}
	var meta FileMeta
	if err := json.Unmarshal(resp, &meta); err != nil {
		return nil, fmt.Errorf("agentdbsemantic: 解析 files/index 响应: %w", err)
	}
	return &meta, nil
}

// SearchFiles 按文本检索索引文件 (GET /v1/files/search, BM25)。
func (c *Client) SearchFiles(ctx context.Context, query string, topK int) ([]FileHit, error) {
	if topK <= 0 {
		topK = 10
	}
	path := "/v1/files/search?q=" + pathEscape(query) + "&top_k=" + fmt.Sprint(topK)
	resp, _, err := c.doJSON(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []FileHit `json:"results"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("agentdbsemantic: 解析 files/search 响应: %w", err)
	}
	return out.Results, nil
}

// GetFileContent 读取已索引文件的完整内容 (按内容 hash 重组, GET /v1/files/content)。
func (c *Client) GetFileContent(ctx context.Context, path string) ([]byte, error) {
	resp, _, err := c.doJSON(ctx, "GET", "/v1/files/content?path="+pathEscape(path), nil)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// SymbolRef 符号引用 (定义/引用位置, 来自代码图谱)。
type SymbolRef struct {
	Symbol   string `json:"Symbol"`
	Path     string `json:"Path"`
	Line     int    `json:"Line"`
	Column   int    `json:"Column"`
	Kind     string `json:"Kind"`
	Language string `json:"Language"`
}

// GetSymbols 按符号名检索代码图谱中的定义/引用位置 (GET /v1/files/symbols)。
func (c *Client) GetSymbols(ctx context.Context, name string) ([]SymbolRef, error) {
	resp, _, err := c.doJSON(ctx, "GET", "/v1/files/symbols?name="+pathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Symbols []SymbolRef `json:"symbols"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("agentdbsemantic: 解析 files/symbols 响应: %w", err)
	}
	return out.Symbols, nil
}
