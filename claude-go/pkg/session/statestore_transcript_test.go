package session

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/types"
)

// kvBytes 统计 FileStore root 下 kv/ 目录的总字节数 —— 也就是"每轮整文件重写"
// 真正要付的那部分体积 (Blob 是内容寻址, 写一次就去重, 不参与每轮重写)。
func kvBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(filepath.Join(root, "kv"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("统计 kv 体积失败: %v", err)
	}
	return total
}

// TestSnapshotStripsBase64Media 坑 5.6: 飞书视觉路径的 base64 图片必须离开 KV 本体
// (KV 是每轮整文件重写), 但 MediaType 等元信息必须原位保留, 且正文可从 Blob 取回。
func TestSnapshotStripsBase64Media(t *testing.T) {
	root := t.TempDir()
	ss := statestore.NewFileStore(root)
	tr := NewStateStoreTranscript(ss, "oc_vision")

	const imgSize = 1 << 20 // 1MiB base64 正文
	imgData := strings.Repeat("A", imgSize)
	orig := []types.Message{{
		Type: types.MessageTypeUser,
		UUID: "u1",
		Content: []types.ContentBlock{
			{Type: types.ContentBlockImage, Source: &types.MediaSource{
				Type: "base64", MediaType: "image/png", Data: imgData,
			}},
			{Type: types.ContentBlockText, Text: "这张图里是什么?"},
		},
	}}

	if err := tr.Snapshot(orig); err != nil {
		t.Fatalf("Snapshot 失败: %v", err)
	}

	// 1) KV 本体不得含 base64 正文 (阈值取 64KiB: 远小于 1MiB, 又足够容纳元信息+文本)
	if got := kvBytes(t, root); got >= 64*1024 {
		t.Fatalf("KV 体积 %d 字节, 期望 < 65536 (base64 正文应已剥离到 Blob)", got)
	}

	// 2) 入参绝不能被就地改写 (它的底层数组就是引擎内存里的 e.Messages)
	if orig[0].Content[0].Source.Data != imgData {
		t.Fatalf("Snapshot 就地改写了调用方的 Source.Data (长度 %d), 必须只改副本",
			len(orig[0].Content[0].Source.Data))
	}

	// 3) 读回: 元信息保留 + 正文从 Blob 完整取回
	got, err := tr.Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(got) != 1 || len(got[0].Content) != 2 {
		t.Fatalf("读回结构不符: %d 条消息", len(got))
	}
	src := got[0].Content[0].Source
	if src == nil {
		t.Fatal("媒体块 Source 丢失")
	}
	if src.MediaType != "image/png" || src.Type != "base64" {
		t.Fatalf("元信息未保留: type=%q mediaType=%q", src.Type, src.MediaType)
	}
	if src.Data != imgData {
		t.Fatalf("Blob 正文未完整取回: 长度 %d, 期望 %d", len(src.Data), imgSize)
	}
	if got[0].Content[1].Text != "这张图里是什么?" {
		t.Fatalf("文本块被破坏: %q", got[0].Content[1].Text)
	}
}

// TestSnapshotDropsMediaWhenBlobMissing 正文取不回时整块丢弃 (而不是留一个 data 为空
// 的 base64 块 —— 那会让此后每一轮 API 请求都 400)。
func TestSnapshotDropsMediaWhenBlobMissing(t *testing.T) {
	// 正常写入, 读回时把 Blob 换成恒空实现: 模拟 blob 被 TTL/GC 清掉。
	writeSS := statestore.NewMemStore()
	tr := NewStateStoreTranscript(writeSS, "oc_lost")
	msgs := []types.Message{{
		Type: types.MessageTypeUser,
		Content: []types.ContentBlock{
			{Type: types.ContentBlockImage, Source: &types.MediaSource{Type: "base64", MediaType: "image/png", Data: "PAYLOAD"}},
			{Type: types.ContentBlockText, Text: "保留我"},
		},
	}, {
		Type:    types.MessageTypeAssistant,
		Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "好"}},
	}}
	if err := tr.Snapshot(msgs); err != nil {
		t.Fatalf("Snapshot 失败: %v", err)
	}

	// 同一份 KV, 但 Blob 取不回任何东西。
	tr2 := NewStateStoreTranscript(noBlobStore{writeSS}, "oc_lost")
	got, err := tr2.Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("期望 2 条消息 (媒体块丢弃但消息保留), 得到 %d", len(got))
	}
	if len(got[0].Content) != 1 || got[0].Content[0].Text != "保留我" {
		t.Fatalf("期望媒体块被丢弃、文本块保留, 得到 %+v", got[0].Content)
	}
	if tr2.ReadErrors() == 0 {
		t.Fatal("blob 取不回应计入 ReadErrors")
	}
}

