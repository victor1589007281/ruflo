package merge

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Mergiraf wraps a syntax-aware 3-way merge tool (mergiraf) with a fallback.
type Mergiraf struct{}

// NewMergiraf creates a new Mergiraf merge helper.
func NewMergiraf() *Mergiraf {
	return &Mergiraf{}
}

// IsAvailable checks if the mergiraf binary is in PATH.
func (m *Mergiraf) IsAvailable() bool {
	_, err := exec.LookPath("mergiraf")
	return err == nil
}

// Merge performs a 3-way merge: base -> left and base -> right.
// Returns merged text or error. Falls back to a simple line-based merge
// when mergiraf is not available.
func (m *Mergiraf) Merge(base, left, right string) (string, error) {
	if m.IsAvailable() {
		return m.mergeWithMergiraf(base, left, right)
	}
	return m.fallbackMerge(base, left, right)
}

func (m *Mergiraf) mergeWithMergiraf(base, left, right string) (string, error) {
	// Real mergiraf integration can be added later.
	// For now, fall through to the fallback to keep behaviour predictable.
	return m.fallbackMerge(base, left, right)
}

// fallbackMerge performs a simple line-based 3-way merge.
// For each conflicting hunk it returns an error.
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

func (e *MergeError) Error() string {
	return fmt.Sprintf("merge conflict at line %d", e.Line)
}

// IsMergeConflict returns true if the error represents a merge conflict.
func IsMergeConflict(err error) bool {
	var me *MergeError
	return errors.As(err, &me)
}
