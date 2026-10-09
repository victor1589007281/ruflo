package asset

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Kind 资产种类 (21.8.12 P1)。
//
// 六种, 不多不少 —— 它们各自在改造前就有自己的结构体与目录, 这里的价值不是"发明"
// 而是**给它们一个共同的名字**, 使得"这台机器上声明了哪些资产""这个名字属于哪一类"
// 变成可以问的问题。此前问不出来: 角色在 RoleRegistry, 工作流在 customWFs + 内置
// registry, 技能在 skills.Registry, 工具在 tool.Registry, hook 在 types.HookConfig
// 切片里, 插件在 dynmcp.Manager 里 —— 六个容器, 没有任何一处能列全。
type Kind string

const (
	KindAgent    Kind = "agent"
	KindWorkflow Kind = "workflow"
	KindPlugin   Kind = "plugin"
	KindSkill    Kind = "skill"
	KindTool     Kind = "tool"
	KindHook     Kind = "hook"
)

// allKinds 声明序即展示序 (稳定, 不依赖 map 遍历)。
var allKinds = []Kind{KindAgent, KindWorkflow, KindPlugin, KindSkill, KindTool, KindHook}

// Kinds 返回全部合法 kind (副本, 调用方可随意改)。
func Kinds() []Kind { return append([]Kind(nil), allKinds...) }

// Valid 是否是一个已声明的 kind。
func (k Kind) Valid() bool {
	for _, x := range allKinds {
		if k == x {
			return true
		}
	}
	return false
}

// ParseKind 解析 kind 字符串 (容忍大小写与空白)。
//
// **不认识的值一律报错**, 不回落成某个默认 kind: 一个把 `kind: agnet` 静默当成
// agent 的解析器, 会让"我明明写了 plugin 怎么没生效"变成一个查不出来的问题。
func ParseKind(s string) (Kind, error) {
	k := Kind(strings.ToLower(strings.TrimSpace(s)))
	if k.Valid() {
		return k, nil
	}
	names := make([]string, len(allKinds))
	for i, x := range allKinds {
		names[i] = string(x)
	}
	return "", fmt.Errorf("asset: 未知的 kind %q (合法取值: %s)", s, strings.Join(names, " | "))
}

// Asset 一份资产声明 (来自一个带 frontmatter 的 .md 文件)。
//
// 字段的选择标准是"六种 kind 都有意义的那些": 名称/描述/版本/状态/正文/来源。
// kind 专属的东西 (工作流的 stages、hook 的 event、工具的参数 schema) 不在结构体
// 里, 而是留在 FM 里由各自的装载器取 —— 把它们全塞进来会长出一个 90% 字段对 90%
// 的 kind 都无意义的超结构体, 那正是这套东西第一次没做成的形态。
type Asset struct {
	Kind        Kind
	Name        string
	Description string
	Version     string
	// Status 治理状态 (shadow | active | archived)。空 = 未声明。
	//
	// 与 skills.Skill.Status 同一取值域: `evo promote` 改的就是它, 而它此前在
	// 技能上长期是"解析了但没人读"的死字段。这里把它提成一等字段, 让"声明了状态
	// 却没有任何消费方"这件事在结构上就看得出来。
	Status     string
	Body       string
	Source     string // 来源标记: builtin | state | project | user ...
	SourcePath string
	// FM 原始 frontmatter, 供各 kind 的装载器取自己的专属键。
	FM Frontmatter
}

// Key 资产的稳定标识, 形如 "agent:coder" / "skill:commit"。
//
// 与进化引擎的 Proposal.Target 同形 (见 learners.FormatTarget): 那套谱系/版本机制
// 本来就只认字符串目标, 把目标写成 "<kind>:<name>" 之后, 版本与谱系对六种资产**一次**
// 全部可用, 而不必为每种资产各写一套。
func (a *Asset) Key() string {
	if a == nil {
		return ""
	}
	return string(a.Kind) + ":" + a.Name
}

// Meta 取一个 kind 专属的单值键。
func (a *Asset) Meta(key string) string {
	if a == nil {
		return ""
	}
	return a.FM.Get(key)
}

