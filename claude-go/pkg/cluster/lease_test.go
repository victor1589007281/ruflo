package cluster

import (
	"sync"
	"testing"
	"time"
)

// TestFileLeaseExclusive 同一 key 并发 TryAcquire 只有一个成功 (cron 选主语义)。
func TestFileLeaseExclusive(t *testing.T) {
	l := NewFileLease(t.TempDir(), time.Minute)
	const n = 20
	var won int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.TryAcquire("cron/job1/202607241200") {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("同一 key 应只有 1 个副本抢到, got %d", won)
	}
	// 不同 key 各自可抢
	if !l.TryAcquire("cron/job1/202607241201") {
		t.Fatal("下一分钟的 key 应可抢")
	}
}

// TestFileLeaseTTLReclaim 过期锁被回收后可重抢 (副本崩溃自愈)。
func TestFileLeaseTTLReclaim(t *testing.T) {
	l := NewFileLease(t.TempDir(), 40*time.Millisecond)
	if !l.TryAcquire("k") {
		t.Fatal("首次应抢到")
	}
	if l.TryAcquire("k") {
		t.Fatal("未过期不应重抢")
	}
	time.Sleep(70 * time.Millisecond)
	if !l.TryAcquire("k") {
		t.Fatal("过期后应可重抢")
	}
}

// TestNilLeaseAcquires nil 租约恒抢到 (单副本退化)。
func TestNilLeaseAcquires(t *testing.T) {
	var l *FileLease
	if !l.TryAcquire("k") {
		t.Fatal("nil 租约应恒返回 true")
	}
}
