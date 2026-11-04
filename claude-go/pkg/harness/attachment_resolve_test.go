// attachment_resolve_test.go — AttachmentRef → 协议内容块的解析契约测试。
//
// 钉的是"引用形式流转"的最后一环：字节只在解析时从存储取出，
// 图片/文本产出正确的协议块，取不到与超预算都是 typed error 而不是静默跳过。
//
// 测试用真实的 FSBlobStore + t.TempDir()，不 mock 存储层：被验证的恰恰是
// "引用经磁盘往返后还能解析回原字节"这条链路，mock 掉存储就把这条链路删了。
package harness

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/types"
)

// countingStore 包一层 BlobStore，统计 Get 次数。
//
// 只计 Get：Put 发生在构造附件的阶段，不在被测路径上；把它也算进来会让
// "同 hash 只读一次"的判据被无关调用污染。
type countingStore struct {
	BlobStore
	getCalls int
}

func (s *countingStore) Get(hash string) ([]byte, error) {
	s.getCalls++
	return s.BlobStore.Get(hash)
}

// nilReturningStore 模拟一个坏实现：Get 返回 (nil, nil)。
//
// 这种实现不会报错，但解析器若把 nil 当成"空内容"，就会产出一个空的 image 块
// 或空 text 块 —— 模型看不到图，调用方却以为请求完整。必须当作取不到处理。
type nilReturningStore struct{}

func (nilReturningStore) Put(content []byte) (string, error) { return ContentHash(content), nil }
func (nilReturningStore) Get(hash string) ([]byte, error)    { return nil, nil }
func (nilReturningStore) Size(hash string) (int64, error)    { return 0, nil }

