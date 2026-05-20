package codeintel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GlobalIndex manages all indexed repositories globally, not tied to the current working directory.
type GlobalIndex struct {
	RootPath string                `json:"root_path"`
	Repos    map[string]*RepoEntry `json:"repos"`
	mu       sync.RWMutex
}

// RepoEntry represents a registered repository.
type RepoEntry struct {
	ID        string                  `json:"id"`
	Name      string                  `json:"name"`
	Path      string                  `json:"path"`
	RemoteURL string                  `json:"remote_url"`
	Branches  map[string]*BranchEntry `json:"branches"`
	Deps      []string                `json:"deps,omitempty"` // 依赖的仓库 ID 列表
	CreatedAt time.Time               `json:"created_at"`
	UpdatedAt time.Time               `json:"updated_at"`
}

// BranchEntry represents a branch within a repository.
type BranchEntry struct {
	Name       string    `json:"name"`
	CommitHash string    `json:"commit_hash"`
	IndexedAt  time.Time `json:"indexed_at"`
	GitNexusOK bool      `json:"gitnexus_ok"`
	GraphifyOK bool      `json:"graphify_ok"`
	IndexPath  string    `json:"index_path"`
}

// QueryReadiness describes whether a query can be served and provides fallback guidance.
type QueryReadiness struct {
	Ready        bool   `json:"ready"`
	RepoExists   bool   `json:"repo_exists"`
	BranchExists bool   `json:"branch_exists"`
	CommitMatch  bool   `json:"commit_match"` // current HEAD matches indexed commit
	CurrentHead  string `json:"current_head"`
	IndexedHead  string `json:"indexed_head"`
	Suggestion   string `json:"suggestion"`
}

const (
	defaultRootName     = ".claude-code-intel"
	globalIndexFileName = "global_index.json"
	reposDirName        = "repos"
	branchesDirName     = "branches"
	repoInfoFileName    = "info.json"
	headFileName        = "head.json"
	stateFileName       = "state.json"
)

// NewGlobalIndex creates or loads a GlobalIndex at the given root path.
// If rootPath is empty, it defaults to ~/.claude-code-intel.
func NewGlobalIndex(rootPath string) (*GlobalIndex, error) {
	if rootPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home dir: %w", err)
		}
		rootPath = filepath.Join(home, defaultRootName)
	}

	if err := os.MkdirAll(rootPath, 0755); err != nil {
		return nil, fmt.Errorf("create root path %s: %w", rootPath, err)
	}

	gi := &GlobalIndex{
		RootPath: rootPath,
		Repos:    make(map[string]*RepoEntry),
	}

	indexFile := filepath.Join(rootPath, globalIndexFileName)
	data, err := os.ReadFile(indexFile)
	if err == nil {
		if err := json.Unmarshal(data, gi); err != nil {
			return nil, fmt.Errorf("unmarshal global index: %w", err)
		}
		// Ensure map is non-nil after unmarshalling.
		if gi.Repos == nil {
			gi.Repos = make(map[string]*RepoEntry)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read global index: %w", err)
	}

	return gi, nil
}

// save persists the global index metadata to disk.
func (gi *GlobalIndex) save() error {
	gi.mu.RLock()
	data, err := json.MarshalIndent(gi, "", "  ")
	gi.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshal global index: %w", err)
	}

	indexFile := filepath.Join(gi.RootPath, globalIndexFileName)
	if err := os.WriteFile(indexFile, data, 0644); err != nil {
		return fmt.Errorf("write global index: %w", err)
	}
	return nil
}

