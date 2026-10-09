// ab.go —— H4-2: 配对 A/B 台。
//
// # 台子的形状
//
// 一次 run 由 Runner 承担 —— 一个把 (任务, 输入) 变成 (是否成功, 读数) 的函数。
// 台子本身只管三件事:
//
//  1. 按 TaskIDs 的**稳定顺序**把每个任务喂给两组 (配对的前提);
//  2. 每次跑之前**重新展开** fixture (两组拿到逐字节相同的初始代码);
//  3. 跑完把两组的输入 diff 出来核一遍、把四格与区间算出来。
//
// 把 Runner 抽出来是为了让这一台能在**不起真实 run** 的前提下被单测:
// 统计算错了、配对跑歪了、两组输入悄悄不一致了 —— 这些都能用一个假 Runner 立刻
// 验出来, 不用等一次真实 run 跑完才发现"哦原来我算错了"。
//
// 真实 Runner (env-gated) 在 run.go。
package longhorizon

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// TaskOutcome 一个任务的运行读数。
//
// Passed 是**客观终态**: 验证命令的退出码。其余字段是判"这个任务够不够长程"的
// 读数 —— 它们不影响成功率, 但方案 §21.5.6 对 H4-1 的验收 (轮数 > 100、
// compact span > 0) 落在它们身上。
type TaskOutcome struct {
	Passed bool
	// Turns 实际回合数。
	Turns int
	// CompactSpans 运行期间发生的上下文折叠次数 (H2-1 的读数)。
	CompactSpans int
	// Tokens 上下文峰值 (估算)。
	Tokens int
	// Err 运行本身的错误 (与 Passed 不同: 运行崩了也算 Passed=false, 但归因不同)。
	Err string
}

// Runner 一次真实运行。输入是任务与输入规格, 输出是读数。
//
// 实现者**必须**保证: 同一份 RunSpec 对不同任务互不影响 (每个任务一个干净目录),
// 否则第二组的第一个任务会看到第一组的残留。
type Runner interface {
	Run(task Task, spec RunSpec, workDir string) (TaskOutcome, error)
}

// RunnerFunc 适配普通函数。
type RunnerFunc func(task Task, spec RunSpec, workDir string) (TaskOutcome, error)

// Run 见 Runner。
func (f RunnerFunc) Run(task Task, spec RunSpec, workDir string) (TaskOutcome, error) {
	return f(task, spec, workDir)
}

// Materializer 准备一个任务的工作目录 (默认展开 Task.Fixture)。
// 抽出来是为了让台子在单测里能被喂一个"必定成功"的假物化器。
type Materializer func(dir string, task Task) error

// DefaultMaterializer 展开 fixture 到 dir。
func DefaultMaterializer(dir string, task Task) error {
	return Materialize(dir, task.Fixture)
}

// ABReport 一次完整配对 A/B 的结果 + 长程读数的汇总。
type ABReport struct {
	// Pair 配对统计的底座。
	Pair PairedResult
	// Diff 两组输入的差异说明 ("配置 diff 可查"的落地)。
	Diff string
	// Outcomes 逐任务的两组读数 (任务 ID -> 组 -> 读数)。
	Outcomes map[string]map[string]TaskOutcome
	// LongHorizon 长程验收 (方案 §21.5.6 H4-1) 的汇总。
	LongHorizon LongHorizonSummary
}

// LongHorizonSummary 任务集够不够"长程"的读数。
type LongHorizonSummary struct {
	// Evaluated 参与统计的任务数 (两组都跑到)。
	Evaluated int
	// TurnsBelowMin 轮数没到 MinTurns 的任务 (ID -> 两组的轮数)。
	TurnsBelowMin map[string][2]int
	// NoCompact 全程没有发生折叠的任务 (ID -> 两组的折叠次数)。
	// 这些任务**没有测到**"跨压缩存活", 它们的成功不算长程的成功。
	NoCompact map[string][2]int
}

// MeetsAcceptance 是否满足 H4-1 的验收口径 (每个任务轮数 > 100 且撞到压缩阈值)。
//
// 判据按**组**给: 一组过了就算这个任务在那一组上够长程。两组取并集是刻意的 ——
// "任务够不够长程"是任务集的性质, 不该因为某一组提前放弃而判它不合格
// (那一组的提前放弃恰恰是要被测的现象)。
func (s LongHorizonSummary) MeetsAcceptance() bool {
	return s.Evaluated > 0 && len(s.TurnsBelowMin) == 0 && len(s.NoCompact) == 0
}

