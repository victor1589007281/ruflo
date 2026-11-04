package api

// R-H2 支撑能力的验收测试: 调用方可以把一份「缓存断点计划」交给 api.Client,
// 让 harness 的 PromptLayout 真正驱动出站请求, 而不是只当观测。
//
// 这个文件锁三件事, 三者缺一都会让这个改动的价值归零:
//
//  1. 零值零影响: CachePlan == nil 时生成的请求体必须与改动前**逐字节相同**,
//     并且与"显式传一个零值 CachePlan"也逐字节相同。后者才是"零值 = 沿用内置"
//     这句话的可执行定义 —— 只测 nil 的话, 一个把零值计划当成"全部关掉"的实现
//     照样能绿。
//  2. 超限在发送前失败: 断点总数 (含另一侧由内置补上的) 超过 MaxCacheBreakpoints
//     时必须在构造请求前报错。用假上游计数断言收到的请求数是 0 —— 只断言 err != nil
//     无法区分"发出去被拒"和"根本没发"。
//  3. 非法下标报错: 越界/负数/重复都必须显式失败。静默跳过会让调用方以为断点生效了,
//     而实际缓存收益为零 —— 没有任何报错、没有任何指标, 只有账单。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// planRecorder 记录假上游收到的每一个请求体。
//
// 用互斥锁而不是 atomic 存 body: 流式路径在 goroutine 里发请求, 断言前虽然已经
// 等到 channel 关闭 (happens-before), 但把假设写在锁上比写在"我记得这里已经同步过"
// 上便宜。
type planRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
	paths  []string
	stream bool
}

func (r *planRecorder) handle(w http.ResponseWriter, req *http.Request) {
	body := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := req.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	r.mu.Lock()
	r.bodies = append(r.bodies, body)
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()

	if !r.stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":11,"output_tokens":7}}`))
		return
	}

	// 最小合法 SSE: 必须带 message_delta(stop_reason), 否则 StreamMessage 的
	// 流完整性判据 (见 client.go 的收尾判据) 会把这条测试流判成断流 ——
	// 那会让所有流式用例因为一个与断点无关的原因变红。
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\n")
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n")
}

// bodyAt 返回第 i 个请求体; 越界直接 Fatal (断言本身写错时不该静默).
func (r *planRecorder) bodyAt(t *testing.T, i int) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.bodies) {
		t.Fatalf("假上游只收到 %d 个请求, 取不到第 %d 个", len(r.bodies), i)
	}
	return r.bodies[i]
}

func (r *planRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// newPlanServer 起一个假上游。stream 决定它回 SSE 还是普通 JSON。
func newPlanServer(t *testing.T, stream bool) (*httptest.Server, *planRecorder) {
	t.Helper()
	rec := &planRecorder{stream: stream}
	srv := httptest.NewServer(http.HandlerFunc(rec.handle))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newPlanClient 构造一个启用缓存的测试客户端。
//
// PromptCacheMode = "on" 是必须的: 默认 "auto" 只在 Anthropic 端点启用, 而假上游
// 是 127.0.0.1。不强制打开的话, 所有断点都不会打, 零值对比测试会变成
// "两个空请求体相同" 的恒真断言 —— 那正是本仓反复踩过的"负对照无分辨"。
func newPlanClient(srv *httptest.Server, plan *CachePlan) *Client {
	c := &Client{
		BaseURL:         srv.URL,
		APIKey:          "k",
		Model:           "m",
		RetryCount:      0,
		PromptCacheMode: "on",
		Client:          &http.Client{Timeout: 5 * time.Second},
		CachePlan:       plan,
	}
	return c
}

// planMsgs 每次都重新构造一份消息历史。
//
// tool_use 的 input 刻意用非字典序键序: 它是"打点是否动了别的字节"的敏感探针
// (见 zzbp_test.go 的事故复盘)。如果哪天有人把 markCacheBreakpointAt 改回
// map 往返, 零值对比会立刻在这一串字节上炸掉。
func planMsgs() []types.APIMessage {
	return []types.APIMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"开始"}]`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"tu1","name":"Write","input":{"path":"/w.py","contents":"x"}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"写好了"}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"继续"}]`)},
	}
}

// planSys 是三段式 system, 与 engine 的 base → memory → task 分层同形。
func planSys() []string {
	return []string{"base 稳定", "memory 稳定段", "本轮任务"}
}

// runSend 走非流式路径, 返回 err。
func runSend(t *testing.T, c *Client, msgs []types.APIMessage, sys []string) error {
	t.Helper()
	_, err := c.SendMessage(context.Background(), msgs, sys, nil, 64)
	return err
}

