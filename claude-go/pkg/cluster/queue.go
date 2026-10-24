// Package dist 分布式任务分发 (design/02 §3.2/§3.3 R3 精简版)。
//
// 模型: 控制面持有任务队列 (Temporal 式 worker 拉取), worker 经 HTTP 长轮询
// Pull 任务、租约内执行、Complete/Fail 回报; 心跳=续租, 租约过期任务自动回队
// 重派 (worker 崩溃自愈)。注册表用同一租约机制管 worker 存活 (design/02 §3.4.5
// 三级心跳的 ①②)。
//
// 存储经 pkg/statestore 抽象: 单机 file 后端即可跑 T2 形态; 分布式后端 (redis/
// nats) 换 StateStore 实现即可, 本包零改动。
package cluster

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// Task 分发任务。Payload 语义由 Kind 决定 (v1: "stage" = 单阶段 agent 执行)。
type Task struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	RunID       string          `json:"run_id,omitempty"` // trace 四元组
	NodeID      string          `json:"node_id,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	RequireCaps []string        `json:"require_caps,omitempty"` // 需要的能力标签 (bash/browser/k8s-sandbox); worker 须全部具备 (design/01 §4.9 Placement)
	Status      string          `json:"status"`                 // pending|leased|completed|failed
	Worker      string          `json:"worker,omitempty"`
	LeaseUntil  int64           `json:"lease_until,omitempty"` // unix milli
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts,omitempty"` // 0=默认3
	Result      json.RawMessage `json:"result,omitempty"`
	Err         string          `json:"err,omitempty"`
	CreatedAt   int64           `json:"created_at"`
	UpdatedAt   int64           `json:"updated_at"`
}

// Queue 租约式任务队列 (进程内互斥 + StateStore 持久化, 控制面单实例假设;
// 多控制面副本需 per-run 单主租约, 属 T3, 见 design/02 §3.2)。
type Queue struct {
	mu    sync.Mutex
	kv    statestore.KVStore
	lease time.Duration
}

// NewQueue lease 为任务默认租约时长 (0 → 5min)。
func NewQueue(ss statestore.StateStore, lease time.Duration) *Queue {
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	return &Queue{kv: ss.KV("dist-tasks"), lease: lease}
}

// Enqueue 入队。ID 为空自动生成。
func (q *Queue) Enqueue(t Task) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t.ID == "" {
		t.ID = trace.NewID("task")
	}
	if t.Kind == "" {
		return "", fmt.Errorf("dist: task.Kind 不能为空")
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = 3
	}
	now := time.Now().UnixMilli()
	t.Status = "pending"
	t.CreatedAt, t.UpdatedAt = now, now
	return t.ID, q.kv.Put(t.ID, t)
}

// Pull worker 拉取一个可执行任务 (无任务返回 nil,false)。kinds 为空=接受全部 Kind。
// 不做能力过滤 (向后兼容); 需要能力标签路由用 PullFor。
func (q *Queue) Pull(worker string, kinds []string) (*Task, bool, error) {
	return q.PullFor(worker, kinds, nil)
}

// PullFor 能力感知拉取 (design/01 §4.9 / design/02 §3.3 R3): 只拉取
// RequireCaps ⊆ workerCaps 的任务 —— 需要 browser/k8s-sandbox 等能力的任务
// 不会被不具备该能力的 worker 拉走。
//
// 算法解读：
//
//	原理：拉取即**占有**。整个"回收过期租约 → 全量读 → 按 CreatedAt 升序找第一个
//	      可执行任务 → 改状态为 leased 并写回"的过程在一把进程内互斥锁里完成,
//	      于是同一时刻只有一个 worker 能拿到同一个任务。
//	关键不变量：先 reapLocked 再 allLocked。顺序反了的话, 上一轮 worker 崩溃留下的
//	      过期租约任务在本轮仍是 leased 状态, 会被跳过 —— 表现为"任务永远不再被派发",
//	      而队列看起来是满的。Attempts 也必须在**派发时**自增 (不是完成时), 否则
//	      一个反复崩溃的 worker 会让 Attempts 永远停在 0, 重试上限形同虚设。
//	代价与边界：每次拉取都是 O(全队列) —— 读全部任务、排序、再遍历。任务量大时
//	      这是平方级开销; 控制面单实例假设下可接受, 多副本需要 per-run 单主租约 (T3)。
//	设计权衡：FIFO 按 CreatedAt 排序而不是优先级队列 —— 先到先得是这里唯一需要的
//	      公平性, 而优先级会引入饥饿, 需要额外的老化机制才能补回来。
func (q *Queue) PullFor(worker string, kinds, workerCaps []string) (*Task, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.reapLocked(); err != nil {
		return nil, false, err
	}
	tasks, err := q.allLocked()
	if err != nil {
		return nil, false, err
	}
	kindOK := func(k string) bool {
		if len(kinds) == 0 {
			return true
		}
		for _, x := range kinds {
			if x == k {
				return true
			}
		}
		return false
	}
	capsOK := func(require []string) bool {
		for _, need := range require {
			has := false
			for _, have := range workerCaps {
				if have == need {
					has = true
					break
				}
			}
			if !has {
				return false // worker 缺少必需能力
			}
		}
		return true
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt < tasks[j].CreatedAt })
	for i := range tasks {
		t := tasks[i]
		if t.Status != "pending" || !kindOK(t.Kind) || !capsOK(t.RequireCaps) {
			continue
		}
		t.Status = "leased"
		t.Worker = worker
		t.Attempts++
		t.LeaseUntil = time.Now().Add(q.lease).UnixMilli()
		t.UpdatedAt = time.Now().UnixMilli()
		if err := q.kv.Put(t.ID, t); err != nil {
			return nil, false, err
		}
		return &t, true, nil
	}
	return nil, false, nil
}

