// runner_state_test.go —— H3-2 隔离运行中间态的验收。
//
// 方案 §21 H3-2 的验收原文是:
//
//	**「杀死子代理进程后重启, 从落盘状态继续而非从零; 产物与杀进程前一致。」**
//
// # 判据落在磁盘与请求体上, 不落在装配态
//
// "RunIsolated 里有一句 persistRunState" 是装配态, 一击就假绿 —— 那句可以写在
// 一个永远跑不到的分支里, 也可以写在一个把空切片存下去的调用里, 两种情况下这条
// 验收都过, 而生产上进程一死照样归零。所以本文件的每条主臂都读**两个真实读数**:
//
//   - 磁盘上的状态文件 (真由生产的 persistRunState 写出来的, 不是手搓的 JSON);
//   - 假上游收到的**请求体** —— 恢复是否真的发生, 只有"第二次运行的首个请求里
//     带着第一次运行的历史"这一条能证明。
//
// # "杀进程"怎么在这里复现
//
// 进程被杀 = 循环里的 persistRunState 已经落过盘, 而末尾那个 defer 没跑。在测试
// 进程里没法真的自杀 (os.Exit 会带走整个测试二进制), 所以这里用**生产的写入口**
// 复现那一瞬间的磁盘内容: e.newRunState + e.runStateFrom + e.persistRunState, 与
// 循环里那三行逐字相同。这不是"手搓一个状态文件"—— 字节由被测代码生成, 测试只
// 决定它在第几轮被生成。
//
// # 每条"恢复成功"都配一条"不该恢复时必须不恢复"
//
// 恢复这个能力的失败模式是不对称的: 少恢复一次只是重跑, 而**多恢复一次会把一段
// 与当前任务无关的历史交给模型**, 且它看到的 prompt 完全正常 —— 事后无从归因。
// 所以 Finished / 身份不符 / 文件损坏 三条都单独成臂, 断言"第二个 run 的第一个
// 请求里没有第一个 run 的东西"。
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// 测试脚手架
// ============================================================================

// seenRequest 假上游收到的一次请求 (只留断言用得上的几样)。
type seenRequest struct {
	Messages []types.APIMessage
	Flat     string // 所有 text / tool_result 内容的拼接, 供"历史有没有带过来"的判据
	// ToolNames 请求体里带的工具名。从原始 JSON 读, 不从我们的 APITool 结构推 ——
	// "没带工具"是 H3-3 交接请求的一条判据, 而它必须落在**实际发出去的字节**上。
	ToolNames []string
	// Handoff 这次请求是交接请求 (按 prompt 里的标记认)。**只用于选回应**,
	// 不作判据 —— 判据是 ToolNames 与请求内容本身。
	Handoff bool
}

// stateUpstreamRun 一轮循环需要的假上游。
type stateUpstreamRun struct {
	srv *httptest.Server

	mu   sync.Mutex
	seen []seenRequest

	// toolTurns 前几轮回 tool_use 驱动工具循环; 到了就转成纯文本收尾。
	// 0 = 一次工具都不要 (单轮收尾); 负数 = **永远**回 tool_use, 只能靠 MaxTurns 结束。
	// 这两种"零"必须分开: 0 与"永不收尾"在 `turn < toolTurns` 这一个判据里同形,
	// 而它们驱动的用例完全不同 (前者造终态, 后者造断点)。
	toolTurns int

	// onRequest 每次请求前回调 (在回应之前), 用于在"杀进程的那一瞬间"抓现场。
	// 回调在发送响应前调用, 此刻循环正阻塞在 SendMessage 里, 状态文件不会被并发写。
	onRequest func(turn int, req seenRequest)

	// text 收尾轮 (turn >= toolTurns) 的回应文本。
	text string
	// handoffText 交接请求的回应文本 (H3-3)。走独立分支回应, 不占用 toolTurns 的计数。
	handoffText string
	// toolInput 回给 noop_tool 的 input (默认 "{}")。H3-4 的覆盖臂用它把"这次要写哪个
	// 文件"送进工具 —— 那个路径必须是**两个 run 共享的同一个**, 否则用例退化成
	// "两个写者写不同文件", 在任何实现下都绿。
	toolInput string
}

// handoffMarker 只出现在交接请求的 prompt 里 (见 runner_handoff.go 的 handoffPrompt)。
// 测试用它把交接请求从业务回合里挑出来 —— **仅用于选回应**, 不作判据。
const handoffMarker = "[harness] 本次执行因为"

func newStateUpstream(t *testing.T, toolTurns int, text string) *stateUpstreamRun {
	t.Helper()
	u := &stateUpstreamRun{toolTurns: toolTurns, text: text, handoffText: "交接内容"}
	u.start(t)
	t.Cleanup(u.srv.Close)
	return u
}

