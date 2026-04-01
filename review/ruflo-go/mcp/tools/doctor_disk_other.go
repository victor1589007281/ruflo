//go:build !unix

package tools

import "fmt"

// 本文件（非 Unix 构建）：diskFreeBytes 桩实现，避免在非类 Unix 平台链接 syscall.Statfs。
//
// 设计思路：doctor_check 仍可统一调用 diskFreeBytes；本实现明确返回不支持错误，由上层将 disk 检查标为失败并展示原因。

// diskFreeBytes 在非 Unix 平台不支持查询磁盘剩余空间，忽略 path，返回固定错误信息。
func diskFreeBytes(path string) (uint64, error) {
	_ = path
	return 0, fmt.Errorf("disk free space: not supported on this platform")
}
