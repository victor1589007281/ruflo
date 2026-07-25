// statestore_transcript.go — 会话历史"整份快照"持久化 (design/02 §1.4 缺陷 + §七 R2 验收)。
//
// 背景: 飞书路径的多轮历史此前只活在 QueryEngine.Messages 里 (纯内存), 进程重启
// 即丢且用户无提示 —— 比"不读回"更严重的是**根本没写**: pkg/feishu/createSession
// 从未给 eng.SessionStore 赋值, 全仓唯一接线处是 CLI。
//
// # 为什么用 KV 整份快照, 而不是 design/02 §3.4.1 表里写的 Log transcript/<sid> 追加日志
//
//  1. 自截断: 快照存的是每轮末的 e.Messages, 它已经过 AutoCompact / MicroCompact /
//     BudgetDegrade (engine.queryLoop 的 PhasePreCompact); 追加日志读回的却是未压缩的
//     全量历史, 会随会话无限膨胀, 而且读回后下一轮立刻要为超限付一次压缩风暴。
//  2. /clear 无法表达: statestore.AppendLog 只有 Append/ReadAll 两个方法
//     (statestore.go:34-42), 没有任何删除/截断原语; KV 有 Delete —— 用户显式
//     "忘记"这件事只有 KV 表达得出来。
//  3. 顺带修老缺陷: 旧的 JSONL transcript (storage.go) 只落 user 文本与 assistant
//     消息, 承载 tool_result 的那些 user 消息从不落盘, 读回的链条是残缺的;
//     快照存的就是引擎内存里那份完整链条, 天然含 tool_result。
//
// 代价 (明确记下来): 每轮重写一份完整历史, 写放大高于 append。故有两条约束 ——
// 一 chat 一 bucket (见 snapshotBucketPrefix) + token 上限裁剪 (见 TrimToTokenBudget)。
package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	// snapshotBucketPrefix 会话快照 bucket 名前缀; 每个会话独占一个 bucket。
	//
	// 为什么不共用一个 bucket: FileStore 的 KV 是"整桶单文件重写" ——
	// Put = 读整桶 JSON → 改一个 key → 整文件原子写 (filestore.go:184-197)。
	// maxSessions 默认 100, 若全部 chat 挤在同一个桶里, 每轮都要把 100 份历史
	// 重新序列化并整文件落盘, 还要共抢同一把 bucket 锁。一 chat 一桶后,
	// 跨 chat 天然隔离 (同 chat 的串行由飞书侧 Session.processing 保证)。
	snapshotBucketPrefix = "transcript-"

	// snapshotKey bucket 内的固定单 key (一个桶只存一份快照)。
	snapshotKey = "messages"

	// snapshotFormatVersion 快照格式版本。读回时版本不匹配就整份丢弃 (宁缺勿错:
	// 半懂的历史喂给模型比空历史更危险)。
	snapshotFormatVersion = 1

	// DefaultSnapshotTokenLimit 快照保留的 token 上限 (估算值, chars/4)。
	//
	// 为什么要设上限: 恢复出来的 Messages 下一轮会自动过 PhasePreCompact, 若超
	// AutoCompact 阈值 (compact.TokenThreshold=0.8 × 上下文窗口, 默认 200K → 160K)
	// 会立刻触发一次真 LLM 压缩 —— 重启后的第一条消息就要付延迟 + token 尖峰,
	// 且 PreCompact 会把事实再蒸馏一遍 (可能与重启前重复)。80K 压在阈值之下,
	// 给 system prompt / 工具 schema / 本轮新消息 / 估算偏差 (中文 chars/4 会低估
	// 约 25%) 都留足余量, 恢复后可直接开聊。
	DefaultSnapshotTokenLimit = 80000
)

// StateStoreTranscript 单会话的历史快照 (一个 chatID → 一个 KV bucket 的一个 key)。
//
// 并发: 同一 chat 的写天然串行 (飞书侧 Session.processing + 单条 pending 队列),
// 跨 chat 靠 per-chat bucket 隔离; 桶内互斥则依赖"进程内共享同一个 FileStore 实例"
// —— FileStore 的锁表挂在实例上 (filestore.go:29-32), 是 per-instance 而非 per-path。
type StateStoreTranscript struct {
	kv         statestore.KVStore
	blob       statestore.BlobStore
	chatID     string
	tokenLimit int

	// 失败计数: 快照缺失不影响交付 (fail-open), 但必须可观测。
	writeErr atomic.Int64
	readErr  atomic.Int64
}