// runStream 走流式路径并等它跑完, 返回首个非 nil 错误。
//
// 必须把 eventCh 排空: StreamMessage 在同一个 goroutine 里先写 eventCh 再写 errCh,
// 不排空会让它在缓冲区满时卡住 (100 个事件的缓冲对本用例够, 但依赖缓冲量是
// 隐性契约)。
func runStream(t *testing.T, c *Client, msgs []types.APIMessage, sys []string) error {
	t.Helper()
	evCh, errCh := c.StreamMessage(context.Background(), msgs, sys, nil, 64)
	for range evCh {
	}
	return <-errCh
}

// ============================================================
// 不变量 1: 零值零影响
// ============================================================

// TestCachePlanZeroValueBytesIdentical_NonStream 非流式路径: nil 计划与零值计划
// 必须生成逐字节相同的请求体。
func TestCachePlanZeroValueBytesIdentical_NonStream(t *testing.T) {
	srv, rec := newPlanServer(t, false)

	if err := runSend(t, newPlanClient(srv, nil), planMsgs(), planSys()); err != nil {
		t.Fatalf("nil 计划请求失败: %v", err)
	}
	if err := runSend(t, newPlanClient(srv, &CachePlan{}), planMsgs(), planSys()); err != nil {
		t.Fatalf("零值计划请求失败: %v", err)
	}

	base := rec.bodyAt(t, 0)
	zero := rec.bodyAt(t, 1)
	if string(base) != string(zero) {
		t.Fatalf("零值 CachePlan 改变了请求体\nnil  : %s\n零值 : %s", base, zero)
	}
	if !strings.Contains(string(base), `"cache_control"`) {
		t.Fatalf("前置条件失败: 请求体里没有任何 cache_control, 对比恒真: %s", base)
	}
}

// TestCachePlanZeroValueBytesIdentical_Stream 流式路径同上。
func TestCachePlanZeroValueBytesIdentical_Stream(t *testing.T) {
	srv, rec := newPlanServer(t, true)

	if err := runStream(t, newPlanClient(srv, nil), planMsgs(), planSys()); err != nil {
		t.Fatalf("nil 计划流式请求失败: %v", err)
	}
	if err := runStream(t, newPlanClient(srv, &CachePlan{}), planMsgs(), planSys()); err != nil {
		t.Fatalf("零值计划流式请求失败: %v", err)
	}

	base := rec.bodyAt(t, 0)
	zero := rec.bodyAt(t, 1)
	if string(base) != string(zero) {
		t.Fatalf("零值 CachePlan 改变了流式请求体\nnil  : %s\n零值 : %s", base, zero)
	}
	if !strings.Contains(string(base), `"cache_control"`) {
		t.Fatalf("前置条件失败: 请求体里没有任何 cache_control, 对比恒真: %s", base)
	}
}

// TestCachePlanNilUsesBuiltin 把 nil 计划的断点布局钉死成内置启发式的结果。
//
// 零值对比只证明"两个请求一致", 无法证明"一致在正确的位置"; 万一有人把内置
// 启发式改了, 对比测试依然全绿而行为已经漂了。这里单独钉一次位置。
func TestCachePlanNilUsesBuiltin(t *testing.T) {
	srv, rec := newPlanServer(t, false)
	if err := runSend(t, newPlanClient(srv, nil), planMsgs(), planSys()); err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	var body struct {
		System   []map[string]json.RawMessage `json:"system"`
		Messages []struct {
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.bodyAt(t, 0), &body); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}

	// 非流式内置只在 system block 0 打点 (见 buildSystemParamNonStream)。
	if _, ok := body.System[0]["cache_control"]; !ok {
		t.Error("内置启发式应在 system[0] 打点")
	}
	for i := 1; i < len(body.System); i++ {
		if _, ok := body.System[i]["cache_control"]; ok {
			t.Errorf("非流式内置不应在 system[%d] 打点", i)
		}
	}
	// 非流式内置在消息侧一个断点都不打 (既有行为, 本次改动不得顺手补上)。
	for i, m := range body.Messages {
		if len(m.Content) == 0 {
			continue
		}
		if _, ok := m.Content[len(m.Content)-1]["cache_control"]; ok {
			t.Errorf("非流式内置不应在 messages[%d] 打点", i)
		}
	}
}

