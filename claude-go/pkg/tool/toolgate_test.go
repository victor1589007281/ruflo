// toolgate_test.go — R-H9 三条执行侧元数据的验收（确认 / 幂等 / 超时）。
//
// 判据一律是"副作用发生了几次"或"调用方拿到了什么"，而不是"字段长什么样"：
// 元数据只是声明，R-H9 的验收原话是"需要确认的写工具在无确认时返回拒绝"、
// "同一幂等键重复调用返回同一结果" —— 都是行为断言。
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// gateTool 是一个可编程的测试工具：能声明确认/幂等/超时，并数执行次数。
type gateTool struct {
	name     string
	calls    atomic.Int64
	confirms bool
	idem     bool
	timeout  time.Duration
	// blocking 非 nil 时 Call 阻塞到它 close（用来制造超时）。
	blocking chan struct{}
	// sleep 是 Call 里的固定耗时。
	sleep time.Duration
	// result 是 Call 返回的内容；空则用默认值。
	result string
	// failTimes 前 N 次调用返回错误（用来验证失败不缓存）。
	failTimes atomic.Int64
	// errWithResult 让失败调用同时返回一份**非 nil** 的部分结果。
	//
	// 这条路径是"失败不缓存"唯一能被真正测到的形态：若失败时 res 恒为 nil，
	// 那么"只在成功时缓存"与"无条件缓存"的行为完全一样（后者存进去的是 nil，
	// 命中后再取出来仍是 nil），测试无法区分。
	errWithResult bool
}

