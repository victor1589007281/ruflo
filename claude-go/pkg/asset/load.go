package asset

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadRoots 一次扫描的全部根目录 (按优先级从低到高)。
//
// 顺序即覆盖关系: 后加载的覆盖先加载的, 且每次覆盖都留一条 problem。
// 内建 < state < 项目, 与技能注册表 (pkg/skills.Registry.Rank) 的既有口径一致 ——
// 两处覆盖方向不一致会让"我改了项目里的那份怎么没生效"变成一个必须分别记住的问题。
type LoadRoots struct {
	// State 状态目录 (<stateDir>, 即 basedir.Layout.Root), 六种资产各取一个子目录,
	// 例如 <stateDir>/agents。这个形状与 basedir.Layout.Agents 等字段**逐字节一致** ——
	// 资产目录的权威定义在 basedir, 这里只是把它念出来。
	State string
	// Project 项目目录 (<cwd>), 取 <cwd>/.claude-go 与 <cwd>/.claude 两个约定目录。
	Project string
	// User 用户主目录 (<home>), 取 <home>/.claude-go 与 <home>/.claude。
	//
	// 为什么单独留一个显式字段而不是在 LoadStandard 里自己调 os.UserHomeDir:
	// 后者会让**每一个**调用方 (含全部测试) 都去扫真实的 home 目录。测试于是不再
	// 密闭 —— 开发机上恰好存在的一份 ~/.claude-go/agents/coder.md 会改变测试结果,
	// 而 CI 上没有它。留成字段, 生产传、测试不传。
	//
	// 它必须存在, 否则会出现"技能放 ~/.claude 能用, agent 放那儿不行" ——
	// pkg/skills 的 defaultSkillDirs 扫的就是这四个目录。
	User string
}

// LoadStandard 按标准布局装载六种资产, 返回索引与装载问题。
//
// 这是给"声明了这些目录"补上的**读取方** (21.8.12 P0/P1)。改造前
// basedir.Layout 声明了 Agents 与 Hooks 两个目录并幂等创建它们, 但全仓除了
// EnsureAll 没有任何地方读过 —— 一个每台机器上都存在、永远为空的目录, 让
// "资产放进去就会生效"从外部看完全成立。
//
// 不返回 error: 目录不存在是常态 (新机器上一个资产都没有), 真正的异常
// (文件读不了 / frontmatter 坏了 / kind 与目录不符) 全部进 Set.Problems()。
// 返回 error 会诱使调用方 `if err != nil { return }` 把剩下的目录一起跳过 ——
// 那正好是 loadCustomRoles 的形态 (三种失败一律 continue, 一个字都不报)。
func LoadStandard(roots LoadRoots) *Set {
	set := NewSet()
	for _, src := range standardSources(roots) {
		for _, k := range allKinds {
			LoadDir(src.dir(k), src.source, k, set)
		}
	}
	return set
}

// sourceUnit 一个来源 (state / project) 在某个根下的资产根目录。
type sourceUnit struct {
	source string
	root   string
	// layout 该来源的六种 kind → 目录名 (相对 root)。
	layout map[Kind]string
}

func (u sourceUnit) dir(k Kind) string {
	sub, ok := u.layout[k]
	if !ok {
		return ""
	}
	return filepath.Join(u.root, sub)
}

// standardLayout 六种资产在各根下的子目录名。
//
// 这是一张**表**而不是六处 `filepath.Join(root, "agents")`: 加一种资产格式只需加一行,
// 而散着写的话"新增的 kind 只在一个来源里被加载"这种漏是不会报错的 ——
// 它在另一个来源上表现为"放进去没反应"。
func standardLayout() map[Kind]string {
	return map[Kind]string{
		KindAgent:    "agents",
		KindWorkflow: "workflows",
		KindPlugin:   "plugins",
		KindSkill:    "skills",
		KindTool:     "tools",
		KindHook:     "hooks",
	}
}