// setupStores 建立真实存储并返回它。
func setupStores(t *testing.T) *FSBlobStore {
	t.Helper()
	store, err := NewFSBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestResolveAttachments_ImageAndTextRoundTrip 是正常路径：image 带 media type 与
// 可还原的 base64，text 内联且带来源标注。
func TestResolveAttachments_ImageAndTextRoundTrip(t *testing.T) {
	store := setupStores(t)
	img := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	txt := []byte("用户手写的症状描述：最近三天头晕。")

	imgRef, err := Attach(store, AttachmentImage, "image/png", "症状.png", img)
	if err != nil {
		t.Fatal(err)
	}
	txtRef, err := Attach(store, AttachmentText, "text/plain", "描述.txt", txt)
	if err != nil {
		t.Fatal(err)
	}

	blocks, err := ResolveAttachments(store, []AttachmentRef{imgRef, txtRef}, 0)
	if err != nil {
		t.Fatalf("正常路径不应报错: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("应产出 2 个块, 实际 %d", len(blocks))
	}

	// image 块
	if blocks[0].Type != types.ContentBlockImage {
		t.Errorf("第一个块应为 image, 实际 %q", blocks[0].Type)
	}
	if blocks[0].Source == nil {
		t.Fatal("image 块缺少 source")
	}
	if blocks[0].Source.Type != "base64" {
		t.Errorf("image source.type 应为 base64, 实际 %q", blocks[0].Source.Type)
	}
	if blocks[0].Source.MediaType != "image/png" {
		t.Errorf("image media_type 应为 image/png, 实际 %q", blocks[0].Source.MediaType)
	}
	gotImg, derr := base64.StdEncoding.DecodeString(blocks[0].Source.Data)
	if derr != nil {
		t.Fatalf("image data 不是合法 base64: %v", derr)
	}
	if !bytes.Equal(gotImg, img) {
		t.Errorf("image 解出的字节与原字节不同: %q vs %q", gotImg, img)
	}

	// text 块
	if blocks[1].Type != types.ContentBlockText {
		t.Errorf("第二个块应为 text, 实际 %q", blocks[1].Type)
	}
	if !strings.Contains(blocks[1].Text, string(txt)) {
		t.Errorf("text 块应内联附件正文, 实际 %q", blocks[1].Text)
	}
	if !strings.Contains(blocks[1].Text, txtRef.ID) {
		t.Errorf("text 块应带附件 ID 的来源标注, 实际 %q", blocks[1].Text)
	}
	if !strings.Contains(blocks[1].Text, "描述.txt") {
		t.Errorf("text 块应带文件名的来源标注, 实际 %q", blocks[1].Text)
	}
	// 正文之外的包装不应破坏原文：去掉标注后必须能找回完整原文。
	if !strings.HasSuffix(blocks[1].Text, string(txt)) {
		t.Errorf("附件正文应完整出现在块尾, 实际 %q", blocks[1].Text)
	}
}

// TestResolveAttachments_PDFBecomesDocumentBlock 钉住 pdf 走 document 块，
// 而不是被当成图片或文本。
func TestResolveAttachments_PDFBecomesDocumentBlock(t *testing.T) {
	store := setupStores(t)
	pdf := []byte("%PDF-1.7\nfake")
	ref, err := Attach(store, AttachmentPDF, "application/pdf", "报告.pdf", pdf)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := ResolveAttachments(store, []AttachmentRef{ref}, 0)
	if err != nil {
		t.Fatalf("pdf 解析不应报错: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != types.ContentBlockDocument {
		t.Fatalf("pdf 应产出 document 块, 实际 %+v", blocks)
	}
	if blocks[0].Source == nil || blocks[0].Source.MediaType != "application/pdf" {
		t.Errorf("document 的 media_type 应为 application/pdf, 实际 %+v", blocks[0].Source)
	}
}

// TestResolveAttachments_MissingHashIsTypedError 钉住"取不到必须报错且能定位"。
//
// 静默跳过是最坏的失败方向：请求看起来成功，模型却少看了一份附件。
// 错误里必须带附件 ID（以及 hash），否则用户面对"附件解析失败"无从重传。
func TestResolveAttachments_MissingHashIsTypedError(t *testing.T) {
	store := setupStores(t)
	ref := AttachmentRef{
		ID:       "att-missing-001",
		Kind:     AttachmentImage,
		MIME:     "image/png",
		BlobHash: strings.Repeat("ab", 32), // 合法形状但从未写入
		Name:     "gone.png",
	}
	blocks, err := ResolveAttachments(store, []AttachmentRef{ref}, 0)
	if err == nil {
		t.Fatalf("缺 blob 必须报错, 却返回了 %d 个块", len(blocks))
	}
	if blocks != nil {
		t.Errorf("失败时不应返回部分块, 实际 %d 个", len(blocks))
	}
	if !errors.Is(err, ErrAttachmentUnresolved) {
		t.Errorf("应为 ErrAttachmentUnresolved, 实际 %v", err)
	}
	if !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("应保留底层 ErrBlobNotFound 供分流, 实际 %v", err)
	}
	var rerr *AttachmentResolveError
	if !errors.As(err, &rerr) {
		t.Fatalf("应为 *AttachmentResolveError, 实际 %T", err)
	}
	if rerr.AttachmentID != ref.ID {
		t.Errorf("错误应带附件 ID %q, 实际 %q", ref.ID, rerr.AttachmentID)
	}
	if !strings.Contains(err.Error(), ref.ID) {
		t.Errorf("错误文本应含附件 ID %q, 实际 %q", ref.ID, err.Error())
	}
	if !strings.Contains(err.Error(), ref.BlobHash) {
		t.Errorf("错误文本应含 hash, 实际 %q", err.Error())
	}
}

// TestResolveAttachments_NilStoreIsError 钉住"存储为 nil 不是 panic，也不是无附件"。
func TestResolveAttachments_NilStoreIsError(t *testing.T) {
	ref := AttachmentRef{ID: "att-nil-store", Kind: AttachmentText, BlobHash: strings.Repeat("cd", 32)}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("nil 存储让解析器 panic 了: %v", r)
			}
		}()
		blocks, err := ResolveAttachments(nil, []AttachmentRef{ref}, 0)
		if err == nil {
			t.Fatalf("nil 存储必须报错, 却返回 %d 个块", len(blocks))
		}
		if !strings.Contains(err.Error(), ref.ID) {
			t.Errorf("错误应带附件 ID %q, 实际 %q", ref.ID, err.Error())
		}
		if !errors.Is(err, ErrAttachmentUnresolved) {
			t.Errorf("应为 ErrAttachmentUnresolved, 实际 %v", err)
		}
	}()
}

// TestResolveAttachments_StoreReturningNilIsError 钉住坏实现的 nil 字节不会被
// 当成"空附件"。
func TestResolveAttachments_StoreReturningNilIsError(t *testing.T) {
	ref := AttachmentRef{ID: "att-nil-bytes", Kind: AttachmentImage, MIME: "image/png", BlobHash: strings.Repeat("ef", 32)}
	blocks, err := ResolveAttachments(nilReturningStore{}, []AttachmentRef{ref}, 0)
	if err == nil {
		t.Fatalf("Get 返回 nil 字节必须报错, 却返回 %d 个块", len(blocks))
	}
	if !strings.Contains(err.Error(), ref.ID) {
		t.Errorf("错误应带附件 ID %q, 实际 %q", ref.ID, err.Error())
	}
}

