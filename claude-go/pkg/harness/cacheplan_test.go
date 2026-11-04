package harness

// R-H2 验收测试：从布局推导出的 api.CachePlan 必须真的落在出站请求上。
//
// 这里刻意不复用 pkg/api 的内部断言，而是起一个假上游读**原始请求体**：
// 只在内存里断言 plan 结构，无法区分"计划算对了"和"计划算对了但 api 层没采纳"。
// 后者正是本改动要消灭的失效方向 —— 请求照常成功，缓存命中率却退回内置启发式，
// 调用方从任何返回值里都看不出来。
//
// 每条用例都对应 cacheplan.go 的一条语义要求，见各函数头注释的 (a)~(f)。

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

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// cpRecorder 记录假上游收到的请求体。
type cpRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (r *cpRecorder) handle(w http.ResponseWriter, req *http.Request) {
	body := make([]byte, 0, 4096)
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
	r.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\n")
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n")
}

func (r *cpRecorder) bodyAt(t *testing.T, i int) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.bodies) {
		t.Fatalf("假上游只收到 %d 个请求，取不到第 %d 个", len(r.bodies), i)
	}
	return r.bodies[i]
}

func (r *cpRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// cpNewServer 起假上游。
func cpNewServer(t *testing.T) (*httptest.Server, *cpRecorder) {
	t.Helper()
	rec := &cpRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handle))
	t.Cleanup(srv.Close)
	return srv, rec
}

// cpClient 构造一个启用缓存的客户端。PromptCacheMode 必须为 "on"：默认 "auto"
// 只在 Anthropic 端点启用，假上游是 127.0.0.1，不强制打开的话一个断点都不会打，
// 后面所有"某位置有/没有 cache_control"的断言都会退化成恒真。
func cpClient(srv *httptest.Server, plan *api.CachePlan) *api.Client {
	return &api.Client{
		BaseURL:         srv.URL,
		APIKey:          "k",
		Model:           "m",
		RetryCount:      0,
		PromptCacheMode: "on",
		Client:          &http.Client{Timeout: 5 * time.Second},
		CachePlan:       plan,
	}
}

// cpMsgs 返回一份有三条消息的历史，供消息侧内置启发式打点。
func cpMsgs() []types.APIMessage {
	return []types.APIMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"开始"}]`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"好"}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"继续"}]`)},
	}
}

// cpStream 走流式路径并排空事件通道，返回错误。
func cpStream(t *testing.T, c *api.Client, msgs []types.APIMessage, sys []string) error {
	t.Helper()
	evCh, errCh := c.StreamMessage(context.Background(), msgs, sys, nil, 64)
	for range evCh {
	}
	return <-errCh
}

// cpOutbound 是出站请求体的观测形态。
type cpOutbound struct {
	System []struct {
		Text         string                     `json:"text"`
		CacheControl map[string]json.RawMessage `json:"cache_control"`
	} `json:"system"`
	Messages []struct {
		Content []map[string]json.RawMessage `json:"content"`
	} `json:"messages"`
}

// cpParse 解析请求体；解析失败直接 Fatal（断言本身坏掉时不该静默）。
func cpParse(t *testing.T, raw []byte) cpOutbound {
	t.Helper()
	var out cpOutbound
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析出站请求体失败: %v\n%s", err, raw)
	}
	return out
}

// cpStable 构造 n 个稳定段，文本刻意可区分，便于断言断点确实落在稳定段上。
func cpStable(n int) []SegmentInput {
	out := make([]SegmentInput, n)
	for i := range out {
		out[i] = SegmentInput{
			ID:      fmt.Sprintf("stable-%d", i),
			Version: "v1",
			Text:    fmt.Sprintf("稳定段 %d", i),
		}
	}
	return out
}

// cpsSystem 把稳定段与动态段拼成出站 system 数组（engine 的分层顺序）。
func cpSystem(stable []SegmentInput, dynamicTexts []string) []string {
	out := make([]string, 0, len(stable)+len(dynamicTexts))
	for _, s := range stable {
		out = append(out, s.Text)
	}
	out = append(out, dynamicTexts...)
	return out
}

// ============================================================
// (a) 预算 <= 0 或输入非法 → nil + error
// ============================================================

