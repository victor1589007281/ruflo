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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

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
	Inline  string `json:"inline,omitempty"`
	Blob    string `json:"blob,omitempty"`
	Size    int    `json:"size"`
	Sampled bool   `json:"sampled,omitempty"` // 正文是否被采样保留 (false=仅存 Size, 正文未落盘)
}

// Options 轨迹采集策略 (design/03 §4.1 成本控制)。
type Options struct {
	// BodySampleRate 正文采样率 [0,1]: 1=全采正文, 0=只采 metadata(正文引用置空)。
	// 元数据(span 结构/tokens/status)恒全采; 正文(prompt/产出)按此率采样降存储。
	BodySampleRate float64
	// InlineLimit 正文内联上限 (字节), 0 用默认 2048。
	InlineLimit int
}

// Store 轨迹存储。
type Store struct {
	ss       statestore.StateStore
	blob     statestore.BlobStore
	writeErr atomic.Int64  // 落盘失败计数 (轨迹缺失不影响交付, 但需可观测)
	sampleN  atomic.Uint64 // MakeRef 调用计数 (确定性采样用, 避免依赖随机数)
	opts     Options
}

// New 构建 Store (默认全采正文)。
func New(ss statestore.StateStore) *Store {
	return NewWithOptions(ss, Options{BodySampleRate: 1})
}

// NewWithOptions 带采样策略构建。
func NewWithOptions(ss statestore.StateStore, opts Options) *Store {
	if opts.BodySampleRate <= 0 {
		opts.BodySampleRate = 0
	}
	if opts.BodySampleRate > 1 {
		opts.BodySampleRate = 1
	}
	if opts.InlineLimit <= 0 {
		opts.InlineLimit = InlineLimit
	}
	return &Store{ss: ss, blob: ss.Blob(), opts: opts}
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
// 正文按 BodySampleRate 确定性采样: 未采样的正文只保留 Size + Sampled=false 标记,
// 不占存储 (元数据仍全采)。
func (s *Store) MakeRef(text string) Ref {
	if text == "" {
		return Ref{}
	}
	if !s.sampleBody() {
		// 未采样: 只留大小, 正文不落盘 (Ref.Inline/Blob 皆空 → Resolve 返回空)
		return Ref{Size: len(text)}
	}
	limit := s.opts.InlineLimit
	if limit <= 0 {
		limit = InlineLimit
	}
	if len(text) <= limit {
		return Ref{Inline: text, Size: len(text), Sampled: true}
	}
	hash, err := s.blob.Put([]byte(text))
	if err != nil {
		s.writeErr.Add(1)
		return Ref{Inline: text[:limit], Size: len(text), Sampled: true}
	}
	return Ref{Blob: hash, Size: len(text), Sampled: true}
}

// sampleBody 确定性采样判定 (不用随机数, 便于测试与 resume 稳定)。
func (s *Store) sampleBody() bool {
	rate := s.opts.BodySampleRate
	if rate >= 1 {
		return true
	}
	if rate <= 0 {
		return false
	}
	n := s.sampleN.Add(1)
	// 每 1/rate 次采一次 (如 rate=0.25 → 每 4 次采 1 次)
	period := uint64(1.0/rate + 0.5)
	if period < 1 {
		period = 1
	}
	return n%period == 0
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

// SweepTraceFiles 清理 mtime 早于 cutoff 的 trace-*.jsonl 文件 (TTL, design/03 §4.1)。
// 仅对 FileStore 的 log 目录有效 (如 <state>/statestore/log); 返回删除的桶数。
// 元数据永久 vs 原始正文 30 天 TTL 的分级策略下, 调用方按周期传入 30 天前的 cutoff。
// 保守实现: 只删整桶 (一个 run 的全部 span), 不做 span 级 TTL。
func SweepTraceFiles(logDir string, cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "trace-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(filepath.Join(logDir, name)) == nil {
				n++
			}
		}
	}
	return n, nil
}

// WriteErrors 返回累计落盘失败数 (可观测)。
func (s *Store) WriteErrors() int64 {
	if s == nil {
		return 0
	}
	return s.writeErr.Load()
}
