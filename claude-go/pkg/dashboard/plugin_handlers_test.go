package dashboard

import (
	"context"
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

// plugin_handlers_test.go —— 插件包资产端点的判据。
//
// 与 agent_handlers_test.go 同一套判据哲学: **不测"返回了 200"**, 主判据是往返
// (buildPluginMarkdown 写出的东西交给读方真正用的 asset.ParseContent 读回来,
// 每个声明都要还在)、盘上无残留 (被拒的请求不能留下文件)、线格式 (空列表是 []
// 不是 null —— 前端 map 会白页)。插件特有的判据是**目录形式**: 落点必须是
// <state>/plugins/<名>/PLUGIN.md 而不是扁平的 <名>.md。

// TestPluginMarkdownRoundTrips 是本文件的核心判据: 渲染器与解析器之间的键名契约。
func TestPluginMarkdownRoundTrips(t *testing.T) {
	md := buildPluginMarkdown(pluginCreateInput{
		Name:        "go-toolkit",
		Description: "Go 开发工具集: 审查、测试与 lint 的打包",
		Body:        "# Go Toolkit\n\n这个包把 Go 开发常用的 agent 与 hook 打在一起。",
		Version:     "1.2.0",
	})

	// 用**读方的解析器**读回来, source 与 path 都用生产里的那个形状 (目录形式)。
	a, err := asset.ParseContent(md, "/tmp/plugins/go-toolkit/PLUGIN.md", "state", asset.KindPlugin)
	if err != nil {
		t.Fatalf("写出来的插件资产自己解析不了: %v\n---\n%s", err, md)
	}
	if a.Name != "go-toolkit" {
		t.Errorf("name = %q, 期望 go-toolkit", a.Name)
	}
	if a.Description == "" {
		t.Error("description 读回来是空的")
	}
	if got := a.Version; got != "1.2.0" {
		t.Errorf(`version: 声明 1.2.0, 读回来 %q —— 键名漂移了`, got)
	}
	if !a.IsActive() {
		t.Errorf("status=%q, 期望 active (新建的插件必须立即可用)", a.Status)
	}
	if !strings.Contains(a.Body, "Go Toolkit") {
		t.Errorf("body 没进资产正文: %q", a.Body)
	}
	// kind 声明与目录必须一致 (不一致时 ParseContent 会报错), 显式确认没被写成别的。
	if got := a.Meta("kind"); got != "plugin" {
		t.Errorf("kind = %q, 期望 plugin", got)
	}
}

// TestPluginIsDirForm 守住 dirForm 的判据 —— 它直接决定前端把插件显示成
// "包 (有成员)"还是"单文件", 判错的表现是成员面板永远空白或永远灰着。
func TestPluginIsDirForm(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/s/plugins/go-toolkit/PLUGIN.md", true}, // 标准目录形式
		{"/s/plugins/go-toolkit/README.md", true}, // README 形态也是目录形式
		{"/c/.claude/plugins/x/PLUGIN.md", true},  // project 根的目录形式
		{"/s/plugins/go-toolkit.md", false},       // 平铺单文件
		{"/s/plugins/PLUGIN.md", false},           // 平铺在根下的 PLUGIN.md (名字就叫 PLUGIN)
		{"/s/plugins/plugins.md", false},          // 名字叫 plugins 的平铺文件
		{"/s/plugins/go-toolkit/notes.md", false}, // 目录里的非文档文件不会被 LoadDir 选中
		{"", false},
	}
	for _, c := range cases {
		if got := pluginIsDirForm(c.path); got != c.want {
			t.Errorf("pluginIsDirForm(%q) = %v, 期望 %v", c.path, got, c.want)
		}
	}
}

