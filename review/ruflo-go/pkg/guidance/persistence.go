package guidance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// 本文件：PolicyBundle 磁盘持久化。Save 采用临时文件写入、fsync、close 后 rename 实现近似原子替换；Load 反序列化 JSON。

// Save 将 bundle 以缩进 JSON 写入 path：创建临时文件、Write、Sync、Close、Rename，失败时清理临时路径。
func Save(bundle PolicyBundle, path string) error {
	if path == "" {
		return fmt.Errorf("guidance: empty path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".policy-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// Load 从 path 读取 JSON 并反序列化为 PolicyBundle。
func Load(path string) (PolicyBundle, error) {
	var b PolicyBundle
	if path == "" {
		return b, fmt.Errorf("guidance: empty path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	return b, nil
}
