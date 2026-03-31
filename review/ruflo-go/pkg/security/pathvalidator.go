package security

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PathValidator constrains filesystem access.
type PathValidator struct {
	allowedRoots []string
}

// NewPathValidator returns a validator that only allows paths under roots (cleaned).
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

// Validate resolves the path, ensures it stays under an allowed root, and optionally rejects symlinks.
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