// start 把假上游起出来。抽出来是因为 H3-3 的预算臂要自己控 toolTurns (预算可能在
// 第 N 轮就耗尽, 而端点还不能收尾), 复制一份响应逻辑就会有两套 toolTurns 语义,
// 迟早分叉成"两条臂验的不是同一件事"。
func (u *stateUpstreamRun) start(t *testing.T) {
	t.Helper()
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role     string            `json:"role"`
				Content  json.RawMessage   `json:"content"`
				Content0 []json.RawMessage `json:"-"`
			} `json:"messages"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		var flat strings.Builder
		msgs := make([]types.APIMessage, 0, len(req.Messages))
		for _, m := range req.Messages {
			am := types.APIMessage{Role: m.Role}
			var blocks []struct {
				Type  string `json:"type"`
				Text  string `json:"text"`
				Think string `json:"thinking"`
				// tool_result 的内容可能是字符串, 也可能是块数组 —— 两种都要能读。
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(m.Content, &blocks) == nil {
				for _, b := range blocks {
					flat.WriteString(b.Text)
					flat.WriteString(b.Think)
					if len(b.Content) > 0 {
						flat.Write(b.Content)
					}
				}
			}
			msgs = append(msgs, am)
		}

		names := make([]string, 0, len(req.Tools))
		for _, t := range req.Tools {
			names = append(names, t.Name)
		}
		seen := seenRequest{
			Messages: msgs, Flat: flat.String(), ToolNames: names,
			Handoff: strings.Contains(flat.String(), handoffMarker),
		}

		u.mu.Lock()
		turn := len(u.seen)
		u.seen = append(u.seen, seen)
		u.mu.Unlock()

		if u.onRequest != nil {
			u.onRequest(turn, seen)
		}

		var resp types.APIResponse
		if seen.Handoff {
			// H3-3 的交接请求: 按 u.handoff 的配置回应。
			//
			// 这条分支是必需的 —— 没有它, 交接请求会落进下面按 turn 计数的业务分支,
			// 而出现在**第 toolTurns 轮**的那次交接会拿到收尾文本 (于是 run 提前结束,
			// 用例以为测的是"跑到上限后的交接", 实际测的是一个正常收尾)。
			// 那样两条臂都会绿, 而生产上"跑到上限"这条路径一次都没被验过。
			resp = types.APIResponse{
				ID: "msg_handoff", Type: "message", Role: "assistant", Model: req.Model,
				Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: u.handoffText}},
				StopReason: "end_turn",
				Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
			}
		} else if u.toolTurns < 0 || turn < u.toolTurns {
			id := "tu" + itoa(turn)
			resp = types.APIResponse{
				ID: "msg_" + id, Type: "message", Role: "assistant", Model: req.Model,
				Content: []types.ContentBlock{
					{Type: types.ContentBlockText, Text: "第" + itoa(turn) + "轮思考"},
					{Type: types.ContentBlockToolUse, ID: id, Name: "noop_tool", Input: json.RawMessage(u.toolInputOf())},
				},
				StopReason: "tool_use",
				Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
			}
		} else {
			resp = types.APIResponse{
				ID: "msg_done", Type: "message", Role: "assistant", Model: req.Model,
				Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: u.text}},
				StopReason: "end_turn",
				Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// toolInputOf 回给工具的 input (未指定时是 "{}", 与改造前逐字相同)。
func (u *stateUpstreamRun) toolInputOf() string {
	if u.toolInput == "" {
		return "{}"
	}
	return u.toolInput
}

func (u *stateUpstreamRun) requests() []seenRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]seenRequest(nil), u.seen...)
}

func (u *stateUpstreamRun) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.seen)
}

// 注: 小整数转字符串用的是本包已有的 itoa (trace_llm.go:213), 不另起一份。

// stateIdentity 一份稳定的运行身份 (五个字段都不是运行时生成的)。
func stateIdentity() (RunIdentity, *Config) {
	id := RunIdentity{Team: "team-a", Workflow: "wf-1", Stage: "implement", Role: "coder", Cwd: "/tmp/ws"}
	return id, &Config{
		Model:            "fake-model",
		Team:             id.Team,
		Workflow:         id.Workflow,
		Role:             id.Role,
		Cwd:              id.Cwd,
		PermissionMode:   types.PermissionModeAcceptEdits,
		IsNonInteractive: true,
	}
}

// newStateEngine 造一个接了假上游、注册了 noop_tool、装配了中间态目录的引擎。
func newStateEngine(t *testing.T, u *stateUpstreamRun, cfg *Config, runStateDir string) *QueryEngine {
	t.Helper()
	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{
		BaseURL: u.srv.URL, APIKey: "k", Model: cfg.Model,
		Client: u.srv.Client(), RetryCount: 0,
	}
	e := NewQueryEngine(cfg, client, reg,
		hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, nil)
	e.SetRunStateDir(runStateDir)
	return e
}

// isoOpts 造隔离运行参数, **把身份里的 Stage 一并填进去**。
//
// 为什么要有这个帮手而不是让每个用例各写一遍: Stage 只从 opts 进来, 而它又是身份的
// 一项 —— 测试里漏填一次, 用例算出来的键就和引擎实际用的键不同, 断言会以"文件不存在"
// 的形式失败, 看起来像被测代码的毛病。(我写这文件时正是这么栽了一次。)
// 收口成一个帮手之后, "身份"在测试里只有一处定义。
func isoOpts(id RunIdentity, maxTurns int) IsolatedRunOptions {
	return IsolatedRunOptions{MaxTurns: maxTurns, Stage: id.Stage}
}

// stateFilePath 被测的状态文件路径 (由生产代码算出来, 不是测试拼的)。
func stateFilePath(t *testing.T, e *QueryEngine, id RunIdentity) string {
	t.Helper()
	p := e.runStatePath(RunKey(id))
	if p == "" {
		t.Fatal("状态文件路径为空 —— runStateDir 没装配上")
	}
	return p
}

// handoffFilePath 被测的交接说明路径 (由生产代码算出来, 不是测试拼的)。
func handoffFilePath(t *testing.T, e *QueryEngine, id RunIdentity) string {
	t.Helper()
	p := e.handoffPath(RunKey(id))
	if p == "" {
		t.Fatal("交接说明路径为空 —— runStateDir 没装配上")
	}
	return p
}

// readHandoffText 从磁盘读回交接说明原文字节 (不存在时 fatal)。
func readHandoffText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("交接说明不存在/读不动 (%s): %v", path, err)
	}
	return string(b)
}

// readState 从磁盘读回状态文件。
func readState(t *testing.T, path string) *RunState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("状态文件不存在/读不动 (%s): %v", path, err)
	}
	var st RunState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("状态文件不是合法 JSON: %v", err)
	}
	return &st
}

// ============================================================================
// 核心: 方案验收原文
// ============================================================================

// TestH32_ResumeContinuesInsteadOfRestarting 杀进程后重启, 从落盘状态继续。
//
// 主臂分三段:
//  1. 跑一个 run, 在第 2 轮请求时把循环**刚写下的**状态文件抓了下来 (等价于
//     "此刻 kill -9", 那一刻 defer 还没机会跑);
//  2. 用生产的写入口把那份中间态摆回磁盘 (Finished=false, 与被杀留下的逐字节同形);
//  3. 新起一次同身份的 run, 断言它**首个**请求里就带着第 1 段跑出来的历史 ——
//     以及那段恢复说明。没有后半句, 模型会以为任务本来就只有这些。
func TestH32_ResumeContinuesInsteadOfRestarting(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	// ---- 第 1 段: 跑到中途, 抓下"被杀死那一刻"的现场 ----
	var killed []byte
	u1 := newStateUpstream(t, -1, "unused") // 永远回 tool_use, 只能靠 MaxTurns 收尾
	u1.onRequest = func(turn int, _ seenRequest) {
		if turn != 1 || killed != nil {
			return
		}
		// turn==1 ⇒ 第 1 轮的 persistRunState 已经跑过了。
		b, err := os.ReadFile(filepath.Join(dir, "run-state", RunKey(id)+".json"))
		if err == nil {
			killed = b
		}
	}
	e1 := newStateEngine(t, u1, cfg, dir)
	_, err := e1.RunIsolated(context.Background(), "实现一个登录接口", isoOpts(id, 3))
	if err == nil {
		t.Fatal("MaxTurns=3 且上游恒回 tool_use, 不该正常结束")
	}
	if killed == nil {
		t.Fatal("循环跑了两轮却没有中途落盘 —— 中间态没通电 (这正是本任务要修的缺口)")
	}
	if st := json.Unmarshal(killed, &RunState{}); st != nil {
		t.Fatalf("中途落盘的内容不是合法 RunState: %v", st)
	}

	// ---- 第 2 段: 摆回"被杀现场" ----
	// 用生产的写入口生成 (与循环里那三行逐字相同), 而不是手搓 JSON。
	killedState := &RunState{}
	if err := json.Unmarshal(killed, killedState); err != nil {
		t.Fatal(err)
	}
	if killedState.Finished {
		t.Fatalf("此刻不该是终态 (被杀时 defer 没跑): %+v", killedState)
	}
	e2 := newStateEngine(t, newStateUpstream(t, -1, "unused"), cfg, dir)
	mid := e2.newRunState(RunKey(id), id, "实现一个登录接口", 3)
	e2.runStateFrom(mid, killedState.Messages, killedState.LastText, killedState.Turn, killedState.Model)
	e2.persistRunState(mid)
	midPath := stateFilePath(t, e2, id)
	if got := readState(t, midPath); got.Turn != killedState.Turn {
		t.Fatalf("摆回去的中间态轮数不对: got %d want %d", got.Turn, killedState.Turn)
	}

	// ---- 第 3 段: 重启, 必须从断点继续 ----
	u3 := newStateUpstream(t, -1, "实现好了")
	e3 := newStateEngine(t, u3, cfg, dir)
	if _, err := e3.RunIsolated(context.Background(), "实现一个登录接口", isoOpts(id, 3)); err == nil {
		t.Fatal("期望 MaxTurns 收尾")
	}
	reqs := u3.requests()
	if len(reqs) == 0 {
		t.Fatal("恢复后的 run 一次请求都没发出")
	}
	first := reqs[0]

	// 判据一: 上一段**全部**的历史内容都要在首个请求里 —— 逐条比, 不数条数。
	//
	// 为什么不能数条数: messagesToAPI 会把同角色的相邻消息合并成一个 content 数组
	// (恢复历史末尾的 tool_result 与恢复说明、本次输入都是 user 角色, 三条并成一条),
	// 于是"3 条 vs 3 条"既可能是恢复了也可能是没恢复 —— 一个数不出差别的判据。
	// 内容级比对没有这个毛病: 缺哪一条就是没恢复哪一条。
	for i, m := range killedState.Messages {
		for _, b := range m.Content {
			if b.Text == "" {
				continue
			}
			if !strings.Contains(first.Flat, b.Text) {
				t.Fatalf("落盘历史的第 %d 条消息 (内容片段 %q) 没有出现在恢复后的首个请求里 —— 没有恢复",
					i, truncateForTest(b.Text, 80))
			}
		}
	}
	// 判据二: 上一段模型说过的话原样带着 (逐字, 不是"结构上像")。
	if !strings.Contains(first.Flat, "第0轮思考") {
		t.Fatalf("恢复后的请求里找不到上一段的历史内容。请求全文片段:\n%s", truncateForTest(first.Flat, 600))
	}
	// 判据三: 恢复说明必须在 —— 少了它, 模型会把这段没有起点的历史当成任务全貌。
	if !strings.Contains(first.Flat, "[harness]") || !strings.Contains(first.Flat, "恢复") {
		t.Fatalf("恢复后的请求里没有恢复说明, 模型无从知道自己接的是半截历史。全文片段:\n%s",
			truncateForTest(first.Flat, 600))
	}
	// 判据四: 落盘里累计的恢复次数必须真的记上了。
	if got := readState(t, midPath).Resumes; got < 1 {
		t.Fatalf("状态里的 Resumes 没有累加, got %d", got)
	}
}

// TestH32_KillSnapshotIsProtocolValid 被杀死那一刻落盘的历史必须是能直接发出去的。
//
// 这条挡的是"在 tool_use 落进 messages 之后、tool_result 之前落盘": 那种历史里
// 最后一个 assistant 带着 tool_use 而没有配对的 tool_result, 恢复后第一次请求
// 必然被上游 400 拒 —— 也就是"恢复了, 但每次恢复都跑不起来"。
func TestH32_KillSnapshotIsProtocolValid(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	var snapped *RunState
	u := newStateUpstream(t, -1, "unused")
	u.onRequest = func(turn int, _ seenRequest) {
		if snapped != nil {
			return
		}
		b, err := os.ReadFile(filepath.Join(dir, "run-state", RunKey(id)+".json"))
		if err != nil {
			return
		}
		st := &RunState{}
		if json.Unmarshal(b, st) == nil && len(st.Messages) > 0 {
			snapped = st
		}
	}
	e := newStateEngine(t, u, cfg, dir)
	if _, err := e.RunIsolated(context.Background(), "随便做点什么", isoOpts(id, 3)); err == nil {
		t.Fatal("期望 MaxTurns 收尾")
	}
	if snapped == nil || len(snapped.Messages) == 0 {
		t.Fatal("没抓到中间态")
	}

	// 每个 tool_use 都要有配对的 tool_result。
	//
	// 这个判据必须**两趟**: 第一趟收齐所有被回答的 tool_use, 第二趟才检查。一趟扫
	// 下来一边收一边查是错的 —— 扫到 assistant(tool_use) 时它的 tool_result 还在
	// 后面那条消息里, 于是每条合法的历史都会被判成"悬空"。(我第一版正是这么写的,
	// 它把一条正确的实现报成了 400 缺陷。)
	answered := map[string]bool{}
	for _, m := range snapped.Messages {
		if m.Type != types.MessageTypeUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == types.ContentBlockToolResult {
				answered[b.ToolUseID] = true
			}
		}
	}
	for i, m := range snapped.Messages {
		if m.Type != types.MessageTypeAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == types.ContentBlockToolUse && !answered[b.ID] {
				t.Fatalf("落盘的历史第 %d 条里有悬空 tool_use %q —— 恢复后第一次请求必然 400", i, b.ID)
			}
		}
	}
}

// TestH32_FinishedRunIsNotResumed 跑完的 run 再次执行时是**新一轮**, 不是续跑。
//
// 同一个 stage 被重新执行 (团队重试、用户重跑) 时, 上一轮已经交付过的整份历史如果
// 接上去, 模型会看见一份"我已经做完的草稿" —— 它会以为自己已经做完了。这条把
// Finished 与"崩在半路"分开: 前者丢弃, 后者恢复。
func TestH32_FinishedRunIsNotResumed(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	// 第 1 次: 正常跑完 —— 且必须**带着若干轮工具往返**跑完。
	//
	// 为什么不能让它一轮就收尾: 那种终态落盘时 messages 只有一条 prompt (循环里
	// 的 persistRunState 还没轮到, 是 defer 里那句 finishRunState 写的), 于是下面
	// 的断言会被"空消息不算可恢复"那条守卫先接住 —— 守卫本身是对的, 但它让这条
	// 用例对 Finished 判据**完全没有分辨力**: 我把 Finished 守卫改成恒假, 这条
	// 用例照样全绿 (变异测试抓到的)。多跑两轮工具, 终态里就有真历史了。
	u1 := newStateUpstream(t, 2, "第一轮做完了")
	e1 := newStateEngine(t, u1, cfg, dir)
	out, err := e1.RunIsolated(context.Background(), "第一次任务", isoOpts(id, 3))
	if err != nil {
		t.Fatalf("第一次运行失败: %v", err)
	}
	if !strings.Contains(out, "第一轮做完了") {
		t.Fatalf("第一次产出不对: %q", out)
	}
	st := readState(t, stateFilePath(t, e1, id))
	if !st.Finished {
		t.Fatal("跑完了却没标 Finished —— 那与'崩在半路'在磁盘上就分不开了")
	}
	// 终态里必须真有历史, 否则这条用例验不到 Finished 判据 (见上方注释)。
	if len(st.Messages) < 3 {
		t.Fatalf("终态里只有 %d 条消息, 这条用例对 Finished 判据没有分辨力 (见上方注释)", len(st.Messages))
	}

	// 第 2 次: 同身份再跑, 首个请求里**不能**有第一次的东西。
	u2 := newStateUpstream(t, 0, "第二次做完了")
	e2 := newStateEngine(t, u2, cfg, dir)
	_, err = e2.RunIsolated(context.Background(), "第二次任务", isoOpts(id, 3))
	if err != nil {
		t.Fatalf("第二次运行失败: %v", err)
	}
	reqs := u2.requests()
	if len(reqs) != 1 {
		t.Fatalf("第二次运行应恰好 1 次请求, got %d", len(reqs))
	}
	if strings.Contains(reqs[0].Flat, "第一次任务") || strings.Contains(reqs[0].Flat, "第一轮做完了") {
		t.Fatalf("已完成的 run 被当成断点接上了 —— 模型会以为自己已经交付过。首个请求:\n%s",
			truncateForTest(reqs[0].Flat, 600))
	}
	if !strings.Contains(reqs[0].Flat, "第二次任务") {
		t.Fatalf("第二次运行的首个请求里没有本次任务: %s", truncateForTest(reqs[0].Flat, 300))
	}
}

// TestH32_IdentityMismatchStartsFresh 身份对不上时丢掉磁盘态。
//
// 这是"键碰撞"的安全出口。磁盘上的态是 A 运行的, 而这次是 B 运行 —— 直接接上会
// 把 A 的历史交给 B 的模型, 而 B 的 prompt 看上去完全正常。丢磁盘态的代价只是
// 重跑, 方向是安全的那一个。
func TestH32_IdentityMismatchStartsFresh(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()
	key := RunKey(id)

	// 造一份"键是 B 的、身份是 A 的"状态: 正是碰撞会留下的形状。
	other := id
	other.Stage = "review" // A/B 只差 stage, 其余四项完全相同
	stolen := RunState{
		Version: runStateVersion, Key: key,
		Team: other.Team, Workflow: other.Workflow, Stage: other.Stage,
		Role: other.Role, Cwd: other.Cwd,
		Objective: "别人的任务", Turn: 7,
		Messages: []types.Message{{
			Type: types.MessageTypeUser, UUID: "stolen-1",
			Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: "别人任务的历史内容"}},
			CreatedAt: time.Now(),
		}},
	}
	p := filepath.Join(dir, "run-state", key+".json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stolen)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	u := newStateUpstream(t, 0, "做完了")
	e := newStateEngine(t, u, cfg, dir)
	if _, err := e.RunIsolated(context.Background(), "我的任务", isoOpts(id, 3)); err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	reqs := u.requests()
	if len(reqs) != 1 {
		t.Fatalf("应恰好 1 次请求, got %d", len(reqs))
	}
	if strings.Contains(reqs[0].Flat, "别人任务的历史内容") {
		t.Fatalf("身份不符的磁盘态被接上了 —— 模型拿到了一段与当前任务无关的历史:\n%s",
			truncateForTest(reqs[0].Flat, 600))
	}
}

// TestH32_CorruptStateStartsFresh 状态文件损坏时按全新运行处理, 且不把 run 打死。
//
// 中间态是**缓存**不是账本: 写坏一次 (写一半被 kill / 磁盘错误) 的代价必须是
// "重跑", 而不是"这条路径从此再也跑不起来"。所以这里连错误都不该冒出来。
func TestH32_CorruptStateStartsFresh(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()
	p := filepath.Join(dir, "run-state", RunKey(id)+".json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"version":1,"messages":[{"type":"user"`), 0o600); err != nil {
		t.Fatal(err)
	}

	u := newStateUpstream(t, 0, "做完了")
	e := newStateEngine(t, u, cfg, dir)
	out, err := e.RunIsolated(context.Background(), "任务", isoOpts(id, 3))
	if err != nil {
		t.Fatalf("状态文件损坏把 run 打死了 (应当按全新运行处理): %v", err)
	}
	if !strings.Contains(out, "做完了") {
		t.Fatalf("产出不对: %q", out)
	}
	if n := len(u.requests()); n != 1 {
		t.Fatalf("应恰好 1 次请求 (全新运行), got %d", n)
	}
}