// standardSources 按**优先级从低到高**列出全部装载来源。
//
// 顺序不是随手排的, 它同时决定了覆盖方向:
//
//	user    (低)  ~/.claude-go, ~/.claude
//	project       <cwd>/.claude-go, <cwd>/.claude
//	state   (高)  <stateDir>
//
// 这与 pkg/skills.SourceRank 的**分挡**一致 (user 200 / project·state 100), 且比它
// 更细一档 (project 与 state 同挡, 这里让 state 赢)。取这个方向的理由: stateDir 是
// 本机这份部署自己的目录, 最贴近"这台机器上的实际配置"; project 是仓库里随代码走的
// 约定; user 面向跨项目复用。后者不该覆盖前者。
//
// 两个约定目录都扫 (.claude-go 与 .claude): 前者是 claude-go 自己的, 后者是对
// Claude Code 目录约定的兼容 —— 技能的 defaultSkillDirs 已经同时认这两个, 资产
// 不认会让"技能放那儿能用, agent 放那儿不行"变成一条没人知道的规则。
func standardSources(roots LoadRoots) []sourceUnit {
	layout := standardLayout()
	var units []sourceUnit
	add := func(source, root string) {
		root = strings.TrimSpace(root)
		if root == "" {
			return
		}
		units = append(units, sourceUnit{source: source, root: root, layout: layout})
	}
	if u := strings.TrimSpace(roots.User); u != "" {
		add("user", filepath.Join(u, ".claude-go"))
		add("user", filepath.Join(u, ".claude"))
	}
	if p := strings.TrimSpace(roots.Project); p != "" {
		add("project", filepath.Join(p, ".claude-go"))
		add("project", filepath.Join(p, ".claude"))
	}
	if s := strings.TrimSpace(roots.State); s != "" {
		add("state", s)
	}
	return units
}

// DefaultRoots 按生产约定拼出装载根: 用户主目录 + 项目目录 + 状态目录。
//
// 存在的理由只有一个 —— 让**生产的那一份**与**测试的那一份**写得出来又对得上:
// 三处调用点 (飞书 bot / CLI / dashboard) 各拼一遍必然漂移, 而漂移的表现是
// "某个入口看不到某个目录里的资产", 没有任何报错。
//
// stateDir 传空时 State 来源为空 (只扫 user 与 project): CLI 的许多子命令没有
// stateDir, 那不是异常。home 取不到时 User 来源为空 (与 os.UserHomeDir 的既有口径
// 一致, 不报错)。
func DefaultRoots(cwd, stateDir string) LoadRoots {
	r := LoadRoots{Project: strings.TrimSpace(cwd), State: strings.TrimSpace(stateDir)}
	if home, err := os.UserHomeDir(); err == nil {
		r.User = home
	}
	return r
}

// Describe 返回一行装载摘要: 各 kind 条数 + **实际存在的**资产根 + 问题数。
//
// 存在的理由是"降级必须可见": 一个资产目录不存在与它是空的, 从索引上完全一样 ——
// 而"我把 agent 放进去了没生效"要能在一行日志里看出是哪一段断了。所以这里列出
// 磁盘上真实存在的根 (不存在的根不列, 否则摘要会被一堆路径淹没), 以及问题的条数
// (具体明细由 DedupReport 给)。
func (s *Set) Describe(roots LoadRoots) string {
	var b strings.Builder
	b.WriteString(s.Summary())
	var present []string
	for _, u := range standardSources(roots) {
		for _, k := range allKinds {
			d := u.dir(k)
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				present = append(present, u.source+":"+d)
			}
		}
	}
	sort.Strings(present)
	if len(present) > 0 {
		fmt.Fprintf(&b, " | 资产根 %s", strings.Join(present, " "))
	}
	return b.String()
}

// DedupReport 把 problem 按路径排序后的可读清单 (供启动日志一行一条地打)。
//
// 排序: problem 的产生顺序取决于 ReadDir 与 map 遍历, 不排序的日志两次运行不一样,
// 没法用来对比"这次比上次少了哪几条"。资产名的排序同理 (Names 已排序)。
func (s *Set) DedupReport() []string {
	probs := s.Problems()
	sort.Slice(probs, func(i, j int) bool {
		if probs[i].Path != probs[j].Path {
			return probs[i].Path < probs[j].Path
		}
		return probs[i].Err < probs[j].Err
	})
	out := make([]string, 0, len(probs))
	for _, p := range probs {
		out = append(out, p.Error())
	}
	return out
}
