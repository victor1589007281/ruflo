package builtin

import (
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

// TestToolNameCatalog与真实注册一致 钉住本目录的**唯一**用途: 回答"这个名字是真工具吗"。
//
// 目录存在的理由是一份写错名的黑名单在运行期完全隐形 (`disallowed-tools: [bash]`
// 与 `[Bash]` 在装配读数上一模一样, 区别只在"一个都挡不住")。它要能担起这个判断,
// 前提是它列出来的名字与真实注册结果**逐字一致** —— 一旦分家, 它就会开始误报:
// 把真的工具说成拼错, 或者反过来漏掉一个真工具的名字。
func TestToolNameCatalog与真实注册一致(t *testing.T) {
	for _, p := range []ToolProfile{
		ToolProfileChat, ToolProfileResearch, ToolProfileCoding,
		ToolProfileTeam, ToolProfileAnalysis, ToolProfileAdmin,
	} {
		catalog := ToolNameCatalog(p)

		// 同档位下真实注册一遍, 两边必须完全相等。
		reg := tool.NewRegistry()
		RegisterProfileToolsWithStore(reg, nil, nil, p)
		want := reg.Names()

		got := map[string]bool{}
		for _, n := range catalog {
			got[n] = true
		}
		if len(catalog) != len(want) {
			t.Errorf("档位 %s: 目录 %d 项, 真实注册 %d 项 (%v vs %v)", p, len(catalog), len(want), catalog, want)
			continue
		}
		for _, n := range want {
			if !got[n] {
				t.Errorf("档位 %s: 真实注册里有 %q, 目录里没有 —— 拿它判拼写会把真工具报成错的", p, n)
			}
		}
	}
}

// TestToolNameCatalog能分辨大小写 是上面那条的**分辨力**对照。
//
// 缺了它, "目录与注册一致"在目录被写成"无论如何都返回空"时**依然全绿** ——
// 而空目录会让每一份黑名单都被报成"名字不存在"。
func TestToolNameCatalog能分辨大小写(t *testing.T) {
	admin := ToolNameCatalog(ToolProfileAdmin)
	has := func(name string) bool {
		for _, n := range admin {
			if n == name {
				return true
			}
		}
		return false
	}
	// admin 档注册全部内置工具, 其中确定有 Bash / Read。
	if !has("Bash") || !has("Read") {
		t.Fatalf("admin 档目录里缺 Bash/Read —— 判据无从落地: %v", admin)
	}
	// 大小写是**逐字节**的: 这正是要拦住的那类拼写错误。
	for _, wrong := range []string{"bash", "BASH", "read", "Bash "} {
		if has(wrong) {
			t.Errorf("目录把 %q 当成了合法工具名 —— 那正是它要拦的拼写错误", wrong)
		}
	}
}

// TestToolNameCatalog按档位收窄 说明"为什么判拼写必须用静态目录, 而不是实际注册表"。
//
// analysis 档下 Bash **从未被注册**。若拿"实际注册表里有没有"当判据,
// `disallowed-tools: [Bash]` + `tool-profile: analysis` 会被报成"名字不存在 (拼错了?)"
// —— 而它其实是一个正确且冗余的声明 (作者在防御性地关掉一个本就不该出现的工具)。
// 静态目录说"Bash 是个真工具", 于是它被正确接受。
func TestToolNameCatalog按档位收窄(t *testing.T) {
	admin, analysis := ToolNameCatalog(ToolProfileAdmin), ToolNameCatalog(ToolProfileAnalysis)
	has := func(list []string, name string) bool {
		for _, n := range list {
			if n == name {
				return true
			}
		}
		return false
	}

	if !has(admin, "Bash") {
		t.Fatal("admin 档应有 Bash")
	}
	if has(analysis, "Bash") {
		t.Fatal("analysis 档不该有 Bash —— 若有了, 本用例的对照前提就变了")
	}
	// 但两条目录都认 Bash 这个名字 —— 判"是不是真工具"与判"这个档位有没有它"
	// 是两个问题, 前者正是本目录要回答的那个。
	if !has(admin, "Read") || !has(analysis, "Read") {
		t.Error("Read 在两个档位下都该有")
	}
}

// TestAdmin档是各档位的并集 钉住一条**被调用方依赖**的性质, 而不是本函数的实现细节。
//
// 组装器 (pkg/agentbind) 判"这个名字是不是真工具"时间的是 admin 档 —— 因为那是一个
// 与"资产声明了哪个档位"无关的是非判断。这条性质若破了 (admin 漏掉某个档位专属的
// 工具名), 那个名字就会被报成"拼写错误", 而看的人会去改一行本来就对的配置。
//
// 这是"收窄的目录被拿去答合法性问题"能成立的**唯一**前提, 所以它必须自己也有个判据
// —— 否则调用方那句注释就是一句没有证据的断言。
func TestAdmin档是各档位的并集(t *testing.T) {
	admin := map[string]bool{}
	for _, n := range ToolNameCatalog(ToolProfileAdmin) {
		admin[n] = true
	}
	if len(admin) == 0 {
		t.Fatal("admin 目录为空 —— 下面每条都会假绿")
	}
	for _, p := range []ToolProfile{
		ToolProfileChat, ToolProfileResearch, ToolProfileCoding, ToolProfileTeam, ToolProfileAnalysis,
	} {
		for _, n := range ToolNameCatalog(p) {
			if !admin[n] {
				t.Errorf("档位 %s 注册了 %q, 而 admin 档没有 —— 拿 admin 判合法性的调用方"+
					"会把它报成拼写错误 (合法性问题问的就是并集)", p, n)
			}
		}
	}
}

// TestToolNameCatalog不报版本号之类的假名 是阴性对照:
// 目录里不该混进任何空串或带空白的条目 (frontmatter 解析出的名字会带前后空白)。
func TestToolNameCatalog不报假名(t *testing.T) {
	for _, n := range ToolNameCatalog(ToolProfileAdmin) {
		if strings.TrimSpace(n) != n || n == "" {
			t.Errorf("目录里有非法条目 %q", n)
		}
	}
}
