// Package synctask 把 IMA/微信读书同步的**执行体**从"进程内 goroutine"改成
// "提交给 TaskService" (design/02 §3.5 通道表 sync 那一行:
// 「触发端点不变，执行体改提交 TaskService」)。
//
// ---------------------------------------------------------------------------
// 改造前的两条独立执行路径 (都是进程内)
// ---------------------------------------------------------------------------
//
//	① HTTP: pkg/wiki/api.go handleSyncTrigger → Scheduler.RunNow(source, fn)
//	   → `go func(){ fn(ctx) }()`, 台账只在 Scheduler.running 这张内存 map 里,
//	   进程重启即丢 (于是 /sync/status/<id> 在重启后一律 404)。
//	② cron tick: pkg/feishu/bot.go registerSyncJobs → Scheduler.check
//	   → 同样 `go fn(ctx)`, 完全不经过 ①的台账。
//
// 两条路都直接调 sync.RunSync, 而 RunSync 在 LoadIndex 与 SaveIndex 之间没有任何
// 互斥 —— **同一 source 并发跑两次会互相覆盖 index.json**。现状下"连点两下
// /sync/ima" 或 "手动触发正好撞上 cron tick" 就能复现。
//
// ---------------------------------------------------------------------------
// 改造后
// ---------------------------------------------------------------------------
//
// 两条路都变成 `SubmitSync(source)` → TaskService 建档 → KindRunner 分派到本包的
// Run → RunSync。收益不只是"分层对了":
//
//   - **幂等收口**: TaskService 的活跃索引按 (team, workflow, objective, kind) 归并,
//     本包给同一 source 生成**稳定**的 objective, 于是同一 source 的第二次提交拿到
//     第一次的档案, 双跑消失。这是顺带修掉的既有缺陷。
//   - **台账落盘**: 档案在 statestore 里, 进程重启后 /sync/status/<jobId> 仍可查。
//   - **可停**: TaskService 的 Stop/Pause 现在对同步任务也有效 (ctx 取消, RunSync
//     的 adapter HTTP 请求随之中断)。
//
// ⚠️ **不制造双跑**: 接线后 wiki 的 handleSyncTrigger 与 feishu 的 registerSyncJobs
// **必须**不再自己调 RunSync (前者跳过 Scheduler.RunNow, 后者整个 job 改成提交)。
// 本仓吃过完全同型的事故: 动作队列注册消费方后 team 动作跑了两遍, 最后靠
// actionSinkOwns 才收口。两处的 fail-safe 都是"submitter 为 nil 才走老路"。
//
// ---------------------------------------------------------------------------
// 为什么不用更直觉的做法
// ---------------------------------------------------------------------------
//
//   - **让 Scheduler.RunNow 内部去提交 TaskService**: Scheduler 是 pkg/sync 的
//     类型, 那会让 L4 的 sync 反向依赖 L2 的 agent (违反 §3 依赖方向), 且
//     RunNow 的 fn 签名 (`func(ctx) error`) 无处安放"提交后立刻返回"的语义。
//   - **给 TaskSpec 塞一个 sync 专用字段**: TaskSpec 是全部任务共用的规格,
//     每来一种任务类型就加一个字段, 最后没人能回答"这份档案该由谁执行"。
//     故走 Kind + Params 这条通用路。
//   - **复用 TaskSpec.Team 装 source**: 那会让 `/api/teams` 与 dashboard 的团队
//     列表里凭空出现名为 "sync-ima" 的团队 —— 下游能观察到的行为变化。
//     故 Team 留空, 身份放 objective/params。
package synctask

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
	claudesync "github.com/anthropic/claude-go/pkg/sync"
)

// Kind 是同步任务在 TaskSpec.Kind 上的取值, 也是 KindRunner 的注册键。
const Kind = "sync"

// ParamSource 是 TaskSpec.Params 里承载 source (ima/weread) 的键。
const ParamSource = "source"

// objectivePrefix 让 objective 对同一 source 稳定 —— 活跃索引与派生幂等键都靠它,
// 带时间戳就永不归并, "同 source 不并发跑两遍" 立刻失效。
const objectivePrefix = "sync:"

