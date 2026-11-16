package harnesspro

// orchestrator.go —— 进化外环 (论文里的 Orchestrator: 全程确定性代码, 零 LLM)。
//
//	阶段0: DPP-lite 选题 → 出卷图 (testsynth→selfcheck, 冻结) → h0 验证集评测定地板
//	轮次:  组本轮 DAG → graph.Engine.Run → 读 select 报告 → 幸存者进入下一轮
//	收尾:  三档幸存者落盘 + 进化总开销 + 报告
//
// 外环刻意留在 Go 代码而不进 DAG: 轮次间的幸存者集是"上一张图的运行结果",
// 图引擎的静态 GraphSpec 表达不了这种跨运行数据依赖 (动态展开/Spawn 是给
// 单轮内 LLM 派生用的, 把编排器 itself 塞进图会让"跑几次、怎么选"失去
// 确定性 —— 正是论文说的"编排层是代码"的那层)。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/graph"
)

// Config 一次进化运行的配置。
type Config struct {
	Epochs      int          // 进化轮次 T (论文 TB2/SWE-Pro: 3; ProgramBench: 4)
	K           int          // 训练集 rollout 重跑次数 (论文 k=3)
	TrainN      int          // 训练集规模 (论文 10 题)
	Pool        []Task       // 任务池 (nil = DefaultPool)
	Tiers       []TierWeight // 三档权重 (nil = DefaultTiers)
	Workdir     string       // 运行目录 (git 工作区 + journal + 报告); 空 = 内存态 (测试)
	Resume      bool         // 从 journal 续跑
	MaxParallel int          // 图并发上限 (0 = 图默认 6)
}

// withDefaults 缺省补齐。
func (c Config) withDefaults() Config {
	if c.Epochs <= 0 {
		c.Epochs = 2 // 冒烟默认 2 轮 (论文 3-4; 一轮进化已能验证机制)
	}
	if c.K <= 0 {
		c.K = 3
	}
	if c.TrainN <= 0 {
		c.TrainN = 10
	}
	if len(c.Pool) == 0 {
		c.Pool = DefaultPool()
	}
	if len(c.Tiers) == 0 {
		c.Tiers = DefaultTiers()
	}
	return c
}

// EpochSummary 一轮的可读摘要 (报告用)。
type EpochSummary struct {
	Epoch     int                `json:"epoch"`
	Status    string             `json:"status"`
	Frontier  []string           `json:"frontier"`
	Filtered  []string           `json:"filtered"`
	Survivors []SelectedSurvivor `json:"survivors"`
	Evals     []CandidateEval    `json:"evals"`
	Tokens    int64              `json:"tokens"`
}

// EvolutionReport 一次进化运行的总报告。
type EvolutionReport struct {
	RunID       string             `json:"run_id"`
	Model       string             `json:"model"`
	StartedAt   int64              `json:"started_at"`
	DurationS   float64            `json:"duration_s"`
	TrainIDs    []string           `json:"train_ids"`
	ValidIDs    []string           `json:"valid_ids"`
	Floor       float64            `json:"floor"`
	Epochs      []EpochSummary     `json:"epochs"`
	Final       []SelectedSurvivor `json:"final"`
	Harnesses   []Harness          `json:"harnesses"`
	TotalTokens int64              `json:"total_tokens"`
	Workdir     string             `json:"workdir,omitempty"`
}

// Orchestrator 进化编排器。
type Orchestrator struct {
	cfg    Config
	llm    LLM
	store  *Store
	tokens atomic.Int64 // 进化总开销 (token), runner 累加
}

// NewOrchestrator 构造编排器。
func NewOrchestrator(cfg Config, llm LLM) (*Orchestrator, error) {
	cfg = cfg.withDefaults()
	if llm == nil {
		return nil, fmt.Errorf("harnesspro: LLM 未注入")
	}
	return &Orchestrator{cfg: cfg, llm: llm, store: NewStore(cfg.Pool)}, nil
}

