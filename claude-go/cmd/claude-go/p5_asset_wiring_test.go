package main

// p5_asset_wiring_test —— 21.8.12 P5 在 **CLI 这条路径**上的通电验收。
//
// pkg/feishu 有一份同名同形的测试, 两份都是必要的: 飞书通过 ConstraintSet 决定档位
// (有来源可审计), CLI 直接决定注册哪个档位, 装配代码是两套。一份通过不代表另一份
// 通过 —— 本仓大量缺陷就是"能力在 A 路径通了、B 路径上没接", 而外部看完全一样。
//
// 观测点是**出站请求本身** (假 LLM 服务抓到的 tools), 不是任何中间变量。
//
// 这条路径改造前比飞书那条更彻底: runNestedAgent 一律 RegisterBaseTools, 连
// "按档位装配"这件事都不存在。所以这里要证的不是"档位选对了", 而是"资产声明
// **第一次**在这条路径上有了落点", 且落点只能是收窄方向。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	cliP5Marker = "CLI-P5-资产正文标记-4c17"
	cliP5Doc    = "你是一个只读的审计者。\n\n" + cliP5Marker + "\n"
)

// cliP5Req 一次出站请求里本用例关心的部分。
type cliP5Req struct {
	tools  map[string]bool
	system string
}

// cliP5Server 起一个抓请求的假 LLM 服务, 返回服务与已捕获的请求列表。
func cliP5Server(t *testing.T) (*httptest.Server, *[]cliP5Req) {
	t.Helper()
	var got []cliP5Req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			System interface{} `json:"system"`
			Tools  []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &req); err == nil {
			c := cliP5Req{tools: map[string]bool{}}
			for _, tl := range req.Tools {
				c.tools[tl.Name] = true
			}
			switch v := req.System.(type) {
			case string:
				c.system = v
			case []interface{}:
				var sb strings.Builder
				for _, blk := range v {
					if m, ok := blk.(map[string]interface{}); ok {
						if txt, ok := m["text"].(string); ok {
							sb.WriteString(txt)
							sb.WriteString("\n")
						}
					}
				}
				c.system = sb.String()
			}
			got = append(got, c)
		}
		writeCLIP5SSE(w, t)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// writeCLIP5SSE 一条最小合法 SSE (单个文本块, end_turn, 无工具调用)。
