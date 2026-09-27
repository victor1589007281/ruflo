package feishu

// p2_assembly_test —— 21.8.12 P2 组装器在**飞书派生路径**上的通电验收。
//
// ---------------------------------------------------------------------------
// 这一条与 pkg/agentbind 的测试的分工
// ---------------------------------------------------------------------------
//
// agentbind 那个文件证明"读数是对的": 声明被解析成了什么、每一面接没接上、非法值
// 有没有被挡住。本文件证明**那些读数真的变成了运行期行为** —— 两者缺一不可:
//
//	只测 agentbind  → 组装器读得再对, 调用点不消费它, 一样是"声明了但没人读";
//	只测本文件      → 行为对了, 但说不清是哪条声明起的作用 (四条绑定混在一起)。
//
// 所以这里的每一条都走**真实的派生入口** (runNestedAgent), 观测点是运行期的实际
// 后果 —— 出站请求、被拒的工具调用、真的被执行了的 hook 进程、系统提示词里的技能
// 清单。没有一条断言中间变量。
//
// ---------------------------------------------------------------------------
// 每条绑定各自的**可观测后果** (决定了判据怎么写)
// ---------------------------------------------------------------------------
//
//	model            → 出站请求里的 model 字段
//	max-turns        → 模型每轮都发工具调用时的**请求条数** (到点就停)
//	permission-mode  → plan 档下 Write 被拒, 拒绝原因出现在下一轮的 tool_result 里
//	hooks            → PreToolUse 的 command hook 真的跑起来了 (一个文件被创建)
//	skills           → 技能清单出现在系统提示词里 (技能名是关键判据)
//
// 这五条里没有一条靠"配置对象上的某个字段等于某值" —— 那种断言在装配接线被整条
// 删掉时**依然全绿**, 因为字段是照着配置写的, 与有没有被消费无关。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/agentbind"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/types"
)

// p2Request 一次出站请求里本文件关心的部分。
type p2Request struct {
	Model  string
	System string
	// Body 原始请求体 —— 被拒的工具调用会以 tool_result 的形态出现在这里。
	Body string
}

// p2Server 起一个可编排响应的假 LLM 服务。
//
// reply 按**第几次请求**给响应 (从 0 起), 越界时沿用最后一次。这让"模型连续发工具
// 调用"这种多轮场景可以被精确编排, 而 max-turns 的判据正是"第几次之后就停了"。
func p2Server(t *testing.T, reply func(n int) string) (*httptest.Server, *[]p2Request, *int32) {
	t.Helper()
	var got []p2Request
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model  string      `json:"model"`
			System interface{} `json:"system"`
		}
		_ = json.Unmarshal(body, &req)
		c := p2Request{Model: req.Model, Body: string(body)}
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
		idx := int(atomic.AddInt32(&n, 1)) - 1
		writeSSEBlock(w, t, reply(idx))
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &n
}

// writeSSEBlock 写一条"纯文本 + end_turn"的最小 SSE。
func writeSSEBlock(w http.ResponseWriter, t *testing.T, text string) {
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
	send(types.StreamDelta{Type: "content_block_delta", Delta: &types.DeltaContent{Type: "text_delta", Text: text}})
	send(types.StreamDelta{Type: "content_block_stop"})
	send(types.StreamDelta{Type: "message_delta", Delta: &types.DeltaContent{StopReason: "end_turn"}, Usage: &types.Usage{OutputTokens: 7}})
	send(types.StreamDelta{Type: "message_stop"})
}

