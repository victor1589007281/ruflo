package asset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// frontmatter 解析
// ---------------------------------------------------------------------------

func TestSplitFrontmatterBasic(t *testing.T) {
	fm := SplitFrontmatter(`---
name: coder
description: 写代码的
version: "2"
status: active
allowed-tools: Read, Bash, Edit
---
正文第一行
更多正文`)
	if !fm.Present {
		t.Fatal("应识别出 frontmatter")
	}
	if fm.Get("name") != "coder" || fm.Get("status") != "active" {
		t.Fatalf("单值键解析不对: %+v", fm.Values)
	}
	// 引号要被去掉
	if fm.Get("version") != "2" {
		t.Fatalf("引号未去除: %q", fm.Get("version"))
	}
	// 行内形态要经 ParseInlineList: fm.List 只认多行形态, 这是刻意的分工
	// (前者是"块里怎么写的", 后者是"值怎么解读的"), Asset.MetaList 把两者接在一起。
	if got := ParseInlineList(fm.Get("allowed-tools")); len(got) != 3 || got[0] != "Read" || got[2] != "Edit" {
		t.Fatalf("行内工具列表: %+v", got)
	}
	if !strings.HasPrefix(fm.Body, "正文第一行") {
		t.Fatalf("正文不对: %q", fm.Body)
	}
}

// TestSplitFrontmatterCRLF 是**回归测试**: 改造前 CRLF 文件会让整个 frontmatter 静默
// 消失 —— 表现成"这个文件本来就没写 frontmatter", 名字回落目录名、描述回落正文首行,
// 一个字都不报。所以这条判据必须同时断正反两面。
func TestSplitFrontmatterCRLF(t *testing.T) {
	crlf := strings.ReplaceAll(`---
name: from-crlf
status: shadow
---
body here`, "\n", "\r\n")

	fm := SplitFrontmatter(crlf)
	if !fm.Present {
		t.Fatal("CRLF 文件应仍识别出 frontmatter (改造前这里是整块丢弃)")
	}
	if fm.Get("name") != "from-crlf" {
		t.Fatalf("CRLF 下 name 解析失败: %q", fm.Get("name"))
	}
	if fm.Get("status") != "shadow" {
		t.Fatalf("CRLF 下 status 解析失败: %q (shadow 丢了 ⇒ 影子资产会被当活跃资产)", fm.Get("status"))
	}
	// 负: 没有 frontmatter 的内容不能被误判成有
	if SplitFrontmatter("just text\n---\nmore").Present {
		t.Fatal("不以 --- 开头的内容不该被判成有 frontmatter")
	}
}

// TestSplitFrontmatterMultilineLists 断言多行列表对所有键都生效 (旧实现只认 paths)。
func TestSplitFrontmatterMultilineLists(t *testing.T) {
	fm := SplitFrontmatter(`---
name: x
tools:
  - Read
  - "Bash"
paths:
  - "**/*.go"
tags: [a, b]
---
b`)
	got := fm.List("tools")
	if len(got) != 2 || got[0] != "Read" || got[1] != "Bash" {
		t.Fatalf("多行列表解析不对: %+v", got)
	}
	if got := fm.List("paths"); len(got) != 1 || got[0] != "**/*.go" {
		t.Fatalf("paths 多行列表: %+v", got)
	}
	// 键名归一: tools 与 Tools / TOOLS 是同一个键
	if fm.Get("TOOLS") == "" && fm.List("Tools") == nil {
		t.Fatal("键名未归一")
	}
}

// TestSplitFrontmatterUnterminated 断言"有开无闭"时不吞掉文档:
// 旧实现此时把前半段当 frontmatter 丢掉, 整篇文档少一半。
func TestSplitFrontmatterUnterminated(t *testing.T) {
	fm := SplitFrontmatter("---\nname: x\n没有结束标记的正文")
	if fm.Present {
		t.Fatal("没有闭合 --- 时不该判成有 frontmatter")
	}
	if !strings.Contains(fm.Body, "没有结束标记的正文") {
		t.Fatalf("正文不该被吞掉: %q", fm.Body)
	}
}