// MetaList 取一个 kind 专属的列表键: 先看多行形态, 再看行内形态。
//
// 两种形态都要看, 因为在野文件里两种都有 (多行列表可读性好, 行内省行数), 而只认
// 一种的解析器会把另一种读成空 —— 空列表与"没声明"在下游是同一个分支。
//
// ★ 本函数**丢掉了"这个键写没写"这件事**: `tools: []` 与"没写 tools" 都返回 nil。
// 需要那个区分的调用方要用 MetaListSet。这里保持旧行为是为了不动既有调用方 ——
// 而"丢掉存在性"正是 MetaListSet 存在的理由, 见它的注释。
func (a *Asset) MetaList(key string) []string {
	v, _ := a.MetaListSet(key)
	return v
}

// MetaListSet 与 MetaList 同, 但**额外回答"这个键写了没有"** (v, ok)。
//
// ---------------------------------------------------------------------------
// 为什么存在性必须能取到: `[]` 与"没写"的语义相反
// ---------------------------------------------------------------------------
//
// 工具白名单这一面把一个**显式空名单**定义成 fail-closed —— 拒绝一切工具
// (见 engine.Config.AllowedTools 与 assembly.Declaration.Tools 两处注释)。
// 于是:
//
//	没写 tools:      → 跟随调用方基线, 该有什么有什么
//	tools: []        → 一个工具都不给
//
// 两者在 frontmatter 层的**取值**恰好一样 (长度 0 的列表), 差别只在键在不在。
// MetaList 把键的存在性吃掉之后, 上面第二条就**永远表达不出来**: 一份故意写的
// fail-closed 声明会静默退化成"什么限制都没有"。这不是理论问题 ——
// TestCLI显式空白名单拒绝一切 就是照这条写出来、然后在改造前的实现上变红的。
//
// 前端 (pkg/asset/frontmatter.go) 本来就把三态分得很清 (Present / 空块 / 有值),
// 是这一层把它抹平的; 补回存在性而不是重写解析, 就是为了让那条已有的区分真的可达。
//
// ok 的判据是"键出现在 Values 或 Lists 里"。多行列表形态下要点名 `tools:` 才会建
// 出 Lists 条目, 所以 `- item` 缩进行写在别的键下面**不会**被误当成声明 —— 这与
// MetaList 逐项一致。
func (a *Asset) MetaListSet(key string) ([]string, bool) {
	if a == nil {
		return nil, false
	}
	k := NormalizeKey(key)
	if v, ok := a.FM.Lists[k]; ok {
		return v, true
	}
	raw, ok := a.FM.Values[k]
	if !ok {
		return nil, false
	}
	v := ParseInlineList(raw)
	// `tools: []` —— 显式空名单。ParseInlineList 对剥掉括号后的空串返回 nil,
	// 而这里必须回一个**非 nil** 的空切片: 下游靠 nil 与非 nil 分辨"没声明"与
	// "声明为空" (WithToolAllowList 的参数注释把这条写死了)。
	if len(v) == 0 && strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), `"'`)) == "[]" {
		return []string{}, true
	}
	return v, true
}

// IsActive 资产是否可被正常使用。空状态视为 active (缺省即可用, 与技能一致)。
func (a *Asset) IsActive() bool {
	if a == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(a.Status)) {
	case "", "active":
		return true
	default: // shadow / archived / 以及任何写错的值
		return false
	}
}