// TestResolveAttachments_ImageOverBudgetFails 钉住图片超限**不截断**。
//
// 截断的图片是坏图：模型看到乱码或上游直接 400，比"明确告诉用户文件太大"更难查。
func TestResolveAttachments_ImageOverBudgetFails(t *testing.T) {
	store := setupStores(t)
	img := bytes.Repeat([]byte("I"), 2048)
	ref, err := Attach(store, AttachmentImage, "image/png", "big.png", img)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := ResolveAttachments(store, []AttachmentRef{ref}, 1024)
	if err == nil {
		t.Fatalf("图片超预算必须报错, 却返回了 %d 个块", len(blocks))
	}
	if !errors.Is(err, ErrAttachmentBudget) {
		t.Errorf("应为 ErrAttachmentBudget, 实际 %v", err)
	}
	var rerr *AttachmentResolveError
	if !errors.As(err, &rerr) {
		t.Fatalf("应为 *AttachmentResolveError, 实际 %T", err)
	}
	if rerr.AttachmentID != ref.ID {
		t.Errorf("错误应指出是哪个附件超限, 实际 %q", rerr.AttachmentID)
	}
	if rerr.Size != int64(len(img)) || rerr.Limit != 1024 {
		t.Errorf("错误应带实际大小与上限, 实际 size=%d limit=%d", rerr.Size, rerr.Limit)
	}
	// 错误里不得出现附件正文（base64 或原文），否则日志/回传又把内容带出去了。
	if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(img)) {
		t.Error("错误信息里出现了附件正文 base64")
	}
}