// TestCachePlanFromLayoutInvalidInputReturnsError 一条都不许返回"看起来能用"的计划。
//
// 静默产出一份空计划或半份计划，会让配置错误伪装成正常运行：请求成功、
// 缓存收益归零，而调用方没有任何可观测信号。
func TestCachePlanFromLayoutInvalidInputReturnsError(t *testing.T) {
	cases := []struct {
		name    string
		stable  []SegmentInput
		dynamic int
		budget  int
	}{
		{name: "预算为零", stable: cpStable(2), dynamic: 1, budget: 0},
		{name: "预算为负", stable: cpStable(2), dynamic: 1, budget: -1},
		{name: "动态段个数为负", stable: cpStable(2), dynamic: -1, budget: DefaultMaxBreakpoints},
		{
			name: "预算超过 provider 上限",
			stable: cpStable(2), dynamic: 1, budget: DefaultMaxBreakpoints + 1,
		},
		{
			name: "稳定段 ID 重复",
			stable: []SegmentInput{
				{ID: "dup", Version: "v1", Text: "a"},
				{ID: "dup", Version: "v1", Text: "b"},
			},
			dynamic: 1, budget: DefaultMaxBreakpoints,
		},
		{
			// 预算装不下"稳定段末尾 1 个 + 消息侧内置预留"：硬塞会让总数超出
			// 调用方给的预算，调用方却从返回值里看不出来。
			name: "预算不足以为消息侧预留",
			stable: cpStable(2), dynamic: 1, budget: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := CachePlanFromLayout(tc.stable, tc.dynamic, tc.budget)
			if err == nil {
				t.Fatalf("非法输入必须返回错误，实际 plan=%+v", plan)
			}
			if plan != nil {
				t.Fatalf("非法输入不得返回计划，实际 %+v", plan)
			}
		})
	}
}

// TestCachePlanFromLayoutBudgetBoundIsProviderConstant 预算上限必须绑定
// DefaultMaxBreakpoints（layout.go 的 provider 硬上限），不是本文件另写的数字。
//
// 变异锚点：把 `> DefaultMaxBreakpoints` 改成 `> 4`，本用例依然绿；但把实现里的
// 常量换成别的数（比如 8）时，budget=DefaultMaxBreakpoints+1 就会放行，这条变红。
func TestCachePlanFromLayoutBudgetBoundIsProviderConstant(t *testing.T) {
	if _, err := CachePlanFromLayout(cpStable(3), 1, DefaultMaxBreakpoints); err != nil {
		t.Fatalf("恰好等于上限的预算应被接受: %v", err)
	}
	if _, err := CachePlanFromLayout(cpStable(3), 1, DefaultMaxBreakpoints+1); err == nil {
		t.Fatalf("超过 DefaultMaxBreakpoints 的预算应报错")
	}
	if plan, err := CachePlanFromLayout(cpStable(3), 1, DefaultMaxBreakpoints+1); err == nil {
		t.Fatalf("超过 DefaultMaxBreakpoints 的预算应报错, 实得 plan=%+v", plan)
	} else if !strings.Contains(err.Error(), "上限") {
		t.Errorf("超限错误应说明超过了 provider 上限, 实得: %v", err)
	}
}

func mustErr(t *testing.T, plan *api.CachePlan, err error) string {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误，实际 plan=%+v", plan)
	}
	return err.Error()
}

// ============================================================
// (b) 稳定段为空 → nil（不干预），不是空切片的计划
// ============================================================

// TestCachePlanFromLayoutEmptyStableReturnsNil 稳定段为空时没有可缓存的前缀。
//
// 必须返回 nil（交给内置启发式），不能返回 &api.CachePlan{SystemIndexes: []int{}}：
// 后者是"system 侧明确不要断点"，会连内置启发式一起关掉。两者在请求体上的
// 差别是"一个 cache_control 都没有" vs "按内置启发式打点"，语义完全不同。
func TestCachePlanFromLayoutEmptyStableReturnsNil(t *testing.T) {
	for _, stable := range [][]SegmentInput{nil, {}} {
		plan, err := CachePlanFromLayout(stable, 3, DefaultMaxBreakpoints)
		if err != nil {
			t.Fatalf("稳定段为空不应报错: %v", err)
		}
		if plan != nil {
			t.Fatalf("稳定段为空必须返回 nil（不干预），实际 %+v；空切片的 CachePlan 会关掉内置断点", plan)
		}
	}
}

