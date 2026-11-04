// fault_contract_test.go — Moqi R-H15 故障注入契约。
//
// # 这个文件钉的是什么
//
// 设计文档 §4 R-H15 要求"故障注入（超时、断流、500、非法 JSON）"，§5 Q-H7 要求
// "错误可分类（模型/工具/预算/安全/sink/取消）"。两条合起来是一件事：
//
//	**上游坏掉的时候，harness 必须说出它坏在哪里，而不是当作对话正常结束。**
//
// 这条要求不是形式主义。业务层（Moqi）拿到 `completed` 就会把"模型一个字都没回"
// 存成一条正常的助手消息；用户看到的是空白回复，排查时 trace 显示一切正常。
// 反过来，只要终态是 model_error，业务层就能重试、降级或明确告诉用户。
//
// # 为什么用假的 HTTP 上游而不是 mock api.Client
//
// 断流和非法 JSON 只有在**真实 SSE 解析路径**上才成立。mock 掉 api.Client 之后
// 测的是"我假设解析层会怎么报错"，而不是解析层实际怎么做 —— 而这个文件的全部
// 价值就在于发现"解析层实际什么都不报"。
package harness

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/harness/harnesstest"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// faultRunner 起一个走生产构造路径的 Runner，上游是入侵点可控的假模型。
func faultRunner(t *testing.T, fm *harnesstest.FakeModel, maxTurns int) *Runner {
	t.Helper()
	srv := fm.Server(t)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(),
		// 重试压到 0：故障注入要测的是**单次请求失败如何暴露**，
		// 不是退避曲线。默认 4 次重试会让 5xx 用例跑几十秒且掩盖终态。
		RetryCount: 1,
	}
	pm := prompt.NewManager(t.TempDir())
	pm.OverridePrompt = "你是执行者"

	eng := engine.NewQueryEngine(&engine.Config{Model: "m", MaxTurns: maxTurns},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, pm)

	return NewRunner(RunnerOptions{Engine: eng})
}

func faultTurn(t *testing.T, r *Runner, timeout time.Duration) *TurnResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, _ := r.RunTurn(ctx, TurnRequest{
		SessionID: "sess-fault", TurnID: "turn-" + t.Name(), OwnerID: "owner-1",
		Input: []types.ContentBlock{{Type: types.ContentBlockText, Text: "你好"}},
	}, nil)
	return res
}

// TestFault_TruncatedStreamIsModelError 钉住断流的语义。
//
// 场景：上游发了 message_start 与半句正文，然后在 message_delta 之前断开。
// 用户确实看到了半句话，但**这一轮没有完成**。
//
// 这不许报 completed：completed 会被业务层当作"模型说完了"存库，那半句话就
// 变成了一句看起来完整（其实被截断）的回复 —— 是数据质量事故，不是可用性问题。
func TestFault_TruncatedStreamIsModelError(t *testing.T) {
	fm := harnesstest.New(harnesstest.Truncate(3))
	r := faultRunner(t, fm, 2)

	res := faultTurn(t, r, 5*time.Second)

	if res.Terminal.Reason == "completed" {
		t.Fatalf("断流被当成正常完成: reason=%q blocks=%d —— 业务层会把截断的半句话存成完整回复",
			res.Terminal.Reason, len(res.Blocks))
	}
	if res.Terminal.Reason != "model_error" {
		t.Errorf("断流终态应为 model_error, 实际 %q (err=%v)", res.Terminal.Reason, res.Terminal.Error)
	}
	if res.Terminal.Error == nil {
		t.Error("断流的终态必须带 error, 否则业务层无法记日志或决定重试")
	}
	if fm.CallCount() != 1 {
		t.Errorf("断流应只请求上游 1 次, 实际 %d 次", fm.CallCount())
	}
}