// NewStateStoreTranscript 创建会话快照句柄; ss 为 nil 时返回 nil (调用方全部方法可空安全)。
func NewStateStoreTranscript(ss statestore.StateStore, chatID string) *StateStoreTranscript {
	if ss == nil {
		return nil
	}
	return &StateStoreTranscript{
		kv:         ss.KV(TranscriptBucket(chatID)),
		blob:       ss.Blob(),
		chatID:     chatID,
		tokenLimit: DefaultSnapshotTokenLimit,
	}
}

// TranscriptBucket 由 chatID 推出 bucket 名。
//
// 必须哈希: statestore 的 validateBucket 只允许 [a-zA-Z0-9._-] (filestore.go:47-62),
// 明确禁止 '/'; 飞书 chat_id (oc_xxx / ou_xxx) 多数合法, 但群/用户 id 的变体与将来
// 接入的外部 id 都不保证。而 tracestore 那种"非法字符替换成 _"是有损映射 ——
// 两个不同的 id 会撞进同一个桶, 历史互相污染。sha256 前缀不需要可逆, 只要单射。
func TranscriptBucket(chatID string) string {
	return snapshotBucketPrefix + hashDir(chatID)
}

// SetTokenLimit 覆盖 token 上限 (<=0 表示不裁剪)。
func (t *StateStoreTranscript) SetTokenLimit(n int) {
	if t == nil {
		return
	}
	t.tokenLimit = n
}

// WriteErrors 返回累计写失败数 (含 blob 外置失败)。
func (t *StateStoreTranscript) WriteErrors() int64 {
	if t == nil {
		return 0
	}
	return t.writeErr.Load()
}

// ReadErrors 返回累计读失败数。
func (t *StateStoreTranscript) ReadErrors() int64 {
	if t == nil {
		return 0
	}
	return t.readErr.Load()
}

// transcriptSnapshot 落盘载荷。
type transcriptSnapshot struct {
	Version   int             `json:"version"`
	ChatID    string          `json:"chat_id"`
	UpdatedAt time.Time       `json:"updated_at"`
	Messages  []types.Message `json:"messages"`

	// MediaBlobs 媒体正文外置引用: key = "<消息下标>/<内容块下标>", value = blob sha256。
	//
	// 为什么用侧表而不是给 ContentBlock 加字段: pkg/types 是全进程共享的 API 契约,
	// 不该为某一种持久化后端加字段; 侧表既让 MediaType 等元信息留在原位 (Source 结构
	// 不变), 又把 base64 正文挪出 KV 那个每轮重写的单文件。
	MediaBlobs map[string]string `json:"media_blobs,omitempty"`
}

// Snapshot 落一份完整历史 (每轮一次)。fail-open: 返回错误供调用方计数/日志, 但
// 调用方绝不能因此阻塞或失败掉用户回复。
func (t *StateStoreTranscript) Snapshot(msgs []types.Message) error {
	if t == nil || t.kv == nil {
		return nil
	}
	trimmed := TrimToTokenBudget(msgs, t.tokenLimit)
	snap := transcriptSnapshot{
		Version:   snapshotFormatVersion,
		ChatID:    t.chatID,
		UpdatedAt: time.Now().UTC(),
	}
	snap.Messages, snap.MediaBlobs = t.externalizeMedia(trimmed)
	if err := t.kv.Put(snapshotKey, snap); err != nil {
		t.writeErr.Add(1)
		return fmt.Errorf("session: 写会话快照失败 (chat=%s): %w", t.chatID, err)
	}
	return nil
}

// Load 读回历史; 无快照时返回 (nil, nil)。
func (t *StateStoreTranscript) Load() ([]types.Message, error) {
	if t == nil || t.kv == nil {
		return nil, nil
	}
	var snap transcriptSnapshot
	ok, err := t.kv.Get(snapshotKey, &snap)
	if err != nil {
		t.readErr.Add(1)
		return nil, fmt.Errorf("session: 读会话快照失败 (chat=%s): %w", t.chatID, err)
	}
	if !ok {
		return nil, nil
	}
	if snap.Version != snapshotFormatVersion {
		t.readErr.Add(1)
		return nil, fmt.Errorf("session: 会话快照格式版本 %d 不受支持 (期望 %d)", snap.Version, snapshotFormatVersion)
	}
	msgs := t.restoreMedia(snap.Messages, snap.MediaBlobs)
	normalizeRawInputs(msgs)
	// 再裁一次: 快照可能由更宽松的上限写出 (或上限后来被下调), 读回侧兜住。
	return TrimToTokenBudget(msgs, t.tokenLimit), nil
}

