// runner_writeclaim_test.go —— H3-4 落在 **RunIsolated 装配面**上的一半验收。
//
// pkg/tool 那边判占用表本身, pkg/tool/builtin 那边判 Write/Edit 真的调了它。
// 还差一处**接线**, 而它恰恰是最容易缺的一环:
//
//	claim := tool.BeginWriter(...)
//	defer claim.Release()                                   ← 少了这句, 别的用例照样绿
//	tctx := &tool.ToolContext{..., WriteClaim: claim}      ← 少了这个字段, 也绿
//
// 两处都是"跑一次 run, 一切看起来正常"。所以这里的判据必须落在**跨 run 的读数**上:
//
//   - 少了 defer Release: 第一个 run 留下的占用没人释放, 第二个同路径的 run 会被
//     一个已经不存在的"并行写者"挡住 —— 现象是"同路径的后继任务必被拒", 而报错说
//     "已被并行的写者占用", 明明没有并行的写者了。这比静默覆盖更糟: 覆盖面从"偶发
//     冲突"变成"必发"。
//   - 少了 WriteClaim 字段: tctx 里那个是 nil, Claim 恒放行 —— 并行的两个 run
//     双双成功, 正是这个能力要挡的静默覆盖。
//
// # 为什么用假工具而不是真的 FileWrite
//
// pkg/engine 的非外部测试包不能 import pkg/tool/builtin (builtin → evotools →
// evolution/govern → agent 成环)。所以这里用一个**只做占用、不落盘**的假工具 ——
// 它调的是与真 Write 逐字相同的那个入口 (tctx.ClaimWrite), 走的是同一条装配路径。
// 落盘那一半由 pkg/tool/builtin/writeclaim_test.go 用真工具判, 两边合起来才完整。
package engine

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// claimProbe 记录每次 ClaimWrite 的结果 (供断言"谁拿到了、谁被拒了")。
type claimProbe struct {
	mu      sync.Mutex
	results []error
}

func (p *claimProbe) add(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.results = append(p.results, err)
}

func (p *claimProbe) snapshot() []error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]error(nil), p.results...)
}

// claimingTool 只做一件事: 用工具上下文的写占用入口去占 input.path。
// 调的是 tctx.ClaimWrite —— 与 filewrite.go / fileedit.go 里那一句逐字相同。
type claimingTool struct{ probe *claimProbe }