// noBlobStore 保留 KV/Log, 但 Blob 恒空 (模拟 blob 丢失/被 GC)。
type noBlobStore struct{ inner statestore.StateStore }

func (n noBlobStore) KV(b string) statestore.KVStore    { return n.inner.KV(b) }
func (n noBlobStore) Log(b string) statestore.AppendLog { return n.inner.Log(b) }
func (n noBlobStore) Blob() statestore.BlobStore        { return emptyBlob{} }

type emptyBlob struct{}

func (emptyBlob) Put(data []byte) (string, error) { return strings.Repeat("0", 64), nil }
func (emptyBlob) Get(string) ([]byte, error)      { return nil, fs.ErrNotExist }
func (emptyBlob) Has(string) bool                 { return false }

// TestTrimKeepsCompactBoundary 坑 5.5: 裁剪必须保住 IsCompactBoundary 那条 ——
// 它是 AutoCompact 产出的摘要, 一条顶掉之前一整段历史, 机械切尾巴会把它丢掉。
func TestTrimKeepsCompactBoundary(t *testing.T) {
	big := strings.Repeat("x", 4000) // ≈1000 est tokens
	msgs := []types.Message{
		{Type: types.MessageTypeUser, UUID: "old", Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: big}}},
		{Type: types.MessageTypeAssistant, UUID: "summary", IsCompactBoundary: true,
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "【历史摘要】用户要做 X"}}},
		{Type: types.MessageTypeUser, UUID: "n1", Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: big}}},
		{Type: types.MessageTypeAssistant, UUID: "n2", Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: big}}},
	}

	// limit 装不下 4 条 (≈3000+) 但装得下"边界及其之后" (≈2000+)
	got := TrimToTokenBudget(msgs, 2500)
	if len(got) != 3 || got[0].UUID != "summary" {
		var ids []string
		for _, m := range got {
			ids = append(ids, m.UUID)
		}
		t.Fatalf("期望从 compact 边界起保留 3 条 [summary n1 n2], 得到 %v", ids)
	}
	if !got[0].IsCompactBoundary {
		t.Fatal("IsCompactBoundary 标记在裁剪后丢失")
	}

	// 边界那段也装不下时: 退化为 [边界] + 尾部窗口, 边界仍在且顺序不变
	got = TrimToTokenBudget(msgs, 1200)
	if len(got) != 2 || got[0].UUID != "summary" || got[1].UUID != "n2" {
		var ids []string
		for _, m := range got {
			ids = append(ids, m.UUID)
		}
		t.Fatalf("期望退化为 [summary n2], 得到 %v", ids)
	}
	if !got[0].IsCompactBoundary {
		t.Fatal("退化路径丢了 IsCompactBoundary 标记")
	}

	// 上限充足 → 原样返回
	if got = TrimToTokenBudget(msgs, 100000); len(got) != 4 {
		t.Fatalf("上限充足时不应裁剪, 得到 %d 条", len(got))
	}
	// 连最后一条都超限 → 至少留住最后一条 (不能读回空历史)
	if got = TrimToTokenBudget(msgs, 1); len(got) != 1 || got[0].UUID != "n2" {
		t.Fatalf("期望至少保留最后一条, 得到 %d 条", len(got))
	}
}

// TestSnapshotBoundaryFlagSurvivesRoundTrip IsCompactBoundary 有 json tag,
// 必须能穿过"裁剪 + 落盘 + 读回"整条链路。
func TestSnapshotBoundaryFlagSurvivesRoundTrip(t *testing.T) {
	tr := NewStateStoreTranscript(statestore.NewMemStore(), "oc_boundary")
	tr.SetTokenLimit(2500)
	big := strings.Repeat("y", 4000)
	msgs := []types.Message{
		{Type: types.MessageTypeUser, UUID: "old", Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: big}}},
		{Type: types.MessageTypeAssistant, UUID: "summary", IsCompactBoundary: true,
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "摘要"}}},
		{Type: types.MessageTypeUser, UUID: "n1", Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: big}}},
		{Type: types.MessageTypeAssistant, UUID: "n2", Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: big}}},
	}
	if err := tr.Snapshot(msgs); err != nil {
		t.Fatalf("Snapshot 失败: %v", err)
	}
	got, err := tr.Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(got) == 0 || got[0].UUID != "summary" || !got[0].IsCompactBoundary {
		t.Fatalf("读回后 compact 边界丢失: %+v", got)
	}
	if len(got) != 3 {
		t.Fatalf("期望读回 3 条 (已按上限裁剪), 得到 %d", len(got))
	}
}

