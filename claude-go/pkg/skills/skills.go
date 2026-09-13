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
	"sort"
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
	// Status 治理状态 (frontmatter `status:`): active | shadow | archived。
	// 空 = active (绝大多数存量 SKILL.md 没有 status 行, 见 statusDisabled 注释)。
	Status string `json:"status,omitempty"`
	// Rank 来源优先级 (13.6 F2 rank 分层): 数值越小优先级越高,
	// 同名技能高优先级来源覆盖低优先级来源。project(100) > user(200) > builtin(600)。
	// 直接 Register 未标来源的技能取中间值 300。见 SourceRank。
	Rank int `json:"rank,omitempty"`
	// ModelInvocable / UserInvocable 双向调用策略 (13.6 F3, frontmatter
	// `model-invocable:` / `user-invocable:`)。指针三态: nil = 未声明 = 双向可用
	// (向后兼容硬要求 —— 存量 SKILL.md 没有这两个键)。false 时:
	//   ModelInvocable=false → 只作为用户命令存在, 不进模型清单、Skill 工具拒载;
	//   UserInvocable=false  → 只作为模型可选项, 不进用户侧技能展示。
	ModelInvocable *bool `json:"model_invocable,omitempty"`
	UserInvocable  *bool `json:"user_invocable,omitempty"`
	// Digest 技能内容指纹 (13.6 F1): Register 时对 name/description/when_to_use/
	// version/body 计算的 sha256, 用于"已注入且未变则不再重复加载"记账。
	Digest     string `json:"digest,omitempty"`
	SkillDir   string `json:"skill_dir"`   // 技能所在目录
	SourcePath string `json:"source_path"` // SKILL.md 完整路径
	LoadedFrom string `json:"loaded_from"` // project/user/managed
	Body       string `json:"body"`        // Markdown 正文
}

// SourceRank 来源 → rank (13.6 F2)。数字员工自训练会产生大量同名/近名技能
// (不同团队各自提炼"code-review"变体), 没有覆盖语义时同名技能会在清单里重复
// 出现, 浪费预算并误导模型。对齐 dsh: project(100) > user(200) > bundled(600)。
// claude-go 的目录来源映射: state(项目状态目录)随 project、config(用户配置目录)
// 随 user。未知来源/直接 Register 给中间值 300 —— 既不被项目技能覆盖, 也不覆盖用户技能。
func SourceRank(source string) int {
	switch source {
	case "project", "state":
		return 100
	case "user", "user-state", "config":
		return 200
	case "builtin":
		return 600
	default:
		return 300
	}
}

// ModelCallable 模型可否经清单发现并加载 (nil = 未声明 = 可用, 向后兼容)。
func (s *Skill) ModelCallable() bool {
	return s != nil && (s.ModelInvocable == nil || *s.ModelInvocable)
}

// UserCallable 用户可否作为命令/技能查看调用 (nil = 未声明 = 可用)。
func (s *Skill) UserCallable() bool {
	return s != nil && (s.UserInvocable == nil || *s.UserInvocable)
}

// 治理状态取值 (与 pkg/evolution/skillaudit 的 rewriteStatus 写入值一致)。
const (
	StatusActive   = "active"   // 正常可用
	StatusShadow   = "shadow"   // 自动提炼产物, 待进化门禁裁决, 运行期无效力
	StatusArchived = "archived" // 已退役
)

// statusDisabled 判定某 status 是否"运行期停用"。
//
// 刻意用**黑名单**而不是"只有 active 才可用"的白名单: 存量 SKILL.md 绝大多数根本
// 没有 status 行, 少数第三方技能还可能把 status 当自由文本用 (stable/beta/...)。
// 白名单会在一夜之间把这些技能全部禁掉; 黑名单只对治理流程自己写入的
// shadow/archived/retired/disabled 生效, 未知值一律放行 (fail-open)。
func statusDisabled(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusShadow, StatusArchived, "retired", "disabled":
		return true
	default:
		return false
	}
}

