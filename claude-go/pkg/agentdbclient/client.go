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

// do 发送一次 HTTP 请求并读回完整响应体, 返回 (body, status, error)。
//
// 算法解读：
//
//	原理：agentDB serve 是集群唯一的串行化点 (进程内 Store.mu + HTTP 全链路跨进程), 所有数据面
//	      调用最终收敛到这条一次性 HTTP 往返, 服从 first-committer-wins。statestore.KVStore /
//	      AppendLog 的接口签名不带 context, 故这里统一用 context.Background() + Client 级 30s 超时兜底。
//	关键不变量：HTTP 状态码必须**原样**返回给调用方, 由各方法自己用 checkStatus 判定期望码。
//	      在这里把非 2xx 折叠成 error, 会让 CAS 的 409 (并发冲突, 语义上可重试) 与真正的传输故障
//	      混成同一种错误, 上层就无法区分"重试一次就好"和"服务坏了"。
//	代价与边界：io.ReadAll 不设上限, 响应体大小由 serve 端保证; 需要硬上限的 JSON 路径走
//	      distributed.go 的 readAllLimited (16MB)。传输层错误在这里不重试。
//	设计权衡：把重试留给调用方而不是内建自动重试 —— CAS/租约这类原语的重试必须重读旧值 (读-改-写),
//	      在传输层盲目重发同一个 PUT 会覆盖别人的写。
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

// checkStatus 校验状态码是否落在期望集合内; 不在则返回带 what 标签的错误 (便于定位是哪次调用)。
func (c *Client) checkStatus(what string, status int, want ...int) error {
	for _, w := range want {
		if status == w {
			return nil
		}
	}
	return fmt.Errorf("agentdbclient: %s: HTTP %d", what, status)
}

// pathEscape 对 bucket/key 做 URL 路径转义, 使含 `/`、空格等字符的键不会把路径切错段。
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

// Get 读取 key 并反序列化进 out; 返回 false 表示键不存在 (404 不算错误)。
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

// Put 序列化 v 并写入 key (期望 204)。
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

// Delete 删除 key (期望 204); 键不存在也算成功。
func (k kvStore) Delete(key string) error {
	_, status, err := k.c.do("DELETE", "/v1/store/"+pathEscape(k.bucket)+"/"+pathEscape(key), nil, false)
	if err != nil {
		return err
	}
	return k.c.checkStatus("KV.Delete", status, http.StatusNoContent)
}

// Keys 列出 bucket 内全部键 (一次全量拉取, 不做分页)。
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

// Append 把 v 序列化成一行 JSON 追加到日志 (期望 204)。
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

// ReadAll 拉取整个日志并按行回调 fn; fn 返回错误即中止 (不再喂后续行)。
//
// 算法解读：
//
//	原理：日志行是**任意字节** (JSONL 里也可能出现换行/非 UTF-8), 故 wire 上用 base64 逐行
//	      编码, 换行符只作为行分隔符出现 —— 否则"一行"的边界会被行内字节搅乱。
//	关键不变量：解码必须逐行独立进行; 任何一行解码失败都立刻中止并报错, 不做"跳过坏行继续"——
//	      静默丢行会让上层基于残缺日志做出错误判断 (例如把丢失的奖励行读成"样本还没攒够")。
//	代价与边界：一次性拉全量日志到内存, 无分页/无流式; 日志很大时内存与延迟都线性增长。
//	设计权衡：回调式而不是返回切片, 是为了让调用方边解边处理, 不必再复制一份全量切片。
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

// Put 上传内容寻址 Blob, 返回 serve 端算出的哈希 (期望 201)。
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

// Get 按哈希取回 Blob 字节; 不存在返回错误而不是空切片。
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

// Has 用 HEAD 判断 Blob 是否存在; 任何传输错误一律当作"不存在"。
//
// 注意: 传输错误 (网络断、serve 未起) 与"确实不存在"在这里被压成同一个 false, 调用方无法区分 ——
// 需要区分的场景应改用 Get 并检查错误。
func (b blobStore) Has(hash string) bool {
	_, status, err := b.c.do("HEAD", "/v1/blob/"+hash, nil, false)
	if err != nil {
		return false
	}
	return status == http.StatusOK
}