// Extend 续租 (worker 心跳期间调用, 长任务防误回收)。
func (q *Queue) Extend(id, worker string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, err := q.getLocked(id)
	if err != nil {
		return err
	}
	if t.Status != "leased" || t.Worker != worker {
		return fmt.Errorf("dist: 任务 %s 不在 %s 的租约内 (status=%s worker=%s)", id, worker, t.Status, t.Worker)
	}
	t.LeaseUntil = time.Now().Add(q.lease).UnixMilli()
	t.UpdatedAt = time.Now().UnixMilli()
	return q.kv.Put(id, *t)
}

// Complete 上报成功结果。
func (q *Queue) Complete(id, worker string, result json.RawMessage) error {
	return q.finish(id, worker, "completed", result, "")
}

// Fail 上报失败; 未达 MaxAttempts 时回 pending 重派, 达上限终态 failed。
func (q *Queue) Fail(id, worker, errMsg string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, err := q.getLocked(id)
	if err != nil {
		return err
	}
	if t.Status != "leased" || t.Worker != worker {
		return fmt.Errorf("dist: 任务 %s 不在 %s 的租约内", id, worker)
	}
	t.UpdatedAt = time.Now().UnixMilli()
	t.Err = errMsg
	if t.Attempts >= t.MaxAttempts {
		t.Status = "failed"
	} else {
		t.Status = "pending" // 回队重派
		t.Worker = ""
		t.LeaseUntil = 0
	}
	return q.kv.Put(id, *t)
}

// finish 终态收尾: 校验租约归属后写入状态/结果/错误并落库 (Complete 的内部实现)。
func (q *Queue) finish(id, worker, status string, result json.RawMessage, errMsg string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, err := q.getLocked(id)
	if err != nil {
		return err
	}
	if t.Status != "leased" || t.Worker != worker {
		return fmt.Errorf("dist: 任务 %s 不在 %s 的租约内 (status=%s)", id, worker, t.Status)
	}
	t.Status = status
	t.Result = result
	t.Err = errMsg
	t.UpdatedAt = time.Now().UnixMilli()
	return q.kv.Put(id, *t)
}

// Get 查询任务。
func (q *Queue) Get(id string) (*Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.getLocked(id)
}

// Wait 阻塞等待任务终态 (轮询实现, 控制面内部使用)。
func (q *Queue) Wait(id string, timeout time.Duration) (*Task, error) {
	deadline := time.Now().Add(timeout)
	for {
		t, err := q.Get(id)
		if err != nil {
			return nil, err
		}
		if t.Status == "completed" || t.Status == "failed" {
			return t, nil
		}
		if time.Now().After(deadline) {
			return t, fmt.Errorf("dist: 等待任务 %s 超时 (status=%s)", id, t.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// List 全量列出 (观测用)。
func (q *Queue) List() ([]Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.reapLocked(); err != nil {
		return nil, err
	}
	return q.allLocked()
}

// getLocked 读一个任务; 不存在时返回错误 (调用方须已持 q.mu)。
func (q *Queue) getLocked(id string) (*Task, error) {
	var t Task
	ok, err := q.kv.Get(id, &t)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("dist: 任务 %s 不存在", id)
	}
	return &t, nil
}

// allLocked 全量读回任务 (调用方须已持 q.mu)。
//
// 单个 key 反序列化失败会被静默跳过 (err != nil || !ok 都 continue), 不中断整轮读取 ——
// 一条坏记录不该让整个队列拉不动。
func (q *Queue) allLocked() ([]Task, error) {
	keys, err := q.kv.Keys()
	if err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(keys))
	for _, k := range keys {
		var t Task
		if ok, err := q.kv.Get(k, &t); err == nil && ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// reapLocked 回收过期租约: leased 且 lease_until 过期 → 回 pending (worker 崩溃自愈);
// 已达 MaxAttempts 的直接终态 failed。
func (q *Queue) reapLocked() error {
	tasks, err := q.allLocked()
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for i := range tasks {
		t := tasks[i]
		if t.Status != "leased" || t.LeaseUntil == 0 || t.LeaseUntil >= now {
			continue
		}
		t.UpdatedAt = now
		if t.Attempts >= t.MaxAttempts {
			t.Status = "failed"
			t.Err = fmt.Sprintf("租约过期且已达最大尝试次数 (%d), worker=%s", t.MaxAttempts, t.Worker)
		} else {
			t.Status = "pending"
			t.Worker = ""
			t.LeaseUntil = 0
		}
		if err := q.kv.Put(t.ID, t); err != nil {
			return err
		}
	}
	return nil
}