// TestCachePlanEmptySliceDisablesSide 空切片 (非 nil) 与 nil 语义不同:
// 空切片是"这一侧一个断点都不要", 必须能真正关掉内置默认值。
func TestCachePlanEmptySliceDisablesSide(t *testing.T) {
	srv, rec := newPlanServer(t, true)
	plan := &CachePlan{SystemIndexes: []int{}, MessageIndexes: []int{}}
	if err := runStream(t, newPlanClient(srv, plan), planMsgs(), planSys()); err != nil {
		t.Fatalf("空计划请求失败: %v", err)
	}
	raw := string(rec.bodyAt(t, 0))
	if strings.Contains(raw, `"cache_control"`) {
		t.Fatalf("非 nil 空切片应关掉该侧全部断点, 请求体却仍带 cache_control: %s", raw)
	}
}

// ============================================================
// 计划确实驱动了出站请求
// ============================================================

// TestCachePlanDrivesBreakpoints 计划给出的下标必须原样落到请求体上:
// 指定的位置有 cache_control, 没指定的位置没有。
func TestCachePlanDrivesBreakpoints(t *testing.T) {
	srv, rec := newPlanServer(t, true)
	plan := &CachePlan{
		SystemIndexes:  []int{0, 2},
		MessageIndexes: []int{1, 3},
	}
	if err := runStream(t, newPlanClient(srv, plan), planMsgs(), planSys()); err != nil {
		t.Fatalf("请求失败: %v", err)
	}

	var body struct {
		System   []map[string]json.RawMessage `json:"system"`
		Messages []struct {
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.bodyAt(t, 0), &body); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}

	wantSys := map[int]bool{0: true, 2: true}
	for i, blk := range body.System {
		_, got := blk["cache_control"]
		if got != wantSys[i] {
			t.Errorf("system[%d] 的 cache_control: got %v, want %v", i, got, wantSys[i])
		}
	}
	wantMsg := map[int]bool{1: true, 3: true}
	for i, m := range body.Messages {
		last := m.Content[len(m.Content)-1]
		_, got := last["cache_control"]
		if got != wantMsg[i] {
			t.Errorf("messages[%d] 的 cache_control: got %v, want %v", i, got, wantMsg[i])
		}
	}
	if n := rec.count(); n != 1 {
		t.Errorf("应恰好发出 1 个请求, 实际 %d", n)
	}
}

// TestCachePlanExactlyAtLimitAccepted 恰好 4 个断点是合法的 —— 上限是 <=4,
// 不是 <4。这条用例是"把 > 改成 >= 就会变红"的变异锚点。
func TestCachePlanExactlyAtLimitAccepted(t *testing.T) {
	// 流式: system 计划 2 个 + messages 计划 2 个 = 4。
	srv, rec := newPlanServer(t, true)
	streamPlan := &CachePlan{SystemIndexes: []int{0, 1}, MessageIndexes: []int{3, 4}}
	if err := runStream(t, newPlanClient(srv, streamPlan), planMsgs(), planSys()); err != nil {
		t.Fatalf("流式: 恰好 4 个断点应被接受, 实际报错: %v", err)
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("流式: 应恰好发出 1 个请求, 实际 %d", n)
	}

	// 非流式: system 计划 3 个 + messages 计划 1 个 = 4 (非流式内置 system 恒 1,
	// 消息侧内置 0, 所以总数算的是计划侧, 见 validateCachePlanPath)。
	srvN, recN := newPlanServer(t, false)
	sendPlan := &CachePlan{SystemIndexes: []int{0, 1, 2}, MessageIndexes: []int{4}}
	if err := runSend(t, newPlanClient(srvN, sendPlan), planMsgs(), planSys()); err != nil {
		t.Fatalf("非流式: 恰好 4 个断点应被接受, 实际报错: %v", err)
	}
	if n := recN.count(); n != 1 {
		t.Fatalf("非流式: 应恰好发出 1 个请求, 实际 %d", n)
	}
}

// ============================================================
// 不变量 2: 超上限在发送前失败 (请求数必须为 0)
// ============================================================