func TestNormalizeKey(t *testing.T) {
	for _, in := range []string{"allowed-tools", "allowed_tools", "ALLOWED_TOOLS", " Allowed-Tools "} {
		if got := NormalizeKey(in); got != "allowed-tools" {
			t.Fatalf("NormalizeKey(%q) = %q", in, got)
		}
	}
}

func TestParseInlineList(t *testing.T) {
	cases := map[string][]string{
		`Read, Bash, Edit`: {"Read", "Bash", "Edit"},
		`[Read, Bash]`:     {"Read", "Bash"},
		`Read Bash Edit`:   {"Read", "Bash", "Edit"},
		// 整串加引号时引号只被当成普通字符去掉两端, 逗号仍在 ⇒ 切成两项。
		// 与 pkg/skills.parseToolList 逐字节同规则, 这是刻意的: 两边面对的是同一批
		// 在野文件, 分叉会让同一个文件在两个解析器里得到不同的工具集。
		`"Read, Bash"`: {"Read", "Bash"},
		``:             nil,
		`  `:           nil,
	}
	for in, want := range cases {
		got := ParseInlineList(in)
		if len(got) != len(want) {
			t.Fatalf("ParseInlineList(%q) = %+v, want %+v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ParseInlineList(%q) = %+v, want %+v", in, got, want)
			}
		}
	}
}

// TestParseBoolFailSafe 断言写错的值一律判 false (权限扩张方向必须是关闭)。
func TestParseBoolFailSafe(t *testing.T) {
	for _, s := range []string{"1", "t", "TRUE", "yes", "on", " Yes "} {
		if !ParseBool(s) {
			t.Fatalf("%q 应判 true", s)
		}
	}
	for _, s := range []string{"", "0", "false", "no", "off", "ture", "maybe"} {
		if ParseBool(s) {
			t.Fatalf("%q 应判 false (写错时按关闭处理)", s)
		}
	}
}

// ---------------------------------------------------------------------------
// Kind
// ---------------------------------------------------------------------------

func TestParseKindRejectsUnknown(t *testing.T) {
	if k, err := ParseKind(" AGENT "); err != nil || k != KindAgent {
		t.Fatalf("大小写/空白应被容忍: %v %v", k, err)
	}
	for _, bad := range []string{"", "agnet", "agents", "workflow ", "unknown"} {
		if bad == "workflow " {
			continue // 空白会被 trim 掉, 这是有意的
		}
		if _, err := ParseKind(bad); err == nil {
			t.Fatalf("kind %q 应被拒绝 (静默回落会让'我写了 plugin 怎么没生效'查不出来)", bad)
		}
	}
	if k, err := ParseKind("workflow"); err != nil || k != KindWorkflow {
		t.Fatalf("workflow 应是合法 kind: %v %v", k, err)
	}
	// 每个声明的 kind 都必须能往返
	for _, k := range Kinds() {
		if got, err := ParseKind(string(k)); err != nil || got != k {
			t.Fatalf("kind %q 往返失败: %v %v", k, got, err)
		}
	}
}

func TestKindsIsCopy(t *testing.T) {
	k := Kinds()
	k[0] = "mutated"
	if Kinds()[0] == "mutated" {
		t.Fatal("Kinds() 必须返回副本, 否则调用方能改掉全局声明序")
	}
}

// ---------------------------------------------------------------------------
// 解析资产
// ---------------------------------------------------------------------------

func TestParseContentNameFallbacks(t *testing.T) {
	// 1) frontmatter 里的 name 优先
	a, err := ParseContent("---\nname: explicit\n---\nbody", "/x/y/coder.md", "state", KindAgent)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "explicit" {
		t.Fatalf("name 未取 frontmatter: %q", a.Name)
	}
	// 2) 平铺文件按文件名
	a, err = ParseContent("body", "/x/agents/coder.md", "state", KindAgent)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "coder" {
		t.Fatalf("平铺文件应按文件名取名: %q", a.Name)
	}
	// 3) <dir>/<name>/AGENT.md 按目录名 (不是 "AGENT")
	a, err = ParseContent("body", "/x/agents/reviewer/AGENT.md", "state", KindAgent)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "reviewer" {
		t.Fatalf("目录形态应按目录名取名, got %q (取成 AGENT 会让所有资产同名)", a.Name)
	}
	// 4) 技能形态同样按目录名
	a, err = ParseContent("body", "/x/skills/commit/SKILL.md", "state", KindSkill)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "commit" {
		t.Fatalf("SKILL.md 应按目录名取名: %q", a.Name)
	}
}

