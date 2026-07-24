// Package tracestore 轨迹底座 (design/03 §4.1 E1)。
//
// 目标: 任一 run 可从 Span 完整还原 prompt→response→工具调用→工具结果链
// (修复 design/03 §1.3 "轨迹不可对齐": llm.jsonl 无关联键、transcript 缺
// tool_result、进化轨迹截断)。
//
// 存储经 pkg/statestore 抽象: 单机 file 后端即可; 分布式换后端零改动。
// 成本控制 (design/03 §4.1): 正文短则内联、长则内容寻址进 Blob (天然去重 —
// system prompt/skill 正文重复率极高)。
package tracestore

import (
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// InlineLimit 正文内联上限 (字节)。超过则落 Blob, 只在 Span 存 hash。
const InlineLimit = 2048

// Span 轨迹跨度 (OTel 风格)。
type Span struct {
	TraceID   string         `json:"trace_id"` // = RunID (episode); 会话路径为 session id
	SpanID    string         `json:"span_id"`
	ParentID  string         `json:"parent_id,omitempty"`
	Kind      string         `json:"kind"` // turn | llm_call | tool_call | gate | node
	Name      string         `json:"name"`
	NodeID    string         `json:"node_id,omitempty"`
	TurnID    string         `json:"turn_id,omitempty"`
	InputRef  Ref            `json:"input_ref,omitempty"`
	OutputRef Ref            `json:"output_ref,omitempty"`
	Attrs     map[string]any `json:"attrs,omitempty"`
	TS        int64          `json:"ts"` // unix milli
	DurMS     int64          `json:"dur_ms"`
}

// Ref 正文引用: 短文本内联, 长文本内容寻址进 Blob。
type Ref struct {
	Inline string `json:"inline,omitempty"`
	Blob   string `json:"blob,omitempty"`
	Size   int    `json:"size"`
}

// Store 轨迹存储。
type Store struct {
	ss       statestore.StateStore
	blob     statestore.BlobStore
	writeErr atomic.Int64 // 落盘失败计数 (轨迹缺失不影响交付, 但需可观测)
}

// New 构建 Store。
func New(ss statestore.StateStore) *Store {
	return &Store{ss: ss, blob: ss.Blob()}
}

// bucketName 把 TraceID 消毒为合法 bucket 名 (statestore 只允许 [a-zA-Z0-9._-])。
// 空 TraceID 归 orphan 桶。
func bucketName(traceID string) string {
	if strings.TrimSpace(traceID) == "" {
		return "trace-orphan"
	}
	var b strings.Builder
	b.WriteString("trace-")
	for _, r := range traceID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_') // 非法字符替换, 保持可读
		}
	}
	return b.String()
}

// MakeRef 内联/Blob 判定 (design/03 §4.1 成本控制)。
func (s *Store) MakeRef(text string) Ref {
	if text == "" {
		return Ref{}
	}
	if len(text) <= InlineLimit {
		return Ref{Inline: text, Size: len(text)}
	}
	hash, err := s.blob.Put([]byte(text))
	if err != nil {
		// Blob 失败降级内联截断 (保留可观测的前缀, 不丢整条 Span)
		s.writeErr.Add(1)
		return Ref{Inline: text[:InlineLimit], Size: len(text)}
	}
	return Ref{Blob: hash, Size: len(text)}
}

// Resolve 反解正文。
func (s *Store) Resolve(r Ref) (string, error) {
	if r.Blob != "" {
		data, err := s.blob.Get(r.Blob)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return r.Inline, nil
}

// Write 落 Span 到 Log bucket。失败静默 (轨迹缺失不影响交付), 但计数可观测。
func (s *Store) Write(span Span) {
	if s == nil {
		return
	}
	if err := s.ss.Log(bucketName(span.TraceID)).Append(span); err != nil {
		s.writeErr.Add(1)
	}
}

// ReadRun 读回一个 run 的全部 Span (按写入顺序)。
func (s *Store) ReadRun(traceID string) ([]Span, error) {
	var spans []Span
	err := s.ss.Log(bucketName(traceID)).ReadAll(func(line []byte) error {
		var sp Span
		if err := json.Unmarshal(line, &sp); err != nil {
			return nil // 容忍单行损坏, 跳过
		}
		spans = append(spans, sp)
		return nil
	})
	return spans, err
}

// WriteErrors 返回累计落盘失败数 (可观测)。
func (s *Store) WriteErrors() int64 {
	if s == nil {
		return 0
	}
	return s.writeErr.Load()
}
