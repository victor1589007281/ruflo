//go:build !unix

package tools

import "fmt"

func diskFreeBytes(path string) (uint64, error) {
	_ = path
	return 0, fmt.Errorf("disk free space: not supported on this platform")
}