// TestParseContentKindMismatch 断言文件声明与所在目录不一致时报错, 不静默选一边。
func TestParseContentKindMismatch(t *testing.T) {
	_, err := ParseContent("---\nkind: hook\n---\nb", "/x/agents/coder.md", "state", KindAgent)
	if err == nil {
		t.Fatal("目录 agents/ 里声明 kind=hook 应报错 (静默按任一边装载都是'配置说了不生效')")
	}
	// 一致时应通过
	if _, err := ParseContent("---\nkind: agent\n---\nb", "/x/agents/coder.md", "state", KindAgent); err != nil {
		t.Fatalf("一致的 kind 声明不该报错: %v", err)
	}
	// 写错的 kind 值应报错
	if _, err := ParseContent("---\nkind: agnet\n---\nb", "/x/agents/coder.md", "state", KindAgent); err == nil {
		t.Fatal("拼错的 kind 值应报错")
	}
}

func TestAssetDerivedFields(t *testing.T) {
	a, err := ParseContent(`---
name: coder
status: SHADOW
allowed-tools:
  - Read
  - Bash
---
# 标题
写代码的 agent。
第二行`, "/x/agents/coder.md", "state", KindAgent)
	if err != nil {
		t.Fatal(err)
	}
	if a.Key() != "agent:coder" {
		t.Fatalf("Key 形状不对 (必须与 learners 的 target 同形): %q", a.Key())
	}
	if a.IsActive() {
		t.Fatal("status=SHADOW 不该是 active (大小写不能被当成另一个状态)")
	}
	if got := a.MetaList("allowed-tools"); len(got) != 2 || got[0] != "Read" {
		t.Fatalf("MetaList 多行形态: %+v", got)
	}
	// 描述缺省取正文第一行非标题内容
	if a.Description != "写代码的 agent。" {
		t.Fatalf("描述缺省不对: %q", a.Description)
	}
	// 空 status = active
	b, _ := ParseContent("---\nname: x\n---\nb", "/x/agents/x.md", "s", KindAgent)
	if !b.IsActive() {
		t.Fatal("未声明 status 应视为 active")
	}
}

func TestTruncateRunesDoesNotSplitMultibyte(t *testing.T) {
	got := TruncateRunes("一二三四五", 3)
	if !strings.HasPrefix(got, "一二三") {
		t.Fatalf("按 rune 截断失败: %q", got)
	}
	if TruncateRunes("abc", 5) != "abc" {
		t.Fatal("短于上限不应改动")
	}
}

// ---------------------------------------------------------------------------
// Set + LoadDir
// ---------------------------------------------------------------------------

func TestSetGetNamesSortedAndOverwriteRecorded(t *testing.T) {
	s := NewSet()
	s.Add(&Asset{Kind: KindAgent, Name: "zeta", SourcePath: "/a/zeta.md"})
	s.Add(&Asset{Kind: KindAgent, Name: "alpha", SourcePath: "/a/alpha.md"})
	names := s.Names(KindAgent)
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zeta" {
		t.Fatalf("Names 必须排序 (未排序会让同一份配置两次运行给出不同的提示词): %+v", names)
	}
	// 覆盖: 后到者赢, 但必须留痕
	s.Add(&Asset{Kind: KindAgent, Name: "alpha", SourcePath: "/b/alpha.md"})
	if got := s.Get(KindAgent, "alpha"); got.SourcePath != "/b/alpha.md" {
		t.Fatalf("后到的资产应覆盖: %+v", got)
	}
	if len(s.Problems()) != 1 {
		t.Fatalf("覆盖必须留下一条 problem (静默覆盖会让'我改了那个文件怎么没用'无从查起): %+v", s.Problems())
	}
	if s.Count(KindAgent) != 2 {
		t.Fatalf("覆盖不该增加计数: %d", s.Count(KindAgent))
	}
	if s.Get(KindHook, "alpha") != nil {
		t.Fatal("不同 kind 之间不该串")
	}
}

func TestLoadDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agents")
	if err := os.MkdirAll(filepath.Join(dir, "reviewer"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, c string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "coder.md"), "---\nname: coder\ndescription: 写代码\n---\nbody")
	write(filepath.Join(dir, "reviewer", "AGENT.md"), "---\nname: reviewer\n---\nbody")
	write(filepath.Join(dir, "broken.md"), "---\nkind: nope\n---\nb")
	write(filepath.Join(dir, "notmarkdown.txt"), "ignored")

	s := NewSet()
	LoadDir(dir, "state", KindAgent, s)

	if got := s.Names(KindAgent); len(got) != 2 || got[0] != "coder" || got[1] != "reviewer" {
		t.Fatalf("应装到 coder 与 reviewer: %+v", got)
	}
	// 坏文件必须被**报出来**, 而不是像 loadCustomRoles 那样 continue 掉
	probs := s.Problems()
	if len(probs) != 1 || !strings.Contains(probs[0].Path, "broken.md") {
		t.Fatalf("坏文件应产生一条 problem: %+v", probs)
	}
	if !strings.Contains(s.Summary(), "agent=2") {
		t.Fatalf("Summary 应含计数: %q", s.Summary())
	}

	// 目录不存在 = 空目录, **不是** problem (没配 vs 配了没生效是两件事)
	s2 := NewSet()
	LoadDir(filepath.Join(root, "nonexistent"), "state", KindAgent, s2)
	if len(s2.Problems()) != 0 {
		t.Fatalf("目录不存在不该报 problem: %+v", s2.Problems())
	}
	if s2.Total() != 0 {
		t.Fatal("不存在的目录应装载 0 条")
	}
}

// TestLoadDirOnlyMatchingKind 断言同一个目录里混进别的 kind 的文件不会被装进来。
func TestLoadDirOnlyMatchingKind(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "h.md"),
		[]byte("---\nkind: hook\nname: h\n---\nb"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSet()
	LoadDir(dir, "state", KindAgent, s)
	if s.Count(KindAgent) != 0 {
		t.Fatalf("声明 kind=hook 的文件不该装进 agent: %+v", s.Names(KindAgent))
	}
	if len(s.Problems()) != 1 {
		t.Fatalf("它应产生一条 problem: %+v", s.Problems())
	}
}

// ---------------------------------------------------------------------------
// LoadStandard: 标准布局
// ---------------------------------------------------------------------------