// TestPluginCreateRejectsBadInputWithoutWriting 覆盖五类拒绝, 每条同时断言**盘上没留文件**。
func TestPluginCreateRejectsBadInputWithoutWriting(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"路径穿越", `{"name":"../../evil","description":"d","body":"b"}`, "name"},
		{"名字带斜杠", `{"name":"a/b","description":"d","body":"b"}`, "name"},
		{"名字带大写", `{"name":"Toolkit","description":"d","body":"b"}`, "name"},
		{"描述为空", `{"name":"ok-name","description":"  ","body":"b"}`, "description"},
		{"正文为空", `{"name":"ok-name","description":"d","body":""}`, "body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, state := newAgentServer(t)
			rec := do(t, s, http.MethodPost, "/api/plugins", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400。响应: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Errorf("错误信息里没有提到 %q: %s", c.want, rec.Body.String())
			}
			entries, _ := os.ReadDir(filepath.Join(state, "plugins"))
			if len(entries) != 0 {
				t.Errorf("被拒绝的请求留下了文件: %v", entries)
			}
		})
	}
}

// TestPluginLifecycleThroughMux 走一遍 创建 → 列表 → 详情 → 删除。
//
// 与 agent 的 lifecycle 测试相同, 同时验"列表读的是进程级索引"这件事; 插件侧额外
// 验两点: 落点是**目录形式** (<state>/plugins/<名>/PLUGIN.md), 详情带 members 段。
func TestPluginLifecycleThroughMux(t *testing.T) {
	s, state := newAgentServer(t)
	prev := agent.AgentCatalog()
	defer agent.SetAgentCatalog(prev)
	agent.SetAgentCatalog(asset.NewSet()) // 起手: 索引为空 (与全新部署一致)

	body := `{"name":"go-kit","description":"Go 开发工具集","body":"# Go Kit\n\n打包 Go 常用能力。","version":"0.1.0"}`
	rec := do(t, s, http.MethodPost, "/api/plugins", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建失败 %d: %s", rec.Code, rec.Body.String())
	}
	// 创建回显的线形状: {"plugin": {...}}。
	var created struct {
		Plugin pluginDTO `json:"plugin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("创建响应里没有 plugin 对象: %v (%s)", err, rec.Body.String())
	}
	if !created.Plugin.Editable || !created.Plugin.DirForm {
		t.Errorf("新建的 state 目录形式插件必须 editable 且 dirForm: %+v", created.Plugin)
	}

	// 落点必须是目录形式, 不是扁平 <名>.md。
	p := filepath.Join(state, "plugins", "go-kit", "PLUGIN.md")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("PLUGIN.md 没落在 %s: %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(state, "plugins", "go-kit.md")); !os.IsNotExist(err) {
		t.Errorf("出现了不该有的扁平形态 go-kit.md")
	}

	// 重扫索引 (生产里由 ReloadAgents 钩子做), 列表里必须有它。
	agent.SetAgentCatalog(asset.LoadStandard(asset.LoadRoots{State: state}))
	rec = do(t, s, http.MethodGet, "/api/plugins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表失败 %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Plugins []pluginDTO `json:"plugins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表响应不是 {\"plugins\": [...]} 形状: %v (%s)", err, rec.Body.String())
	}
	var found *pluginDTO
	for i := range list.Plugins {
		if list.Plugins[i].Name == "go-kit" {
			found = &list.Plugins[i]
		}
	}
	if found == nil {
		t.Fatalf("新建的插件不在列表里: %s", rec.Body.String())
	}
	if !found.Editable {
		t.Error("state 来源的插件 editable=false —— 前端会以为它不可删")
	}
	if !found.DirForm {
		t.Error("目录形式的插件 dirForm=false —— 前端会把它显示成单文件")
	}
	if found.Version != "0.1.0" {
		t.Errorf("version = %q, 期望 0.1.0", found.Version)
	}
	if found.Problems == nil {
		t.Error("problems 序列化成了 null (前端 map 会白页), 期望 []")
	}

	// 详情: 带 body 与 members (新建包无成员, 五类都必须是 [] 不是 null)。
	rec = do(t, s, http.MethodGet, "/api/plugins/go-kit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("详情失败 %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Plugin pluginDetailDTO `json:"plugin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("详情响应形状不对: %v (%s)", err, rec.Body.String())
	}
	if !strings.Contains(detail.Plugin.Body, "Go Kit") {
		t.Errorf("详情没带 PLUGIN.md 正文: %q", detail.Plugin.Body)
	}
	m := detail.Plugin.Members
	if m.Agents == nil || m.Skills == nil || m.Hooks == nil || m.Tools == nil || m.Workflows == nil {
		t.Errorf("members 五类里有 null (前端 map 会白页): %+v", m)
	}

	// 删除: 整个目录被移除。
	rec = do(t, s, http.MethodDelete, "/api/plugins/go-kit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除失败 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(state, "plugins", "go-kit")); !os.IsNotExist(err) {
		t.Errorf("删除之后插件目录还在: %v", err)
	}
	// 再删一次必须 404 —— 不存在的名字与刚删掉的名字在响应上不能一样。
	rec = do(t, s, http.MethodDelete, "/api/plugins/go-kit", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("重复删除的状态码 = %d, 期望 404。响应: %s", rec.Code, rec.Body.String())
	}
}

// TestPluginDeleteRejectsTraversalDirectly 绕过 mux 的路径清洗直接打 handler,
// 验第二道守卫: 就算 URL 清洗失效, 非法名字也到不了 RemoveAll。
func TestPluginDeleteRejectsTraversalDirectly(t *testing.T) {
	s, state := newAgentServer(t)
	// 哨兵目录: 如果穿越守卫失效, 它会被 RemoveAll 吃掉。
	sentinel := filepath.Join(state, "sentinel-keep")
	if err := os.MkdirAll(sentinel, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../sentinel-keep", "..", "a/b", "UPPER"} {
		r := httptest.NewRequest(http.MethodDelete, "/api/plugins/"+name, nil)
		rec := httptest.NewRecorder()
		s.handlePluginDetail(rec, r) // 直打 handler, 不走 mux 的 path Clean
		if rec.Code != http.StatusBadRequest {
			t.Errorf("名字 %q 的状态码 = %d, 期望 400。响应: %s", name, rec.Code, rec.Body.String())
		}
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("哨兵目录被删了 —— 路径穿越守卫失效: %v", err)
	}
}

// TestPluginDeleteFlatForm 平铺形态 (<root>/<名>.md) 也是 LoadDir 认的合法布局,
// 列表把它标成 editable, 删除就必须真的删得掉 —— 否则是"面板说能删、点了 404"。
func TestPluginDeleteFlatForm(t *testing.T) {
	s, state := newAgentServer(t)
	root := filepath.Join(state, "plugins")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(root, "legacy-flat.md")
	if err := os.WriteFile(flat, []byte("---\nname: legacy-flat\ndescription: d\n---\n\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := do(t, s, http.MethodDelete, "/api/plugins/legacy-flat", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("平铺形态删除失败 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(flat); !os.IsNotExist(err) {
		t.Errorf("平铺文件还在: %v", err)
	}
}

// TestPluginListWorksWithoutProcessCatalog 独立 dashboard (:7777) 的最短闭环:
// 单例为 nil 时, 创建 → 列表里必须看得到 (退化成现扫, 不是空列表)。
func TestPluginListWorksWithoutProcessCatalog(t *testing.T) {
	s, state := newAgentServer(t)
	prev := agent.AgentCatalog()
	defer agent.SetAgentCatalog(prev)
	agent.SetAgentCatalog(nil)
	if agent.AgentCatalog() != nil {
		t.Fatal("前置不成立: 进程级索引不是 nil, 这条测试就失去意义了")
	}

	body := `{"name":"standalone-kit","description":"只在独立 dashboard 里建的","body":"正文。"}`
	if rec := do(t, s, http.MethodPost, "/api/plugins", body); rec.Code != http.StatusOK {
		t.Fatalf("创建失败 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(state, "plugins", "standalone-kit", "PLUGIN.md")); err != nil {
		t.Fatalf("PLUGIN.md 没落盘: %v", err)
	}

	rec := do(t, s, http.MethodGet, "/api/plugins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表失败 %d: %s", rec.Code, rec.Body.String())
	}
	// 只断言**刚建的那个名字在不在** —— DefaultRoots 带真实 user 根, 断言全量
	// 会让测试在开发机与 CI 上结果不同 (与 agent 侧同名测试同一教训)。
	var list struct {
		Plugins []pluginDTO `json:"plugins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表不是合法 JSON: %v", err)
	}
	found := false
	for _, d := range list.Plugins {
		if d.Name == "standalone-kit" {
			found = true
		}
	}
	if !found {
		t.Errorf("单例为 nil 时, 刚创建的插件不在列表里 —— 用户会以为没建成。响应: %s", rec.Body.String())
	}
}

