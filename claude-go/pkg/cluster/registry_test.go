package cluster

import (
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

func TestRegistryHeartbeatAndAlive(t *testing.T) {
	r := NewRegistry(statestore.NewMemStore(), time.Minute)
	if err := r.Heartbeat(WorkerInfo{Name: "w1", Caps: []string{"bash"}, Kinds: []string{"stage"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Heartbeat(WorkerInfo{Name: "w2", Caps: []string{"browser"}}); err != nil {
		t.Fatal(err)
	}
	alive, err := r.Alive()
	if err != nil {
		t.Fatal(err)
	}
	if len(alive) != 2 {
		t.Fatalf("应有 2 个存活 worker, got %d", len(alive))
	}
	// 续约幂等: 再次心跳不新增
	_ = r.Heartbeat(WorkerInfo{Name: "w1", Caps: []string{"bash", "gpu"}})
	alive2, _ := r.Alive()
	if len(alive2) != 2 {
		t.Fatalf("续约不应新增 worker, got %d", len(alive2))
	}
	// caps 更新生效
	for _, w := range alive2 {
		if w.Name == "w1" && len(w.Caps) != 2 {
			t.Errorf("w1 caps 应更新为 2 个: %v", w.Caps)
		}
	}
}

func TestRegistryLeaseExpiry(t *testing.T) {
	r := NewRegistry(statestore.NewMemStore(), 50*time.Millisecond)
	_ = r.Heartbeat(WorkerInfo{Name: "w1"})
	time.Sleep(80 * time.Millisecond)
	alive, _ := r.Alive()
	if len(alive) != 0 {
		t.Fatalf("过期 worker 应被摘除, got %d", len(alive))
	}
}

func TestRegistryFirstSeenPreserved(t *testing.T) {
	r := NewRegistry(statestore.NewMemStore(), time.Minute)
	_ = r.Heartbeat(WorkerInfo{Name: "w1"})
	alive1, _ := r.Alive()
	first := alive1[0].FirstSeen
	time.Sleep(5 * time.Millisecond)
	_ = r.Heartbeat(WorkerInfo{Name: "w1"})
	alive2, _ := r.Alive()
	if alive2[0].FirstSeen != first {
		t.Error("续约不应改变 FirstSeen")
	}
	if alive2[0].LastBeat < first {
		t.Error("LastBeat 应 >= FirstSeen")
	}
}