// Describe 人可读的长程验收报告。
func (s LongHorizonSummary) Describe() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "长程验收 (方案 §21.5.6): 参与 %d 个任务\n", s.Evaluated)
	if len(s.TurnsBelowMin) == 0 {
		fmt.Fprintf(&sb, "  ✓ 每个任务都达到了 %d 轮以上\n", MinLongHorizonTurns)
	} else {
		fmt.Fprintf(&sb, "  ✗ %d 个任务的轮数不足 %d 轮 (A/B 两组轮数):\n",
			len(s.TurnsBelowMin), MinLongHorizonTurns)
		for _, id := range sortedKeys2(s.TurnsBelowMin) {
			fmt.Fprintf(&sb, "      %s: A=%d B=%d\n", id, s.TurnsBelowMin[id][0], s.TurnsBelowMin[id][1])
		}
	}
	if len(s.NoCompact) == 0 {
		fmt.Fprintf(&sb, "  ✓ 每个任务都撞到了压缩阈值 (compact span > 0)\n")
	} else {
		fmt.Fprintf(&sb, "  ✗ %d 个任务全程没发生折叠 —— 它们没测到「跨压缩存活」:\n", len(s.NoCompact))
		for _, id := range sortedKeys2(s.NoCompact) {
			fmt.Fprintf(&sb, "      %s: A=%d B=%d 次\n", id, s.NoCompact[id][0], s.NoCompact[id][1])
		}
	}
	return sb.String()
}

func sortedKeys2(m map[string][2]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RunPair 跑一次配对 A/B。
//
// 顺序 (每一处都是配对能不能成立的关键):
//
//  1. **先核输入** —— 两组输入不合法就直接停, 不浪费一次真实 run。
//  2. 按 TaskIDs 的稳定顺序逐个任务跑两组 (任务 A 的两组紧邻, 而不是先跑完
//     A 组全程再跑 B 组) —— 这样上游客栈/缓存的漂移落在**任务内**,
//     成为配对能消掉的方差; 反过来它就成了两组之间的系统差异。
//  3. 每个任务、每一组各用一个**独立**的临时目录, 且都是从 fixture 重新展开的。
func RunPair(pair ConfigPair, tasks []Task, r Runner, mat Materializer) (ABReport, error) {
	var rep ABReport

	if err := pair.AssertOnlyKnobDiffers(); err != nil {
		return rep, fmt.Errorf("A/B 输入不合规, 未开跑: %w", err)
	}
	if r == nil {
		return rep, fmt.Errorf("Runner 为空 —— 没有东西会真的跑任务")
	}
	if len(tasks) == 0 {
		return rep, fmt.Errorf("任务集为空")
	}
	if mat == nil {
		mat = DefaultMaterializer
	}

	aOK := make(map[string]bool, len(tasks))
	bOK := make(map[string]bool, len(tasks))
	rep.Outcomes = make(map[string]map[string]TaskOutcome, len(tasks))
	rep.LongHorizon = LongHorizonSummary{
		TurnsBelowMin: map[string][2]int{},
		NoCompact:     map[string][2]int{},
	}
	rep.Diff = pair.DiffSummary()

	// 稳定顺序 (TaskIDs 的语义) —— 也接受调用方传入的子集, 但保持其声明顺序。
	for _, task := range tasks {
		outs := map[string]TaskOutcome{}
		for _, arm := range []struct {
			key  string
			spec RunSpec
		}{{"A", pair.A}, {"B", pair.B}} {
			dir, cleanup, err := WorkDirFactory(task)
			if err != nil {
				return rep, err
			}
			if err := mat(dir, task); err != nil {
				cleanup()
				return rep, fmt.Errorf("任务 %s 的 %s 组: 准备工作目录失败: %w", task.ID, arm.key, err)
			}
			outcome, runErr := r.Run(task, arm.spec, dir)
			cleanup()
			err = runErr
			if err != nil {
				// 运行本身出错**不丢样本** —— 记成失败并留下原因。丢掉它会让分母
				// 变小 (成功率虚高), 而那正是配对最该防的形态。
				outcome = TaskOutcome{Passed: false, Err: err.Error()}
			}
			outs[arm.key] = outcome
		}
		rep.Outcomes[task.ID] = outs
		aOK[task.ID] = outs["A"].Passed
		bOK[task.ID] = outs["B"].Passed

		rep.LongHorizon.Evaluated++
		// 轮数与折叠按"两组里做得更充分的那个"算: 任务够不够长程是任务集的性质。
		at, bt := outs["A"].Turns, outs["B"].Turns
		if at < task.MinTurns && bt < task.MinTurns {
			rep.LongHorizon.TurnsBelowMin[task.ID] = [2]int{at, bt}
		}
		ac, bc := outs["A"].CompactSpans, outs["B"].CompactSpans
		if ac == 0 && bc == 0 {
			rep.LongHorizon.NoCompact[task.ID] = [2]int{ac, bc}
		}
	}

	res, err := BuildPairedResult(pair.Knob.Name, aOK, bOK)
	if err != nil {
		return rep, err
	}
	rep.Pair = res
	return rep, nil
}

// WorkDirFactory 分配一个干净的工作目录 (返回的 cleanup 必须被调用)。
// 抽成变量是为了让单测喂一个只读的假目录, 不碰真实文件系统。
var WorkDirFactory = func(task Task) (string, func(), error) {
	dir, err := os.MkdirTemp("", "h42-"+task.ID+"-")
	if err != nil {
		return "", nil, fmt.Errorf("建临时目录失败: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// Report 完整报告 (配对统计 + 长程验收 + 配置 diff)。
func (r ABReport) Report() string {
	var sb strings.Builder
	sb.WriteString(r.Pair.Report())
	sb.WriteString("\n" + r.LongHorizon.Describe())
	sb.WriteString("\n两组输入差异 (「配置 diff 可查」):\n")
	sb.WriteString(r.Diff)
	return sb.String()
}
