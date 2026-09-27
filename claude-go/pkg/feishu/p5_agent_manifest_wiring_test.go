package feishu

// p5_agent_manifest_wiring_test —— 21.8.12 P5「agent 清单**真的**喂到模型眼前」的通电验收。
//
// ---------------------------------------------------------------------------
// 为什么清单那一半要单独验一次 (P5 明说"两步缺一不可")
// ---------------------------------------------------------------------------
//
// P5 是两条腿: ①`subagent_type` 真查表 (p5_agent_lookup_test.go); ②**agent 清单喂给
// 模型**。改造前 `AgentTool.Description()` 是一段写死的英文, 一个名字都没有, 于是
// 模型只能"猜一个像样的名字" —— 而瞎猜**今天就能过**, 它没有任何动机改。
//
// ②这一半的成品是**一段字符串**, 这让它比①更容易假绿: `AgentManifest()` 返回得再漂亮,
// 只要拼接那一行不在, 出站请求里就一个字都没有, 而**没有任何一层会报错**。
// 变异测试证实了这一点 —— 把 `Description()` 里的拼接删掉 (P5-1)、把"这些名字填到哪"
// 的说明句删掉 (P5-2), 既有全部用例**照绿**(它们只断言 `AgentManifest()` 的返回值)。
//
// ---------------------------------------------------------------------------
// 判据落在**出站请求本身**, 不落在任何中间变量
// ---------------------------------------------------------------------------
//
// 本文件用与 p5_agent_lookup_test.go 同一个假 LLM 服务, 直接把 `tools[].description`
// 摊平成一个字符串再断言。这是模型唯一看得见的那份字节 —— 断言 `Description()` 的
// 返回值仍然漏掉了注册表这一环 (工具注册了没有、注册的是哪一份), 而"能力写完了但
// 装配处没接"在本仓被区分过很多次 (见 P4 补记之四)。
//
// 走的是**生产装配入口** `newProfileRegistry` (与真实会话那条路同一个函数), 不是
// `reg.Register(agent.NewAgentTool(nil))` —— 从内部某一步进, 测到的只是"那一步做得对"。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// agentToolDescriptionFor 走生产装配路径造一个注册表, 取出 Agent 工具的描述。
//
// traceStore 传 nil 时 NewAgentToolWithTrace 与 NewAgentTool 完全等价 (见它的注释),
// 所以这里量到的与生产是同一份描述。
func agentToolDescriptionFor(t *testing.T, sm *SessionManager) string {
	t.Helper()
	reg := sm.newProfileRegistry(builtin.ToolProfileChat, registryOptions{
		includeAgent: true,
		runAgentFn: func(context.Context, string, agent.RunOptions) (string, error) {
			return "", nil // 本文件只量描述, 不让派生真的发生
		},
	})
	tl, ok := reg.Get(agent.AgentToolName)
	if !ok {
		t.Fatalf("chat 档 + includeAgent 下注册表里必须有 %q 工具 —— "+
			"少了它, 本文件全部断言都会变成空断言", agent.AgentToolName)
	}
	return tl.Description()
}

// TestP5Agent清单真的进了出站工具描述 是②那一半的核心判据。
//
// 三件事一起断言, 因为它们在改造前/改造中各自会独立失效:
//   - 清单里的**名字**在模型眼前的描述里 (只做①查表不列清单 = 模型不知道能填什么);
//   - 有一句话说明这些名字**该填到哪** (`subagent_type`) —— 光有一串名字, 模型得
//     自己猜那是给哪个字段用的 (变异 P5-2: 删掉它, 既有用例全绿);
//   - **影子资产不在**清单里 (列出来等于绕过灰度)。
func TestP5Agent清单真的进了出站工具描述(t *testing.T) {
	ss := statestore.NewMemStore()
	sm := newTestSessionManagerWithLLM(t, ss, "http://127.0.0.1:1", nil)

	restore := installAgentAsset(t, sm.config.Cwd, "manifest-auditor",
		"name: manifest-auditor\ndescription: 只读审计", "正文")
	defer restore()

	desc := agentToolDescriptionFor(t, sm)
	if !strings.Contains(desc, "manifest-auditor") {
		t.Fatalf("资产名没有出现在 Agent 工具描述里 —— 模型看不到可填的清单, 只能瞎猜 "+
			"(而瞎猜今天就能过, 见 P5 缺口 B)。描述: %q", desc)
	}
	if !strings.Contains(desc, "subagent_type") {
		t.Fatalf("清单里没有说明这些名字该填到哪个参数 —— 一串名字本身不自证用途。描述: %q", desc)
	}

	// 阴性对照: 描述里不该出现影子资产。缺了这条, "名字在不在"就是唯一判据, 而一个
	// 无视 status 的渲染实现照样能过。
	const shadow = "manifest-shadow"
	dir := filepath.Join(sm.config.Cwd, ".claude-go", "agents")
	if err := os.WriteFile(filepath.Join(dir, shadow+".md"),
		[]byte("---\nname: "+shadow+"\nstatus: shadow\ndescription: 影子\n---\n正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore2 := reloadCatalog(t, sm.config.Cwd)
	defer restore2()
	if d2 := agentToolDescriptionFor(t, sm); strings.Contains(d2, shadow) {
		t.Fatalf("影子资产进了模型眼前的清单 (等于绕过灰度): %q", d2)
	}
}

// TestP5清单为空时描述逐字节等于改造前 是这次改造的**回归底线**, 且必须量在
// **组装好的描述**上。
//
// 六种资产在绝大部分部署上还是空的。空资产体系若不等于"改造前", 这次上线会静默改变
// 所有没装资产的部署 —— 而它们恰恰是当前的全部生产。
//
// 与 `TestP5空资产体系不改任何既有行为` 的分工: 那一条量 `AgentManifest()` 的返回值,
// 这一条量**最终描述**。两条都要, 因为它们各自能独立失效 —— 清单渲染对了而拼接写错,
// 与拼接对了而渲染写出一个占位串, 是两种不同的缺陷。
func TestP5清单为空时描述逐字节等于改造前(t *testing.T) {
	ss := statestore.NewMemStore()
	sm := newTestSessionManagerWithLLM(t, ss, "http://127.0.0.1:1", nil)

	// 本用例不装任何资产 —— newTestSessionManagerWithLLM 的 cwd 是 t.TempDir(),
	// 所以进程级索引会是空的。
	prev := agent.AgentCatalog()
	agent.SetAgentCatalog(nil)
	defer agent.SetAgentCatalog(prev)
	_ = ss

	const want = "Launch a new agent to handle complex, multi-step tasks autonomously.\n" +
		"The agent runs in a separate context and returns results when complete."
	if got := agentToolDescriptionFor(t, sm); got != want {
		t.Fatalf("空资产体系下出站描述与改造前不一致 —— 这次上线会静默改变所有没装资产的部署:\n got=%q\nwant=%q", got, want)
	}
}
