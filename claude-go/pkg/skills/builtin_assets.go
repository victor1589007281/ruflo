package skills

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed builtin/*/SKILL.md
var builtinSkillFS embed.FS

const builtinSkillRoot = "builtin"

// LoadBuiltins loads embedded skills that ship with claude-go.
func (r *Registry) LoadBuiltins() int {
	return r.LoadFromFS(builtinSkillFS, builtinSkillRoot, "builtin")
}

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
