package feishu

// p5_agent_lookup_test —— 21.8.12 P5「subagent_type 真的被查过表」的通电验收。
//
// ---------------------------------------------------------------------------
// 为什么必须走真实入口
// ---------------------------------------------------------------------------
//
// 改造前的事实: 模型在 Agent 工具里填的 subagent_type 从来没有被查过表, 只被
// profileForRunOptions 拿去做**子串猜测**。于是一份真实存在的 agents/<name>.md
// 声明**存在也读不到** —— 目录在、文件在、没有任何报错, 而"我写了这份 agent 定义
// 怎么没生效"从一个外部视角完全成立。
//
// pkg/agent 侧的测试证明"清单会拼进 Description"; 本文件证明"飞书装配在真的派生
// 时按它查了表"。这两件事在本仓被区分过很多次 —— 能力写完了但装配处没接, 症状与
// "没实现"完全一样。所以这里的观测点是**出站请求本身** (fake LLM server 抓到的
// system + tools), 不是任何中间变量:
//
//   - 提示词: 命中查表 ⇒ 资产正文出现在 system 里;
//   - 工具档位: 命中查表 ⇒ 请求里的 tools 与 analysis 档相符。
//
// 判据是"出站请求里有什么", 不是"某个函数返回了什么" —— 后者可以全绿而请求照旧。
//
// ---------------------------------------------------------------------------
// 三个档位的可观测差异 (决定了断言该看哪个工具名)
// ---------------------------------------------------------------------------
//
// pkg/tool/builtin 的各档注册集互不相同, 可用作区分:
//
//	coding   有 Bash / Write / Read
//	analysis 有 Read / Glob / WebFetch / WebSearch, **没有** Bash / AskUserQuestion
//	chat     只有 AskUserQuestion / codeIntel 只读 / ToolSearch, **没有** Read
//
// 所以 analysis 与 chat 之间的判据是 `Read` 的有无, 不是 `Bash` 的有无 —— 两者
// 都没有 Bash。这一点是我第一版写错的地方: 拿"没有 Bash"去断言 analysis, 那么
// 一个恒被判成 chat 的实现也能让用例全绿。同一条测试里另配一个 `coder` 派生作
// 阳性对照, 证明 `Bash` 这枚探针在 coding 档下**确实看得见** —— 否则"没看到 Bash"
// 无法区分"档位正确"与"探针根本不工作"。这正是 layout/paint 那类
// "尺寸不是绘制证据"的同一种错误。

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
	"github.com/anthropic/claude-go/pkg/agentbind"
	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/statestore"
)

// capturedRequest 一次出站请求里本用例关心的部分。
type capturedRequest struct {
	System string
	Tools  map[string]bool
}

