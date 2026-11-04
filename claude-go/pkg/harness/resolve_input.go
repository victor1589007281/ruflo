// resolve_input.go — 把本轮附件拼进模型输入的库级接缝（Moqi R-H5/R-H11）。
//
// # 为什么需要这一层
//
// attachment_resolve.go 只回答"引用怎么变成协议内容块"，但它没有生产调用方时，
// 附件就永远停在 req.Attachments 里：最多参与一次模态校验，然后被 sink 原样
// 记录，模型请求里一个字节都没有。用户上传了检查报告，模型却只看到正文 ——
// 而且没有任何信号说明它没看到内容。本文件是 runner 构造模型请求前要调用的
// 那一步：解析引用，并把附件块合并到用户正文之后。
//
// # 为什么正文在前、附件在后
//
// 用户这一轮打的字是"指令"，附件是被指令指向的"材料"。模型按块顺序读请求：
// 正文先说"看附件里的剂量对不对"，后面的附件块才是它要去核对的材料。反过来
// 先放附件，模型先看到一大段没有任务上下文的字节，容易把附件自身当成任务；
// 同时这也让"正文 + 附件"与"只发正文"的历史前缀尽量保持一致。
//
// # 为什么失败必须整轮失败
//
// 解析失败（取不到、超预算、种类表达不了）时返回 nil 块 + typed error，绝不
// 返回"input + 已经解析成功的那几个附件"。少几个附件继续发是静默降级：模型
// 会基于不完整材料给出很确定的回答，调用方和用户却都以为附件已被看到。宁可
// 整轮报错，让业务层按 errors.Is 分流（重传 / 换小文件 / 换模型）。
package harness

import "github.com/anthropic/claude-go/pkg/types"

// ResolveTurnInput 把本轮附件解析成协议内容块，追加在 input 之后。
//
// refs 为空时原样返回 input（nil 进 nil 出）：没有附件时这条路径不产生分配，
// 请求体与附件功能上线前保持一致，也让调用方可以无条件调用而不写分支。
//
// refs 非空时返回的切片总是新分配的：调用方可能持有并复用 input（例如把它
// 作为下一轮的历史），直接 append 到 input 的底层数组会写坏调用方的数据，
// 且这种损坏只在容量恰好够时出现，极难复现。input 为 nil 时结果里只有附件块。
//
// 错误原样透传 ResolveAttachments 的 typed error（业务层靠 errors.Is 区分
// "引用失效该重传"与"文件太大该换文件"），同时返回 nil 块 —— 调用方看到非 nil
// 块就有理由继续发请求，而附件已经缺了，所以失败路径上不能带任何块。
func ResolveTurnInput(store BlobStore, refs []AttachmentRef, input []types.ContentBlock, maxBytes int64) ([]types.ContentBlock, error) {
	if len(refs) == 0 {
		return input, nil
	}

	attachments, err := ResolveAttachments(store, refs, maxBytes)
	if err != nil {
		// 不返回 input，也不返回部分附件：让调用方整轮失败。
		return nil, err
	}

	// 容量一次算够：即便 len(input) == cap(input)，append 也会重新分配，
	// 但显式 make 让"这是新切片"成为实现保证而不是碰巧。
	out := make([]types.ContentBlock, 0, len(input)+len(attachments))
	out = append(out, input...)
	out = append(out, attachments...)
	return out, nil
}