func writeCLIP5SSE(w http.ResponseWriter, t *testing.T) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(d types.StreamDelta) {
		b, err := json.Marshal(d)
		if err != nil {
			t.Errorf("序列化 SSE 失败: %v", err)
			return
		}
		w.Write([]byte("data: " + string(b) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(types.StreamDelta{Type: "message_start", Message: &types.APIResponse{Usage: &types.Usage{InputTokens: 12}}})
	send(types.StreamDelta{Type: "content_block_start", ContentBlock: &types.ContentBlock{Type: types.ContentBlockText}})
	send(types.StreamDelta{Type: "content_block_delta", Delta: &types.DeltaContent{Type: "text_delta", Text: "好了"}})
	send(types.StreamDelta{Type: "content_block_stop"})
	send(types.StreamDelta{Type: "message_delta", Delta: &types.DeltaContent{StopReason: "end_turn"}, Usage: &types.Usage{OutputTokens: 7}})
	send(types.StreamDelta{Type: "message_stop"})
}

// cliP5Deps 造一份最小可跑的 engineDeps。
//
// 走**真实的 runNestedAgent**, 不碰 runCmd 的整条 RunE —— 后者要起 MCP/网关/编排,
// 与本用例要验的东西无关, 而那些依赖会让用例变成"装配不起来就跳过"。
func cliP5Deps(t *testing.T, srv *httptest.Server) *engineDeps {
	t.Helper()
	cwd := t.TempDir()
	cfg := &engine.Config{
		Model:            "test-model",
		MaxTokens:        1024,
		MaxTurns:         3,
		Cwd:              cwd,
		PermissionMode:   types.PermissionModeDefault,
		IsNonInteractive: true,
	}
	client := api.NewClient(srv.URL, "test-key", "test-model")
	client.Tag = "test"
	return &engineDeps{
		cfg:         cfg,
		apiClient:   client,
		hookRunner:  hooks.NewRunner(nil, ""),
		permChecker: permissions.NewChecker(types.PermissionModeDefault),
		compactor:   compact.NewCompactor(client, 200000),
		promptMgr:   prompt.NewManager(cwd),
	}
}

// installCLIAgentAsset 按标准布局写一份 agent 资产并重建索引。
func installCLIAgentAsset(t *testing.T, cwd, name, frontmatter, body string) func() {
	t.Helper()
	stateDir := filepath.Join(cwd, ".claude-go")
	dir := filepath.Join(stateDir, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := agent.AgentCatalog()
	agent.SetAgentCatalog(asset.LoadStandard(asset.LoadRoots{State: stateDir}))
	return func() { agent.SetAgentCatalog(prev) }
}

// TestCLI资产档位与正文都接进子代理 是主用例。
//
// 两条断言必须同时成立:
//   - 档位: 声明 analysis ⇒ Read 在, Write/Bash 不在 (它们只在编码档);
//   - 正文: 资产正文出现在出站 system 里 —— 只换工具集而不给角色指令, 子代理会拿到
//     一把被削过的工具却不知道自己被派来干什么。
func TestCLI资产档位与正文都接进子代理(t *testing.T) {
	srv, reqs := cliP5Server(t)
	deps := cliP5Deps(t, srv)
	restore := installCLIAgentAsset(t, deps.cfg.Cwd, "auditor",
		"name: auditor\ntool-profile: analysis\ndescription: 只读审计", cliP5Doc)
	defer restore()

	if _, err := runNestedAgent(context.Background(), deps, nil, "去查一下",
		agent.RunOptions{SubagentType: "auditor"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到请求, 后面的断言全是空断言")
	}
	last := (*reqs)[len(*reqs)-1]
	if !last.tools["Read"] {
		t.Errorf("声明 analysis 时应注册只读档 (tools=%v)", cliKeys(last.tools))
	}
	if !strings.Contains(last.system, cliP5Marker) {
		t.Errorf("资产正文没进子代理的 system 提示词 —— 只削工具不给角色指令, 子代理不知道自己被派来干什么")
	}
	if last.tools["Bash"] || last.tools["Write"] {
		t.Errorf("analysis 档不该有 Bash/Write (tools=%v)", cliKeys(last.tools))
	}

	// 阳性对照: 没有资产时走基线 (RegisterBaseTools), Bash/Write 必须在。
	// 缺了它, 上面那条"没有 Bash"无法区分"档位生效"与"这条路径本来就没有 Bash"。
	if _, err := runNestedAgent(context.Background(), deps, nil, "去写代码",
		agent.RunOptions{SubagentType: "coder-nonexistent"}); err != nil {
		t.Fatalf("对照派生失败: %v", err)
	}
	ctrl := (*reqs)[len(*reqs)-1]
	if !ctrl.tools["Bash"] || !ctrl.tools["Write"] {
		t.Fatalf("对照run 应走基线全量工具集 (tools=%v) —— 否则主用例的结论不成立", cliKeys(ctrl.tools))
	}
	if strings.Contains(ctrl.system, cliP5Marker) {
		t.Errorf("对照run 的 system 里出现了资产正文 —— 正文不是按名字查出来的")
	}
}

// TestCLI不收宽声明被拒 断言"资产声明不得放宽工具面"。
//
// 这条路径的基线是全量工具集, 所以任何声明都无法放宽 —— 但**没有任何声明**与
// **有一个非法/未知档位**必须表现一致, 而 builtin.NormalizeToolProfile 会把不认识的
// 串静默掰成 chat。若不在这里挡住, `tool-profile: codign` 会被解读成 chat: 子代理
// 突然连 Read/Bash 都没有了, 而日志上一个字都没有。
func TestCLI不收宽声明被拒(t *testing.T) {
	srv, reqs := cliP5Server(t)
	deps := cliP5Deps(t, srv)
	restore := installCLIAgentAsset(t, deps.cfg.Cwd, "typo",
		"name: typo\ntool-profile: codign\ndescription: 拼错了", cliP5Doc)
	defer restore()

	if _, err := runNestedAgent(context.Background(), deps, nil, "去查一下",
		agent.RunOptions{SubagentType: "typo"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	last := (*reqs)[len(*reqs)-1]
	if !last.tools["Bash"] {
		t.Errorf("非法档位声明被当成了某个合法档位 (tools=%v) —— 静默降级正是要防的", cliKeys(last.tools))
	}
	if !strings.Contains(last.system, cliP5Marker) {
		t.Errorf("档位声明非法不该连正文一起丢 —— 名字查得到, 正文就该生效")
	}
}

// TestCLI资产清单喂进Agent工具描述 补上清单那一半。
//
// 只有查表时, 模型猜不出该填什么名字, 于是绝大多数派生走的还是猜的那条路 ——
// 这条断言就是"清单真的到了模型手上"的判据。
func TestCLI资产清单喂进Agent工具描述(t *testing.T) {
	agent.SetAgentCatalog(nil)
	defer agent.SetAgentCatalog(nil)
	tool := agent.NewAgentTool(nil)
	if strings.Contains(tool.Description(), "subagent_type") {
		t.Fatalf("空资产体系不该提清单, got %q", tool.Description())
	}

	cwd := t.TempDir()
	restore := installCLIAgentAsset(t, cwd, "auditor",
		"name: auditor\ndescription: 只读审计", cliP5Doc)
	defer restore()

	desc := agent.NewAgentTool(nil).Description()
	if !strings.Contains(desc, "auditor") {
		t.Fatalf("资产清单没进 Agent 工具描述 —— 模型无从知道该填什么名字: %q", desc)
	}
	if !strings.Contains(desc, "subagent_type") {
		t.Errorf("清单必须写明名字填到哪个参数, got %q", desc)
	}
}

func cliKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ============================================================================
// P2′ 工具名单在 CLI 这条路径上的通电 (21.8.12 P2′)
// ============================================================================

// TestCLI黑名单真的从出站请求里去掉工具 是 P2′ 在 CLI 路径上的全链条判据。
//
// 与飞书那条同形, 但**必须单独存在**: 两条路径的装配代码是两套 (飞书经
// ConstraintSet, CLI 直接写 cfg 的两个字段)。只在飞书那条验过, 得到的是
// "能力在 A 路径通了" —— 而本仓大量缺陷正是这个形态, 外部看完全一样。
//
// 判据同样是出站请求的 tools 数组, 而不是 cfg.DisabledTools 这个字段:
// 断言字段等于把"我写进去了"当成"它生效了", 而 P2′ 修的恰恰是这两者之间的缝。
func TestCLI黑名单真的从出站请求里去掉工具(t *testing.T) {
	srv, reqs := cliP5Server(t)
	deps := cliP5Deps(t, srv)

	// 对照: 同档位, 不带黑名单 —— Bash 必须可见。
	restoreBase := installCLIAgentAsset(t, deps.cfg.Cwd, "cli-base",
		"name: cli-base\ntool-profile: coding\ndescription: 基线", cliP5Doc)
	defer restoreBase()
	if _, err := runNestedAgent(context.Background(), deps, nil, "干活",
		agent.RunOptions{SubagentType: "cli-base"}); err != nil {
		t.Fatalf("对照派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到请求, 后面的断言全是空断言")
	}
	if !(*reqs)[len(*reqs)-1].tools["Bash"] {
		t.Fatalf("对照里没有 Bash —— coding 档本该注册它, 本用例的前提不成立 (%v)",
			cliKeys((*reqs)[len(*reqs)-1].tools))
	}

	// 正例: 只多一行 disallowed-tools。
	restore := installCLIAgentAsset(t, deps.cfg.Cwd, "cli-nobash",
		"name: cli-nobash\ntool-profile: coding\ndescription: 不许 shell\ndisallowed-tools: [Bash]", cliP5Doc)
	defer restore()
	if _, err := runNestedAgent(context.Background(), deps, nil, "干活",
		agent.RunOptions{SubagentType: "cli-nobash"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	last := (*reqs)[len(*reqs)-1]
	if last.tools["Bash"] {
		t.Error("资产声明 disallowed-tools: [Bash], 而出站请求里仍然有 Bash —— " +
			"CLI 这条路径上的声明没接进运行时 (声明了但没人读)")
	}
	if !last.tools["Read"] {
		t.Errorf("黑名单应当只去掉被点名的工具, Read 不该受影响 (%v)", cliKeys(last.tools))
	}
	if len(last.tools) < 2 {
		t.Fatalf("正例工具面只剩 %d 个 —— 判据区分不出「精确挡住 Bash」与「清空了工具面」", len(last.tools))
	}
}

// TestCLI白名单从出站请求里收窄工具面 补上白名单那一半。
//
// 改造前这条路径**完全不读** `tools:` —— 它只影响装配读数, 不影响注册表。所以
// 这个用例在改造前是红的, 而它在读数值上完全看不出来 (读数是"声明了 [Read]")。
func TestCLI白名单从出站请求里收窄工具面(t *testing.T) {
	srv, reqs := cliP5Server(t)
	deps := cliP5Deps(t, srv)

	restore := installCLIAgentAsset(t, deps.cfg.Cwd, "cli-onlyread",
		"name: cli-onlyread\ntool-profile: coding\ndescription: 只读\ntools: [Read, Grep]", cliP5Doc)
	defer restore()
	if _, err := runNestedAgent(context.Background(), deps, nil, "干活",
		agent.RunOptions{SubagentType: "cli-onlyread"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	last := (*reqs)[len(*reqs)-1]

	if !last.tools["Read"] || !last.tools["Grep"] {
		t.Errorf("白名单里的工具必须可见 (%v)", cliKeys(last.tools))
	}
	// 关键: 注册了但**不在**白名单里的工具必须对模型不可见。coding 档注册了 Bash,
	// 所以这条断言量的是白名单而不是注册表 —— 若白名单没接上, Bash 会在这里出现。
	if last.tools["Bash"] || last.tools["Write"] {
		t.Errorf("资产声明 tools: [Read, Grep], 而出站请求里仍有名单外的工具 —— "+
			"白名单没接进运行时 (%v)", cliKeys(last.tools))
	}
}

// TestCLI显式空白名单拒绝一切 验 fail-closed 那一档, 也是最容易写坏的一档。
//
// `tools: []` 与"没写 tools:" 在 YAML 解析之后都是"长度为 0 的列表", 差别只在
// nil 与非 nil —— 而这两者的运行期后果相反: 前者拒绝一切工具, 后者跟随基线。
// 合并它们会让一份**故意**的 fail-closed 声明静默退化成"什么限制都没有"。
func TestCLI显式空白名单拒绝一切(t *testing.T) {
	srv, reqs := cliP5Server(t)
	deps := cliP5Deps(t, srv)

	restore := installCLIAgentAsset(t, deps.cfg.Cwd, "cli-nothing",
		"name: cli-nothing\ntool-profile: coding\ndescription: 什么都不给\ntools: []", cliP5Doc)
	defer restore()
	if _, err := runNestedAgent(context.Background(), deps, nil, "干活",
		agent.RunOptions{SubagentType: "cli-nothing"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	last := (*reqs)[len(*reqs)-1]
	if len(last.tools) != 0 {
		t.Errorf("`tools: []` 是显式空白名单 (fail-closed, 拒绝一切), 与「没写 tools:」"+
			"必须表现不同 —— 实际仍给了 %d 个工具 (%v)", len(last.tools), cliKeys(last.tools))
	}
}
