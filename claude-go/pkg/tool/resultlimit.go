// resultlimit.go — 工具结果的字节上限（Moqi R-H9 的 MaxResultBytes）。
//
// # 与 compactToolResultContent 的分工
//
// 两个上限解决的不是同一个问题：
//
//	MaxResultBytes（本文件）  工具**自己声明**的契约："我的输出超过 N 字节就是
//	                          异常，别把整段灌进上下文"。它是按工具的知识写的，
//	                          比如"一条记忆正文最多 8KB"。
//	compactToolResultContent 引擎的**兜底**预算（默认 24000 字符）：任何工具都
//	                          可能因为异常输入吐出巨量文本，这道闸不看工具是谁。
//
// 顺序必须是先本文件、后 compact：工具的声明比通用兜底更严格也更了解内容，
// 让它先说了算；若先跑通用压缩，工具声明的阈值就可能永远够不到（压缩已经把
// 内容降到 24000 字符以下，而工具的阈值是 8KB —— 那样 8KB 的契约实际上从未生效）。
//
// # 为什么"截断"必须带引用
//
// R-H9 的验收原话是"超大结果被截断并带引用"。只截断不给引用，等于把数据**丢掉**：
// 模型知道自己少看了东西，却没有任何办法把它取回来（重跑工具可能产生不同结果，
// 或者有副作用）。所以这里把完整内容按内容哈希落盘，并在截断处写清路径与字节数。
//
// # 落盘失败怎么办
//
// 明说"无全文留存"，**不**假装有。写一个不存在的路径比不写更坏：模型会去 Read
// 一个不存在的文件，得到一条与真正问题无关的错误。
package tool

import (
	"encoding/json"
	"fmt"

	"github.com/anthropic/claude-go/pkg/textutil"
)

// truncationSuffixFmt 是截断处的标记。用 %d/%d（保留字节/原始字节）而不是只说
// "已截断"：模型据此能判断"少了多少"，进而决定是接受摘要还是去读全文。
const truncationSuffixFmt = "\n\n...[tool_result 已截断: %d/%d 字节, 上限 %d]"

// applyResultByteLimit 把结果降到工具声明的字节上限内。
//
// 三条不变量：
//
//  1. maxBytes <= 0 或未超限 → 返回原串，**逐字节**不变（存量工具不受影响）。
//  2. 超限 → 完整内容落 artifact，返回"保留前缀 + 截断标记 + 引用"。落盘失败时
//     标记里明说没有全文。
//  3. 切点永远在 UTF-8 rune 边界上（textutil.TruncateBytes 保证），否则截出来的
//     是非法 UTF-8，会一路走到转录、模型、前端，最后表现为一个乱码方块。
//
// **上限是对整条 tool_result 的**，不是只对正文：标记与 artifact 引用都算在里面。
// 只卡正文会让"声明 400 字节"的工具实际吐出 549 字节（标记 + 路径），而调用方
// 按声明值算出的请求体预算就总是被突破 —— 上限一旦不覆盖全部输出，它就不是上限。
//
// 参数 input 只为对称性与将来按入参调整上限留口，当前不参与判定。
func applyResultByteLimit(toolName string, input json.RawMessage, content string, tctx *ToolContext, maxBytes int) string {
	_ = input
	if maxBytes <= 0 || len(content) <= maxBytes {
		return content
	}

	// 先落盘：引用行的长度决定了还能给正文留多少字节，所以必须先知道路径。
	ref := "\nfull_artifact: (落盘失败, 本次无全文留存)\n"
	if path := writeToolResultArtifact(toolName, content, tctx); path != "" {
		ref = "\nfull_artifact: " + path + "\n"
	}
	if len(ref) >= maxBytes {
		// 连引用行都放不下：只给纯前缀。此时确实没有可回取的坐标，但至少
		// 没有超出声明上限，也不会给出一个指向不存在文件的路径。
		return textutil.TruncateBytes(content, maxBytes, "")
	}

	// 标记里有两个数字会随 keep 变化。用**上界**（保留字节数不会超过总字节数）
	// 先算一次长度，实际取值只会更短，于是最终结果一定不超限 —— 比循环收敛简单，
	// 也比"先拼再截"安全（后者可能把引用行截断成半个路径）。
	markerUpper := fmt.Sprintf(truncationSuffixFmt, len(content), len(content), maxBytes)
	keep := maxBytes - len(markerUpper) - len(ref)
	if keep <= 0 {
		// 放不下标记：放弃诊断信息，保住可回取的引用。数据可回取比"知道少了多少"重要。
		head := textutil.TruncateBytes(content, maxBytes-len(ref), "")
		return head + ref
	}
	head := textutil.TruncateBytes(content, keep, "")
	b := make([]byte, 0, maxBytes)
	b = append(b, head...)
	b = append(b, fmt.Sprintf(truncationSuffixFmt, len(head), len(content), maxBytes)...)
	b = append(b, ref...)
	return string(b)
}