func (g *gateTool) Name() string                           { return g.name }
func (g *gateTool) Description() string                    { return "gate test tool" }
func (g *gateTool) InputSchema() json.RawMessage           { return json.RawMessage(`{"type":"object"}`) }
func (g *gateTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (g *gateTool) IsReadOnly(json.RawMessage) bool        { return false }
func (g *gateTool) CheckPermissions(json.RawMessage, *ToolContext) *types.PermissionResult {
	return &types.PermissionResult{Behavior: types.PermissionAllow}
}

func (g *gateTool) NeedsConfirmation() bool { return g.confirms }
func (g *gateTool) Idempotent() bool        { return g.idem }
func (g *gateTool) Timeout() time.Duration  { return g.timeout }

func (g *gateTool) Call(ctx context.Context, _ json.RawMessage, _ *ToolContext) (*ToolResult, error) {
	g.calls.Add(1)
	if g.blocking != nil {
		select {
		case <-g.blocking:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if g.sleep > 0 {
		select {
		case <-time.After(g.sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if g.failTimes.Load() > 0 {
		g.failTimes.Add(-1)
		if g.errWithResult {
			// 真实工具确实会这样返回：扫了一半目录后权限不足、流断了但已拿到部分内容。
			return &ToolResult{Content: "部分结果", IsError: true}, errors.New("暂时性失败")
		}
		return nil, errors.New("暂时性失败")
	}
	content := g.result
	if content == "" {
		content = "ok"
	}
	// 内容里带调用序号: 幂等命中时调用方拿到的必须是**第一次**的那份。
	return &ToolResult{Content: content}, nil
}

// fixedConfirm 是固定答案的确认检查器。
type fixedConfirm struct {
	ok    bool
	calls atomic.Int64
}

func (f *fixedConfirm) IsConfirmed(string, json.RawMessage) bool {
	f.calls.Add(1)
	return f.ok
}

// execGate 跑一次 RunToolUse 并返回那个 tool_result 消息。
func execGate(t *testing.T, tl Tool, tctx *ToolContext) types.Message {
	t.Helper()
	reg := NewRegistry()
	reg.Register(tl)
	return RunToolUse(context.Background(), types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: tl.Name(),
		Input: json.RawMessage(`{"x":1}`),
	}, reg, tctx, nil)
}

// resultText 取出 tool_result 的文本与 is_error。
func resultText(t *testing.T, m types.Message) (text string, isErr bool) {
	t.Helper()
	for _, b := range m.Content {
		if b.Type == types.ContentBlockToolResult {
			return b.Content, b.IsError
		}
	}
	t.Fatalf("消息里没有 tool_result 块: %+v", m)
	return "", false
}

// ===========================================================================
// 确认闸（NeedsConfirmation）
// ===========================================================================

// TestConfirmGate_无确认时拒绝且不执行 是 R-H9 的核心验收。
//
// 失效形态：闸写了但排在权限闸之后 —— acceptEdits 模式下权限闸放行，确认闸永远
// 跑不到，"声明了需要确认"变成一句空话。这里的 tctx 用 PermissionAllow 的
// CheckPermissions，正是要复现那个顺序问题。
func TestConfirmGate_无确认时拒绝且不执行(t *testing.T) {
	g := &gateTool{name: "danger", confirms: true}
	msg := execGate(t, g, &ToolContext{})

	text, isErr := resultText(t, msg)
	if !isErr {
		t.Fatalf("无确认时必须返回 error tool_result, 实得成功: %q", text)
	}
	if !strings.Contains(text, "需要确认") {
		t.Fatalf("拒绝文案没说清原因: %q", text)
	}
	if n := g.calls.Load(); n != 0 {
		t.Fatalf("无确认时工具不得执行, 实得 %d 次", n)
	}
}

// TestConfirmGate_已确认时执行 是负对照。
//
// 没有它，"永远拒绝"的实现会让上面那条通过 —— 那时所有声明确认的工具都不工作。
func TestConfirmGate_已确认时执行(t *testing.T) {
	g := &gateTool{name: "danger", confirms: true}
	conf := &fixedConfirm{ok: true}
	msg := execGate(t, g, &ToolContext{Confirmed: conf})

	text, isErr := resultText(t, msg)
	if isErr {
		t.Fatalf("已确认时不应拒绝: %q", text)
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("已确认时应执行一次, 实得 %d 次", n)
	}
	if conf.calls.Load() != 1 {
		t.Fatalf("确认检查器应被问一次, 实得 %d 次", conf.calls.Load())
	}
}

// TestConfirmGate_nil检查器视为未确认 守住 fail-closed 方向。
//
// 反过来（nil = 放行）会让"忘了装配确认检查器"静默变成"安全边界消失"。
func TestConfirmGate_nil检查器视为未确认(t *testing.T) {
	g := &gateTool{name: "danger", confirms: true}
	msg := execGate(t, g, &ToolContext{Confirmed: nil})

	if _, isErr := resultText(t, msg); !isErr {
		t.Fatal("Confirmed 为 nil 时必须拒绝 (fail-closed)")
	}
	if n := g.calls.Load(); n != 0 {
		t.Fatalf("nil 检查器时不得执行, 实得 %d 次", n)
	}
}

// TestConfirmGate_未声明的工具不受影响 钉住零值 = 旧行为。
func TestConfirmGate_未声明的工具不受影响(t *testing.T) {
	g := &gateTool{name: "safe", confirms: false}
	msg := execGate(t, g, &ToolContext{})

	if _, isErr := resultText(t, msg); isErr {
		t.Fatal("未声明 NeedsConfirmation 的工具不应被确认闸拦下")
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("应执行一次, 实得 %d 次", n)
	}
}

// TestConfirmGate_入参参与判定钉住"换参数不算已确认"。
//
// 实现上这里只验接口收到的是真实入参：若闸传了空 input 给检查器，那么一个按
// 路径判定的实现会把"确认了 a.txt"当成"确认了 b.txt"。
func TestConfirmGate_入参传递到检查器(t *testing.T) {
	g := &gateTool{name: "danger", confirms: true}
	rec := &inputRecorder{}

	reg := NewRegistry()
	reg.Register(g)
	RunToolUse(context.Background(), types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: "danger",
		Input: json.RawMessage(`{"path":"/etc/passwd"}`),
	}, reg, &ToolContext{Confirmed: rec}, nil)

	if got := rec.lastInput(); got != `{"path":"/etc/passwd"}` {
		t.Fatalf("检查器收到的入参与真实入参不符: %q", got)
	}
}

type inputRecorder struct {
	mu   sync.Mutex
	last string
}

func (r *inputRecorder) IsConfirmed(_ string, input json.RawMessage) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = string(input)
	return false
}

func (r *inputRecorder) lastInput() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// ===========================================================================
// 幂等闸（IdempotencyKey）
// ===========================================================================

// TestIdempotencyGate_同键返回同一结果 是 R-H9 的核心验收。
func TestIdempotencyGate_同键返回同一结果(t *testing.T) {
	gate := NewIdempotencyGate(true, 0)
	tctx := &ToolContext{Idempotency: gate}

	var seq atomic.Int64
	fn := func() (*ToolResult, error) {
		return &ToolResult{Content: "第" + gateItoa(seq.Add(1)) + "次"}, nil
	}

	first, hit1, err := gate.Do(context.Background(), "k", fn)
	if err != nil || hit1 {
		t.Fatalf("首次调用: hit=%v err=%v", hit1, err)
	}
	second, hit2, err := gate.Do(context.Background(), "k", fn)
	if err != nil {
		t.Fatalf("第二次调用: %v", err)
	}
	if !hit2 {
		t.Fatal("第二次同键调用应命中缓存 (hit=true)")
	}
	if first.Content != second.Content {
		t.Fatalf("同键返回了不同结果: %q vs %q", first.Content, second.Content)
	}
	if seq.Load() != 1 {
		t.Fatalf("fn 应只执行一次, 实得 %d 次", seq.Load())
	}
	_ = tctx
}

// TestIdempotencyGate_端到端同键只执行一次 走真实的 RunToolUse。
//
// 上面那条直接调闸; 这条验的是"闸真的接在 RunToolUse 上", 且只对声明了
// Idempotent() 的工具生效。
func TestIdempotencyGate_端到端同键只执行一次(t *testing.T) {
	g := &gateTool{name: "memo", idem: true}
	gate := NewIdempotencyGate(true, 0)
	tctx := &ToolContext{Idempotency: gate}

	block := types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: "memo",
		Input: json.RawMessage(`{"q":"同一个查询"}`),
	}
	reg := NewRegistry()
	reg.Register(g)

	m1 := RunToolUse(context.Background(), block, reg, tctx, nil)
	m2 := RunToolUse(context.Background(), block, reg, tctx, nil)

	if n := g.calls.Load(); n != 1 {
		t.Fatalf("同键两次调用应只执行一次, 实得 %d 次", n)
	}
	c1, _ := resultText(t, m1)
	c2, _ := resultText(t, m2)
	if c1 != c2 {
		t.Fatalf("同键两次拿到不同结果: %q vs %q", c1, c2)
	}
}

// TestIdempotencyGate_未声明不生效 是负对照。
//
// 没有它，一个"对所有工具去重"的实现会让上面那条通过 —— 而那种实现会把
// "Read → Edit → Read" 的第二次 Read 换成陈旧内容（静默数据损坏）。
func TestIdempotencyGate_未声明不生效(t *testing.T) {
	g := &gateTool{name: "plain", idem: false}
	gate := NewIdempotencyGate(true, 0)
	tctx := &ToolContext{Idempotency: gate}

	block := types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: "plain",
		Input: json.RawMessage(`{"q":"x"}`),
	}
	reg := NewRegistry()
	reg.Register(g)
	RunToolUse(context.Background(), block, reg, tctx, nil)
	RunToolUse(context.Background(), block, reg, tctx, nil)

	if n := g.calls.Load(); n != 2 {
		t.Fatalf("未声明 Idempotent 的工具不应去重 (应执行 2 次), 实得 %d 次", n)
	}
}

// TestIdempotencyGate_不同键分别执行 钉住键真的参与判定。
func TestIdempotencyGate_不同键分别执行(t *testing.T) {
	gate := NewIdempotencyGate(true, 0)
	fn := func() (*ToolResult, error) { return &ToolResult{Content: "ok"}, nil }

	_, _, _ = gate.Do(context.Background(), "k1", fn)
	_, _, _ = gate.Do(context.Background(), "k2", fn)
	if hits, misses := gate.Stats(); hits != 0 || misses != 2 {
		t.Fatalf("两个不同键应各执行一次: hits=%d misses=%d", hits, misses)
	}
}

// TestIdempotencyGate_失败不缓存 守住"错误可重试"。
//
// 缓存失败会让这个 key 永久毒化: 重试永远拿到同一个错误, 调用方以为自己只是
// 运气不好。
func TestIdempotencyGate_失败不缓存(t *testing.T) {
	g := &gateTool{name: "flaky", idem: true, errWithResult: true}
	g.failTimes.Store(1)
	gate := NewIdempotencyGate(true, 0)
	tctx := &ToolContext{Idempotency: gate}

	block := types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: "flaky",
		Input: json.RawMessage(`{"q":"x"}`),
	}
	reg := NewRegistry()
	reg.Register(g)

	m1 := RunToolUse(context.Background(), block, reg, tctx, nil)
	if _, isErr := resultText(t, m1); !isErr {
		t.Fatal("第一次应失败")
	}
	m2 := RunToolUse(context.Background(), block, reg, tctx, nil)
	text2, isErr2 := resultText(t, m2)
	if isErr2 {
		t.Fatalf("失败不应被缓存, 第二次应成功: %q", text2)
	}
	if n := g.calls.Load(); n != 2 {
		t.Fatalf("失败未被缓存时应真的重试 (2 次), 实得 %d 次", n)
	}
}

