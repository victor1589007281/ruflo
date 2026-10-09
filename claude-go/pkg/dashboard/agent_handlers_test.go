package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/asset"
)

// agent_handlers_test.go —— agent 资产写接口的判据。
//
// 这里**不测"接口返回了 200"**。返回 200 只说明进程没崩, 而这一类接口真正会出的错
// 是"文件写下去了、键名不是读方认识的那个" —— 那种产物从外面看完全正常: 文件在,
// 能解析, 描述也对, 只有那个声明的字段静默不生效。
//
// 所以主判据是**往返**: buildAgentMarkdown 写出来的东西, 交给 pkg/asset 的解析器
// (读方真正用的那个函数) 读回来, 每一个声明都要还在。发现键名漂移的只有这一条。

func newAgentServer(t *testing.T) (*Server, string) {
	t.Helper()
	state := t.TempDir()
	return NewServer(Config{StateDir: state, Addr: "127.0.0.1:0"}), state
}

// do 打一次请求, 走真实 mux (路由本身也是要验的东西 —— 前缀匹配写错会 404)。
func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// TestAgentMarkdownRoundTripsEveryDeclaration 是本文件的核心判据。
//
// ★ 为什么它必须是**往返**而不是逐字段断言渲染结果: 渲染结果对不对是写方自己的
// 口径, 而可调项能不能生效取决于**读方**认不认那几个键 (pkg/agentbind 读的是一组
// 扁平 kebab-case 键)。断言渲染字符串只能证明我写了自己想写的, 证明不了它会被读到。
func TestAgentMarkdownRoundTripsEveryDeclaration(t *testing.T) {
	turns := 7
	md := buildAgentMarkdown(agentCreateInput{
		Name:        "code-reviewer",
		Description: "审查 Go 代码并给出可执行的修改建议",
		Body:        "你是 Go 代码审查员。\n只报能被复现的问题。",
		Model:       "kimi:k3",
		MaxTurns:    &turns,
		ToolProfile: "analysis",
		PermMode:    "plan",
		Skills:      []string{"go-review", "mysql-dba"},
		Disallowed:  []string{"Bash", "Write"},
	})

	// 用**读方的解析器**读回来, source 与 path 都用生产里的那个形状。
	a, err := asset.ParseContent(md, "/tmp/agents/code-reviewer.md", "state", asset.KindAgent)
	if err != nil {
		t.Fatalf("写出来的资产自己解析不了: %v\n---\n%s", err, md)
	}

	if a.Name != "code-reviewer" {
		t.Errorf("name = %q, 期望 code-reviewer", a.Name)
	}
	if a.Description == "" {
		t.Error("description 读回来是空的")
	}
	if !strings.Contains(a.Body, "Go 代码审查员") {
		t.Errorf("body 没进资产正文: %q", a.Body)
	}
	if !a.IsActive() {
		t.Errorf("status=%q, 期望 active (新建的 agent 必须立即可用)", a.Status)
	}

	// 逐项对账: 每一个可调项都要在读侧**真的取到**。
	if got := a.Meta("model"); got != "kimi:k3" {
		t.Errorf(`model: 声明 kimi:k3, 读回来 %q —— 键名漂移了`, got)
	}
	if got := a.Meta("max-turns"); got != "7" {
		t.Errorf(`max-turns: 声明 7, 读回来 %q —— 键名漂移了`, got)
	}
	if got := a.Meta("tool-profile"); got != "analysis" {
		t.Errorf(`tool-profile: 声明 analysis, 读回来 %q —— 键名漂移了`, got)
	}
	if got := a.Meta("permission-mode"); got != "plan" {
		t.Errorf(`permission-mode: 声明 plan, 读回来 %q —— 键名漂移了`, got)
	}
	if got := a.MetaList("skills"); len(got) != 2 || got[0] != "go-review" {
		t.Errorf("skills: 读回来 %v, 期望 [go-review mysql-dba]", got)
	}
	if got := a.MetaList("disallowed-tools"); len(got) != 2 || got[1] != "Write" {
		t.Errorf("disallowed-tools: 读回来 %v, 期望 [Bash Write]", got)
	}
	// kind 声明与目录必须一致 (ParseContent 会因此报错), 这里是显式确认它没被写成别的。
	if got := a.Meta("kind"); got != "agent" {
		t.Errorf("kind = %q, 期望 agent", got)
	}
}

