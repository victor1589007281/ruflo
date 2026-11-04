package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// ============================================================================
// R-H13 持久后台任务队列
// ============================================================================
//
// 存储键方案（bucket 固定为 "harness-tasks"）：
//
//	task/<id>  活跃任务（pending | leased | completed）。Claim 只扫描这个前缀。
//	dead/<id>  死信任务快照。任务进入死信时**搬移**到这里，Claim 永不读该前缀。
//
// 为什么用前缀搬移而不是在同一记录上标个 status=dead：死信是运维面（重放/丢弃/
// 统计）唯一关心的集合，单独前缀让它 O(死信数) 而不是 O(全队列)；同时也把
// "还能不能被执行"（live 路径）与"被谁放弃、为什么"（dead 路径）两个生命周期
// 分开，重放死信时不必逆向解析状态机。
//
// 单实例假设：进程内 mu 串行化"回收过期租约 → 全量读 → 选任务 → 写回"的整个
// 过程，因此同一时刻只有一个 worker 能占有同一个任务。多副本控制面需要
// per-task 分布式租约（本包不做，见 cluster.Queue 的同类边界说明）。

// 任务状态机。pending --claim--> leased --ack--> completed
//
//	\--nack(未达上限)--> pending
//	\--nack/租约过期(达上限)--> dead
//
// pending 是唯一可被 Claim 选中的状态；dead 只存在于 dead/ 前缀。
const (
	statusPending   = "pending"
	statusLeased    = "leased"
	statusCompleted = "completed"
	statusDead      = "dead"
)

const (
	// taskBucket 是任务记录所在的 KV bucket。
	taskBucket = "harness-tasks"
	// liveKeyPrefix / deadKeyPrefix 见文件头键方案说明。
	liveKeyPrefix = "task/"
	deadKeyPrefix = "dead/"
)

// 队列默认值。可被 QueueOptions 覆盖。
const (
	defaultLease       = 5 * time.Minute
	defaultMaxAttempts = 3
	defaultBackoffBase = time.Second
	defaultBackoffMax  = time.Minute
)

