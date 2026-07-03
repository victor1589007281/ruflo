package sync

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v2"
)

// Result 保存一次同步的统计结果。
type Result struct {
	Created   int
	Updated   int
	Unchanged int
	Deleted   int
	Errors    int
}

// RunSync 执行一次增量同步。
func RunSync(cfg Config, source string, adapter Adapter) (*Result, error) {
	idx, err := LoadIndex(cfg.KnowledgeRepo, source)
	if err != nil {
		return nil, err
	}

	rawDir := filepath.Join(cfg.KnowledgeRepo, "raw", source)
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 raw 目录失败: %w", err)
	}
	deletedDir := filepath.Join(cfg.KnowledgeRepo, "raw", "_deleted", source)
	if err := os.MkdirAll(deletedDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建删除目录失败: %w", err)
	}

	items, err := adapter.List()
	if err != nil {
		return nil, fmt.Errorf("列出 %s 数据失败: %w", source, err)
	}

	res := &Result{}
	now := time.Now().UTC()
	seen := make(map[string]bool, len(items))

	for _, summary := range items {
		seen[summary.ExternalID] = true
		full, err := adapter.Fetch(summary)
		if err != nil {
			res.Errors++
			continue
		}

		content := buildMarkdown(full, source, now)
		hash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content)))

		existing, ok := idx.Items[full.ExternalID]
		needsWrite := !ok || existing.Hash != hash || !existing.UpdatedAt.Equal(full.UpdatedAt)

		var relPath string
		if ok && existing.Path != "" {
			relPath = existing.Path
		} else {
			relPath = makeFilePath(source, full.Type, full.ExternalID, full.Title, now)
		}
		absPath := filepath.Join(cfg.KnowledgeRepo, relPath)

		if needsWrite {
			if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
				res.Errors++
				continue
			}
			if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
				res.Errors++
				continue
			}
			if ok {
				res.Updated++
			} else {
				res.Created++
			}
		} else {
			res.Unchanged++
		}

		idx.Items[full.ExternalID] = IndexItem{
			ExternalID: full.ExternalID,
			Path:       relPath,
			Title:      strings.ToValidUTF8(full.Title, ""),
			UpdatedAt:  full.UpdatedAt,
			SyncedAt:   now,
			Hash:       hash,
			URL:        full.URL,
		}
	}

	// 处理外部已删除的条目
	for id, item := range idx.Items {
		if seen[id] {
			continue
		}
		oldPath := filepath.Join(cfg.KnowledgeRepo, item.Path)
		if _, err := os.Stat(oldPath); err == nil {
			target := filepath.Join(deletedDir, filepath.Base(item.Path))
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			if err := os.Rename(oldPath, target); err == nil {
				res.Deleted++
			}
		}
		delete(idx.Items, id)
	}

	idx.SyncedAt = now
	if err := SaveIndex(cfg.KnowledgeRepo, idx); err != nil {
		return res, fmt.Errorf("保存索引失败: %w", err)
	}

	// 把本次同步产物提交(并尝试推送)到 knowledge 仓库 —— 否则同步只落在工作区、进不了版本库。
	// 全程尽力而为: git 不可用/非仓库/无凭证时只告警、不让同步失败(文件已写入工作区)。
	if cfg.KnowledgeRepo != "" {
		commitKnowledge(cfg.KnowledgeRepo, source)
	}

	return res, nil
}

// commitKnowledge 尽力把本次同步产物 (raw/ + schema/) 提交并推送到 knowledge 仓库。
// 任何一步失败都只告警、返回 —— 同步文件已写入工作区, 不因 git 问题让整次同步失败。
// 只暂存 raw/ 与 schema/, 不牵连仓库里的其它改动(wiki/.obsidian 等)。
func commitKnowledge(repoDir, source string) {
	// 非 git 仓库(或无 git)→ 跳过提交, 保持"仅写文件"的原行为。
	if _, err := runGit(repoDir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return
	}
	if out, err := runGit(repoDir, "add", "raw", "schema"); err != nil {
		log.Printf("[sync] %s: git add 失败, 跳过提交: %v (%s)", source, err, out)
		return
	}
	// 无暂存变更 → 无需提交(含上次已提交/无新笔记的情况)。
	if _, err := runGit(repoDir, "diff", "--cached", "--quiet"); err == nil {
		return
	}
	msg := fmt.Sprintf("chore(sync): %s @ %s", source, time.Now().UTC().Format("2006-01-02T15:04Z"))
	if out, err := runGit(repoDir, "commit", "-m", msg); err != nil {
		log.Printf("[sync] %s: git commit 失败: %v (%s)", source, err, out)
		return
	}
	if out, err := runGit(repoDir, "push"); err != nil {
		log.Printf("[sync] %s: 已本地提交但 push 失败(下次同步会重试): %v (%s)", source, err, out)
	} else {
		log.Printf("[sync] %s: 已提交并推送到 knowledge 仓库", source)
	}
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func buildMarkdown(item ExternalItem, source string, syncedAt time.Time) string {
	front := map[string]interface{}{
		"source":      source,
		"source_type": item.Type,
		"external_id": item.ExternalID,
		"title":       strings.ToValidUTF8(item.Title, ""),
		"url":         item.URL,
		"updated_at":  item.UpdatedAt.UTC().Format(time.RFC3339),
		"synced_at":   syncedAt.Format(time.RFC3339),
		"tags":        item.Tags,
	}
	if front["tags"] == nil {
		front["tags"] = []string{}
	}
	yamlBytes, _ := yaml.Marshal(front)
	body := strings.TrimSpace(strings.ToValidUTF8(item.Body, ""))
	return "---\n" + string(yamlBytes) + "---\n\n" + body + "\n"
}

var nonWord = regexp.MustCompile(`[^\w\-一-鿿]+`)

func makeFilePath(source, typ, externalID, title string, t time.Time) string {
	date := t.UTC().Format("2006-01-02")
	// 清理非法 UTF-8，避免索引 JSON 与文件名不一致
	safeTitle := strings.ToValidUTF8(title, "")
	slug := strings.ToLower(strings.Trim(nonWord.ReplaceAllString(safeTitle, "-"), "-"))
	if slug == "" {
		slug = "untitled"
	}
	// 按 rune 截断，避免切开多字节 UTF-8 字符
	runes := []rune(slug)
	if len(runes) > 60 {
		slug = string(runes[:60])
	}
	name := fmt.Sprintf("%s_%s-%s-%s_%s.md", date, source, typ, externalID, slug)
	name = strings.ReplaceAll(name, "/", "-")
	return filepath.Join("raw", source, name)
}
