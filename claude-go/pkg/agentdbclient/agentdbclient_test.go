package agentdbclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agentdbclient"
	"gitee.com/lorydb/agentDB/pkg/agentstore"
	"gitee.com/lorydb/agentDB/pkg/kv"
	"gitee.com/lorydb/agentDB/pkg/server"
)

// newTestServer 起一个进程内 agentDB serve (仅 Store+Leases backends),
// 返回 client 与可重启的数据目录。
func newTestServer(t *testing.T) (*agentdbclient.Client, func()) {
	t.Helper()
	dir := t.TempDir()
	kvDB, err := kv.Open(dir, kv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := agentstore.Open(kvDB, filepath.Join(dir, "agentstore"))
	if err != nil {
		t.Fatal(err)
	}
	lm, err := agentstore.NewLeaseManager(st)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Backends{Store: st, Leases: lm})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return agentdbclient.New(ts.URL), func() { kvDB.Close() }
}

type demoVal struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestClientKVContract(t *testing.T) {
	c, _ := newTestServer(t)
	kv := c.KV("teams")

	var got demoVal
	if ok, err := kv.Get("a", &got); err != nil || ok {
		t.Fatalf("Get 未写入: ok=%v err=%v, 期望 (false,nil)", ok, err)
	}
	want := demoVal{Name: "alpha", Count: 42}
	if err := kv.Put("a", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok, err := kv.Get("a", &got); err != nil || !ok || got != want {
		t.Fatalf("roundtrip: ok=%v got=%+v err=%v", ok, got, err)
	}
	for _, k := range []string{"z", "m"} {
		if err := kv.Put(k, demoVal{Name: k}); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	keys, err := kv.Keys()
	if err != nil || len(keys) != 3 || keys[0] != "a" || keys[1] != "m" || keys[2] != "z" {
		t.Fatalf("Keys 应有序 [a m z], got=%v err=%v", keys, err)
	}
	if err := kv.Delete("a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := kv.Delete("a"); err != nil {
		t.Fatalf("重复 Delete 幂等: %v", err)
	}
	if ok, _ := kv.Get("a", &got); ok {
		t.Fatalf("Delete 后 Get 应为 false")
	}
}

func TestClientLogContract(t *testing.T) {
	c, _ := newTestServer(t)
	lg := c.Log("journal")
	for i := 0; i < 5; i++ {
		if err := lg.Append(map[string]int{"seq": i}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	var lines [][]byte
	if err := lg.ReadAll(func(line []byte) error {
		lines = append(lines, append([]byte(nil), line...))
		return nil
	}); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(lines) != 5 || string(lines[3]) != `{"seq":3}` {
		t.Fatalf("期望 5 行且第4行={\"seq\":3}, got %d 行 %q", len(lines), lines)
	}
}

func TestClientBlobContract(t *testing.T) {
	c, _ := newTestServer(t)
	blob := c.Blob()
	data := []byte("distributed content payload")
	h1, err := blob.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	h2, err := blob.Put(data)
	if err != nil || h1 != h2 || len(h1) != 64 {
		t.Fatalf("同内容应同 64 位 hash: %s != %s err=%v", h1, h2, err)
	}
	if !blob.Has(h1) {
		t.Fatalf("Has 应为 true")
	}
	if got, err := blob.Get(h1); err != nil || string(got) != string(data) {
		t.Fatalf("Get: err=%v got=%q", err, got)
	}
}

func TestClientCAS(t *testing.T) {
	c, _ := newTestServer(t)
	ctx := context.Background()
	// KV 存的是 demoVal 完整 JSON 字节 {"name":"v1","count":0}
	if err := c.KV("job").Put("x", demoVal{Name: "v1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v1 := mustJSON(t, demoVal{Name: "v1"})
	// 不匹配 → ErrCASConflict
	if err := c.CAS(ctx, "job", "x", []byte(`{"name":"999"}`), mustJSON(t, demoVal{Name: "v2"})); !errors.Is(err, agentdbclient.ErrCASConflict) {
		t.Fatalf("CAS 不匹配应 ErrCASConflict, got %v", err)
	}
	// 匹配 (字节级旧值) → 成功
	if err := c.CAS(ctx, "job", "x", v1, mustJSON(t, demoVal{Name: "v2"})); err != nil {
		t.Fatalf("CAS 匹配应成功: %v", err)
	}
	var got demoVal
	if ok, _ := c.KV("job").Get("x", &got); !ok || got.Name != "v2" {
		t.Fatalf("CAS 后值=%+v", got)
	}
	// old=nil 前置键不存在
	if err := c.CAS(ctx, "job", "fresh", nil, mustJSON(t, demoVal{Name: "v"})); err != nil {
		t.Fatalf("CAS(nil) 新 key 应成功: %v", err)
	}
	if err := c.CAS(ctx, "job", "fresh", nil, mustJSON(t, demoVal{Name: "x"})); !errors.Is(err, agentdbclient.ErrCASConflict) {
		t.Fatalf("CAS(nil) 已存在应冲突, got %v", err)
	}
}

func TestClientTx(t *testing.T) {
	c, _ := newTestServer(t)
	ctx := context.Background()
	// 跨 bucket 原子批写 (值是 JSON 字节, 之后 Get 反序列化进 demoVal)
	if err := c.Tx(ctx, []agentdbclient.TxOp{
		{Bucket: "a", Key: "k1", Value: mustJSON(t, demoVal{Name: "a1"})},
		{Bucket: "b", Key: "k2", Value: mustJSON(t, demoVal{Name: "b2"})},
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	var v demoVal
	if ok, _ := c.KV("a").Get("k1", &v); !ok || v.Name != "a1" {
		t.Fatalf("a/k1 应存在且可反序列化: ok=%v v=%+v", ok, v)
	}
	if ok, _ := c.KV("b").Get("k2", &v); !ok || v.Name != "b2" {
		t.Fatalf("b/k2 应存在且可反序列化: ok=%v v=%+v", ok, v)
	}
}

// TestTaskClaimRace M3 验收: 双 worker 经 HTTP 同抢一任务, CAS 只放行一个。
func TestTaskClaimRace(t *testing.T) {
	c, _ := newTestServer(t)
	ctx := context.Background()
	// 初始任务: 状态 pending
	if err := c.KV("dist-tasks").Put("task-1", map[string]string{"state": "pending", "owner": ""}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	claim := func(worker string) error {
		// 读-改-写 CAS: pending → claimed by worker
		for {
			var cur map[string]string
			ok, err := c.KV("dist-tasks").Get("task-1", &cur)
			if err != nil {
				return err
			}
			if !ok || cur["state"] != "pending" {
				return errors.New("task already claimed")
			}
			next := map[string]string{"state": "claimed", "owner": worker}
			err = c.CAS(ctx, "dist-tasks", "task-1",
				mustJSON(t, cur), mustJSON(t, next))
			if errors.Is(err, agentdbclient.ErrCASConflict) {
				continue // 冲突重试
			}
			return err
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, w := range []string{"worker-1", "worker-2"} {
		wg.Add(1)
		go func(w string) {
			defer wg.Done()
			results <- claim(w)
		}(w)
	}
	wg.Wait()
	close(results)

	var winners int
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("任务认领应有且仅有 1 个 winner, got %d", winners)
	}
	var final map[string]string
	if ok, _ := c.KV("dist-tasks").Get("task-1", &final); !ok || final["state"] != "claimed" {
		t.Fatalf("任务终态=%+v", final)
	}
}

// TestLeaseDistributed M3 验收: 租约跨进程 (acquire 冲突 / 续租 / 过期重派)。
func TestLeaseDistributed(t *testing.T) {
	c, _ := newTestServer(t)
	ctx := context.Background()

	l, err := c.Acquire(ctx, "worker-1", "node-a", time.Minute)
	if err != nil || l.Holder != "node-a" {
		t.Fatalf("Acquire: l=%+v err=%v", l, err)
	}
	if _, err := c.Acquire(ctx, "worker-1", "node-b", time.Minute); err == nil {
		t.Fatalf("node-b 抢占应被拒")
	}
	if _, err := c.Renew(ctx, "worker-1", "node-a", time.Minute); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if err := c.Release(ctx, "worker-1", "node-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if l, err := c.Acquire(ctx, "worker-1", "node-b", time.Minute); err != nil || l.Holder != "node-b" {
		t.Fatalf("释放后 node-b 应可抢占: l=%+v err=%v", l, err)
	}

	// 过期重派: 短 TTL 租约过期后他人接管 (worker 崩溃 → 任务重派)
	if _, err := c.Acquire(ctx, "ephemeral", "dead-worker", 150*time.Millisecond); err != nil {
		t.Fatalf("Acquire ephemeral: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	l2, err := c.Acquire(ctx, "ephemeral", "live-worker", time.Minute)
	if err != nil || l2.Holder != "live-worker" {
		t.Fatalf("过期后接管: l=%+v err=%v", l2, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