// normalizeRawInputs 把 tool_use 的 Input 压回紧凑 JSON。
//
// 为什么需要: FileStore 的 KV 整桶落盘走 json.MarshalIndent (filestore.go:159-165),
// 它会把内嵌的 json.RawMessage 一起重新缩进。后果有两条 ——
//  1. 同一份历史经 file 后端读回的字节与 mem 后端不同 (语义等价但不可字节比对);
//  2. 那些缩进空白会在此后每一轮 API 请求里白占 prompt token, 且每轮重新落盘时
//     还会被再缩进一层层放大。
//
// 统一压紧后, 读回结果与后端无关。非法 JSON 原样留着 (交给 JSONRepair/网关处理)。
func normalizeRawInputs(msgs []types.Message) {
	var buf bytes.Buffer
	for mi := range msgs {
		for bi := range msgs[mi].Content {
			raw := msgs[mi].Content[bi].Input
			if len(raw) == 0 {
				continue
			}
			buf.Reset()
			if err := json.Compact(&buf, raw); err != nil {
				continue
			}
			compacted := make([]byte, buf.Len())
			copy(compacted, buf.Bytes())
			msgs[mi].Content[bi].Input = compacted
		}
	}
}

// Delete 删除持久化的历史 (对应 /clear 的"用户显式忘记")。
//
// 只删 KV 里的快照, 不删 Blob: Blob 是内容寻址且跨会话去重, 删了可能连带打断
// 其他会话对同一份图片的引用; Blob 由统一的 TTL/GC 负责。
func (t *StateStoreTranscript) Delete() error {
	if t == nil || t.kv == nil {
		return nil
	}
	if err := t.kv.Delete(snapshotKey); err != nil {
		t.writeErr.Add(1)
		return fmt.Errorf("session: 删除会话快照失败 (chat=%s): %w", t.chatID, err)
	}
	return nil
}

// mediaRefKey 侧表 key: "<消息下标>/<内容块下标>"。
func mediaRefKey(msgIdx, blockIdx int) string {
	return strconv.Itoa(msgIdx) + "/" + strconv.Itoa(blockIdx)
}

func parseMediaRefKey(k string) (msgIdx, blockIdx int, ok bool) {
	slash := strings.IndexByte(k, '/')
	if slash <= 0 {
		return 0, 0, false
	}
	m, err1 := strconv.Atoi(k[:slash])
	b, err2 := strconv.Atoi(k[slash+1:])
	if err1 != nil || err2 != nil || m < 0 || b < 0 {
		return 0, 0, false
	}
	return m, b, true
}

// externalizeMedia 把 base64 媒体正文 (ContentBlock.Source.Data) 挪出快照本体。
//
// 为什么必须剥: 飞书视觉路径的图片是 base64 塞在 Source.Data 里 (types.go:64,70-75),
// 一张图就能把单份历史撑到 MB 级 —— 而 KV 是每轮整文件重写。转存 Blob 后正文
// 内容寻址天然去重 (同一张图在多轮历史里只存一份), MediaType / Source.Type /
// Source.URL 等元信息全部原位保留。Blob 写失败则退化为直接丢正文 (仍保元信息)。
//
// 绝不原地修改入参: msgs 的底层数组就是引擎内存里那份 e.Messages。
func (t *StateStoreTranscript) externalizeMedia(msgs []types.Message) ([]types.Message, map[string]string) {
	out := make([]types.Message, len(msgs))
	copy(out, msgs)
	var refs map[string]string
	for mi := range out {
		var blocks []types.ContentBlock // 惰性深拷贝: 只有真含媒体正文的消息才复制内容块
		for bi := range out[mi].Content {
			src := out[mi].Content[bi].Source
			if src == nil || src.Data == "" {
				continue
			}
			if blocks == nil {
				blocks = make([]types.ContentBlock, len(out[mi].Content))
				copy(blocks, out[mi].Content)
			}
			stripped := *src // 值拷贝: 原 Source 归引擎, 不能动
			if hash, err := t.blob.Put([]byte(stripped.Data)); err != nil {
				t.writeErr.Add(1)
			} else {
				if refs == nil {
					refs = make(map[string]string)
				}
				refs[mediaRefKey(mi, bi)] = hash
			}
			stripped.Data = ""
			blocks[bi].Source = &stripped
		}
		if blocks != nil {
			out[mi].Content = blocks
		}
	}
	return out, refs
}

