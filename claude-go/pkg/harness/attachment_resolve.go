// attachment_resolve.go — AttachmentRef 到协议内容块的解析（Moqi R-H5/R-H11）。
//
// # 要解决的问题
//
// attachments.go 只解决"引用怎么来"：hash 由内容算、字节落盘、转录里只有元数据。
// 但模型请求需要的是**协议内容块**（image/document/text）。这段转换如果散落在
// 业务层，会出现两种典型错误：
//
//   - 取不到 blob 时静默跳过。表现是"用户上传了病例照片，模型客气地回了一句没看图的话"，
//     而调用方以为请求已经发出去了。这里把"取不到"变成 typed error，宁可整轮失败，
//     也不让模型带着一份缺了附件的输入回答。
//   - 图省事把 PDF/音频塞成 text 块。模型看到的是一段 base64 或一句"见附件"，
//     同样没有任何信号说明它根本没读到内容。协议不支持的形状必须显式失败。
//
// # 预算
//
// 预算是**总字节**上限，由调用方给（<=0 = 不限）。图片与 PDF 超限直接报错：
// 截断的图片不是"小一点的图片"，而是坏图 —— 模型会看到乱码的像素或直接请求失败，
// 比明确报错更难排查。text 附件超限可以截断，但截断处必须留下原始字节数，
// 否则模型会以为它看到的是全文。
//
// # 为什么不缓存到跨轮
//
// 这里只做**一次解析**内的去重（同一 hash 被多个引用指向时只读一次 store）。
// 解析结果不缓存到包级：解析结果里含 base64，长期驻留内存正是 R-H5 要避免的
// "base64 常驻"。每次发请求时重新解析，代价是一次磁盘读，换来的是会话历史里
// 永远只有引用。
package harness

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/types"
)

// 解析失败的三类哨兵错误。业务层用 errors.Is 分流：
//
//   - ErrAttachmentUnresolved：引用指向的内容取不到（用户该重传，或存储故障该重试）；
//   - ErrAttachmentBudget：内容太大（该让用户换小文件，或调高预算）；
//   - ErrAttachmentKindUnsupported：协议表达不了这种附件（该换模型或先转格式）。
//
// 三者都**不是**可重试的模型错误，混成一个字符串会让业务层无法给出正确的用户提示。
var (
	ErrAttachmentUnresolved      = errors.New("harness: 附件内容无法解析")
	ErrAttachmentBudget          = errors.New("harness: 附件超出字节预算")
	ErrAttachmentKindUnsupported = errors.New("harness: 附件种类无法表达为协议内容块")
)

// AttachmentResolveError 是附件解析失败的结构化错误。
//
// 字段只带**定位信息**（ID / hash / 种类 / 大小），绝不带正文：错误会被写日志、
// 回传前端，把附件字节（或 base64）塞进错误信息等于把 R-H5 的"转录不内联内容"
// 从另一条路绕开了。
type AttachmentResolveError struct {
	// Op 是失败阶段："store"（存储为空）、"get"（取不到 blob）、
	// "budget"（超预算）、"kind"（种类或媒体类型表达不了）。
	Op           string
	AttachmentID string
	BlobHash     string
	Kind         string
	// Size 是本附件的字节数；Used 是此前已计入预算的字节数；Limit 是总上限。
	// 三者只在 Op == "budget" 时有意义。
	Size  int64
	Used  int64
	Limit int64
	// Err 是底层原因（如 ErrBlobNotFound）。可为 nil。
	Err error
}