// QueueOptions 是 StoreQueue 的可调参数。零值全部取保守默认。
type QueueOptions struct {
	// Now 是时钟注入点（nil = time.Now）。测试用假时钟推进租约/退避，
	// 不靠 time.Sleep —— 多秒 sleep 既慢又不稳定。
	Now func() time.Time
	// DefaultLease 是 Claim 传 lease<=0 时使用的租约时长（默认 5min）。
	DefaultLease time.Duration
	// DefaultMaxAttempts 是 Task.MaxAttempts<=0 时生效的上限（默认 3）。
	DefaultMaxAttempts int
	// BackoffBase / BackoffMax 是 Nack 指数退避的起点与封顶（默认 1s / 1min）。
	// 必须有封顶：无上限的 2^n 在几十次重试后会溢出成负数，任务反而立刻可领。
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

// storeTask 是任务的持久化形状 = 对外 Task + 队列内部状态。
//
// 内部状态不塞进 Task：Task 是对业务层的冻结契约（只增不删），而 status/attempts/
// 租约这些都是队列实现细节，混进去会让业务层误以为可以写。
type storeTask struct {
	Task
	Status     string    `json:"status"`
	Attempts   int       `json:"attempts"`
	LeaseUntil time.Time `json:"leaseUntil,omitempty"`
	Worker     string    `json:"worker,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// Cause 是最后一次失败的说明（Nack 的 cause / 租约过期原因），仅用于死信排障。
	Cause  string    `json:"cause,omitempty"`
	DeadAt time.Time `json:"deadAt,omitempty"`
}

// claimed 把持久化记录投影成对外的 ClaimedTask。返回副本，调用方改它不会影响队列。
func (r *storeTask) claimed() *ClaimedTask {
	return &ClaimedTask{
		Task:       r.Task,
		Attempts:   r.Attempts,
		LeaseUntil: r.LeaseUntil,
		Worker:     r.Worker,
	}
}

// StoreQueue 是 TaskQueue 的 StateStore 实现。
//
// 一个实例内的所有读写都在 mu 下完成 —— 持久层（file/redis）单次 Put 是原子的，
// 但"读-判-写"不是；没有这把锁，两个 worker 会同时选中同一个 pending 任务。
type StoreQueue struct {
	mu sync.Mutex
	kv statestore.KVStore

	now                func() time.Time
	defaultLease       time.Duration
	defaultMaxAttempts int
	backoffBase        time.Duration
	backoffMax         time.Duration
}

// 编译期断言：StoreQueue 必须始终满足冻结的 TaskQueue 契约。
var _ TaskQueue = (*StoreQueue)(nil)

// NewStoreQueue 构造持久任务队列。bucket 固定 "harness-tasks"（键方案见文件头）。
func NewStoreQueue(ss statestore.StateStore, opts QueueOptions) *StoreQueue {
	q := &StoreQueue{
		kv:                 ss.KV(taskBucket),
		now:                opts.Now,
		defaultLease:       opts.DefaultLease,
		defaultMaxAttempts: opts.DefaultMaxAttempts,
		backoffBase:        opts.BackoffBase,
		backoffMax:         opts.BackoffMax,
	}
	if q.now == nil {
		q.now = time.Now
	}
	if q.defaultLease <= 0 {
		q.defaultLease = defaultLease
	}
	if q.defaultMaxAttempts <= 0 {
		q.defaultMaxAttempts = defaultMaxAttempts
	}
	if q.backoffBase <= 0 {
		q.backoffBase = defaultBackoffBase
	}
	if q.backoffMax <= 0 {
		q.backoffMax = defaultBackoffMax
	}
	if q.backoffMax < q.backoffBase {
		q.backoffMax = q.backoffBase
	}
	return q
}

// Enqueue 入队。Task.ID 为空时生成；显式 ID 重复则报错而不是覆盖 ——
// 覆盖一个在途任务等于把它的租约和 attempts 一起抹掉。
func (q *StoreQueue) Enqueue(ctx context.Context, task Task) (TaskID, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	if task.Kind == "" {
		return "", fmt.Errorf("harness: task.Kind 不能为空")
	}
	if task.ID == "" {
		task.ID = newTaskID()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	// 存在性检查必须在本实例锁内：否则两个并发 Enqueue 会都看到"不存在"。
	if _, ok, err := q.getLiveLocked(task.ID); err != nil {
		return "", err
	} else if ok {
		return "", fmt.Errorf("harness: 任务 %s 已存在", task.ID)
	}
	if task.MaxAttempts <= 0 {
		task.MaxAttempts = q.defaultMaxAttempts
	}
	now := q.now()
	rec := storeTask{
		Task:      task,
		Status:    statusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := q.kv.Put(liveKey(task.ID), rec); err != nil {
		return "", err
	}
	return TaskID(task.ID), nil
}

// Claim 是不带能力过滤的取任务入口。nil caps 交给 ClaimFor 后表示
// **完全忽略 RequireCaps**（兼容不宣告能力的旧调用方，见 ClaimFor 说明）。
// 空队列返回 (nil, nil)，不是错误 —— "暂时没活"是 worker 的正常稳态。
func (q *StoreQueue) Claim(ctx context.Context, worker string, kinds []string, lease time.Duration) (*ClaimedTask, error) {
	return q.ClaimFor(ctx, worker, kinds, nil, lease)
}

// ClaimFor 是能力感知取任务。
//
// 算法与不变量（与 cluster.Queue.PullFor 同源）：
//
//	原理：拉取即**占有**。"回收过期租约 → 全量读 → 按 CreatedAt 升序找第一个
//	      可执行任务 → 标 leased 并写回"整段在一把锁里，同一时刻只有一个 worker
//	      能拿到同一个任务。
//	关键不变量①：先 reapLocked 再 loadAllLocked。顺序反了，崩溃 worker 留下的
//	      过期租约任务在扫描时仍是 leased，会被跳过 —— 任务永远不再被派发，
//	      而队列看起来是满的。
//	关键不变量②：Attempts 在**派发时**自增，不在完成时。只在完成时加的话，
//	      反复崩溃的 worker 会让 Attempts 永远停在 0，重试上限只是装饰。
//	能力语义：caps == nil → 忽略 RequireCaps（Claim 的旧路径）；caps 非 nil
//	      （包括显式空切片）→ 要求 RequireCaps ⊆ caps，不具备能力的 worker
//	      不会把任务领走再失败。空切片表示"我没有任何能力"，只接无要求任务。
//	代价：每次领取 O(全队列)；任务量大时是平方级开销，单实例控制面可接受。
func (q *StoreQueue) ClaimFor(ctx context.Context, worker string, kinds, caps []string, lease time.Duration) (*ClaimedTask, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	if err := q.reapLocked(); err != nil {
		return nil, err
	}
	recs, err := q.allLiveLocked()
	if err != nil {
		return nil, err
	}
	// FIFO：先到先得是这里唯一需要的公平性。同 CreatedAt 用 ID 兜底，保证
	// 测试与多实例重放的顺序确定（map 遍历顺序不可依赖）。
	sort.Slice(recs, func(i, j int) bool {
		if !recs[i].CreatedAt.Equal(recs[j].CreatedAt) {
			return recs[i].CreatedAt.Before(recs[j].CreatedAt)
		}
		return recs[i].ID < recs[j].ID
	})

	now := q.now()
	enforceCaps := caps != nil
	for i := range recs {
		r := &recs[i]
		if r.Status != statusPending {
			continue
		}
		// VisibleAt 是延迟任务/退避任务的可领取时间。零值 = 立刻。
		if !r.VisibleAt.IsZero() && now.Before(r.VisibleAt) {
			continue
		}
		if !kindAllowed(kinds, r.Kind) {
			continue
		}
		if enforceCaps && !capsSatisfy(caps, r.RequireCaps) {
			continue
		}
		r.Status = statusLeased
		r.Worker = worker
		r.Attempts++ // 派发即计数（不变量②）
		r.LeaseUntil = now.Add(q.effectiveLease(lease))
		r.UpdatedAt = now
		if err := q.kv.Put(liveKey(r.ID), *r); err != nil {
			return nil, err
		}
		return r.claimed(), nil
	}
	return nil, nil
}

// Ack 标记完成。对已完成任务重复 Ack 返回 nil（幂等）：worker 在写完结果后崩溃、
// 重启又报告一次成功，这是至少一次投递下的正常现象，不该让调用方处理错误。
func (q *StoreQueue) Ack(ctx context.Context, id TaskID) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok, err := q.getLiveLocked(string(id))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("harness: 任务 %s 不存在", id)
	}
	switch r.Status {
	case statusCompleted:
		return nil
	case statusDead:
		// 已进死信说明队列已放弃该任务；迟到的成功上报只作为无操作。
		// 若在这里改回 completed，死信前缀与 live 记录就会分叉。
		return nil
	}
	now := q.now()
	r.Status = statusCompleted
	r.LeaseUntil = time.Time{}
	r.UpdatedAt = now
	return q.kv.Put(liveKey(r.ID), *r)
}

// Nack 报告失败：未达上限回 pending 并指数退避，达到上限进死信。
//
// 退避（VisibleAt）而不是立刻回队：失败往往是下游暂时不可用，立刻重试只会
// 把同一次故障放大成重试风暴。
//
// 注意：TaskQueue 的 Nack 不带 worker 身份。若本 worker 的租约已被回收并被他
// 人重新领取，这次 Nack 会被记到新一次尝试上 —— 调用方必须在确认自己仍持有
// 租约后才 Nack（Renew 失败即停止处理）。
func (q *StoreQueue) Nack(ctx context.Context, id TaskID, cause error) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok, err := q.getLiveLocked(string(id))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("harness: 任务 %s 不存在", id)
	}
	switch r.Status {
	case statusCompleted, statusDead:
		// 迟到的失败上报：任务已有终态，不能把它拉回来重跑。
		return nil
	case statusPending:
		// 已被 reaper 回收，已经可再领取，无需重复退避。
		return nil
	}
	now := q.now()
	r.Cause = errString(cause)
	if r.Attempts >= q.maxAttempts(r) {
		return q.deadLetterLocked(r, r.Cause)
	}
	r.Status = statusPending
	r.Worker = ""
	r.LeaseUntil = time.Time{}
	r.VisibleAt = now.Add(q.backoff(r.Attempts))
	r.UpdatedAt = now
	return q.kv.Put(liveKey(r.ID), *r)
}

// Renew 续租。**必须能失败**：调用方把续租失败当作"我已失去该任务"的信号。
//
// 局限（由冻结的 TaskQueue 契约决定）：Renew 没有 worker 参数，所以它只能拒绝
// "已不是 leased / 租约已过期"的情况，无法识别"租约被回收后又被**别人**重新
// 领取"（新租约同样未过期）。需要归属校验的 worker 应使用 RenewFor。
func (q *StoreQueue) Renew(ctx context.Context, id TaskID, lease time.Duration) error {
	return q.renew(ctx, id, "", lease)
}

// RenewFor 是带 worker 归属校验的续租。reaped 后被其他 worker 重新领取时，
// 旧持有者调用本方法会拿到错误而不是替新持有者延长租约 —— 否则两个 worker
// 会同时认为自己持有任务，表现为静默的重复执行。
func (q *StoreQueue) RenewFor(ctx context.Context, id TaskID, worker string, lease time.Duration) error {
	return q.renew(ctx, id, worker, lease)
}

// renew 是 Renew/RenewFor 的共同实现。worker 为空表示不做归属校验。
func (q *StoreQueue) renew(ctx context.Context, id TaskID, worker string, lease time.Duration) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok, err := q.getLiveLocked(string(id))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("harness: 任务 %s 不存在", id)
	}
	if r.Status != statusLeased {
		return fmt.Errorf("harness: 任务 %s 租约已失效 (status=%s)", id, r.Status)
	}
	now := q.now()
	// 过期租约不允许复活：此刻 reaper 可能正准备回收它，续租成功就等于和
	// 回收赛跑，结果取决于调度顺序 —— 这种不确定性比直接失败更难排查。
	if !now.Before(r.LeaseUntil) {
		return fmt.Errorf("harness: 任务 %s 租约已过期 (until=%s)", id, r.LeaseUntil.Format(time.RFC3339Nano))
	}
	if worker != "" && r.Worker != worker {
		return fmt.Errorf("harness: 任务 %s 当前由 %q 持有，不是 %q", id, r.Worker, worker)
	}
	r.LeaseUntil = now.Add(q.effectiveLease(lease))
	r.UpdatedAt = now
	return q.kv.Put(liveKey(r.ID), *r)
}

// DeadLetters 返回死信任务 ID，按字典序排序。
//
// 排序是契约的一部分：运维脚本 diff 两次输出时，无序结果会把"没有变化"误报成
// "一批任务新进死信"。KVStore.Keys 虽已排序，这里仍显式再排一次 —— 后端实现
// 换掉时不能把确定性寄托在别人的文档上。
func (q *StoreQueue) DeadLetters(ctx context.Context) ([]TaskID, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	keys, err := q.kv.Keys()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		if len(k) > len(deadKeyPrefix) && k[:len(deadKeyPrefix)] == deadKeyPrefix {
			ids = append(ids, k[len(deadKeyPrefix):])
		}
	}
	sort.Strings(ids)
	out := make([]TaskID, len(ids))
	for i, id := range ids {
		out[i] = TaskID(id)
	}
	return out, nil
}

// ============================================================================
// 内部实现（所有 *Locked 方法要求调用方已持有 q.mu）
// ============================================================================

// reapLocked 回收过期租约：leased 且 lease_until 已过 → 回 pending（崩溃自愈）；
// 已达 MaxAttempts 的直接进死信。
//
// 调用顺序约束见 ClaimFor 的不变量①：必须在扫描之前、同一把锁内执行。
func (q *StoreQueue) reapLocked() error {
	recs, err := q.allLiveLocked()
	if err != nil {
		return err
	}
	now := q.now()
	for i := range recs {
		r := &recs[i]
		if r.Status != statusLeased || r.LeaseUntil.IsZero() || now.Before(r.LeaseUntil) {
			continue
		}
		if r.Attempts >= q.maxAttempts(r) {
			r.Cause = fmt.Sprintf("租约过期且已达最大尝试次数 (%d), worker=%s", q.maxAttempts(r), r.Worker)
			if err := q.deadLetterLocked(r, r.Cause); err != nil {
				return err
			}
			continue
		}
		// 不做退避：租约过期通常意味 worker 崩溃，不是下游故障，立即重派能让
		// 恢复时间与租约时长同阶，而不是再叠加一次退避。
		r.Status = statusPending
		r.Worker = ""
		r.LeaseUntil = time.Time{}
		r.VisibleAt = time.Time{}
		r.UpdatedAt = now
		if err := q.kv.Put(liveKey(r.ID), *r); err != nil {
			return err
		}
	}
	return nil
}

// deadLetterLocked 把任务搬进死信前缀，幂等。
//
// 写入顺序：先把 live 记录标成 dead（不可再领取），再写 dead 快照，最后删 live。
// 若中途失败，live 侧已是 dead，任务不会被执行；调用方（Nack）可重试本函数完成
// 搬移。反过来先写 dead 再标 live 的话，崩在两步之间会留下"死信里有一份、live
// 里还 leased"的记录，reaper 会把它再派发一次 —— 让已放弃的任务多跑一轮。
func (q *StoreQueue) deadLetterLocked(r *storeTask, cause string) error {
	now := q.now()
	r.Status = statusDead
	r.Cause = cause
	r.DeadAt = now
	r.UpdatedAt = now
	if err := q.kv.Put(liveKey(r.ID), *r); err != nil {
		return err
	}
	if err := q.kv.Put(deadKey(r.ID), *r); err != nil {
		return err
	}
	return q.kv.Delete(liveKey(r.ID))
}

// getLiveLocked 读一个 live 任务；不存在返回 (nil, false, nil)。
func (q *StoreQueue) getLiveLocked(id string) (*storeTask, bool, error) {
	var r storeTask
	ok, err := q.kv.Get(liveKey(id), &r)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return &r, true, nil
}

// allLiveLocked 全量读回 live 任务。
//
// 单条记录反序列化失败静默跳过：一条坏记录不该让整个队列拉不动（与
// cluster.Queue.allLocked 同策略）。跳过意味着它暂时不可见，而不是被删除。
func (q *StoreQueue) allLiveLocked() ([]storeTask, error) {
	keys, err := q.kv.Keys()
	if err != nil {
		return nil, err
	}
	out := make([]storeTask, 0, len(keys))
	for _, k := range keys {
		if len(k) <= len(liveKeyPrefix) || k[:len(liveKeyPrefix)] != liveKeyPrefix {
			continue
		}
		var r storeTask
		if ok, err := q.kv.Get(k, &r); err == nil && ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// effectiveLease 解析租约时长：Claim 传 0 用队列默认。
func (q *StoreQueue) effectiveLease(lease time.Duration) time.Duration {
	if lease > 0 {
		return lease
	}
	return q.defaultLease
}

// maxAttempts 解析重试上限。Enqueue 已把 0 归一化，这里仍兜底，是为了兼容
// 升级前写入的 MaxAttempts=0 记录 —— 旧记录不能因为默认值变动就永远不放弃。
func (q *StoreQueue) maxAttempts(r *storeTask) int {
	if r.MaxAttempts > 0 {
		return r.MaxAttempts
	}
	return q.defaultMaxAttempts
}

// backoff 计算第 attempts 次失败后的退避：base * 2^(attempts-1)，封顶 max。
// 逐步乘并检查上限（而不是算 2^n 再截断）：后者在 attempts 较大时会先溢出。
func (q *StoreQueue) backoff(attempts int) time.Duration {
	d := q.backoffBase
	for i := 1; i < attempts && d < q.backoffMax; i++ {
		d *= 2
	}
	if d > q.backoffMax {
		d = q.backoffMax
	}
	return d
}

func liveKey(id string) string { return liveKeyPrefix + id }
func deadKey(id string) string { return deadKeyPrefix + id }

// kindAllowed 判断 Kind 是否可领取。kinds 为空 = 接受全部（旧调用方不筛选）。
func kindAllowed(kinds []string, kind string) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// capsSatisfy 判断 worker 是否具备任务要求的全部能力（RequireCaps ⊆ workerCaps）。
// RequireCaps 为空恒为 true：无能力要求的任务任何 worker 都能接。
func capsSatisfy(workerCaps, require []string) bool {
	for _, need := range require {
		found := false
		for _, have := range workerCaps {
			if have == need {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// newTaskID 生成 task-<32hex>。不用 trace.NewID（6 hex）是因为任务 ID 会长期
// 落盘，~1.6e7 的碰撞域在累计任务量上会真的撞；128 位随机不需要协调也不会撞。
func newTaskID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败时退化：仍保证非空且带时间分量。
		return fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	return "task-" + hex.EncodeToString(b[:])
}

// ctxErr 统一做入口取消检查。KVStore 接口不接 ctx，长扫描无法中途取消，
// 但至少在开头就能让已取消的调用快速返回。
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
