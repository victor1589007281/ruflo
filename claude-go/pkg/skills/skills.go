// Package skills 实现 Skills 系统。
// 对应 TS: skills/loadSkillsDir.ts + commands.ts + SkillTool/
//
// Skills 是用户定义的可复用提示词模板，存储为 SKILL.md 文件。
// 当 LLM 遇到匹配的任务时，可以通过 Skill 工具调用对应的技能。
//
// 目录结构:
//
//	.claude/skills/
//	  ├── my-skill/
//	  │   └── SKILL.md      # 技能定义 (YAML frontmatter + Markdown body)
//	  └── another-skill/
//	      └── SKILL.md
//
// SKILL.md 格式:
//
//	---
//	name: My Skill
//	description: What this skill does
//	when_to_use: When the user asks about X
//	paths:
//	  - "src/**/*.go"     # 可选: 仅当操作匹配路径的文件时激活
//	---
//	# Skill Content (Markdown)
//	Detailed instructions for the AI...
//
// 技能生命周期 (对应 TS):
//  1. Load: 从 .claude/skills/ 和 ~/.claude/skills/ 加载
//  2. Parse: 解析 YAML frontmatter + Markdown body
//  3. Register: 注册到 SkillRegistry (进程级别共享)
//  4. Discover: 文件操作时动态发现新技能 (对应 discoverSkillDirsForPaths)
//  5. Invoke: LLM 通过 Skill 工具调用 → 注入 skill 内容到对话
//  6. Watch: 文件变更时自动重载 (对应 skillChangeDetector.ts)
package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// Skill 技能定义
type Skill struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	WhenToUse    string   `json:"when_to_use"`
	Paths        []string `json:"paths,omitempty"` // 条件路径 (glob 模式)
	AllowedTools []string `json:"allowed_tools,omitempty"`
	Model        string   `json:"model,omitempty"`
	Version      string   `json:"version,omitempty"`
	SkillDir     string   `json:"skill_dir"`   // 技能所在目录
	SourcePath   string   `json:"source_path"` // SKILL.md 完整路径
	LoadedFrom   string   `json:"loaded_from"` // project/user/managed
	Body         string   `json:"body"`        // Markdown 正文
}

// Registry 技能注册表 (进程级别共享)。
// 对应 TS: dynamicSkills Map + getSkillDirCommands (memoized)
type Registry struct {
	mu     sync.RWMutex
	skills map[string]*Skill // name → skill
	dirs   []scanDir         // 已扫描的目录
}

type scanDir struct {
	Path   string
	Source string
}

// NewRegistry 创建技能注册表
func NewRegistry() *Registry {
	return &Registry{
		skills: make(map[string]*Skill),
	}
}

// LoadFromDirs 从多个目录加载技能。
// 对应 TS: getSkillDirCommands → loadSkillsFromSkillsDir
//
// 扫描每个目录下的子目录，查找 SKILL.md 文件:
//
//	baseDir/
//	  skill-a/SKILL.md
//	  skill-b/SKILL.md
func (r *Registry) LoadFromDirs(dirs []string, source string) int {
	loaded := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			skillPath := filepath.Join(dir, entry.Name(), "SKILL.md")
			if _, err := os.Stat(skillPath); err != nil {
				continue
			}
			skill, err := ParseSkillFile(skillPath, source)
			if err != nil {
				log.Printf("[Skills] 解析失败 %s: %v", skillPath, err)
				continue
			}
			r.Register(skill)
			loaded++
		}
	}
	if loaded > 0 {
		r.mu.Lock()
		for _, dir := range dirs {
			r.dirs = append(r.dirs, scanDir{Path: dir, Source: source})
		}
		r.mu.Unlock()
	}
	return loaded
}

// LoadDefaults 加载默认技能目录。
// 对应 TS: getSkillsPath 的三个来源:
//  1. <cwd>/.claude/skills/ (项目级)
//  2. <cwd>/.claude-go/skills/ (状态目录)
//  3. ~/.claude/skills/ (用户级)
//  4. ~/.claude-go/skills/ (用户状态目录)
func (r *Registry) LoadDefaults(cwd string) int {
	total := r.LoadBuiltins()

	for _, dir := range defaultSkillDirs(cwd) {
		if info, err := os.Stat(dir.Path); err == nil && info.IsDir() {
			total += r.LoadFromDirs([]string{dir.Path}, dir.Source)
		}
	}
	return total
}

// defaultSkillDirs returns the default search paths for skills.
func defaultSkillDirs(cwd string) []scanDir {
	var dirs []scanDir
	if cwd != "" {
		dirs = append(dirs,
			scanDir{Path: filepath.Join(cwd, ".claude", "skills"), Source: "project"},
			scanDir{Path: filepath.Join(cwd, ".claude-go", "skills"), Source: "state"},
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			scanDir{Path: filepath.Join(home, ".claude", "skills"), Source: "user"},
			scanDir{Path: filepath.Join(home, ".claude-go", "skills"), Source: "user-state"},
		)
	}
	return dirs
}

// Register 注册一个技能 (同名覆盖)
func (r *Registry) Register(skill *Skill) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skills[skill.Name] = skill
}

// Unregister 卸载一个技能
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.skills[name]; ok {
		delete(r.skills, name)
		return true
	}
	return false
}

// Get 获取指定技能
func (r *Registry) Get(name string) (*Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	return s, ok
}

// All 列出所有技能
func (r *Registry) All() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Skill, 0, len(r.skills))
	for _, s := range r.skills {
		result = append(result, s)
	}
	return result
}

// Count 返回注册的技能数量
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.skills)
}