// TestAgentToolsEmptyIsFailClosed 守住"没写 tools"与"写了空 tools"的区别。
//
// 资产语义里 `tools: []` 是 fail-closed (拒绝一切工具), 而"没写"是跟随基线 (什么都有)。
// 两者在解析器那边靠 MetaListSet 的第二个返回值区分。渲染器如果把空列表写成
// 什么都不写, 用户说的"我要一个没有工具的 agent"会静默变成"给它全部工具" ——
// 一份**更宽**的权限, 而且方向正好相反。
func TestAgentToolsEmptyIsFailClosed(t *testing.T) {
	empty := []string{}
	md := buildAgentMarkdown(agentCreateInput{
		Name: "no-tools", Description: "不需要任何工具的 agent", Body: "只输出一段文本。",
		Tools: &empty,
	})
	a, err := asset.ParseContent(md, "/tmp/agents/no-tools.md", "state", asset.KindAgent)
	if err != nil {
		t.Fatal(err)
	}
	tools, ok := a.MetaListSet("tools")
	if !ok {
		t.Fatalf("`tools: []` 没被读成「声明了」 —— 这会把它变成「跟随基线(全部工具)」。\n---\n%s", md)
	}
	if len(tools) != 0 {
		t.Errorf("tools = %v, 期望空列表", tools)
	}

	// 负对照: 没给 Tools 时必须是"没声明"。没有这一条, 上面那个 ok 恒真也看不出来。
	md2 := buildAgentMarkdown(agentCreateInput{Name: "normal", Description: "d", Body: "b"})
	a2, err := asset.ParseContent(md2, "/tmp/agents/normal.md", "state", asset.KindAgent)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a2.MetaListSet("tools"); ok {
		t.Error("没给 Tools 时读成了「声明了」 —— 区分丢失, fail-closed 的判据就不成立了")
	}
}

// TestAgentCreateRejectsBadInputWithoutWriting 覆盖四类拒绝。
//
// ★ 每一条都同时断言**盘上没有文件**。只断言状态码的话, 一个"先写盘再校验"的
//   实现会全绿 —— 而它的后果是用户拿到一份坏文件, 且旧定义已经被覆盖掉了。
func TestAgentCreateRejectsBadInputWithoutWriting(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // 期望错误里出现的字样
	}{
		{"路径穿越", `{"name":"../../evil","description":"d","body":"b"}`, "name"},
		{"名字带斜杠", `{"name":"a/b","description":"d","body":"b"}`, "name"},
		{"名字带大写", `{"name":"Coder","description":"d","body":"b"}`, "name"},
		{"描述为空", `{"name":"ok-name","description":"  ","body":"b"}`, "description"},
		{"正文为空", `{"name":"ok-name","description":"d","body":""}`, "body"},
		{"档位拼错", `{"name":"ok-name","description":"d","body":"b","toolProfile":"codign"}`, "toolProfile"},
		{"权限模式非法", `{"name":"ok-name","description":"d","body":"b","permissionMode":"yolo"}`, "permissionMode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, state := newAgentServer(t)
			rec := do(t, s, http.MethodPost, "/api/agents", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400。响应: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Errorf("错误信息里没有提到 %q: %s", c.want, rec.Body.String())
			}
			// 盘上必须干净 —— 尤其是"档位拼错"那条: 它是在**校验阶段**被拒的,
			// 而 NormalizeToolProfile 的既有行为是静默掰成 chat。掰了再写下去,
			// 用户会得到一个工具面完全不对、却看起来正常的 agent。
			entries, _ := os.ReadDir(filepath.Join(state, "agents"))
			if len(entries) != 0 {
				t.Errorf("被拒绝的请求留下了文件: %v", entries)
			}
		})
	}
}