// TestCachePlanOverLimitFailsBeforeRequest 超限必须在发送前失败, 两条路径分别断言。
//
// 两套用例不能合并: 内置形态在两条路径上不同 (流式 system 打 2 个、消息侧打 2 个;
// 非流式 system 打 1 个、消息侧 0 个), 同一份计划在一侧合法、另一侧超限是常态。
// 用同一套期望会把"路径感知"这个正确性要求压平, 于是回退侧算错也测不出来。
func TestCachePlanOverLimitFailsBeforeRequest(t *testing.T) {
	streamCases := []struct {
		name string
		plan *CachePlan
	}{
		{
			// 计划只给了 system 一侧 3 个, 但 messages 侧为 nil → 内置会补 2 个,
			// 总数 5。只数计划里的下标会得出 3 (合法), 把它发出去就是上游 400。
			name: "system 计划 3 个 + messages 内置 2 个",
			plan: &CachePlan{SystemIndexes: []int{0, 1, 2}},
		},
		{
			name: "messages 计划 3 个 + system 内置 2 个",
			plan: &CachePlan{MessageIndexes: []int{0, 1, 2}},
		},
		{
			name: "两侧都超限",
			plan: &CachePlan{SystemIndexes: []int{0, 1, 2}, MessageIndexes: []int{0, 1, 2}},
		},
		{
			// 5 个消息下标全给上: system 会被内置补 2 个, 总数 7。
			name: "messages 给满",
			plan: &CachePlan{MessageIndexes: []int{0, 1, 2, 3, 4}},
		},
	}
	for _, tc := range streamCases {
		t.Run("stream/"+tc.name, func(t *testing.T) {
			srv, rec := newPlanServer(t, true)
			err := runStream(t, newPlanClient(srv, tc.plan), planMsgs(), planSys())
			if err == nil {
				t.Fatalf("超限计划应返回错误 (请求数 %d)", rec.count())
			}
			assertBreakpointLimitError(t, err)
			if n := rec.count(); n != 0 {
				t.Fatalf("超限计划不得发出请求, 假上游收到 %d 个", n)
			}
		})
	}

	overSend := []struct {
		name string
		plan *CachePlan
	}{
		{
			// 3 + 2 = 5: 非流式下两侧都是计划侧 (内置不会再补), 仍超限。
			name: "system 计划 3 个 + messages 计划 2 个",
			plan: &CachePlan{SystemIndexes: []int{0, 1, 2}, MessageIndexes: []int{0, 1}},
		},
		{
			// 4 + 内置 1 = 5。
			name: "messages 计划 4 个 + system 内置 1 个",
			plan: &CachePlan{MessageIndexes: []int{0, 1, 2, 3}},
		},
	}
	for _, tc := range overSend {
		t.Run("nonstream/"+tc.name, func(t *testing.T) {
			srv, rec := newPlanServer(t, false)
			err := runSend(t, newPlanClient(srv, tc.plan), planMsgs(), planSys())
			if err == nil {
				t.Fatalf("超限计划应返回错误 (请求数 %d)", rec.count())
			}
			assertBreakpointLimitError(t, err)
			if n := rec.count(); n != 0 {
				t.Fatalf("超限计划不得发出请求, 假上游收到 %d 个", n)
			}
		})
	}
}

// TestCachePlanLimitIsPathAware 同一份计划 (system 3 个, messages nil) 在流式下
// 超限 (3 + 内置 2 = 5), 在非流式下合法 (3 + 内置 1 = 4)。
//
// 这条用例锁的是"回退侧断点数必须按路径分岔"这件事。两条路径共用一个公式时,
// 要么流式漏判 (发上游 400), 要么非流式误杀 (合法请求被本地拒掉) —— 后者更隐蔽,
// 因为它只表现为"某些调用方的请求突然全失败"。
func TestCachePlanLimitIsPathAware(t *testing.T) {
	plan := func() *CachePlan { return &CachePlan{SystemIndexes: []int{0, 1, 2}} }

	srvS, recS := newPlanServer(t, true)
	if err := runStream(t, newPlanClient(srvS, plan()), planMsgs(), planSys()); err == nil {
		t.Fatalf("流式: 3 + 内置 2 = 5 应超限 (请求数 %d)", recS.count())
	}
	if n := recS.count(); n != 0 {
		t.Fatalf("流式: 超限不得发请求, 实际 %d", n)
	}

	srvN, recN := newPlanServer(t, false)
	if err := runSend(t, newPlanClient(srvN, plan()), planMsgs(), planSys()); err != nil {
		t.Fatalf("非流式: 3 + 内置 1 = 4 应被接受, 实际报错: %v", err)
	}
	if n := recN.count(); n != 1 {
		t.Fatalf("非流式: 应发出 1 个请求, 实际 %d", n)
	}
}

// assertBreakpointLimitError 断言错误信息能让人看出"断点超限", 而不是一个裸的下标错误。
func assertBreakpointLimitError(t *testing.T, err error) {
	t.Helper()
	if !strings.Contains(err.Error(), "超过") || !strings.Contains(err.Error(), "断点") {
		t.Errorf("错误信息应说明断点超限, 实际: %v", err)
	}
}

