// stale_detector.go — 索引过期检测器。
//
// Phase 5 性能与运维：
//   - 检测当前 git HEAD commit 与索引记录的 commit hash 是否一致
//   - 检测文件修改时间是否晚于索引时间
//   - 返回过期分片列表和建议操作
//
// 零 LLM：纯 git 命令 + 文件系统 stat。
package codeintel

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StaleDetector 索引过期检测器。
type StaleDetector struct {
	Store *Store
}

// NewStaleDetector 创建过期检测器。
func NewStaleDetector(repoPath string) *StaleDetector {
	return &StaleDetector{Store: NewStore(repoPath)}
}

// StaleReport 过期检测报告。
type StaleReport struct {
	RepoPath      string                 `json:"repo_path"`
	BranchName    string                 `json:"branch_name"`
	IndexCommit   string                 `json:"index_commit"`
	CurrentCommit string                 `json:"current_commit"`
	IsStale       bool                   `json:"is_stale"`
	StaleShards   []StaleShardInfo       `json:"stale_shards,omitempty"`
	MissingShards []string               `json:"missing_shards,omitempty"`
	Suggestion    string                 `json:"suggestion"`
	CheckedAt     time.Time              `json:"checked_at"`
}

// StaleShardInfo 单个过期分片的信息。
type StaleShardInfo struct {
	ShardName    string   `json:"shard_name"`
	Reason       string   `json:"reason"`        // commit_mismatch / file_modified / index_missing
	ChangedFiles []string `json:"changed_files,omitempty"`
	LastIndexed  time.Time `json:"last_indexed,omitempty"`
}

// Check 检测指定分支的索引是否过期。
func (sd *StaleDetector) Check(branchName string) (*StaleReport, error) {
	repoPath := sd.Store.RepoPath
	currentCommit, err := getCurrentCommit(repoPath)
	if err != nil {
		currentCommit = ""
	}

	idx, err := sd.Store.LoadBranchIndex(branchName)
	if err != nil {
		// 分支索引完全不存在
		return &StaleReport{
			RepoPath:      repoPath,
			BranchName:    branchName,
			IndexCommit:   "",
			CurrentCommit: currentCommit,
			IsStale:       true,
			Suggestion:    fmt.Sprintf("Branch %q not indexed. Run code_intel_init first.", branchName),
			CheckedAt:     time.Now(),
		}, nil
	}

	report := &StaleReport{
		RepoPath:      repoPath,
		BranchName:    branchName,
		IndexCommit:   idx.CommitHash,
		CurrentCommit: currentCommit,
		CheckedAt:     time.Now(),
	}

	// 1. Commit hash 不一致 → 全部分片都视为过期
	if currentCommit != "" && idx.CommitHash != currentCommit {
		report.IsStale = true
		for shardName, si := range idx.Shards {
			report.StaleShards = append(report.StaleShards, StaleShardInfo{
				ShardName:   shardName,
				Reason:      "commit_mismatch",
				LastIndexed: si.LastIndexed,
			})
		}
		report.Suggestion = fmt.Sprintf("Index commit (%s) != current HEAD (%s). Run code_intel_update.", idx.CommitHash, currentCommit)
		return report, nil
	}

	// 2. 逐分片检测文件修改时间
	for shardName, si := range idx.Shards {
		shardCfg := sd.findShardConfig(shardName)
		if shardCfg == nil {
			continue
		}

		changedFiles := sd.detectChangedFilesSince(si.LastIndexed, shardCfg)
		if len(changedFiles) > 0 {
			report.IsStale = true
			report.StaleShards = append(report.StaleShards, StaleShardInfo{
				ShardName:    shardName,
				Reason:       "file_modified",
				ChangedFiles: changedFiles,
				LastIndexed:  si.LastIndexed,
			})
		}
	}

	if report.IsStale {
		report.Suggestion = fmt.Sprintf("%d shards have modified files since last index. Run code_intel_update.", len(report.StaleShards))
	} else {
		report.Suggestion = "Index is up to date."
	}
	return report, nil
}

// CheckShard 检测单个分片是否过期。
func (sd *StaleDetector) CheckShard(branchName, shardName string) (*StaleShardInfo, error) {
	idx, err := sd.Store.LoadBranchIndex(branchName)
	if err != nil {
		return nil, err
	}
	si, ok := idx.Shards[shardName]
	if !ok {
		return nil, fmt.Errorf("shard %q not found in branch %q", shardName, branchName)
	}

	shardCfg := sd.findShardConfig(shardName)
	if shardCfg == nil {
		return &StaleShardInfo{ShardName: shardName, Reason: "config_missing"}, nil
	}

	changedFiles := sd.detectChangedFilesSince(si.LastIndexed, shardCfg)
	if len(changedFiles) > 0 {
		return &StaleShardInfo{
			ShardName:    shardName,
			Reason:       "file_modified",
			ChangedFiles: changedFiles,
			LastIndexed:  si.LastIndexed,
		}, nil
	}
	return &StaleShardInfo{ShardName: shardName, Reason: "up_to_date", LastIndexed: si.LastIndexed}, nil
}

// ============================================================================
// 私有辅助
// ============================================================================

func getCurrentCommit(repoPath string) (string, error) {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (sd *StaleDetector) findShardConfig(name string) *ShardConfig {
	cfg, err := sd.Store.LoadConfig()
	if err != nil {
		return nil
	}
	for i := range cfg.Shards {
		if cfg.Shards[i].Name == name {
			return &cfg.Shards[i]
		}
	}
	return nil
}

func (sd *StaleDetector) detectChangedFilesSince(lastIndexed time.Time, cfg *ShardConfig) []string {
	var changed []string
	for _, root := range cfg.RootDirs {
		rootPath := filepath.Join(sd.Store.RepoPath, root)
		_ = filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if info.ModTime().After(lastIndexed) {
				changed = append(changed, path)
			}
			return nil
		})
	}
	return changed
}

// InjectStaleWarning 在查询结果中注入过期警告。
func InjectStaleWarning(qr *QueryResult, report *StaleReport) {
	if qr == nil || report == nil || !report.IsStale {
		return
	}
	if m, ok := qr.Results.(map[string]interface{}); ok {
		m["_stale_warning"] = true
		m["_index_commit"] = report.IndexCommit
		m["_current_commit"] = report.CurrentCommit
		m["_stale_shards"] = report.StaleShards
		m["_suggestion"] = report.Suggestion
	}
}