// writeSSEToolUse 写一条"请求调用某个工具"的 SSE (stop_reason=tool_use)。
func writeSSEToolUse(w http.ResponseWriter, t *testing.T, name, inputJSON string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(d types.StreamDelta) {
		b, _ := json.Marshal(d)
		w.Write([]byte("data: " + string(b) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(types.StreamDelta{Type: "message_start", Message: &types.APIResponse{Usage: &types.Usage{InputTokens: 12}}})
	send(types.StreamDelta{Type: "content_block_start", ContentBlock: &types.ContentBlock{Type: types.ContentBlockToolUse, ID: "tu-1", Name: name}})
	send(types.StreamDelta{Type: "content_block_delta", Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: inputJSON}})
	send(types.StreamDelta{Type: "content_block_stop"})
	send(types.StreamDelta{Type: "message_delta", Delta: &types.DeltaContent{StopReason: "tool_use"}, Usage: &types.Usage{OutputTokens: 7}})
	send(types.StreamDelta{Type: "message_stop"})
}

// p2Manager 造一个带技能池的 SessionManager。
//
// 技能池必须**真的注入** (不像其它用例传 nil): skill 绑定的判据是技能清单出现在
// 系统提示词里, 而 nil 池下组装器会把所有技能报成 unknown_name —— 那样测的就不是
// 接线而是"没池时会不会说假话"。
func p2Manager(t *testing.T, baseURL string, reg *skills.Registry) *SessionManager {
	t.Helper()
	ss := statestore.NewMemStore()
	sm := newTestSessionManagerWithLLM(t, ss, baseURL, nil)
	sm.skillReg = reg
	return sm
}

// p2SkillReg 造一个只含指定技能的内存技能池。
func p2SkillReg(t *testing.T, names ...string) *skills.Registry {
	t.Helper()
	reg := skills.NewRegistry()
	for _, n := range names {
		reg.Register(&skills.Skill{
			Name:        n,
			Description: "P2 测试技能 " + n,
			Body:        "技能正文 " + n,
			Status:      "active",
		})
	}
	return reg
}

// TestP2模型与回合上限都从声明来 一次验两条绑定。
//
// 判据分开写, 因为两者的可观测后果不同:
//   - model: 出站请求里的 model 字段 (子代理真的换了个模型);
//   - max-turns: 模型**每轮都发工具调用**时的请求条数 —— 到点就停。
//
// max-turns 这条必须用"一直发工具调用"的假服务才能验: 若模型自己就 end_turn 了,
// 请求数停在 1 是因为模型停了, 而不是因为上限 —— 那样断言恒真而没有分辨力。
func TestP2模型与回合上限都从声明来(t *testing.T) {
	const marker = "P2-MODEL-MARKER-7b21"
	srv, reqs, n := p2Server(t, func(i int) string { return "" })
	// 每次请求都发一个无害的只读工具调用, 逼引擎继续下一轮。
	srv.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model  string      `json:"model"`
			System interface{} `json:"system"`
		}
		_ = json.Unmarshal(body, &req)
		c := p2Request{Model: req.Model, Body: string(body)}
		if s, ok := req.System.(string); ok {
			c.System = s
		}
		*reqs = append(*reqs, c)
		atomic.AddInt32(n, 1)
		writeSSEToolUse(w, t, "Read", `{"file_path":"/nonexistent"}`)
	}))
	defer srv2.Close()

	sm := p2Manager(t, srv2.URL, nil)
	restore := installAgentAsset(t, sm.config.Cwd, "capped",
		"name: capped\nmodel: "+marker+"\nmax-turns: 2", "正文")
	defer restore()

	if _, err := sm.runNestedAgent(context.Background(), nil, "干活",
		agent.RunOptions{SubagentType: "capped"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}

	got := *reqs
	if len(got) == 0 {
		t.Fatal("假 LLM 服务没收到请求, 后面的断言全是空断言")
	}
	if got[0].Model != marker {
		t.Errorf("资产声明的 model 没进子代理的出站请求: got %q want %q", got[0].Model, marker)
	}
	// max-turns: 2 ⇒ 最多两次请求。模型每次都在发工具调用, 所以"停了"只能是上限
	// 起作用。给一个宽松的上界 (上限 +1) 是为了容忍最后一轮之后的一次收尾调用 ——
	// 但绝不能是"一直跑下去", 那样这条断言就没有分辨力。
	if len(got) > 3 {
		t.Errorf("资产声明 max-turns=2, 实际发了 %d 次请求 —— 上限声明没生效", len(got))
	}

	// 阳性对照: 同一个假服务, 换一份**没有** max-turns 的资产, 请求数必须更多。
	// 缺了它, 上面的上界无法区分"上限生效"与"这个假服务本来就跑不了几轮"。
	*reqs = nil
	atomic.StoreInt32(n, 0)
	restore2 := installAgentAsset(t, sm.config.Cwd, "uncapped",
		"name: uncapped\nmodel: "+marker, "正文")
	defer restore2()
	if _, err := sm.runNestedAgent(context.Background(), nil, "干活",
		agent.RunOptions{SubagentType: "uncapped"}); err != nil {
		t.Fatalf("对照派生失败: %v", err)
	}
	if len(*reqs) <= len(got) {
		t.Errorf("没有 max-turns 声明时请求数应当更多 (%d vs %d) —— "+
			"否则上面那条上界量到的不是上限, 而是这个假服务自己的行为", len(*reqs), len(got))
	}
}