// Bridge 实现 claudesync.TaskSubmitter (提交侧) 与 agent.TaskRunner (执行侧)。
//
// 一个类型担两头是有意的: 提交时写进 Params 的 source 与执行时读出来的 source
// 必须是同一套约定, 分成两个类型就得把约定写成两份。
type Bridge struct {
	tasks agent.TaskService
	cfg   claudesync.Config
	// runSync 便于测试替换真实网络调用; 生产为 nil → 用 claudesync.RunSync。
	runSync func(cfg claudesync.Config, source string, ad claudesync.Adapter) (*claudesync.Result, error)
	// newAdapter 同上; 生产为 nil → 按 source 造 IMA/WeRead 适配器。
	newAdapter func(cfg claudesync.Config, source string) (claudesync.Adapter, error)
}

var (
	_ claudesync.TaskSubmitter = (*Bridge)(nil)
	_ agent.TaskRunner         = (*Bridge)(nil)
)

// Options 构造参数。
type Options struct {
	Tasks  agent.TaskService // 必填
	Config claudesync.Config // 同步配置 (知识库路径 + 各源凭据)

	// RunSync / NewAdapter 仅测试注入; 生产留零值。
	RunSync    func(cfg claudesync.Config, source string, ad claudesync.Adapter) (*claudesync.Result, error)
	NewAdapter func(cfg claudesync.Config, source string) (claudesync.Adapter, error)
}

// New 构造桥。tasks 为 nil 时返回 nil —— 调用方据此判断"本进程没装配 TaskService",
// 然后据实降级走老路 (而不是拿着一个会 panic 的桥假装接线成功)。
func New(opts Options) *Bridge {
	if opts.Tasks == nil {
		return nil
	}
	b := &Bridge{tasks: opts.Tasks, cfg: opts.Config, runSync: opts.RunSync, newAdapter: opts.NewAdapter}
	if b.runSync == nil {
		b.runSync = claudesync.RunSync
	}
	if b.newAdapter == nil {
		b.newAdapter = defaultAdapter
	}
	return b
}

// defaultAdapter 按 source 造生产适配器。
func defaultAdapter(cfg claudesync.Config, source string) (claudesync.Adapter, error) {
	switch source {
	case "ima":
		return claudesync.NewIMAAdapter(cfg.IMA.ClientID, cfg.IMA.APIKey), nil
	case "weread":
		return claudesync.NewWeReadAdapter(cfg.WeRead.APIKey), nil
	default:
		return nil, fmt.Errorf("未知同步源 %q (应为 ima 或 weread)", source)
	}
}

// SubmitSync 见 claudesync.TaskSubmitter。
func (b *Bridge) SubmitSync(source string) (*claudesync.Job, error) {
	source = strings.TrimSpace(source)
	// 提交前就拒未知 source: 否则会先建一份注定失败的档案, 再由执行器报错 ——
	// 而 HTTP 触发端点的契约是"未知 source 回 400", 那时档案已经落盘了。
	if _, err := b.newAdapter(b.cfg, source); err != nil {
		return nil, err
	}
	rec, err := b.tasks.Submit(agent.TaskSpec{
		Kind:      Kind,
		Objective: objectivePrefix + source,
		Params:    map[string]any{ParamSource: source},
	}, agent.SubmitOpts{Source: "sync"})
	if err != nil {
		return nil, err
	}
	return recordToJob(rec, source), nil
}

// SyncJob 见 claudesync.TaskSubmitter。
func (b *Bridge) SyncJob(jobID string) (*claudesync.Job, bool) {
	rec, ok := b.tasks.Get(jobID)
	if !ok {
		return nil, false
	}
	// 只认本包提交的档案: 否则 /sync/status/<团队任务 ID> 会把一个团队运行
	// 伪装成同步任务返回给下游 (两者的 Job 字段含义完全不同)。
	if rec.Spec.Kind != Kind {
		return nil, false
	}
	return recordToJob(rec, sourceOf(rec)), true
}