// TestCachePlanFromLayoutEmptyStableStillUsesBuiltin 是从请求体一侧钉住 (b)：
// 空稳定段的返回值（nil）赋给 Client 后，出站请求必须仍是内置启发式的形态。
//
// 只断言返回值 == nil 不足以区分"返回 nil"与"返回一个被 api 层当成 nil 处理的
// 空计划"（比如 SystemIndexes 为 nil 的 &CachePlan{}）。后者今天恰好等价，
// 但一旦 api 层改变 nil 计划的处理，语义就会悄悄漂移。
func TestCachePlanFromLayoutEmptyStableStillUsesBuiltin(t *testing.T) {
	srv, rec := cpNewServer(t)
	plan, err := CachePlanFromLayout(nil, 2, DefaultMaxBreakpoints)
	if err != nil {
		t.Fatalf("稳定段为空不应报错: %v", err)
	}
	// 动态段只有 2 块，没有稳定段：system 只有动态内容。
	sys := []string{"本轮任务 0", "本轮任务 1"}
	if err := cpStream(t, cpClient(srv, plan), cpMsgs(), sys); err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	out := cpParse(t, rec.bodyAt(t, 0))
	if out.System[0].CacheControl == nil {
		t.Errorf("nil 计划应回退到内置启发式（system[0] 打点），实际没有")
	}
}

// ============================================================
// (c) system 断点必须落在稳定段范围内
// ============================================================

// TestCachePlanFromLayoutBreakpointInsideStable 任何稳定段个数下，system 断点
// 下标都必须 < len(stable)，即严格落在稳定段范围内、绝不触及动态段。
//
// 指到动态段是**静默**失效：请求成功、断点也在，但每轮动态内容一变缓存键就变，
// 命中率永远是 0，任何返回值都看不出来。
func TestCachePlanFromLayoutBreakpointInsideStable(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5} {
		t.Run(fmt.Sprintf("stable=%d", n), func(t *testing.T) {
			stable := cpStable(n)
			plan, err := CachePlanFromLayout(stable, 2, DefaultMaxBreakpoints)
			if err != nil {
				t.Fatalf("合法输入报错: %v", err)
			}
			if plan == nil {
				t.Fatal("有稳定段时必须返回计划")
			}
			if len(plan.SystemIndexes) != 1 {
				t.Fatalf("应恰好给出 1 个 system 断点，实际 %v", plan.SystemIndexes)
			}
			idx := plan.SystemIndexes[0]
			if idx < 0 || idx >= len(stable) {
				t.Fatalf("system 断点下标 %d 不在稳定段 [0,%d) 内：落在动态段上会让缓存每轮失效且不报错",
					idx, len(stable))
			}
			// 从请求体一侧再确认：system 数组 = 稳定段 + 动态段，稳定段边界
			// 正好是动态段起点 len(stable)。下标 >= len(stable) 就是打到了动态段。
			if idx >= len(stable) {
				t.Fatalf("断点下标 %d 越过稳定段边界 %d", idx, len(stable))
			}
		})
	}
}

// ============================================================
// (d) 只在稳定段末尾放一个断点
// ============================================================

// TestCachePlanFromLayoutSingleBreakpointAtStableEnd 预算再大也只放一个断点，
// 且必须是最后一个稳定段。
//
// 理由（与 cacheplan.go 的实现注释一致）：断点覆盖它之前（含自身）的全部前缀，
// 打在最后一个稳定段上已经覆盖整个稳定前缀；更靠前的断点不会扩大覆盖，只会
// 制造更短、更易与主缓存条目重复的条目。撒满预算等于放弃"有依据的布局"。
func TestCachePlanFromLayoutSingleBreakpointAtStableEnd(t *testing.T) {
	stable := cpStable(4)
	for _, budget := range []int{3, 4} {
		plan, err := CachePlanFromLayout(stable, 3, budget)
		if err != nil {
			t.Fatalf("预算 %d 报错: %v", budget, err)
		}
		if len(plan.SystemIndexes) != 1 {
			t.Fatalf("预算 %d: 应只放 1 个断点，实际 %v", budget, plan.SystemIndexes)
		}
		if got, want := plan.SystemIndexes[0], len(stable)-1; got != want {
			t.Fatalf("预算 %d: 断点应在最后一个稳定段 %d，实际 %d", budget, want, got)
		}
	}
}