// notify 透传进度 (调用方注入)。
func (o *Orchestrator) notify(f func(string), format string, args ...any) {
	if f != nil {
		f(fmt.Sprintf(format, args...))
	}
}

// Run 跑一次完整进化 (出卷 → 定地板 → T 轮 → 三档幸存者)。
func (o *Orchestrator) Run(ctx context.Context, notify func(string)) (*EvolutionReport, error) {
	started := time.Now()
	cfg := o.cfg
	if err := o.initWorkdir(); err != nil {
		return nil, err
	}
	// —— 阶段 0.1: 选题 ——
	train := SelectTrainTasks(cfg.Pool, cfg.TrainN)
	trainIDs := taskIDs(train)
	validIDs := complementIDs(cfg.Pool, trainIDs)
	o.notify(notify, "选题: 训练 %d 题 (%v), 验证 %d 题 (%v)", len(trainIDs), trainIDs, len(validIDs), validIDs)

	// —— 阶段 0.2: 注册 h0 ——
	h0 := Harness{ID: "h0", Instructions: H0Instructions(), Origin: "h0", Epoch: 0}
	o.store.RegisterHarness(h0)
	if err := o.commitHarnessFile(h0, "h0 baseline harness"); err != nil {
		o.notify(notify, "h0 落盘失败 (继续): %v", err)
	}

	// —— 阶段 0.3: 出卷图 (一次, 冻结; 覆盖全池, 训练集驱动 DPP、验证集供地板) ——
	if err := o.runTestSynthGraph(ctx, cfg.Pool, notify); err != nil {
		return nil, fmt.Errorf("出卷期失败: %w", err)
	}

	// —— 阶段 0.4: 定地板 (h0 在验证集, 固定测试, 每任务 1 次) ——
	floor, err := o.evalPassRate(ctx, h0.ID, validIDs)
	if err != nil {
		return nil, fmt.Errorf("地板评测失败: %w", err)
	}
	o.notify(notify, "地板 floor=%.2f (h0 验证集通过率)", floor)

	// —— 进化主循环 ——
	report := &EvolutionReport{
		RunID:     fmt.Sprintf("harnesspro-%d", started.Unix()),
		Model:     o.llm.Model(),
		StartedAt: started.Unix(), TrainIDs: trainIDs, ValidIDs: validIDs,
		Floor: floor, Workdir: cfg.Workdir,
	}
	survivors := []string{h0.ID}
	for epoch := 1; epoch <= cfg.Epochs; epoch++ {
		selectReport, toks, err := o.runEpoch(ctx, epoch, survivors, trainIDs, validIDs, floor, notify)
		if err != nil {
			return report, fmt.Errorf("epoch %d 失败: %w", epoch, err)
		}
		sel := make([]SelectedSurvivor, len(selectReport.Survivors))
		copy(sel, selectReport.Survivors)
		report.Epochs = append(report.Epochs, EpochSummary{
			Epoch: epoch, Status: "completed",
			Frontier: selectReport.Frontier, Filtered: selectReport.Filtered,
			Survivors: sel, Evals: selectReport.Evals,
			Tokens: toks,
		})
		if len(sel) == 0 {
			return report, fmt.Errorf("epoch %d 无晋级者, 进化中断", epoch)
		}
		survivors = survivors[:0]
		for _, s := range sel {
			survivors = append(survivors, s.CandidateID)
		}
		o.notify(notify, "epoch %d 完成, 晋级: %v", epoch, survivors)
	}
	report.Final = report.Epochs[len(report.Epochs)-1].Survivors
	report.Harnesses = o.store.Harnesses()
	report.TotalTokens = o.tokens.Load()
	report.DurationS = time.Since(started).Seconds()

	// —— 收尾落盘 ——
	if cfg.Workdir != "" {
		if err := os.WriteFile(filepath.Join(cfg.Workdir, "report.json"),
			[]byte(mustJSON(report)), 0o644); err != nil {
			return report, fmt.Errorf("报告落盘失败: %w", err)
		}
		o.commitAll("evolution report")
	}
	return report, nil
}

