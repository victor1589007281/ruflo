package replay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fnCand struct {
	name string
	fn   func(Task) (string, error)
}

func (c *fnCand) Name() string                                      { return c.name }
func (c *fnCand) Produce(_ context.Context, t Task) (string, error) { return c.fn(t) }

type fixedJudge struct {
	score  float64
	err    error
	called *atomic.Int64
}

func (j *fixedJudge) Score(context.Context, Task, string) (float64, string, error) {
	if j.called != nil {
		j.called.Add(1)
	}
	return j.score, "fixed", j.err
}

// 指纹必须按内容而非序号——任务集增删后按序号续跑会错位，把 A 的结果记到 B 头上。
func TestTask_Fingerprint按内容(t *testing.T) {
	a := Task{ID: "1", Objective: "obj", Input: "in", Expect: []string{"x"}}
	b := Task{ID: "999", Objective: "obj", Input: "in", Expect: []string{"x"}} // ID 不同
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("指纹不应受 ID 影响（它只是展示名）")
	}
	c := Task{ID: "1", Objective: "obj2", Input: "in", Expect: []string{"x"}}
	if a.Fingerprint() == c.Fingerprint() {
		t.Error("Objective 变了指纹必须变")
	}
	d := Task{ID: "1", Objective: "obj", Input: "in", Expect: []string{"y"}}
	if a.Fingerprint() == d.Fingerprint() {
		t.Error("Expect 变了指纹必须变")
	}
}

// 空产出短路：zero-turn 直接 0 分且**不启动 Judge**——否则会为空字符串付一次 LLM 钱。
func TestHarness_空产出短路不调Judge(t *testing.T) {
	called := &atomic.Int64{}
	h, err := New(Config{}, &fixedJudge{score: 1, called: called})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	rep, err := h.Run(context.Background(),
		&fnCand{name: "empty", fn: func(Task) (string, error) { return "   ", nil }},
		[]Task{{ID: "t1", Objective: "o"}})
	if err != nil {
		t.Fatal(err)
	}
	r := rep.Results[0]
	if !r.ZeroTurn || r.Score != 0 || r.Passed {
		t.Errorf("空产出应短路为 0 分未通过, 实得 %+v", r)
	}
	if called.Load() != 0 {
		t.Errorf("空产出不应调用 Judge, 实际调了 %d 次", called.Load())
	}
}

// 确定性断言是硬否决：不过直接 0 分，且不问 Judge
// （能确定性判的不该花 LLM 钱，也不该让 LLM 的宽容盖过硬事实）。
func TestHarness_确定性断言硬否决(t *testing.T) {
	called := &atomic.Int64{}
	h, _ := New(Config{}, &fixedJudge{score: 1, called: called}) // Judge 给满分
	defer h.Close()
	rep, _ := h.Run(context.Background(),
		&fnCand{name: "c", fn: func(Task) (string, error) { return "hello world", nil }},
		[]Task{{ID: "t", Objective: "o", Expect: []string{"MUST_HAVE"}}})
	r := rep.Results[0]
	if r.Passed || r.Score != 0 {
		t.Errorf("断言未命中应 0 分未通过（Judge 给满分也不能救）, 实得 %+v", r)
	}
	if !strings.Contains(r.Reason, "MUST_HAVE") {
		t.Errorf("Reason 应指出缺了哪个断言: %q", r.Reason)
	}
	if called.Load() != 0 {
		t.Error("断言未命中不应调用 Judge")
	}
}

// 断言全过 + 无 Judge = 满分（"能确定性判的就不花 LLM 钱"的极端情形）。
func TestHarness_无Judge时断言全过即满分(t *testing.T) {
	h, _ := New(Config{}, nil)
	defer h.Close()
	rep, _ := h.Run(context.Background(),
		&fnCand{name: "c", fn: func(Task) (string, error) { return "has TOKEN here", nil }},
		[]Task{{ID: "t", Objective: "o", Expect: []string{"TOKEN"}}})
	if !rep.Results[0].Passed || rep.Results[0].Score != 1 {
		t.Errorf("断言全过且无 Judge 应满分, 实得 %+v", rep.Results[0])
	}
}