func (t claimingTool) Name() string { return "noop_tool" } // 与假上游回的 tool_use 同名
func (t claimingTool) Description() string {
	return "Claims a write path for concurrency-conflict testing."
}
func (t claimingTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
}
func (t claimingTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// IsReadOnly=true 是刻意的: 这个假工具只做占用、不落盘, 而 acceptEdits 权限模式下
// "非只读的写入工具"会被权限闸拦下 (根本走不到 Call), 于是探针恒为空 —— 用例会以
// "工具没被调用"的形式失败, 看起来像接线的毛病, 实际是探针自己没被放行。
// 占用入口 (tctx.ClaimWrite) 与只读与否无关, 这里要验的是它, 不是权限。
func (t claimingTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (t claimingTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (t claimingTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &in)
	err := tctx.ClaimWrite(in.Path)
	t.probe.add(err)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return &tool.ToolResult{Content: "claimed " + in.Path}, nil
}

// runClaiming 跑一次"声明要写 path"的隔离 run, 返回该 run 里工具拿到的占用结果。
func runClaiming(t *testing.T, id RunIdentity, path string, gate *turnGate) []error {
	t.Helper()
	probe := &claimProbe{}
	up := newStateUpstream(t, 1, "done") // 1 轮工具, 然后收尾
	up.toolInput = `{"path":` + jsonString(path) + `}`
	if gate != nil {
		up.onRequest = func(turn int, _ seenRequest) {
			if turn == 0 {
				gate.wait()
			}
		}
	}

	reg := tool.NewRegistry()
	reg.Register(claimingTool{probe: probe})
	_, cfg := stateIdentity()
	client := &api.Client{
		BaseURL: up.srv.URL, APIKey: "k", Model: cfg.Model,
		Client: up.srv.Client(), RetryCount: 0,
	}
	e := NewQueryEngine(cfg, client, reg,
		hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, nil)
	e.SetRunStateDir(t.TempDir())

	if _, err := e.RunIsolated(context.Background(), "改 "+path, isoOpts(id, 3)); err != nil {
		t.Fatalf("run 不该报错 (写冲突由工具结果表达, 不是 run 失败): %v", err)
	}
	return probe.snapshot()
}

// TestH34_并行写同一文件恰好一个被拒 验收原文的直接落点 (两件事一起判)。
func TestH34_并行写同一文件恰好一个被拒(t *testing.T) {
	path := t.TempDir() + "/shared.go"

	// 两个 run 必须**真的重叠**: 假上游在收到 2 个 turn-0 请求之前不回应, 于是两个
	// run 会一起走到工具执行那一步。没有这道闸, 第一个 run 可能已经跑完并释放了占用,
	// 第二个才申请 —— 那时它当然拿得到, 而用例会以为"冲突没被检测到"。
	// (先后跑不重叠的两个 run **本来就该**双双成功, 那是 TestH34_占用不跨run泄漏。)
	gate := newTurnGate(2)
	t.Cleanup(gate.release)

	ids := []RunIdentity{}
	for _, stage := range []string{"stage-a", "stage-b"} {
		id, _ := stateIdentity()
		id.Stage = stage
		ids = append(ids, id)
	}

	var wg sync.WaitGroup
	all := make([][]error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id RunIdentity) {
			defer wg.Done()
			all[i] = runClaiming(t, id, path, gate)
		}(i, id)
	}
	wg.Wait()

	var ok, conflict int
	for i, got := range all {
		if len(got) != 1 {
			t.Fatalf("run %d 应调用一次工具, 实得 %d 次 —— 重叠没造出来, 用例没有判别力", i, len(got))
		}
		switch {
		case got[0] == nil:
			ok++
		case tool.IsWriteConflict(got[0]):
			conflict++
		default:
			t.Fatalf("run %d 出现非冲突类错误: %v", i, got[0])
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("两个并行 run 抢同一文件必须**恰好一个**成功、一个被明确拒绝, 实得 成功=%d 冲突=%d\n"+
			"    成功=2 ⇒ RunIsolated 没把 WriteClaim 装进 ToolContext (tctx 里是 nil, Claim 恒放行) —— 静默覆盖\n"+
			"    冲突=2 ⇒ 占用在 run 之间泄漏, 连不冲突的也会被拒", ok, conflict)
	}
}

// TestH34_占用不跨run泄漏 Release 的回归臂。
//
// 判据落在**跨 run 的全局读数**上而不是"RunIsolated 里有没有那句 defer": 后者是
// 装配态, 一击就假绿。这里先跑一个 run 写下占用, 再跑第二个 —— 若前一个没释放,
// 第二个会被一个已经不存在的"并行写者"挡住。
//
// 用**同一身份顺序执行**的形状: 那是编排层把共享写文件的两个 leaf 串行化的结果
// (后来者依赖前者)。这一路必须畅通, 否则 H3-4 会把编排层已经串行化好的正常流程打死。
func TestH34_占用不跨run泄漏(t *testing.T) {
	path := t.TempDir() + "/shared.go"
	id, _ := stateIdentity()
	id.Stage = "shared-file-stage"

	first := runClaiming(t, id, path, nil)
	if len(first) != 1 || first[0] != nil {
		t.Fatalf("第一个 run 该拿到占用, 实得: %v", first)
	}

	// 关键读数: run 结束之后占用表必须**回到空**。
	// 只看"第二个 run 成没成功"是不够的 —— 那是一个间接读数, 而占用泄漏是关于全局
	// 状态的断言, 就该直接读全局状态。
	if n := tool.Holders(); n != 0 {
		t.Fatalf("run 结束后占用表里还剩 %d 条 —— defer claim.Release() 没生效或没装上。"+
			"泄漏的后果不是「偶发冲突」, 而是同路径后继任务必被拒", n)
	}

	second := runClaiming(t, id, path, nil)
	if len(second) != 1 {
		t.Fatalf("第二个 run 应调用一次工具, 实得 %d 次", len(second))
	}
	if second[0] != nil {
		t.Fatalf("前一个 run 已结束, 占用必须已释放; 第二个 run 被自己人挡住了: %v", second[0])
	}
}

// TestH34_不同文件的两个run都不受阻 反面臂 —— 并行写不同文件的扇出能力。
//
// 一个"凡是并发就拒"的实现能让上面两条全绿, 而它会把并行子代理彻底废掉。这条用
// **两个真正的并行 run 写不同文件**来判: 两个都必须成功。
func TestH34_不同文件的两个run都不受阻(t *testing.T) {
	dir := t.TempDir()

	gate := newTurnGate(2)
	t.Cleanup(gate.release)

	specs := []struct {
		id   RunIdentity
		path string
	}{}
	for i, stage := range []string{"writer-a", "writer-b"} {
		id, _ := stateIdentity()
		id.Stage = stage
		specs = append(specs, struct {
			id   RunIdentity
			path string
		}{id, dir + "/f" + string(rune('a'+i)) + ".go"})
	}

	var wg sync.WaitGroup
	all := make([][]error, len(specs))
	for i, sp := range specs {
		wg.Add(1)
		go func(i int, id RunIdentity, path string) {
			defer wg.Done()
			all[i] = runClaiming(t, id, path, gate)
		}(i, sp.id, sp.path)
	}
	wg.Wait()

	for i, got := range all {
		if len(got) != 1 {
			t.Fatalf("run %d 应调用一次工具, 实得 %d 次 (重叠没造出来)", i, len(got))
		}
		if got[0] != nil {
			t.Fatalf("两个 run 写**不同**文件, run %d 不该被拒: %v —— 这种失效会让并行编辑全废", i, got[0])
		}
	}
}

// ============================================================================
// 小工具
// ============================================================================

// jsonString 把一个字符串编成 JSON 字面量 (拼 tool_use 的 input 用)。
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// turnGate 让 N 个 run 在各自的第 1 轮请求处对齐 —— 保证它们**真的重叠**。
//
// 为什么必须有它: 两个 goroutine 起起来不等于它们会同时走到工具执行那一步。
// 没有这道闸, 第一个 run 完全可能跑完并释放占用之后第二个才开始申请, 于是用例在
// **任何**实现下都绿 —— 包括把整个占用表删掉的实现。这正是"测试在生产不走的路径上"
// 的一种: 被测的语义 (并发) 根本没发生。
type turnGate struct {
	mu      sync.Mutex
	need    int
	arrived int
	ch      chan struct{}
	once    sync.Once
}

func newTurnGate(n int) *turnGate {
	return &turnGate{need: n, ch: make(chan struct{})}
}

// wait 阻塞到 n 个调用方都到达 (或 release 被调用)。
func (g *turnGate) wait() {
	g.mu.Lock()
	g.arrived++
	reached := g.arrived >= g.need
	g.mu.Unlock()
	if reached {
		g.once.Do(func() { close(g.ch) })
	}
	<-g.ch
}

// release 兜底放行。**必须在 t.Cleanup 里注册**: 若某个 run 在第 1 轮就出错返回,
// 另一个会永久阻塞在这个闸上, 整个测试挂死 —— 那比失败更难查 (它不报错, 只是不动)。
func (g *turnGate) release() {
	g.once.Do(func() { close(g.ch) })
}