// TestH32_NoAssemblyMeansNoFiles 没装配中间态目录时, 磁盘上一个字节都不该多。
//
// 这是改造的**对照臂**: 这条能力是旁路, 没接上时必须与改造前逐字节相同。缺了它,
// 一个"无论装配与否都往外写文件"的实现能让上面所有恢复用例照样全绿 —— 而它在
// 生产上会给每个工作区留下一堆没被任何开关允许写下的文件。
func TestH32_NoAssemblyMeansNoFiles(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	u := newStateUpstream(t, 1, "做完了")
	e := newStateEngine(t, u, cfg, "") // 不装配
	// 判据一 (直接读开关): 未装配时连路径都算不出来。
	//
	// 这一条不是多余的: 先前的版本只走"目录里有没有多出文件", 而那只在**写进了
	// 参数里那个目录**时才看得见。我把 runStatePath 的空目录守卫改成"空则落到
	// 别处", 文件写到别处去了, 而临时目录里依然一个文件都没有 —— 用例全绿。
	// 开关本身有没有通电, 只有直接读它才判得准。
	if p := e.runStatePath(RunKey(id)); p != "" {
		t.Fatalf("没装配中间态目录却算出了路径 %q —— 关不掉的开关等于没有开关", p)
	}
	if _, err := e.RunIsolated(context.Background(), "任务", isoOpts(id, 3)); err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	// 判据二: 目录里有没有多出东西。
	var found []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("没装配中间态目录却写下了文件: %v", found)
	}
	// 对照: 装配了就一定会写。两条合起来才说明"装配"是真的开关, 而不是装饰 ——
	// 只断言前半条的话, 一个**从不落盘**的实现也能全绿。
	u2 := newStateUpstream(t, 1, "做完了")
	e2 := newStateEngine(t, u2, cfg, dir)
	if _, err := e2.RunIsolated(context.Background(), "任务", isoOpts(id, 3)); err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	st := readState(t, stateFilePath(t, e2, id)) // 路径由生产代码算出, 断言"确实写了"
	if !st.Finished {
		t.Fatalf("跑完了的 run 落了盘但不是终态: %+v", st)
	}
}

// ============================================================================
// 折叠豁免的解除 (H3-2 后半句)
// ============================================================================

// TestH32_FoldFiresInsideIsolatedRun 隔离运行里真的会折叠, 而不是一路涨到 PTL。
//
// 改造前的注释写着"假设 stage 输出量适中, 不应用 autoCompact / microCompact" ——
// 而这条路径上常态是 150 轮。判据落在**请求体**上: 通读整轮对话, 前面几轮请求
// 里应当出现过折叠产物 (<context_summary>), 且出现之后请求不再无限变大。
func TestH32_FoldFiresInsideIsolatedRun(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	// 摘要上游: 与业务上游分开, 这样断言"折叠真的发生了"不会与业务回合混淆。
	var summaryCalls int32
	var mu sync.Mutex
	sumSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		summaryCalls++
		mu.Unlock()
		resp := types.APIResponse{
			ID: "sum", Type: "message", Role: "assistant", Model: "fake-model",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "这是被折叠掉的历史摘要"}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(sumSrv.Close)

	// 上游恒回 tool_use, 每轮结果很长 —— 很快越过 0.8 阈值。
	u := newStateUpstream(t, -1, "unused")
	reg := tool.NewRegistry()
	reg.Register(longNoopTool{})
	client := &api.Client{BaseURL: u.srv.URL, APIKey: "k", Model: cfg.Model, Client: u.srv.Client(), RetryCount: 0}
	// contextWindow 取 4000 ⇒ 阈值 3200 tokens ≈ 12800 字符。长结果每轮 +4000 字符。
	compactor := compact.NewCompactor(
		&api.Client{BaseURL: sumSrv.URL, APIKey: "k", Model: cfg.Model, Client: sumSrv.Client(), RetryCount: 0},
		4000)
	e := NewQueryEngine(cfg, client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), compactor, nil)
	e.SetRunStateDir(dir)

	if _, err := e.RunIsolated(context.Background(), "长任务", isoOpts(id, 12)); err == nil {
		t.Fatal("期望 MaxTurns 收尾")
	}

	mu.Lock()
	folded := summaryCalls
	mu.Unlock()
	if folded == 0 {
		t.Fatal("一次摘要请求都没发出 —— 隔离运行仍然豁免压缩, 长 run 会一路撞死在 prompt-too-long 上")
	}
	// 判据二: 折叠产物必须出现在后续的**业务请求体**里 (只发摘要请求而不采纳结果,
	// 是"折叠了但没生效"—— 从调用计数上完全看不出来)。
	reqs := u.requests()
	sawSummary := false
	for i, r := range reqs {
		if strings.Contains(r.Flat, "被折叠掉的历史摘要") {
			sawSummary = true
			if i == 0 {
				t.Fatal("第 0 轮请求里就有摘要 —— 折叠产物不是本次运行产生的")
			}
			break
		}
	}
	if !sawSummary {
		t.Fatalf("发了 %d 次摘要请求, 但它没进上下文 —— 折叠没生效, token 一分没省", folded)
	}
}