// 确定性门禁：exit code 非 0 直接判负；未注入 GateRunner 时要在 Reason 里注明被跳过
// （静默跳过门禁会让人以为门禁过了）。
func TestHarness_确定性门禁(t *testing.T) {
	h, _ := New(Config{Gate: func(context.Context, string, string) error {
		return errors.New("build failed")
	}}, nil)
	defer h.Close()
	rep, _ := h.Run(context.Background(),
		&fnCand{name: "c", fn: func(Task) (string, error) { return "code", nil }},
		[]Task{{ID: "t", Objective: "o", Gate: "go build ./..."}})
	if rep.Results[0].Passed {
		t.Error("门禁失败应判负")
	}
	if !strings.Contains(rep.Results[0].Reason, "门禁失败") {
		t.Errorf("Reason 应说明门禁失败: %q", rep.Results[0].Reason)
	}

	h2, _ := New(Config{}, nil) // 未注入 GateRunner
	defer h2.Close()
	rep2, _ := h2.Run(context.Background(),
		&fnCand{name: "c", fn: func(Task) (string, error) { return "code", nil }},
		[]Task{{ID: "t", Objective: "o", Gate: "go build ./..."}})
	if !strings.Contains(rep2.Results[0].Reason, "跳过") {
		t.Errorf("未注入 GateRunner 应注明门禁被跳过, 实得 %q", rep2.Results[0].Reason)
	}
}

// 流式落盘：每完成一条立即写，中断不丢已完成结果。
func TestHarness_流式落盘(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sub", "results.jsonl")
	h, err := New(Config{OutPath: out}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks := []Task{
		{ID: "a", Objective: "o1", Expect: []string{"X"}},
		{ID: "b", Objective: "o2", Expect: []string{"X"}},
	}
	if _, err := h.Run(context.Background(),
		&fnCand{name: "c", fn: func(Task) (string, error) { return "X", nil }}, tasks); err != nil {
		t.Fatal(err)
	}
	h.Close()
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("结果文件应存在: %v", err)
	}
	if n := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; n != 2 {
		t.Errorf("应有 2 行结果, 实得 %d 行", n)
	}
}

// 续跑按指纹跳过已完成任务（H12），且跳过的不计入均分。
func TestHarness_续跑按指纹跳过(t *testing.T) {
	out := filepath.Join(t.TempDir(), "r.jsonl")
	tasks := []Task{
		{ID: "a", Objective: "o1", Expect: []string{"X"}},
		{ID: "b", Objective: "o2", Expect: []string{"X"}},
	}
	h, _ := New(Config{OutPath: out}, nil)
	// 第一轮只跑第一个任务
	if _, err := h.Run(context.Background(),
		&fnCand{name: "c", fn: func(Task) (string, error) { return "X", nil }}, tasks[:1]); err != nil {
		t.Fatal(err)
	}
	h.Close()

	h2, _ := New(Config{OutPath: out, Resume: true}, nil)
	defer h2.Close()
	calls := &atomic.Int64{}
	rep, _ := h2.Run(context.Background(), &fnCand{name: "c", fn: func(Task) (string, error) {
		calls.Add(1)
		return "X", nil
	}}, tasks)
	if rep.Skipped != 1 || rep.Ran != 1 {
		t.Errorf("应跳过 1 跑 1, 实得 Skipped=%d Ran=%d", rep.Skipped, rep.Ran)
	}
	if calls.Load() != 1 {
		t.Errorf("续跑应只执行 1 次 candidate, 实际 %d 次", calls.Load())
	}
}

// 并发受信号量限制——不限并发会打爆全平台共享的网关配额。
func TestHarness_并发受限(t *testing.T) {
	var cur, peak atomic.Int64
	h, _ := New(Config{Concurrency: 2}, nil)
	defer h.Close()
	var tasks []Task
	for i := 0; i < 12; i++ {
		tasks = append(tasks, Task{ID: string(rune('a' + i)), Objective: string(rune('a' + i)), Expect: []string{"X"}})
	}
	_, _ = h.Run(context.Background(), &fnCand{name: "c", fn: func(Task) (string, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
		return "X", nil
	}}, tasks)
	if peak.Load() > 2 {
		t.Errorf("并发峰值 %d 超过上限 2", peak.Load())
	}
}