// runTestSynthGraph 出卷期: 一张两节点 DAG (并行出卷 → 冻结自检), 覆盖给定任务集
// (调用方传全池; 训练集是 DPP 选出的子集, 验证集用于地板 —— 全池都要卷)。
func (o *Orchestrator) runTestSynthGraph(ctx context.Context, tasks []Task, notify func(string)) error {
	spec := BuildTestSynthGraph(tasks)
	r := &Runner{
		LLM: o.llm, Store: o.store, Workdir: o.cfg.Workdir,
		TrainIDs: taskIDs(tasks), Notify: notify, TokenSink: &o.tokens,
	}
	eng := o.engine(r, "testsynth")
	res, err := eng.Run(ctx, spec, graph.RunOpts{
		Objective: "为任务池合成冻结测试卷 (训练+验证全覆盖)",
		Params:    TestSynthParams(tasks),
		Resume:    o.cfg.Resume,
	})
	if err != nil {
		return err
	}
	if res.Status != graph.RunStatusCompleted {
		return fmt.Errorf("出卷图未完整收尾: status=%s", res.Status)
	}
	return nil
}

// runEpoch 一轮进化: 组 DAG → 跑 → 读选点报告。
func (o *Orchestrator) runEpoch(ctx context.Context, epoch int, survivors, trainIDs, validIDs []string, floor float64, notify func(string)) (*SelectionReport, int64, error) {
	spec := BuildEpochGraph(epoch)
	r := &Runner{
		LLM: o.llm, Store: o.store, Workdir: o.cfg.Workdir,
		Epoch: epoch, K: o.cfg.K, Floor: floor, Tiers: o.cfg.Tiers,
		TrainIDs: trainIDs, ValidIDs: validIDs, Notify: notify, TokenSink: &o.tokens,
	}
	eng := o.engine(r, fmt.Sprintf("epoch-%d", epoch))
	before := o.tokens.Load()
	res, err := eng.Run(ctx, spec, graph.RunOpts{
		RunID:     fmt.Sprintf("%s-epoch-%d", "harnesspro", epoch),
		Objective: fmt.Sprintf("harness 进化第 %d 轮", epoch),
		Params:    EpochParams(survivors, mustTasks(o.store, trainIDs)),
		Resume:    o.cfg.Resume,
	})
	if err != nil {
		return nil, 0, err
	}
	selNode, ok := res.Nodes["select"]
	if !ok || selNode.Status != graph.NodeStatusCompleted {
		return nil, 0, fmt.Errorf("选点节点未 completed (run=%s): 见 journal", res.RunID)
	}
	var report SelectionReport
	if err := json.Unmarshal([]byte(selNode.Output), &report); err != nil {
		return nil, 0, fmt.Errorf("选点报告解析失败: %w", err)
	}
	return &report, o.tokens.Load() - before, nil
}

// engine 构造图引擎 (FileJournal 落 workdir/journal/<name>)。
func (o *Orchestrator) engine(r *Runner, name string) *graph.Engine {
	eng := &graph.Engine{Runner: r, MaxParallel: o.cfg.MaxParallel}
	if o.cfg.Workdir != "" {
		jdir := filepath.Join(o.cfg.Workdir, "journal", name)
		if fj, err := graph.NewFileJournal(jdir); err == nil {
			eng.Journal = fj
		}
	}
	return eng
}

