// registry_pool_test.go —— 13.7.9 池观测: WorkerInfo.Pool 字段在心跳合并中的语义。
//
// 锁定: ① 注册时携带 Pool → 落 KV; ② 存量 worker 心跳整体替换 (nil 也写回,
// 摘要随 worker 侧池装配状态消失); ③ TasksDone 单调不受影响; ④ Alive 原样带出。
package cluster

import (
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	ss := statestore.NewFileStore(t.TempDir())
	return NewRegistry(ss, 90*time.Second)
}

func TestHeartbeatCarriesPool(t *testing.T) {
	r := newTestRegistry(t)

	// ① 首次注册带池摘要。
	p := tool.NewPool()
	p.AddMember("evo_status")
	p.Load("evo_status")
	snap := p.Snapshot()
	if err := r.Heartbeat(WorkerInfo{Name: "w1", Caps: []string{"bash"}, Pool: &snap}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	alive, err := r.Alive()
	if err != nil || len(alive) != 1 {
		t.Fatalf("Alive()=%v,%v", alive, err)
	}
	if alive[0].Pool == nil || alive[0].Pool.Members != 1 || len(alive[0].Pool.Loaded) != 1 {
		t.Fatalf("注册时池摘要未落 KV: %+v", alive[0].Pool)
	}

	// ② 存量心跳 nil 摘要 → 整体替换写回 (worker 关池后摘要消失)。
	if err := r.Heartbeat(WorkerInfo{Name: "w1", Caps: []string{"bash"}}); err != nil {
		t.Fatalf("Heartbeat#2: %v", err)
	}
	alive, _ = r.Alive()
	if alive[0].Pool != nil {
		t.Fatalf("nil 摘要必须整体替换写回, got %+v", alive[0].Pool)
	}

	// ③ 再带新摘要 + TasksDone 单调不变。
	snap2 := tool.PoolSnapshot{Enabled: true, Members: 5}
	if err := r.Heartbeat(WorkerInfo{Name: "w1", TasksDone: 3, Pool: &snap2}); err != nil {
		t.Fatalf("Heartbeat#3: %v", err)
	}
	alive, _ = r.Alive()
	if alive[0].Pool == nil || alive[0].Pool.Members != 5 {
		t.Fatalf("新摘要未替换: %+v", alive[0].Pool)
	}
	if alive[0].TasksDone != 3 {
		t.Fatalf("TasksDone=%d, want 3", alive[0].TasksDone)
	}
	if err := r.Heartbeat(WorkerInfo{Name: "w1", TasksDone: 1}); err != nil { // 乱序回退
		t.Fatal(err)
	}
	alive, _ = r.Alive()
	if alive[0].TasksDone != 3 {
		t.Fatalf("乱序心跳回退 TasksDone: %d, want 3", alive[0].TasksDone)
	}
	// 无摘要 (nil) 的心跳同样整体替换清空: worker 每次心跳都带 poolSnapshot(),
	// nil 就是明确的"本 worker 无池"信号, 协议层不存在"未带"与"显式 nil"之别。
	if alive[0].Pool != nil {
		t.Fatalf("nil 摘要心跳应清空已有摘要, got %+v", alive[0].Pool)
	}
}