// TestPluginListEmptyWireFormat 守住空列表的线格式: {"plugins":[]} 而不是
// {"plugins":null} (Go 的 nil slice 序列化成 null, 前端 map 当场白页)。
func TestPluginListEmptyWireFormat(t *testing.T) {
	s, _ := newAgentServer(t)
	prev := agent.AgentCatalog()
	defer agent.SetAgentCatalog(prev)
	agent.SetAgentCatalog(asset.NewSet())

	rec := do(t, s, http.MethodGet, "/api/plugins", "")
	if got := strings.TrimSpace(rec.Body.String()); got != `{"plugins":[]}` {
		t.Errorf("空列表的响应 = %q, 期望 {\"plugins\":[]}", got)
	}
}

// TestPluginGenerate 草稿生成: 返回草稿但**不落盘**; LLM 未配置时 503。
func TestPluginGenerate(t *testing.T) {
	state := t.TempDir()
	stub := func(ctx context.Context, sys, user string) (string, error) {
		// 故意裹一层散文, 验 extractJSONObj 真的被用上了。
		return "好的, 草稿如下:\n```json\n{\"name\":\"go-kit\",\"description\":\"d\",\"body\":\"b\"," +
			"\"scaffold\":{\"agents\":[{\"name\":\"reviewer\",\"description\":\"r\",\"body\":\"rb\"}]}}\n```", nil
	}
	s := NewServer(Config{StateDir: state, Addr: "127.0.0.1:0", LLMComplete: stub})

	rec := do(t, s, http.MethodPost, "/api/plugins/generate", `{"objective":"打包 Go 开发能力"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("生成失败 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Plugin pluginDraft `json:"plugin"`
		Warn   string      `json:"warn"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应形状不对: %v (%s)", err, rec.Body.String())
	}
	if resp.Plugin.Name != "go-kit" || len(resp.Plugin.Scaffold.Agents) != 1 {
		t.Errorf("草稿内容不对: %+v", resp.Plugin)
	}
	// LLM 没给的类必须归一成 [] (stub 只给了 agents)。
	if resp.Plugin.Scaffold.Skills == nil || resp.Plugin.Scaffold.Hooks == nil {
		t.Errorf("scaffold 里有 null 段: %+v", resp.Plugin.Scaffold)
	}
	// 不落盘: 草稿在被人看过之前不能进资产目录。
	entries, _ := os.ReadDir(filepath.Join(state, "plugins"))
	if len(entries) != 0 {
		t.Errorf("生成接口往盘上写了东西: %v —— 草稿必须经人工确认后才落盘", entries)
	}

	// LLM 未配置: 503, 与 agent/skill 的 generate 同形。
	s2 := NewServer(Config{StateDir: t.TempDir(), Addr: "127.0.0.1:0"})
	rec = do(t, s2, http.MethodPost, "/api/plugins/generate", `{"objective":"x"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("无 LLM 时状态码 = %d, 期望 503。响应: %s", rec.Code, rec.Body.String())
	}
	// objective 为空: 400。
	rec = do(t, s, http.MethodPost, "/api/plugins/generate", `{"objective":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空 objective 状态码 = %d, 期望 400", rec.Code)
	}
}

// TestPluginCreateWithScaffold 带骨架创建: 成员文件随清单一并落盘, 创建回显的
// 计数是展开器口径 (与运行时实际装载同一份数字), 不是"反正写进去了"的入参回显。
func TestPluginCreateWithScaffold(t *testing.T) {
	s, state := newAgentServer(t)
	prev := agent.AgentCatalog()
	defer agent.SetAgentCatalog(prev)
	agent.SetAgentCatalog(asset.NewSet())

	body := `{"name":"duty","description":"值班包","body":"# Duty","scaffold":{` +
		`"agents":[{"name":"patrol","description":"巡检","body":"你是巡检 agent。"}],` +
		`"skills":[{"name":"handbook","description":"手册","body":"按清单逐项检查。"}],` +
		`"hooks":[{"name":"guard","description":"守卫","decls":["PreToolUse:command:/bin/true:NoSuchTool*"],"body":"说明"}]}}`
	rec := do(t, s, http.MethodPost, "/api/plugins", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("带骨架创建失败 %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Plugin pluginDTO `json:"plugin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("创建响应里没有 plugin 对象: %v (%s)", err, rec.Body.String())
	}
	c := created.Plugin.Counts
	if c.Agents != 1 || c.Skills != 1 || c.Hooks != 1 {
		t.Errorf("回显计数 = %+v, 期望 agents/skills/hooks 各 1 (展开器口径)", c)
	}

	// 盘上布局与运行时的读取口径一一对应:
	// agents 扁平 .md, skills 目录形式 SKILL.md (skills.Registry 只认这个), hooks 扁平 .md。
	for _, p := range []string{
		"plugins/duty/agents/patrol.md",
		"plugins/duty/skills/handbook/SKILL.md",
		"plugins/duty/hooks/guard.md",
	} {
		if _, err := os.Stat(filepath.Join(state, p)); err != nil {
			t.Errorf("成员文件没落在 %s: %v", p, err)
		}
	}
	// hook 声明必须进 frontmatter 的 hooks: 列表 —— 写在 body 里运行时看不到。
	hookRaw, _ := os.ReadFile(filepath.Join(state, "plugins", "duty", "hooks", "guard.md"))
	if !strings.Contains(string(hookRaw), "hooks:\n") || !strings.Contains(string(hookRaw), "PreToolUse:command:/bin/true:NoSuchTool*") {
		t.Errorf("hook 文件里没有 frontmatter hooks: 声明:\n%s", hookRaw)
	}

	// 重扫索引后, 详情 members 与回显计数一致 (两份口径不许漂移)。
	agent.SetAgentCatalog(asset.LoadStandard(asset.LoadRoots{State: state}))
	rec = do(t, s, http.MethodGet, "/api/plugins/duty", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("详情失败 %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Plugin pluginDetailDTO `json:"plugin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("详情形状不对: %v", err)
	}
	if len(detail.Plugin.Members.Agents) != 1 || len(detail.Plugin.Members.Skills) != 1 || len(detail.Plugin.Members.Hooks) != 1 {
		t.Errorf("详情 members = %+v, 期望三类各 1", detail.Plugin.Members)
	}
}

// TestPluginCreateScaffoldRejectedAtomically 骨架里任何一个成员非法, 整个创建 400
// 且盘上一片不留 —— 落一半等于替用户改了他签过字的内容。
func TestPluginCreateScaffoldRejectedAtomically(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"成员名非法", `{"name":"bad1","description":"d","body":"b","scaffold":{"agents":[{"name":"Bad Name","body":"x"}]}}`, "agents"},
		{"成员重名", `{"name":"bad2","description":"d","body":"b","scaffold":{"agents":[{"name":"a","body":"x"},{"name":"a","body":"y"}]}}`, "重名"},
		{"hook 声明拼错", `{"name":"bad3","description":"d","body":"b","scaffold":{"hooks":[{"name":"g","decls":["garbage"]}]}}`, "hooks"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, state := newAgentServer(t)
			rec := do(t, s, http.MethodPost, "/api/plugins", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400。响应: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Errorf("错误信息里没有提到 %q: %s", c.want, rec.Body.String())
			}
			entries, _ := os.ReadDir(filepath.Join(state, "plugins"))
			if len(entries) != 0 {
				t.Errorf("被拒绝的请求留下了文件: %v", entries)
			}
		})
	}
}
