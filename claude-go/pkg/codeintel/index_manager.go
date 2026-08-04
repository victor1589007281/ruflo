// index_manager.go — 集中式索引目录管理器。
//
// 职责:
//   1. 将各仓库的 .gitnexus/ 和 graphify-out/ 统一托管到 /mnt/data/codeintel 下
//   2. 在仓库根目录创建 symlink，让 CLI 无感知写入集中目录
//   3. 维护 index.json 元数据（仓库、分支、commit、索引目录、更新时间）
//
// 目录结构:
//   /mnt/data/codeintel/
//     index.json
//     gitnexus/
//       <repo-name>/<branch>/.gitnexus/
//     graphify/
//       <repo-name>/<branch>/graphify-out/
package codeintel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// IndexMeta 集中索引元数据。
type IndexMeta struct {
	Version int              `json:"version"`
	Repos   []RepoIndexEntry `json:"repos"`
}

// RepoIndexEntry 单个仓库的索引记录。
type RepoIndexEntry struct {
	Name         string `json:"name"`
	RepoPath     string `json:"repo_path"`
	Branch       string `json:"branch"`
	Commit       string `json:"commit"`
	GitNexusDir  string `json:"gitnexus_dir"`
	GraphifyDir  string `json:"graphify_dir"`
	IndexedAt    string `json:"indexed_at"`
}

// IndexManager 集中索引管理器。
type IndexManager struct {
	BaseDir string
	mu      sync.Mutex
}

// setupMu 进程级串行化 symlink 建链/迁移。RunIndex 同进程并行跑 Analyze 与 Update，
// 二者各自 setupIndex → NewIndexManager → SetupRepoIndex，对同一 .gitnexus /
// graphify-out 做 ensureSymlink。若用实例级互斥锁，两个 goroutine 持的是两把
// 不相干的锁，仍会同时 Lstat 到 ENOENT 后双双 symlink → 后者 EEXIST。必须进程级。
// （跨进程已由 flock 串行，进程内由它兜底。）
var setupMu sync.Mutex

// NewIndexManager 创建索引管理器。baseDir 为空时不启用集中管理。
func NewIndexManager(baseDir string) *IndexManager {
	if baseDir == "" {
		return nil
	}
	return &IndexManager{BaseDir: baseDir}
}

// repoNameFromPath 从绝对路径提取仓库名称（取最后一段）。
func repoNameFromPath(repoPath string) string {
	base := filepath.Base(repoPath)
	if base == "" || base == "." {
		return "repo"
	}
	return base
}

// currentGitInfo 获取仓库当前分支和 commit。
func currentGitInfo(repoPath string) (branch, commit string, err error) {
	out, _, err := runTool(repoPath, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("git branch: %w", err)
	}
	branch = strings.TrimSpace(string(out))

	out, _, err = runTool(repoPath, "git", "rev-parse", "--short", "HEAD")
	if err != nil {
		return branch, "", fmt.Errorf("git commit: %w", err)
	}
	commit = strings.TrimSpace(string(out))
	return branch, commit, nil
}

