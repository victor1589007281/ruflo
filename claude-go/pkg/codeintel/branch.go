// branch.go — 分支隔离管理。
//
// 策略：CoW (Copy-on-Write) 元数据 + 共享只读数据。
//   - 创建分支：复制 branch.json 元数据，共享各分片实际索引文件
//   - 切换分支：修改活跃分支指针（config.yaml 中的 active_branch）
//   - 删除分支：仅删除分支目录中的元数据，共享数据引用计数由外部管理
package codeintel

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// BranchManager 分支管理器。
type BranchManager struct {
	Store *Store
}

// NewBranchManager 创建分支管理器。
func NewBranchManager(repoPath string) *BranchManager {
	return &BranchManager{Store: NewStore(repoPath)}
}

// SwitchBranch 切换活跃分支。
func (bm *BranchManager) SwitchBranch(branchName string) error {
	if !bm.Store.BranchExists(branchName) {
		return fmt.Errorf("branch %q not indexed, run create first", branchName)
	}
	cfg, err := bm.Store.LoadConfig()
	if err != nil {
		return err
	}
	// 在 config 中记录活跃分支
	// TODO: add active_branch field to RepoConfig if needed
	_ = cfg
	return nil
}

// CreateBranch 基于当前分支创建新分支索引（CoW）。
func (bm *BranchManager) CreateBranch(baseBranch, newBranch string) error {
	if bm.Store.BranchExists(newBranch) {
		return fmt.Errorf("branch %q already exists", newBranch)
	}
	baseIdx, err := bm.Store.LoadBranchIndex(baseBranch)
	if err != nil {
		return fmt.Errorf("load base branch %q: %w", baseBranch, err)
	}

	// 创建新分支目录结构
	if err := bm.Store.EnsureDirs(newBranch); err != nil {
		return err
	}

	// CoW：复制元数据，不复制实际索引数据
	newIdx := &BranchIndex{
		BranchName:  newBranch,
		CommitHash:  baseIdx.CommitHash,
		Shards:      make(map[string]*ShardIndex),
		CrossEdges:  baseIdx.CrossEdges,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	// 共享分片索引（通过路径引用）
	for name, shard := range baseIdx.Shards {
		newIdx.Shards[name] = shard
	}

	if err := bm.Store.SaveBranchIndex(newIdx); err != nil {
		return err
	}

	// 创建符号链接或引用文件指向 base 的数据
	for name := range baseIdx.Shards {
		shardDir := bm.Store.ShardDir(newBranch, name)
		baseShardDir := bm.Store.ShardDir(baseBranch, name)
		_ = os.MkdirAll(shardDir, 0755)
		// 写引用文件
		refFile := filepath.Join(shardDir, ".ref")
		_ = os.WriteFile(refFile, []byte(baseShardDir), 0644)
	}

	return nil
}

// DeleteBranch 删除分支索引。
func (bm *BranchManager) DeleteBranch(branchName string) error {
	if !bm.Store.BranchExists(branchName) {
		return fmt.Errorf("branch %q does not exist", branchName)
	}
	return bm.Store.DeleteBranch(branchName)
}

// ListBranches 列出所有分支。
func (bm *BranchManager) ListBranches() ([]string, error) {
	return bm.Store.ListBranches()
}

// ResolveShardDir 解析分片目录（处理 CoW 引用）。
func (bm *BranchManager) ResolveShardDir(branchName, shardName string) string {
	dir := bm.Store.ShardDir(branchName, shardName)
	refFile := filepath.Join(dir, ".ref")
	data, err := os.ReadFile(refFile)
	if err == nil {
		// 有引用，返回被引用的目录
		return string(data)
	}
	return dir
}
