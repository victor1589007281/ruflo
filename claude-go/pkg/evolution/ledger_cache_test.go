package evolution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FoldSnapshot 缓存: 同指纹 + TTL 内 → 复用 (不重读盘); 指纹变 → 重算。
func TestFoldSnapshotCache(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "metrics")
	os.MkdirAll(dir, 0o755)
	appendTok := func(module string, value float64) {
		f, _ := os.OpenFile(filepath.Join(dir, module+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		b, _ := json.Marshal(map[string]any{"ts": time.Now().Format(time.RFC3339Nano), "module": module, "name": "llm_total_tokens", "value": value, "labels": map[string]string{"source": "evolution"}})
		f.Write(append(b, '\n'))
		f.Close()
	}
	appendTok("llm", 42)
	now := time.Now()

	a := foldSnapshotUncached(stateDir, now)
	b := FoldSnapshot(stateDir, now)
	if a.TotalLLMTokens1h != 42 || b.TotalLLMTokens1h != 42 {
		t.Fatalf("两次折叠应同值 42, 得 %v / %v", a.TotalLLMTokens1h, b.TotalLLMTokens1h)
	}

	// 缓存命中后再 append (指纹变) → 重折叠, 且重算取新 now → 新行进新窗口。
	appendTok("llm", 8)
	c := FoldSnapshot(stateDir, time.Now())
	if c.TotalLLMTokens1h != 50 {
		t.Fatalf("指纹变化后应重折叠, 新行进新窗口 =50, 得 %v", c.TotalLLMTokens1h)
	}

	// TTL 到期但指纹未变: 也要重算 (now 已换, 窗口语义变)。
	snapshotCache.mu.Lock()
	snapshotCache.expiresAt = time.Now().Add(-time.Second)
	snapshotCache.mu.Unlock()
	d := FoldSnapshot(stateDir, time.Now())
	if d.TotalLLMTokens1h != 50 || d.Now.Before(c.Now) {
		t.Fatalf("TTL 过期后应重算且 Now 前推, 得 %v now=%v", d.TotalLLMTokens1h, d.Now)
	}
}