// TestLoadStandardCoversEveryKind 断言六种资产在**每个**来源里都被扫到。
//
// 这条判据的形状是关键: 它不逐个 kind 写断言 (那样加一种 kind 时测试不会红),
// 而是遍历 asset.Kinds() —— 于是"新增一种资产格式但只在 state 来源里加载"
// 这种漏会当场现形。散着写标准布局正是这种漏的生成机制。
func TestLoadStandardCoversEveryKind(t *testing.T) {
	state, project := t.TempDir(), t.TempDir()
	// 每个来源用**不同的名字**, 否则同名互相覆盖, 断言就分不出"装到了"与"被盖掉了"。
	sources := []struct{ name, root string }{
		{"a", filepath.Join(project, ".claude-go")},
		{"b", filepath.Join(project, ".claude")},
		{"c", state},
	}
	for _, src := range sources {
		for _, k := range Kinds() {
			dir := filepath.Join(src.root, standardLayout()[k])
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := "---\nname: " + src.name + "-" + string(k) + "\n---\nb"
			if err := os.WriteFile(filepath.Join(dir, src.name+"-"+string(k)+".md"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := LoadStandard(LoadRoots{State: state, Project: project})
	for _, k := range Kinds() {
		if n := s.Count(k); n != len(sources) {
			t.Fatalf("%s 应有 %d 份 (每个来源一份), got %d — 某个来源没扫到该 kind", k, len(sources), n)
		}
		for _, src := range sources {
			if s.Get(k, src.name+"-"+string(k)) == nil {
				t.Fatalf("%s 来源 %s 的 %s 资产没被装载 (标准布局里漏了这个来源)", k, src.root, k)
			}
		}
	}
	if n := s.Total(); n != len(sources)*len(Kinds()) {
		t.Fatalf("总数应为 %d, got %d", len(sources)*len(Kinds()), n)
	}
	if len(s.Problems()) != 0 {
		t.Fatalf("名字不冲突时不该有 problem: %+v", s.Problems())
	}
}

// TestLoadStandardOverwriteDirection 断言覆盖方向是 state > project > 项目内 .claude,
// 且**每次覆盖都留痕**。
//
// 覆盖方向与 pkg/skills.Registry.Rank 的口径必须一致: 两处不一致会让"我改了项目里
// 那份怎么没生效"变成一条必须分别记住的规则。
func TestLoadStandardOverwriteDirection(t *testing.T) {
	state, project := t.TempDir(), t.TempDir()
	write := func(root, name, body string) {
		t.Helper()
		dir := filepath.Join(root, standardLayout()[KindAgent])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// project 来源的实际根是 <project>/.claude-go (见 standardSources) —— 写错了地方
	// 这条测试会退化成"只有一份, 没有覆盖, 也没有 problem"的假绿。
	write(filepath.Join(project, ".claude-go"), "dup.md", "---\nname: dup\ndescription: project\n---\nb")
	write(state, "dup.md", "---\nname: dup\ndescription: state\n---\nb")

	s := LoadStandard(LoadRoots{State: state, Project: project})
	a := s.Get(KindAgent, "dup")
	if a == nil || a.Description != "state" {
		t.Fatalf("state 应覆盖 project: %+v", a)
	}
	if len(s.Problems()) != 1 {
		t.Fatalf("覆盖必须留一条 problem (静默覆盖会让'改了没生效'无从查起): %+v", s.Problems())
	}
}

// TestLoadStandardMissingDirsAreNotProblems 断言一台全新机器 (什么目录都没有)
// 装出的是空索引 + 零 problem —— "没配"不是异常。
func TestLoadStandardMissingDirsAreNotProblems(t *testing.T) {
	s := LoadStandard(LoadRoots{State: filepath.Join(t.TempDir(), "nope"), Project: t.TempDir()})
	if s.Total() != 0 {
		t.Fatalf("空机器应装出 0 条: %d", s.Total())
	}
	if len(s.Problems()) != 0 {
		t.Fatalf("目录不存在不该报 problem: %+v", s.Problems())
	}
	if !strings.Contains(s.Summary(), "空") {
		t.Fatalf("摘要应如实说空: %q", s.Summary())
	}
}

// TestDescribeListsPresentRootsOnly 断言摘要只列**磁盘上真实存在**的根。
func TestDescribeListsPresentRootsOnly(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	s := LoadStandard(LoadRoots{State: state, Project: missing})
	d := s.Describe(LoadRoots{State: state, Project: missing})
	if !strings.Contains(d, filepath.Join(state, "agents")) {
		t.Fatalf("摘要应列出存在的根: %q", d)
	}
	if strings.Contains(d, "absent") {
		t.Fatalf("摘要不该列不存在的根 (会被路径淹没): %q", d)
	}
}

// TestDedupReportSorted 断言问题清单已排序: 不排序的启动日志两次运行不一样,
// 没法用来对比"这次比上次少了哪几条"。
func TestDedupReportSorted(t *testing.T) {
	s := NewSet()
	s.AddProblem("/z/last.md", os.ErrNotExist)
	s.AddProblem("/a/first.md", os.ErrPermission)
	s.AddProblem("/a/also.md", os.ErrPermission)
	got := s.DedupReport()
	if len(got) != 3 {
		t.Fatalf("应返回 3 条: %+v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("未排序: %+v", got)
		}
	}
}
