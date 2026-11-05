// A3 VersionedKV as-of 客户端封装。
//
// 对应 agentDB pkg/server/versioned_kv_handlers.go 的 /v1/vkv/{key} 端点。
// 与学习状态快照结合使用时, 通过 AsOf 查询能回溯任意时间点的 LearningState,
// 支撑"当时基于什么信息做了什么预测"的审计与反思。

package agentdbclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// VersionedKV 返回 key 的版本链存储 (HTTP /v1/vkv/{key})。
// 与 KV 的主要差异: Put 自动追加版本, Get 支持 AsOf 时间旅行。
func (c *Client) VersionedKV(key string) VersionedKVStore {
	return VersionedKVStore{c: c, key: key}
}

// VersionedKVStore /v1/vkv/{key} 句柄。
type VersionedKVStore struct {
	c   *Client
	key string
}

// VKVEntry 单条版本链记录 (与服务端 vkEntry 对齐, value 经 encoding/json 自动 base64)。
type VKVEntry struct {
	Value    []byte `json:"value"`        // 该版本的用户值
	UnixNano int64  `json:"ts_unix_nano"` // 写入时间戳
	Version  int    `json:"version"`      // 单调递增版本号
}

// Put 追加一个版本 (服务端同时更新主键 + 链)。返回分配的 version。
// 链写失败时仍返回 0, nil (主键成功, 链失败仅 warn, 见服务端实现)。
func (v VersionedKVStore) Put(value []byte) (int, error) {
	data, status, err := v.c.do("PUT", "/v1/vkv/"+pathEscape(v.key), bytes.NewReader(value), false)
	if err != nil {
		return 0, err
	}
	if status != http.StatusNoContent {
		return 0, fmt.Errorf("agentdbclient: vkv put: HTTP %d: %s", status, truncate(data, 200))
	}
	// X-Version 头由服务端在链写成功时设置。
	if ver := lastResponseVersion(v.c, data); ver > 0 {
		return ver, nil
	}
	return 0, nil
}

// Get 读最新版本 (asOfUnixNano=0) 或 as-of 版本 (asOfUnixNano>0)。
// 返回 (nil, nil) 表示未命中 (404)。
func (v VersionedKVStore) Get(asOfUnixNano int64) (*VKVEntry, error) {
	path := "/v1/vkv/" + pathEscape(v.key)
	if asOfUnixNano > 0 {
		path += "?asOf=" + strconv.FormatInt(asOfUnixNano, 10)
	}
	data, status, err := v.c.do("GET", path, nil, true)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("agentdbclient: vkv get: HTTP %d: %s", status, truncate(data, 200))
	}
	var entry VKVEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("agentdbclient: vkv decode: %w", err)
	}
	return &entry, nil
}

// History 列出版本链元数据 (时间倒序)。
func (v VersionedKVStore) History() ([]VKVEntry, error) {
	data, status, err := v.c.do("GET", "/v1/vkv/"+pathEscape(v.key)+"?history=1", nil, true)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("agentdbclient: vkv history: HTTP %d: %s", status, truncate(data, 200))
	}
	var out struct {
		History []VKVEntry `json:"history"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("agentdbclient: vkv history decode: %w", err)
	}
	return out.History, nil
}

// lastResponseVersion 解析最近一次响应的 X-Version 头。do() 当前不返回 header,
// 这里仅作占位 — 真正的版本号从 History()[0].Version 读更可靠。
func lastResponseVersion(_ *Client, _ []byte) int { return 0 }

// truncate 截断字节切片用于错误消息, 防爆破日志。
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
