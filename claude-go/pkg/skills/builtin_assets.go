package skills

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed builtin/*/SKILL.md
var builtinSkillFS embed.FS

const builtinSkillRoot = "builtin"

// LoadBuiltins 加载编译进二进制的内置 skills, 返回成功加载的条数。
//
// LoadBuiltins loads embedded skills that ship with claude-go.
func (r *Registry) LoadBuiltins() int {
	return r.LoadFromFS(builtinSkillFS, builtinSkillRoot, "builtin")
}

// LoadFromFS 从任意文件系统 (如 embed.FS) 的 baseDir 下加载 skills, source 标注来源。
//
// 注意：目录读不到、单个 SKILL.md 读不到或解析失败时都是**静默 continue / 返回 0**,
// 调用方无法区分"没有 skills"与"全都读失败了"。
//
// LoadFromFS loads skills from an arbitrary filesystem such as embed.FS.
func (r *Registry) LoadFromFS(fsys fs.FS, baseDir string, source string) int {
	entries, err := fs.ReadDir(fsys, baseDir)
	if err != nil {
		return 0
	}

	loaded := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillPath := path.Join(baseDir, entry.Name(), "SKILL.md")
		data, err := fs.ReadFile(fsys, skillPath)
		if err != nil {
			continue
		}
		skill, err := ParseSkillContent(string(data), skillPath, source)
		if err != nil {
			continue
		}
		r.Register(skill)
		loaded++
	}
	return loaded
}