// TestResolveAttachments_TextOverBudgetTruncatesWithMarker 钉住文本超限的截断
// 必须**明示**：保留前 N 字节，并在正文里写原始字节数与上限。
//
// 中文内容顺带钉住截断点落在 UTF-8 字符边界上。
func TestResolveAttachments_TextOverBudgetTruncatesWithMarker(t *testing.T) {
	store := setupStores(t)
	content := []byte(strings.Repeat("中文", 100)) // 600 字节
	ref, err := Attach(store, AttachmentText, "text/plain", "长文.txt", content)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := ResolveAttachments(store, []AttachmentRef{ref}, 100)
	if err != nil {
		t.Fatalf("文本超限应截断而不是报错: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != types.ContentBlockText {
		t.Fatalf("应产出 1 个 text 块, 实际 %+v", blocks)
	}
	text := blocks[0].Text
	if !strings.Contains(text, "附件已截断") {
		t.Errorf("截断的正文必须带截断标记, 实际 %q", text)
	}
	if !strings.Contains(text, "原始 600 字节") {
		t.Errorf("截断标记必须带原始字节数 600, 实际 %q", text)
	}
	if !strings.Contains(text, "上限 100") && !strings.Contains(text, "预算 100") {
		t.Errorf("截断标记必须带上限 100, 实际 %q", text)
	}
	// 保留的正文必须是合法 UTF-8 且是原文的前缀。
	bodyStart := strings.Index(text, "\n") + 1
	bodyEnd := strings.Index(text, "\n\n[附件已截断")
	if bodyStart <= 0 || bodyEnd < bodyStart {
		t.Fatalf("无法在块里定位正文区间: %q", text)
	}
	body := text[bodyStart:bodyEnd]
	if !strings.HasPrefix(string(content), body) {
		t.Errorf("截断后的正文必须是原文前缀, 实际 %q", body)
	}
	if !utf8Valid(body) {
		t.Errorf("截断后的正文不是合法 UTF-8: %q", body)
	}
	if len(body) >= len(content) {
		t.Errorf("正文未被截断: %d 字节", len(body))
	}
}

// utf8Valid 判断字符串是否为完整合法的 UTF-8（截断点落在字符边界上）。
func utf8Valid(s string) bool { return utf8.ValidString(s) }

// TestResolveAttachments_SameHashReadOnce 钉住两件事：
//   - 同 hash 的多次引用**各出一块**（引用可能指向不同 Anchor）；
//   - 但 store 只被读一次（缓存按 hash）。
//
// 只断言块数而不计次，删掉缓存测试依然是绿的；只计次不断言块数，去重去过头
// （把第二个引用吞掉）也依然是绿的。两件一起钉。
func TestResolveAttachments_SameHashReadOnce(t *testing.T) {
	store := setupStores(t)
	content := []byte("同一张图在会话里被引用了两次")
	base, err := Attach(store, AttachmentImage, "image/png", "dup.png", content)
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingStore{BlobStore: store}

	second := base
	second.ID = "att-second-ref"
	second.Anchors = []Anchor{{Kind: "region", Start: 10, End: 20}}

	blocks, err := ResolveAttachments(counting, []AttachmentRef{base, second}, 0)
	if err != nil {
		t.Fatalf("重复引用不应报错: %v", err)
	}
	if len(blocks) != 2 {
		t.Errorf("2 次引用应产出 2 个块, 实际 %d", len(blocks))
	}
	if counting.getCalls != 1 {
		t.Errorf("同 hash 应只读 store 一次, 实际 %d 次", counting.getCalls)
	}
	if !reflect.DeepEqual(blocks[0], blocks[1]) {
		t.Errorf("同一内容的两个块应相同, 实际 %+v vs %+v", blocks[0], blocks[1])
	}
}

// TestResolveAttachments_ByteStableAcrossRuns 钉住逐字节确定性。
//
// 同一输入两次解析必须产出逐字节相同的块序列。支配它的两条实现要求是：
// 块顺序按输入 refs 的顺序（不能走 map 迭代），以及 base64 编码稳定。
func TestResolveAttachments_ByteStableAcrossRuns(t *testing.T) {
	store := setupStores(t)
	var refs []AttachmentRef
	for _, item := range []struct {
		kind string
		mime string
		name string
		body []byte
	}{
		{AttachmentImage, "image/png", "a.png", bytes.Repeat([]byte{0x01, 0x02}, 300)},
		{AttachmentText, "text/plain", "b.txt", []byte("第二份附件")},
		{AttachmentImage, "image/jpeg", "c.jpg", bytes.Repeat([]byte{0x03}, 400)},
	} {
		ref, err := Attach(store, item.kind, item.mime, item.name, item.body)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}

	first, err := ResolveAttachments(store, refs, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveAttachments(store, refs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("两次解析的块序列不相等")
	}
	b1, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("两次解析的 JSON 字节不同:\n%s\n%s", b1, b2)
	}
	// 顺序必须等于输入顺序：三种块的类型序列可以直接核对。
	wantTypes := []types.ContentBlockType{
		types.ContentBlockImage, types.ContentBlockText, types.ContentBlockImage,
	}
	for i, want := range wantTypes {
		if first[i].Type != want {
			t.Errorf("第 %d 个块类型应为 %q, 实际 %q（块顺序必须按输入 refs）", i, want, first[i].Type)
		}
	}
}

// TestResolveAttachments_EmptyRefsIsNil 钉住空输入返回 nil 而不是空切片。
//
// 调用方据此走"没有附件"的旧路径；返回空切片会让 JSON 序列化多出一个
// "attachments": []，破坏请求体逐字节不变。
func TestResolveAttachments_EmptyRefsIsNil(t *testing.T) {
	store := setupStores(t)
	for _, refs := range [][]AttachmentRef{nil, {}} {
		blocks, err := ResolveAttachments(store, refs, 0)
		if err != nil {
			t.Errorf("空 refs 不应报错: %v", err)
		}
		if blocks != nil {
			t.Errorf("空 refs 应返回 nil, 实际 %#v", blocks)
		}
	}
}

// TestResolveAttachments_AudioAndUnknownKindFail 钉住协议表达不了的种类
// **明确失败**，而不是退化成 text 块。
func TestResolveAttachments_AudioAndUnknownKindFail(t *testing.T) {
	store := setupStores(t)
	audio, err := Attach(store, AttachmentAudio, "audio/mpeg", "voice.mp3", []byte("fake-audio"))
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := ResolveAttachments(store, []AttachmentRef{audio}, 0)
	if err == nil {
		t.Fatalf("audio 附件应报错, 却返回了 %d 个块", len(blocks))
	}
	if !errors.Is(err, ErrAttachmentKindUnsupported) {
		t.Errorf("应为 ErrAttachmentKindUnsupported, 实际 %v", err)
	}

	unknown := audio
	unknown.Kind = "video"
	unknown.ID = "att-video"
	if _, err := ResolveAttachments(store, []AttachmentRef{unknown}, 0); !errors.Is(err, ErrAttachmentKindUnsupported) {
		t.Errorf("未知种类应为 ErrAttachmentKindUnsupported, 实际 %v", err)
	}
}