// WaitSync 见 claudesync.TaskSubmitter。
func (b *Bridge) WaitSync(ctx context.Context, jobID string) (*claudesync.Job, error) {
	if _, err := b.tasks.Wait(ctx, jobID); err != nil {
		return nil, err
	}
	job, ok := b.SyncJob(jobID)
	if !ok {
		return nil, fmt.Errorf("synctask: 任务 %q 档案已消失", jobID)
	}
	return job, nil
}

// Run 见 agent.TaskRunner —— 这就是被搬过来的"执行体"。
func (b *Bridge) Run(ctx context.Context, rec agent.TaskRecord) (string, error) {
	source := sourceOf(rec)
	if source == "" {
		return "", fmt.Errorf("synctask: 任务 %s 缺少 params.%s", rec.ID, ParamSource)
	}
	ad, err := b.newAdapter(b.cfg, source)
	if err != nil {
		return "", err
	}
	// ctx 取消要能真的止住: RunSync 自己不收 ctx (既有签名, 8+ 处调用方),
	// 故在开跑前先看一眼 —— Stop 一个还没开始拉取的任务不该白跑一整轮。
	// 拉取过程中的取消由 adapter 的 HTTP 客户端负责, 这里不假装能中断它。
	if err := ctx.Err(); err != nil {
		return "", err
	}
	res, err := b.runSync(b.cfg, ad.Source(), ad)
	if err != nil {
		return "", err
	}
	return formatResult(res), nil
}

// formatResult 与 botCronExecutor.TriggerSync 的摘要口径一致 (那条文案已进飞书
// 通知与 cron 日志, 改了就是用户能看见的行为变化)。
func formatResult(res *claudesync.Result) string {
	if res == nil {
		return "ok"
	}
	return fmt.Sprintf("新增 %d · 更新 %d · 不变 %d · 删除 %d · 错误 %d",
		res.Created, res.Updated, res.Unchanged, res.Deleted, res.Errors)
}

// sourceOf 从档案里取回 source。优先 Params (提交时写的), 回退 objective 前缀
// —— 档案经 JSON 往返后 Params 的值是 any, 但 statestore 存的就是 JSON,
// 两条路都要能读出来。
func sourceOf(rec agent.TaskRecord) string {
	if rec.Spec.Params != nil {
		if v, ok := rec.Spec.Params[ParamSource].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return strings.TrimPrefix(rec.Spec.Objective, objectivePrefix)
}

// recordToJob 把任务档案映射成冻结的 sync.Job 形态。
//
// 状态映射是契约的一部分: sync.JobStatus 只有 pending/running/completed/failed
// 四个值, 而 TaskState 多出 paused/stopped。paused → pending (它还会继续),
// stopped → failed (它不会再产出结果了)。把 stopped 映成 completed 是最坏的选择
// —— 下游会以为同步成功了。
func recordToJob(rec agent.TaskRecord, source string) *claudesync.Job {
	job := &claudesync.Job{
		ID:        rec.ID,
		Source:    source,
		Status:    mapState(rec.State),
		Message:   rec.Output,
		StartedAt: rec.SubmittedAt.UTC(),
	}
	if !rec.StartedAt.IsZero() {
		job.StartedAt = rec.StartedAt.UTC()
	}
	if rec.Error != "" {
		job.Message = rec.Error
	}
	if job.Status == claudesync.JobCompleted && job.Message == "" {
		// 老实现在成功时写死 "ok", 下游有按这个值判成功的可能。
		job.Message = "ok"
	}
	if !rec.FinishedAt.IsZero() {
		t := rec.FinishedAt.UTC()
		job.FinishedAt = &t
	}
	return job
}

func mapState(s agent.TaskState) claudesync.JobStatus {
	switch s {
	case agent.TaskStateRunning:
		return claudesync.JobRunning
	case agent.TaskStateCompleted:
		return claudesync.JobCompleted
	case agent.TaskStateFailed, agent.TaskStateStopped:
		return claudesync.JobFailed
	default: // pending / paused
		return claudesync.JobPending
	}
}
