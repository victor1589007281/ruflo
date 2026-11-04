// resolve_input_test.go — ResolveTurnInput 的生产接缝契约测试（R-H5/R-H11）。
//
// attachment_resolve_test.go 钉的是"引用 → 块"；这里钉的是"轮输入 → 模型输入"
// 合并时的三个容易出错的方向：块的顺序（正文前、附件后）、切片的归属（不能
// 改写调用方的 input）、以及失败方向（不返回半成品，整轮失败）。
//
// 测试同样用真实的 FSBlobStore：这条路径的价值就在于引用能从磁盘解析回内容。
package harness

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// TestResolveTurnInput_AttachmentAfterUserText 是正常路径：用户正文在第一个块，
// 附件块排在后面，且块内带附件名、ID 与正文。
func TestResolveTurnInput_AttachmentAfterUserText(t *testing.T) {
	store := setupStores(t)
	body := []byte("血压 145/95，连续三天头晕。")
	ref, err := Attach(store, AttachmentText, "text/plain", "体征记录.txt", body)
	if err != nil {
		t.Fatal(err)
	}

	userText := "请结合附件判断是否需要复诊。"
	input := []types.ContentBlock{{Type: types.ContentBlockText, Text: userText}}
	snapshot := append([]types.ContentBlock(nil), input...)

	got, err := ResolveTurnInput(store, []AttachmentRef{ref}, input, 0)
	if err != nil {
		t.Fatalf("正常路径不应报错: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应为 1 个正文块 + 1 个附件块, 实际 %d 个", len(got))
	}

	// 顺序：正文必须在附件之前。顺序是请求字节的一部分，反了会改变模型对
	// "哪段是任务、哪段是材料"的理解。
	if got[0].Type != types.ContentBlockText || got[0].Text != userText {
		t.Errorf("第 0 块应是用户正文 %q, 实际 %+v", userText, got[0])
	}
	att := got[1]
	if att.Type != types.ContentBlockText {
		t.Fatalf("文本附件应产出 text 块, 实际 %q", att.Type)
	}
	for _, want := range []string{ref.Name, ref.ID, string(body)} {
		if !strings.Contains(att.Text, want) {
			t.Errorf("附件块应包含 %q, 实际 %q", want, att.Text)
		}
	}

	// 返回新切片：调用方的 input 内容一个字节都不能变。
	if !reflect.DeepEqual(input, snapshot) {
		t.Errorf("调用方 input 被改写了: %+v vs %+v", input, snapshot)
	}

	// 证明底层数组不同：改写返回值不应影响调用方持有的 input。
	got[0].Text = "被调用方改写"
	if input[0].Text != userText {
		t.Errorf("返回切片与 input 共享底层数组, input 被污染为 %q", input[0].Text)
	}
}

