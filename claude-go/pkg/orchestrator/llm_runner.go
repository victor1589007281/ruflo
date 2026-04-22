package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
)

// LLMClient 定义 LLM 调用接口。
// 与 pkg/api.Client 和 pkg/agent.LLMClient 通过 duck typing 兼容。
type LLMClient interface {
	SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// LLMRunner 将 LLM 调用封装为 TaskRunner。
//
// 核心机制:
//   - 从 task.Config["system_prompt"] 读取系统提示词
//   - 从 task.Config["user_prompt"] 读取用户提示词 (支持模板变量)
//   - 自动从 Blackboard 读取上游输出, 替换 {prev_result} 和 {dep:taskID} 占位符
//   - 输出为 LLM 原始文本响应
type LLMRunner struct {
	name string
	llm  LLMClient
}

func NewLLMRunner(name string, llm LLMClient) *LLMRunner {
	return &LLMRunner{name: name, llm: llm}
}

func (r *LLMRunner) Name() string { return r.name }

func (r *LLMRunner) Execute(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error) {
	systemPrompt, _ := task.Config["system_prompt"].(string)
	userPrompt, _ := task.Config["user_prompt"].(string)
	objective, _ := task.Config["objective"].(string)

	userPrompt = r.resolveTemplate(userPrompt, objective, task, bb)

	if systemPrompt == "" {
		systemPrompt = "你是一个专业的AI助手。"
	}
	if userPrompt == "" {
		userPrompt = objective
	}

	resp, err := r.llm.SimpleComplete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// resolveTemplate 替换 prompt 中的模板变量:
//   - {objective} → 任务目标
//   - {prev_result} → 所有上游输出的拼接
//   - {dep:taskID} → 指定上游任务的输出
func (r *LLMRunner) resolveTemplate(prompt, objective string, task *Task, bb ReadOnlyBlackboard) string {
	prompt = strings.ReplaceAll(prompt, "{objective}", objective)

	if strings.Contains(prompt, "{prev_result}") {
		var parts []string
		for _, depID := range task.DependsOn {
			if val, _, err := bb.Read(depID + "/output"); err == nil {
				parts = append(parts, fmt.Sprintf("[%s]\n%v", depID, val))
			}
		}
		prompt = strings.ReplaceAll(prompt, "{prev_result}", strings.Join(parts, "\n\n"))
	}

	for _, depID := range task.DependsOn {
		placeholder := fmt.Sprintf("{dep:%s}", depID)
		if strings.Contains(prompt, placeholder) {
			if val, _, err := bb.Read(depID + "/output"); err == nil {
				prompt = strings.ReplaceAll(prompt, placeholder, fmt.Sprintf("%v", val))
			}
		}
	}

	return prompt
}

// ---- 质量评分与终止策略 ----

// QualityScore 审查/测试质量评分 (由 QualityScorer 从 LLM 输出中提取)。
type QualityScore struct {
	Pass        bool    `json:"pass"`
	Overall     float64 `json:"overall"`      // 0-10
	Completeness float64 `json:"completeness"` // 0-10
	Correctness float64 `json:"correctness"`  // 0-10
	Feedback    string  `json:"feedback"`
}

// QualityScorer 从 LLM 的评审输出中提取质量评分。
type QualityScorer interface {
	Score(reviewOutput any) QualityScore
}

// JSONQualityScorer 从 JSON 格式的输出中解析 QualityScore。
// 如果输出不是 JSON, 则用关键词启发式评分。
type JSONQualityScorer struct{}

func (s *JSONQualityScorer) Score(output any) QualityScore {
	text := fmt.Sprintf("%v", output)

	var score QualityScore
	if json.Unmarshal([]byte(text), &score) == nil && score.Overall > 0 {
		return score
	}

	// 启发式: 从文本中推断质量
	score.Overall = 6.0
	score.Completeness = 6.0
	score.Correctness = 6.0
	score.Pass = true

	lower := strings.ToLower(text)
	if strings.Contains(lower, "blocker") || strings.Contains(lower, "critical") ||
		strings.Contains(lower, "不通过") || strings.Contains(lower, "failed") {
		score.Pass = false
		score.Overall = 4.0
	}
	if strings.Contains(lower, "passed") || strings.Contains(lower, "通过") ||
		strings.Contains(lower, "合格") {
		score.Pass = true
		score.Overall = 8.0
	}
	score.Feedback = text
	return score
}

// QualityTermination 基于质量评分的自适应终止策略。
//
// 终止条件 (满足任一即停):
//  1. 质量达标: 最近评分 >= PassThreshold 且 Pass=true
//  2. 收敛检测: 最近 3 轮评分变化 < ConvergeDelta (分数不再提升)
//  3. 退化检测: 连续 2 轮评分下降
type QualityTermination struct {
	PassThreshold float64
	ConvergeDelta float64
	Scorer        QualityScorer
	mu            sync.Mutex
	scores        []QualityScore
}

func NewQualityTermination(passThreshold, convergeDelta float64, scorer QualityScorer) *QualityTermination {
	if scorer == nil {
		scorer = &JSONQualityScorer{}
	}
	return &QualityTermination{
		PassThreshold: passThreshold,
		ConvergeDelta: convergeDelta,
		Scorer:        scorer,
	}
}

func (t *QualityTermination) ShouldTerminate(iteration int, lastOutput any, history []any) bool {
	if lastOutput == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	score := t.Scorer.Score(lastOutput)
	t.scores = append(t.scores, score)

	// 条件 1: 质量达标
	if score.Pass && score.Overall >= t.PassThreshold {
		return true
	}

	n := len(t.scores)

	// 条件 2: 收敛 (最近 3 轮变化很小)
	if n >= 3 {
		d1 := math.Abs(t.scores[n-1].Overall - t.scores[n-2].Overall)
		d2 := math.Abs(t.scores[n-2].Overall - t.scores[n-3].Overall)
		if d1 < t.ConvergeDelta && d2 < t.ConvergeDelta {
			return true
		}
	}

	// 条件 3: 退化 (连续 2 轮下降)
	if n >= 3 {
		if t.scores[n-1].Overall < t.scores[n-2].Overall &&
			t.scores[n-2].Overall < t.scores[n-3].Overall {
			return true
		}
	}

	return false
}

// Scores 返回已收集的所有质量评分 (线程安全)。
func (t *QualityTermination) Scores() []QualityScore {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := make([]QualityScore, len(t.scores))
	copy(cp, t.scores)
	return cp
}

// ---- 对抗循环执行器 (增强版 CompositeRunner) ----

// AdversarialRunner 实现真正的多角色对抗循环, 带反馈回路。
//
// 与 CompositeRunner 的区别:
//  1. 每轮 generator 输出会注入到 evaluator 的输入 (通过 Blackboard)
//  2. evaluator 的反馈会注入到下一轮 generator 的输入
//  3. 使用 QualityTermination 做自适应终止
//  4. 支持多评审角色 (Red/Blue/Skeptic)
//
// 对抗流程:
//
//	Round N:
//	  generator.Execute(task + 上轮feedback) → output
//	  bb.Write("adv/round-N/output", output)
//	  for _, reviewer := range reviewers:
//	    reviewer.Execute(task + output) → review
//	  bb.Write("adv/round-N/feedback", mergedReview)
//	  if QualityTermination.ShouldTerminate → break
type AdversarialRunner struct {
	name       string
	generator  TaskRunner   // 生成器 (coder/writer/etc)
	reviewers  []TaskRunner // 审查者列表 (reviewer/skeptic/red/blue)
	policy     *QualityTermination
	maxRounds  int
}

func NewAdversarialRunner(
	name string,
	generator TaskRunner,
	reviewers []TaskRunner,
	policy *QualityTermination,
	maxRounds int,
) *AdversarialRunner {
	if maxRounds <= 0 {
		maxRounds = 5
	}
	return &AdversarialRunner{
		name:      name,
		generator: generator,
		reviewers: reviewers,
		policy:    policy,
		maxRounds: maxRounds,
	}
}

func (r *AdversarialRunner) Name() string { return r.name }

func (r *AdversarialRunner) Execute(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error) {
	// 需要可写的 Blackboard 来传递轮间状态
	writable, ok := bb.(WritableBlackboard)
	if !ok {
		return nil, fmt.Errorf("AdversarialRunner 需要可写的 Blackboard")
	}

	prefix := fmt.Sprintf("adv/%s", task.ID)
	var lastOutput any
	var lastFeedback string
	var history []any

	for round := 1; round <= r.maxRounds; round++ {
		select {
		case <-ctx.Done():
			return lastOutput, ctx.Err()
		default:
		}

		// 注入上轮反馈到 task config
		if lastFeedback != "" {
			if task.Config == nil {
				task.Config = make(map[string]any)
			}
			task.Config["adversarial_feedback"] = lastFeedback
			task.Config["adversarial_round"] = round
		}

		// 1. Generator 执行
		genOutput, err := r.generator.Execute(ctx, task, bb)
		if err != nil {
			return lastOutput, fmt.Errorf("generator 第 %d 轮失败: %w", round, err)
		}
		lastOutput = genOutput
		writable.Write(
			fmt.Sprintf("%s/round-%d/output", prefix, round),
			genOutput,
			WriteMeta{Author: r.generator.Name(), Category: "adversarial"},
		)

		// 2. 所有 Reviewer 评审 (可并行, 但为保证确定性串行)
		var allReviews []string
		task.Config["review_target"] = genOutput
		for _, reviewer := range r.reviewers {
			review, rErr := reviewer.Execute(ctx, task, bb)
			if rErr != nil {
				allReviews = append(allReviews, fmt.Sprintf("[%s] 评审失败: %v", reviewer.Name(), rErr))
				continue
			}
			allReviews = append(allReviews, fmt.Sprintf("[%s]\n%v", reviewer.Name(), review))
		}
		mergedFeedback := strings.Join(allReviews, "\n\n---\n\n")
		lastFeedback = mergedFeedback
		writable.Write(
			fmt.Sprintf("%s/round-%d/feedback", prefix, round),
			mergedFeedback,
			WriteMeta{Author: "adversarial", Category: "feedback"},
		)

		// 3. 质量评分 + 终止检查
		history = append(history, lastOutput)
		if r.policy != nil && r.policy.ShouldTerminate(round, lastOutput, history) {
			break
		}
	}

	return lastOutput, nil
}

// WritableBlackboard 扩展 ReadOnlyBlackboard 以支持写入。
// AdversarialRunner 需要在轮间写入状态。
type WritableBlackboard interface {
	ReadOnlyBlackboard
	Write(key string, value any, meta WriteMeta) (int64, error)
}

// ---- LLM 驱动的 DAG 裂变 ----

// LLMExpander 使用 LLM 分析任务输出, 决定是否需要创建子任务。
//
// 工作原理:
//  1. 任务完成后, 将输出发给 LLM
//  2. LLM 分析是否需要拆分子任务
//  3. 如果需要, 返回新的 Task 列表和 Edge 列表
//  4. ExpanderHook 将新任务注入到图中, 引擎继续调度
//
// 典型场景:
//   - 测试团队: 测试计划 → LLM 拆分为具体测试函数任务
//   - 审查团队: 代码分析 → LLM 决定需要哪些专项审查
type LLMExpander struct {
	llm       LLMClient
	runnerName string // 新任务使用的 Runner 名称
	maxExpand int     // 单次最多裂变的任务数
}

func NewLLMExpander(llm LLMClient, runnerName string, maxExpand int) *LLMExpander {
	if maxExpand <= 0 {
		maxExpand = 10
	}
	return &LLMExpander{llm: llm, runnerName: runnerName, maxExpand: maxExpand}
}

// ExpandRequest 是 LLM 裂变请求的 JSON 结构。
type ExpandRequest struct {
	Tasks []ExpandTask `json:"tasks"`
}

type ExpandTask struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Prompt   string `json:"prompt"`
	Priority int    `json:"priority"`
}

func (e *LLMExpander) OnTaskComplete(task *Task, output any) ([]*Task, []Edge) {
	shouldExpand, _ := task.Config["expandable"].(bool)
	if !shouldExpand {
		return nil, nil
	}

	ctx := context.Background()
	prompt := fmt.Sprintf(`分析以下任务输出, 判断是否需要拆分为子任务。

任务: %s
输出:
%v

如果需要拆分, 返回 JSON:
{"tasks":[{"id":"sub-xxx","name":"子任务名","prompt":"子任务提示词","priority":5}]}

如果不需要拆分, 返回:
{"tasks":[]}

只返回 JSON, 不要其他内容。`, task.Name, output)

	resp, err := e.llm.SimpleComplete(ctx, "你是一个任务分解专家。分析任务输出决定是否需要进一步拆分。", prompt)
	if err != nil {
		return nil, nil
	}

	var req ExpandRequest
	resp = extractJSON(resp)
	if json.Unmarshal([]byte(resp), &req) != nil || len(req.Tasks) == 0 {
		return nil, nil
	}

	if len(req.Tasks) > e.maxExpand {
		req.Tasks = req.Tasks[:e.maxExpand]
	}

	var newTasks []*Task
	var newEdges []Edge
	for _, et := range req.Tasks {
		t := &Task{
			ID:       fmt.Sprintf("%s/%s", task.ID, et.ID),
			Name:     et.Name,
			Priority: et.Priority,
			Runner:   e.runnerName,
			Config: map[string]any{
				"system_prompt": et.Prompt,
				"user_prompt":   fmt.Sprintf("%v", output),
				"objective":     et.Name,
			},
		}
		newTasks = append(newTasks, t)
		newEdges = append(newEdges, Edge{From: task.ID, To: t.ID, Kind: EdgeDependency})
	}
	return newTasks, newEdges
}

func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

// ---- Per-Runner 并发池管理 ----

// RunnerPool 管理每种 Runner 的并发度。
//
// 不同于全局的 BackpressureCtrl, RunnerPool 按角色控制并发:
//   - LLM 调用: 受 RPM 限制, 并发度较低
//   - 本地计算: 无限制
//   - 同一角色的多个任务共享池
type RunnerPool struct {
	mu       sync.Mutex
	limits   map[string]int           // runner name → 最大并发
	active   map[string]int           // runner name → 当前活跃数
	waiters  map[string][]chan struct{} // runner name → 等待队列
}

func NewRunnerPool() *RunnerPool {
	return &RunnerPool{
		limits:  make(map[string]int),
		active:  make(map[string]int),
		waiters: make(map[string][]chan struct{}),
	}
}

// SetLimit 设置某个 Runner 的最大并发度。0 表示不限制。
func (p *RunnerPool) SetLimit(runner string, maxConc int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limits[runner] = maxConc
}

// Acquire 获取执行许可。如果超过并发限制则阻塞直到有空位或 ctx 取消。
func (p *RunnerPool) Acquire(ctx context.Context, runner string) error {
	p.mu.Lock()
	limit, hasLimit := p.limits[runner]
	if !hasLimit || limit <= 0 {
		p.active[runner]++
		p.mu.Unlock()
		return nil
	}

	if p.active[runner] < limit {
		p.active[runner]++
		p.mu.Unlock()
		return nil
	}

	ch := make(chan struct{}, 1)
	p.waiters[runner] = append(p.waiters[runner], ch)
	p.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		// ctx 取消时, 从等待队列中移除自己, 防止泄漏
		p.mu.Lock()
		for i, w := range p.waiters[runner] {
			if w == ch {
				p.waiters[runner] = append(p.waiters[runner][:i], p.waiters[runner][i+1:]...)
				break
			}
		}
		p.mu.Unlock()
		return ctx.Err()
	}
}