// TestFault_BadJSONIsModelError 钉住非法 JSON 的语义。
//
// 场景：上游回了一帧 `data: {"type":`（截断的 JSON）。sscanf 拿到 data,
// json.Unmarshal 失败。
//
// 当前实现是 `continue` —— 那一帧被丢掉，流照常结束。于是 0 个事件、err=nil、
// 引擎看到"没有工具调用"就报 completed。这是**静默吞掉协议错误**：
// 上游协议变了、网关截断了响应、代理插入了错误页，全都表现为"模型什么都没说"。
//
// 错误**文本**也是判据的一部分：一帧都没解析成功时，业务层看到的必须是
// "协议不匹配/无法解析"，而不是"断流"。两种故障的处置不同 —— 前者要查网关
// 配置或上游版本，后者要重试。只断言 reason 的话，这两条分支在变异下互相
// 掩盖（实测：删掉协议错误分支后本测试仍然绿）。
func TestFault_BadJSONIsModelError(t *testing.T) {
	fm := harnesstest.New(harnesstest.BadJSON())
	r := faultRunner(t, fm, 2)

	res := faultTurn(t, r, 5*time.Second)

	if res.Terminal.Reason == "completed" {
		t.Fatalf("非法 JSON 被当成正常完成: blocks=%d —— 上游协议错误被静默吞掉", len(res.Blocks))
	}
	if res.Terminal.Reason != "model_error" {
		t.Errorf("非法 JSON 终态应为 model_error, 实际 %q (err=%v)", res.Terminal.Reason, res.Terminal.Error)
	}
	if res.Terminal.Error == nil {
		t.Fatal("非法 JSON 的终态必须带 error")
	}
	msg := res.Terminal.Error.Error()
	if !strings.Contains(msg, "无法解析") && !strings.Contains(msg, "非法") {
		t.Errorf("错误信息应指向协议解析失败, 而不是被当成断流: %q", msg)
	}
	if strings.Contains(msg, "断流") {
		t.Errorf("一帧都没解析成功不应报成断流 (处置方式不同): %q", msg)
	}
}

// TestFault_PartialMalformedFramesTolerated 钉住**容忍边界**。
//
// 与上一条相反：流里混进了两帧非法 data，但后面是一段完整合法的回复。
// 这必须判成功 —— 真实网关会往 SSE 里混心跳行和注释，一遇到坏帧就中断
// 会把可用的流打断。
//
// 这条与上一条合起来，才把"协议错误"的判据钉成两句话：**一帧都不通**是故障，
// **个别帧不通但流完整**不是。只有前一条会允许实现"见坏帧就丢、最后什么都不报"；
// 只有这一条会允许实现"见坏帧就中止",把正常上游打断。
func TestFault_PartialMalformedFramesTolerated(t *testing.T) {
	fm := harnesstest.New(harnesstest.BadJSONThenComplete())
	r := faultRunner(t, fm, 2)

	res := faultTurn(t, r, 5*time.Second)

	if res.Terminal.Reason != "completed" {
		t.Fatalf("个别坏帧但流完整, 应判成功, 实际 %q (err=%v)",
			res.Terminal.Reason, res.Terminal.Error)
	}
	var text strings.Builder
	for _, b := range res.Blocks {
		if b.Type == types.ContentBlockText {
			text.WriteString(b.Text)
		}
	}
	if !strings.Contains(text.String(), "好的") {
		t.Errorf("坏帧不该影响正常正文, 实际 %q", text.String())
	}
}

// TestFault_MessageStopOnlyIsComplete 钉住一个**反方向**的判据。
//
// 上面几条说"不完整要报错"，这条说"完整的不许报错"。
//
// 没有它，一个只认 message_delta 的实现能通过上面全部测试，却在真实环境里
// 把每一条只发 message_stop 的流都判成断流 —— 这正是我第一版修复犯的错
// （pkg/llmgw 的既有夹具当场变红）。协议里两种收尾都合法。
func TestFault_MessageStopOnlyIsComplete(t *testing.T) {
	fm := harnesstest.New(harnesstest.TextStopOnly("好的"))
	r := faultRunner(t, fm, 2)

	res := faultTurn(t, r, 5*time.Second)

	if res.Terminal.Reason != "completed" {
		t.Fatalf("只发 message_stop 的合法收尾被误判: reason=%q err=%v",
			res.Terminal.Reason, res.Terminal.Error)
	}
	if len(res.Blocks) == 0 {
		t.Error("应带回正文")
	}
}

// TestFault_CleanEOFBeforeStopIsModelError 钉住最隐蔽的一种截断。
//
// 与上一条的区别在传输层：这里 HTTP 响应**正常结束**，scanner 没有报错，
// 只是没有任何收口帧。网关正常关闭连接、或上游把响应截短了，都是这个形状。
//
// 必须单独测：我第一版修复只写了"scanner.Err() != nil"和"sawStopReason"两条，
// 用 hijack 硬断开的用例被前者抓住，看起来全绿；但一个只靠 scanner.Err() 的
// 实现放到干净 EOF 上就完全失效 —— 变异测试当场证实了这一点（删掉 sawStopReason
// 判据后，硬断流用例仍然绿）。
func TestFault_CleanEOFBeforeStopIsModelError(t *testing.T) {
	fm := harnesstest.New(harnesstest.CleanEOF(3))
	r := faultRunner(t, fm, 2)

	res := faultTurn(t, r, 5*time.Second)

	if res.Terminal.Reason == "completed" {
		t.Fatalf("干净 EOF 断流被当成正常完成: blocks=%d —— 传输层没有错误, 只能靠收口帧判断",
			len(res.Blocks))
	}
	if res.Terminal.Reason != "model_error" {
		t.Errorf("干净 EOF 断流的终态应为 model_error, 实际 %q (err=%v)",
			res.Terminal.Reason, res.Terminal.Error)
	}
	if res.Terminal.Error == nil {
		t.Error("必须带 error, 否则业务层无法区分断流与正常完成")
	}
}