// ============================================================
// (f) MessageIndexes 一律 nil（交回内置启发式），不是空切片
// ============================================================

// TestCachePlanFromLayoutMessageSideIsNil MessageIndexes 必须是 nil。
//
// 不能返回 []int{}：nil = "消息侧用内置启发式"，[]int{} = "消息侧一个断点都不要"。
// 本函数没有消息内容，无法负责任地选点；返回空切片会静默关掉内置的两级消息
// 断点，历史增长部分重新按未命中计费，而请求仍成功、没有任何报错。
func TestCachePlanFromLayoutMessageSideIsNil(t *testing.T) {
	for _, n := range []int{1, 3} {
		plan, err := CachePlanFromLayout(cpStable(n), 1, DefaultMaxBreakpoints)
		if err != nil {
			t.Fatalf("合法输入报错: %v", err)
		}
		if plan.MessageIndexes != nil {
			t.Fatalf("stable=%d: MessageIndexes 必须为 nil（交回内置），实际 %#v", n, plan.MessageIndexes)
		}
	}
}

// ============================================================
// (e) 总数 <= 预算，且计划被 api.Client 接受并落到出站请求
// ============================================================

// TestCachePlanFromLayoutDrivesOutboundRequest 是本文件的端到端验收：
//
//   - 计划真的被 api.Client 接受（没有触发超限/非法下标校验）；
//   - 出站请求体里 cache_control 恰好落在最后一个稳定段上，动态段一个都没有；
//   - 消息侧仍按内置启发式打点（证明 MessageIndexes 的 nil 没有被当成"关闭"）；
//   - 实际断点总数 <= 预算。
func TestCachePlanFromLayoutDrivesOutboundRequest(t *testing.T) {
	stable := cpStable(2)
	dynamic := []string{"本轮任务", "本轮临时上下文"}
	budget := DefaultMaxBreakpoints

	plan, err := CachePlanFromLayout(stable, len(dynamic), budget)
	if err != nil {
		t.Fatalf("推导计划失败: %v", err)
	}
	if plan == nil {
		t.Fatal("有稳定段时必须返回计划")
	}

	srv, rec := cpNewServer(t)
	if err := cpStream(t, cpClient(srv, plan), cpMsgs(), cpSystem(stable, dynamic)); err != nil {
		t.Fatalf("计划被 api.Client 拒绝或请求失败: %v", err)
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("应恰好发出 1 个请求，实际 %d", n)
	}
	out := cpParse(t, rec.bodyAt(t, 0))

	if got, want := len(out.System), len(stable)+len(dynamic); got != want {
		t.Fatalf("system 块数: got %d, want %d", got, want)
	}

	// 最后一个稳定段（下标 len(stable)-1）必须有断点，且文本确实是稳定段内容。
	wantIdx := len(stable) - 1
	if out.System[wantIdx].CacheControl == nil {
		t.Errorf("system[%d]（最后一个稳定段）应带 cache_control，实际没有", wantIdx)
	}
	if out.System[wantIdx].Text != stable[wantIdx].Text {
		t.Errorf("system[%d] 文本 %q 不是稳定段 %q：断点落错了位置",
			wantIdx, out.System[wantIdx].Text, stable[wantIdx].Text)
	}
	// 其余稳定段不额外打点（(d)：不撒预算）。
	for i := 0; i < wantIdx; i++ {
		if out.System[i].CacheControl != nil {
			t.Errorf("system[%d] 是更靠前的稳定段，不应额外打点", i)
		}
	}
	// 动态段绝对不能有断点 (c)。
	for i := len(stable); i < len(out.System); i++ {
		if out.System[i].CacheControl != nil {
			t.Errorf("system[%d] 是动态段，断点落到这里会让缓存每轮失效且不报错", i)
		}
	}
	// 消息侧 nil → 内置启发式仍在工作，(f) 的反向证明。
	msgMarks := 0
	for i, m := range out.Messages {
		if len(m.Content) == 0 {
			continue
		}
		if _, ok := m.Content[len(m.Content)-1]["cache_control"]; ok {
			msgMarks++
		}
		_ = i
	}
	if msgMarks == 0 {
		t.Errorf("消息侧断点被关掉了：MessageIndexes 若返回空切片就会出现这种静默失效")
	}

	// (e) 总数必须 <= 预算。
	total := len(plan.SystemIndexes) + msgMarks
	if total > budget {
		t.Fatalf("断点总数 %d 超过预算 %d", total, budget)
	}
}

