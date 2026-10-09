// budget_retry_test.go — P0-1 / H0-5 在**团队路径**上的接线验收。
//
// pkg/engine 那两条测的是"引擎自己会不会因为预算停下"; 这里测的是另一半:
// 引擎已经停下来了, **上层的两个重试环会不会又把它拉起来**。
//
// 为什么这是必须单独测的一条:
//
//	预算闸门是**事后记账** —— 调用发生之后才看累计值。所以"停下来"不是一次性的,
//	它是一个持续为真的状态。引擎返回之后, agent 包里有**两层**重试环
//	(WorkflowExecutor.executeStageWithRetry 内层, Coordinator.executeStageWithRetry 外层)。
//	内层由既有的 isStageTransientError 接住 (文案里的 "retry" 命中了瞬态模式表,
//	于是落进"永久错误不重试"分支 —— 这条依赖是隐式的, workflow.go 里有一个断言把它
//	变成显式的)。**外层没有任何东西接得住**: 它按 c.maxRetries 无条件再跑几遍,
//	每一遍都是一次真的模型调用。所以分流点必须在外层, 且必须靠 cause 而不是文本 ——
//	"预算耗尽"的文案里带着 limit/spent 两个数字, 归一化后的指纹每次重试都不同,
//	文本判据会把它当成"新原因"而继续重试。
//
//	后果不是"多跑一次", 是闸门刚判完"这个 run 花超了", 重试环立刻再花一笔 ——
//	闸门的全部意义被抵消。这也是 H0-4 记在案的那类失败 (48 分钟墙钟空转) 的最贵版本。
//
// 判据落在**读取侧**: 假 runner 自己数被调用了几次。不是"字段被赋了什么值"。
package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
)

// countingRunner 是一个必然失败的假 runner, 自己数调用次数。
type countingRunner struct {
	calls int
	err   error
}

func (r *countingRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	r.calls++
	return "", r.err
}

// budgetTestTeam 造一个最小可跑的 team: 所有可选的协作者 (Blackboard / dataDir /
// taskTracker) 全部留空 —— 留空是刻意的, 空 team 才是"重试环本身"的裸读法,
// 不会被持久化路径的噪声干扰。
func budgetTestTeam() *ProductionTeam {
	return &ProductionTeam{
		Name:     "t",
		Workflow: "pipeline",
		Agents: map[string]*BGAgent{
			"coder": {Name: "coder", Role: "coder"},
		},
	}
}

func budgetTestStage() StageDef {
	return StageDef{Name: "implement", Agent: "coder", Prompt: "做 {objective}"}
}

// budgetTestExecutor 造一个接上假 runner 的 WorkflowExecutor。
func budgetTestExecutor(runner *countingRunner) *WorkflowExecutor {
	return &WorkflowExecutor{
		factory: func(ctx context.Context, role, systemPrompt string) (AgentRunner, error) {
			return runner, nil
		},
		notify: func(_, _ string) {},
	}
}

// 判据 1: 预算耗尽从 runner 一路到外层 StageResult, runner 只被调一次。
//
// 对照臂写进同一条测试: 同样的失败换成一条普通瞬态错误, runner 必须被调**多于**
// 一次。没有这个对照, "只调了一次"完全可能是因为重试逻辑压根没生效, 这条测试就会
// 在重试环被整个删掉时依然通过。
//
// 这条同时钉住了 cause 的**穿透链**: runner → runAgent (加 cause) → 外层
// Coordinator 分流。链上任何一环把 error 换成字符串, 外层就再也认不出哨兵。
func TestH05_BudgetFailureStopsBothRetryLoops(t *testing.T) {
	run := func(runnerErr error) (int, StageResult, map[string]*Checkpoint) {
		runner := &countingRunner{err: runnerErr}
		we := budgetTestExecutor(runner)
		c := &Coordinator{
			notify:      func(_, _ string) {},
			chatID:      "c",
			checkpoints: map[string]*Checkpoint{},
			maxRetries:  3,
		}
		sr := c.executeStageWithRetry(context.Background(), budgetTestStage(), "目标", nil,
			budgetTestTeam(), we)
		c.mu.Lock()
		defer c.mu.Unlock()
		return runner.calls, sr, c.checkpoints
	}

	budgetCalls, budgetSR, cps := run(&internal_hook.RunBudgetError{Limit: 100, Spent: 250})

	if budgetSR.Status != TaskFailed {
		t.Fatalf("前置条件失败: 预算耗尽应当判失败, got %+v", budgetSR.Status)
	}
	if budgetCalls != 1 {
		t.Fatalf("预算耗尽后 runner 被调用了 %d 次, 期望 1 —— 重试环把闸门刚拦下的 run 又拉起来了",
			budgetCalls)
	}
	if !errors.Is(budgetSR.cause, internal_hook.ErrRunBudgetExhausted) {
		t.Fatalf("返回的 StageResult 丢了哨兵 (got %v) —— 文本不是类型, "+
			"上层再想区分预算耗尽与输出不合格就只能猜字符串", budgetSR.cause)
	}

	// 检查点必须是终态。"预算耗尽"与"永远停在 running"在磁盘上完全同形 ——
	// 进程重启后 resume 会以为这个阶段还在跑。分流的三条出口都会直接 return,
	// 落检查点必须排在分流之前。
	if cp := cps["implement"]; cp == nil {
		t.Fatal("没有落检查点")
	} else if cp.Status == "running" {
		t.Fatalf("检查点停在 %q —— 预算终止的出口漏了 saveCheckpoint, "+
			"磁盘上留下'这个阶段正在跑'而进程早已退出", cp.Status)
	}

	// 对照臂: 普通瞬态错误必须被重试放大 (内层 3 次 × 外层 4 次)。
	transientCalls, _, _ := run(fmt.Errorf("connection reset by peer"))
	if transientCalls <= budgetCalls {
		t.Fatalf("对照臂失败: 普通瞬态错误只被调用 %d 次 (预算臂 %d) —— "+
			"'只调一次'不是预算判据造成的", transientCalls, budgetCalls)
	}
}