// 单任务硬超时：一个任务卡住不能让整轮评测挂死。
func TestHarness_单任务硬超时(t *testing.T) {
	h, _ := New(Config{TaskTimeout: 40 * time.Millisecond}, nil)
	defer h.Close()
	start := time.Now()
	rep, _ := h.Run(context.Background(), &fnCand{name: "slow", fn: func(t Task) (string, error) {
		<-time.After(3 * time.Second) // 远超超时
		return "late", nil
	}}, []Task{{ID: "t", Objective: "o"}})
	if time.Since(start) > 2*time.Second {
		t.Error("硬超时未生效, 整轮被单任务拖住")
	}
	_ = rep
}

// candidate 报错要记进 Err 且不算通过；panic 要被兜住不带走整轮。
func TestHarness_错误与panic兜住(t *testing.T) {
	h, _ := New(Config{}, nil)
	defer h.Close()
	rep, err := h.Run(context.Background(), &fnCand{name: "bad", fn: func(t Task) (string, error) {
		if t.ID == "panic" {
			panic("boom")
		}
		return "", errors.New("produce failed")
	}}, []Task{{ID: "err", Objective: "o1"}, {ID: "panic", Objective: "o2"}})
	if err != nil {
		t.Fatalf("整轮不应因单任务失败而报错: %v", err)
	}
	if len(rep.Results) != 2 {
		t.Fatalf("应有 2 条结果, 实得 %d", len(rep.Results))
	}
	var sawErr, sawPanic bool
	for _, r := range rep.Results {
		if strings.Contains(r.Err, "produce failed") {
			sawErr = true
		}
		if strings.Contains(r.Err, "panic") {
			sawPanic = true
		}
		if r.Passed {
			t.Error("失败任务不应算通过")
		}
	}
	if !sawErr || !sawPanic {
		t.Errorf("错误与 panic 都应被记录: %+v", rep.Results)
	}
}

// Judge 分数越界要被钳到 [0,1]。
func TestHarness_Judge分数钳制(t *testing.T) {
	for _, c := range []struct{ raw, want float64 }{{-5, 0}, {9, 1}, {0.7, 0.7}} {
		h, _ := New(Config{PassScore: 0.6}, &fixedJudge{score: c.raw})
		rep, _ := h.Run(context.Background(),
			&fnCand{name: "c", fn: func(Task) (string, error) { return "out", nil }},
			[]Task{{ID: "t", Objective: "o"}})
		if got := rep.Results[0].Score; got != c.want {
			t.Errorf("Judge 给 %v 应钳到 %v, 实得 %v", c.raw, c.want, got)
		}
		h.Close()
	}
}

// 报告按指纹排序：并发完成顺序不该影响输出（否则报告不可比）。
func TestHarness_报告顺序确定(t *testing.T) {
	var tasks []Task
	for i := 0; i < 8; i++ {
		tasks = append(tasks, Task{ID: string(rune('a' + i)), Objective: string(rune('a' + i)), Expect: []string{"X"}})
	}
	var first []string
	for round := 0; round < 3; round++ {
		h, _ := New(Config{Concurrency: 4}, nil)
		rep, _ := h.Run(context.Background(),
			&fnCand{name: "c", fn: func(Task) (string, error) { return "X", nil }}, tasks)
		var got []string
		for _, r := range rep.Results {
			got = append(got, r.Fingerprint)
		}
		if round == 0 {
			first = got
		} else if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("报告顺序不确定: %v vs %v", got, first)
		}
		h.Close()
	}
}

// LoadTasks 容忍空行与 # 注释；坏行要报出行号。
func TestLoadTasks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tasks.jsonl")
	os.WriteFile(p, []byte(`# 注释行
{"id":"a","objective":"o1"}

{"id":"b","objective":"o2"}
`), 0o644)
	got, err := LoadTasks(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("应读到 2 个任务, 实得 %d", len(got))
	}

	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	os.WriteFile(bad, []byte("{\"id\":\"a\"}\nNOT JSON\n"), 0o644)
	if _, err := LoadTasks(bad); err == nil || !strings.Contains(err.Error(), "第 2 行") {
		t.Errorf("坏行应报出行号, 实得 %v", err)
	}
}

// Compare 是 uplift 因果评估的落点：替代"感觉变好了"。
func TestCompare_uplift(t *testing.T) {
	base := Report{MeanScore: 0.5}
	cand := Report{MeanScore: 0.8}
	if got := Compare(base, cand); got < 0.29 || got > 0.31 {
		t.Errorf("uplift = %v, 期望约 0.3", got)
	}
	if got := Compare(cand, base); got > 0 {
		t.Error("变差时 uplift 应为负")
	}
}