// ParseContent 从内存内容解析资产。sourcePath 用于推导默认名字与记录来源。
func ParseContent(content, sourcePath, source string, kind Kind) (*Asset, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("asset: 解析 %s 时 kind %q 非法", sourcePath, kind)
	}
	fm := SplitFrontmatter(content)
	a := &Asset{
		Kind:        kind,
		Name:        fm.Get("name"),
		Description: fm.Get("description"),
		Version:     fm.Get("version"),
		Status:      fm.Get("status"),
		Body:        fm.Body,
		Source:      source,
		SourcePath:  sourcePath,
		FM:          fm,
	}
	if a.Name == "" {
		a.Name = DefaultNameForPath(sourcePath, kind)
	}
	// 文件自己声明了 kind 时以文件为准 —— 但**只在它与所在目录一致时**。
	// 不一致时不静默采纳, 由 LoadDir 报成 problem: 一个躺在 agents/ 里却声明
	// `kind: hook` 的文件, 静默按 hook 装载会让"我放进 agents/ 了怎么没生效"没法解释,
	// 静默按 agent 装载则让文件里的声明变成一句谎话。两条路都是"配置说了不生效"。
	if decl := fm.Get("kind"); decl != "" {
		dk, err := ParseKind(decl)
		if err != nil {
			return nil, fmt.Errorf("asset: %s: %w", sourcePath, err)
		}
		if dk != kind {
			return nil, fmt.Errorf("asset: %s 声明 kind=%s 但位于 %s 目录 (两者必须一致)", sourcePath, dk, kind)
		}
	}
	if a.Description == "" {
		a.Description = firstProseLine(a.Body)
	}
	if strings.TrimSpace(a.Name) == "" {
		return nil, fmt.Errorf("asset: %s 既没有 name 字段也无法从路径推导出名字", sourcePath)
	}
	return a, nil
}

// ParseFile 读盘并解析一份资产。
func ParseFile(path, source string, kind Kind) (*Asset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("asset: 读取 %s 失败: %w", path, err)
	}
	return ParseContent(string(data), path, source, kind)
}

// DefaultNameForPath 从路径推导资产名。
//
// 支持两种在野布局, 都取自技能的既有约定 (`<dir>/<name>/SKILL.md` 与平铺单文件):
//
//	<dir>/coder.md          → coder
//	<dir>/coder/AGENT.md    → coder
//	<dir>/coder/SKILL.md    → coder
//
// 目录形态**优先于**文件名: 后者如果按文件名取会得到 "AGENT" / "SKILL" 这种把所有
// 资产都叫同一个名字的结果。
func DefaultNameForPath(path string, kind Kind) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if strings.EqualFold(stem, string(kind)) || isGenericDocName(stem) {
		if dir := filepath.Dir(path); dir != "" && dir != "." && dir != string(filepath.Separator) {
			if n := filepath.Base(dir); n != "" && n != "." {
				return n
			}
		}
	}
	return stem
}

// isGenericDocName 文件名本身不携带名字信息的那几种 (大小写不敏感)。
func isGenericDocName(stem string) bool {
	switch strings.ToUpper(stem) {
	case "AGENT", "SKILL", "HOOK", "TOOL", "PLUGIN", "WORKFLOW", "ASSET", "INDEX":
		return true
	}
	return false
}

// firstProseLine 取正文里第一行不是标题也不是空行的内容, 作为缺省描述。
func firstProseLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 描述要能塞进一行清单, 长段落截断 (按 rune 截, 免得把一个汉字劈成两半)。
		return TruncateRunes(line, 200)
	}
	return ""
}

// TruncateRunes 按 rune 截断 (不劈开多字节字符)。
func TruncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// ---------------------------------------------------------------------------
// Set: 资产索引
// ---------------------------------------------------------------------------

// Problem 一条装载问题。**不是错误返回而是收集品**: 一个目录里十个文件坏了六个,
// 剩下四个仍然应该可用, 但坏掉的那六个必须能被说出来。
//
// 这条纪律来自 pkg/agent.loadCustomRoles 的反面教材: 它对"目录不存在 / 文件读不了 /
// JSON 坏了"三种情况一律 `continue`, 于是"我放进去的角色怎么没生效"在日志里一个字
// 都找不到。
type Problem struct {
	Path string `json:"path"`
	Err  string `json:"err"`
}

func (p Problem) Error() string { return p.Path + ": " + p.Err }

// Set 资产索引 (按 kind 分桶)。
type Set struct {
	mu       sync.RWMutex
	byKind   map[Kind]map[string]*Asset
	problems []Problem
}

// NewSet 创建一个空索引。
func NewSet() *Set {
	s := &Set{byKind: map[Kind]map[string]*Asset{}}
	for _, k := range allKinds {
		s.byKind[k] = map[string]*Asset{}
	}
	return s
}

