// Cron — 定时任务调度器, 通过飞书控制和管理。
//
// 核心能力:
//
//  1. 标准 cron 表达式 (分 时 日 月 周)
//
//  2. 支持多种任务类型 (workflow/query/command/sync/wiki-*)
//
//  3. 飞书 /cron 命令 + 自然语言创建
//
//  4. 持久化: 重启后自动恢复
//
//  5. 任务执行结果通过飞书推送
//
//     ┌────────────────────────────────────────────┐
//     │ CronScheduler                              │
//     │  AddJob()      → 添加定时任务              │
//     │  RemoveJob()   → 删除任务                  │
//     │  PauseJob()    → 暂停/恢复                 │
//     │  ListJobs()    → 列出所有任务              │
//     │  tick()        → 每分钟检查匹配的任务      │
//     │  executeJob()  → 执行并推送结果到飞书      │
//     └────────────────────────────────────────────┘
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CronJob 定时任务定义。
type CronJob struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`           // cron 表达式: "分 时 日 月 周"
	JobType  string `json:"jobType"`            // workflow, query, command, sync, wiki-*
	Payload  string `json:"payload"`            // 执行内容 (任务目标/消息/命令/数据源)
	ChatID   string `json:"chatId"`             // 结果推送的飞书 chat_id
	Workflow string `json:"workflow,omitempty"` // jobType=workflow 时的工作流类型
	// Model 任务级执行模型别名 (provider:model, 如 ollama:lfm2.5:2.6b-q4_k_m); 空 = 默认模型。
	// 消费面: 当前仅 jobType=query 真实消费 (一次性覆盖会话模型, 不污染既有会话)。
	// workflow 的模型走 ai.plans.<type>.modelAlias 配置 (按工作流类型, 本身可配);
	// sync 是**外部数据拉取**, 不走 LLM (weread 纯 HTTP; ima 是 node 子进程调
	// IMA OpenAPI, 同样不碰模型) —— 给它配模型是无效旋钮, 刻意不接线
	// (本仓纪律: 不建"建成未通电"的假开关)。
	Model      string    `json:"model,omitempty"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"createdAt"`
	LastRunAt  time.Time `json:"lastRunAt,omitempty"`
	LastResult string    `json:"lastResult,omitempty"` // 上次执行结果摘要
	RunCount   int       `json:"runCount"`
	FailCount  int       `json:"failCount"`
}

// CronExecutor 执行器接口, 解耦 Bot 依赖。
type CronExecutor interface {
	RunWorkflow(ctx context.Context, name, workflow, objective, chatID string) error
	SendQuery(ctx context.Context, chatID, message string) (string, error)
	RunCommand(ctx context.Context, chatID, command string) error
	Notify(chatID, message string)
	// Wiki 操作
	WikiOrganize(ctx context.Context, mode string) (string, error)
	WikiHealthCheck(ctx context.Context) (string, error)
	WikiLint(ctx context.Context) (string, error)
	// TriggerSync 触发外部数据源(ima/weread)同步, 返回结果摘要。
	TriggerSync(ctx context.Context, source string) (string, error)
}

// CronModelExecutor 可选扩展接口: 执行器支持按任务级模型跑 query 时实现。
// executeJob 发现 job.Model 非空时做类型断言; 未实现则回退 SendQuery 并记日志
// (模型配置被忽略必须可观测, 不能静默)。
type CronModelExecutor interface {
	SendQueryWithModel(ctx context.Context, chatID, message, model string) (string, error)
}

// CronScheduler 定时任务调度器。
// CronLease 定时任务分布式租约 (design/02 §3.4.4 cron 选主)。
// 多副本控制面下, tick 同一分钟同一 job 时先抢租约, 抢到才执行 → 防重复触发。
// 单副本部署 lease 为 nil, 退化为原地执行 (零开销)。
type CronLease interface {
	// TryAcquire 原子抢占 key 的租约; 返回 true 表示本副本抢到、应执行。
	// key 形如 "cron/<jobID>/<yyyymmddHHMM>"（分钟粒度幂等）。
	TryAcquire(key string) bool
}

type CronScheduler struct {
	jobs     map[string]*CronJob
	mu       sync.RWMutex
	dataDir  string
	executor CronExecutor
	stopCh   chan struct{}
	stopOnce sync.Once // Stop 幂等: 二次 close(stopCh) 会 panic
	nextID   int
	lease    CronLease // 分布式租约 (可为 nil, 单副本)
}

// SetLease 注入分布式租约 (K8s 多副本控制面用)。
func (cs *CronScheduler) SetLease(l CronLease) { cs.lease = l }

// NewCronScheduler 创建调度器。
func NewCronScheduler(dataDir string, executor CronExecutor) *CronScheduler {
	cs := &CronScheduler{
		jobs:     make(map[string]*CronJob),
		dataDir:  dataDir,
		executor: executor,
		stopCh:   make(chan struct{}),
	}
	cs.load()
	return cs
}

// Start 启动调度器 (后台 goroutine, 每分钟 tick)。
func (cs *CronScheduler) Start() {
	go cs.tickLoop()
	log.Printf("[Cron] 调度器已启动, 当前 %d 个任务", len(cs.jobs))
}

// Stop 停止调度器。**幂等** —— 可以安全地重复调用。
//
// 此前是裸 close(stopCh): 二次调用直接 panic。这不是理论风险 —— 独立 worker 进程
// 复用 NewBot 装配真执行体, 需要先 Stop() 掉 cron (worker 跑 cron 会与控制面重复
// 触发同一 job), 之后又不敢调 bot.Shutdown()(它会再 Stop 一次), 于是 MCP 子进程
// 只能靠进程退出回收。让 Stop 幂等比让调用方记住"只能停一次"可靠。
func (cs *CronScheduler) Stop() {
	cs.stopOnce.Do(func() { close(cs.stopCh) })
}

// AddJob 添加定时任务。
func (cs *CronScheduler) AddJob(job *CronJob) error {
	if err := validateCronExpr(job.Schedule); err != nil {
		return fmt.Errorf("无效的 cron 表达式 %q: %w", job.Schedule, err)
	}
	if job.JobType == "" {
		return fmt.Errorf("jobType 不能为空")
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	if job.ID == "" {
		cs.nextID++
		job.ID = fmt.Sprintf("cron-%d", cs.nextID)
	}
	if job.Name == "" {
		job.Name = job.ID
	}
	job.CreatedAt = time.Now()
	job.Enabled = true

	cs.jobs[job.ID] = job
	cs.persist()
	log.Printf("[Cron] 添加任务: %s (%s) [%s]", job.Name, job.Schedule, job.JobType)
	return nil
}

// RemoveJob 删除任务。
func (cs *CronScheduler) RemoveJob(id string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if _, ok := cs.jobs[id]; !ok {
		return fmt.Errorf("任务 %q 不存在", id)
	}
	delete(cs.jobs, id)
	cs.persist()
	return nil
}

// PauseJob 暂停任务。
func (cs *CronScheduler) PauseJob(id string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	job, ok := cs.jobs[id]
	if !ok {
		return fmt.Errorf("任务 %q 不存在", id)
	}
	job.Enabled = false
	cs.persist()
	return nil
}

// ResumeJob 恢复任务。
func (cs *CronScheduler) ResumeJob(id string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	job, ok := cs.jobs[id]
	if !ok {
		return fmt.Errorf("任务 %q 不存在", id)
	}
	job.Enabled = true
	cs.persist()
	return nil
}

// UpdateJob 更新任务的可变字段 (schedule/name/payload/jobType/workflow/chatId),
// 保留 ID、CreatedAt 与运行统计。patch 中非空字段才覆盖; enabled 由 Pause/ResumeJob 管理。
func (cs *CronScheduler) UpdateJob(id string, patch *CronJob) error {
	if patch.Schedule != "" {
		if err := validateCronExpr(patch.Schedule); err != nil {
			return fmt.Errorf("无效的 cron 表达式 %q: %w", patch.Schedule, err)
		}
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	job, ok := cs.jobs[id]
	if !ok {
		return fmt.Errorf("任务 %q 不存在", id)
	}
	if patch.Name != "" {
		job.Name = patch.Name
	}
	if patch.Schedule != "" {
		job.Schedule = patch.Schedule
	}
	if patch.JobType != "" {
		job.JobType = patch.JobType
	}
	if patch.Payload != "" {
		job.Payload = patch.Payload
	}
	if patch.Workflow != "" {
		job.Workflow = patch.Workflow
	}
	if patch.ChatID != "" {
		job.ChatID = patch.ChatID
	}
	// Model: 空 = 不动; "default"/"-" = 清除回默认; 其他 = 设置为该别名。
	// (patch 语义与其他字段一致 —— 空值不覆盖, 故清除需要哨兵值。)
	switch patch.Model {
	case "":
	case "default", "-":
		job.Model = ""
	default:
		job.Model = patch.Model
	}
	cs.persist()
	log.Printf("[Cron] 更新任务: %s (%s) [%s]", job.Name, job.Schedule, job.JobType)
	return nil
}

// TriggerJob 立即执行一次任务 (不动调度计划与启停状态)。
// 供 dashboard /api/cron/{id}/trigger 手动触发 —— 此前 "cron.trigger" 动作
// 全仓没有消费方, 提示"等待主进程消费"是假承诺; 本方法就是那个诚实的消费方。
func (cs *CronScheduler) TriggerJob(id string) error {
	cs.mu.RLock()
	job, ok := cs.jobs[id]
	cs.mu.RUnlock()
	if !ok {
		return fmt.Errorf("任务 %q 不存在", id)
	}
	go cs.executeJob(job)
	log.Printf("[Cron] 手动触发任务: %s (%s) [%s]", job.Name, job.ID, job.JobType)
	return nil
}

// ListJobs 列出所有任务。
func (cs *CronScheduler) ListJobs() []*CronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	result := make([]*CronJob, 0, len(cs.jobs))
	for _, j := range cs.jobs {
		result = append(result, j)
	}
	return result
}

// GetJob 获取单个任务。
func (cs *CronScheduler) GetJob(id string) *CronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.jobs[id]
}

// Stats 统计信息。
func (cs *CronScheduler) Stats() (total, enabled, totalRuns int) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	for _, j := range cs.jobs {
		total++
		if j.Enabled {
			enabled++
		}
		totalRuns += j.RunCount
	}
	return
}

// FormatJobList 格式化任务列表用于飞书展示。
func (cs *CronScheduler) FormatJobList() string {
	jobs := cs.ListJobs()
	if len(jobs) == 0 {
		return "无定时任务。发送 `/cron add` 添加。"
	}

	var sb strings.Builder
	sb.WriteString("**定时任务列表:**\n\n")
	for _, j := range jobs {
		icon := "✅"
		if !j.Enabled {
			icon = "⏸️"
		}
		sb.WriteString(fmt.Sprintf("%s **%s** (`%s`)\n", icon, j.Name, j.ID))
		sb.WriteString(fmt.Sprintf("  调度: `%s` | 类型: %s\n", j.Schedule, j.JobType))
		if j.Model != "" {
			sb.WriteString(fmt.Sprintf("  模型: `%s`\n", j.Model))
		}
		sb.WriteString(fmt.Sprintf("  内容: %s\n", truncateResult(j.Payload, 80)))
		sb.WriteString(fmt.Sprintf("  运行: %d 次 | 失败: %d 次", j.RunCount, j.FailCount))
		if !j.LastRunAt.IsZero() {
			sb.WriteString(fmt.Sprintf(" | 上次: %s", j.LastRunAt.Format("01-02 15:04")))
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// --- 调度核心 ---

// tickLoop 每分钟触发一次 tick, 直到 stopCh 关闭; 退出时停掉 ticker。
func (cs *CronScheduler) tickLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-cs.stopCh:
			return
		case now := <-ticker.C:
			cs.tick(now)
		}
	}
}

// tick 是每分钟一次的调度步: 先快照出本轮命中的任务, 再逐个抢租约后并发执行。
//
// 算法解读：
//
//	原理：cron 按分钟对齐 —— ticker 每分钟触发, 用当前时刻去匹配每个任务的 5 字段
//	  表达式, 命中的任务各自在独立 goroutine 里执行, 不阻塞下一分钟。
//	关键不变量：持锁期间**只做匹配与收集, 不执行任务**。executeJob 会回写 job 的
//	  RunCount/LastRunAt 并调 persist (需要写锁), 若在 RLock 里直接执行就是自锁死。
//	代价与边界：每轮 O(任务数); 派发出去的 goroutine 没有上限 —— 同一分钟命中很多
//	  任务时会同时起同样多的执行体 (各自 15 分钟超时)。
//	设计权衡：防重复触发靠分布式租约而不是本地去重。单副本下 lease 为 nil, 退化为
//	  原地执行 (零开销); 多副本下 key 含分钟粒度, 抢不到就静默跳过 —— 幂等交给 key,
//	  不必让每个副本各自记"这一分钟我跑过了"。
func (cs *CronScheduler) tick(now time.Time) {
	cs.mu.RLock()
	var toRun []*CronJob
	for _, j := range cs.jobs {
		if j.Enabled && matchesCron(j.Schedule, now) {
			toRun = append(toRun, j)
		}
	}
	cs.mu.RUnlock()

	minute := now.Format("200601021504")
	for _, j := range toRun {
		// 分布式租约选主 (design/02 §3.4.4): 多副本下同一 job/分钟只有抢到租约的执行。
		if cs.lease != nil && !cs.lease.TryAcquire("cron/"+j.ID+"/"+minute) {
			continue // 别的副本抢到了, 本副本跳过
		}
		go cs.executeJob(j)
	}
}

// executeJob 按 jobType 分派任务给 CronExecutor, 并在结尾统一回写运行统计。
//
// 算法解读：
//
//	原理：一个 switch 把 job.JobType 映射到执行器的对应方法 (workflow/query/command/
//	  sync/wiki-*), 结果只收敛成一个 execErr, 收尾处再统一记账与通知。
//	关键不变量：**先校验具名会话再分派**。query/command/workflow 必须带 chatID, 空值
//	  直接判失败并 return —— 否则会静默跑进一个匿名会话: 任务报"成功", 而那个员工
//	  根本没被唤醒 (详见下方长注释)。
//	代价与边界：单任务整体超时 15 分钟, 由 ctx 传导给执行体; 执行体若不理会 ctx,
//	  这里不会强行中断。未知 JobType 记一条明确失败, 不静默成功。
//	设计权衡：失败既写 LastResult 又发飞书通知 —— 双写是因为 dashboard 与飞书是两个
//	  不同的观察面, 只写一个就会出现"页面上看得见但没人知道"或反之。
func (cs *CronScheduler) executeJob(job *CronJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	start := time.Now()
	var execErr error

	// ★ 具名会话闸门 (fail-closed): query / command / workflow 这三类任务都是
	// "去和某个会话说话" —— chatID 就是那个会话的身份。
	//
	// 空 chatID 时**拒绝执行**, 而不是让它跑进一个匿名会话:
	//
	//   - 会话级轨迹 (pkg/feishu/session_trace.go) 按 chatID 命名桶。空 chatID 的
	//     运行会写进 `trace-run--<n>` 这种桶, 轨迹页上归不了任何团队 —— 一次真实
	//     运行, 在页面上与"没跑过"长得一样。
	//   - 用具名会话当 chatID 的作业 (调度唤醒某个员工) 若 id 打错成空串, 结果是
	//     **静默**起了一个新会话: 那个员工没被唤醒, 但任务报"成功", 且盘上多出一个
	//     谁也不认识的会话。
	//
	// 所以这里发一条明确失败 (进 FailCount 与 LastResult), 让它在 dashboard 上看得见。
	// 与 sessionapi 的 409 同一条纪律: "现在不行"必须是可重试的明确信号, 不是静默降级。
	if needsSessionTarget(job.JobType) && strings.TrimSpace(job.ChatID) == "" {
		execErr = fmt.Errorf("任务类型 %s 必须指定会话 (chatID): 空值会让这次运行跑进一个匿名会话, "+
			"轨迹归不了团队且报成功", job.JobType)
		cs.executor.Notify(job.ChatID, fmt.Sprintf("❌ 定时任务 **%s** 未执行: %v", job.Name, execErr))
		cs.mu.Lock()
		job.LastRunAt = time.Now()
		job.RunCount++
		job.FailCount++
		job.LastResult = "失败: " + execErr.Error()
		cs.persist()
		cs.mu.Unlock()
		return
	}

	cs.executor.Notify(job.ChatID, fmt.Sprintf("⏰ 定时任务 **%s** 开始执行...\n类型: %s\n内容: %s",
		job.Name, job.JobType, truncateResult(job.Payload, 100)))

	switch job.JobType {
	case "workflow":
		teamName := fmt.Sprintf("cron-%s-%d", job.ID, time.Now().Unix()%10000)
		execErr = cs.executor.RunWorkflow(ctx, teamName, job.Workflow, job.Payload, job.ChatID)

	case "query":
		if job.Model != "" {
			if me, ok := cs.executor.(CronModelExecutor); ok {
				result, err := me.SendQueryWithModel(ctx, job.ChatID, job.Payload, job.Model)
				if err != nil {
					execErr = err
				} else if result != "" {
					cs.executor.Notify(job.ChatID, fmt.Sprintf("⏰ 定时任务 **%s** 结果 (模型 %s):\n\n%s", job.Name, job.Model, result))
				}
				break
			}
			log.Printf("[Cron] 任务 %s 配置了模型 %s 但执行器不支持按模型执行, 回退默认模型", job.ID, job.Model)
		}
		result, err := cs.executor.SendQuery(ctx, job.ChatID, job.Payload)
		if err != nil {
			execErr = err
		} else if result != "" {
			cs.executor.Notify(job.ChatID, fmt.Sprintf("⏰ 定时任务 **%s** 结果:\n\n%s", job.Name, result))
		}

	case "command":
		execErr = cs.executor.RunCommand(ctx, job.ChatID, job.Payload)

	case "sync":
		// payload = 数据源 (ima / weread)
		result, err := cs.executor.TriggerSync(ctx, job.Payload)
		if err != nil {
			execErr = err
		} else {
			cs.executor.Notify(job.ChatID, fmt.Sprintf("🔄 同步任务 **%s** 完成:\n%s", job.Name, result))
		}

	case "wiki-organize":
		mode := job.Payload
		if mode == "" {
			mode = "full"
		}
		result, err := cs.executor.WikiOrganize(ctx, mode)
		if err != nil {
			execErr = err
		} else {
			cs.executor.Notify(job.ChatID, fmt.Sprintf("📝 Wiki 整理完成:\n%s", result))
		}

	case "wiki-health-check":
		result, err := cs.executor.WikiHealthCheck(ctx)
		if err != nil {
			execErr = err
		} else {
			cs.executor.Notify(job.ChatID, fmt.Sprintf("🏥 Wiki 健康检查:\n%s", result))
		}

	case "wiki-lint":
		result, err := cs.executor.WikiLint(ctx)
		if err != nil {
			execErr = err
		} else {
			cs.executor.Notify(job.ChatID, fmt.Sprintf("🔍 Wiki Lint:\n%s", result))
		}

	default:
		execErr = fmt.Errorf("未知任务类型: %s", job.JobType)
	}

	duration := time.Since(start).Round(time.Second)

	cs.mu.Lock()
	job.LastRunAt = time.Now()
	job.RunCount++
	if execErr != nil {
		job.FailCount++
		job.LastResult = "失败: " + execErr.Error()
		cs.executor.Notify(job.ChatID, fmt.Sprintf("❌ 定时任务 **%s** 失败 (耗时 %v): %v", job.Name, duration, execErr))
	} else {
		job.LastResult = fmt.Sprintf("成功 (耗时 %v)", duration)
	}
	cs.persist()
	cs.mu.Unlock()
}

// --- Cron 表达式解析 (最小实现: 分 时 日 月 周) ---

// validateCronExpr 校验 5 字段 cron 表达式 (分 时 日 月 周) 的字段数与各字段取值域。
func validateCronExpr(expr string) error {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fmt.Errorf("需要 5 个字段 (分 时 日 月 周), 得到 %d 个", len(fields))
	}
	limits := []struct{ min, max int }{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	for i, f := range fields {
		if err := validateField(f, limits[i].min, limits[i].max); err != nil {
			return fmt.Errorf("字段 %d (%s): %w", i+1, f, err)
		}
	}
	return nil
}

// validateField 校验单个 cron 字段: 支持 `*`、`*/N` 步长、逗号分隔与 `a-b` 范围。
//
// 注意：只校验**语法与取值域**, 不校验语义 —— 例如 `5-1` 这种 lo>hi 的逆序范围会被
// 拒绝, 但 `*/0` 之外的退化表达式 (如超大步长) 仍会通过, 匹配时自然永不命中。
func validateField(field string, min, max int) error {
	if field == "*" {
		return nil
	}
	// 支持 */N 步长
	if strings.HasPrefix(field, "*/") {
		step := field[2:]
		if _, err := strconv.Atoi(step); err != nil {
			return fmt.Errorf("无效步长 %q", step)
		}
		return nil
	}
	// 支持逗号分隔
	for _, part := range strings.Split(field, ",") {
		// 支持范围 a-b
		if strings.Contains(part, "-") {
			bounds := strings.Split(part, "-")
			if len(bounds) != 2 {
				return fmt.Errorf("无效范围 %q", part)
			}
			lo, err1 := strconv.Atoi(bounds[0])
			hi, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil || lo < min || hi > max || lo > hi {
				return fmt.Errorf("无效范围 %q", part)
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < min || n > max {
			return fmt.Errorf("无效值 %q (范围 %d-%d)", part, min, max)
		}
	}
	return nil
}

// matchesCron 判断给定时刻是否命中 cron 表达式 (5 字段逐位与, 全中才算命中)。
func matchesCron(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	return matchField(fields[0], t.Minute(), 0, 59) &&
		matchField(fields[1], t.Hour(), 0, 23) &&
		matchField(fields[2], t.Day(), 1, 31) &&
		matchField(fields[3], int(t.Month()), 1, 12) &&
		matchField(fields[4], int(t.Weekday()), 0, 7) // 0 和 7 都是周日
}

// matchField 判断单个 cron 字段是否匹配当前值。
//
// 算法解读：
//
//	原理：把 `*`、`*/N`、逗号分隔的 `a-b` 范围与裸数字展开成对 value 的判定。
//	关键不变量：**星期字段的 0 与 7 都表示周日**, 必须双向等价 —— 只做 n==value
//	  会让 crontab 常见的 "7" 写法永远等不到触发。这里用 max==7 识别星期字段,
//	  避免把其他字段里的 0/7 也误当等价。
//	代价与边界：每字段 O(逗号项数); 步长以 (value-min) 为基准取模, 所以日/月字段
//	  (min 非 0) 的 `*/2` 得到 1,3,5..., 与标准 crontab 一致。
//	设计权衡：不支持 `?`/`L`/`W` 等扩展语法 —— validateField 同样拒绝它们, 校验与
//	  匹配两处口径一致, 不会出现"能存进去但永远不触发"的表达式。
func matchField(field string, value, min, max int) bool {
	if field == "*" {
		return true
	}
	if strings.HasPrefix(field, "*/") {
		step, _ := strconv.Atoi(field[2:])
		if step > 0 {
			return (value-min)%step == 0
		}
		return false
	}
	for _, part := range strings.Split(field, ",") {
		if strings.Contains(part, "-") {
			bounds := strings.Split(part, "-")
			lo, _ := strconv.Atoi(bounds[0])
			hi, _ := strconv.Atoi(bounds[1])
			if value >= lo && value <= hi {
				return true
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err == nil && n == value {
			return true
		}
		// 周日: 0 和 7 等价
		if err == nil && max == 7 && (n == 0 && value == 7 || n == 7 && value == 0) {
			return true
		}
	}
	return false
}

// --- 持久化 ---

// persist 把任务表以 JSON 落盘到 dataDir/cron_jobs.json; dataDir 为空或序列化失败
// 时静默跳过 (持久化是增强项, 不该让调度本身失败)。
func (cs *CronScheduler) persist() {
	if cs.dataDir == "" {
		return
	}
	os.MkdirAll(cs.dataDir, 0755)
	data, err := json.MarshalIndent(cs.jobs, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(cs.dataDir, "cron_jobs.json"), data, 0644)
}

// load 从 dataDir/cron_jobs.json 恢复任务表, 并把 nextID 推进到已有 id 的最大序号之后。
//
// 不推进 nextID 的话, 重启后 AddJob 会从 cron-1 重新发号, 直接覆盖掉恢复出来的同名任务。
func (cs *CronScheduler) load() {
	if cs.dataDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(cs.dataDir, "cron_jobs.json"))
	if err != nil {
		return
	}
	var jobs map[string]*CronJob
	if json.Unmarshal(data, &jobs) == nil && jobs != nil {
		cs.jobs = jobs
		for _, j := range jobs {
			if id := extractCronID(j.ID); id > cs.nextID {
				cs.nextID = id
			}
		}
	}
}

// extractCronID 从 "cron-<n>" 形式的 id 里取出末尾序号; 取不到时返回 0。
func extractCronID(id string) int {
	parts := strings.Split(id, "-")
	if len(parts) >= 2 {
		n, _ := strconv.Atoi(parts[len(parts)-1])
		return n
	}
	return 0
}

// --- 自然语言 cron 解析辅助 ---

// ParseNaturalSchedule 将中文自然语言时间描述转为 cron 表达式。
// 支持: "每天9点", "每小时", "每周一9点", "工作日早上9点", "每5分钟"
func ParseNaturalSchedule(text string) (string, string) {
	lower := strings.ToLower(text)

	if strings.Contains(lower, "每5分钟") || strings.Contains(lower, "每五分钟") {
		return "*/5 * * * *", "每5分钟"
	}
	if strings.Contains(lower, "每10分钟") || strings.Contains(lower, "每十分钟") {
		return "*/10 * * * *", "每10分钟"
	}
	if strings.Contains(lower, "每30分钟") || strings.Contains(lower, "每半小时") {
		return "*/30 * * * *", "每30分钟"
	}
	if strings.Contains(lower, "每小时") {
		return "0 * * * *", "每小时"
	}

	// 提取小时 (从大数字开始匹配, 避免 "10点" 误匹配 "0点")
	hour := -1
	for h := 23; h >= 0; h-- {
		patterns := []string{
			fmt.Sprintf("%d点", h), fmt.Sprintf("%d:00", h),
			fmt.Sprintf("早上%d点", h), fmt.Sprintf("下午%d点", h),
			fmt.Sprintf("晚上%d点", h),
		}
		for _, p := range patterns {
			if strings.Contains(lower, p) {
				if strings.Contains(lower, "下午") && h < 12 {
					hour = h + 12
				} else if strings.Contains(lower, "晚上") && h < 12 {
					hour = h + 12
				} else {
					hour = h
				}
				break
			}
		}
		if hour >= 0 {
			break
		}
	}
	if hour < 0 {
		hour = 9 // 默认早上 9 点
	}

	if strings.Contains(lower, "工作日") {
		return fmt.Sprintf("0 %d * * 1-5", hour), fmt.Sprintf("工作日%d点", hour)
	}

	weekdays := map[string]int{
		"周一": 1, "周二": 2, "周三": 3, "周四": 4, "周五": 5, "周六": 6, "周日": 0,
		"星期一": 1, "星期二": 2, "星期三": 3, "星期四": 4, "星期五": 5, "星期六": 6, "星期日": 0,
	}
	for name, wd := range weekdays {
		if strings.Contains(lower, name) {
			return fmt.Sprintf("0 %d * * %d", hour, wd), fmt.Sprintf("每%s%d点", name, hour)
		}
	}

	if strings.Contains(lower, "每天") || strings.Contains(lower, "每日") {
		return fmt.Sprintf("0 %d * * *", hour), fmt.Sprintf("每天%d点", hour)
	}

	// 默认每天
	return fmt.Sprintf("0 %d * * *", hour), fmt.Sprintf("每天%d点", hour)
}

// needsSessionTarget 报告该任务类型是否必须有一个具名会话 (chatID) 才能执行。
//
// 单列成函数而不是写在开关里: 将来加一类任务时, "它要不要会话"是一个必须被
// **显式回答**的问题 —— 漏答的默认是 false (不需要), 而 false 的后果就是这次
// 改动要消灭的那种静默。
func needsSessionTarget(jobType string) bool {
	switch jobType {
	case "query", "command", "workflow":
		return true
	default:
		// sync / wiki-* 只用 chatID 发通知, 不寻址会话, 空值可接受。
		return false
	}
}