// evalPassRate 在验证集上评测一个候选的通过率 (地板与基线用)。
// 判定语义: 任务无冻结卷 = 该任务出卷失败, 是**评测前提缺失**而非
// "infra_error 不计入" —— 静默跳过会让地板建立在残缺验证集上.
func (o *Orchestrator) evalPassRate(ctx context.Context, harnessID string, validIDs []string) (float64, error) {
	h, ok := o.store.Harness(harnessID)
	if !ok {
		return 0, fmt.Errorf("未知候选 %s", harnessID)
	}
	var missing []string
	for _, tid := range validIDs {
		if len(o.store.Tests(tid)) == 0 {
			missing = append(missing, tid)
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("验证集缺冻结测试卷: %v", missing)
	}
	var pass, n int
	for _, tid := range validIDs {
		t, ok := o.store.Task(tid)
		if !ok {
			return 0, fmt.Errorf("未知任务 %s", tid)
		}
		tests := o.store.Tests(tid)
		if len(tests) == 0 {
			return 0, fmt.Errorf("任务 %s 无冻结测试卷", tid)
		}
		out, usage, err := o.llm.Complete(ctx, h.Instructions, t.Prompt)
		o.tokens.Add(usage.Total())
		if err != nil {
			continue // infra_error 不计入
		}
		passed, _ := RunTests(out, tests)
		if passed == len(tests) {
			pass++
		}
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("验证集全部 infra_error, 无法定地板")
	}
	return float64(pass) / float64(n), nil
}

// initWorkdir 初始化运行目录与 git。
func (o *Orchestrator) initWorkdir() error {
	if o.cfg.Workdir == "" {
		return nil
	}
	for _, d := range []string{"harnesses", "journal"} {
		if err := os.MkdirAll(filepath.Join(o.cfg.Workdir, d), 0o755); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(o.cfg.Workdir, ".git")); os.IsNotExist(err) {
		if out, err := exec.Command("git", "-C", o.cfg.Workdir, "init").CombinedOutput(); err != nil {
			return fmt.Errorf("git init: %v: %s", err, out)
		}
		_ = os.WriteFile(filepath.Join(o.cfg.Workdir, ".gitignore"), []byte("journal/\n"), 0o644)
		// 仓库级身份: 容器/pod 里通常没有全局 git 配置, 不给就 commit 全挂
		// (harnesspro-evo 身份落进每次提交的作者, 与 commitHarness 的 --author 一致).
		for _, kv := range [][2]string{
			{"user.name", "harnesspro-evo"},
			{"user.email", "evo@claude-go.local"},
		} {
			_ = exec.Command("git", "-C", o.cfg.Workdir, "config", kv[0], kv[1]).Run()
		}
	}
	return nil
}

// commitHarnessFile 编排器侧的 harness 落盘 (h0 用; 候选由 editor 分片自己提交)。
func (o *Orchestrator) commitHarnessFile(h Harness, msg string) error {
	if o.cfg.Workdir == "" {
		return nil
	}
	r := &Runner{LLM: o.llm, Store: o.store, Workdir: o.cfg.Workdir}
	return r.commitHarness(h, msg)
}

// commitAll 全量提交 (报告/h0 等编排器产物)。
func (o *Orchestrator) commitAll(msg string) {
	if o.cfg.Workdir == "" {
		return
	}
	_ = exec.Command("git", "-C", o.cfg.Workdir, "add", ".").Run()
	_ = exec.Command("git", "-C", o.cfg.Workdir, "commit", "-m", msg,
		"--author", "harnesspro-evo <evo@claude-go.local>").Run()
}

// taskIDs 任务 id 列表。
func taskIDs(ts []Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

// complementIDs 池子里不在 trainIDs 的题 (验证集)。
func complementIDs(pool []Task, trainIDs []string) []string {
	inTrain := map[string]bool{}
	for _, id := range trainIDs {
		inTrain[id] = true
	}
	out := []string{}
	for _, t := range pool {
		if !inTrain[t.ID] {
			out = append(out, t.ID)
		}
	}
	return out
}

// mustTasks 按 id 取任务 (Store 权威)。
func mustTasks(s *Store, ids []string) []Task {
	out := make([]Task, 0, len(ids))
	for _, id := range ids {
		if t, ok := s.Task(id); ok {
			out = append(out, t)
		}
	}
	return out
}
