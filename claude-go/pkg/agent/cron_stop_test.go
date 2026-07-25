package agent

import "testing"

// Stop 必须幂等 —— 裸 close(stopCh) 二次调用会 panic, 而"先 Stop 再 Shutdown"
// 是独立 worker 进程的真实装配序列。
func TestCronScheduler_Stop幂等(t *testing.T) {
	cs := NewCronScheduler(t.TempDir(), nil)
	cs.Start()
	for i := 0; i < 3; i++ {
		cs.Stop() // 不许 panic
	}
}
