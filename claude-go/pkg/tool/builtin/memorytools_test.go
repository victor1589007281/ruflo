package builtin

// memorytools_test.go —— 按需取回两件套 (memory_search / memory_read)。
//
// 这套测试守的是**拉取路径的每一处静默失败**。推送改拉取之后, 记忆只有在模型
// 主动调工具时才进上下文; 工具坏了不会有任何报错 —— 模型只是"这次没想起来",
// 与"本来就没有相关记忆"在账本上长得一模一样。所以要在这里把每个出口断言死。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/tool"
)

func newToolStore(t *testing.T) *memory.TieredStore {
	t.Helper()
	s := memory.NewTieredStoreWithPersist(t.TempDir())
	t.Cleanup(s.WaitPersist)
	return s
}

func callMemTool(t *testing.T, tl tool.Tool, in string) string {
	t.Helper()
	res, err := tl.Call(context.Background(), json.RawMessage(in), &tool.ToolContext{})
	if err != nil {
		t.Fatalf("%s 返回 error: %v", tl.Name(), err)
	}
	if res == nil {
		t.Fatalf("%s 返回 nil result", tl.Name())
	}
	return res.Content
}

// ---------------------------------------------------------------------------
// 开关
// ---------------------------------------------------------------------------

// 未设置环境变量时走默认值 (当前为**关**)。这条把"默认值是多少"钉在盘上 ——
// 改默认值是一个有部署后果的动作, 不该顺手就翻了。
func Test索引开关_未设置走默认(t *testing.T) {
	if MemoryIndexModeEnabled(func(string) string { return "" }) {
		t.Fatal("未设置时不该默认开 (改默认可直接翻这里, 但要是有意的)")
	}
	if MemoryIndexModeEnabled(nil) {
		t.Fatal("getenv 为 nil 时不该崩, 也不该默认开")
	}
}