// TestP2权限档只许收窄 验两件事, 且两件都要验。
//
// ① 更严的声明生效: 外层 default, 资产写 plan ⇒ Write 被**直接拒**。
// ② 更宽的声明被拒: 外层 default, 资产写 bypass ⇒ 不得变成 bypass。
//
// ②是本条的重点。资产目录是 state/project/user 三层都写得进去的地方 (evo 与面板
// 也会落盘), 若无条件生效, 一份 .md 里写 `permission-mode: bypass` 就是一条从文件
// 到权限的提升路径 —— 而且完全静默。
//
// ---------------------------------------------------------------------------
// 两条前提, 第一版都写错了, 记在这里免得下次再踩
// ---------------------------------------------------------------------------
//
// ① **资产必须同时声明 tool-profile: coding**。chat 档 (未声明档位时的回落) 根本
// 不注册 Write —— 于是"Write 被拒"这条断言在权限档完全没生效时也成立, 因为压根没
// 有这个工具 (第一版收到的是 `未知工具: Write`)。判据要量的是**权限**, 就先把工具
// 存在这件事钉死, 否则量到的是工具注册表。
//
// ② **外层档必须显式调成 default**。DefaultBotConfig 是 bypass, 而 bypass 是全序里
// 最宽的一档 ⇒ Widens(bypass, *) 恒为 false ⇒ "更宽的声明被拒"这条**永远无法触发**,
// 断言恒真。default vs plan/bypass 才有分辨力。
func TestP2权限档只许收窄(t *testing.T) {
	// 假服务每次请求都让模型调 Write。权限判定的后果以 tool_result 的形态出现在
	// **下一次**请求的 body 里 —— 这是运行期后果, 不是配置字段。
	var reqs []p2Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		reqs = append(reqs, p2Request{Model: req.Model, Body: string(body)})
		writeSSEToolUse(w, t, "Write", `{"file_path":"`+
			filepath.Join(t.TempDir(), "x.txt")+`","content":"hi"}`)
	}))
	defer srv.Close()

	sm := p2Manager(t, srv.URL, nil)
	sm.config.PermissionMode = string(types.PermissionModeDefault)

	// ① 更严的声明照收。
	restore := installAgentAsset(t, sm.config.Cwd, "narrowed",
		"name: narrowed\ntool-profile: coding\npermission-mode: plan", "正文")
	defer restore()

	if _, err := sm.runNestedAgent(context.Background(), nil, "写点东西",
		agent.RunOptions{SubagentType: "narrowed"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if len(reqs) < 2 {
		t.Fatalf("假服务只收到 %d 次请求 —— 拿不到被拒之后的第二次请求, 判据无从落地", len(reqs))
	}
	// 拒因的**实际文本**来自工具自己的 CheckPermissions (filewrite.go:68), 不是
	// permissions.modeDecision —— 两条路都通 plan, 但先判定的是这一条, 所以这里
	// 断言的是工具侧那句话。写死 modeDecision 的那句 "Plan 模式" 会得到一个
	// 明明通了却报红的用例 (我第一版就是这样)。
	if second := reqs[1].Body; !strings.Contains(second, "Plan mode") {
		t.Errorf("资产声明 permission-mode=plan, 但 Write 的拒绝原因里没有 Plan mode 的字样 —— "+
			"权限档声明没生效 (第二次请求体: %.400s)", second)
	}

	// ② 更宽的声明必须被拒 —— 外层 default 保持不变。
	//
	// 怎么分辨"被拒"与"生效了": 两者的拒绝原因逐字不同。default 下 Write 走 Ask,
	// 非交互子代理里翻成 `权限待确认: ...`; 若 bypass 真的生效, 这个调用会被放行,
	// tool_result 里出现的是**写入成功**而不是任何拒绝。所以判据是"第二条请求里
	// 有没有 `权限待确认` 这句 default 专属的话" —— 缺了它就说明外层档被换掉了。
	reqs = nil
	restore2 := installAgentAsset(t, sm.config.Cwd, "sneaky",
		"name: sneaky\ntool-profile: coding\npermission-mode: bypass", "正文")
	defer restore2()
	if _, err := sm.runNestedAgent(context.Background(), nil, "写点东西",
		agent.RunOptions{SubagentType: "sneaky"}); err != nil {
		t.Fatalf("对照派生失败: %v", err)
	}
	if len(reqs) < 2 {
		t.Fatalf("对照只收到 %d 次请求, 判据无从落地", len(reqs))
	}
	body := reqs[1].Body
	if strings.Contains(body, "Plan mode") {
		t.Error("对照 run 里出现了 Plan mode 的字样 —— 说明这一轮的档位不是外层那个 default, 本用例的前提被破坏了")
	}
	if !strings.Contains(body, "权限待确认") {
		t.Errorf("资产声明 permission-mode=bypass 改变了外层的 default —— 声明只能收窄权限面, "+
			"否则一份 .md 就是一条从文件到权限的静默提升路径 (第二次请求体: %.400s)", body)
	}
}

// TestP2资产hook真的被执行 是本文件里最"实"的一条判据。
//
// hook 声明最容易写成"装进 Runner 就算接上" —— 而 Runner 的配置数组里有它就等于
// 它会触发吗? 不一定 (事件名拼错、If 匹配器写歪、Runner 根本没被引擎用)。所以这里
// 让 hook 去**创建一个文件**, 断言那个文件存在: 要么进程真的被拉起来了, 要么没有。
func TestP2资产hook真的被执行(t *testing.T) {
	srv, reqs, _ := p2Server(t, func(int) string { return "好了" })
	sm := p2Manager(t, srv.URL, nil)

	marker := filepath.Join(t.TempDir(), "hook-fired.marker")
	restore := installAgentAsset(t, sm.config.Cwd, "hooked",
		"name: hooked\nhooks:\n  - SessionStart:command:touch "+marker, "正文")
	defer restore()

	if _, err := sm.runNestedAgent(context.Background(), nil, "干活",
		agent.RunOptions{SubagentType: "hooked"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到请求 —— 派生根本没跑起来, 后面的断言是空断言")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("资产声明的 SessionStart hook 没有执行 (标记文件 %s 不存在): %v\n"+
			"只把 hook 装进 Runner 而没有让它真的被引擎用, 在装配读数上与接好了完全一样", marker, err)
	}
}

// TestP2资产技能进清单 验 skill 绑定 —— 且必须挑**它真正独有的那个场景**来验。
//
// ---------------------------------------------------------------------------
// 为什么不能用"声明了的进清单、没声明的没进"当判据
// ---------------------------------------------------------------------------
//
// 那是我第一版写的判据, 它错了。本路径的基线清单是 shortSkillListingInDir, 用的是
// **全池** (只滤 shadow/archived), 而资产 append 走的 FormatShortListingForNames
// 同样是**全池视图、不看 paths** —— 两者对"这条技能在不在清单里"给同一个答案。
// 所以基线下**所有**技能本来就在清单里, "没声明的也出现了"是正常行为而不是缺陷;
// 拿它当反例会让用例恒红, 而拿"声明的出现了"当正例则恒真 (接线删掉也绿)。
//
// ---------------------------------------------------------------------------
// 真正的分辨点: paths 过滤
// ---------------------------------------------------------------------------
//
// 基线走的 FormatShortListingForDir 会按技能的 `paths:` 声明与当前目录过滤; 资产
// 声明走的 ForNames **不看 paths**。于是一条 paths 与工作目录不匹配的技能:
//
//	不声明它 → 清单里没有它 (被 paths 滤掉);
//	声明它   → 清单里有它   (显式声明压过目录推断)。
//
// 这一个方向差就是这条绑定的全部语义, 也是 P2「显式声明 > 目录推断」的落点。
// 用例自带阴性对照: 同一个技能、同一份池, 不声明的那一轮必须看不到它 —— 缺了这条,
// "声明后可见"无法区分"声明起作用"与"它本来就在清单里"。
func TestP2资产技能进清单(t *testing.T) {
	srv, reqs, _ := p2Server(t, func(int) string { return "好了" })
	reg := p2SkillReg(t, "p2-alpha")
	// 这条技能的 paths 与工作目录 (一个空的临时目录) 不匹配 ⇒ 基线清单里不会有它。
	reg.Register(&skills.Skill{
		Name:        "p2-gated",
		Description: "只在 rust 项目里可见",
		Status:      "active",
		Paths:       []string{"**/*.rs"},
	})
	sm := p2Manager(t, srv.URL, reg)

	// 阴性对照: 不声明 ⇒ 清单里没有 p2-gated (被 paths 滤掉)。
	restore0 := installAgentAsset(t, sm.config.Cwd, "plain", "name: plain", "正文")
	defer restore0()
	if _, err := sm.runNestedAgent(context.Background(), nil, "干活",
		agent.RunOptions{SubagentType: "plain"}); err != nil {
		t.Fatalf("对照派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到请求, 后面的断言全是空断言")
	}
	ctrl := (*reqs)[len(*reqs)-1].System
	if strings.Contains(ctrl, "p2-gated") {
		t.Fatalf("对照 run 的清单里就有 p2-gated —— 那说明基线本来就不做 paths 过滤, "+
			"本用例量不到 skill 绑定 (清单片段: %.400s)", ctrl)
	}
	// 阳性读数: 池里那条**无 paths** 的技能基线就该有 —— 缺了它, 上面那条
	// "对照里没有 p2-gated"无法区分"paths 过滤生效"与"这份清单根本没渲染"。
	if !strings.Contains(ctrl, "p2-alpha") {
		t.Fatalf("对照 run 的清单里连无 paths 的技能都没有 —— 清单没被渲染进请求, 判据无效")
	}

	// 主用例: 资产显式声明它 ⇒ 必须进清单。
	*reqs = nil
	restore := installAgentAsset(t, sm.config.Cwd, "skilled",
		"name: skilled\nskills: [p2-gated]", "正文")
	defer restore()
	if _, err := sm.runNestedAgent(context.Background(), nil, "干活",
		agent.RunOptions{SubagentType: "skilled"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到请求, 后面的断言是空断言")
	}
	if sys := (*reqs)[len(*reqs)-1].System; !strings.Contains(sys, "p2-gated") {
		t.Errorf("资产显式声明的技能没进清单 —— 而它的 paths 与工作目录不匹配, "+
			"所以只有这一条通路能让它出现 (清单片段: %.400s)", sys)
	}
}

// ============================================================================
// P2′ 工具名单: 从资产声明到出站请求 (21.8.12 P2′)
// ============================================================================
//
// 上面那五条绑定的判据模式在这里继续: 观测量必须是**运行期后果**, 不能是配置字段。
// 工具名单这一面的"运行期后果"有一个特别干净的形式 —— **出站请求体里的 tools 数组**。
// 模型只能调用它看得见的工具, 所以"这个工具名在不在请求里"就是这条声明生效与否的
// 直接读数, 中间不需要任何推断。
//
// ---------------------------------------------------------------------------
// 为什么这条必须端到端, 不能只靠 agentbind 的单测
// ---------------------------------------------------------------------------
//
// agentbind 的单测证明的是"读数对": DisallowedTools 里确实有 Bash。而改造前那个洞
// 恰恰是**读数全对、接线为零** —— `disallowed-tools: [Bash]` 解析得好好的, 也写进了
// Binding.Declared, 就是没有任何一行把它送进运行时。那种缺陷在单测里完全隐形。
//
// 所以这里断言的是请求体: 顺着**真实的派生入口**跑一遍, 看模型手上到底有没有那个工具。

// p2ToolNames 从一次出站请求体里取出 tools 数组的工具名。
//
// 直接解析请求体的 JSON 而不是断言字符串包含: `"Bash"` 这个子串会出现在系统提示词、
// 工具说明、甚至别的工具的 schema 描述里 —— 用包含判断会得到一个**恒真**的断言
// (阴性对照那条尤其会因此失去分辨力)。取的是数组里的 name 字段。
func p2ToolNames(t *testing.T, body string) map[string]bool {
	t.Helper()
	var req struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("请求体不是合法 JSON, 判据无从落地: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range req.Tools {
		names[tl.Name] = true
	}
	return names
}

// TestP2黑名单真的从出站请求里去掉工具 是 P2′ 的全链条判据。
//
// 正例与对照**差在一条声明上**, 其余逐字相同 —— 于是两者的差异只可能来自那一行。
// 这正是"同一条声明只能有一条送达路径"那条纪律的检验方式: 把接线删掉, 正例必须变红。
//
// 资产声明 tool-profile: coding 是必须的前提 (与 TestP2权限档只许收窄 那条同一个坑):
// chat 档根本不注册 Bash, 于是对照里也看不到它, 判据就恒真了。
func TestP2黑名单真的从出站请求里去掉工具(t *testing.T) {
	srv, reqs, _ := p2Server(t, func(int) string { return "好了" })
	sm := p2Manager(t, srv.URL, nil)

	// 对照: 只声明档位, 不声明黑名单 —— 此时模型**看得见** Bash。
	restoreBase := installAgentAsset(t, sm.config.Cwd, "base-coding",
		"name: base-coding\ntool-profile: coding", "正文")
	defer restoreBase()
	if _, err := sm.runNestedAgent(context.Background(), nil, "干活",
		agent.RunOptions{SubagentType: "base-coding"}); err != nil {
		t.Fatalf("对照派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到请求, 后面的断言全是空断言")
	}
	baseTools := p2ToolNames(t, (*reqs)[0].Body)
	if !baseTools["Bash"] {
		t.Fatalf("对照里没有 Bash —— coding 档本该注册它, 本用例的前提不成立 (工具名: %v)", baseTools)
	}

	// 正例: 同一份资产, 只多一行 `disallowed-tools: [Bash]`。
	*reqs = nil
	restore := installAgentAsset(t, sm.config.Cwd, "no-bash",
		"name: no-bash\ntool-profile: coding\ndisallowed-tools: [Bash]", "正文")
	defer restore()
	if _, err := sm.runNestedAgent(context.Background(), nil, "干活",
		agent.RunOptions{SubagentType: "no-bash"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到请求")
	}
	tools := p2ToolNames(t, (*reqs)[0].Body)

	if tools["Bash"] {
		t.Error("资产声明 disallowed-tools: [Bash], 而出站请求里仍然有 Bash —— " +
			"声明的效果是「报告里写着挡了, 实际一个没挡」(声明了但没人读)")
	}
	// 只挡一个不等于把工具面清空 —— 否则这条判据分不清"黑名单精确生效"与
	// "整个工具面被清空"。Read 是 coding 档的另一个工具, 它必须在。
	if !tools["Read"] {
		t.Errorf("黑名单应当只去掉被点名的工具, Read 不该受影响 (工具名: %v)", tools)
	}
	// 反向对照: 请求里确实有工具可谈, 不是空数组导致的假绿。
	if len(tools) < 2 {
		t.Fatalf("正例的工具面只剩 %d 个 —— 判据区分不出「精确挡住 Bash」与「清空了工具面」", len(tools))
	}
}

// TestP2显式空白名单拒绝一切 补的是**飞书这条路径独有**的一个覆盖洞。
//
// ---------------------------------------------------------------------------
// 这条用例是怎么被发现的: 变异测试拆掉两处"互为掩体"的补偿, 全体用例照绿
// ---------------------------------------------------------------------------
//
// `tools: []` 从 frontmatter 到运行时要过三道关, 而它们在 CLI 路径上是**冗余**的:
//
//	A. asset.MetaListSet 认出 `[]` 并回一个非 nil 的空切片     (pkg/asset)
//	B. bindTools 把去重后的空名单重新补成非 nil 空切片          (pkg/agentbind)
//	C. 调用点按 **HasTools** (键在不在) 判, 而不是按 nil 判     (两条路径各一处)
//
// CLI 路径上有 A/B/C 三道, 所以**任意拆掉 A 与 B 中的一道、甚至两道一起拆**, 命令行
// 那一侧的用例照样全绿 —— C 那道会用一个空 map 兜住 (它的 `== nil` 分支直接
// make 一个新 map)。这不是"测试写得不好", 是三条防线互为掩体的必然结果, 而它掩盖
// 的恰好是**飞书路径上真实存在的洞**: 那边用的是 `WithToolAllowList(names []string)`,
// 而它的契约是 **nil = 不设白名单**, 于是 A/B 一旦被拆掉, 传进去的 nil 会让这条路
// 静默退化成"完全不限制" —— 一份 fail-closed 声明变成一次静默的权限放宽。
//
// 也就是说: 上面那条变异 (拆 A+B) 之所以全绿, 唯一原因是**没有人测飞书侧的
// `tools: []`**。本用例就是那条缺失的判据, 它测的正是"飞书这一侧自己有没有把关"。
//
// 实测记录 (写下来, 免得有人把下面的 C 当成"测过了"):
//
//	拆 B 单独        → 本用例 RED  (判据接住了)
//	拆 A+B 一起      → 本用例 RED  (两个掩体一起拆也接得住)
//	只把 C 换成 nil 判据 → 本用例**仍然 GREEN**
//
// 第三条不是漏洞, 是**如实记录的一处冗余**: B 保证了"显式空名单出自 bindTools 时
// 必为非 nil", 于是 C 在飞书这一侧今天无法与 nil 判据区分开。保留 C 的理由不是
// "它被测到了", 而是两条路径对同一份契约的读法必须一致 —— CLI 那一侧的 C 是**真
// 带电的** (把它换成长度判据, TestCLI显式空白名单拒绝一切 立刻变红), 一边用
// HasTools 一边用 nil 判断, 就是同一个契约有两个读者、两种读法。
// 代价说清楚: 飞书侧这一行, 今天的测试覆盖不到。
func TestP2显式空白名单拒绝一切(t *testing.T) {
	srv, reqs, _ := p2Server(t, func(int) string { return "好了" })
	sm := p2Manager(t, srv.URL, nil)

	// 阴性对照: 同档位、同正文, **只差** tools: [] 那一行 —— 此时模型看得见一堆工具。
	restoreBase := installAgentAsset(t, sm.config.Cwd, "p2-baseline",
		"name: p2-baseline\ntool-profile: coding", "正文")
	defer restoreBase()
	if _, err := sm.runNestedAgent(context.Background(), nil, "干活",
		agent.RunOptions{SubagentType: "p2-baseline"}); err != nil {
		t.Fatalf("对照派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到请求, 后面的断言全是空断言")
	}
	baseTools := p2ToolNames(t, (*reqs)[0].Body)
	if len(baseTools) < 2 {
		t.Fatalf("对照的工具面只有 %d 个 —— 判据区分不出「白名单拒绝一切」与「这个档位本来就没有工具」(%v)",
			len(baseTools), baseTools)
	}

	// 正例: 显式空白名单 ⇒ 一个工具都不给。
	*reqs = nil
	restore := installAgentAsset(t, sm.config.Cwd, "p2-nothing",
		"name: p2-nothing\ntool-profile: coding\ntools: []", "正文")
	defer restore()
	if _, err := sm.runNestedAgent(context.Background(), nil, "干活",
		agent.RunOptions{SubagentType: "p2-nothing"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到请求")
	}
	tools := p2ToolNames(t, (*reqs)[0].Body)
	if len(tools) != 0 {
		t.Errorf("`tools: []` 是显式空白名单 (fail-closed, 拒绝一切), 与「没写 tools:」"+
			"必须表现不同 —— 实际仍给了 %d 个工具 (%v)。飞书这条路用的是 "+
			"WithToolAllowList(nil 判据), 所以一旦上游把空名单丢了非 nil-ness, "+
			"这里就会静默退化成完全放行", len(tools), tools)
	}
}

// TestP2黑名单大小写写错会说话 是上一条的**分辨力**边界的如实记录。
//
// 写错大小写的黑名单**照样会被原样接线** (那是刻意的: 本仓不替调用方改写声明),
// 所以它的运行期后果是"什么都没挡住" —— 与不写这条声明逐字相同。这类失败在功能
// 上**不可观测**, 唯一能看见它的地方是装配读数 (BindingInvalidValue)。
//
// 也就是说: 端到端判据在这里量不到东西, 而这不是判据的缺陷, 是这个失败模式的形态。
// 写下来, 免得以后有人以为"接线的端到端测试能覆盖拼写错误"。
func TestP2黑名单大小写写错会说话(t *testing.T) {
	srv, reqs, _ := p2Server(t, func(int) string { return "好了" })
	sm := p2Manager(t, srv.URL, nil)

	restore := installAgentAsset(t, sm.config.Cwd, "bad-case",
		"name: bad-case\ntool-profile: coding\ndisallowed-tools: [bash]", "正文")
	defer restore()
	if _, err := sm.runNestedAgent(context.Background(), nil, "干活",
		agent.RunOptions{SubagentType: "bad-case"}); err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("假 LLM 服务没收到请求")
	}
	tools := p2ToolNames(t, (*reqs)[0].Body)

	// 如实记录这个后果: 小写的 bash 挡不住 Bash。这条断言**不是**在要求它挡住 ——
	// 它是在钉住"这个失败模式长什么样", 以及在有人将来"顺手"把验名结果也用来
	// 过滤名单时立刻变红 (那样会改变生效行为, 而本仓的决定是不改)。
	if !tools["Bash"] {
		t.Error("小写 bash 挡不住 Bash 是本项记录在案的后果; 这条变了说明接线改了行为, " +
			"请重新评估「认不出的名字照原样接线」这个决定")
	}
	// 而它**必须**在装配读数里被说出来 —— 这是唯一能看见它的地方。
	b := agentbind.BindAgentAsset("bad-case", nil)
	for _, bd := range b.Bindings {
		if bd.Facet == "tools" && bd.Status == agentbind.BindingInvalidValue {
			return
		}
	}
	t.Error("拼错的黑名单在功能上不可观测, 装配读数必须留痕 (BindingInvalidValue) —— " +
		"否则它是一处完全静默的配置错误")
}