// restoreMedia 从 Blob 反填媒体正文。
//
// 取不回正文的媒体块直接丢弃 (而非留一个空 Data): 一个 type=base64 却没有 data 的
// 图片块会让后续每一轮 API 请求都 400, 那是把一次丢图放大成整个会话不可用。
// 内容块全丢光的消息整条丢弃 (空 content 消息同样会被网关拒)。
func (t *StateStoreTranscript) restoreMedia(msgs []types.Message, refs map[string]string) []types.Message {
	if len(msgs) == 0 {
		return nil
	}
	// 先按消息聚合引用, 避免对每个块都遍历整张表。
	byMsg := make(map[int]map[int]string, len(refs))
	for k, hash := range refs {
		mi, bi, ok := parseMediaRefKey(k)
		if !ok {
			continue
		}
		if byMsg[mi] == nil {
			byMsg[mi] = make(map[int]string)
		}
		byMsg[mi][bi] = hash
	}

	out := make([]types.Message, 0, len(msgs))
	for mi := range msgs {
		msg := msgs[mi]
		needFix := len(byMsg[mi]) > 0
		if !needFix {
			// 无媒体引用, 但仍要防住"有 Source 却没有 Data 也没有 URL"的残块
			hasBroken := false
			for bi := range msg.Content {
				if isBrokenMedia(&msg.Content[bi]) {
					hasBroken = true
					break
				}
			}
			if !hasBroken {
				out = append(out, msg)
				continue
			}
		}
		blocks := make([]types.ContentBlock, 0, len(msg.Content))
		for bi := range msg.Content {
			b := msg.Content[bi]
			if b.Source != nil && b.Source.Data == "" {
				if hash, ok := byMsg[mi][bi]; ok {
					if data, err := t.blob.Get(hash); err == nil {
						src := *b.Source
						src.Data = string(data)
						b.Source = &src
					} else {
						t.readErr.Add(1)
						continue // 正文取不回 → 丢块
					}
				} else if isBrokenMedia(&b) {
					continue
				}
			}
			blocks = append(blocks, b)
		}
		if len(blocks) == 0 {
			continue // 整条消息内容为空 → 丢消息
		}
		msg.Content = blocks
		out = append(out, msg)
	}
	return out
}

// isBrokenMedia 判定"声明了 base64 媒体源却既无正文也无 URL"的残块。
func isBrokenMedia(b *types.ContentBlock) bool {
	return b.Source != nil && b.Source.Data == "" && b.Source.URL == ""
}

// TrimToTokenBudget 把历史裁到 token 上限内 (limit<=0 表示不裁)。
//
// 为什么在持久化边界裁剪 (而不是指望引擎自己压): 恢复出来的 Messages 下一轮确实会
// 过 PhasePreCompact, 但那已经太晚 —— 超阈值就是一次真 LLM 压缩的延迟与 token 尖峰,
// 且 PreCompactFn 会把事实再蒸馏一遍, 可能与重启前重复。压在阈值下就没有这一跳。
//
// IsCompactBoundary 语义 (types.go:103): 该消息是 AutoCompact 产出的摘要边界,
// 一条顶掉它之前的一整段历史。所以裁剪优先"切在最近的边界上"而非机械切尾巴:
//   - 边界及其之后整段装得下 → 从边界开始保留 (摘要 + 后续全在);
//   - 装不下 → 退化为 [边界那条] + 尾部窗口 (下标序不变, 只丢中段)。
//     丢中段可能造出孤儿 tool_use / tool_result, 由 engine.sanitizeToolPairing 兜住。
//
// 估算复用 internal_hook.EstimateMessageTokens (chars/4), 与引擎内部同一把尺子。
func TrimToTokenBudget(msgs []types.Message, limit int) []types.Message {
	if limit <= 0 || len(msgs) == 0 {
		return msgs
	}
	total := 0
	for i := range msgs {
		total += internal_hook.EstimateMessageTokens(msgs[i])
	}
	if total <= limit {
		return msgs
	}

	// 从尾部回退, 求最大的尾部窗口 [cut:]。
	cut := len(msgs)
	used := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		n := internal_hook.EstimateMessageTokens(msgs[i])
		if used+n > limit {
			break
		}
		used += n
		cut = i
	}
	if cut >= len(msgs) {
		// 连最后一条都超限: 也要留住它, 否则读回是空历史 (丢掉用户刚说的话)。
		cut = len(msgs) - 1
		used = internal_hook.EstimateMessageTokens(msgs[cut])
	}

	// 找即将被丢弃部分里最近的 compact 边界。
	boundary := -1
	for i := cut - 1; i >= 0; i-- {
		if msgs[i].IsCompactBoundary {
			boundary = i
			break
		}
	}
	if boundary < 0 {
		return msgs[cut:]
	}
	sum := 0
	for i := boundary; i < len(msgs); i++ {
		sum += internal_hook.EstimateMessageTokens(msgs[i])
	}
	if sum <= limit {
		return msgs[boundary:]
	}
	if bt := internal_hook.EstimateMessageTokens(msgs[boundary]); bt+used <= limit {
		out := make([]types.Message, 0, 1+len(msgs)-cut)
		out = append(out, msgs[boundary])
		out = append(out, msgs[cut:]...)
		return out
	}
	return msgs[cut:]
}
