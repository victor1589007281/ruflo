// 路径安全验证（pathvalidator.go）
//
// 设计思路：防止路径穿越（../）与任意文件系统访问——仅允许访问预先配置的「根目录集合」下的路径。
// 通过 Abs + Clean 得到规范绝对路径后，用 filepath.Rel(root, path) 的代数性质判定是否位于 root 子树内。
//
// 整体架构：PathValidator 持有多个 allowedRoots；Validate 对每个 root 尝试 Rel，命中则可选执行符号链接分量检查（noSymlink），
// 避免通过 symlink 将逻辑路径「跳出」允许区。
package security

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PathValidator 基于白名单根目录集合约束所有受控文件访问。
type PathValidator struct {
	allowedRoots []string // 允许的根路径切片，构造时已 filepath.Clean；若为空则无法通过任何 Validate
}

// NewPathValidator 过滤空根路径后，对其余根执行 filepath.Clean 再保存，避免尾随分隔符等导致的 Rel 误判。
func NewPathValidator(roots ...string) *PathValidator {
	clean := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" {
			continue
		}
		clean = append(clean, filepath.Clean(r))
	}
	return &PathValidator{allowedRoots: clean}
}

// Validate 核心算法：abs := Abs(path)，clean := Clean(abs)。对每个 root，rel := Rel(root, clean)。
// 若 rel == "." 表示与 root 同一路径；若 rel 不以 ".." 开头，则在 Unix/Windows 语义下表示 clean 位于 root 目录树内（含子目录）。
// 任一 root 命中即返回 clean；rejectSymlinks 为 true 时额外调用 noSymlink 自叶向根 Lstat，任一分量为 symlink 则失败。
func (v *PathValidator) Validate(path string, rejectSymlinks bool) (string, error) {
	if len(v.allowedRoots) == 0 {
		return "", errors.New("security: no allowed roots configured")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(abs)
	for _, root := range v.allowedRoots {
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			continue
		}
		if rel == "." || !strings.HasPrefix(rel, "..") {
			if rejectSymlinks {
				if err := v.noSymlink(clean); err != nil {
					return "", err
				}
			}
			return clean, nil
		}
	}
	return "", fmt.Errorf("security: path outside allowed directories: %s", path)
}

// noSymlink 自深向浅遍历路径分量：对每个 cur 使用 Lstat（不跟随链接）；若为 symlink 则拒绝；
// 若某层不存在（ErrNotExist）则提前结束并视为通过（路径尚未完全创建场景）。
func (v *PathValidator) noSymlink(p string) error {
	for cur := p; cur != filepath.Dir(cur); cur = filepath.Dir(cur) {
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("security: symlink in path: %s", cur)
		}
	}
	return nil
}

// isSubpathRel 对 filepath.Rel 的结果做显式子路径判定："." 为根自身；".." 或 "../" 前缀或路径段中含 "/../" 均视为越界。
// 当前仓库内未引用，保留为可读性工具函数。
func isSubpathRel(rel string) bool {
	if rel == "." {
		return true
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	return !strings.Contains(rel, "/../")
}
