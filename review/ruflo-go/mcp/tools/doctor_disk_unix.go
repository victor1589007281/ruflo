//go:build unix

package tools

import "syscall"

// 本文件（Unix 构建）：为 doctor_check 提供路径所在文件系统的可用字节数查询。
//
// 设计思路：通过 syscall.Statfs 读取块大小与可用块数（Bavail），相乘得到近似可用空间，供磁盘检查项展示。

// diskFreeBytes 对 path 所在挂载点执行 Statfs，返回可用字节数（Bavail * Bsize）；失败时返回 0 与底层错误。
func diskFreeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
