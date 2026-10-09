package longhorizon

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeRunner 记录每次调用, 按预设返回读数。
//
// 它同时是**取证工具**: 台子有没有按对的顺序、对的输入喂任务, 只能靠记录下来的
// 调用链看 —— 只看最终的成功率, "两组喂反了"与"开关真的有效"读数一样。
type fakeRunner struct {
	calls []fakeCall
	fn    func(call fakeCall) TaskOutcome
}

type fakeCall struct {
	TaskID string
	Knob   string // 这次运行的环境变量里被测开关的取值
	Dir    string
	Config any
}

func (f *fakeRunner) Run(task Task, spec RunSpec, workDir string) (TaskOutcome, error) {
	c := fakeCall{TaskID: task.ID, Dir: workDir, Config: spec.Config}
	c.Knob = spec.Env[runInterceptorsEnv]
	if c.Knob == "" {
		if ai, ok := spec.Config.(map[string]any)["ai"].(map[string]any); ok {
			c.Knob = fmt.Sprintf("%v", ai["longHorizonMaxTurns"])
		}
	}
	f.calls = append(f.calls, c)
	if f.fn != nil {
		return f.fn(c), nil
	}
	return TaskOutcome{Turns: MinLongHorizonTurns + 5, CompactSpans: 1}, nil
}

func tinyTaskSet(n int) []Task {
	out := make([]Task, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Task{
			ID: fmt.Sprintf("t%d", i), Goal: "g", Fixture: "lexer",
			VerifyCmd: []string{"go", "test", "./..."},
			MinTurns:  MinLongHorizonTurns, MustCompact: true,
		})
	}
	return out
}

// noopMaterializer 不碰文件系统 (台子自己的测试不该依赖真实 fixture 展开)。
func noopMaterializer(string, Task) error { return nil }

// TestH42_配对跑法逐任务交替 —— 顺序这件事有可观测后果。
//
// 若实现成"先跑完 A 组全部任务再跑 B 组", 上游的漂移 (模型版本、限流、宿主机负载)
// 就落在两组之间, 成为配对消不掉、也无法从数据里看出的系统差异。
// 正确形状是**同一任务的两组紧邻**。
func TestH42_配对跑法逐任务交替(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair := GatePair(base)
	r := &fakeRunner{}
	tasks := tinyTaskSet(3)

	// 台子自己建目录; 这里让它用 t.TempDir 之外的临时目录即可, 跑完清理。
	prev := WorkDirFactory
	WorkDirFactory = func(Task) (string, func(), error) { return "/tmp/fake", func() {}, nil }
	defer func() { WorkDirFactory = prev }()

	if _, err := RunPair(pair, tasks, r, noopMaterializer); err != nil {
		t.Fatalf("跑配对失败: %v", err)
	}

	want := []string{"t0:off", "t0:gate", "t1:off", "t1:gate", "t2:off", "t2:gate"}
	var got []string
	seen := map[string]bool{}
	for _, c := range r.calls {
		got = append(got, c.TaskID+":"+c.Knob)
		seen[c.Knob] = true
	}
	// 两组的开关取值必须**都**真的到达了运行侧。只判顺序的话, 一个"两组都传 off"
	// 的台子照样有序, 而它跑的是同一组两遍 —— 报告只会说"这个开关没有效果"。
	if !seen["off"] || !seen["gate"] {
		t.Fatalf("两组开关取值没有都到达运行侧 (看到 %v) —— 这不是 A/B, 是同一组跑了两遍", seen)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("调用顺序 = %v, 期望逐任务交替 %v (先跑完一组再跑另一组会把上游漂移\n"+
			"变成两组之间的系统差异, 而配对统计看不出来)", got, want)
	}
}

