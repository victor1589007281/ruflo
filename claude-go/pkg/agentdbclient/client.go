// Package agentdbclient implements statestore.StateStore over the AgentDB serve
// HTTP API (规划 14.1.5 分布式横切).
//
// 与 pkg/statestore/agentdbstore (嵌入式, 单机) 对应: 本包是集群模式的数据面
// 客户端 —— 多个 worker 节点共享同一个 agentDB serve 数据面, 队列 / 注册表 /
// 任务认领 / 租约跨进程成立。serve 进程是串行化点 (Store.mu 进程内 + HTTP 全链路
// 跨进程), 服从 first-committer-wins。
//
// 接口约束: statestore.KVStore / AppendLog 方法不带 context, 故每次调用内部用
// context.Background() + Client 级超时; 需要 context 的原子原语 (CAS / Tx / 租约)
// 单独暴露。
package agentdbclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrCASConflict 远程 CAS 旧值不匹配 (HTTP 409)。调用方按"并发冲突, 重试"处理。
var ErrCASConflict = errors.New("agentdbclient: CAS conflict")

// Client 是 agentDB serve 的 HTTP 数据面客户端。
type Client struct {
	baseURL string
	httpc   *http.Client
}

// New 创建指向 baseURL 的客户端 (baseURL 如 http://127.0.0.1:18080, 尾部斜杠可省)。
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpc:   &http.Client{Timeout: 30 * time.Second},
	}
}

// ---- 内部请求工具 ----

func (c *Client) do(method, path string, body io.Reader, acceptJSON bool) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, c.baseURL+path, body)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if acceptJSON {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("agentdbclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("agentdbclient: read body: %w", err)
	}
	return data, resp.StatusCode, nil
}

func (c *Client) checkStatus(what string, status int, want ...int) error {
	for _, w := range want {
		if status == w {
			return nil
		}
	}
	return fmt.Errorf("agentdbclient: %s: HTTP %d", what, status)
}

func pathEscape(s string) string { return url.PathEscape(s) }

// ---- StateStore 三入口 ----

// KV 返回 bucket 的键值存储 (HTTP /v1/store/{bucket})。
func (c *Client) KV(bucket string) kvStore {
	return kvStore{c: c, bucket: bucket}
}

// Log 返回 bucket 的 JSONL 追加日志 (HTTP /v1/log/{bucket})。
func (c *Client) Log(bucket string) logStore {
	return logStore{c: c, bucket: bucket}
}

// Blob 返回内容寻址 Blob 存储 (HTTP /v1/blob)。
func (c *Client) Blob() blobStore {
	return blobStore{c: c}
}

// ---- KV ----

type kvStore struct {
	c      *Client
	bucket string
}

func (k kvStore) Get(key string, out any) (bool, error) {
	data, status, err := k.c.do("GET", "/v1/store/"+pathEscape(k.bucket)+"/"+pathEscape(key), nil, true)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if err := k.c.checkStatus("KV.Get", status, http.StatusOK); err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, fmt.Errorf("agentdbclient: 反序列化 key %q 失败: %w", key, err)
	}
	return true, nil
}

func (k kvStore) Put(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("agentdbclient: 序列化 key %q 失败: %w", key, err)
	}
	_, status, err := k.c.do("PUT", "/v1/store/"+pathEscape(k.bucket)+"/"+pathEscape(key), bytes.NewReader(raw), false)
	if err != nil {
		return err
	}
	return k.c.checkStatus("KV.Put", status, http.StatusNoContent)
}

func (k kvStore) Delete(key string) error {
	_, status, err := k.c.do("DELETE", "/v1/store/"+pathEscape(k.bucket)+"/"+pathEscape(key), nil, false)
	if err != nil {
		return err
	}
	return k.c.checkStatus("KV.Delete", status, http.StatusNoContent)
}

func (k kvStore) Keys() ([]string, error) {
	data, status, err := k.c.do("GET", "/v1/store/"+pathEscape(k.bucket)+"/keys", nil, true)
	if err != nil {
		return nil, err
	}
	if err := k.c.checkStatus("KV.Keys", status, http.StatusOK); err != nil {
		return nil, err
	}
	var resp struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("agentdbclient: 解析 keys: %w", err)
	}
	return resp.Keys, nil
}

// ---- Log ----

type logStore struct {
	c      *Client
	bucket string
}

func (l logStore) Append(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("agentdbclient: 序列化日志记录失败: %w", err)
	}
	_, status, err := l.c.do("POST", "/v1/log/"+pathEscape(l.bucket)+"/append", bytes.NewReader(line), false)
	if err != nil {
		return err
	}
	return l.c.checkStatus("Log.Append", status, http.StatusNoContent)
}

func (l logStore) ReadAll(fn func(line []byte) error) error {
	data, status, err := l.c.do("GET", "/v1/log/"+pathEscape(l.bucket), nil, true)
	if err != nil {
		return err
	}
	if err := l.c.checkStatus("Log.ReadAll", status, http.StatusOK); err != nil {
		return err
	}
	var resp struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("agentdbclient: 解析日志: %w", err)
	}
	for _, l64 := range resp.Lines {
		raw, err := base64.StdEncoding.DecodeString(l64)
		if err != nil {
			return fmt.Errorf("agentdbclient: 解码日志行: %w", err)
		}
		if err := fn(raw); err != nil {
			return err
		}
	}
	return nil
}

// ---- Blob ----

type blobStore struct {
	c *Client
}

func (b blobStore) Put(data []byte) (string, error) {
	resp, status, err := b.c.do("PUT", "/v1/blob", bytes.NewReader(data), true)
	if err != nil {
		return "", err
	}
	if err := b.c.checkStatus("Blob.Put", status, http.StatusCreated); err != nil {
		return "", err
	}
	var out struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return "", fmt.Errorf("agentdbclient: 解析 blob put: %w", err)
	}
	return out.Hash, nil
}

func (b blobStore) Get(hash string) ([]byte, error) {
	data, status, err := b.c.do("GET", "/v1/blob/"+hash, nil, false)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("agentdbclient: blob %s 不存在", hash)
	}
	if err := b.c.checkStatus("Blob.Get", status, http.StatusOK); err != nil {
		return nil, err
	}
	return data, nil
}

func (b blobStore) Has(hash string) bool {
	_, status, err := b.c.do("HEAD", "/v1/blob/"+hash, nil, false)
	if err != nil {
		return false
	}
	return status == http.StatusOK
}
