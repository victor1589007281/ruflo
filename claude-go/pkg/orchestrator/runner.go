package orchestrator

import (
	"context"
	"fmt"
	"sync"
)

// TaskRunner 定义了任务执行器的接口。
// 每种 Runner 封装了特定的执行逻辑 (LLM调用/编译/测试等)。
// Runner 通过 ReadOnlyBlackboard 访问共享状态, 不可直接写入。
type TaskRunner interface {
	Name() string
	Execute(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (output any, err error)
}

// RunnerRegistry 管理命名的 TaskRunner 实例。
// 引擎通过 task.Runner 字段查找对应的执行器。
type RunnerRegistry struct {
	mu      sync.RWMutex
	runners map[string]TaskRunner
}

// NewRunnerRegistry 创建空的注册表。
func NewRunnerRegistry() *RunnerRegistry {
	return &RunnerRegistry{runners: make(map[string]TaskRunner)}
}

// Register 注册一个执行器。名称重复会 panic (启动时即发现配置错误)。
func (r *RunnerRegistry) Register(runner TaskRunner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := runner.Name()
	if _, exists := r.runners[name]; exists {
		panic(fmt.Sprintf("重复的执行器名称: %s", name))
	}
	r.runners[name] = runner
}

// Get 按名称查找执行器。
func (r *RunnerRegistry) Get(name string) (TaskRunner, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	runner, ok := r.runners[name]
	if !ok {
		return nil, fmt.Errorf("执行器未找到: %s", name)
	}
	return runner, nil
}

// List 返回所有已注册的执行器名称。
func (r *RunnerRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.runners))
	for n := range r.runners {
		names = append(names, n)
	}
	return names
}

// ---- 内置执行器 ----

// FuncRunner 将普通函数包装为 TaskRunner, 适合快速原型或测试。
type FuncRunner struct {
	name string
	fn   func(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error)
}

func NewFuncRunner(name string, fn func(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error)) *FuncRunner {
	return &FuncRunner{name: name, fn: fn}
}

func (r *FuncRunner) Name() string { return r.name }
func (r *FuncRunner) Execute(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error) {
	return r.fn(ctx, task, bb)
}

// NoopRunner 空操作执行器, 用于 DAG 中的同步点和汇合节点。
type NoopRunner struct{}

func (r *NoopRunner) Name() string { return "noop" }
func (r *NoopRunner) Execute(_ context.Context, _ *Task, _ ReadOnlyBlackboard) (any, error) {
	return "noop", nil
}

// CompositeRunner 组合执行器: 按顺序链式调用多个 Runner。
//
// 典型用法: 对抗循环 (Adversarial Loop)
//
//	steps: [coder, reviewer, tester]
//	policy: AdaptiveTerminator (质量达标时终止)
//	maxIter: 5
//
// 每次迭代依次执行所有 steps, 直到 policy 判定终止或达到 maxIter。
// 历史记录 (history) 传给 policy, 用于检测收敛/退化。
type CompositeRunner struct {
	name    string
	steps   []TaskRunner        // 每轮迭代要执行的步骤列表
	policy  TerminationPolicy   // 终止策略 (nil = 直接执行 maxIter 轮)
	maxIter int                 // 最大迭代次数 (硬上限)
}

// TerminationPolicy 决定迭代式执行器何时停止。
// 可实现: 质量达标、收敛检测、退化检测等策略。
type TerminationPolicy interface {
	ShouldTerminate(iteration int, lastOutput any, history []any) bool
}

// MaxIterTermination 固定轮数终止策略: 执行满 Max 轮后停止。
type MaxIterTermination struct {
	Max int
}

func (t *MaxIterTermination) ShouldTerminate(iteration int, _ any, _ []any) bool {
	return iteration >= t.Max
}

// NewCompositeRunner 创建一个组合执行器。
func NewCompositeRunner(name string, steps []TaskRunner, policy TerminationPolicy, maxIter int) *CompositeRunner {
	return &CompositeRunner{name: name, steps: steps, policy: policy, maxIter: maxIter}
}

func (r *CompositeRunner) Name() string { return r.name }

// CompositeRunner 组合执行器: 按顺序链式调用多个 Runner。
//
// 迭代循环时序图 (以 adversarial coder→reviewer 为例):
//
//	Iter 0:
//	  coder.Execute()     → 生成初版代码, 写入 bb
//	  reviewer.Execute()  → 审查代码, 返回评审意见
//	  history = [评审意见]
//	  policy.ShouldTerminate(1, 评审意见, history) → false (质量未达标)
//
//	Iter 1:
//	  coder.Execute()     → 接收评审意见, 改进代码
//	  reviewer.Execute()  → 重新审查
//	  history = [评审意见_0, 评审意见_1]
//	  policy.ShouldTerminate(2, 新意见, history) → true (收敛或达标)
//
//	终止策略 (TerminationPolicy):
//	  - MaxIterTermination: 固定轮数, 最简单
//	  - QualityTermination: 质量分数达标/收敛/退化检测
//
// 注意事项:
//   - 每一步之间检查 ctx.Done(), 支持外部取消
//   - bb 是 ReadOnlyBlackboard, 步骤间通过 bb 共享状态
//   - 任何一步失败, 整个复合任务立即失败返回
func (r *CompositeRunner) Execute(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error) {
	var history []any
	var lastOutput any

	for iter := 0; iter < r.maxIter; iter++ {
		// 终止条件检查 (在执行前)
		if r.policy != nil && r.policy.ShouldTerminate(iter, lastOutput, history) {
			break
		}
		for _, step := range r.steps {
			select {
			case <-ctx.Done():
				return lastOutput, ctx.Err()
			default:
			}
			out, err := step.Execute(ctx, task, bb)
			if err != nil {
				return lastOutput, fmt.Errorf("组合步骤 %s 第 %d 轮失败: %w", step.Name(), iter, err)
			}
			lastOutput = out
		}
		history = append(history, lastOutput)
	}
	return lastOutput, nil
}
