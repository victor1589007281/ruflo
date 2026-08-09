package agentdbclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// readAllLimited 读取响应体 (上限 16MB, 防恶意/异常大响应)。
func readAllLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("agentdbclient: read body: %w", err)
	}
	return data, nil
}

// TxOp 事务操作 (跨 bucket 原子批写)。
type TxOp struct {
	Bucket string
	Key    string
	Delete bool
	Value  []byte
}

// CAS 对 bucket/key 执行原子比较并交换 (远程, 经 serve 串行化)。
// oldVal 为 nil 表示"键必须不存在"; 旧值不匹配返回 ErrCASConflict。
func (c *Client) CAS(ctx context.Context, bucket, key string, oldVal, newVal []byte) error {
	req := map[string]any{"key": key, "new_value": base64.StdEncoding.EncodeToString(newVal)}
	if oldVal != nil {
		req["old_value"] = base64.StdEncoding.EncodeToString(oldVal)
	}
	body, _ := json.Marshal(req)
	path := "/v1/store/" + pathEscape(bucket) + "/cas"
	resp, status, err := c.doJSON(ctx, "POST", path, body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return ErrCASConflict
	default:
		return fmt.Errorf("agentdbclient: CAS %s: HTTP %d: %s", path, status, string(resp))
	}
}

// Tx 执行跨 bucket 原子批写 (失败整体回滚)。
func (c *Client) Tx(ctx context.Context, ops []TxOp) error {
	type opJSON struct {
		Bucket string `json:"bucket"`
		Key    string `json:"key"`
		Delete bool   `json:"delete,omitempty"`
		Value  string `json:"value,omitempty"`
	}
	req := make([]opJSON, 0, len(ops))
	for _, o := range ops {
		oj := opJSON{Bucket: o.Bucket, Key: o.Key, Delete: o.Delete}
		if !o.Delete {
			oj.Value = base64.StdEncoding.EncodeToString(o.Value)
		}
		req = append(req, oj)
	}
	body, _ := json.Marshal(map[string]any{"ops": req})
	resp, status, err := c.doJSON(ctx, "POST", "/v1/store/tx", body)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("agentdbclient: Tx: HTTP %d: %s", status, string(resp))
	}
	return nil
}

// Lease 描述一个租约 (来自 serve)。
type Lease struct {
	Name      string        `json:"name"`
	Holder    string        `json:"holder"`
	ExpiresAt time.Time     `json:"expires_at"`
	TTL       time.Duration `json:"ttl"`
}

// Acquire 获取/续期名为 name 的租约; 已被其他 holder 持有时返回错误。
func (c *Client) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (*Lease, error) {
	return c.leaseOp(ctx, "acquire", name, holder, ttl)
}

// Renew 续期租约 (须为当前 holder)。
func (c *Client) Renew(ctx context.Context, name, holder string, ttl time.Duration) (*Lease, error) {
	return c.leaseOp(ctx, "renew", name, holder, ttl)
}

// Release 释放租约 (须为当前 holder)。
func (c *Client) Release(ctx context.Context, name, holder string) error {
	_, status, err := c.leaseRaw(ctx, "release", name, holder, 0)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("agentdbclient: lease release: HTTP %d", status)
	}
	return nil
}

// GetLease 读取租约状态 (不存在返回 nil)。
func (c *Client) GetLease(ctx context.Context, name string) (*Lease, error) {
	l, status, err := c.leaseRaw(ctx, "get", name, "", 0)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound || (l == nil && status == http.StatusOK) {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("agentdbclient: lease get: HTTP %d", status)
	}
	return l, nil
}

func (c *Client) leaseOp(ctx context.Context, op, name, holder string, ttl time.Duration) (*Lease, error) {
	l, status, err := c.leaseRaw(ctx, op, name, holder, ttl)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("agentdbclient: lease %s: HTTP %d", op, status)
	}
	return l, nil
}

func (c *Client) leaseRaw(ctx context.Context, op, name, holder string, ttl time.Duration) (*Lease, int, error) {
	req := map[string]any{"op": op, "name": name}
	if holder != "" {
		req["holder"] = holder
	}
	if ttl > 0 {
		if ttl < time.Second {
			req["ttl_ms"] = ttl.Milliseconds()
		} else {
			req["ttl_seconds"] = int(ttl.Seconds())
		}
	}
	body, _ := json.Marshal(req)
	resp, status, err := c.doJSON(ctx, "POST", "/v1/lease", body)
	if err != nil {
		return nil, status, err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return nil, status, nil
	}
	if op == "release" {
		return nil, status, nil
	}
	var out struct {
		Lease *Lease `json:"lease"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, status, fmt.Errorf("agentdbclient: 解析 lease: %w", err)
	}
	return out.Lease, status, nil
}

// doJSON 发送带 Accept: application/json 的请求。
func (c *Client) doJSON(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("agentdbclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := readAllLimited(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}