// TestH32_ShortRunIsNotFolded 历史太短时不折叠 —— 挡的是"折叠判据只剩阈值"。
//
// # 为什么必须落在消息数 7..11 这个窄带里
//
// 折叠本身有两道关, 消息数下限 (isolatedFoldMinMessages=12) 只是后面那道:
//
//	≤6 条: compactLocked 的**尾部保护** (tailCount=6) 就返回了, 连摘要都不发 ——
//	       这条路上"下限"存不存在都一样。
//	7..11: 尾部保护放行 (len>6), 阈值这一关决定折不折 —— **只有这一带**能读出
//	       下限到底通没通电。
//
// 我第一版把这条用例写成"DisableTools 的单轮运行不折叠" (3 条消息), 落在第一带
// 里, 于是把生产代码里的下限整个删掉它照样全绿 —— 一个测不出差别的用例。
// 现在跑 4 轮工具往返 ⇒ 结束时 9 条消息, 正好在第二带。
//
// 上下文窗口取 100 (阈值 80 tokens), 每个 tool_result 4000 字符 (≈1000 tokens)
// ⇒ 阈值这一关必然放行, 是否折叠**只**取决于下限。
func TestH32_ShortRunIsNotFolded(t *testing.T) {
	id, cfg := stateIdentity()

	var summaryCalls int32
	var mu sync.Mutex
	sumSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		summaryCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.APIResponse{
			ID: "sum", Type: "message", Role: "assistant", Model: "fake-model",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "摘要"}},
			StopReason: "end_turn",
		})
	}))
	t.Cleanup(sumSrv.Close)

	// 4 轮工具往返 ⇒ 结束时 9 条消息 (7..11 带内), 每轮结果很长 ⇒ 阈值必然越过。
	u := newStateUpstream(t, 4, "设计稿")
	reg := tool.NewRegistry()
	reg.Register(longNoopTool{})
	client := &api.Client{BaseURL: u.srv.URL, APIKey: "k", Model: cfg.Model, Client: u.srv.Client(), RetryCount: 0}
	compactor := compact.NewCompactor(
		&api.Client{BaseURL: sumSrv.URL, APIKey: "k", Model: cfg.Model, Client: sumSrv.Client(), RetryCount: 0},
		100)
	e := NewQueryEngine(cfg, client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), compactor, nil)

	if _, err := e.RunIsolated(context.Background(), "写个设计稿", isoOpts(id, 6)); err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if summaryCalls != 0 {
		t.Fatalf("%d 条消息 (在尾部保护 6 之上、下限 12 之下) 的短 run 发起了 %d 次摘要请求 —— "+
			"折叠判据只剩阈值, 消息数下限没生效", 2*u.count()-1, summaryCalls)
	}
}

// ============================================================================
// H3-3 交接物: 预算 / 轮数逼近上限前, 强制落一份"进度 + 下一步"
// ============================================================================
//
// 方案 §21 H3-3 的验收原文是:
//
//	**「把预算设到必然耗尽, run 结束后交接文件存在且非空; 下一次 run 读它不需要
//	重新探索 (探索步数显著下降)。」**
//
// # 前半句与后半句分别怎么判
//
//	前半句 (文件存在且非空) 落在**磁盘**上, 由生产路径写出来的字节;
//	后半句 (不需要重新探索) 落在**第二个 run 发给上游的请求体**上。
//
// 后半句不能靠"文件里写了什么"来判 —— 文件写得再好, 它没进上下文就等于没有。
// 也不能靠"请求条数变少"来判 (两个 run 的轮数本来就由上游脚本决定, 与交接无关);
// 唯一的判据是:**第二个 run 的首个请求里出现了只有交接说明才可能带的内容**。
//
// 每一条"留下了交接"都配一条"不该留下时必须不留下" (正常收尾 / 被 ctx 取消 /
// 没装配目录), 理由与 H3-2 相同: 这个能力的失败模式不对称 —— 少留一次只是重跑,
// 而**多留一次会让下一个 run 去重做已经做完的事**, 且它的 prompt 看上去完全正常。

// newBudgetUpstream 造一个"业务回合永远回 tool_use"的上游, 并给出控轮数的把手。
//
// 与 newStateUpstream 的区别只有一处: 这里要**精确控制预算在第几轮耗尽**, 而那要求
// 业务端点不能自己收尾 (否则 run 会在预算耗尽之前正常结束, 用例测的就不是这条路)。
// 响应逻辑仍走同一份 start(), 避免两套 toolTurns 语义。
func newBudgetUpstream(t *testing.T) (*stateUpstreamRun, *api.Client) {
	t.Helper()
	u := &stateUpstreamRun{toolTurns: -1, handoffText: "我改了 auth/login.go, 下一步该跑 go test ./auth/..."}
	u.start(t)
	t.Cleanup(u.srv.Close)
	return u, &api.Client{BaseURL: u.srv.URL, APIKey: "k", Model: "fake-model",
		Client: u.srv.Client(), RetryCount: 0}
}

// newBudgetEngine 预算闸门装配好了的引擎 (limit 个 token 之后终止)。
func newBudgetEngine(t *testing.T, u *stateUpstreamRun, cfg *Config, dir string, limit int) *QueryEngine {
	t.Helper()
	e := newStateEngine(t, u, cfg, dir)
	e.RunBudget = internal_hook.NewRunBudgetGate(limit)
	return e
}

// TestH33_BudgetExhaustionLeavesHandoff 预算必然耗尽时, 磁盘上留得下一份非空交接说明。
//
// 这是方案验收的前半句, 逐字对照: "把预算设到必然耗尽, run 结束后交接文件存在且非空"。
func TestH33_BudgetExhaustionLeavesHandoff(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()
	u, _ := newBudgetUpstream(t)
	// 每轮报 10 个 token (见假上游的 Usage), 上限 25 ⇒ 第 3 轮起必然耗尽。
	e := newBudgetEngine(t, u, cfg, dir, 25)

	_, err := e.RunIsolated(context.Background(), "实现登录接口", isoOpts(id, 50))
	if err == nil {
		t.Fatal("预算设成必然耗尽, 不该正常结束")
	}
	if !errors.Is(err, internal_hook.ErrRunBudgetExhausted) {
		t.Fatalf("终止原因不是预算耗尽, 而是 %v —— 这条用例测的就不是预算这条路", err)
	}

	// 判据一: 文件存在。
	path := handoffFilePath(t, e, id)
	body := readHandoffText(t, path)
	// 判据二: 非空 —— 且不是"只有骨架没有内容"。
	//
	// 只断言 len(body) > 0 是假的: 上面那一段 run 元信息 (目标/轮数/身份) 是 harness
	// 自己拼的, 模型一个字都不吐也能让它非空。所以判据落在**模型自述那一段**上。
	if !strings.Contains(body, u.handoffText) {
		t.Fatalf("交接说明里没有模型写的内容 (只有 harness 拼的骨架)。正文:\n%s",
			truncateForTest(body, 800))
	}
	// 判据三: 终止原因与实耗落在正文里 —— 读它的人 (与下一个执行者) 得知道为什么停。
	if !strings.Contains(body, string(HandoffByBudget)) {
		t.Fatalf("交接说明没写停止原因, 正文:\n%s", truncateForTest(body, 400))
	}
	if !strings.Contains(body, "实耗") {
		t.Fatalf("交接说明没写预算实耗, 正文:\n%s", truncateForTest(body, 400))
	}
	// 判据四: 交接请求必须**不带工具**。
	//
	// 这不是形式要求: 带了工具, 一个正干到兴头上的模型会继续 Read/Edit, 于是这次
	// 请求的产出全是 tool_use, 而"下一步"根本没被写下来 —— 交接说明变成一份空壳,
	// 且从"文件存在且非空"这个判据上完全看不出来。
	reqs := u.requests()
	var hreq *seenRequest
	for i := range reqs {
		if reqs[i].Handoff {
			hreq = &reqs[i]
		}
	}
	if hreq == nil {
		t.Fatal("上游一次交接请求都没收到 —— 交接说明里的内容不可能是模型写的")
	}
	if len(hreq.ToolNames) != 0 {
		t.Fatalf("交接请求带了 %d 个工具 (%v) —— 模型可能选择「再做一步」而不是写交接说明",
			len(hreq.ToolNames), hreq.ToolNames)
	}
}