// "0/false/off/no" 都算关, 大小写与空白不敏感, 其余一律算开 (与 pool 的开关同构)。
func Test索引开关_取值(t *testing.T) {
	cases := map[string]bool{
		"1": true, "true": true, "TRUE": true, "on": true, "yes": true,
		"  true  ": true, "任意其它值": true,
		"0": false, "false": false, "FALSE": false, "off": false, "no": false,
		"  Off  ": false,
	}
	for v, want := range cases {
		got := MemoryIndexModeEnabled(func(string) string { return v })
		if got != want {
			t.Errorf("开关取 %q: 得到 %v, 期望 %v", v, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// memory_search
// ---------------------------------------------------------------------------

// 检索只回索引, 不回正文 —— 这是两个工具分工的全部意义。回了正文等于把
// 推送模式那次 19K 的账重新付了一遍, 而且还是模型主动点的。
func Test检索_只回索引不回正文(t *testing.T) {
	s := newToolStore(t)
	secret := strings.Repeat("这段正文只该在 memory_read 里出现。", 30)
	s.Add(&memory.MemoryEntry{Content: "JD 分析 " + secret, Source: "team_result", Importance: 0.5})

	out := callMemTool(t, NewMemorySearchTool(s), `{"query":"JD 分析"}`)

	if !strings.Contains(out, "<memory_index>") {
		t.Fatalf("检索应当回索引形态:\n%s", out)
	}
	// 先确认这条**确实被命中并印进了索引** —— 否则下面"没回正文"的断言是空的
	// (一条都没命中时它同样成立, 却什么也没证明)。
	if !strings.Contains(out, "- id=") {
		t.Fatalf("这条记忆没进索引, 下面的断言不作数:\n%s", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("检索把正文也回出来了 (两件套的分工失效):\n%s", out)
	}
}

// 空库与"没搜到"必须分开说: 前者说明这个部署压根没积累记忆, 模型不该据此改写查询。
func Test检索_空库与没搜到分开(t *testing.T) {
	empty := newToolStore(t)
	gotEmpty := callMemTool(t, NewMemorySearchTool(empty), `{"query":"随便"}`)
	if !strings.Contains(gotEmpty, "为空") {
		t.Fatalf("空库应当如实说记忆库为空:\n%s", gotEmpty)
	}

	s := newToolStore(t)
	s.Add(&memory.MemoryEntry{Content: "关于编译器的记忆", Source: "manual", Importance: 0.5})
	gotMiss := callMemTool(t, NewMemorySearchTool(s), `{"query":"量子纠缠光谱学"}`)
	if strings.Contains(gotMiss, "为空") {
		t.Fatalf("库里有条目就不该说为空 (会把模型引向错误结论):\n%s", gotMiss)
	}
	if !strings.Contains(gotMiss, "没有匹配") {
		t.Fatalf("没搜到应当说没匹配:\n%s", gotMiss)
	}
}

// 索引里印出来的 id, 必须能被 memory_read 原样取回 —— 两个工具之间唯一的契约。
// 这条链断在哪里都不报错 (检索看着正常, 读的时候才发现查不到)。
func Test检索与读取_的id契约(t *testing.T) {
	s := newToolStore(t)
	s.Add(&memory.MemoryEntry{Content: "团队完成了 JD 分析并产出面试题", Source: "team_result", Importance: 0.9})

	idx := callMemTool(t, NewMemorySearchTool(s), `{"query":"JD 分析 面试题"}`)
	line := ""
	for _, l := range strings.Split(idx, "\n") {
		if strings.HasPrefix(l, "- id=") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("检索没回任何条目:\n%s", idx)
	}
	// "- id=XXX (date)...: preview" -> XXX
	id := strings.Fields(strings.TrimPrefix(line, "- id="))[0]

	body := callMemTool(t, NewMemoryReadTool(s), `{"id":"`+id+`"}`)
	if !strings.Contains(body, "团队完成了 JD 分析并产出面试题") {
		t.Fatalf("按索引里的 id=%q 取不回正文:\n%s", id, body)
	}
}

// limit 缺省与非法值不该让工具报错, 也不该退化成"只回一条"。
func Test检索_limit缺省与非法值(t *testing.T) {
	s := newToolStore(t)
	for i := 0; i < 12; i++ {
		s.Add(&memory.MemoryEntry{
			Content: "关于分布式事务的记忆条目", Source: "manual", Importance: 0.5,
		})
	}

	for _, in := range []string{`{"query":"分布式事务"}`, `{"query":"分布式事务","limit":0}`, `{"query":"分布式事务","limit":-3}`} {
		out := callMemTool(t, NewMemorySearchTool(s), in)
		if n := strings.Count(out, "- id="); n < 2 {
			t.Fatalf("输入 %s 只回了 %d 条, 缺省 limit 生效了吗:\n%s", in, n, out)
		}
	}

	out := callMemTool(t, NewMemorySearchTool(s), `{"query":"分布式事务","limit":3}`)
	if n := strings.Count(out, "- id="); n != 3 {
		t.Fatalf("limit=3 应当恰好回 3 条, 实际 %d 条", n)
	}
}

// 输入是坏 JSON 时如实报错, 而不是 panic 或静默回空。
func Test检索_坏输入不panic(t *testing.T) {
	s := newToolStore(t)
	out := callMemTool(t, NewMemorySearchTool(s), `{"query":`)
	if !strings.Contains(out, "解析失败") {
		t.Fatalf("坏 JSON 应当如实报错:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// memory_read
// ---------------------------------------------------------------------------

// 有未截断快照时读全文 —— 这是 Ref 存在的理由。读不到快照就静默回落摘要:
// 快照是加分项, 它丢了不该让这条记忆变得读不出来。
func Test读取_优先取未截断原文(t *testing.T) {
	s := newToolStore(t)
	summary := "短摘要"
	original := summary + strings.Repeat("未截断的完整产出。", 100)

	e := &memory.MemoryEntry{Content: summary, Source: "team_result", Importance: 0.9}
	s.AddWithOriginal(e, original)

	out := callMemTool(t, NewMemoryReadTool(s), `{"id":"`+e.ID+`"}`)
	if !strings.Contains(out, "完整原文") {
		t.Fatalf("有快照时应当标注完整原文:\n%s", out)
	}
	if !strings.Contains(out, strings.Repeat("未截断的完整产出。", 100)) {
		t.Fatalf("没取到未截断原文 (读到的是摘要):\n%s", out)
	}
}

// 快照文件被外部删掉 (运维清理、目录搬迁) 时, 这条记忆仍然要读得出来。
func Test读取_快照丢失时回落摘要(t *testing.T) {
	dir := t.TempDir()
	s := memory.NewTieredStoreWithPersist(dir)
	t.Cleanup(s.WaitPersist)

	e := &memory.MemoryEntry{Content: "摘要内容", Source: "team_result", Importance: 0.9}
	s.AddWithOriginal(e, "摘要内容"+strings.Repeat("很长的原文。", 100))
	if e.Ref == "" {
		t.Fatal("前置条件不成立: 没有产生 Ref")
	}

	// 整个快照目录删掉。
	if err := os.RemoveAll(filepath.Join(dir, "blobs")); err != nil {
		t.Fatalf("删快照目录失败: %v", err)
	}

	out := callMemTool(t, NewMemoryReadTool(s), `{"id":"`+e.ID+`"}`)
	if !strings.Contains(out, "摘要内容") {
		t.Fatalf("快照丢了仍应回落摘要读出这条记忆:\n%s", out)
	}
}

// id 查不到是记忆被衰减淘汰的**正常结局**, 不是故障。报错必须给出下一步,
// 否则模型会以为是自己记错了 id 而反复重试同一个。
func Test读取_id查不到时给出下一步(t *testing.T) {
	s := newToolStore(t)
	out := callMemTool(t, NewMemoryReadTool(s), `{"id":"不存在的-id"}`)

	if !strings.Contains(out, "衰减") && !strings.Contains(out, "淘汰") {
		t.Fatalf("查不到应当说明可能是被淘汰:\n%s", out)
	}
	if !strings.Contains(out, "memory_search") {
		t.Fatalf("查不到应当给出下一步 (改用 memory_search):\n%s", out)
	}
}

// 空 id 与坏 JSON 分别报错, 都不 panic。
func Test读取_空id与坏输入(t *testing.T) {
	s := newToolStore(t)
	if out := callMemTool(t, NewMemoryReadTool(s), `{"id":"   "}`); !strings.Contains(out, "需要 id") {
		t.Fatalf("空 id 应当明确报错:\n%s", out)
	}
	if out := callMemTool(t, NewMemoryReadTool(s), `{"id":`); !strings.Contains(out, "解析失败") {
		t.Fatalf("坏 JSON 应当如实报错:\n%s", out)
	}
}

// 单次读取有上限 —— 否则一条 1MB 的快照能把上下文一次打满。
func Test读取_正文被截断到上限(t *testing.T) {
	s := newToolStore(t)
	e := &memory.MemoryEntry{Content: "x", Source: "team_result", Importance: 0.5}
	s.AddWithOriginal(e, strings.Repeat("超长原文段落。", 5000)) // ≈35000 字符

	out := callMemTool(t, NewMemoryReadTool(s), `{"id":"`+e.ID+`"}`)
	body := out[strings.Index(out, "\n\n")+2:]
	if n := len([]rune(body)); n > memoryReadMaxRunes+64 {
		t.Fatalf("单次读取 %d 字符, 超出上限 %d", n, memoryReadMaxRunes)
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("截断应当有标记:\n%s", out[:200])
	}
}

// ---------------------------------------------------------------------------
// 未接入记忆的会话
// ---------------------------------------------------------------------------

// store 为 nil (未配置长期记忆的部署) 时工具仍注册, 调用时如实报错 ——
// 不 panic, 也不假装"没有相关记忆" (那会让模型白试一轮)。
func Test未接入记忆_如实报错(t *testing.T) {
	for _, tl := range []tool.Tool{NewMemorySearchTool(nil), NewMemoryReadTool(nil)} {
		var out string
		if tl.Name() == "memory_search" {
			out = callMemTool(t, tl, `{"query":"x"}`)
		} else {
			out = callMemTool(t, tl, `{"id":"x"}`)
		}
		if !strings.Contains(out, "不可用") {
			t.Fatalf("%s: 未接入记忆时应当如实报不可用:\n%s", tl.Name(), out)
		}
	}
}

// 注册后两个工具都能被注册表找到, 且都是只读 + 并发安全 (它们只读盘, 不写盘)。
func Test注册_两件套都在且只读(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterMemoryTools(reg, newToolStore(t))

	for _, name := range []string{"memory_search", "memory_read"} {
		tl, ok := reg.Get(name)
		if !ok || tl == nil {
			t.Fatalf("注册表里找不到 %s", name)
		}
		if !tl.IsReadOnly(nil) {
			t.Errorf("%s 应当只读", name)
		}
		if !tl.IsConcurrencySafe(nil) {
			t.Errorf("%s 应当并发安全", name)
		}
		if tl.Description() == "" {
			t.Errorf("%s 缺少描述 (模型看不到就不知道何时该调)", name)
		}
	}
}