// Error 组装可读信息。附件 ID 与 hash 一定出现：缺附件时首先要能定位是哪一个。
func (e *AttachmentResolveError) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("harness: 解析附件 ")
	fmt.Fprintf(&b, "id=%q hash=%q kind=%q", e.AttachmentID, e.BlobHash, e.Kind)
	switch e.Op {
	case "store":
		b.WriteString(": blob 存储不可用")
	case "get":
		b.WriteString(": 取不到 blob 内容")
	case "budget":
		fmt.Fprintf(&b, ": 超出字节预算（本附件 %d 字节, 已用 %d, 上限 %d）", e.Size, e.Used, e.Limit)
	case "kind":
		b.WriteString(": 种类无法表达为协议内容块")
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap 同时暴露底层错误与对应哨兵，业务层可以同时判断
// "是引用失效还是存储坏了"（errors.Is(err, ErrBlobNotFound)）与
// "该走哪条处置路径"（errors.Is(err, ErrAttachmentUnresolved)）。
func (e *AttachmentResolveError) Unwrap() []error {
	out := make([]error, 0, 2)
	if e.Err != nil {
		out = append(out, e.Err)
	}
	switch e.Op {
	case "store", "get":
		out = append(out, ErrAttachmentUnresolved)
	case "budget":
		out = append(out, ErrAttachmentBudget)
	case "kind":
		out = append(out, ErrAttachmentKindUnsupported)
	}
	return out
}

// ResolveAttachments 把附件引用解析成协议内容块。
//
// 顺序严格等于 refs 的顺序（同一 hash 多次引用也各出一块），因为块顺序是
// 请求字节的一部分：用 map 迭代顺序会让同一输入在不同进程里发出不同请求，
// 既破坏可复现性，也会让上游 prompt cache 无谓失效。
//
// maxBytes 是本轮附件正文的**总字节**上限（<=0 = 不限）。计数口径是 blob 的原始
// 字节数，不是 base64 后的长度：调用方给预算时看的是"用户传了多大的文件"，
// 而 base64 膨胀对所有附件是同一个比例。
//
// refs 为空时返回 (nil, nil) —— 调用方据此完全跳过附件路径，请求体与没有附件
// 功能时逐字节一致。
func ResolveAttachments(store BlobStore, refs []AttachmentRef, maxBytes int64) ([]types.ContentBlock, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if store == nil {
		// 有引用却没有存储：这是装配错误，不是"没有附件"。用第一条引用定位，
		// 返回 nil 块 —— 绝不能返回空切片假装附件都被处理过了。
		first := refs[0]
		return nil, &AttachmentResolveError{
			Op:           "store",
			AttachmentID: first.ID,
			BlobHash:     first.BlobHash,
			Kind:         first.Kind,
			Err:          errors.New("blob 存储为 nil"),
		}
	}

	// 缓存只存原始字节，不含编码结果：text 的截断量取决于该引用在预算里的位置，
	// 同 hash 的两次引用可能得到不同的正文，但取字节这一步可以共享。
	cache := make(map[string][]byte, len(refs))
	blocks := make([]types.ContentBlock, 0, len(refs))
	var used int64

	for _, ref := range refs {
		data, ok := cache[ref.BlobHash]
		if !ok {
			got, err := store.Get(ref.BlobHash)
			if err != nil {
				return nil, &AttachmentResolveError{
					Op:           "get",
					AttachmentID: ref.ID,
					BlobHash:     ref.BlobHash,
					Kind:         ref.Kind,
					Err:          err,
				}
			}
			// Get 返回 (nil, nil) 是坏实现，但它的后果是"产出一个空的 image 块"，
			// 与静默跳过同属"模型看不到图而调用方以为发出去了"。按取不到处理。
			if got == nil {
				return nil, &AttachmentResolveError{
					Op:           "get",
					AttachmentID: ref.ID,
					BlobHash:     ref.BlobHash,
					Kind:         ref.Kind,
					Err:          errors.New("blob 存储返回了 nil 字节"),
				}
			}
			data = got
			cache[ref.BlobHash] = data
		}

		block, consumed, err := attachmentBlock(ref, data, maxBytes, used)
		if err != nil {
			return nil, err
		}
		used += consumed
		blocks = append(blocks, block)
	}
	return blocks, nil
}

// attachmentBlock 把一个已在手的附件转成协议块，并返回计入预算的字节数。
//
// data 来自缓存，可能被同 hash 的其它引用共享；这里只读它（切片与 base64 编码
// 都不改原字节），所以共享是安全的。
func attachmentBlock(ref AttachmentRef, data []byte, maxBytes, used int64) (types.ContentBlock, int64, error) {
	switch ref.Kind {
	case AttachmentImage, AttachmentPDF:
		size := int64(len(data))
		// 超限直接失败。截断图片/PDF 会产出一个看似合法、实际损坏的块：
		// 模型要么看到乱码，要么上游直接 400，两种都比"用户被告知文件太大"更糟。
		if maxBytes > 0 && used+size > maxBytes {
			return types.ContentBlock{}, 0, &AttachmentResolveError{
				Op:           "budget",
				AttachmentID: ref.ID,
				BlobHash:     ref.BlobHash,
				Kind:         ref.Kind,
				Size:         size,
				Used:         used,
				Limit:        maxBytes,
			}
		}
		mediaType, err := protocolMediaType(ref, data)
		if err != nil {
			return types.ContentBlock{}, 0, err
		}
		blockType := types.ContentBlockImage
		if ref.Kind == AttachmentPDF {
			blockType = types.ContentBlockDocument
		}
		return types.ContentBlock{
			Type: blockType,
			Source: &types.MediaSource{
				Type:      "base64",
				MediaType: mediaType,
				Data:      base64.StdEncoding.EncodeToString(data),
			},
		}, size, nil

	case AttachmentAudio:
		// types.ContentBlock 没有 audio 块，modality 表里也没有任何模型声明支持音频。
		// 塞成 text 就是在骗模型和调用方：必须显式失败。
		return types.ContentBlock{}, 0, &AttachmentResolveError{
			Op:           "kind",
			AttachmentID: ref.ID,
			BlobHash:     ref.BlobHash,
			Kind:         ref.Kind,
			Size:         int64(len(data)),
			Err:          errors.New("协议没有 audio 内容块"),
		}

	case AttachmentText:
		return attachmentTextBlock(ref, data, maxBytes, used)

	default:
		return types.ContentBlock{}, 0, &AttachmentResolveError{
			Op:           "kind",
			AttachmentID: ref.ID,
			BlobHash:     ref.BlobHash,
			Kind:         ref.Kind,
			Size:         int64(len(data)),
			Err:          fmt.Errorf("未知附件种类 %q", ref.Kind),
		}
	}
}

// attachmentTextBlock 把文本附件内联成 text 块，必要时按剩余预算截断。
//
// 文本可以截断而图片不行，因为截断的文本仍然"可读"；但截断必须留下标记与
// 原始字节数，否则模型会把它当成全文，回答里出现的"文中没有提到"就成了误导。
func attachmentTextBlock(ref AttachmentRef, data []byte, maxBytes, used int64) (types.ContentBlock, int64, error) {
	keep := int64(len(data))
	truncated := false
	if maxBytes > 0 {
		remain := maxBytes - used
		if remain < 0 {
			remain = 0
		}
		if remain < keep {
			keep = remain
			truncated = true
		}
	}
	// 截断点必须落在 UTF-8 字符边界上：切在半字符中间会产出非法 UTF-8，
	// 上游 JSON 编码可能报错，或者模型看到替换字符。
	if keep < int64(len(data)) {
		for keep > 0 && !utf8.RuneStart(data[keep]) {
			keep--
		}
	}

	body := data[:keep]
	var b strings.Builder
	// 来源标注让模型（以及回看转录的人）知道这段文字是附件而不是用户现打的，
	// 并且可以按 ID 反查原文件。
	if ref.Name != "" {
		fmt.Fprintf(&b, "[附件 %s (ID: %s)]\n", ref.Name, ref.ID)
	} else {
		fmt.Fprintf(&b, "[附件 (ID: %s)]\n", ref.ID)
	}
	b.Write(body)
	if truncated {
		fmt.Fprintf(&b, "\n\n[附件已截断：原始 %d 字节，因总预算 %d 字节仅保留前 %d 字节]",
			len(data), maxBytes, keep)
	}
	return types.ContentBlock{Type: types.ContentBlockText, Text: b.String()}, keep, nil
}

// protocolMediaType 决定内容块的 media_type。
//
// MIME 为空时按字节嗅探而不是默认一个常见值：默认 image/png 对 JPEG 会得到
// 上游 400 或模型解析失败，而嗅探至少能在同一份字节上稳定复现。
func protocolMediaType(ref AttachmentRef, data []byte) (string, error) {
	mediaType := strings.TrimSpace(ref.MIME)
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	switch ref.Kind {
	case AttachmentImage:
		if !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
			return "", &AttachmentResolveError{
				Op:           "kind",
				AttachmentID: ref.ID,
				BlobHash:     ref.BlobHash,
				Kind:         ref.Kind,
				Size:         int64(len(data)),
				Err:          fmt.Errorf("image 附件需要 image/* 媒体类型, 实际为 %q", mediaType),
			}
		}
		return mediaType, nil
	case AttachmentPDF:
		if !strings.EqualFold(mediaType, "application/pdf") {
			return "", &AttachmentResolveError{
				Op:           "kind",
				AttachmentID: ref.ID,
				BlobHash:     ref.BlobHash,
				Kind:         ref.Kind,
				Size:         int64(len(data)),
				Err:          fmt.Errorf("pdf 附件的媒体类型必须是 application/pdf, 实际为 %q", mediaType),
			}
		}
		return "application/pdf", nil
	}
	// 走到这里说明调用方绕过了上面按 Kind 的分派；保守失败而不是猜一个类型。
	return "", &AttachmentResolveError{
		Op:           "kind",
		AttachmentID: ref.ID,
		BlobHash:     ref.BlobHash,
		Kind:         ref.Kind,
		Size:         int64(len(data)),
		Err:          fmt.Errorf("无法为种类 %q 确定媒体类型", ref.Kind),
	}
}