// TestH33_TurnLimitLeavesHandoff 轮数用光时同样留交接说明。
//
// # 这条臂是变异测试逼出来的
//
// 我第一版把 H3-3 的用例全写在预算那条路上 (那是最容易构造"必然耗尽"的一条),
// 于是**轮数用光这条路上一个断言都没有**: 把循环出口那一次 emitHandoff 整块注释
// 掉, 全部用例照样全绿。而生产上"150 轮用光"与"预算耗尽"是同等的常态 —— 后者
// 要有配置才会发生, 前者是无配置的默认收场。
//
// 顺带钉住原因名的**区分**: 两条路都留交接说明, 但那句话里的原因不同。写成一个
// 常量是最省事的写法, 而它会让读到交接的下一次执行去调一个不相干的配置。
func TestH33_TurnLimitLeavesHandoff(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	// 上游永不收尾, 且**不设**预算 ⇒ 唯一的终止方式就是轮数用光。
	u := newStateUpstream(t, -1, "unused")
	u.handoffText = "轮数用光前我改到了 handlers/auth.go"
	e := newStateEngine(t, u, cfg, dir)
	if _, err := e.RunIsolated(context.Background(), "实现登录接口", isoOpts(id, 3)); err == nil {
		t.Fatal("MaxTurns=3 且上游恒回 tool_use, 不该正常结束")
	}
	// 判据一: 文件在、模型那段在。
	body := readHandoffText(t, handoffFilePath(t, e, id))
	if !strings.Contains(body, u.handoffText) {
		t.Fatalf("轮数用光的 run 没留下交接说明 (或模型那段丢了)。正文:\n%s", truncateForTest(body, 600))
	}
	// 判据二: 原因名是 max_turns, **不是** budget_exhausted。
	if !strings.Contains(body, string(HandoffByTurns)) {
		t.Fatalf("交接说明的停止原因不是 %q —— 下一次读到它的人会去调一个不相干的配置。正文:\n%s",
			HandoffByTurns, truncateForTest(body, 400))
	}
	if strings.Contains(body, string(HandoffByBudget)) {
		t.Fatalf("轮数用光的 run 把原因写成了预算耗尽。正文:\n%s", truncateForTest(body, 400))
	}
	// 判据三: 交接请求不带工具 (与预算那条同一纪律)。
	for _, r := range u.requests() {
		if r.Handoff && len(r.ToolNames) != 0 {
			t.Fatalf("交接请求带了工具 (%v)", r.ToolNames)
		}
	}
	// 判据四: 这一次多花的调用必须**只有一次** —— 临终重试是拿数据换一个
	// "看起来做完了"的假象。
	n := 0
	for _, r := range u.requests() {
		if r.Handoff {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("交接请求发了 %d 次 (期望恰好 1 次)", n)
	}
}

// TestH33_NextRunAdoptsHandoff 下一次 run 读它, 不需要重新探索。
//
// 验收的后半句。判据是**第二个 run 发给上游的首个请求里带着交接说明的内容** ——
// "文件写出来了"与"文件被用上了"是两件事, 中间断在哪一环都只在请求体上看得见。
func TestH33_NextRunAdoptsHandoff(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	// ---- 第 1 段: 跑一次必然耗尽预算的 run, 它留下一份交接说明 ----
	u1, _ := newBudgetUpstream(t)
	e1 := newBudgetEngine(t, u1, cfg, dir, 25)
	if _, err := e1.RunIsolated(context.Background(), "实现登录接口", isoOpts(id, 50)); !errors.Is(err, internal_hook.ErrRunBudgetExhausted) {
		t.Fatalf("第一段期望预算耗尽, 得到 %v", err)
	}

	// ---- 第 2 段: 同身份的下一轮, 必须以交接说明为起点 ----
	u2 := newStateUpstream(t, 0, "接着做完了")
	e2 := newStateEngine(t, u2, cfg, dir)
	if _, err := e2.RunIsolated(context.Background(), "实现登录接口", isoOpts(id, 50)); err != nil {
		t.Fatalf("第二段运行失败: %v", err)
	}
	reqs := u2.requests()
	if len(reqs) == 0 {
		t.Fatal("第二段一次请求都没发出")
	}
	first := reqs[0]
	// 判据一: 上一次的具体结论进了首个请求。
	//
	// 判据必须落在这句**只有上一次跑过才知道的内容**上, 不能落在"请求里有没有
	// 交接这个词"上 —— 后者在"harness 拼了骨架但模型那段丢了"时同样成立, 而那正是
	// 交接说明最常失败的方式 (模型写了一堆 tool_use, 说明里只剩元信息)。
	if !strings.Contains(first.Flat, "auth/login.go") {
		t.Fatalf("第二段的**首个**请求里没有上一次的结论 —— 交接说明没被采纳, "+
			"下一次仍然要从零探索 (这正是本任务要消除的重复劳动)。首个请求:\n%s",
			truncateForTest(first.Flat, 900))
	}
	// 判据二: 必须说清楚"这是上一次的交接、不用重做已写明的探索"。
	//
	// 少了这层说明, 模型会把说明当成一份普通的前置材料, 礼貌地照做一遍 ——
	// 于是"不用重新探索"这条验收在最贵的那个维度 (token) 上没有兑现, 而文件、
	// 注入、内容三条判据全都还是绿的。
	if !strings.Contains(first.Flat, "上一次执行") || !strings.Contains(first.Flat, "不要重做") {
		t.Fatalf("交接说明被注入时没有带「这是上次的、不必重做」的说明, 模型会照单重跑一遍。首个请求:\n%s",
			truncateForTest(first.Flat, 900))
	}
	// 判据三: 本次的目标仍然在 —— 交接说明是垫在输入**之前**的, 不替代输入。
	if !strings.Contains(first.Flat, "实现登录接口") {
		t.Fatalf("首个请求里没有本次目标 —— 交接说明把输入挤掉了。首个请求:\n%s",
			truncateForTest(first.Flat, 600))
	}
	// 判据四: 这个事实被记账了 —— "有没有用上"得能事后查, 不能只靠"文件在不在"。
	if st := readState(t, stateFilePath(t, e2, id)); !st.HandoffUsed {
		t.Fatal("采纳了交接说明, 但中间态里没记 HandoffUsed")
	}
}

// TestH33_HandoffIsNotResumedAsHistory 交接说明与"中断恢复"是两条互斥的路。
//
// # 为什么这条必须单独验
//
// 两件事都会在"下一次 run 的首个请求里出现上一次的内容", 于是**一条只断言"上次的
// 东西在请求里"的用例对两者没有分辨力** —— 把交接说明换成直接恢复原始历史, 它照样绿。
// 而这两种语义的后果完全不同: 恢复历史是"接着那段对话继续", 交接说明是"重开一个,
// 但先看这份备忘"。前者会让模型以为自己刚写完那 50 轮; 后者不会。
//
// 分辨它们靠**形状**, 不靠内容:
//
//	恢复历史 ⇒ 上一次的**每一轮**都在 (tool_use / tool_result 全带过来);
//	交接说明 ⇒ 上一次只有**一段文本**在, 且带着注入说明的包裹。
func TestH33_HandoffIsNotResumedAsHistory(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	u1, _ := newBudgetUpstream(t)
	e1 := newBudgetEngine(t, u1, cfg, dir, 25)
	if _, err := e1.RunIsolated(context.Background(), "实现登录接口", isoOpts(id, 50)); !errors.Is(err, internal_hook.ErrRunBudgetExhausted) {
		t.Fatalf("第一段期望预算耗尽, 得到 %v", err)
	}
	// 第一段跑了不止一轮 —— 否则下面"历史有没有带过来"的分辨力不成立。
	if n := len(u1.requests()); n < 3 {
		t.Fatalf("第一段只跑了 %d 轮, 这条用例区分不了「恢复历史」与「交接说明」", n)
	}
	// 第一段的中间态必须是 Finished: 这既是"两条路互斥"的结构保证, 也是它成立的原因。
	if st := readState(t, stateFilePath(t, e1, id)); !st.Finished {
		t.Fatal("跑到上限的 run 没有把中间态标成终态 —— 那么下一次会走恢复路径, 与交接说明叠在一起")
	}

	u2 := newStateUpstream(t, 0, "接着做完了")
	e2 := newStateEngine(t, u2, cfg, dir)
	if _, err := e2.RunIsolated(context.Background(), "实现登录接口", isoOpts(id, 50)); err != nil {
		t.Fatalf("第二段运行失败: %v", err)
	}
	reqs := u2.requests()
	if len(reqs) != 1 {
		t.Fatalf("第二段应恰好 1 次请求, got %d", len(reqs))
	}
	// 判据一: 第一段的**过程**没有带过来 (恢复历史会把它全带过来)。
	//
	// "第N轮思考"是第一段每轮的 assistant 文本 (见假上游)。它在第二段的请求里出现,
	// 就说明带过来的是原始历史而不是一份提炼。
	for i := 0; i < 3; i++ {
		if marker := "第" + itoa(i) + "轮思考"; strings.Contains(reqs[0].Flat, marker) {
			t.Fatalf("第二段的首个请求里带着第一段的原始回合 %q —— 交接说明退化成了历史恢复; "+
				"模型会以为那 50 轮是它自己刚写完的。首个请求:\n%s",
				marker, truncateForTest(reqs[0].Flat, 900))
		}
	}
	// 判据二: 带过来的是**包裹过的**文本, 不是裸的。
	if !strings.Contains(reqs[0].Flat, "交接说明开始") {
		t.Fatalf("第二段的首个请求里没有交接说明的包裹 —— 单独一段来路不明的文本会被当成前置材料。首个请求:\n%s",
			truncateForTest(reqs[0].Flat, 900))
	}
}

// TestH33_NormalFinishClearsHandoff 正常跑完的 run 会把上一次的交接说明清掉。
//
// 交接说明的语义是"我半路停下了, 下一步在这里"。一次成功的 run 之后这句话是**假的**
// ——留着它, 下一个同身份的 run 会读到"还有没做完的事"而去重做已经完成的活。
//
// 这条同时钉住了一个很容易写反的地方: 收尾时**不能无条件清**。被迫停止的那一次 run
// 也走同一个 defer, 而它 err != nil —— 无条件清会把同一次 run 刚写下的交接说明就地
// 删掉, 于是最主要的那条路径上永远没有交接物, 而另外三条用例全都还是绿的
// (它们只跑被迫停止的那一段)。
func TestH33_NormalFinishClearsHandoff(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	// 手工摆一份"上一次留下的"交接说明 (内容无关紧要, 它只用来验证被清掉)。
	e0 := newStateEngine(t, newStateUpstream(t, 0, "x"), cfg, dir)
	path := handoffFilePath(t, e0, id)
	if err := writeFileAtomic(path, []byte(renderHandoff(handoffAt{
		Key: RunKey(id), Identity: id, Objective: "上一次的任务",
		Reason: HandoffByTurns, Turn: 7, MaxTurns: 7,
	}, "上一次没做完的部分"))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("交接说明没摆上: %v", err)
	}

	// 正常跑完 (0 轮工具 ⇒ 一轮收尾, err == nil)。
	u := newStateUpstream(t, 0, "这次做完了")
	e := newStateEngine(t, u, cfg, dir)
	if _, err := e.RunIsolated(context.Background(), "本次任务", isoOpts(id, 5)); err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("正常跑完之后过期的交接说明还在磁盘上 —— 下一个 run 会去重做已经完成的活")
	}

	// 判据二: 再起一个同身份的 run, 它的**首个请求**里不能有那份旧内容。
	//
	// 为什么判据落在下一个 run 而不是刚跑完的这个: 上面那次跑**本来就该**采纳这份
	// 交接说明 (它确实是"上一次被迫停下"留下的), 采纳是对的。真正要防的是它跑完之后
	// 那份说明还留着 —— 而那只有从**下一个** run 的请求体上才读得出来。(第一版把判据
	// 写在了刚跑完的那次上, 于是它断言的是一个正确的行为, 必然红。)
	u2 := newStateUpstream(t, 0, "又做了一次")
	e2 := newStateEngine(t, u2, cfg, dir)
	if _, err := e2.RunIsolated(context.Background(), "本次任务", isoOpts(id, 5)); err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	if reqs := u2.requests(); len(reqs) != 1 || strings.Contains(reqs[0].Flat, "上一次没做完的部分") {
		t.Fatalf("正常收尾之后的 run 仍然读到了上一份交接说明 —— 它会去重做已经完成的活:%s",
			truncateForTest(reqs[0].Flat, 600))
	}
}