// 判据 2: maxRetries = 0 时预算终止仍必须保住 cause。
//
// 这条守的是一个极易漏的配置边界: 判据 1 走的是默认 maxRetries (≥1), 于是
// classifyStageFailureWith 会被调用, 它恰好也认预算 ⇒ 判据 1 在"缺了显式分支"时
// 依然能通过。但 maxRetries=0 时那个调用点整块不执行, 循环直接落到末尾返回一句
// 合成的 "超过最大重试次数 (0)", cause 被丢掉 —— 台账上"预算花超了"变成
// "重试次数用完了", 这是两件不同的事, 且后者会让人去调大重试次数。
func TestH05_BudgetSurvivesZeroMaxRetries(t *testing.T) {
	runner := &countingRunner{err: &internal_hook.RunBudgetError{Limit: 100, Spent: 250}}
	we := budgetTestExecutor(runner)
	c := &Coordinator{
		notify:      func(_, _ string) {},
		chatID:      "c",
		checkpoints: map[string]*Checkpoint{},
		maxRetries:  0, // 分类器一次都不会被调用
	}
	sr := c.executeStageWithRetry(context.Background(), budgetTestStage(), "目标", nil,
		budgetTestTeam(), we)

	if runner.calls != 1 {
		t.Fatalf("runner 被调用了 %d 次, 期望 1", runner.calls)
	}
	if !errors.Is(sr.cause, internal_hook.ErrRunBudgetExhausted) {
		t.Fatalf("cause 丢了 (got %v, Error=%q) —— 预算终止被记成了'重试次数用完'",
			sr.cause, sr.Error)
	}
}

// 判据 3: 分类器在**没有 cause** 时不得凭文本猜出"预算耗尽"。
//
// 这条守的是分流的**输入侧**: 如果哪天有人图省事, 改成在 classifyStageFailure 里
// 匹配 "budget"/"预算" 这些词, 那么任何一条恰好在正文里提到这两个字的模型输出都会
// 被当成终态而放弃重试。文本不是类型, 这条钉住这个边界。
func TestH05_ClassifierDoesNotGuessBudgetFromText(t *testing.T) {
	// 文案与真实哨兵的错误串一字不差, 但 cause 为 nil (旧调用方 / 反序列化回来的
	// StageResult 就是这种形态)。
	text := "run token budget exhausted: limit=100 spent=250"

	action, _, _ := classifyStageFailureWith(text, nil, "", false)
	if action == stageRetryAbort {
		t.Fatalf("仅凭文本就判成不可重试的终态 (action=%v) —— "+
			"那么任何正文里提到 budget 的失败都会被静默放弃重试", action)
	}

	// 同一个文案, 带上哨兵 ⇒ 必须分流。这条与上面那条成对, 否则可能只是
	// classifyStageFailureWith 恒不返回 abort 而已。
	wrapped := fmt.Errorf("阶段失败: %w", &internal_hook.RunBudgetError{Limit: 100, Spent: 250})
	action2, _, why := classifyStageFailureWith(wrapped.Error(), wrapped, "", false)
	if action2 != stageRetryAbort {
		t.Fatalf("带哨兵却没有被判成终态 (action=%v, why=%q)", action2, why)
	}
}

// 判据 4: 预算终止的等待时间是 0 —— 它不该和限流一样先睡一觉。
//
// 这条是可读性最弱但代价最直接的一条: 重试环里限流路径带了 30s 起的固定退避,
// 如果预算失败被误判进那条分支, 每个阶段白等半分钟, 而等的东西**永远不会好**。
func TestH05_BudgetFailureReturnsPromptly(t *testing.T) {
	runner := &countingRunner{err: &internal_hook.RunBudgetError{Limit: 100, Spent: 250}}
	we := budgetTestExecutor(runner)
	start := time.Now()
	_ = we.executeStageWithRetry(context.Background(), budgetTestStage(), "目标", "目标",
		budgetTestTeam(), nil)

	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("预算终止耗时 %s —— 它走进了带退避的重试分支", d)
	}
}
