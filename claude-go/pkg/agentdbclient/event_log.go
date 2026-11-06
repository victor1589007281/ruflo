// A6 Event Log 客户端封装。
//
// 对应 agentDB pkg/server/event_log_handlers.go 的 /v1/events/{stream} 端点。
// 事件流是 append-only 的, 序号由服务端在 kv.Tx 内分配 (单调不减, 允许跳号),
// 消费方用 since 游标增量读, 靠 seq 去重实现 at-least-once。

package agentdbclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// EventLog 返回 stream 事件流句柄 (HTTP /v1/events/{stream})。
// stream 名规则与服务端一致: 字母/数字开头, 可含中划线与下划线, ≤64 字符。
func (c *Client) EventLog(stream string) eventLogStore {
	return eventLogStore{c: c, stream: stream}
}

// eventLogStore /v1/events/{stream} 句柄。
type eventLogStore struct {
	c      *Client
	stream string
}

// EventLogEntry 单条事件记录 (与服务端 evEntry 对齐, payload 经 encoding/json 自动 base64)。
type EventLogEntry struct {
	Seq      int64  `json:"seq"`          // 流内单调序号
	UnixNano int64  `json:"ts_unix_nano"` // 写入时间戳
	Payload  []byte `json:"payload"`      // 调用方原始字节
}

// Append 追加一条事件, 返回服务端分配的 seq。
// 201 之外的响应 (含 503 KV backend not configured) 返回错误。
func (s eventLogStore) Append(payload []byte) (int64, error) {
	data, status, err := s.c.do("POST", "/v1/events/"+pathEscape(s.stream), bytes.NewReader(payload), true)
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated {
		return 0, fmt.Errorf("agentdbclient: event append: HTTP %d: %s", status, truncate(data, 200))
	}
	var out struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, fmt.Errorf("agentdbclient: event append decode: %w", err)
	}
	return out.Seq, nil
}

// AppendBatch 批量追加 (单事务连续序号, 1..1024 条)。
// 返回首条与末条 seq。
func (s eventLogStore) AppendBatch(payloads [][]byte) (first, last int64, err error) {
	raw, err := json.Marshal(payloads)
	if err != nil {
		return 0, 0, err
	}
	data, status, err := s.c.do("POST", "/v1/events/"+pathEscape(s.stream)+"/batch", bytes.NewReader(raw), true)
	if err != nil {
		return 0, 0, err
	}
	if status != http.StatusCreated {
		return 0, 0, fmt.Errorf("agentdbclient: event batch: HTTP %d: %s", status, truncate(data, 200))
	}
	var out struct {
		FirstSeq int64 `json:"first_seq"`
		LastSeq  int64 `json:"last_seq"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, 0, fmt.Errorf("agentdbclient: event batch decode: %w", err)
	}
	return out.FirstSeq, out.LastSeq, nil
}

// Read 游标式增量读: 返回 seq > since 的事件 (升序) 与 next_since 续读游标。
// limit<=0 用服务端默认 (100); limit 超过服务端上限 (1000) 由服务端截断,
// 调用方用返回的 next_since 续读即可, 事件总量大时循环读直到返回空集。
func (s eventLogStore) Read(since, limit int64) ([]EventLogEntry, int64, error) {
	path := "/v1/events/" + pathEscape(s.stream) + "?since=" + strconv.FormatInt(since, 10)
	if limit > 0 {
		path += "&limit=" + strconv.FormatInt(limit, 10)
	}
	data, status, err := s.c.do("GET", path, nil, true)
	if err != nil {
		return nil, 0, err
	}
	if status != http.StatusOK {
		return nil, 0, fmt.Errorf("agentdbclient: event read: HTTP %d: %s", status, truncate(data, 200))
	}
	var out struct {
		Events    []EventLogEntry `json:"events"`
		NextSince int64           `json:"next_since"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, 0, fmt.Errorf("agentdbclient: event read decode: %w", err)
	}
	return out.Events, out.NextSince, nil
}