// TestH33_ForcedStopKeepsItsOwnHandoff "被迫停止"那一次自己写下的交接说明必须留住。
//
// 这是上一条的反面臂, 也是把它钉死的那一枚钉子: 清与不清的分界只差 `err == nil`
// 这一个判据, 而写成"无条件清"时, 上一条用例依然全绿 (它测的是正常收尾)。
func TestH33_ForcedStopKeepsItsOwnHandoff(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()
	u, _ := newBudgetUpstream(t)
	e := newBudgetEngine(t, u, cfg, dir, 25)

	if _, err := e.RunIsolated(context.Background(), "实现登录接口", isoOpts(id, 50)); !errors.Is(err, internal_hook.ErrRunBudgetExhausted) {
		t.Fatalf("期望预算耗尽, 得到 %v", err)
	}
	body := readHandoffText(t, handoffFilePath(t, e, id))
	if !strings.Contains(body, u.handoffText) {
		t.Fatalf("被迫停止的那次 run 把自己的交接说明清掉了 (或没写)。正文:\n%s",
			truncateForTest(body, 600))
	}
}

// TestH33_CancelledRunLeavesNoHandoff 被取消的 run 不留交接说明。
//
// 这不是"省一次调用": 进程被 Ctrl-C / 超时杀掉时, 上下文里那半截工作**已经**由
// H3-2 的中间态保存了, 下一次会从断点继续。再写一份交接说明等于给下一次两条互相
// 矛盾的信息 ("接着跑那段历史" 与 "重开一次, 看这份备忘")。
//
// 拿 ctx 取消当成"进程被杀"的替身是**不精确**的 (真被杀时连这条路径都跑不到),
// 所以这条用例证的是它名下那一件事: **可取消的运行不留交接物**。这仍然是必要判据
// —— 取消是超时熔断的收场, 它在生产里天天发生。
func TestH33_CancelledRunLeavesNoHandoff(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	u := newStateUpstream(t, -1, "unused") // 永不收尾, 只能靠取消
	e := newStateEngine(t, u, cfg, dir)
	ctx, cancel := context.WithCancel(context.Background())
	// 第 1 轮请求发出后取消 —— 循环下一次检查 ctx.Err() 时退出, 走的是提前 return,
	// 不是 MaxTurns 出口, 也不是预算出口。
	u.onRequest = func(turn int, _ seenRequest) {
		if turn == 0 {
			cancel()
		}
	}
	_, err := e.RunIsolated(ctx, "实现登录接口", isoOpts(id, 50))
	if err == nil {
		t.Fatal("ctx 已取消, 不该正常结束")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("终止原因不是取消, 而是 %v", err)
	}
	if _, statErr := os.Stat(handoffFilePath(t, e, id)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("被取消的 run 留下了交接说明 —— 下一次会拿到两条互相矛盾的指令: "+
			"既有一份可恢复的中间态 (%v), 又有一份「重开一次」的交接说明", statErr)
	}
}

// countingTransport 数**上线次数**的传输层包装。
//
// 为什么要数到这一层, 而不是数假上游收到几次请求: 带着已取消的 ctx 发请求时,
// 连接在服务端 handler 被调用**之前**就断了 —— 假上游的计数器恒为 0, 于是
// "被守卫挡住没发"与"发了但失败"给出同一个读数。第一版正是这么量的, 它对那个
// 守卫没有分辨力 (变异测试实测: 把 ctx.Err() 判据整个删掉, 用例照样全绿)。
//
// RoundTrip 是 http.Client 真正把请求送上线的位置, 无论 ctx 是否已取消都会被调到。
type countingTransport struct {
	n  atomic.Int64
	rt http.RoundTripper
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.rt.RoundTrip(r)
}

// TestH33_CancelledCtxSkipsTheHandoffCall 已经取消的 ctx 一个字节都不往上游发。
//
// # 这条判据归谁
//
// 归 pkg/api 的重试环 (pkg/api/client.go:1705 在第一次上线之前就返回), **不归**
// emitHandoff —— 我在那里写过一句 `ctx.Err() != nil` 的守卫, 移掉它这条用例照样
// 全绿, 因为它从来就没起过作用。删掉那个守卫之后, 这条用例留下, 且它的注释指向
// 真正的实现者: 哪天 pkg/api 那个判据被挪走, 这里会红, 而"谁该修"是清楚的。
//
// # 为什么直接打 emitHandoff, 而不是走一遍 RunIsolated
//
// 这个状态在循环里**构造不出来**: 走到两处 emitHandoff 都需要此前有一次成功的
// SendMessage, 而循环顶上的 ctx.Err() 与实际取消会把取消拦在前面 (能命中的只有
// "SendMessage 刚成功、emitHandoff 还没跑"那一瞬间的死线到期, 是个竞态窗口)。
// 直接打这个函数与"版本不符一律全新运行"那条臂同一种写法。
func TestH33_CancelledCtxSkipsTheHandoffCall(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	// 引擎的 HTTP 出口包一层计数器。其余一切照旧 (同一个假上游, 同一套注册表)。
	u := newStateUpstream(t, 0, "unused")
	tr := &countingTransport{rt: u.srv.Client().Transport}
	e := NewQueryEngine(cfg, &api.Client{
		BaseURL: u.srv.URL, APIKey: "k", Model: cfg.Model,
		Client: &http.Client{Transport: tr}, RetryCount: 0,
	}, func() *tool.Registry { r := tool.NewRegistry(); r.Register(noopTool{}); return r }(),
		hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeAcceptEdits), nil, nil)
	e.SetRunStateDir(dir)

	live := handoffAt{
		Key: RunKey(id), Identity: id, Objective: "任务",
		Reason: HandoffByTurns, Turn: 5, MaxTurns: 5, Model: cfg.Model,
		System:   []string{"sys"},
		Messages: []types.Message{{Type: types.MessageTypeUser, UUID: "u1"}, {Type: types.MessageTypeAssistant, UUID: "a1"}},
		Client:   e.APIClient, MaxTokens: 100,
	}

	// 臂一: ctx 活着 —— 必须真的发出去一次并落盘。这一臂是下面那条判据的地基:
	// 少了它, "上线 0 次"在 emitHandoff 整个坏掉时同样成立。
	e.emitHandoff(context.Background(), live)
	if n := tr.n.Load(); n != 1 {
		t.Fatalf("ctx 活着时上线 %d 次 (期望 1 次)", n)
	}
	if _, err := os.Stat(handoffFilePath(t, e, id)); err != nil {
		t.Fatalf("ctx 活着时没写下交接说明: %v", err)
	}
	_ = os.Remove(handoffFilePath(t, e, id))

	// 臂二: ctx 已取消 —— 一次都不该上线。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := tr.n.Load()
	e.emitHandoff(ctx, live)
	if n := tr.n.Load() - before; n != 0 {
		t.Fatalf("ctx 已取消却仍然上线了 %d 次 —— 关机路径上多一串注定失败的重试", n)
	}
	if _, err := os.Stat(handoffFilePath(t, e, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ctx 已取消却写下了交接说明")
	}
}

// TestH33_HandoffCallIsMarkedInTrace 交接那次调用在轨迹里带 handoff 标记。
//
// # 为什么这枚标记必须有判据
//
// 交接请求是这项能力**唯一多花的钱**(对照 SWE-agent 的 1.1×)。它与一次普通的
// llm_call 长得完全一样 (同一套请求/响应字段), 唯一的区别是语义: 它不推进任务,
// 且只在 run 临终时出现。账本里少了这枚标记, "这个 run 为什么多花了 5%"就无从
// 解释 —— 而看见一次不带工具的调用更会让人以为 run 是被别的东西打断的。
//
// 判据不能落在"tools 字段是空的"上: 不带工具的调用不止它一个 (DisableTools 的
// 思考型角色同样如此), 那是个充分不必要条件。变异测试实测: 把这枚标记整块删掉,
// 全部用例照样全绿 —— 直到写下这一条。
func TestH33_HandoffCallIsMarkedInTrace(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()
	u, _ := newBudgetUpstream(t)
	e := newBudgetEngine(t, u, cfg, dir, 25)

	store := tracestore.New(statestore.NewMemStore())
	e.TraceStore = store
	runID := "run-handoff-trace"
	ctx := trace.With(context.Background(), trace.IDs{RunID: runID, NodeID: "n1", TurnID: "t1"})

	if _, err := e.RunIsolated(ctx, "实现登录接口", isoOpts(id, 50)); !errors.Is(err, internal_hook.ErrRunBudgetExhausted) {
		t.Fatalf("期望预算耗尽, 得到 %v", err)
	}

	spans, err := store.ReadRun(runID)
	if err != nil {
		t.Fatalf("ReadRun: %v", err)
	}
	var marked, unmarked int
	for _, sp := range spans {
		if sp.Kind != tracestore.KindLLMCall {
			continue
		}
		if sp.Attrs["handoff"] == true {
			marked++
			// 交接请求的另一半事实: 不带工具 (上面已按请求体验过, 这里按账本再验一次
			// —— 两处都错才会一起漏)。
			if n, _ := sp.Attrs["tools"].(float64); n != 0 {
				t.Fatalf("带 handoff 标记的 span 记着 %v 个工具", sp.Attrs["tools"])
			}
			continue
		}
		unmarked++
	}
	if marked != 1 {
		t.Fatalf("带 handoff 标记的 llm_call span 有 %d 条 (期望恰好 1 条) —— "+
			"没有它, 这次多花的调用在账本上无从解释", marked)
	}
	if unmarked == 0 {
		t.Fatal("除了交接请求以外一条 llm_call span 都没有 —— 这个 run 没跑业务回合, 用例没有分辨力")
	}
}

