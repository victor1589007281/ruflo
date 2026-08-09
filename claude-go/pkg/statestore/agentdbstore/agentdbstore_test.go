// Package agentdbstore_test runs the statestore 接口契约 against the embedded
// AgentDB backend (规划 14.1 M1 验收: 层 1 BucketKV 适配, statestore 语义全绿)。
//
// 语义与父包 pkg/statestore/statestore_test.go 的 FileStore/MemStore 契约一致
// (Get 未命中 → (false,nil) / Keys 有序 / 非法 bucket 报错 / Log 逐行 JSON /
// torn-tail 容忍 / 中部损坏报错 / Blob sha256 去重), 另覆盖 agentdb 特有的
// 崩溃安全路径 (<dir>/agentstore/log/*.jsonl) 与 CAS / 租约原语。
package agentdbstore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore/agentdbstore"
)

func openStore(t *testing.T) *agentdbstore.Store {
	t.Helper()
	s, err := agentdbstore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open agentdbstore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type demoVal struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// TestContractKVRoundtrip 对齐父包 TestKVRoundtrip。
func TestContractKVRoundtrip(t *testing.T) {
	s := openStore(t)
	kv := s.KV("teams")

	var got demoVal
	if ok, err := kv.Get("a", &got); err != nil || ok {
		t.Fatalf("Get 未写入 key: ok=%v err=%v, 期望 (false,nil)", ok, err)
	}
	want := demoVal{Name: "alpha", Count: 42}
	if err := kv.Put("a", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok, err := kv.Get("a", &got); err != nil || !ok || got != want {
		t.Fatalf("roundtrip: ok=%v got=%+v want=%+v err=%v", ok, got, want, err)
	}
	want2 := demoVal{Name: "beta", Count: 7}
	if err := kv.Put("a", want2); err != nil {
		t.Fatalf("覆盖 Put: %v", err)
	}
	if _, err := kv.Get("a", &got); err != nil || got != want2 {
		t.Fatalf("覆盖后 Get: got=%+v err=%v", got, err)
	}
}

// TestContractKeysAndDelete 对齐父包 TestKVDeleteAndKeys。
func TestContractKeysAndDelete(t *testing.T) {
	s := openStore(t)
	kv := s.KV("jobs")
	for _, k := range []string{"z", "a", "m"} {
		if err := kv.Put(k, demoVal{Name: k}); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	keys, err := kv.Keys()
	if err != nil || len(keys) != 3 || keys[0] != "a" || keys[1] != "m" || keys[2] != "z" {
		t.Fatalf("Keys 应有序 [a m z], got=%v err=%v", keys, err)
	}
	if err := kv.Delete("m"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var got demoVal
	if ok, err := kv.Get("m", &got); err != nil || ok {
		t.Fatalf("Delete 后 Get: ok=%v err=%v", ok, err)
	}
	if err := kv.Delete("m"); err != nil {
		t.Fatalf("重复 Delete 应幂等: %v", err)
	}
	keys, _ = kv.Keys()
	if len(keys) != 2 {
		t.Fatalf("Delete 后剩 %d key, want 2", len(keys))
	}
}

// TestContractConcurrentKV 对齐父包 TestKVConcurrent (8 goroutine × 20 轮)。
func TestContractConcurrentKV(t *testing.T) {
	s := openStore(t)
	kv := s.KV("concurrent")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", n)
			for j := 0; j < 20; j++ {
				if err := kv.Put(key, demoVal{Name: key, Count: j}); err != nil {
					t.Errorf("并发 Put: %v", err)
					return
				}
				var got demoVal
				if _, err := kv.Get(key, &got); err != nil {
					t.Errorf("并发 Get: %v", err)
					return
				}
				if _, err := kv.Keys(); err != nil {
					t.Errorf("并发 Keys: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	keys, err := kv.Keys()
	if err != nil || len(keys) != 8 {
		t.Fatalf("并发后应 8 key, got=%v err=%v", keys, err)
	}
}

// TestContractBadBucket 对齐父包 TestBadBucketName。
func TestContractBadBucket(t *testing.T) {
	s := openStore(t)
	for _, bad := range []string{"", "a/b", "../x", "a b", "中文", "a\x00b"} {
		kv := s.KV(bad)
		if err := kv.Put("k", 1); err == nil {
			t.Errorf("KV bucket %q 应报错", bad)
		}
		if _, err := kv.Get("k", new(int)); err == nil {
			t.Errorf("KV Get bucket %q 应报错", bad)
		}
		if _, err := kv.Keys(); err == nil {
			t.Errorf("KV Keys bucket %q 应报错", bad)
		}
		if err := kv.Delete("k"); err == nil {
			t.Errorf("KV Delete bucket %q 应报错", bad)
		}
		lg := s.Log(bad)
		if err := lg.Append(1); err == nil {
			t.Errorf("Log bucket %q 应报错", bad)
		}
		if err := lg.ReadAll(func([]byte) error { return nil }); err == nil {
			t.Errorf("Log ReadAll bucket %q 应报错", bad)
		}
	}
	if err := s.KV("a.b_c-d").Put("k", 1); err != nil {
		t.Errorf("合法 bucket 名不应报错: %v", err)
	}
}

// TestContractLogAppendReadAll 对齐父包 TestLogAppendReadAll。
func TestContractLogAppendReadAll(t *testing.T) {
	s := openStore(t)
	lg := s.Log("journal")
	for i := 0; i < 5; i++ {
		if err := lg.Append(map[string]int{"seq": i}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	var lines [][]byte
	if err := lg.ReadAll(func(line []byte) error {
		cp := make([]byte, len(line))
		copy(cp, line)
		lines = append(lines, cp)
		return nil
	}); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(lines) != 5 || string(lines[3]) != `{"seq":3}` {
		t.Fatalf("期望 5 行且第4行={\"seq\":3}, got %d 行 %q", len(lines), lines)
	}
	calls := 0
	if err := lg.ReadAll(func([]byte) error { calls++; return fmt.Errorf("stop") }); err == nil || calls != 1 {
		t.Fatalf("回调错误应中止透传: err=%v calls=%d", err, calls)
	}
}

// TestContractLogCrashSafety agentdb 布局: <dir>/agentstore/log/<bucket>.jsonl。
// 尾部半行容忍 + 中部坏行报错, 语义与父包 TestLogCrashSafety 对齐。
func TestContractLogCrashSafety(t *testing.T) {
	dir := t.TempDir()
	s, err := agentdbstore.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	lg := s.Log("crash")
	for i := 0; i < 3; i++ {
		if err := lg.Append(map[string]int{"seq": i}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// 手工写半行模拟断电
	path := filepath.Join(dir, "agentstore", "log", "crash.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	if _, err := f.WriteString(`{"seq":3,"tru`); err != nil {
		t.Fatalf("写半行: %v", err)
	}
	f.Close()

	count := 0
	if err := lg.ReadAll(func([]byte) error { count++; return nil }); err != nil {
		t.Fatalf("尾部截断应容忍, got %v", err)
	}
	if count != 3 {
		t.Fatalf("应读到 3 条, got %d", count)
	}
	// 半行后继续 Append → 合并成尾部坏行, 仍容忍
	if err := lg.Append(map[string]int{"seq": 4}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	count = 0
	if err := lg.ReadAll(func([]byte) error { count++; return nil }); err != nil {
		t.Fatalf("合并尾部坏行应容忍, got %v", err)
	}
	if count != 3 {
		t.Fatalf("应仍 3 条, got %d", count)
	}
	// 中部完整坏行 → 数据损坏报错
	lg2 := s.Log("corrupt")
	if err := lg2.Append(map[string]int{"seq": 0}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	f2, _ := os.OpenFile(filepath.Join(dir, "agentstore", "log", "corrupt.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	f2.WriteString("{\"bad\n")
	f2.Close()
	if err := lg2.Append(map[string]int{"seq": 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := lg2.ReadAll(func([]byte) error { return nil }); err == nil {
		t.Fatalf("中部坏行应报错")
	}
}

// TestContractBlobDedupe 对齐父包 TestBlobDedupeAndGet。
func TestContractBlobDedupe(t *testing.T) {
	s := openStore(t)
	blob := s.Blob()
	data := []byte("hello content-addressed world")
	h1, err := blob.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	h2, err := blob.Put(data)
	if err != nil || h1 != h2 || len(h1) != 64 {
		t.Fatalf("同内容应同 64 位 hash: h1=%s h2=%s err=%v", h1, h2, err)
	}
	if !blob.Has(h1) {
		t.Fatalf("Has 应为 true")
	}
	if got, err := blob.Get(h1); err != nil || string(got) != string(data) {
		t.Fatalf("Get: err=%v got=%q", err, got)
	}
	missing := "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := blob.Get(missing); err == nil {
		t.Fatalf("Get 不存在应报错")
	}
	if blob.Has(missing) {
		t.Fatalf("Has 不存在应为 false")
	}
}

// TestCASAgentDB CAS 原语 (规划 14.1.4.4 层1 + 14.1.5 分布式横切)。
func TestCASAgentDB(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	b := s.KV("job")

	if err := b.Put("x", demoVal{Name: "v1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// 旧值不匹配 → 冲突
	err := s.CAS(ctx, "job", "x", demoVal{Name: "999"}, demoVal{Name: "v2"})
	if err == nil {
		t.Fatalf("CAS 不匹配应报错")
	}
	// 旧值匹配 → 成功
	if err := s.CAS(ctx, "job", "x", demoVal{Name: "v1"}, demoVal{Name: "v2"}); err != nil {
		t.Fatalf("CAS 匹配应成功: %v", err)
	}
	var got demoVal
	if ok, _ := b.Get("x", &got); !ok || got.Name != "v2" {
		t.Fatalf("CAS 后值=%+v", got)
	}
	// old=nil 前置键不存在 → 新 key 成功; 已存在则冲突
	if err := s.CAS(ctx, "job", "fresh", nil, demoVal{Name: "v"}); err != nil {
		t.Fatalf("CAS(nil) 新 key 应成功: %v", err)
	}
	if err := s.CAS(ctx, "job", "fresh", nil, demoVal{Name: "x"}); err == nil {
		t.Fatalf("CAS(nil) 已存在应冲突")
	}
}

// TestLeaseAgentDB 租约原语 (规划 14.1.5: worker 心跳 / 任务认领)。
func TestLeaseAgentDB(t *testing.T) {
	s := openStore(t)
	lm, err := s.Leases()
	if err != nil {
		t.Fatalf("Leases: %v", err)
	}
	ctx := context.Background()

	l, err := lm.Acquire(ctx, "worker-1", "node-a", time.Minute)
	if err != nil || l.Holder != "node-a" {
		t.Fatalf("Acquire: l=%+v err=%v", l, err)
	}
	if _, err := lm.Acquire(ctx, "worker-1", "node-b", time.Minute); err == nil {
		t.Fatalf("其他 holder 抢占应被拒")
	}
	// TTL 过期后他人可抢占
	lm2, _ := s.Leases()
	if _, err := lm2.Acquire(ctx, "ephemeral", "a", 150*time.Millisecond); err != nil {
		t.Fatalf("Acquire ephemeral: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	l2, err := lm2.Acquire(ctx, "ephemeral", "b", time.Minute)
	if err != nil || l2.Holder != "b" {
		t.Fatalf("过期后抢占: l=%+v err=%v", l2, err)
	}
}

// TestPersistenceAcrossReopen 重新打开同一目录, 数据仍在 (持久化语义)。
func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := agentdbstore.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.KV("teams").Put("a", demoVal{Name: "alpha"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Log("journal").Append(map[string]int{"seq": 0}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := s.Blob().Put([]byte("payload")); err != nil {
		t.Fatalf("Blob Put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := agentdbstore.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var got demoVal
	if ok, err := s2.KV("teams").Get("a", &got); err != nil || !ok || got.Name != "alpha" {
		t.Fatalf("reopen KV: ok=%v got=%+v err=%v", ok, got, err)
	}
	n := 0
	if err := s2.Log("journal").ReadAll(func([]byte) error { n++; return nil }); err != nil {
		t.Fatalf("reopen Log: %v", err)
	}
	if n != 1 {
		t.Fatalf("reopen Log 应 1 条, got %d", n)
	}
}