// repoIDFromPath generates a deterministic repo ID from a local path and remote URL.
func repoIDFromPath(repoPath, remoteURL string) string {
	h := sha256.New()
	h.Write([]byte(repoPath))
	h.Write([]byte("|"))
	h.Write([]byte(remoteURL))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// hashString returns a short hash of a string for directory naming.
func hashString(s string) string {
	h := sha256.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// getRemoteURL attempts to read the git remote URL for origin.
func getRemoteURL(repoPath string) string {
	gitDir := filepath.Join(repoPath, ".git")
	configPath := filepath.Join(gitDir, "config")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}

	// Very simple INI-like parsing for remote "origin" url.
	lines := strings.Split(string(data), "\n")
	inOrigin := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[remote \"origin\"]") {
			inOrigin = true
			continue
		}
		if inOrigin {
			if strings.HasPrefix(trimmed, "[") {
				break
			}
			if strings.HasPrefix(trimmed, "url = ") {
				return strings.TrimSpace(strings.TrimPrefix(trimmed, "url = "))
			}
		}
	}
	return ""
}

// RegisterRepo registers a repository at the given local path.
func (gi *GlobalIndex) RegisterRepo(repoPath, repoName string) (*RepoEntry, error) {
	repoPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}

	remoteURL := getRemoteURL(repoPath)
	repoID := repoIDFromPath(repoPath, remoteURL)

	gi.mu.Lock()
	defer gi.mu.Unlock()

	if existing, ok := gi.Repos[repoID]; ok {
		return existing, nil
	}

	now := time.Now().UTC()
	entry := &RepoEntry{
		ID:        repoID,
		Name:      repoName,
		Path:      repoPath,
		RemoteURL: remoteURL,
		Branches:  make(map[string]*BranchEntry),
		CreatedAt: now,
		UpdatedAt: now,
	}

	gi.Repos[repoID] = entry
	if err := gi.save(); err != nil {
		delete(gi.Repos, repoID)
		return nil, err
	}

	// Write repo info.json
	repoDir := filepath.Join(gi.RootPath, reposDirName, repoID)
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		return nil, fmt.Errorf("create repo dir: %w", err)
	}
	infoPath := filepath.Join(repoDir, repoInfoFileName)
	infoData, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal repo info: %w", err)
	}
	if err := os.WriteFile(infoPath, infoData, 0644); err != nil {
		return nil, fmt.Errorf("write repo info: %w", err)
	}

	return entry, nil
}

// GetRepo retrieves a repository by its ID.
func (gi *GlobalIndex) GetRepo(repoID string) (*RepoEntry, bool) {
	gi.mu.RLock()
	defer gi.mu.RUnlock()
	entry, ok := gi.Repos[repoID]
	if !ok {
		return nil, false
	}
	// Return a shallow copy to avoid external mutation of the map.
	copy := *entry
	return &copy, true
}

// branchDir returns the on-disk directory for a branch.
func (gi *GlobalIndex) branchDir(repoID, branchName string) string {
	branchHash := hashString(branchName)
	return filepath.Join(gi.RootPath, reposDirName, repoID, branchesDirName, branchHash)
}

// UpdateBranch updates the indexed commit hash for a branch and persists metadata.
func (gi *GlobalIndex) UpdateBranch(repoID, branchName, commitHash string) error {
	gi.mu.Lock()
	defer gi.mu.Unlock()

	repo, ok := gi.Repos[repoID]
	if !ok {
		return fmt.Errorf("repo %s not found", repoID)
	}

	branch, ok := repo.Branches[branchName]
	if !ok {
		branch = &BranchEntry{
			Name:      branchName,
			IndexPath: gi.branchDir(repoID, branchName),
		}
		repo.Branches[branchName] = branch
	}

	branch.CommitHash = commitHash
	branch.IndexedAt = time.Now().UTC()
	repo.UpdatedAt = time.Now().UTC()

	if err := gi.save(); err != nil {
		return err
	}

	return gi.writeBranchState(branch)
}

