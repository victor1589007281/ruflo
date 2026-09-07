package cluster

import (
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool"
)

// WorkerInfo worker 注册信息 (design/02 §3.4.5 心跳①: runtime 级 lease)。
type WorkerInfo struct {
	Name      string   `json:"name"`
	Caps      []string `json:"caps,omitempty"` // 能力标签: bash/browser/k8s-sandbox/... (design/01 §4.9 Placement)
	Kinds     []string `json:"kinds,omitempty"`
	LastBeat  int64    `json:"last_beat"` // unix milli
	FirstSeen int64    `json:"first_seen"`
	TasksDone int      `json:"tasks_done"`
	// 池观测摘要 (13.7.9): worker 心跳经 tool.ObservedPool() 取包级快照上报。
	// nil = 未观测到池 (池开关未开或本进程无会话装配) —— 控制面渲染"池未启用"。
	Pool *tool.PoolSnapshot `json:"pool,omitempty"`
}

// Registry worker 注册表, lease 过期即摘除 (List 时惰性清理)。
type Registry struct {
	mu    sync.Mutex
	kv    statestore.KVStore
	lease time.Duration
}

// NewRegistry lease 为心跳有效期 (0 → 90s, 即 3 个 30s 心跳周期)。
func NewRegistry(ss statestore.StateStore, lease time.Duration) *Registry {
	if lease <= 0 {
		lease = 90 * time.Second
	}
	return &Registry{kv: ss.KV("dist-workers"), lease: lease}
}

// Heartbeat 注册/续约二合一 (幂等)。
func (r *Registry) Heartbeat(w WorkerInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var cur WorkerInfo
	ok, _ := r.kv.Get(w.Name, &cur)
	now := time.Now().UnixMilli()
	if ok {
		cur.LastBeat = now
		cur.Caps, cur.Kinds = w.Caps, w.Kinds
		// 池摘要整体替换 (nil 也写回: worker 侧关池/未装配时摘要随之消失)。
		cur.Pool = w.Pool
		if w.TasksDone > cur.TasksDone {
			cur.TasksDone = w.TasksDone
		}
		return r.kv.Put(w.Name, cur)
	}
	w.FirstSeen, w.LastBeat = now, now
	return r.kv.Put(w.Name, w)
}

// Alive 列出租约内存活的 worker; 过期的顺手删除。
func (r *Registry) Alive() ([]WorkerInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys, err := r.kv.Keys()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-r.lease).UnixMilli()
	var out []WorkerInfo
	for _, k := range keys {
		var w WorkerInfo
		if ok, err := r.kv.Get(k, &w); err != nil || !ok {
			continue
		}
		if w.LastBeat < cutoff {
			_ = r.kv.Delete(k) // 过期摘除 (design/02: lease 过期=停滞)
			continue
		}
		out = append(out, w)
	}
	return out, nil
}
