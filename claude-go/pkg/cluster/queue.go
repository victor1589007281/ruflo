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
	Status      string          `json:"status"` // pending|leased|completed|failed
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

// Pull worker 拉取一个可执行任务 (无任务返回 nil,false)。
// 先回收过期租约再挑最老的 pending; kinds 为空表示接受全部 Kind。
func (q *Queue) Pull(worker string, kinds []string) (*Task, bool, error) {
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
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt < tasks[j].CreatedAt })
	for i := range tasks {
		t := tasks[i]
		if t.Status != "pending" || !kindOK(t.Kind) {
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