// TestH33_TooShortRunSkipsTheHandoffCall 还没跑起来就停下的 run 不发交接请求。
//
// 一份"我什么也没做"的说明书比没有更糟: 下一次读到它会以为自己有一份进度。
func TestH33_TooShortRunSkipsTheHandoffCall(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()
	u := newStateUpstream(t, 0, "unused")
	e := newStateEngine(t, u, cfg, dir)

	e.emitHandoff(context.Background(), handoffAt{
		Key: RunKey(id), Identity: id, Objective: "任务",
		Reason: HandoffByTurns, Turn: 1, MaxTurns: 1, Model: cfg.Model,
		System:   []string{"sys"},
		Messages: []types.Message{{Type: types.MessageTypeUser, UUID: "u1"}}, // 只有起始输入
		Client:   e.APIClient, MaxTokens: 100,
	})
	if n := u.count(); n != 0 {
		t.Fatalf("只跑了一条消息的 run 发了 %d 次交接请求", n)
	}
	if _, err := os.Stat(handoffFilePath(t, e, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("只跑了一条消息的 run 写下了交接说明 —— 下一次会以为自己有进度")
	}
}

// TestH33_NoAssemblyMeansNoHandoff 没装配目录时, 磁盘上一个字节都不该多。
//
// 与 H3-2 的对照臂同一条纪律: 这条能力是旁路, 没接上时必须与改造前逐字节相同。
func TestH33_NoAssemblyMeansNoHandoff(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	u, _ := newBudgetUpstream(t)
	e := newBudgetEngine(t, u, cfg, "", 25) // 不装配 runStateDir
	if _, err := e.RunIsolated(context.Background(), "实现登录接口", isoOpts(id, 50)); !errors.Is(err, internal_hook.ErrRunBudgetExhausted) {
		t.Fatalf("期望预算耗尽, 得到 %v", err)
	}
	// 判据一 (直接读开关): 未装配时连路径都算不出来 —— 与 TestH32_NoAssemblyMeansNoFiles
	// 同一条理由 (只走"目录里有没有多出文件"会在"写到了别处"时假绿)。
	if p := e.handoffPath(RunKey(id)); p != "" {
		t.Fatalf("没装配目录却算出了交接说明路径 %q —— 关不掉的开关等于没有开关", p)
	}
	// 判据二: 整个临时目录树里没有多出任何东西。
	if rest := strayFiles(t, dir); len(rest) != 0 {
		t.Fatalf("没装配目录仍然写出了文件: %v", rest)
	}
	// 判据三: 也不该为此多花一次上游调用 —— 验收允许的那一次"多花的钱"必须以
	// "确实留下了交接物"为前提, 否则纯是消耗。
	for _, r := range u.requests() {
		if r.Handoff {
			t.Fatal("没装配目录却仍然发出了交接请求 —— 白花一次调用, 且什么也没留下")
		}
	}
}

// TestH33_WriteFailuresAreReported "写不进去"必须在日志里看得见。
//
// # 这条挡的是什么
//
// 这个文件里所有写盘都是 fail-soft 的: 超限、目录建不出来、rename 失败 —— 一律
// 不返回错误, 只记一条 Warn。fail-soft 是对的 (旁路能力不该打死 run), 但它有一个
// 危险的默认状态: **「没写成」与「没发生」在磁盘上长得一模一样**。落盘失败的那次 run
// 跑完之后, 目录里什么也没有, 与「这个 run 没到上限」完全同形。
//
// 所以那条 Warn 不是一个可选的诊断, 它是这项能力的**唯一告警面**。而"记了一条日志"
// 这种判据天生容易写成假绿 (写了没写、级别对不对、被下游 handler 吃掉都测不出来),
// 所以这里直接把 slog 的输出接管过来, 断言那条字符确实出现。
//
// 变异测试逼出了这条: 把 finishRunState 里那句 `if !st.save(path) { Warn(...) }`
// 换成 `_ = st.save(path)`, 全部用例照样全绿 —— 因为没有任何一条在听日志。
//
// 注: pkg/engine 的测试从不调 logging.Init, 所以 logging.For 走的是 slog.Default()
// 这条退化路径 (见 pkg/logging/logging.go 的 For)。这里替换的正是那一个。
func TestH33_WriteFailuresAreReported(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// 制造一次确定性的写失败: 状态**超限** (save 直接返回 false, 不去碰文件系统)。
	e := &QueryEngine{}
	e.SetRunStateDir(t.TempDir())
	st := e.newRunState("isolated-logprobe", RunIdentity{Stage: "s", Role: "r"}, "任务", 3)
	st.Finished = true
	st.Messages = []types.Message{{
		Type: types.MessageTypeUser, UUID: "u",
		Content: []types.ContentBlock{{Type: types.ContentBlockText,
			Text: strings.Repeat("x", runStateMaxBytes+1024)}},
	}}
	e.finishRunState(st, "产出", errors.New("预算耗尽"))

	logged := buf.String()
	if !strings.Contains(logged, "终态落盘失败") {
		t.Fatalf("中间态超限没写成, 但日志里没有任何痕迹 —— "+
			"落盘失败与「这个 run 没到上限」在磁盘上完全同形, 这条日志是唯一告警面。日志:\n%s",
			truncateForTest(logged, 500))
	}
	// 反向对照: 正常大小的状态落盘**不该**报 "失败" —— 否则上面那条判据在
	// "每写必报"的实现上也成立, 而那种实现会让真正的失败被淹没在噪声里。
	buf.Reset()
	st.Messages = []types.Message{{Type: types.MessageTypeUser, UUID: "u"}}
	e.finishRunState(st, "产出", nil)
	if strings.Contains(buf.String(), "终态落盘失败") {
		t.Fatalf("正常落盘报了失败: %s", truncateForTest(buf.String(), 300))
	}
	if _, err := os.Stat(e.runStatePath("isolated-logprobe")); err != nil {
		t.Fatalf("正常大小的终态没写下去: %v", err)
	}
}

// strayFiles 列出目录树下的所有文件 (相对路径)。
func strayFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", dir, err)
	}
	return out
}

// TestH33_SuccessPathWritesNoHandoff 成功收尾时一次交接都不写。
//
// 与"预算耗尽"成对照臂。少了它, 一个"无论如何都写一份交接说明"的实现能让上面所有
// 用例照样全绿 —— 而它在生产上会让每一次成功的 run 都给下一次留一句"我还没做完",
// 于是下一个 run 反复重做已经交付的东西。这是本能力最贵的一种失效。
func TestH33_SuccessPathWritesNoHandoff(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()

	u := newStateUpstream(t, 2, "做完了")
	e := newStateEngine(t, u, cfg, dir)
	out, err := e.RunIsolated(context.Background(), "本次任务", isoOpts(id, 5))
	if err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	if !strings.Contains(out, "做完了") {
		t.Fatalf("产出不对: %q", out)
	}
	for _, r := range u.requests() {
		if r.Handoff {
			t.Fatalf("正常收尾的 run 发了交接请求 —— 每一次成功都会给下一次留一句假话")
		}
		if len(r.ToolNames) == 0 {
			t.Fatalf("正常回合没带工具 —— 这条用例的上游约定被破坏了")
		}
	}
	if _, statErr := os.Stat(handoffFilePath(t, e, id)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("正常收尾的 run 写了交接说明")
	}
}

// TestH33_HandoffWithWrongKeyIsRefused key 对不上的交接说明一律不采用。
//
// 路径本来就由 key 算出来, 所以"不符"在正常流程里不可能发生 —— 它挡的是**错位**:
// 文件被搬动、被另一个身份的运行命名、或有人在同名路径下放了一份别的文件。这条守卫
// 的成本是一次字符串比较, 而它挡掉的错是"把 A 的进度当成 B 的"—— 事后无从归因。
//
// 同时钉住前缀匹配的边界: key="isolated-a" 不能命中 key="isolated-ab" 那份文件。
func TestH33_HandoffWithWrongKeyIsRefused(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()
	key := RunKey(id)

	e0 := newStateEngine(t, newStateUpstream(t, 0, "x"), cfg, dir)

	// 情形一: 路径对、首行标记对, 但 key 是**另一个**身份的 (且是它的前缀)。
	path := handoffFilePath(t, e0, id)
	other := key + "-tail"
	if err := writeFileAtomic(path, []byte(renderHandoff(handoffAt{
		Key: other, Identity: id, Objective: "别人的任务",
		Reason: HandoffByTurns, Turn: 3, MaxTurns: 3,
	}, "别人没做完的部分"))); err != nil {
		t.Fatal(err)
	}
	if got := e0.loadHandoff(key); got != "" {
		t.Fatalf("key 不符的交接说明被采用了 (前缀匹配没有边界): %q", truncateForTest(got, 200))
	}
	// 情形二: 首行标记缺失 (同名路径下的无关文件)。
	if err := writeFileAtomic(path, []byte("这就是一份普通的 markdown, 不是交接说明\n# 随便写的")); err != nil {
		t.Fatal(err)
	}
	if got := e0.loadHandoff(key); got != "" {
		t.Fatalf("没有首行标记的文件被当成交接说明采用了: %q", truncateForTest(got, 200))
	}
	// 情形二·续: 首行没有标记, 但**碰巧**带一个正确的 key= token。
	//
	// 这一条单独列出来, 是因为没有它的话"首行标记"那道判据是**冗余的**: 上面两个
	// 情形里 key 也对不上, 于是"检查标记"与"检查 key"给出同一个结论, 删掉前者
	// 一个用例都不会红 (变异测试实测如此)。而这道判据真正挡的是这里: 一份 YAML /
	// 配置文件碰巧写着 key=<我们的 key>, 它不是我们的格式, 但 key 判据放它过去。
	// 一个能被另一种输入区分的守卫才不是纯装饰。
	if err := writeFileAtomic(path, []byte("key="+key+" 这是别人的配置文件\nserver: 8080")); err != nil {
		t.Fatal(err)
	}
	if got := e0.loadHandoff(key); got != "" {
		t.Fatalf("首行没有我们的标记、只是碰巧带对了 key= 的文件被采用了: %q", truncateForTest(got, 200))
	}
	// 情形三 (正向对照): 标记与 key 都对的必须读得出来 —— 否则上面两条判据恒真。
	if err := writeFileAtomic(path, []byte(renderHandoff(handoffAt{
		Key: key, Identity: id, Objective: "本任务",
		Reason: HandoffByTurns, Turn: 3, MaxTurns: 3,
	}, "本次没做完的部分"))); err != nil {
		t.Fatal(err)
	}
	if got := e0.loadHandoff(key); !strings.Contains(got, "本次没做完的部分") {
		t.Fatalf("合法的交接说明反而读不出来 (上面两条判据因此恒真): %q", truncateForTest(got, 200))
	}
}

