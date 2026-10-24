package merge

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Mergiraf wraps a syntax-aware 3-way merge tool (mergiraf) with a fallback.
type Mergiraf struct{}

// NewMergiraf 创建 Mergiraf 合并助手 (无状态, 不持有外部进程句柄)。
// NewMergiraf creates a new Mergiraf merge helper.
func NewMergiraf() *Mergiraf {
	return &Mergiraf{}
}

// IsAvailable 报告 PATH 里能否找到 mergiraf 二进制 (只看存在性, 不试运行)。
// IsAvailable checks if the mergiraf binary is in PATH.
func (m *Mergiraf) IsAvailable() bool {
	_, err := exec.LookPath("mergiraf")
	return err == nil
}

// Merge 执行三方合并 (base → left / base → right): mergiraf 可用时走它, 否则降级为逐行合并。
// Merge performs a 3-way merge: base -> left and base -> right.
// Returns merged text or error. Falls back to a simple line-based merge
// when mergiraf is not available.
func (m *Mergiraf) Merge(base, left, right string) (string, error) {
	if m.IsAvailable() {
		return m.mergeWithMergiraf(base, left, right)
	}
	return m.fallbackMerge(base, left, right)
}

// mergeWithMergiraf 名义上的 mergiraf 集成入口, **当前是桩**: 直接转 fallbackMerge。
//
// 注意：调用方 Merge 已经判过 IsAvailable, 但这里并没有真的去执行 mergiraf 二进制 ——
// 于是"装了 mergiraf"与"没装"会得到完全相同的逐行合并结果, IsAvailable 目前不影响
// 任何行为。这是刻意保留的保守实现 (见函数内原注释), 不是漏接线。
func (m *Mergiraf) mergeWithMergiraf(base, left, right string) (string, error) {
	// Real mergiraf integration can be added later.
	// For now, fall through to the fallback to keep behaviour predictable.
	return m.fallbackMerge(base, left, right)
}

// fallbackMerge 逐行三方合并: 两侧都没改就用 base, 只有一侧改就用改后的, 两侧都改则报冲突。
// fallbackMerge performs a simple line-based 3-way merge.
// For each conflicting hunk it returns an error.
//
// 算法解读：
//
//	原理：以 base 为基准**按行号对齐**左右两侧 (i/j/k 三个游标同步前进), 每步用
//	  ll==bl 与 rl==bl 判断哪一侧发生了修改; 两侧相对 base 都改了即视为冲突。
//	关键不变量：三侧游标必须在每个分支里**同步推进**, 否则从该行起整体错位, 会把
//	  "其实没冲突"判成"后面处处冲突"。这是该实现成立的唯一前提。
//	代价与边界：O(最长一侧行数) 时间、O(结果长度) 内存。它**不是基于 diff 的合并**:
//	  任一侧出现插入/删除行, 行号就对不上了 —— 后续行会被误判为冲突, 或把改动错位
//	  拼进结果。所以只适用于"三份行数相同、仅逐行内容不同"的场景。
//	设计权衡：宁可保守报错也不猜。真正的语法感知合并留给 mergiraf; 这里的定位是
//	  "没有外部工具时也不静默产出错误的合并结果"。
func (m *Mergiraf) fallbackMerge(base, left, right string) (string, error) {
	baseLines := strings.Split(base, "\n")
	leftLines := strings.Split(left, "\n")
	rightLines := strings.Split(right, "\n")

	// Simple 3-way merge: walk through lines, detect changes in left/right.
	// If both left and right changed the same line relative to base, it's a conflict.
	var result []string
	i, j, k := 0, 0, 0
	for i < len(baseLines) || j < len(leftLines) || k < len(rightLines) {
		bl := ""
		if i < len(baseLines) {
			bl = baseLines[i]
		}
		ll := ""
		if j < len(leftLines) {
			ll = leftLines[j]
		}
		rl := ""
		if k < len(rightLines) {
			rl = rightLines[k]
		}

		// No change in either branch.
		if ll == bl && rl == bl {
			if i < len(baseLines) {
				result = append(result, bl)
			}
			i++
			j++
			k++
			continue
		}

		// Changed only in left.
		if ll != bl && rl == bl {
			if j < len(leftLines) {
				result = append(result, ll)
			}
			i++
			j++
			if k < len(rightLines) {
				k++
			}
			continue
		}

		// Changed only in right.
		if rl != bl && ll == bl {
			if k < len(rightLines) {
				result = append(result, rl)
			}
			i++
			if j < len(leftLines) {
				j++
			}
			k++
			continue
		}

		// Both changed relative to base — conflict.
		return "", fmt.Errorf("merge conflict at base line %d: left=%q right=%q", i+1, ll, rl)
	}

	return strings.Join(result, "\n"), nil
}

// MergeError indicates a merge conflict.
type MergeError struct {
	Line int
	Left string
	Right string
}

// Error 实现 error 接口: 只输出冲突所在的行号。
func (e *MergeError) Error() string {
	return fmt.Sprintf("merge conflict at line %d", e.Line)
}

// IsMergeConflict 报告错误是否代表一次合并冲突 (按 *MergeError 做类型断言)。
//
// 注意：fallbackMerge 的冲突路径返回的是 fmt.Errorf 而不是 *MergeError, 所以对本
// 实现实际产生的冲突, 这个判据恒为 false —— 调用方不能靠它区分"冲突"与"其他失败"。
// IsMergeConflict returns true if the error represents a merge conflict.
func IsMergeConflict(err error) bool {
	var me *MergeError
	return errors.As(err, &me)
}