// captureLLMServer 起一个抓请求的假 LLM 服务。
//
// 与 newFakeLLMServer 的区别只在"把请求体留下来" —— 响应仍是同一条最小合法 SSE,
// 因此被测路径的行为与既有测试逐字节一致。
func captureLLMServer(t *testing.T, reply string) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var got []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			System interface{} `json:"system"`
			Tools  []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &req); err == nil {
			c := capturedRequest{Tools: map[string]bool{}}
			for _, t2 := range req.Tools {
				c.Tools[t2.Name] = true
			}
			// system 可能是字符串或块数组, 两种形态都摊平成一段文本 —— 本用例只关心
			// "那段文字在不在", 不关心它落在哪个块里。
			switch v := req.System.(type) {
			case string:
				c.System = v
			case []interface{}:
				var sb strings.Builder
				for _, blk := range v {
					if m, ok := blk.(map[string]interface{}); ok {
						if s, ok := m["text"].(string); ok {
							sb.WriteString(s)
							sb.WriteString("\n")
						}
					}
				}
				c.System = sb.String()
			}
			got = append(got, c)
		}
		writeFakeSSE(w, t, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// stateDirFor 一个项目目录对应的状态目录, 与 newTestSessionManagerWithLLM 里
// `cfg.StateDir = filepath.Join(cfg.Cwd, ".claude-go")` 逐字节一致。
//
// 单独拎出来是因为这里的目录约定有一个**容易搞反**的地方: LoadRoots.State 是
// `<stateDir>` 本身 (资产落在 <stateDir>/agents), 而不是项目目录 —— 项目目录对应
// 的是 LoadRoots.Project, 资产落在 <cwd>/.claude-go/agents。测试里写错这一层不会
// 报任何错, 只会表现为"资产明明写了却查不到", 也就是把被测缺陷原样重演一遍。
func stateDirFor(cwd string) string { return filepath.Join(cwd, ".claude-go") }

// installAgentAsset 把一份 agent 资产写进**标准布局**并装载索引, 返回还原函数。
//
// 走的必须是真实装载路径 (asset.LoadStandard), 不是手工 new 一个 Set:
// 手工构造能过而真实路径过不了的接法, 正是"声明了但没人读"最爱的藏身处。
func installAgentAsset(t *testing.T, cwd, name, frontmatter, body string) func() {
	t.Helper()
	dir := filepath.Join(stateDirFor(cwd), "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := body
	if frontmatter != "" {
		content = "---\n" + frontmatter + "\n---\n" + body
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return reloadCatalog(t, cwd)
}

// reloadCatalog 按真实装载路径重扫状态目录并装进进程级索引。
//
// 只给 State 来源, **不**给 Project/User 是刻意的: 生产上的 Project/User 来源会扫
// 到开发机上真实存在的 ~/.claude-go 与仓库里的 .claude-go, 于是测试结果取决于跑测试
// 的那台机器上恰好有什么 —— 本机绿、CI 红。测试要密闭。
func reloadCatalog(t *testing.T, cwd string) func() {
	t.Helper()
	prev := agent.AgentCatalog()
	agent.SetAgentCatalog(asset.LoadStandard(asset.LoadRoots{State: stateDirFor(cwd)}))
	return func() { agent.SetAgentCatalog(prev) }
}

// 命中的资产: 正文带一个不可能自然出现的标记, 档位声明为 analysis。
const (
	p5Marker = "P5-资产正文标记-9f3a"
	p5Doc    = "你是一个只读的审计者。\n\n" + p5Marker + "\n"
)

func p5AssetFM() string { return "name: auditor\ntool-profile: analysis\ndescription: 只读审计" }

// TestP5查表命中时资产正文与档位同时生效 是本文件的主用例。
//
// 两个断言必须**同时**成立才叫接上了:
//   - 只接正文不接档位: 模型读到了"你是只读审计者", 手里却仍有 Bash —— 声明与
//     权限脱节, 而这是比"完全没接"更危险的一种 (看起来接上了);
//   - 只接档位不接正文: 子代理拿到了窄工具集, 却不知道自己被派来干什么。
func TestP5查表命中时资产正文与档位同时生效(t *testing.T) {
	srv, reqs := captureLLMServer(t, "审计完毕")
	ss := statestore.NewMemStore()
	sm := newTestSessionManagerWithLLM(t, ss, srv.URL, nil)

	restore := installAgentAsset(t, sm.config.Cwd, "auditor", p5AssetFM(), p5Doc)
	defer restore()

	if _, err := sm.runNestedAgent(context.Background(), nil, "去查一下",
		agent.RunOptions{SubagentType: "auditor"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到任何请求, 后面两条断言会变成空断言 (空集上的全过恒真)")
	}
	last := (*reqs)[len(*reqs)-1]

	if !strings.Contains(last.System, p5Marker) {
		t.Errorf("资产正文未进 system 提示词 —— 查表没接上 (声明存在也读不到)")
	}
	if !last.Tools["Read"] {
		t.Errorf("资产声明 tool-profile=analysis, 出站 tools 却无 Read (tools=%v) —— 档位没接上", keysOf(last.Tools))
	}
	if last.Tools["Bash"] {
		t.Errorf("analysis 档不该有 Bash (tools=%v)", keysOf(last.Tools))
	}

	// 阳性对照: Bash 这枚探针在 coding 档下必须看得见。缺了它, 上面那条
	// "没有 Bash"无法区分"档位正确"与"探针根本不工作"。
	if _, err := sm.runNestedAgent(context.Background(), nil, "去写代码",
		agent.RunOptions{SubagentType: "coder"}); err != nil {
		t.Fatalf("阳性对照派生失败: %v", err)
	}
	pos := (*reqs)[len(*reqs)-1]
	if !pos.Tools["Bash"] {
		t.Fatalf("阳性对照失败: subagent_type=coder 走的是 coding 档, 应当有 Bash (tools=%v) —— "+
			"探针不工作, 本文件全部档位断言都无效", keysOf(pos.Tools))
	}

	// 反向读数: 同一环境, 换一个**没有资产**的 subagent_type。若这一轮也有 Read,
	// 说明上面那条断言量到的不是这份资产 —— 那种情况下主用例是假绿。
	if _, err := sm.runNestedAgent(context.Background(), nil, "去查一下",
		agent.RunOptions{SubagentType: "auditor-nonexistent"}); err != nil {
		t.Fatalf("对照派生失败: %v", err)
	}
	ctrl := (*reqs)[len(*reqs)-1]
	if ctrl.Tools["Read"] {
		t.Fatalf("对照run 里也有 Read —— 读数无分辨力, 主用例的结论不成立 (tools=%v)", keysOf(ctrl.Tools))
	}
	if strings.Contains(ctrl.System, p5Marker) {
		t.Errorf("对照run 的 system 里出现了资产正文 —— 正文不是按名字查出来的")
	}
}

// TestP5查表是精确匹配不做模糊 锁死"猜一个像样的"这条路的边界。
//
// 名字带尾巴时**必须** miss 并回退猜测。若做前缀/包含匹配, `auditor2` 会静默拿到
// `auditor` 的正文与档位 —— 那次替换没有任何用户可见的信号: 模型以为派了 A,
// 实际跑的是 B (含 B 的权限)。
func TestP5查表是精确匹配不做模糊(t *testing.T) {
	srv, reqs := captureLLMServer(t, "好了")
	ss := statestore.NewMemStore()
	sm := newTestSessionManagerWithLLM(t, ss, srv.URL, nil)

	restore := installAgentAsset(t, sm.config.Cwd, "auditor", p5AssetFM(), p5Doc)
	defer restore()

	// 三条都**不该**命中: 多一个字符 / 少一个字符 / 大小写不同。
	//
	// 探针用 Read (analysis 有、chat 没有), 并逐条断言它**必须**缺席:
	// 这三个名字都不含任何 profileForRunOptions 认得的子串 ("auditor2" 里没有
	// "coder"/"review"/"research"...), 所以回退的猜测结果是 chat。
	for _, name := range []string{"auditor2", "audito", "AUDITOR"} {
		if _, err := sm.runNestedAgent(context.Background(), nil, "去查一下",
			agent.RunOptions{SubagentType: name}); err != nil {
			t.Fatalf("派生 %q 失败: %v", name, err)
		}
		last := (*reqs)[len(*reqs)-1]
		if strings.Contains(last.System, p5Marker) {
			t.Errorf("subagent_type=%q 命中了名为 auditor 的资产 —— 模糊匹配会让模型派的是 A 而跑的是 B", name)
		}
		if last.Tools["Read"] {
			t.Errorf("subagent_type=%q 拿到了 auditor 的 analysis 档 (tools=%v)—— "+
				"名字不符却享受了它的权限与能力", name, keysOf(last.Tools))
		}
	}
}

// TestP5非法档位声明被拒且不静默降级 覆盖"资产写错了"这一种真实运维现场。
//
// builtin.NormalizeToolProfile 对不认识的串**静默回落 chat**。若不在这里挡住,
// `tool-profile: codign` 的表现是"这个 agent 拿到了最窄的档位" —— 一次没有任何
// 报错的降级, 而 chat 档连 Read 都没有, 子代理会表现得像"什么工具都不会用"。
func TestP5非法档位声明被拒且不静默降级(t *testing.T) {
	srv, reqs := captureLLMServer(t, "好了")
	ss := statestore.NewMemStore()
	sm := newTestSessionManagerWithLLM(t, ss, srv.URL, nil)

	restore := installAgentAsset(t, sm.config.Cwd, "typo",
		"name: typo\ntool-profile: codign\ndescription: 拼错了", p5Doc)
	defer restore()

	if _, err := sm.runNestedAgent(context.Background(), nil, "去查一下",
		agent.RunOptions{SubagentType: "typo"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	last := (*reqs)[len(*reqs)-1]
	// 非法声明 ⇒ 该档位声明被忽略, 回退到猜测 ("typo" 不含任何关键词 ⇒ chat)。
	// 两条一起断言才说明"是这一处被忽略了", 而不是整份资产都没读到:
	//   ① 正文照常生效 (名字这一维是对的, 查表命中了);
	//   ② 档位不是声明里那个拼错的串被 NormalizeToolProfile 静默掰成的 chat ——
	//      而 chat 的判据也是"没有 Read", 与"声明被忽略"同形。
	// 所以这里必须再补一条**正向**证据, 证明这份资产确实被读到了: 用同一份资产、
	// 只改档位拼写, 看它是否生效。见下面的 TestP5档位拼写正确时才生效。
	if !strings.Contains(last.System, p5Marker) {
		t.Errorf("档位声明非法不该连正文一起丢 —— 名字查得到, 正文就该生效")
	}
	if last.Tools["Read"] {
		t.Errorf("非法档位声明被当成了 analysis (tools=%v)—— 静默降级正是要防的", keysOf(last.Tools))
	}
}

// TestP5档位拼写正确时才生效 是上一条的**阳性对照**: 同一份资产、同一段正文,
// 只把 tool-profile 拼对, 档位就必须生效。
//
// 两条合起来才排除了"analysis 这个取值在我们的装配里本来就到不了"这种可能 ——
// 只有上一条时, 一个"从不采用任何资产档位"的实现也能全绿。
func TestP5档位拼写正确时才生效(t *testing.T) {
	srv, reqs := captureLLMServer(t, "好了")
	ss := statestore.NewMemStore()
	sm := newTestSessionManagerWithLLM(t, ss, srv.URL, nil)

	restore := installAgentAsset(t, sm.config.Cwd, "spelled-right",
		"name: spelled-right\ntool-profile: analysis\ndescription: 拼对了", p5Doc)
	defer restore()

	if _, err := sm.runNestedAgent(context.Background(), nil, "去查一下",
		agent.RunOptions{SubagentType: "spelled-right"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	last := (*reqs)[len(*reqs)-1]
	if !last.Tools["Read"] {
		t.Fatalf("档位拼写正确时 analysis 必须生效 (tools=%v) —— 否则上一条用例是假绿: "+
			"它断言的'拼错了所以没生效'可能只是'这个取值从来就没生效过'", keysOf(last.Tools))
	}
}

// TestP5影子资产不进清单 是灰度机制的边界: 影子资产被模型直接调用等于绕过全部灰度。
func TestP5影子资产不进清单(t *testing.T) {
	cwd := t.TempDir()
	restore := installAgentAsset(t, cwd, "live-one", "name: live-one\ndescription: 正式", "正文")
	defer restore()
	if err := os.WriteFile(filepath.Join(cwd, ".claude-go", "agents", "shadow-one.md"),
		[]byte("---\nname: shadow-one\nstatus: shadow\ndescription: 影子\n---\n正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore2 := reloadCatalog(t, cwd)
	defer restore2()

	m := agent.AgentManifest()
	if !strings.Contains(m, "live-one") {
		t.Fatalf("正式资产应进清单, got %q", m)
	}
	if strings.Contains(m, "shadow-one") {
		t.Fatalf("影子资产不该进清单 (列出来等于绕过灰度), got %q", m)
	}
}

// TestP5空资产体系不改任何既有行为 是这次改造的**回归底线**。
//
// 六种资产在绝大部分部署上还是空的。空的资产体系若不等于"改造前", 那么这次上线
// 会静默改变所有没装资产的部署 —— 而它们恰恰是当前的全部生产。
func TestP5空资产体系不改任何既有行为(t *testing.T) {
	agent.SetAgentCatalog(asset.NewSet())
	defer agent.SetAgentCatalog(nil)

	if m := agent.AgentManifest(); m != "" {
		t.Fatalf("空资产体系的清单应为空串 (拼接方按 != \"\" 判定), got %q", m)
	}
	tool := agent.NewAgentTool(nil)
	const want = "Launch a new agent to handle complex, multi-step tasks autonomously.\n" +
		"The agent runs in a separate context and returns results when complete."
	if got := tool.Description(); got != want {
		t.Fatalf("空资产体系下 Agent 工具描述与改造前不一致:\n got=%q\nwant=%q", got, want)
	}
	// nil 资产体系 (从未调用 SetAgentCatalog) 同样不得 panic —— 大量测试与裸调走这条。
	agent.SetAgentCatalog(nil)
	if m := agent.AgentManifest(); m != "" {
		t.Fatalf("nil 资产体系的清单应为空串, got %q", m)
	}
	// 走**装配那条真实入口** (P2 之后它取代了裸查表): nil 资产体系下装配结果必须
	// 是"什么都没有" —— 没有资产可指, 也没有任何一面声明可用。
	if b := agentbind.BindAgentAsset("any", nil); b == nil || b.Asset != nil {
		t.Fatalf("nil 资产体系下装配应查不到资产, got %+v", b)
	} else if b.HasToolProfile || b.HasPermissionMode || b.Prompt != "" || len(b.Hooks) > 0 {
		t.Fatalf("没有资产时不得凭空空出任何一面声明, got %+v", b)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
