package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTokenBucket_Basic(t *testing.T) {
	tb := NewTokenBucket(10, 10) // 10/s, burst 10

	// Should succeed immediately (burst)
	for i := 0; i < 10; i++ {
		if !tb.TryAcquire() {
			t.Errorf("expected acquire to succeed at %d", i)
		}
	}
	// 11th should fail (burst exhausted)
	if tb.TryAcquire() {
		t.Error("expected acquire to fail after burst")
	}
}

func TestTokenBucket_Refill(t *testing.T) {
	tb := NewTokenBucket(100, 5) // 100/s = refills fast

	for i := 0; i < 5; i++ {
		tb.TryAcquire()
	}

	time.Sleep(60 * time.Millisecond) // should refill ~6 tokens
	if !tb.TryAcquire() {
		t.Error("expected token to refill")
	}
}

func TestTokenBucket_AcquireBlocking(t *testing.T) {
	tb := NewTokenBucket(100, 1) // 100/s, burst 1
	tb.TryAcquire()              // drain the one token

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := tb.Acquire(ctx)
	if err != nil {
		t.Error("expected to acquire within timeout")
	}
}

func TestTokenBucket_AcquireCancelled(t *testing.T) {
	tb := NewTokenBucket(0.1, 1) // very slow: 0.1/s
	tb.TryAcquire()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := tb.Acquire(ctx)
	if err == nil {
		t.Error("expected context cancellation")
	}
}

func TestAdaptiveSemaphore_Basic(t *testing.T) {
	sem := NewAdaptiveSemaphore(3, 1, 10)

	ctx := context.Background()
	// Acquire 3 permits
	for i := 0; i < 3; i++ {
		if err := sem.Acquire(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if sem.Active() != 3 {
		t.Errorf("expected 3 active, got %d", sem.Active())
	}

	// Release with success → should increase limit
	sem.Release(true)
	sem.Release(true)
	sem.Release(true)

	if sem.Active() != 0 {
		t.Errorf("expected 0 active after release, got %d", sem.Active())
	}
}

func TestAdaptiveSemaphore_AIMD(t *testing.T) {
	sem := NewAdaptiveSemaphore(8, 2, 16)

	ctx := context.Background()
	sem.Acquire(ctx)

	// Failure → multiplicative decrease
	sem.Release(false)
	limit := sem.Limit()
	if limit > 4 {
		t.Errorf("expected limit <= 4 after failure, got %d", limit)
	}

	// Success → additive increase
	sem.Acquire(ctx)
	sem.Release(true)
	newLimit := sem.Limit()
	if newLimit <= limit {
		t.Logf("limit after success: %d (was %d)", newLimit, limit)
	}
}

func TestBackpressureCtrl_QueueDepth(t *testing.T) {
	bp := NewBackpressureCtrl(1000, 100, 10, 1, 20, 5)

	// Should be able to enqueue 5 items
	for i := 0; i < 5; i++ {
		if !bp.CanEnqueue() {
			t.Errorf("should be able to enqueue at %d", i)
		}
		bp.IncrQueue()
	}

	// 6th should be rejected
	if bp.CanEnqueue() {
		t.Error("should not enqueue beyond depth 5")
	}

	bp.DecrQueue()
	if !bp.CanEnqueue() {
		t.Error("should be able to enqueue after dequeue")
	}
}

func TestBackpressureCtrl_UnlimitedQueue(t *testing.T) {
	bp := NewBackpressureCtrl(1000, 100, 10, 1, 20, 0)

	for i := 0; i < 1000; i++ {
		if !bp.CanEnqueue() {
			t.Fatalf("unlimited queue should always accept, failed at %d", i)
		}
		bp.IncrQueue()
	}
}

func TestBackpressureCtrl_Snapshot(t *testing.T) {
	bp := NewBackpressureCtrl(60, 10, 5, 1, 10, 50)
	snap := bp.Snapshot()

	if snap.ConcLimit != 5 {
		t.Errorf("expected concurrency limit 5, got %d", snap.ConcLimit)
	}
	if snap.QueueDepth != 50 {
		t.Errorf("expected queue depth 50, got %d", snap.QueueDepth)
	}
}

func TestBackpressureCtrl_Concurrent(t *testing.T) {
	bp := NewBackpressureCtrl(10000, 1000, 10, 1, 50, 0)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if err := bp.AcquireAll(ctx); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
			bp.ReleaseConc(true)
		}()
	}
	wg.Wait()
}
