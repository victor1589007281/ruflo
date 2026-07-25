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

// NewCRDTDoc creates a CRDT doc from existing text.
func NewCRDTDoc(text string) *CRDTDoc {
	return &CRDTDoc{Text: text}
}

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

func commonSuffix(a, b string) string {
	i, j := len(a)-1, len(b)-1
	for i >= 0 && j >= 0 && a[i] == b[j] {
		i--
		j--
	}
	return a[i+1:]
}