// TestH42_每次运行都拿到干净目录 —— 反面臂。
//
// 复用目录会让第二组看到第一组的产物: 任务"一开始就通过了". 这种污染的读数是
// 成功率虚高, 且**偏向后跑的那一组** —— 正好是要测的那一组。
func TestH42_每次运行都拿到干净目录(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair := GatePair(base)
	r := &fakeRunner{}

	n := 0
	prev := WorkDirFactory
	WorkDirFactory = func(Task) (string, func(), error) {
		n++
		return fmt.Sprintf("/tmp/dir%d", n), func() {}, nil
	}
	defer func() { WorkDirFactory = prev }()

	if _, err := RunPair(pair, tinyTaskSet(3), r, noopMaterializer); err != nil {
		t.Fatalf("跑配对失败: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range r.calls {
		if seen[c.Dir] {
			t.Fatalf("目录 %s 被两次运行复用 —— 后跑的那一组会看到前一组的产物", c.Dir)
		}
		seen[c.Dir] = true
	}
	if n != 6 {
		t.Fatalf("分配了 %d 个目录, 期望 6 (3 任务 x 2 组)", n)
	}
}

// TestH42_输入不合规就不开跑 —— 省钱, 也省掉一份没人能归因的报告。
func TestH42_输入不合规就不开跑(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair, err := MaxTurnsPair(base)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	// 有人只给 B 组改了 maxTurns —— 典型的演进中漂移。
	pair.B.Config = mustDoc(t, `{"ai":{"maxTurns":999,"longHorizonMaxTurns":150}}`)
	r := &fakeRunner{}
	if _, err := RunPair(pair, tinyTaskSet(2), r, noopMaterializer); err == nil {
		t.Fatal("输入不合规时必须直接报错, 实得 nil")
	}
	if len(r.calls) != 0 {
		t.Fatalf("输入不合规却已经跑了 %d 次真实运行", len(r.calls))
	}
}

// TestH42_运行出错也计入分母 —— 反面臂。
//
// 把出错的样本丢掉会让分母变小、成功率虚高, 而"哪一组更容易出错"恰恰可能是被测
// 开关的效果之一。
func TestH42_运行出错也计入分母(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair := GatePair(base)

	prev := WorkDirFactory
	WorkDirFactory = func(Task) (string, func(), error) { return "/tmp/fake", func() {}, nil }
	defer func() { WorkDirFactory = prev }()

	r := &fakeRunner{fn: func(c fakeCall) TaskOutcome {
		if c.Knob == "gate" {
			return TaskOutcome{} // 全挂
		}
		return TaskOutcome{Passed: true, Turns: 120, CompactSpans: 2}
	}}
	rep, err := RunPair(pair, tinyTaskSet(4), r, noopMaterializer)
	if err != nil {
		t.Fatalf("跑配对失败: %v", err)
	}
	if rep.Pair.N != 4 {
		t.Fatalf("N = %d, 期望 4 —— 出错的样本不该被丢掉", rep.Pair.N)
	}
	if rep.Pair.APass() != 4 || rep.Pair.BPass() != 0 {
		t.Fatalf("通过数 A=%d B=%d, 期望 4/0", rep.Pair.APass(), rep.Pair.BPass())
	}
}

// TestH42_长程验收的读数来自运行 —— 方案 §21.5.6 H4-1 的落点。
func TestH42_长程验收的读数来自运行(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair := GatePair(base)

	prev := WorkDirFactory
	WorkDirFactory = func(Task) (string, func(), error) { return "/tmp/fake", func() {}, nil }
	defer func() { WorkDirFactory = prev }()

	t.Run("两组都够长程时通过", func(t *testing.T) {
		r := &fakeRunner{} // 默认: 105 轮, 1 次折叠
		rep, err := RunPair(pair, tinyTaskSet(3), r, noopMaterializer)
		if err != nil {
			t.Fatalf("跑配对失败: %v", err)
		}
		if !rep.LongHorizon.MeetsAcceptance() {
			t.Fatalf("应满足长程验收:\n%s", rep.LongHorizon.Describe())
		}
	})

	t.Run("轮数不足要指名", func(t *testing.T) {
		r := &fakeRunner{fn: func(fakeCall) TaskOutcome {
			return TaskOutcome{Passed: true, Turns: 12, CompactSpans: 1}
		}}
		rep, err := RunPair(pair, tinyTaskSet(3), r, noopMaterializer)
		if err != nil {
			t.Fatalf("跑配对失败: %v", err)
		}
		if rep.LongHorizon.MeetsAcceptance() {
			t.Fatal("轮数只有 12, 不该判为满足长程验收")
		}
		if len(rep.LongHorizon.TurnsBelowMin) != 3 {
			t.Fatalf("应报出 3 个轮数不足的任务, 实得 %v", rep.LongHorizon.TurnsBelowMin)
		}
	})

	t.Run("没撞压缩阈值要指名", func(t *testing.T) {
		r := &fakeRunner{fn: func(fakeCall) TaskOutcome {
			return TaskOutcome{Passed: true, Turns: 150, CompactSpans: 0}
		}}
		rep, err := RunPair(pair, tinyTaskSet(3), r, noopMaterializer)
		if err != nil {
			t.Fatalf("跑配对失败: %v", err)
		}
		if rep.LongHorizon.MeetsAcceptance() {
			t.Fatal("全程没有折叠, 不该判为满足长程验收 —— 它没测到「跨压缩存活」")
		}
		if len(rep.LongHorizon.NoCompact) != 3 {
			t.Fatalf("应报出 3 个没折叠的任务, 实得 %v", rep.LongHorizon.NoCompact)
		}
	})
}

// TestH42_报告里同时有统计与长程验收 —— 报告是交付物。
func TestH42_报告里同时有统计与长程验收(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair := GatePair(base)

	prev := WorkDirFactory
	WorkDirFactory = func(Task) (string, func(), error) { return "/tmp/fake", func() {}, nil }
	defer func() { WorkDirFactory = prev }()

	r := &fakeRunner{}
	rep, err := RunPair(pair, tinyTaskSet(3), r, noopMaterializer)
	if err != nil {
		t.Fatalf("跑配对失败: %v", err)
	}
	text := rep.Report()
	for _, want := range []string{"95%CI", "McNemar", "长程验收", runInterceptorsEnv} {
		if !strings.Contains(text, want) {
			t.Fatalf("报告里缺少 %q:\n%s", want, text)
		}
	}
}

// TestH42_MaxTurnsPair两组只在配置上差一个键 —— H4-3 的 A/B 形态。
func TestH42_MaxTurnsPair两组只在配置上差一个键(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25,"maxTokens":32768}}`)
	pair, err := MaxTurnsPair(base)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if err := pair.AssertOnlyKnobDiffers(); err != nil {
		t.Fatalf("长程档配对被判不合规: %v", err)
	}
}

// errRunner 一半运行会以 error 收场 (二进制缺失/超时/配置写不进去)。
type errRunner struct{ failOn string }

func (e errRunner) Run(task Task, spec RunSpec, workDir string) (TaskOutcome, error) {
	if spec.Env[runInterceptorsEnv] == e.failOn {
		return TaskOutcome{}, errors.New("台子注入的 Runner 错误")
	}
	return TaskOutcome{Passed: true, Turns: 120, CompactSpans: 2}, nil
}

// TestH42_运行返回错误也必须留下样本 —— 反面臂。
//
// 上面那条判的是"Runner 自己报了个失败读数", 那是个**正常返回**。这里判的是
// Runner 返回 error 的那条路径: 若把它 continue 掉, 样本会整条消失 ——
// 分母变小、成功率虚高, 而"哪一组更容易跑不起来"恰恰可能就是被测开关的效果
// (闸门开着的那组要多跑一环, 更容易撞超时)。这条路径上丢掉样本, 报告只会
// 显示"这一组更少失败", 结论方向完全反了。
func TestH42_运行返回错误也必须留下样本(t *testing.T) {
	base := mustDoc(t, `{"ai":{"maxTurns":25}}`)
	pair := GatePair(base)

	prev := WorkDirFactory
	WorkDirFactory = func(Task) (string, func(), error) { return "/tmp/fake", func() {}, nil }
	defer func() { WorkDirFactory = prev }()

	tasks := tinyTaskSet(4)
	rep, err := RunPair(pair, tasks, errRunner{failOn: "gate"}, noopMaterializer)
	if err != nil {
		t.Fatalf("跑配对失败: %v", err)
	}
	// 4 个任务各跑两组 = 8 次, 其中 B 组 4 次全部返回 error —— 分母必须是 4,
	// 不能是 0, 也不能把出错的那些算成"没跑过"。
	if rep.Pair.N != 4 {
		t.Fatalf("N = %d, 期望 4 —— 返回 error 的样本被整条丢掉了", rep.Pair.N)
	}
	if rep.Pair.BPass() != 0 {
		t.Fatalf("B 组通过数 = %d, 期望 0", rep.Pair.BPass())
	}
	// 逐任务核 B 组的样本: 既要在, 也要留下原因。
	for _, task := range tasks {
		o, ok := rep.Outcomes[task.ID]["B"]
		if !ok {
			t.Fatalf("任务 %s 在 B 组下没有留下样本 —— 返回 error 时被 continue 掉了", task.ID)
		}
		if o.Err == "" {
			t.Fatalf("任务 %s 的 B 组样本没有留下错误说明 —— 报告里看不出它是「没做出来」还是「根本没跑起来」", task.ID)
		}
		if o.Passed {
			t.Fatalf("任务 %s 的 B 组以 error 收场, 不该被记成通过", task.ID)
		}
	}
}