// TestAgentLifecycleThroughMux 走一遍 创建 → 列表 → 删除。
//
// 这里同时验一件不做就静默坏掉的事: **列表读的是进程级索引, 不是重新扫盘**。
// 如果列表自己去 LoadStandard 一遍, 那么"写下去了但索引没刷新"这个状态在列表上是
// 看不见的 (自己扫当然能看到新文件), 而运行时的 --agent 查表仍然查不到它 ——
// 面板说它在、运行说找不到。
func TestAgentLifecycleThroughMux(t *testing.T) {
	s, state := newAgentServer(t)
	prev := agent.AgentCatalog()
	defer agent.SetAgentCatalog(prev)

	// 起手: 索引是空的 (与全新部署一致)。
	agent.SetAgentCatalog(asset.NewSet())

	body := `{"name":"my-helper","description":"帮忙整理日志","body":"你负责整理日志。","toolProfile":"analysis","maxTurns":3}`
	rec := do(t, s, http.MethodPost, "/api/agents", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建失败 %d: %s", rec.Code, rec.Body.String())
	}

	// 文件真的落在 state 根下 —— 落点的判据是"asset 的 state 来源会不会扫到它"。
	p := filepath.Join(state, "agents", "my-helper.md")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("文件没落在 %s: %v", p, err)
	}

	// 重扫一次索引 (生产里由 ReloadAgents 钩子做), 然后列表里**必须**有它。
	// 这条断言的是"新建的 agent 会在清单里出现" —— 没有它, 用户建完在下拉里选不到,
	// 会以为没建成; 而它其实能跑。
	agent.SetAgentCatalog(asset.LoadStandard(asset.LoadRoots{State: state}))

	rec = do(t, s, http.MethodGet, "/api/agents", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表失败 %d: %s", rec.Code, rec.Body.String())
	}
	var list []agentAssetDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表响应不是数组: %v (%s)", err, rec.Body.String())
	}
	var found *agentAssetDTO
	for i := range list {
		if list[i].Name == "my-helper" {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("新建的 agent 不在列表里: %s", rec.Body.String())
	}
	// sourcePath 非空 = 这一份是**本机可编辑的**。前端靠它决定删不删按钮。
	if found.SourcePath == "" {
		t.Error("state 来源的资产没有回报 sourcePath —— 前端会以为它不可删")
	}

	// 删除。
	rec = do(t, s, http.MethodDelete, "/api/agents/my-helper", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除失败 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("删除之后文件还在: %v", err)
	}
	// 再删一次必须 404, 不能报"删成功" —— 否则一个不存在的名字与一个刚删掉的名字
	// 在响应上完全一样。
	rec = do(t, s, http.MethodDelete, "/api/agents/my-helper", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("重复删除的状态码 = %d, 期望 404。响应: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentListWorksWithoutProcessCatalog 独立 dashboard 的最短闭环:
// 创建 → 列表里必须看得到 → 详情里必须看得到。
//
// ★ 这一条是**实测撞出来的**, 不是想出来的。上面那个 lifecycle 测试自己
//   SetAgentCatalog 了一份索引, 于是它验的是"有索引时列表对不对", 而生产里
//   独立 dashboard (dashboard run, :7777) **从来没有索引** —— 全仓只有两处
//   SetAgentCatalog (CLI 的 --agent 路径 与 pkg/feishu), 都不在那个进程里。
//
//   实测表现: POST /api/agents 返回 200 且文件正确落在 <stateDir>/agents/ 下,
//   紧接着 GET /api/agents 返回 0 条。用户看到的是"建完就没了"。
//
//   判据不能写成"列表非空" —— DefaultRoots 会带上真实的 user 根, 开发机上
//   恰好存在的 ~/.claude-go/agents/*.md 会让这条断言在 CI 与本地结果不同。
//   只断言**刚建的那个名字在不在**, 与宿主上有什么无关。
func TestAgentListWorksWithoutProcessCatalog(t *testing.T) {
	s, state := newAgentServer(t)
	prev := agent.AgentCatalog()
	defer agent.SetAgentCatalog(prev)
	// 关键前提前移到断言之前: 这条测试的全部意义就是"单例为 nil 时也要能工作",
	// 若哪天前面某个测试漏了还原, 这里会安静地退化成又一条重复的 lifecycle 测试。
	agent.SetAgentCatalog(nil)
	if agent.AgentCatalog() != nil {
		t.Fatal("前置不成立: 进程级索引不是 nil, 这条测试就失去意义了")
	}

	body := `{"name":"standalone-only","description":"只在独立 dashboard 里建的","body":"你负责整理日志。"}`
	if rec := do(t, s, http.MethodPost, "/api/agents", body); rec.Code != http.StatusOK {
		t.Fatalf("创建失败 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(state, "agents", "standalone-only.md")); err != nil {
		t.Fatalf("文件没落盘: %v", err)
	}

	rec := do(t, s, http.MethodGet, "/api/agents", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表失败 %d: %s", rec.Code, rec.Body.String())
	}
	var list []agentAssetDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表响应不是数组: %v", err)
	}
	found := false
	for _, d := range list {
		if d.Name == "standalone-only" {
			found = true
		}
	}
	if !found {
		t.Errorf("单例为 nil 时, 刚创建的 agent 不在列表里 —— 用户会以为没建成。响应: %s", rec.Body.String())
	}

	// 详情走的是同一份索引, 必须与列表同口径 (分叉的表现是"列表里有、点进去 503")。
	rec = do(t, s, http.MethodGet, "/api/agents/standalone-only", "")
	if rec.Code != http.StatusOK {
		t.Errorf("详情失败 %d: %s (列表里有它, 详情却拿不到 = 两个入口口径不一致)", rec.Code, rec.Body.String())
	}
}

// TestAgentListIsEmptyNotNil 守住空列表的线格式。
//
// Go 的 nil slice 序列化成 `null`, 前端 `list.map(...)` 当场白页 —— 这是本仓
// 反复出现过的形态 (见 memory: nil-slice-marshals-to-null-blanks-the-page)。
func TestAgentListIsEmptyNotNil(t *testing.T) {
	s, _ := newAgentServer(t)
	prev := agent.AgentCatalog()
	defer agent.SetAgentCatalog(prev)
	agent.SetAgentCatalog(asset.NewSet())

	rec := do(t, s, http.MethodGet, "/api/agents", "")
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("空列表的响应 = %q, 期望 []", got)
	}
	// 索引为 nil (独立 dashboard :7777 从未装载) 时**退化成现扫**, 见 agentAssetList 的
	// 注释 —— 所以这里不能再断言 "== []"。
	//
	// ★ 那条旧断言的前提被改掉了, 必须一并改: DefaultRoots 会带上**真实的 user 根**
	//   (~/.claude-go/agents), 开发机上恰好放了一份 agent 资产就会让 "== []" 变红,
	//   而 CI 上没有它。留着它等于给这条测试埋一个"本机绿、别处红"的差异。
	//   空列表的线格式由上面 SetAgentCatalog(asset.NewSet()) 那一支完整守着,
	//   这一支要守的只剩"nil 时不能崩、不能吐 null"。
	agent.SetAgentCatalog(nil)
	rec = do(t, s, http.MethodGet, "/api/agents", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("无索引时列表的状态码 = %d, 期望 200 (退化成现扫, 不是 503)", rec.Code)
	}
	var arr []agentAssetDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Errorf("无索引时列表不是合法 JSON 数组 (前端 map 会当场白页): %v, 响应 %q", err, rec.Body.String())
	}
}
