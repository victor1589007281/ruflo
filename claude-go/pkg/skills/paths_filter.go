// paths_filter.go —— SKILL.md paths 字段的运行期消费者 (第七章缺口修复)。
//
// 语义 (对齐 Claude Code TS 的 paths 约定): 技能声明 paths 即"我只与这些文件相关"——
// 工作目录里存在命中文件时才进注入清单; 未声明 paths 的技能恒可见 (向后兼容)。
// 缓存于 Registry.pathsCache, Register/Reload 失效。
package skills

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// pathsScanCap 单目录可见性判定的最大扫描文件数 (防巨型仓库 walk 失控)。
const pathsScanCap = 2000

// pathsSkipDirs 判定扫描时跳过的目录 (命中这些目录的技能请用更精确的 glob)。
var pathsSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".claude-go": true,
	"dist": true, "build": true, "__pycache__": true,
}

// VisibleInDir 判定技能在给定目录下是否因 paths 命中而可见。
// 无 paths 声明 → 恒 true (不改变既有行为)。
func (s *Skill) VisibleInDir(dir string) bool {
	if len(s.Paths) == 0 || dir == "" {
		return true
	}
	files := scanDirFiles(dir)
	if len(files) == 0 {
		return false
	}
	for _, pattern := range s.Paths {
		for _, f := range files {
			if matchPathGlob(pattern, f) {
				return true
			}
		}
	}
	return false
}

// dirFilesCache 目录文件列表缓存 (TTL 语义交给 pathsCache 一并失效)。
var dirFilesCache sync.Map // dir → []string

// scanDirFiles 有界扫描目录文件 (相对路径, 正斜杠)。
func scanDirFiles(dir string) []string {
	if v, ok := dirFilesCache.Load(dir); ok {
		return v.([]string)
	}
	var files []string
	count := 0
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || count >= pathsScanCap {
			return filepath.SkipAll
		}
		if info.IsDir() {
			if pathsSkipDirs[info.Name()] && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		count++
		return nil
	})
	dirFilesCache.Store(dir, files)
	return files
}

// matchPathGlob 轻量 glob 匹配: 支持 "**" 前后缀与单层 * (path.Match 语义)。
// 例: "**/*.go" 命中任意深度 .go; "cmd/**" 命中 cmd 下任意; "pkg/*/main.go" 单层。
func matchPathGlob(pattern, path string) bool {
	pattern = strings.TrimSpace(filepath.ToSlash(pattern))
	path = filepath.ToSlash(path)
	if pattern == "" {
		return false
	}
	// 含 **: 拆前后缀
	if idx := strings.Index(pattern, "**"); idx >= 0 {
		prefix := strings.TrimSuffix(pattern[:idx], "/")
		suffix := strings.TrimPrefix(pattern[idx+2:], "/")
		if prefix != "" && !strings.HasPrefix(path, prefix+"/") && path != prefix {
			return false
		}
		if suffix == "" {
			return true
		}
		// 后缀按 basename/尾段匹配 ("*.go" / "main.go" / "*/main.go")
		return matchTailGlob(suffix, path)
	}
	// 无 **: 全路径或 basename 命中即算
	if ok, _ := filepath.Match(pattern, path); ok {
		return true
	}
	ok, _ := filepath.Match(pattern, filepath.Base(path))
	return ok
}

// matchTailGlob 匹配路径尾部 ("*.go" 命中任何以 .go 结尾; "*/main.go" 命中任意父目录下的 main.go)。
func matchTailGlob(suffix, path string) bool {
	if !strings.Contains(suffix, "/") {
		ok, _ := filepath.Match(suffix, filepath.Base(path))
		return ok
	}
	// 逐段从右往左对
	sp := strings.Split(suffix, "/")
	pp := strings.Split(path, "/")
	if len(sp) > len(pp) {
		return false
	}
	for i := range sp {
		pat := sp[len(sp)-1-i]
		seg := pp[len(pp)-1-i]
		ok, _ := filepath.Match(pat, seg)
		if !ok {
			return false
		}
	}
	return true
}

// ActiveInDir Active 的目录相关变体: 再按 paths 命中过滤。
// 供模型侧清单注入使用 → 用 ModelVisibleActive (F3: ModelInvocable=false 不进模型视图)。
func (r *Registry) ActiveInDir(dir string) []*Skill {
	active := r.ModelVisibleActive()
	if dir == "" {
		return active
	}
	r.mu.RLock()
	if r.pathsCache == nil {
		r.pathsCache = make(map[string]bool)
	}
	r.mu.RUnlock()

	out := make([]*Skill, 0, len(active))
	for _, s := range active {
		if len(s.Paths) == 0 {
			out = append(out, s)
			continue
		}
		key := dir + "|" + s.Name
		r.mu.RLock()
		vis, ok := r.pathsCache[key]
		r.mu.RUnlock()
		if !ok {
			vis = s.VisibleInDir(dir)
			r.mu.Lock()
			r.pathsCache[key] = vis
			r.mu.Unlock()
		}
		if vis {
			out = append(out, s)
		}
	}
	return out
}

// FormatShortListingForDir FormatShortListing 的目录相关变体。
// dir 为空时与原方法完全一致 (不过滤 paths)。
func (r *Registry) FormatShortListingForDir(limit int, dir string) string {
	if dir == "" {
		return r.FormatShortListing(limit)
	}
	skillsAll := r.ModelVisibleActive()
	filtered := make([]*Skill, 0, len(skillsAll))
	for _, s := range skillsAll {
		if len(s.Paths) == 0 || s.VisibleInDir(dir) {
			filtered = append(filtered, s)
		}
	}
	r.sortForListing(filtered) // 13.7-P2 L1 配给: 与 FormatShortListing 同一排序语义
	// 复用原渲染: 临时换 skills 视图太侵入, 这里直接按同格式渲染。
	if len(filtered) == 0 {
		return ""
	}
	if limit <= 0 {
		limit = len(filtered)
	}
	var sb strings.Builder
	sb.WriteString("<available_skills summary=\"short\">\n")
	usedDesc := 0
	for i, sk := range filtered {
		line := "- " + sk.Name
		if i < limit && usedDesc < shortListingDescBudget && sk.Description != "" {
			desc := truncateRunes(sk.Description, 140)
			if usedDesc+len([]rune(desc)) <= shortListingDescBudget {
				line += ": " + desc
				usedDesc += len([]rune(desc))
			}
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("</available_skills>\n")
	return sb.String()
}