// Add 放入一份资产。同名后到者覆盖先到者, 但**留下一条 problem 记录**:
// 覆盖是有意的能力 (state 目录覆盖 builtin), 而静默覆盖会让"我改了那个文件怎么没用"
// 变成无从查起 —— 两件事都要。
func (s *Set) Add(a *Asset) {
	if s == nil || a == nil || a.Name == "" || !a.Kind.Valid() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.byKind[a.Kind][a.Name]; dup {
		s.problems = append(s.problems, Problem{
			Path: a.SourcePath,
			Err:  fmt.Sprintf("覆盖了先加载的同名 %s 资产 %q", a.Kind, a.Name),
		})
	}
	s.byKind[a.Kind][a.Name] = a
}

// AddProblem 记一条装载问题。
func (s *Set) AddProblem(path string, err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problems = append(s.problems, Problem{Path: path, Err: err.Error()})
}

// Get 取一份资产 (不存在返回 nil)。
func (s *Set) Get(kind Kind, name string) *Asset {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byKind[kind][name]
}

// Names 返回某 kind 下全部资产名 (**已排序**)。
//
// 排序不是美观问题: 这些名字会进 LLM 提示词与 dashboard 列表, map 遍历序会让同一份
// 配置两次运行给出不同的提示词 —— 进而不同的产出与不同的缓存命中。角色注册表
// (pkg/agent.roles.go Names) 至今是未排序的, 这里不重复那个错。
func (s *Set) Names(kind Kind) []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.byKind[kind]))
	for n := range s.byKind[kind] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Count 某 kind 的资产数。
func (s *Set) Count(kind Kind) int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byKind[kind])
}

// Total 全部资产的条数。
func (s *Set) Total() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.byKind {
		n += len(m)
	}
	return n
}

// Problems 返回装载问题 (副本)。
func (s *Set) Problems() []Problem {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Problem(nil), s.problems...)
}

// Summary 一行汇总, 供启动日志与装配报告引用 (降级必须可见)。
func (s *Set) Summary() string {
	if s == nil {
		return "资产: 无索引"
	}
	parts := make([]string, 0, len(allKinds))
	for _, k := range allKinds {
		if n := s.Count(k); n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", k, n))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "空")
	}
	return fmt.Sprintf("资产: %s (问题 %d 条)", strings.Join(parts, " "), len(s.Problems()))
}

// ---------------------------------------------------------------------------
// 目录装载
// ---------------------------------------------------------------------------

// DirLayout 描述一个资产目录的两种在野布局。
//
// 为什么要两种: 技能的约定是 `<dir>/<name>/SKILL.md`, 而角色/agent 的自然写法是
// `<dir>/<name>.md`。两种在野都存在, 只认一种会让另一批文件"放进去没反应"。
var docNames = map[Kind][]string{
	KindAgent:    {"AGENT.md", "SKILL.md"},
	KindSkill:    {"SKILL.md", "AGENT.md"},
	KindWorkflow: {"WORKFLOW.md"},
	KindPlugin:   {"PLUGIN.md", "README.md"},
	KindTool:     {"TOOL.md"},
	KindHook:     {"HOOK.md"},
}

// LoadDir 从目录装载某 kind 的全部资产。
//
// 目录不存在**不是错误** (视为空目录), 但目录存在而某个文件坏了是 problem —— 这个
// 区分很重要: 前者是"没配", 后者是"配了没生效", 两者的处置完全不同。
//
// 不递归: 一种资产的目录层级由 docNames 决定, 让加载器去猜"这个子目录算不算"会
// 引入一套没有消费者的规则。
func LoadDir(dir string, source string, kind Kind, set *Set) {
	if set == nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			set.AddProblem(dir, fmt.Errorf("读取目录失败: %w", err))
		}
		return
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			for _, doc := range docNames[kind] {
				p := filepath.Join(full, doc)
				if st, err := os.Stat(p); err == nil && !st.IsDir() {
					paths = append(paths, p)
					break
				}
			}
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			paths = append(paths, full)
		}
	}
	sort.Strings(paths) // 装载顺序稳定 ⇒ 覆盖关系稳定 ⇒ 提示词稳定
	for _, p := range paths {
		a, err := ParseFile(p, source, kind)
		if err != nil {
			set.AddProblem(p, err)
			continue
		}
		set.Add(a)
	}
}