// writeBranchState 写入分支元数据文件（在 Lock 内调用）。
func (gi *GlobalIndex) writeBranchState(branch *BranchEntry) error {
	bdir := branch.IndexPath
	if err := os.MkdirAll(bdir, 0755); err != nil {
		return fmt.Errorf("create branch dir: %w", err)
	}

	headPath := filepath.Join(bdir, headFileName)
	headData, err := json.MarshalIndent(map[string]interface{}{
		"commit_hash": branch.CommitHash,
		"timestamp":   branch.IndexedAt,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal head: %w", err)
	}
	if err := os.WriteFile(headPath, headData, 0644); err != nil {
		return fmt.Errorf("write head: %w", err)
	}

	statePath := filepath.Join(bdir, stateFileName)
	stateData, err := json.MarshalIndent(branch, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := os.WriteFile(statePath, stateData, 0644); err != nil {
		return fmt.Errorf("write state: %w", err)
	}

	return nil
}

// GetBranch retrieves a branch entry for a repo.
func (gi *GlobalIndex) GetBranch(repoID, branchName string) (*BranchEntry, bool) {
	gi.mu.RLock()
	defer gi.mu.RUnlock()

	repo, ok := gi.Repos[repoID]
	if !ok {
		return nil, false
	}
	branch, ok := repo.Branches[branchName]
	if !ok {
		return nil, false
	}
	copy := *branch
	return &copy, true
}

// getCurrentHead returns the current HEAD commit hash for a repo path by reading .git/HEAD and .git/refs/heads/*.
func getCurrentHead(repoPath string) string {
	headFile := filepath.Join(repoPath, ".git", "HEAD")
	data, err := os.ReadFile(headFile)
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	// If HEAD is a symref
	if strings.HasPrefix(content, "ref: ") {
		refPath := strings.TrimPrefix(content, "ref: ")
		fullRef := filepath.Join(repoPath, ".git", refPath)
		refData, err := os.ReadFile(fullRef)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(refData))
	}
	// Detached HEAD
	return content
}

// CheckQueryReady checks whether a query can be served for the given repo and branch,
// and returns a QueryReadiness with actionable suggestions when not ready.
func (gi *GlobalIndex) CheckQueryReady(repoID, branchName string) *QueryReadiness {
	gi.mu.RLock()
	repo, repoOK := gi.Repos[repoID]
	var branch *BranchEntry
	var branchOK bool
	if repoOK {
		branch, branchOK = repo.Branches[branchName]
	}
	gi.mu.RUnlock()

	qr := &QueryReadiness{
		RepoExists:   repoOK,
		BranchExists: branchOK,
	}

	if !repoOK {
		qr.Suggestion = fmt.Sprintf("Repository %s is not registered. Please register it first with RegisterRepo.", repoID)
		return qr
	}

	if !branchOK {
		qr.Suggestion = fmt.Sprintf("Branch %s is not indexed for repo %s. Please index the branch first.", branchName, repoID)
		return qr
	}

	qr.IndexedHead = branch.CommitHash
	qr.CurrentHead = getCurrentHead(repo.Path)
	qr.CommitMatch = qr.CurrentHead != "" && qr.CurrentHead == qr.IndexedHead

	if !qr.CommitMatch {
		if qr.CurrentHead == "" {
			qr.Suggestion = fmt.Sprintf("Could not determine current HEAD for %s. The indexed commit is %s. Please ensure the repo is a valid git repository and re-index if needed.", repo.Path, qr.IndexedHead)
		} else {
			qr.Suggestion = fmt.Sprintf("HEAD mismatch: current %s vs indexed %s. Please re-index branch %s to update the code intelligence data.", qr.CurrentHead, qr.IndexedHead, branchName)
		}
		return qr
	}

	if !branch.GitNexusOK && !branch.GraphifyOK {
		qr.Suggestion = fmt.Sprintf("Branch %s is registered but no indexers (GitNexus/Graphify) have completed. Please run the indexers.", branchName)
		return qr
	}

	qr.Ready = true
	qr.Suggestion = "Query ready."
	return qr
}

// ListRepos returns a snapshot of all registered repositories.
func (gi *GlobalIndex) ListRepos() []*RepoEntry {
	gi.mu.RLock()
	defer gi.mu.RUnlock()

	result := make([]*RepoEntry, 0, len(gi.Repos))
	for _, r := range gi.Repos {
		copy := *r
		result = append(result, &copy)
	}
	return result
}

// GetIndexPath returns the on-disk index directory for a repo branch.
func (gi *GlobalIndex) GetIndexPath(repoID, branchName string) string {
	return gi.branchDir(repoID, branchName)
}