// SetupRepoIndex 为仓库建立集中索引目录和 symlink。
// 返回 GitNexus 和 Graphify 的集中目录路径。
func (im *IndexManager) SetupRepoIndex(repoPath string) (gnDir, gfDir string, err error) {
	branch, commit, err := currentGitInfo(repoPath)
	if err != nil {
		branch = "unknown"
		commit = "unknown"
	}

	name := repoNameFromPath(repoPath)

	// 进程级互斥: 同进程 Analyze/Update 并行 setupIndex，串行化建链与迁移。
	setupMu.Lock()
	defer setupMu.Unlock()

	// 集中目录
	gnDir = filepath.Join(im.BaseDir, "gitnexus", name, branch)
	gfDir = filepath.Join(im.BaseDir, "graphify", name, branch)

	// 创建目录
	for _, d := range []string{gnDir, gfDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", "", fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// GitNexus: 迁移或创建 symlink
	repoGn := filepath.Join(repoPath, ".gitnexus")
	destGn := filepath.Join(gnDir, ".gitnexus")
	if err := im.ensureSymlink(repoGn, destGn); err != nil {
		return "", "", fmt.Errorf("gitnexus symlink: %w", err)
	}

	// Graphify: 迁移或创建 symlink
	repoGf := filepath.Join(repoPath, "graphify-out")
	destGf := filepath.Join(gfDir, "graphify-out")
	if err := im.ensureSymlink(repoGf, destGf); err != nil {
		return "", "", fmt.Errorf("graphify symlink: %w", err)
	}

	// 更新元数据
	if err := im.updateMeta(name, repoPath, branch, commit, gnDir, gfDir); err != nil {
		// 非致命错误，只记录
		_ = err
	}

	return gnDir, gfDir, nil
}

// ensureSymlink 确保 src 是指向 dst 的 symlink。
// 如果 src 是真实目录，先移动到 dst；如果 src 已是 symlink 指向 dst，忽略；否则替换。
func (im *IndexManager) ensureSymlink(src, dst string) error {
	// 先确保 dst 目录存在（避免创建 broken symlink）
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir dst %s: %w", dst, err)
	}

	fi, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			// 直接创建 symlink
			return os.Symlink(dst, src)
		}
		return err
	}

	// src 存在
	if fi.Mode()&os.ModeSymlink != 0 {
		// 已是 symlink，检查指向
		cur, err := os.Readlink(src)
		if err == nil && cur == dst {
			return nil
		}
		// 指向不对，删除重建
		_ = os.Remove(src)
		return os.Symlink(dst, src)
	}

	// src 是真实文件或目录，移动到 dst
	if err := os.Rename(src, dst); err != nil {
		// 移动失败，尝试复制+删除
		if err := copyDir(src, dst); err != nil {
			return fmt.Errorf("move %s -> %s: %w", src, dst, err)
		}
		_ = os.RemoveAll(src)
	}
	return os.Symlink(dst, src)
}

// metaPath 返回元数据文件路径。
func (im *IndexManager) metaPath() string {
	return filepath.Join(im.BaseDir, "index.json")
}

// loadMeta 加载元数据。
func (im *IndexManager) loadMeta() (*IndexMeta, error) {
	p := im.metaPath()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &IndexMeta{Version: 1, Repos: []RepoIndexEntry{}}, nil
		}
		return nil, err
	}
	var m IndexMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// saveMeta 保存元数据。
func (im *IndexManager) saveMeta(m *IndexMeta) error {
	p := im.metaPath()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}

// updateMeta 更新或追加仓库索引记录。
// 跨进程安全：多实例共享同一 PV 时，对 index.json 的读-改-写必须加 flock，
// 否则两个实例同时写会互相覆盖（读旧值→各自写→丢记录）。进程内 im.mu 只
// 挡单进程并发，flock 挡跨进程并发。
func (im *IndexManager) updateMeta(name, repoPath, branch, commit, gnDir, gfDir string) error {
	// 跨进程排他锁：覆盖 loadMeta → saveMeta 整段读-改-写。
	fl, err := AcquireFileLock(im.locksDir(), "index.json", 30*time.Second)
	if err != nil {
		// 拿不到锁（如只读挂载）不致命，退化为仅进程内互斥。
		_ = err
	} else {
		defer fl.Release()
	}

	im.mu.Lock()
	defer im.mu.Unlock()

	m, err := im.loadMeta()
	if err != nil {
		m = &IndexMeta{Version: 1, Repos: []RepoIndexEntry{}}
	}

	now := time.Now().Format(time.RFC3339)
	found := false
	for i := range m.Repos {
		if m.Repos[i].RepoPath == repoPath && m.Repos[i].Branch == branch {
			m.Repos[i].Commit = commit
			m.Repos[i].GitNexusDir = gnDir
			m.Repos[i].GraphifyDir = gfDir
			m.Repos[i].IndexedAt = now
			found = true
			break
		}
	}
	if !found {
		m.Repos = append(m.Repos, RepoIndexEntry{
			Name:        name,
			RepoPath:    repoPath,
			Branch:      branch,
			Commit:      commit,
			GitNexusDir: gnDir,
			GraphifyDir: gfDir,
			IndexedAt:   now,
		})
	}
	return im.saveMeta(m)
}

// copyDir 递归复制目录。
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dstPath, data, info.Mode())
	})
}

// GetMeta 读取当前元数据（外部调用用）。
func (im *IndexManager) GetMeta() (*IndexMeta, error) {
	return im.loadMeta()
}