// TestCachePlanFromLayoutExactlyMinimumBudgetAccepted 最小可用预算
// （稳定段末尾 1 + 消息侧预留 2）必须能被 api.Client 接受。
//
// 这两条一起锁住预算的算法边界：预算低于 3 时本函数报错（见 (a)），等于 3 时
// 计划必须真的能发出去。把预留 2 删掉，下面的 budget=3 用例仍绿，但 (a) 里
// "预算 1 应报错"会变红 —— 两条合起来才能钉死这个数。
func TestCachePlanFromLayoutExactlyMinimumBudgetAccepted(t *testing.T) {
	stable := cpStable(2)
	plan, err := CachePlanFromLayout(stable, 1, 3)
	if err != nil {
		t.Fatalf("最小预算 3 应被接受: %v", err)
	}
	srv, rec := cpNewServer(t)
	if err := cpStream(t, cpClient(srv, plan), cpMsgs(), cpSystem(stable, []string{"本轮任务"})); err != nil {
		t.Fatalf("预算 3 的计划应被 api.Client 接受，实际: %v", err)
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("应恰好发出 1 个请求，实际 %d", n)
	}
}

// ============================================================
// 负对照：没有计划时断点位置必须与计划不同
// ============================================================

// TestCachePlanFromLayoutNegativeControlWithoutPlan 同一个输入不传 CachePlan 时，
// 内置启发式给出的 system 断点集合必须与我们的计划**不同**。
//
// 如果两者恰好相同，端到端用例只能证明"请求没崩"，证明不了"计划生效"。
// 这里选的形状（稳定段 2 + 动态段 2）下，内置启发式按"最后一块是变量、退一格"
// 的假设会打在 system[0] 与 system[2] —— 后者正是动态段，也就是本函数要避免的
// 那种静默失效；而计划只打 system[1]。
func TestCachePlanFromLayoutNegativeControlWithoutPlan(t *testing.T) {
	stable := cpStable(2)
	dynamic := []string{"本轮任务", "本轮临时上下文"}
	sys := cpSystem(stable, dynamic)

	plan, err := CachePlanFromLayout(stable, len(dynamic), DefaultMaxBreakpoints)
	if err != nil {
		t.Fatalf("推导计划失败: %v", err)
	}

	// 有计划的请求。
	srvPlanned, recPlanned := cpNewServer(t)
	if err := cpStream(t, cpClient(srvPlanned, plan), cpMsgs(), sys); err != nil {
		t.Fatalf("有计划的请求失败: %v", err)
	}
	planned := cpParse(t, recPlanned.bodyAt(t, 0))

	// 同一输入、不传计划（nil → 内置启发式）。
	srvBuiltin, recBuiltin := cpNewServer(t)
	if err := cpStream(t, cpClient(srvBuiltin, nil), cpMsgs(), sys); err != nil {
		t.Fatalf("无计划的请求失败: %v", err)
	}
	builtin := cpParse(t, recBuiltin.bodyAt(t, 0))

	plannedSet := map[int]bool{}
	for i := range planned.System {
		if planned.System[i].CacheControl != nil {
			plannedSet[i] = true
		}
	}
	builtinSet := map[int]bool{}
	for i := range builtin.System {
		if builtin.System[i].CacheControl != nil {
			builtinSet[i] = true
		}
	}

	if !sameIndexSet(plannedSet, builtinSet) {
		return // 期望路径：两者不同，证明计划确实改变了出站请求。
	}
	t.Fatalf("计划与内置启发式的 system 断点位置相同 %v：本用例无法证明计划生效", plannedSet)
}

// sameIndexSet 比较两个下标集合。
func sameIndexSet(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