// ============================================================
// 不变量 3: 非法下标必须报错, 不得静默跳过
// ============================================================

func TestCachePlanInvalidIndexes(t *testing.T) {
	cases := []struct {
		name    string
		plan    *CachePlan
		wantSub string
	}{
		{
			name:    "system 负数",
			plan:    &CachePlan{SystemIndexes: []int{-1}},
			wantSub: "负数",
		},
		{
			name:    "messages 负数",
			plan:    &CachePlan{MessageIndexes: []int{-2}},
			wantSub: "负数",
		},
		{
			name:    "system 越界",
			plan:    &CachePlan{SystemIndexes: []int{3}},
			wantSub: "越界",
		},
		{
			name:    "messages 越界",
			plan:    &CachePlan{MessageIndexes: []int{5}},
			wantSub: "越界",
		},
		{
			name:    "system 重复",
			plan:    &CachePlan{SystemIndexes: []int{1, 1}},
			wantSub: "重复",
		},
		{
			name:    "messages 重复",
			plan:    &CachePlan{MessageIndexes: []int{2, 2}},
			wantSub: "重复",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := newPlanServer(t, false)
			err := runSend(t, newPlanClient(srv, tc.plan), planMsgs(), planSys())
			if err == nil {
				t.Fatalf("非法下标必须报错, 实际通过 (请求数 %d)", rec.count())
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息应包含 %q, 实际: %v", tc.wantSub, err)
			}
			if n := rec.count(); n != 0 {
				t.Fatalf("非法计划不得发出请求, 假上游收到 %d 个", n)
			}

			srvS, recS := newPlanServer(t, true)
			errS := runStream(t, newPlanClient(srvS, tc.plan), planMsgs(), planSys())
			if errS == nil {
				t.Fatalf("流式路径: 非法下标必须报错 (请求数 %d)", recS.count())
			}
			if n := recS.count(); n != 0 {
				t.Fatalf("流式路径: 非法计划不得发出请求, 假上游收到 %d 个", n)
			}
		})
	}
}

// TestCachePlanInvalidIndexOnEmptyHistory 空历史下任何下标都越界 —— 包括 0。
// 这是最容易被"len(messages) > 0 才打点"的守卫掩盖掉的一类非法计划:
// 守卫让打点跳过, 于是错误计划看起来成功了。
func TestCachePlanInvalidIndexOnEmptyHistory(t *testing.T) {
	srv, rec := newPlanServer(t, false)
	plan := &CachePlan{MessageIndexes: []int{0}}
	err := runSend(t, newPlanClient(srv, plan), nil, planSys())
	if err == nil {
		t.Fatal("空历史上的 messages[0] 应报越界")
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("不得发出请求, 实际 %d", n)
	}
}

// TestValidateCachePlanDirect 直接锁校验函数本身 (导出入口的最小契约)。
func TestValidateCachePlanDirect(t *testing.T) {
	if err := (&Client{}).validateCachePlan(5, 3); err != nil {
		t.Fatalf("无计划时不应报错: %v", err)
	}
	c := &Client{CachePlan: &CachePlan{SystemIndexes: []int{0}, MessageIndexes: []int{0}}}
	if err := c.validateCachePlan(5, 3); err != nil {
		t.Fatalf("合法计划不应报错: %v", err)
	}
}

// TestCachePlanSurvivesModelClone 计划必须跨 WithModel 副本存活。
//
// engine 的相位路由每个执行回合都用 WithModel 造临时副本 —— 漏传播会让那部分
// 回合悄悄退回内置启发式。这是无声失效: 请求照常成功, 只是缓存命中率变了,
// 而调用方从任何返回值里都看不出来。
func TestCachePlanSurvivesModelClone(t *testing.T) {
	plan := &CachePlan{SystemIndexes: []int{0}, MessageIndexes: []int{4}}
	base := &Client{CachePlan: plan}

	clone := base.WithModel("other")
	if clone.CachePlan != plan {
		t.Fatalf("WithModel 副本丢失了 CachePlan")
	}
	configured := base.ConfiguredClone("http://example.invalid", "k", "m", nil)
	if configured.CachePlan != plan {
		t.Fatalf("ConfiguredClone 副本丢失了 CachePlan")
	}
	// 反向: 计划不该在 nil 的 Client 上凭空长出来。
	if (&Client{}).WithModel("x").CachePlan != nil {
		t.Fatal("无计划的 Client 克隆后不应有计划")
	}
}