// Release 释放执行许可。
func (p *RunnerPool) Release(runner string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[runner]--
	if p.active[runner] < 0 {
		p.active[runner] = 0
	}

	if len(p.waiters[runner]) > 0 {
		ch := p.waiters[runner][0]
		p.waiters[runner] = p.waiters[runner][1:]
		p.active[runner]++
		ch <- struct{}{}
	}
}

// ActiveCount 返回某个 Runner 当前的活跃数。
func (p *RunnerPool) ActiveCount(runner string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active[runner]
}

// PooledRunner 将任何 TaskRunner 包装为受 RunnerPool 管控的版本。
type PooledRunner struct {
	inner TaskRunner
	pool  *RunnerPool
}

func NewPooledRunner(inner TaskRunner, pool *RunnerPool) *PooledRunner {
	return &PooledRunner{inner: inner, pool: pool}
}

func (r *PooledRunner) Name() string { return r.inner.Name() }

func (r *PooledRunner) Execute(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error) {
	if err := r.pool.Acquire(ctx, r.inner.Name()); err != nil {
		return nil, fmt.Errorf("获取 %s 并发许可失败: %w", r.inner.Name(), err)
	}
	defer r.pool.Release(r.inner.Name())
	return r.inner.Execute(ctx, task, bb)
}