// TestFault_ClientErrorIsModelError 是**正对照**：4xx 现在就走对了。
//
// 没有这条，上面两条的失败可能被误读成"这个夹具根本不工作"。它同时钉住一个
// 容易回归的点：4xx 不可重试，必须只请求上游一次。
func TestFault_ClientErrorIsModelError(t *testing.T) {
	fm := harnesstest.New(harnesstest.HTTPError(400))
	r := faultRunner(t, fm, 2)

	res := faultTurn(t, r, 5*time.Second)

	if res.Terminal.Reason != "model_error" {
		t.Errorf("4xx 终态应为 model_error, 实际 %q", res.Terminal.Reason)
	}
	if res.Terminal.Error == nil || !strings.Contains(res.Terminal.Error.Error(), "400") {
		t.Errorf("4xx 的 error 应携带状态码, 实际 %v", res.Terminal.Error)
	}
	if fm.CallCount() != 1 {
		t.Errorf("4xx 不可重试, 应只请求 1 次, 实际 %d 次", fm.CallCount())
	}
}

// TestFault_HappyPathStillCompletes 是另一半正对照：
// 修掉上面两条缺陷之后，正常回复必须仍然是 completed 且带正文。
//
// 只加"坏输入必须报错"的断言是危险的 —— 一个把**所有**流都判成失败的实现
// 同样能通过前三条。这条保证失败判据没有把正常路径一起吃掉。
func TestFault_HappyPathStillCompletes(t *testing.T) {
	fm := harnesstest.New(harnesstest.Text("好的"))
	r := faultRunner(t, fm, 2)

	res := faultTurn(t, r, 5*time.Second)

	if res.Terminal.Reason != "completed" {
		t.Fatalf("正常回复应为 completed, 实际 %q (err=%v)", res.Terminal.Reason, res.Terminal.Error)
	}
	if len(res.Blocks) == 0 {
		t.Fatal("正常回复应带回正文块")
	}
	var text strings.Builder
	for _, b := range res.Blocks {
		if b.Type == types.ContentBlockText {
			text.WriteString(b.Text)
		}
	}
	if !strings.Contains(text.String(), "好的") {
		t.Errorf("正文应包含上游回的文本, 实际 %q", text.String())
	}
	if res.Terminal.Error != nil {
		t.Errorf("正常完成不该带 error, 实际 %v", res.Terminal.Error)
	}
}

// TestFakeModel_RecordsRequestBytes 钉住替身自身可用：
// 业务层靠它断言"发出去的请求里有什么"，所以它必须真的记录请求体与头。
func TestFakeModel_RecordsRequestBytes(t *testing.T) {
	fm := harnesstest.New(harnesstest.Text("好"))
	r := faultRunner(t, fm, 2)

	faultTurn(t, r, 5*time.Second)

	calls := fm.Calls()
	if len(calls) != 1 {
		t.Fatalf("应对上游发 1 次请求, 实际 %d", len(calls))
	}
	c := calls[0]
	if c.Method != "POST" {
		t.Errorf("请求方法应为 POST, 实际 %s", c.Method)
	}
	if !strings.Contains(c.Body, "max_tokens") {
		t.Errorf("请求体应含 max_tokens 字段, 实际 %q", c.Body)
	}
	m, err := c.BodyJSON()
	if err != nil {
		t.Errorf("请求体应是合法 JSON: %v", err)
	}
	if _, ok := m["model"]; !ok {
		t.Errorf("请求体应含 model 字段, 实际键: %v", keysOf(m))
	}
}

// keysOf 让断言失败时能直接看到有哪些键，省去一次改测试再跑。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestClock_AdvancesWithoutSleeping 钉住 fake clock：
// 时间相关的契约（租约、退避、预算窗口）必须能靠 Advance 推进，
// 否则 CI 里全是 sleep，既慢又不稳。
func TestClock_AdvancesWithoutSleeping(t *testing.T) {
	clk := harnesstest.NewClock(time.Time{})
	start := clk.Now()
	if start.IsZero() {
		t.Fatal("零值起始时间应被换成非零值")
	}
	clk.Advance(3 * time.Hour)
	if got := clk.Now().Sub(start); got != 3*time.Hour {
		t.Errorf("Advance 3h 后应前进 3h, 实际 %v", got)
	}
}