// TestIdempotencyGate_并发同键只执行一次 是并发正确性。
//
// 读-判-写的实现会让两个 goroutine 同时看到"没有缓存"而双双执行。
func TestIdempotencyGate_并发同键只执行一次(t *testing.T) {
	gate := NewIdempotencyGate(true, 0)
	var ran atomic.Int64
	fn := func() (*ToolResult, error) {
		ran.Add(1)
		time.Sleep(20 * time.Millisecond) // 给第二个 goroutine 足够时间撞进来
		return &ToolResult{Content: "ok"}, nil
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]*ToolResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, _, err := gate.Do(context.Background(), "same-key", fn)
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	if got := ran.Load(); got != 1 {
		t.Fatalf("并发同键应只执行一次, 实得 %d 次", got)
	}
	for i, res := range results {
		if res == nil || res.Content != "ok" {
			t.Fatalf("goroutine %d 拿到了坏结果: %+v", i, res)
		}
	}
}

// TestIdempotencyGate_等待者能被取消 守住"不无限期挂住"。
func TestIdempotencyGate_等待者能被取消(t *testing.T) {
	gate := NewIdempotencyGate(true, 0)
	release := make(chan struct{})
	started := make(chan struct{})

	go func() {
		_, _, _ = gate.Do(context.Background(), "k", func() (*ToolResult, error) {
			close(started)
			<-release
			return &ToolResult{Content: "slow"}, nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := gate.Do(ctx, "k", func() (*ToolResult, error) {
		t.Error("等待者不应执行 fn")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("等待者应随 ctx 取消返回, 实得 %v", err)
	}
	close(release)
}

// TestIdempotencyGate_缓存副本隔离 钉住"调用方改结果不影响缓存"。
//
// # 判据为什么必须落在**第三次**调用上
//
// 写侧与读侧各拷贝一次，是两个独立的缺陷点，只测其中一个会漏掉另一个：
//
//	读侧不拷贝（命中时直接返回缓存里的对象）→ 调用方改第二次拿到的结果，
//	                                          第三次就会拿到被改过的内容；
//	写侧不拷贝（落缓存时存的是调用方手里那个对象）→ 调用方改第一次拿到的结果，
//	                                              第二次就会拿到被改过的内容。
//
// 所以两次都要触发：先改第一次拿到的（打写侧），再改第二次（命中）拿到的（打读侧），
// 最后第三次必须是原始值。Images 切片另起底层数组这件事也在这里被钉住 ——
// 浅拷贝结构体只复制 slice header，两个结果会共用同一段数组。
func TestIdempotencyGate_缓存副本隔离(t *testing.T) {
	gate := NewIdempotencyGate(true, 0)
	fn := func() (*ToolResult, error) {
		return &ToolResult{Content: "orig", Images: []types.MediaSource{{Data: "abc"}}}, nil
	}

	first, _, _ := gate.Do(context.Background(), "k", fn)
	first.Content = "第一次被改"
	first.Images[0].Data = "第一次被改"

	second, hit, _ := gate.Do(context.Background(), "k", fn)
	if !hit {
		t.Fatal("第二次应命中缓存")
	}
	if second.Content != "orig" {
		t.Fatalf("写侧未拷贝: 第二次拿到被改过的内容 %q", second.Content)
	}
	if second.Images[0].Data != "abc" {
		t.Fatalf("写侧未拷贝 Images 底层数组: %q", second.Images[0].Data)
	}

	// 改"命中回来"的那份：只有读侧返回副本时，缓存才不受影响。
	second.Content = "第二次被改"
	second.Images[0].Data = "第二次被改"

	third, _, _ := gate.Do(context.Background(), "k", fn)
	if third.Content != "orig" {
		t.Fatalf("读侧未拷贝: 第三次拿到 %q —— 调用方改命中的结果污染了缓存", third.Content)
	}
	if third.Images[0].Data != "abc" {
		t.Fatalf("读侧未拷贝 Images: 第三次拿到 %q", third.Images[0].Data)
	}
}

// TestIdempotencyGate_上限清空 守住内存有界。
func TestIdempotencyGate_上限清空(t *testing.T) {
	gate := NewIdempotencyGate(true, 3)
	fn := func() (*ToolResult, error) { return &ToolResult{Content: "ok"}, nil }
	for i := 0; i < 10; i++ {
		_, _, _ = gate.Do(context.Background(), "k"+gateItoa(int64(i)), fn)
	}
	if size := gate.Size(); size > 3 {
		t.Fatalf("缓存条数应受上限约束 (<=3), 实得 %d", size)
	}
}

// TestIdempotencyGate_未启用零开销 钉住默认关闭。
func TestIdempotencyGate_未启用零开销(t *testing.T) {
	gate := NewIdempotencyGate(false, 0)
	var ran atomic.Int64
	fn := func() (*ToolResult, error) { ran.Add(1); return &ToolResult{Content: "ok"}, nil }
	_, _, _ = gate.Do(context.Background(), "k", fn)
	_, _, _ = gate.Do(context.Background(), "k", fn)
	if ran.Load() != 2 {
		t.Fatalf("未启用时应每次都执行, 实得 %d 次", ran.Load())
	}
	if size := gate.Size(); size != 0 {
		t.Fatalf("未启用时不应缓存, 实得 %d 条", size)
	}
}

// ===========================================================================
// 超时闸（Timeout）
// ===========================================================================

// TestTimeoutGate_超时返回typed错误 是核心验收。
//
// # 为什么要自带看门狗而不是直接调 execGate
//
// 这个测试的被测对象正是"阻塞会不会被切断"。若闸失效，直接调用会**永久阻塞**，
// 于是测试不是变红而是挂到 go test 的 10 分钟全局超时 —— 那时得到的信号是
// "包超时"，既定位不到是哪条用例，也没法在变异验证里快速迭代（实测挂满 600s）。
// 所以把调用放进 goroutine，用 select + 本地 watchdog 收口：闸失效 → 2 秒后
// 明确报"超时未生效"，而不是挂死。
func TestTimeoutGate_超时返回typed错误(t *testing.T) {
	block := make(chan struct{})
	defer close(block) // 即便提前失败也要放行被阻塞的那个 goroutine, 免得它泄漏
	g := &gateTool{name: "slow", blocking: block, timeout: 20 * time.Millisecond}

	type callOut struct{ msg types.Message }
	done := make(chan callOut, 1)
	reg := NewRegistry()
	reg.Register(g)
	block0 := types.ContentBlock{
		Type: types.ContentBlockToolUse, ID: "tu-1", Name: "slow",
		Input: json.RawMessage(`{"x":1}`),
	}
	start := time.Now()
	go func() {
		// 直接调 RunToolUse 而不是 execGate: 后者内部会 t.Fatalf, 而 t.Fatalf
		// 在非测试 goroutine 上调用是无效的（Goexit 只结束那个 goroutine,
		// 测试会继续跑下去）。断言一律留在主 goroutine 里。
		done <- callOut{msg: RunToolUse(context.Background(), block0, reg, &ToolContext{}, nil)}
	}()

	var msg types.Message
	select {
	case out := <-done:
		msg = out.msg
	case <-time.After(2 * time.Second):
		t.Fatalf("超时未生效: 闸声明 20ms, 但调用在 2s 后仍未返回 —— "+
			"callWithTimeout 没有切断阻塞 (耗时 %s)", time.Since(start))
	}
	elapsed := time.Since(start)

	text, isErr := resultText(t, msg)
	if !isErr {
		t.Fatal("超时必须返回 error tool_result")
	}
	if !strings.Contains(text, "超时") {
		t.Fatalf("错误文案没说是超时: %q", text)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("超时未生效 (耗时 %s)", elapsed)
	}
	// 给模型的建议必须指向"收窄范围", 而不是让它原样重试。
	if !strings.Contains(text, "收窄") {
		t.Fatalf("超时文案应提示收窄范围: %q", text)
	}
}

// TestTimeoutGate_未超时正常返回 是负对照。
func TestTimeoutGate_未超时正常返回(t *testing.T) {
	g := &gateTool{name: "quick", timeout: time.Second, result: "done"}
	msg := execGate(t, g, &ToolContext{})
	text, isErr := resultText(t, msg)
	if isErr {
		t.Fatalf("未超时不应报错: %q", text)
	}
	if text != "done" {
		t.Fatalf("结果被改动: %q", text)
	}
}

// TestTimeoutGate_零值不设超时 钉住零值 = 旧行为。
//
// 一个卡住不动的工具会一直挂着 —— 这确实是"没声明就等于没护栏", 但反过来
// 给所有未声明工具拍一个默认超时, 会让慢工具在高峰期随机失败。选择可见的
// 挂起而不是随机的失败。
func TestTimeoutGate_零值不设超时(t *testing.T) {
	g := &gateTool{name: "nosleep", timeout: 0, result: "ok"}
	msg := execGate(t, g, &ToolContext{})
	if text, isErr := resultText(t, msg); isErr {
		t.Fatalf("未声明超时的工具不应被拦: %q", text)
	}
}

// TestTimeoutGate_父取消不被报成超时 是归因正确性。
//
// 两者都是"调用返回了 ctx 错误", 但处置完全不同: 取消要终止整轮, 超时只让
// 这一个工具失败。混成一个错误会让这两条路无法分辨。
func TestTimeoutGate_父取消不被报成超时(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := callWithTimeout(ctx, time.Second, func(c context.Context) (*ToolResult, error) {
		return nil, c.Err()
	})
	if err == nil {
		t.Fatal("已取消的 ctx 不应成功")
	}
	if errors.Is(err, ErrToolTimedOut) {
		t.Fatalf("父取消被误报成超时: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled, 实得 %v", err)
	}
	_ = res
}

// TestCallWithTimeout_不泄漏goroutine 覆盖 Q-H3 的一部分。
//
// 超时后那个仍在执行的 goroutine 必须能写进带缓冲的 channel 并退出 ——
// 无缓冲 channel 会让它永久阻塞在 send 上。
func TestCallWithTimeout_不泄漏goroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	block := make(chan struct{})
	for i := 0; i < 20; i++ {
		_, _ = callWithTimeout(context.Background(), 5*time.Millisecond, func(context.Context) (*ToolResult, error) {
			<-block
			return &ToolResult{Content: "late"}, nil
		})
	}
	close(block)
	// 给残留 goroutine 一点时间退出。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine 泄漏: 之前 %d, 之后 %d", before, runtime.NumGoroutine())
}

// TestTimeoutOf_未声明返回零 钉住探测函数本身。
func TestTimeoutOf_未声明返回零(t *testing.T) {
	if got := TimeoutOf(nil); got != 0 {
		t.Fatalf("nil 工具超时应为 0, 实得 %s", got)
	}
	if got := TimeoutOf(&gateTool{name: "x"}); got != 0 {
		t.Fatalf("零值超时应为 0, 实得 %s", got)
	}
	if got := TimeoutOf(&gateTool{name: "x", timeout: 3 * time.Second}); got != 3*time.Second {
		t.Fatalf("声明超时未被读出: %s", got)
	}
}

// itoa 避免为几个数字引入 strconv（测试文件里只用得到小整数）。
func gateItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
