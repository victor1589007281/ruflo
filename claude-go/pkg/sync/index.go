package sync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const indexVersion = 1

// IndexPath 返回指定数据源的索引文件绝对路径。
func IndexPath(repo, source string) string {
	return filepath.Join(repo, "schema", source+"-index.json")
}

// LoadIndex 读取数据源索引文件，若不存在则返回空索引。
func LoadIndex(repo, source string) (*SourceIndex, error) {
	path := IndexPath(repo, source)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &SourceIndex{
				Version:  indexVersion,
				Source:   source,
				SyncedAt: time.Time{},
				Items:    make(map[string]IndexItem),
			}, nil
		}
		return nil, fmt.Errorf("读取索引 %s 失败: %w", path, err)
	}
	var idx SourceIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("解析索引 %s 失败: %w", path, err)
	}
	if idx.Items == nil {
		idx.Items = make(map[string]IndexItem)
	}
	return &idx, nil
}

// SaveIndex 将索引写回磁盘。
func SaveIndex(repo string, idx *SourceIndex) error {
	path := IndexPath(repo, idx.Source)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建索引目录失败: %w", err)
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化索引失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写入索引 %s 失败: %w", path, err)
	}
	return nil
}
