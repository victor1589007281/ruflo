// Package merge 曾计划提供并行编辑的三级合并阶梯 (AST 无交集 → 文本合并 →
// 语法感知 Mergiraf), 让并行 coder agent 编辑同一文件时不再被冲突键串行化。
//
// Deprecated: 本包**零引用, 且根本没被链进生产二进制**。计划中的接线
// (patch_apply.go 的 ApplyCRDT/ApplyMergiraf) 从未实现, 而其宿主 PatchApplier
// 本身也是零调用的死代码。并行编辑安全当前由冲突键串行化 + git worktree
// 隔离解决, 不涉及文本合并。已知缺陷:
//
//   - **CRDTDoc 不是 CRDT**: 只有一个 Text string 字段, 无元素 ID、无逻辑时钟、
//     无墓碑, 且 Merge 会**返回冲突错误**——conflict-free 类型按定义不会冲突。
//     实际语义是"仅当一侧改动时才接受的两路前后缀文本合并"(连 3 路都不是)。
//   - Mergiraf 从不调用 mergiraf: mergeWithMergiraf 自认是桩, 直接走 fallback,
//     故 IsAvailable 门控不了任何东西, 语法感知合并并不存在。
//   - MergeError 从无构造点, 于是 IsMergeConflict 恒返回 false。
//   - commonPrefix/commonSuffix 按**字节**比较并按字节下标切片, 对中文等多字节
//     文本会在 UTF-8 序列中间截断, 产出非法 UTF-8。
//
// 保留仅为留痕; 若将来真要做语法感知合并, 应按 rune 重写并直接对 pkg/graph
// 设计, 不要在本包上加功能。
//
// # 裁决 (H3-4, 方案 §21.5.5)
//
// 二选一的问题 —— "救活合并"还是"并发写显式拒绝" —— 已经判给后者, 实现落在
// pkg/tool/writeclaim.go (写占用) 与 Write/Edit 的申请点。判据不是"合并做不到",
// 而是**合并的语义正确性无法保证**: 两份对同一段代码的改动合出来的结果可以语法合法
// 而语义错误, 那种错比覆盖更难发现 —— 覆盖至少少了一大块东西, 合并出来的是一份
// "看起来都对"的文本。业界共识 (扇出读、串行写) 指向同一个答案。
//
// 所以本包的现状**不是"待救活的死代码", 而是"已裁决不做"**。它的价值只剩留痕:
// 上面对 CRDTDoc 的四条缺陷记录 (不是 CRDT / mergiraf 从不调用 / MergeError 从无
// 构造点 / 多字节截断) 说明了"照这个方向修"要付的代价, 那是本包唯一的产出物。
// 谁要重新打开这个方向, 应当先读那四条, 再读 pkg/tool/writeclaim.go 顶部的
// "为什么是拒绝而不是合并"。
package merge

import (
	"errors"
	"fmt"
	"strings"
)

// CRDTDoc holds a text document that can accept concurrent edits.
type CRDTDoc struct {
	Text string
}

// NewCRDTDoc 由既有文本构造文档。
//
// 注意：结构里只有一个 Text 字符串, 没有元素 ID / 逻辑时钟 / 墓碑 —— 它不是 CRDT
// (见包注释的裁决与四条缺陷记录)。
//
// NewCRDTDoc creates a CRDT doc from existing text.
func NewCRDTDoc(text string) *CRDTDoc {
	return &CRDTDoc{Text: text}
}

// Apply 在文本中定位 startMarker..endMarker 区间并整体替换为 replacement (含两端标记)。
//
// 注意：定位用的是**字节下标** (strings.Index + len), 多字节文本上若标记本身被截断
// 会切在 UTF-8 序列中间; 标记缺失时返回错误而不做任何改动。
//
// Apply applies an edit (find start/end markers, replace).
// It finds the markers in the current text and replaces the region between them (inclusive of markers).
func (d *CRDTDoc) Apply(startMarker, endMarker, replacement string) error {
	if startMarker == "" || endMarker == "" {
		return errors.New("startMarker and endMarker must be non-empty")
	}
	startIdx := strings.Index(d.Text, startMarker)
	if startIdx == -1 {
		return fmt.Errorf("startMarker %q not found", startMarker)
	}
	endIdx := strings.Index(d.Text[startIdx+len(startMarker):], endMarker)
	if endIdx == -1 {
		return fmt.Errorf("endMarker %q not found after startMarker", endMarker)
	}
	endIdx += startIdx + len(startMarker)

	before := d.Text[:startIdx]
	after := d.Text[endIdx+len(endMarker):]
	d.Text = before + replacement + after
	return nil
}