// IsActive 技能在运行期是否可用 (可进清单 / 可被 Skill 工具加载)。
// 无 status 字段 = 可用 —— 向后兼容硬要求。
func (s *Skill) IsActive() bool {
	if s == nil {
		return false
	}
	return !statusDisabled(s.Status)
}

// Registry 技能注册表 (进程级别共享)。
// 对应 TS: dynamicSkills Map + getSkillDirCommands (memoized)
type Registry struct {
	mu     sync.RWMutex
	skills map[string]*Skill // name → skill
	dirs   []scanDir         // 已扫描的目录
	// pathsCache paths 可见性缓存 (dir|skill → 是否命中), Register/Reload 时清空。
	// 清单注入每轮都可能调, 不能每次全目录 walk。
	pathsCache map[string]bool
	// injected 已注入记账 (name → 注入时的 digest) (13.6 F1)。
	// 供持久注入方判断"已注入且内容未变"跳过重复注入。跨 Reload 保留。
	injected map[string]string
	// ranker 清单描述位配给的排序评分 (13.7-P2 L1 配给, SetRanker 注入;
	// 由池装配方接 bandit 后验)。nil = 名称序 (默认, 行为零变化)。
	ranker func(name string) float64
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

// Register 注册一个技能。
//
// 13.6 F2 rank 分层覆盖语义: 同名技能按 Rank 比较, 只有更高优先级 (rank 数值更小)
// 才覆盖既有注册, 低优先级/同 rank 的后来者丢弃。同 rank 丢弃 (而非后来者覆盖) 保证
// Reload 结果与目录扫描顺序无关 —— 确定性要求。此前是无条件"后来者覆盖": 数字员工
// 自训练会产生大量同名变体 (不同团队各自提炼"code-review"), 后加载的 user 技能会
// 静默顶掉更具体的 project 技能, 清单里既重复又误导。
//
// 副作用: 为未显式赋值的 Rank (直接构造的 *Skill, 非文件解析路径) 按 LoadedFrom
// 推导; 为空 Digest 计算 sha256 指纹 (13.6 F1 已注入记账的比对键)。
func (r *Registry) Register(skill *Skill) {
	if skill == nil || skill.Name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if skill.Rank == 0 {
		skill.Rank = SourceRank(skill.LoadedFrom)
	}
	if skill.Digest == "" {
		skill.Digest = ComputeDigest(skill)
	}
	if old, ok := r.skills[skill.Name]; ok {
		if skill.Rank >= old.Rank {
			return // 优先级不高于既有注册, 丢弃 (同 rank 先到先得)
		}
	}
	r.pathsCache = nil // 技能集合变了, paths 可见性缓存失效
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

// Get 获取指定技能 —— **运行期视图: 只返回 active 技能** (design/03 §4.6 必过闸)。
//
// 历史缺陷: shadow 技能 (自动提炼产物, 未过进化门禁) 照常能被 Get 取到并把正文注入
// prompt (Skill 工具、角色内建技能注入), 于是 `evo promote` 改的 status 字段是个没人读的
// 文本字段。这里让 shadow/archived 在运行期直接"不存在"。
// 治理/审计场景 (查看、改进、晋升 shadow 技能) 请用 GetAny。
func (r *Registry) Get(name string) (*Skill, bool) {
	s, ok := r.GetAny(name)
	if !ok || !s.IsActive() {
		return nil, false
	}
	return s, true
}

// GetAny 获取指定技能, 不过滤治理状态 (审计/晋升/自改进用)。
func (r *Registry) GetAny(name string) (*Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	return s, ok
}

// SetRanker 注入清单描述位配给的排序评分 (13.7-P2 L1 配给): 描述位预算
// (shortListingDescBudget) 按 ranker(name) 降序分配 —— 高后验资产的描述先占位,
// 低频资产只留名称。nil / 返回负值视同无评分 (落到名称序)。
// 由池装配方在装配时注入 (skills 不能反向 import builtin, 走函数注入)。
// 与 rank 分层 (F2, 同名覆盖) 正交: ranker 只改**渲染顺序**, 不碰注册覆盖语义。
func (r *Registry) SetRanker(fn func(name string) float64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ranker = fn
}

// sortForListing 清单渲染前的排序: 有 ranker 时按 (评分降序, 名称同分确定性),
// 无 ranker = 纯名称序 (All() 已排好, 稳定透传)。
func (r *Registry) sortForListing(skills []*Skill) {
	r.mu.RLock()
	ranker := r.ranker
	r.mu.RUnlock()
	if ranker == nil {
		return
	}
	sort.SliceStable(skills, func(i, j int) bool {
		ri, rj := ranker(skills[i].Name), ranker(skills[j].Name)
		if ri != rj {
			return ri > rj
		}
		return skills[i].Name < skills[j].Name
	})
}

// All 列出所有已加载技能 (含 shadow/archived) —— 管理视图。
//
// 注意: 要往 prompt 里塞的"技能清单"一律用 Active()/FormatListing(), 不要用 All(),
// 否则 shadow 技能会绕过进化门禁进到模型上下文。
func (r *Registry) All() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Skill, 0, len(r.skills))
	for _, s := range r.skills {
		result = append(result, s)
	}
	// 确定性排序 (按名称)。否则 map 迭代顺序随机, 导致每次重载时
	// 技能清单 / Skill 工具回退列表暴露的顺序漂移, 截断时还会随机隐藏不同技能。
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// Active 列出运行期可用的技能 (排除 shadow/archived), 确定性排序。
// 这是清单/加载路径的默认视图。
func (r *Registry) Active() []*Skill {
	all := r.All()
	result := make([]*Skill, 0, len(all))
	for _, s := range all {
		if s.IsActive() {
			result = append(result, s)
		}
	}
	return result
}

// ModelVisibleActive Active 且模型可调用 (13.6 F3)。
// 模型清单注入 (FormatListing/FormatShortListing/ForDir) 与 Skill 工具加载
// 一律走本视图 —— ModelInvocable=false 的技能只作用户命令, 对模型不存在。
// 治理/用户展示视图不受此过滤 (Active 语义不变, 保证既有测试全绿)。
func (r *Registry) ModelVisibleActive() []*Skill {
	all := r.Active()
	result := make([]*Skill, 0, len(all))
	for _, s := range all {
		if s.ModelCallable() {
			result = append(result, s)
		}
	}
	return result
}

// WithStatus 按治理状态过滤 (状态名大小写不敏感; 传 "active" 时也匹配无 status 字段的技能)。
// 供进化门禁/操作台按状态盘点, 如 WithStatus("shadow") 列出待裁决技能。
func (r *Registry) WithStatus(statuses ...string) []*Skill {
	want := make(map[string]bool, len(statuses))
	for _, st := range statuses {
		want[strings.ToLower(strings.TrimSpace(st))] = true
	}
	all := r.All()
	result := make([]*Skill, 0, len(all))
	for _, s := range all {
		st := strings.ToLower(strings.TrimSpace(s.Status))
		if st == "" {
			st = StatusActive // 无 status 字段视为 active
		}
		if want[st] {
			result = append(result, s)
		}
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
	defer func() { r.mu.Lock(); r.pathsCache = nil; r.mu.Unlock() }()
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
	// 只暴露 active (shadow 未过进化门禁不进模型上下文) 且模型可调用 (F3:
	// ModelInvocable=false 的技能只作用户命令, 不进模型清单)。
	skills := r.ModelVisibleActive()
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

// shortListingDescBudget 是精简清单中"描述"部分的总字符(rune)预算。
// 名称不计入预算且永远写出; 描述在预算耗尽后被丢弃(仅留名称),
// 这样技能数量增长时 prompt 体积可控, 同时所有技能始终可被发现。
// ~2400 runes ≈ 600 tokens, 足以容纳数十个技能的描述。
const shortListingDescBudget = 2400

// truncateRunes 按 rune 截断, 避免切坏多字节 UTF-8 (如中文描述)。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "…"
}

// FormatShortListing 格式化精简技能清单。
// 用于长驻 Bot/system prompt: 只暴露可发现性，不把完整 when_to_use 清单重复塞进每次请求。
//
// 与旧实现的区别 (修复"技能数 >limit 时其余技能名称不可见、无法被 Skill 工具调用"的可发现性缺陷):
//   - 所有技能名称始终列出 (确定性排序), 不再随机隐藏在 "N more skills" 计数里;
//   - 描述受字符预算约束, 预算耗尽后只保留名称, 保证清单体积随技能增长平滑可控;
//   - limit (>0) 作为"展示完整描述的技能数"软上限; <=0 表示不设上限, 仅由预算约束。
func (r *Registry) FormatShortListing(limit int) string {
	skills := r.ModelVisibleActive() // 已按名称确定性排序; 排除 shadow/archived 与模型不可调用
	r.sortForListing(skills)         // 13.7-P2 L1 配给: 有 ranker 时描述位按后验优先分配
	return renderShortListing(skills, limit)
}

// FormatShortListingForNames 只列出给定名字的技能 (仍按名称确定性排序)。
//
// 用途: 团队 stage 的清单按**角色**裁剪。全量清单会把内置库里 28 个跨项目技能
// (Django / C++ / C# / 并发审查 / 品牌规范…) 塞进每一次请求 —— 实测某 stage 的
// system[0] 里 <available_skills> 占 3133 字节 / 35 条, 而 Skill 工具在全部
// 8516 次工具调用里 **0 次**被用过, 纯成本。
//
// 没列出来的技能并没有消失: RegisterPoolTools 把 ModelVisibleActive 全部登记为
// 池成员, 需要时 pool_search 检索 + pool_load 展开即可 —— 池对技能这一路此前
// 形同虚设, 恰恰因为清单本来就全量可见, 没人需要去搜。
func (r *Registry) FormatShortListingForNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	out := make([]*Skill, 0, len(names))
	for _, n := range names {
		s, ok := r.Get(n) // Get 已按运行期语义过滤 shadow/archived
		if !ok || !s.ModelCallable() {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return ""
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return renderShortListing(out, 0)
}

func renderShortListing(skills []*Skill, limit int) string {
	if len(skills) == 0 {
		return ""
	}
	if limit <= 0 {
		limit = len(skills)
	}

	var sb strings.Builder
	sb.WriteString("<available_skills summary=\"short\">\n")
	usedDesc := 0
	for i, s := range skills {
		line := "- " + s.Name
		// 名称恒写出; 描述受 limit 与剩余预算双重约束
		if i < limit && usedDesc < shortListingDescBudget && s.Description != "" {
			desc := truncateRunes(s.Description, 140)
			if usedDesc+len([]rune(desc)) <= shortListingDescBudget {
				line += ": " + desc
				usedDesc += len([]rune(desc))
			}
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("Use the Skill tool with a skill name to load its full instructions.\n")
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
	inPaths := false // paths 字段的 YAML 列表延续行状态
	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// paths 的多行列表形态:
		//   paths:
		//     - "**/*.go"
		//     - "cmd/**"
		if inPaths && strings.HasPrefix(line, "-") {
			item := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "-")), `"'`)
			if item != "" {
				skill.Paths = append(skill.Paths, item)
			}
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		inPaths = false // 新 key 行结束列表延续
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
		case "status":
			// 治理状态 (shadow/active/archived)。此前不解析 ⇒ shadow 技能照常进清单、
			// 照常可被 Skill 工具加载, `evo promote` 改的是没人读的文本字段。
			skill.Status = val
		case "model-invocable", "model_invocable":
			// 13.6 F3 双向调用策略: false → 技能只作用户命令, 不进模型清单、
			// Skill 工具拒载。缺省 (键不存在) = 双向可用, 向后兼容。
			b := parseBoolFM(val)
			skill.ModelInvocable = &b
		case "user-invocable", "user_invocable":
			// 13.6 F3: false → 只作模型可选项, 不进用户侧技能展示。
			b := parseBoolFM(val)
			skill.UserInvocable = &b
		case "allowed-tools", "allowed_tools":
			// 声明式工具面。**此前 Skill.AllowedTools 字段存在但全仓没有任何地方给它
			// 赋值**, 于是 SKILL.md 里写 `allowed-tools: Bash` 在结构体里恒为空 ——
			// 任何依据它做判断的代码都会得出"该技能没声明工具"这个错误结论。
			//
			// 目前唯一消费者是 pkg/evolution/govern 的不越权闸 (design/03 §4.6):
			// 自动提炼的技能若声明了写/执行类工具, 晋升被拒。运行期的工具可见性仍然
			// 只由 engine.Config.toolExposed 一处裁决, 本字段不参与 ⇒ 解析它不改变
			// 任何既有执行行为, 只是让治理层看得见声明。
			skill.AllowedTools = parseToolList(val)
		case "paths":
			// 行内形态 paths: ["**/*.go", "cmd/**"] (复用工具清单解析);
			// 多行 YAML 列表形态由上面的 inPaths 延续行处理 (val 为空时开启)。
			// paths 语义: 声明本技能适用的文件 glob——只在目录里存在命中文件时
			// 才进注入清单 (见 VisibleInDir)。此前字段存在但零解析零消费, 死字段。
			if val != "" {
				skill.Paths = parseToolList(val)
			} else {
				inPaths = true
			}
		}
	}
}

// parseBoolFM frontmatter 布尔值解析 (13.6 F3)。宽容: 1/t/T/true/TRUE/yes/on = true,
// 其余一律 false (写错配置按"关闭该侧调用"处理, fail-safe —— 配置错误不应把技能
// 悄悄暴露给模型)。
func parseBoolFM(val string) bool {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "1", "t", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// parseToolList 解析 frontmatter 里的工具清单。
//
// 兼容三种写法 (三种在野文件里都出现过): `A, B` / `[A, B]` / `A B`。
// 逗号优先, 无逗号才按空白切 —— 反过来会把 "Read, Bash" 切成 "Read," 这种带标点的
// 假工具名, 而治理层对"不认识的工具名"是拒绝晋升, 于是一个格式问题会变成一次误拒。
func parseToolList(val string) []string {
	val = strings.TrimSpace(strings.Trim(strings.TrimSpace(val), "[]"))
	if val == "" {
		return nil
	}
	var fields []string
	if strings.Contains(val, ",") {
		fields = strings.Split(val, ",")
	} else {
		fields = strings.Fields(val)
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if n := strings.Trim(strings.TrimSpace(f), `"'`); n != "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
	return "Load a skill's full instructions by name. Use this when the short skill listing or task intent indicates a relevant skill; if the name is unknown, ask for a likely name and the error response will list available skills."
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

	// 用 GetAny 取, 再自己判状态: 这样 shadow 技能能给出"存在但未过门禁"的准确错误,
	// 而不是让调用方以为名字打错了 (可排障, 也不泄露正文)。
	skill, ok := t.registry.GetAny(in.Name)
	if ok && !skill.IsActive() {
		return &tool.ToolResult{
			Content: fmt.Sprintf("技能 %q 处于 %s 态, 未通过进化门禁, 运行期不可加载 (需先晋升为 active)。",
				in.Name, strings.TrimSpace(skill.Status)),
			IsError: true,
		}, nil
	}
	// 13.6 F3: ModelInvocable=false 的技能对模型不可见, Skill 工具拒载 (用户侧仍可用)。
	if ok && !skill.ModelCallable() {
		return &tool.ToolResult{
			Content: fmt.Sprintf("技能 %q 配置为仅用户可调用 (model-invocable: false), 模型侧不可加载。", in.Name),
			IsError: true,
		}, nil
	}
	if !ok {
		// 提示清单与模型清单同口径: 只列 active 且模型可调用的技能
		available := make([]string, 0)
		for _, s := range t.registry.ModelVisibleActive() {
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

	// 13.6 F1 已注入记账: 记录注入时刻的 digest, 供持久注入方
	// (WasInjectedUnchanged) 判断"已注入且未变, 跳过重复注入"。
	t.registry.MarkInjected(skill.Name)

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
