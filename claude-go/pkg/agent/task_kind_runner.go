package agent

// task_kind_runner.go —— 按 TaskSpec.Kind 分派的 TaskRunner (design/02 §3.5
// 「sync（IMA/WeRead）… 执行体改提交 TaskService」的前置件)。
//
// ---------------------------------------------------------------------------
// 为什么必须有这一层
// ---------------------------------------------------------------------------
//
// TaskService 只有**一个** Runner 字段 (`TaskServiceOptions.Runner`), 生产接线是
// `taskSvc.SetRunner(agent.NewTeamRunner(bot.TeamManager()))`
// (cmd/claude-go/main.go)。把 sync 的执行体也提交给同一个 TaskService 之后,
// 那个唯一的执行器会拿到一份 Kind="sync" 的档案, 然后**按团队跑它** ——
// Spec.Team 是空的, 于是它去创建/运行一个名字为空的团队。既不报错也不同步失败,
// 这正是本仓反复吃过的那类"静默错行为"。
//
// ---------------------------------------------------------------------------
// 三条必须这样定的语义
// ---------------------------------------------------------------------------
//
// ① **未注册的 kind 一律报错, 绝不回落到默认执行器**。
//    "找不到就用默认的" 看起来更宽容, 实际是把上面那个静默错行为变成默认路径:
//    任何拼错的 kind (或部署里忘了注册 sync 执行器) 都会安静地跑成团队任务。
//    fail-closed 的代价只是一条 failed 档案 + 明确的错误文本。
//
// ② **Interrupt 必须逐层转发**。`FileQueueTaskService.interrupt` 是这么找中断能力的:
//    `s.currentRunner().(TaskInterrupter)` —— 拿的是**当前那一个** runner。
//    TeamRunner 实现了 TaskInterrupter (团队执行的 ctx 由 RunTeam 内部
//    context.Background() 派生, 外层 cancel 到不了, 只能靠 StopTeam)。
//    一旦把它包进本类型而本类型不实现 TaskInterrupter, 类型断言就失败 ——
//    `Pause`/`Stop` 编译通过、返回 nil、日志干净, 而团队**根本停不下来**。
//    这是包装一个已有 runner 时最容易漏的一条, 有专门的变异反证守着。
//
// ③ **默认执行器允许为 nil**。装配顺序决定了这件事: sync 的执行体在 NewBot 之前
//    就绪, 团队执行器要等 NewBot 返回后才有 TeamManager。nil 时团队任务落
//    "未接线"错误而不是静默 pending —— 后者会让用户以为任务还在排队。
//
// 为什么不用更直觉的做法:
//   - **给 TaskService 加一张 kind→runner 表**: 那要改 TaskServiceOptions 与
//     SetRunner 的语义, 而 SetRunner 已有生产调用方与 29 个测试; 包一层
//     TaskRunner 则完全在既有接口内, 不带任何 kind 概念的部署零改动。
//   - **让 TeamRunner 自己认 kind**: 团队执行器不该知道 sync 存在 (L2 依赖 L4
//     的具体实现, 违反 §3 依赖方向), 且每加一种任务类型都要改它。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// KindRunner 按 TaskSpec.Kind 把任务分派给对应执行器。
//
// 并发安全: Register 通常只在进程装配期调用, 但读取 (Run/Interrupt) 与之无序,
// 故用 RWMutex 而不是裸 map —— 裸 map 在 -race 下是真报错, 不是理论问题。
type KindRunner struct {
	mu     sync.RWMutex
	def    TaskRunner            // Kind 为空时的执行器 (团队编排); 可为 nil
	byKind map[string]TaskRunner // 非空 Kind → 执行器
}

var (
	_ TaskRunner      = (*KindRunner)(nil)
	_ TaskInterrupter = (*KindRunner)(nil)
)

// NewKindRunner 构造分派器。def 为 Kind 空值 (团队任务) 的执行器, 允许 nil。
func NewKindRunner(def TaskRunner) *KindRunner {
	return &KindRunner{def: def, byKind: make(map[string]TaskRunner)}
}

// Register 注册某个 kind 的执行器。kind 为空或 r 为 nil 时是空操作 ——
// 空 kind 属于默认执行器, 从这里注册会让 Run 的分派语义有两个答案。
func (k *KindRunner) Register(kind string, r TaskRunner) {
	kind = strings.TrimSpace(kind)
	if kind == "" || r == nil {
		return
	}
	k.mu.Lock()
	k.byKind[kind] = r
	k.mu.Unlock()
}

// Kinds 返回已注册的 kind (升序), 供接线自检与错误文本使用。
func (k *KindRunner) Kinds() []string {
	k.mu.RLock()
	out := make([]string, 0, len(k.byKind))
	for kd := range k.byKind {
		out = append(out, kd)
	}
	k.mu.RUnlock()
	sort.Strings(out)
	return out
}

// resolve 找到该档案对应的执行器。第二个返回值是"找不到"时的原因。
func (k *KindRunner) resolve(rec TaskRecord) (TaskRunner, error) {
	kind := strings.TrimSpace(rec.Spec.Kind)
	k.mu.RLock()
	def, r := k.def, k.byKind[kind]
	k.mu.RUnlock()
	if kind == "" {
		if def == nil {
			return nil, fmt.Errorf("taskservice: 未接线默认 (团队) 执行器, 任务 %s 无法执行", rec.ID)
		}
		return def, nil
	}
	if r == nil {
		// 注意: 这里**不能**退回 def。见文件头 ①。
		return nil, fmt.Errorf("taskservice: 未注册 kind=%q 的执行器 (已注册: %v), 任务 %s 无法执行",
			kind, k.Kinds(), rec.ID)
	}
	return r, nil
}

// Run 见 TaskRunner.Run。
func (k *KindRunner) Run(ctx context.Context, rec TaskRecord) (string, error) {
	r, err := k.resolve(rec)
	if err != nil {
		return "", err
	}
	return r.Run(ctx, rec)
}

// Interrupt 见 TaskInterrupter。转发给该档案对应的执行器 (若它支持主动中断)。
//
// 目标执行器不实现 TaskInterrupter 时返回 nil 而不是错误: 那只表示"它靠 ctx
// 取消就够了", 不是故障。调用方 (interrupt) 无论如何都会 cancel ctx。
func (k *KindRunner) Interrupt(rec TaskRecord) error {
	r, err := k.resolve(rec)
	if err != nil {
		return err
	}
	if it, ok := r.(TaskInterrupter); ok {
		return it.Interrupt(rec)
	}
	return nil
}