// TestResolveTurnInput_NilInputGetsOnlyAttachments 钉住 input 为 nil 时不是 panic、
// 也不是返回 nil，而是只含附件块的新切片。
func TestResolveTurnInput_NilInputGetsOnlyAttachments(t *testing.T) {
	store := setupStores(t)
	body := []byte("孤立的附件正文")
	ref, err := Attach(store, AttachmentText, "text/plain", "only.txt", body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveTurnInput(store, []AttachmentRef{ref}, nil, 0)
	if err != nil {
		t.Fatalf("input 为 nil 不应报错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应只有 1 个附件块, 实际 %d 个", len(got))
	}
	if !strings.Contains(got[0].Text, string(body)) {
		t.Errorf("附件正文缺失, 实际 %q", got[0].Text)
	}
}

// TestResolveTurnInput_EmptyRefsPassthrough 钉住"无附件"路径逐字节不变：
// 内容与 input 完全相同，input 为 nil 时结果必须也是 nil（而不是空切片）。
func TestResolveTurnInput_EmptyRefsPassthrough(t *testing.T) {
	input := []types.ContentBlock{
		{Type: types.ContentBlockText, Text: "只有正文，没有附件"},
	}
	// store 传 nil 也不该报错：refs 为空时根本不该碰存储。
	for _, refs := range [][]AttachmentRef{nil, {}} {
		got, err := ResolveTurnInput(nil, refs, input, 0)
		if err != nil {
			t.Fatalf("空 refs 不应报错: %v", err)
		}
		if !reflect.DeepEqual(got, input) {
			t.Errorf("空 refs 应原样返回 input, 实际 %+v", got)
		}
	}

	gotNil, err := ResolveTurnInput(nil, nil, nil, 0)
	if err != nil {
		t.Fatalf("nil input + nil refs 不应报错: %v", err)
	}
	if gotNil != nil {
		t.Errorf("nil 进必须 nil 出, 实际 %#v", gotNil)
	}
}

// TestResolveTurnInput_NilStoreIsTypedError 钉住 store 缺失时透传 typed error，
// 且不返回任何块（包括不用 input 凑一个"看起来还能发"的结果）。
func TestResolveTurnInput_NilStoreIsTypedError(t *testing.T) {
	ref := AttachmentRef{
		ID:       "att-turn-no-store",
		Kind:     AttachmentText,
		BlobHash: strings.Repeat("ab", 32),
	}
	input := []types.ContentBlock{{Type: types.ContentBlockText, Text: "正文"}}

	blocks, err := ResolveTurnInput(nil, []AttachmentRef{ref}, input, 0)
	if err == nil {
		t.Fatalf("store 为 nil 必须报错, 却返回了 %d 个块", len(blocks))
	}
	if blocks != nil {
		t.Errorf("失败时不得返回块（含 input）, 实际 %+v", blocks)
	}
	if !errors.Is(err, ErrAttachmentUnresolved) {
		t.Errorf("应为 ErrAttachmentUnresolved, 实际 %v", err)
	}
}

// TestResolveTurnInput_OverBudgetReturnsNoPartialBlocks 钉住超预算时不能把
// 已解析的前几个附件发出去：那正是"模型少看一份材料却回答得很确定"的来源。
func TestResolveTurnInput_OverBudgetReturnsNoPartialBlocks(t *testing.T) {
	store := setupStores(t)
	img := bytes.Repeat([]byte("I"), 4096)
	ref, err := Attach(store, AttachmentImage, "image/png", "big.png", img)
	if err != nil {
		t.Fatal(err)
	}
	input := []types.ContentBlock{{Type: types.ContentBlockText, Text: "正文"}}

	blocks, err := ResolveTurnInput(store, []AttachmentRef{ref}, input, 1024)
	if err == nil {
		t.Fatalf("超预算必须报错, 却返回了 %d 个块", len(blocks))
	}
	if blocks != nil {
		t.Errorf("超预算时不得返回部分块, 实际 %+v", blocks)
	}
	if !errors.Is(err, ErrAttachmentBudget) {
		t.Errorf("应为 ErrAttachmentBudget, 实际 %v", err)
	}
}

// TestResolveTurnInput_SameHashKeepsBothBlocksInOrder 钉住同 hash 的两个引用
// **各出一块**且顺序对应。按 hash 去重掉第二个块会让"同一份材料在不同轮/
// 不同锚点被引用两次"变成一次，块数对不上就是模型少读一份引用。
func TestResolveTurnInput_SameHashKeepsBothBlocksInOrder(t *testing.T) {
	store := setupStores(t)
	body := []byte("同一份材料，被引用了两次")
	first, err := Attach(store, AttachmentText, "text/plain", "首次.txt", body)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = "att-second-ref"
	second.Name = "再次.txt"

	input := []types.ContentBlock{{Type: types.ContentBlockText, Text: "正文"}}
	got, err := ResolveTurnInput(store, []AttachmentRef{first, second}, input, 0)
	if err != nil {
		t.Fatalf("重复引用同一 hash 不应报错: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应为 1 正文 + 2 附件 = 3 个块, 实际 %d 个", len(got))
	}
	// 顺序对应 refs：第一块带第一次引用的 ID/名字，第二块带第二次的。
	if !strings.Contains(got[1].Text, first.ID) || !strings.Contains(got[1].Text, "首次.txt") {
		t.Errorf("第 1 个附件块应对应第一次引用, 实际 %q", got[1].Text)
	}
	if !strings.Contains(got[2].Text, second.ID) || !strings.Contains(got[2].Text, "再次.txt") {
		t.Errorf("第 2 个附件块应对应第二次引用, 实际 %q", got[2].Text)
	}
	// 同一正文但引用元数据不同：两个块不该被误合并。
	if got[1].Text == got[2].Text {
		t.Error("两个引用产出完全相同的块，疑似按 hash 去重掉了引用元数据")
	}
}