// TestH33_HandoffIsCappedAndSaysSo 超长的交接说明被截断, 且**明说**被截断。
//
// 上限的理由与 run-state 不同: 那份是"读它的程序会崩", 这份是"读它的模型会被挤爆"
// —— 一份 5MB 的备忘会占掉下一次真正的工作上下文, 而省上下文正是它的目的。
//
// 判据看的是**截断后的尾巴**: 一份被砍掉结尾的说明读起来是"说完了", 而它没有。
func TestH33_HandoffIsCappedAndSaysSo(t *testing.T) {
	e := &QueryEngine{}
	id, _ := stateIdentity()
	huge := renderHandoff(handoffAt{
		Key: RunKey(id), Identity: id, Objective: strings.Repeat("目标", 1000),
		Reason: HandoffByBudget, Turn: 9, MaxTurns: 9,
	}, strings.Repeat("z", handoffMaxBytes+4096))
	if len(huge) > handoffMaxBytes {
		t.Fatalf("渲染结果 %d 字节, 超过上限 %d —— 上限没生效", len(huge), handoffMaxBytes)
	}
	if !strings.Contains(huge, "已被截断") {
		t.Fatal("超限的交接说明被截断了却没说 —— 读它的人/模型会以为这就是全部")
	}
	// 首行标记必须还在: 截断发生在正文上, 不能把身份标记砍掉 (否则下一次直接拒收)。
	if !strings.HasPrefix(huge, handoffLinePrefix) {
		t.Fatal("截断把首行身份标记砍掉了 —— 那份文件再也读不回来")
	}
	_ = e
}

// ============================================================================
// RunKey / RunState 的纯函数性质
// ============================================================================

// TestH32_RunKeyIsStableAndSeparated 键是身份的纯函数, 且字段不串联。
func TestH32_RunKeyIsStableAndSeparated(t *testing.T) {
	a := RunIdentity{Team: "ab", Workflow: "c", Stage: "s", Role: "r", Cwd: "/w"}
	b := RunIdentity{Team: "a", Workflow: "bc", Stage: "s", Role: "r", Cwd: "/w"}
	if RunKey(a) == RunKey(b) {
		t.Fatal("字段边界没分隔 —— (\"ab\",\"c\") 与 (\"a\",\"bc\") 撞了同一个键")
	}
	if RunKey(a) != RunKey(a) {
		t.Fatal("同一个身份两次算出的键不同")
	}
	// 任何一项变化都必须换键 —— 少一项就是这个字段不参与身份, 而那是静默的。
	for _, mut := range []func(*RunIdentity){
		func(x *RunIdentity) { x.Team = "z" },
		func(x *RunIdentity) { x.Workflow = "z" },
		func(x *RunIdentity) { x.Stage = "z" },
		func(x *RunIdentity) { x.Role = "z" },
		func(x *RunIdentity) { x.Cwd = "z" },
	} {
		v := a
		mut(&v)
		if RunKey(v) == RunKey(a) {
			t.Fatalf("改了身份的一项之后键没变: %+v vs %+v", v, a)
		}
	}
	// 键本身不能是"运行时装出来的" —— 它得能在另一个进程里算出来。
	if !strings.HasPrefix(RunKey(a), "isolated-") {
		t.Fatalf("键的形状不对: %q", RunKey(a))
	}
}

// TestH32_OversizeStateIsNotWrittenAndIsReported 超限时不写盘, 且这是**读得出来**的。
//
// 判据从磁盘读: 超限的那次 save 之后, 旧文件必须还在 (没被半截覆盖), 且不能出现
// 一个比上限还大的新文件。"静默不写"与"写了"在磁盘上的差别只在内容旧 —— 所以
// 生产代码里那条 Warn 是这条能力的唯一告警面, 这里同时钉住它的返回值。
func TestH32_OversizeStateIsNotWrittenAndIsReported(t *testing.T) {
	e := &QueryEngine{}
	dir := t.TempDir()
	e.SetRunStateDir(dir)
	p := e.runStatePath("isolated-oversize")
	if p == "" {
		t.Fatal("路径为空")
	}

	// 先写一份正常的, 作为"旧文件还在不在"的锚。
	small := &RunState{Version: runStateVersion, Key: "isolated-oversize",
		Messages: []types.Message{{Type: types.MessageTypeUser, UUID: "u1"}}}
	if !small.save(p) {
		t.Fatal("正常大小的状态写不进去")
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	// 再造一份超限的 (单条消息就超过 32MiB)。
	huge := &RunState{Version: runStateVersion, Key: "isolated-oversize",
		Messages: []types.Message{{
			Type: types.MessageTypeUser, UUID: "u2",
			Content: []types.ContentBlock{{Type: types.ContentBlockText,
				Text: strings.Repeat("x", runStateMaxBytes+1024)}},
		}}}
	if huge.save(p) {
		t.Fatalf("超过 %d 字节的状态仍然写成功了 —— 上限没生效", runStateMaxBytes)
	}
	// 旧文件必须原样还在: 超限的写不能把一份好的状态毁掉。
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("超限的写把旧状态文件弄没了: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("超限的写改动了磁盘上已有的状态")
	}
	// 也不能留下临时文件 (原子写的 rename 前那一步)。
	if _, err := os.Stat(p + ".tmp"); err == nil {
		t.Fatal("超限的写留下了 .tmp 临时文件")
	}
}

// TestH32_SaveIsAtomicUnderConcurrentRead 落盘是原子的: 并发读到的永远是完整的一份。
//
// # 为什么不能只断言"写完读回来是对的"
//
// 那对原子写与直接覆盖是同一个读数 —— 写完再看, 两种实现都给出新内容。我第一版
// 就是这么写的, 然后把它要防的那个变异 (rename 换回 os.WriteFile) 做过一遍:
// 用例照样全绿。**一个测不出差别的断言不该留着冒充验收。**
//
// 真正的区别只在**写的过程中**看得见: os.WriteFile 先截断再写, 中间那段时间读者
// 看到的是一个空的或写了一半的文件; rename 则是"要么旧的、要么新的"。
//
// 所以这里一边写一边读, 断言每次**读到的**字节都能完整解析成 JSON。载荷取 2MiB:
// 窗口足够宽, 非原子写几乎必然被读到半个文件。原子写这一侧是**确定性**的 0 次失败
// (rename 的语义保证), 所以这条用例正常跑永远不 flaky, 只有变异时才翻。
func TestH32_SaveIsAtomicUnderConcurrentRead(t *testing.T) {
	e := &QueryEngine{}
	e.SetRunStateDir(t.TempDir())
	p := e.runStatePath("isolated-atomic")

	payload := strings.Repeat("z", 2<<20) // 2MiB
	st := &RunState{Version: runStateVersion, Key: "isolated-atomic",
		Messages: []types.Message{{
			Type: types.MessageTypeUser, UUID: "a",
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: payload}},
		}}}
	if !st.save(p) {
		t.Fatal("首次写失败")
	}

	var (
		stop    = make(chan struct{})
		wg      sync.WaitGroup
		badRead atomic.Int64
		reads   atomic.Int64
	)
	// 读侧: 反复读盘, 每次都必须能完整解析。
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(p)
				if err != nil {
					// 文件在 rename 的一瞬间理论上不该消失 (rename 是原子的),
					// 但读到 ENOENT 只可能是测试自己删了 —— 这里不算失败。
					continue
				}
				reads.Add(1)
				var got RunState
				if json.Unmarshal(data, &got) != nil {
					badRead.Add(1)
				}
			}
		}()
	}
	// 写侧: 反复覆盖。
	for i := 0; i < 30; i++ {
		st.Turn = i + 1
		if !st.save(p) {
			close(stop)
			wg.Wait()
			t.Fatalf("第 %d 次写失败", i)
		}
	}
	close(stop)
	wg.Wait()

	if reads.Load() == 0 {
		t.Fatal("读侧一次都没读到 —— 用例没在测任何东西")
	}
	if n := badRead.Load(); n != 0 {
		t.Fatalf("并发读到了 %d 次不完整的状态文件 (共 %d 次读) —— 落盘不是原子的, "+
			"进程在写一半时被杀会留下半个 JSON (读了它的那次启动会直接当损坏丢掉)",
			n, reads.Load())
	}
}

// TestH32_LoadRejectsUnfinishedWithoutMessages 空消息的中间态不算可恢复状态。
//
// "有文件"与"有东西可恢复"是两件事。一个只有身份、没有历史的文件接上去, 模型会
// 看到一段比全新运行还少的历史, 而它看起来就是一次正常启动。
func TestH32_LoadRejectsUnfinishedWithoutMessages(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()
	e := newStateEngine(t, newStateUpstream(t, 0, "x"), cfg, dir)
	p := e.runStatePath(RunKey(id))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	empty := &RunState{Version: runStateVersion, Key: RunKey(id),
		Team: id.Team, Workflow: id.Workflow, Stage: id.Stage, Role: id.Role, Cwd: id.Cwd}
	if !empty.save(p) {
		t.Fatal("写空态失败")
	}
	if got := e.loadRunState(RunKey(id), id); got != nil {
		t.Fatalf("空历史的中间态被当成可恢复状态: %+v", got)
	}
}

// TestH32_VersionMismatchStartsFresh 不认识的版本一律按全新运行。
//
// 猜一个迁移规则比丢掉重跑更危险: 隔离运行的中间态是缓存, 丢掉的代价是重跑, 而
// 猜错一次就是把一份结构对不上的历史喂给模型。
func TestH32_VersionMismatchStartsFresh(t *testing.T) {
	id, cfg := stateIdentity()
	dir := t.TempDir()
	e := newStateEngine(t, newStateUpstream(t, 0, "x"), cfg, dir)
	p := e.runStatePath(RunKey(id))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	future := &RunState{Version: runStateVersion + 1, Key: RunKey(id),
		Team: id.Team, Workflow: id.Workflow, Stage: id.Stage, Role: id.Role, Cwd: id.Cwd,
		Messages: []types.Message{{Type: types.MessageTypeUser, UUID: "u"}}}
	if !future.save(p) {
		t.Fatal("写失败")
	}
	// save 会把 Version 改回当前版本, 所以这里直接改盘上的字节。
	raw, _ := json.Marshal(future)
	raw = []byte(strings.Replace(string(raw), `"version":1`, `"version":99`, 1))
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := e.loadRunState(RunKey(id), id); got != nil {
		t.Fatalf("未来版本的状态被接上了: %+v", got)
	}
}

// 注: 失败信息里的截断用本包已有的 truncateForTest (task_instruction_test.go:115)。

// longNoopTool 每轮吐一大段内容, 用来把 token 估算推过阈值。
type longNoopTool struct{}

func (longNoopTool) Name() string { return "noop_tool" } // 与 noopTool 同名: 覆盖注册
func (longNoopTool) Description() string {
	return "Verbose no-op tool for compaction tests."
}
func (longNoopTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (longNoopTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (longNoopTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (longNoopTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (longNoopTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: strings.Repeat("y", 4000)}, nil
}
