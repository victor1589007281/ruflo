// shard.go — 分片管理与自动检测。
package codeintel

import (
	"os"
	"path/filepath"
	"strings"
)

// AutoShardConfig 自动分片配置。
type AutoShardConfig struct {
	MaxFilesPerShard int      // 每个分片最大文件数
	MinFilesPerShard int      // 每个分片最小文件数
	LangFilter       []string // 语言过滤
	TopLevelDirs     []string // 优先作为分片根的一级目录
}

// DefaultAutoShardConfig 返回默认自动分片配置。
func DefaultAutoShardConfig() *AutoShardConfig {
	return &AutoShardConfig{
		MaxFilesPerShard: 5000,
		MinFilesPerShard: 50,
		LangFilter:       []string{".c", ".cc", ".cpp", ".h", ".hpp", ".go", ".rs", ".java", ".py", ".js", ".ts"},
		TopLevelDirs:     []string{},
	}
}

// DetectShards 自动检测仓库分片。
// 策略：按顶层目录分组，超过 MaxFilesPerShard 时细分。
func DetectShards(repoPath string, cfg *AutoShardConfig) ([]ShardConfig, error) {
	if cfg == nil {
		cfg = DefaultAutoShardConfig()
	}

	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return nil, err
	}

	var shards []ShardConfig
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// 跳过常见非代码目录
		if isSkippedDir(name) {
			continue
		}

		rootDir := name + "/"
		count, err := countCodeFiles(filepath.Join(repoPath, name), cfg.LangFilter)
		if err != nil {
			continue
		}
		if count == 0 {
			continue
		}
		if count < cfg.MinFilesPerShard {
			// 文件太少，归到 "misc" 或其他大类
			continue
		}
		if count > cfg.MaxFilesPerShard {
			// 过大，需要子分片（简化：按二级目录分）
			subs, err := subdivideShard(repoPath, name, cfg)
			if err == nil && len(subs) > 0 {
				shards = append(shards, subs...)
				continue
			}
		}
		shards = append(shards, ShardConfig{
			Name:     name,
			RootDirs: []string{rootDir},
			MaxFiles: cfg.MaxFilesPerShard,
		})
	}

	return shards, nil
}

// subdivideShard 将过大的分片按二级目录细分。
func subdivideShard(repoPath, topDir string, cfg *AutoShardConfig) ([]ShardConfig, error) {
	path := filepath.Join(repoPath, topDir)
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	var shards []ShardConfig
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if isSkippedDir(name) {
			continue
		}
		subRoot := filepath.Join(topDir, name) + "/"
		count, err := countCodeFiles(filepath.Join(repoPath, subRoot), cfg.LangFilter)
		if err != nil || count == 0 {
			continue
		}
		shards = append(shards, ShardConfig{
			Name:     topDir + "_" + name,
			RootDirs: []string{subRoot},
			MaxFiles: cfg.MaxFilesPerShard,
		})
	}
	return shards, nil
}

func isSkippedDir(name string) bool {
	skipped := map[string]bool{
		".git": true, "vendor": true, "node_modules": true, "target": true,
		"build": true, "dist": true, "out": true, "bin": true, "obj": true,
		".github": true, ".ci": true, "docs": true, "test": true, "tests": true,
	}
	return skipped[name] || strings.HasPrefix(name, ".")
}

func countCodeFiles(dir string, exts []string) (int, error) {
	count := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		for _, e := range exts {
			if ext == e {
				count++
				break
			}
		}
		return nil
	})
	return count, err
}