// Merge 尝试把 other 的文本并入 d; 两侧改动区重叠时返回冲突错误。
//
// 算法解读：
//
//	原理：只有两侧的**最终文本**可用 (没有编辑历史、没有基线), 所以合并退化成
//		"掐掉公共前后缀, 看中间剩下的两段能否直接拼接": 只要有一侧中间为空
//		(纯插入), 就把另一侧的中间段拼进去。
//	关键不变量：公共前后缀不重叠 —— 即 len(prefix)+len(suffix) 不得超过任一侧长度,
//		否则说明两侧在同一段上都有改动, 必须报冲突。这条不成立时会切出负长度区间。
//	代价与边界：按**字节**而非 rune 比较与切片, 中文/emoji 文本会在多字节序列中间
//		截断并产出非法 UTF-8; 且 diffRange 算出的行区间**根本没被用于判定**,
//		只有 leftDiffStart==-1 这一个信号被消费。
//	设计权衡：这不是 CRDT 的合并语义 —— 真正的 CRDT 按定义不会冲突, 而这里会返回
//		冲突错误。包注释已裁决该方向不做, 保留仅为留痕。
//
// Merge merges two CRDT docs by applying all edits from `other` onto `d`.
// For simplicity: if edits touch non-overlapping regions, apply both.
// If overlapping, return an error indicating conflict.
func (d *CRDTDoc) Merge(other *CRDTDoc) error {
	if d.Text == other.Text {
		return nil
	}

	// Simple line-based diff heuristic.
	leftLines := strings.Split(d.Text, "\n")
	rightLines := strings.Split(other.Text, "\n")

	// Find differing line ranges.
	leftDiffStart, _ := diffRange(leftLines, rightLines)
	if leftDiffStart == -1 {
		// No diff detected (shouldn't happen because texts differ, but handle gracefully).
		d.Text = other.Text
		return nil
	}

	// Check for overlap: if both changed the same lines, it's a conflict.
	// Since we only have the final texts, we approximate overlap by checking
	// whether the common prefix + suffix still results in a clean merge.
	merged, ok := threeWayMergeStrings(d.Text, other.Text)
	if !ok {
		return errors.New("merge conflict: overlapping edits detected")
	}
	d.Text = merged
	return nil
}

// diffRange 返回 a、b 之间第一段连续差异行的起止行号; 完全相同返回 (-1, -1)。
//
// 注意：返回值只被 Merge 当作"有没有差异"使用, 算出的行区间并未参与冲突判定。
//
// diffRange returns the start and end indices of the first contiguous block of
// differing lines between a and b. Returns -1, -1 if they are identical.
func diffRange(a, b []string) (int, int) {
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	start := -1
	for i := 0; i < minLen; i++ {
		if a[i] != b[i] {
			start = i
			break
		}
	}
	if start == -1 {
		if len(a) == len(b) {
			return -1, -1
		}
		start = minLen
	}
	end := start
	for i := minLen - 1; i >= start; i-- {
		if a[i] != b[i] {
			end = i
			break
		}
	}
	return start, end
}

// threeWayMergeStrings 对共享公共前后缀的两串做一次最朴素的合并; 改动区重叠时返回 false。
//
// 算法解读：
//
//	原理：掐掉公共前缀 prefix 与公共后缀 suffix, 中间剩下的 midLeft / midRight 就是
//		两侧各自的改动; 只要**至少一侧为空** (即那次改动是纯插入), 就拼成
//		prefix + midLeft + midRight + suffix。
//	关键不变量：prefix 与 suffix 互不重叠, 即 len(prefix)+len(suffix) 不超过任一侧长度。
//		这条是"两侧改的是不同位置"的机械判据; 一旦越界, 后面的切片下标会反向,
//		所以必须先判后切。
//	代价与边界：按字节比较, 多字节字符会被切在序列中间; 且只接受"一侧为空"这一种
//		情形 —— 两侧都在不同位置各改一处 (最常见的真并行编辑) 会被判为冲突。
//	设计权衡：不做逐行三路合并 (那需要共同祖先基线, 而这里只有两份最终文本)。
//
// threeWayMergeStrings attempts a trivial merge of two strings that share a common
// prefix and suffix. If the changed regions overlap, it returns false.
func threeWayMergeStrings(left, right string) (string, bool) {
	if left == right {
		return left, true
	}
	prefix := commonPrefix(left, right)
	suffix := commonSuffix(left, right)

	// If suffix is longer than what remains after prefix, regions overlap.
	if len(prefix)+len(suffix) > len(left) || len(prefix)+len(suffix) > len(right) {
		return "", false
	}

	midLeft := left[len(prefix) : len(left)-len(suffix)]
	midRight := right[len(prefix) : len(right)-len(suffix)]

	// Non-overlapping means one side's middle is empty (pure insertion at edge)
	// or both sides have changes but they don't interfere. For simplicity we
	// accept if at least one middle is empty, otherwise treat as conflict.
	if midLeft == "" || midRight == "" {
		return prefix + midLeft + midRight + suffix, true
	}
	return "", false
}

// commonPrefix 返回 a 与 b 的最长公共前缀 (按字节比较)。
//
// 注意：逐字节推进, 多字节字符可能被切在 UTF-8 序列中间 (见包注释的缺陷记录)。
func commonPrefix(a, b string) string {
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	i := 0
	for i < minLen && a[i] == b[i] {
		i++
	}
	return a[:i]
}

// commonSuffix 返回 a 与 b 的最长公共后缀 (从尾到头按字节比较)。
//
// 注意：同 commonPrefix, 以字节为界, 多字节字符可能被截断。
func commonSuffix(a, b string) string {
	i, j := len(a)-1, len(b)-1
	for i >= 0 && j >= 0 && a[i] == b[j] {
		i--
		j--
	}
	return a[i+1:]
}