// Reload 重新加载所有已知目录的技能
func (r *Registry) Reload() int {
	r.mu.Lock()
	dirs := make([]scanDir, len(r.dirs))
	copy(dirs, r.dirs)
	r.skills = make(map[string]*Skill)
	r.dirs = nil
	r.mu.Unlock()

	total := r.LoadBuiltins()
	for _, dir := range dirs {
		total += r.LoadFromDirs([]string{dir.Path}, dir.Source)
	}
	return total
}

// FormatListing 格式化技能列表 (用于系统提示词)。
// 对应 TS: formatCommandsWithinBudget
func (r *Registry) FormatListing() string {
	skills := r.All()
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<available_skills>\n")
	for _, s := range skills {
		sb.WriteString(fmt.Sprintf("- **%s**: %s", s.Name, s.Description))
		if s.WhenToUse != "" {
			sb.WriteString(fmt.Sprintf(" (Use when: %s)", s.WhenToUse))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("</available_skills>\n")
	return sb.String()
}

// ParseSkillFile 解析 SKILL.md 文件。
// 对应 TS: parseSkillFrontmatterFields + createSkillCommand
//
// 格式: YAML frontmatter (---...---) + Markdown body
func ParseSkillFile(path string, source string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseSkillContent(string(data), path, source)
}

// ParseSkillContent 从内存中的 Markdown 内容解析技能。
// 用于磁盘文件和 embed FS 共享同一套解析逻辑。
func ParseSkillContent(content string, sourcePath string, source string) (*Skill, error) {
	skill := &Skill{
		SourcePath: sourcePath,
		SkillDir:   filepath.Dir(sourcePath),
		LoadedFrom: source,
	}

	// 默认名称从目录名推导
	skill.Name = filepath.Base(filepath.Dir(sourcePath))

	// 解析 YAML frontmatter
	if strings.HasPrefix(content, "---\n") {
		end := strings.Index(content[4:], "\n---")
		if end >= 0 {
			frontmatter := content[4 : 4+end]
			skill.Body = strings.TrimSpace(content[4+end+4:])
			parseFrontmatter(frontmatter, skill)
		} else {
			skill.Body = content
		}
	} else {
		skill.Body = content
	}

	if skill.Description == "" {
		lines := strings.SplitN(skill.Body, "\n", 3)
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				skill.Description = line
				break
			}
		}
	}

	return skill, nil
}

// parseFrontmatter 简单的 YAML frontmatter 解析器
func parseFrontmatter(fm string, skill *Skill) {
	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"'`)

		switch key {
		case "name":
			skill.Name = val
		case "description":
			skill.Description = val
		case "when_to_use":
			skill.WhenToUse = val
		case "version":
			skill.Version = val
		case "model":
			skill.Model = val
		}
	}
}

// ============================================================================
// SkillTool — 将技能注册为 LLM 可调用的工具
// 对应 TS: tools/SkillTool/SkillTool.ts
// ============================================================================

// SkillTool Skill 工具实现。
// LLM 通过此工具调用已注册的技能，技能内容会被注入到对话中。
type SkillTool struct {
	registry *Registry
}

// NewSkillTool 创建 Skill 工具
func NewSkillTool(reg *Registry) *SkillTool {
	return &SkillTool{registry: reg}
}

func (t *SkillTool) Name() string { return "Skill" }

func (t *SkillTool) Description() string {
	listing := ""
	for _, s := range t.registry.All() {
		listing += fmt.Sprintf("\n- %s: %s", s.Name, s.Description)
	}
	if listing == "" {
		listing = "\n(no skills currently loaded)"
	}
	return fmt.Sprintf(
		"Invoke a skill by name. Available skills:%s\n\nUse this tool when a skill's description matches the current task.",
		listing,
	)
}

func (t *SkillTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "The skill name to invoke."},
			"arguments": {"type": "object", "description": "Optional arguments passed to the skill."}
		},
		"required": ["name"]
	}`)
}

func (t *SkillTool) IsReadOnly(_ json.RawMessage) bool        { return true }
func (t *SkillTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *SkillTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 调用技能。
// 对应 TS: SkillTool.ts 中的 call → getPromptForCommand
//
// 流程:
//  1. 解析 name 参数
//  2. 查找技能
//  3. 返回技能的完整 Markdown 内容 (由 LLM 作为指令执行)
func (t *SkillTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析失败: %v", err), IsError: true}, nil
	}

	skill, ok := t.registry.Get(in.Name)
	if !ok {
		available := make([]string, 0)
		for _, s := range t.registry.All() {
			available = append(available, s.Name)
		}
		return &tool.ToolResult{
			Content: fmt.Sprintf("技能 %q 未找到。可用技能: %s", in.Name, strings.Join(available, ", ")),
			IsError: true,
		}, nil
	}

	content := fmt.Sprintf("## Skill: %s\n\n%s", skill.Name, skill.Body)
	if skill.SkillDir != "" {
		content += fmt.Sprintf("\n\n---\nSkill directory: %s", skill.SkillDir)
	}

	return &tool.ToolResult{Content: content}, nil
}

// InstallSkill 安装技能到指定目录。
// 创建 <dir>/<name>/SKILL.md 文件。
func InstallSkill(baseDir, name, content string) error {
	skillDir := filepath.Join(baseDir, name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("创建技能目录失败: %w", err)
	}
	path := filepath.Join(skillDir, "SKILL.md")
	return os.WriteFile(path, []byte(content), 0644)
}

// UninstallSkill 卸载技能 (删除目录)
func UninstallSkill(baseDir, name string) error {
	skillDir := filepath.Join(baseDir, name)
	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		return fmt.Errorf("技能 %q 不存在于 %s", name, baseDir)
	}
	return os.RemoveAll(skillDir)
}