// TestSnapshotCompactsToolInputAcrossBackends 回归: FileStore 的 KV 整桶落盘用
// json.MarshalIndent, 会把内嵌的 json.RawMessage (tool_use.Input) 重新缩进 ——
// 不归一就会造成 file/mem 两后端读回字节不同, 且缩进空白此后每轮都白占 prompt token
// 并被逐轮再缩进放大。Load 必须压回紧凑形式。
func TestSnapshotCompactsToolInputAcrossBackends(t *testing.T) {
	const wantInput = `{"path":"a.go","limit":10}`
	msgs := []types.Message{{
		Type: types.MessageTypeAssistant,
		Content: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, ID: "t1", Name: "Read", Input: []byte(wantInput)},
		},
	}}

	stores := map[string]statestore.StateStore{
		"file": statestore.NewFileStore(t.TempDir()),
		"mem":  statestore.NewMemStore(),
	}
	for name, ss := range stores {
		t.Run(name, func(t *testing.T) {
			tr := NewStateStoreTranscript(ss, "oc_input")
			if err := tr.Snapshot(msgs); err != nil {
				t.Fatalf("Snapshot 失败: %v", err)
			}
			got, err := tr.Load()
			if err != nil {
				t.Fatalf("Load 失败: %v", err)
			}
			if len(got) != 1 || len(got[0].Content) != 1 {
				t.Fatalf("读回结构不符: %+v", got)
			}
			if string(got[0].Content[0].Input) != wantInput {
				t.Fatalf("tool_use Input 未归一为紧凑 JSON:\n want %s\n got  %s",
					wantInput, got[0].Content[0].Input)
			}
			// 二次快照必须稳定 (不能每轮把缩进层层放大)
			if err := tr.Snapshot(got); err != nil {
				t.Fatalf("二次 Snapshot 失败: %v", err)
			}
			again, err := tr.Load()
			if err != nil {
				t.Fatalf("二次 Load 失败: %v", err)
			}
			if string(again[0].Content[0].Input) != wantInput {
				t.Fatalf("多轮往返后 Input 漂移: %s", again[0].Content[0].Input)
			}
		})
	}
}

// TestTranscriptBucketIsHashedAndInjective 坑 5.3: bucket 名必须是哈希 ——
// validateBucket 禁止 '/' 等字符, 而"非法字符替换成 _"是有损映射会撞桶。
func TestTranscriptBucketIsHashedAndInjective(t *testing.T) {
	a := TranscriptBucket("oc_a/b")
	b := TranscriptBucket("oc_a_b")
	if a == b {
		t.Fatalf("两个不同 chatID 撞进同一个 bucket: %s", a)
	}
	for _, name := range []string{a, b} {
		if !strings.HasPrefix(name, "transcript-") {
			t.Fatalf("bucket 名缺前缀: %s", name)
		}
		for _, r := range name {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			default:
				t.Fatalf("bucket 名含 statestore 不允许的字符 %q: %s", r, name)
			}
		}
	}
	// 合法性反证: 非法 bucket 名走 badBucketKV, 写必失败
	if err := NewStateStoreTranscript(statestore.NewFileStore(t.TempDir()), "oc_a/b").
		Snapshot([]types.Message{{Type: types.MessageTypeUser}}); err != nil {
		t.Fatalf("哈希后的 bucket 名应被 statestore 接受, 却报错: %v", err)
	}
}

// TestSnapshotToolResultRoundTrip 快照必须含 tool_result (旧 JSONL transcript 的老缺陷:
// 承载 tool_result 的 user 消息从不落盘, 读回的链条残缺)。
func TestSnapshotToolResultRoundTrip(t *testing.T) {
	tr := NewStateStoreTranscript(statestore.NewMemStore(), "oc_tools")
	msgs := []types.Message{
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "读一下 main.go"}}},
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockThinking, Thinking: "先 Read"},
			{Type: types.ContentBlockToolUse, ID: "t1", Name: "Read", Input: []byte(`{"path":"main.go"}`)},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "t1", Content: "package main"},
		}},
	}
	if err := tr.Snapshot(msgs); err != nil {
		t.Fatalf("Snapshot 失败: %v", err)
	}
	got, err := tr.Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	var sawResult, sawThinking bool
	for _, m := range got {
		for _, b := range m.Content {
			if b.Type == types.ContentBlockToolResult && b.ToolUseID == "t1" && b.Content == "package main" {
				sawResult = true
			}
			if b.Type == types.ContentBlockThinking && b.Thinking == "先 Read" {
				sawThinking = true
			}
		}
	}
	if !sawResult {
		t.Fatal("tool_result 未落盘/未读回")
	}
	if !sawThinking {
		t.Fatal("thinking 块未落盘/未读回")
	}
}
