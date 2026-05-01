package observability

import (
	"path/filepath"
	"testing"
)

func TestPromptObservatoryRecordAndLookup(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	obs, err := NewPromptObservatory(dbPath)
	if err != nil {
		t.Fatalf("创建 observatory 失败: %v", err)
	}
	defer obs.Close()

	rec := PromptRecord{
		PromptID:     "pid-1",
		Version:      "v1",
		Hash:         "hash1",
		TemplateName: "test-template",
		Rendered:     "hello world",
		RenderedLen:  11,
		RenderTimeMs: 12.5,
	}
	id, err := obs.RecordPrompt(rec)
	if err != nil {
		t.Fatalf("记录 prompt 失败: %v", err)
	}
	if id <= 0 {
		t.Fatalf("ID 应大于 0")
	}

	// 按 version 查询
	got, err := obs.LookupVersion("pid-1", "v1")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got == nil || got.PromptID != "pid-1" || got.Version != "v1" {
		t.Fatalf("查询结果不匹配")
	}

	// 按 hash 查询
	got2, err := obs.LookupByHash("hash1")
	if err != nil {
		t.Fatalf("按 hash 查询失败: %v", err)
	}
	if got2 == nil || got2.Hash != "hash1" {
		t.Fatalf("按 hash 查询结果不匹配")
	}
}

func TestPromptObservatoryVersionStats(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	obs, err := NewPromptObservatory(dbPath)
	if err != nil {
		t.Fatalf("创建 observatory 失败: %v", err)
	}
	defer obs.Close()

	// 记录同一 prompt 的两个版本
	for i := 0; i < 3; i++ {
		if _, err := obs.RecordPrompt(PromptRecord{PromptID: "p1", Version: "v1", Hash: "h1", RenderTimeMs: 10}); err != nil {
			t.Fatalf("记录 v1 失败: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := obs.RecordPrompt(PromptRecord{PromptID: "p1", Version: "v2", Hash: "h2", RenderTimeMs: 20}); err != nil {
			t.Fatalf("记录 v2 失败: %v", err)
		}
	}

	stats, err := obs.VersionStats("p1")
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if len(stats) != 2 {
		// 调试: 直接查询数据库
		row := obs.db.QueryRow("SELECT COUNT(*) FROM prompt_versions WHERE prompt_id = ?", "p1")
		var count int
		_ = row.Scan(&count)
		t.Fatalf("期望 2 个版本统计, 实际 %d (表中记录数: %d)", len(stats), count)
	}
}

func TestPromptObservatoryOutcomeStats(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	obs, err := NewPromptObservatory(dbPath)
	if err != nil {
		t.Fatalf("创建 observatory 失败: %v", err)
	}
	defer obs.Close()

	// 记录 outcomes
	_ = obs.RecordOutcome("p1", "v1", true, 100, 50, 500)
	_ = obs.RecordOutcome("p1", "v1", false, 80, 40, 600)
	_ = obs.RecordOutcome("p1", "v1", true, 120, 60, 400)

	count, sr, avgIn, avgOut, avgLat, err := obs.OutcomeStats("p1", "v1")
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 3 {
		t.Fatalf("期望 count=3, 实际 %d", count)
	}
	// 2 成功 / 3 总 = 0.666...
	if sr < 0.6 || sr > 0.7 {
		t.Fatalf("success_rate 异常: %f", sr)
	}
	if avgIn <= 0 || avgOut <= 0 || avgLat <= 0 {
		t.Fatalf("平均值应大于 0")
	}
}

func TestPromptObservatoryABTest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	obs, err := NewPromptObservatory(dbPath)
	if err != nil {
		t.Fatalf("创建 observatory 失败: %v", err)
	}
	defer obs.Close()

	err = obs.StartABTest("ab-1", "p1", "variantA", "variantB", map[string]string{"metric": "latency"})
	if err != nil {
		t.Fatalf("启动 A/B 测试失败: %v", err)
	}

	// 记录 A/B 结果
	_ = obs.RecordABOutcome("ab-1", "variantA", true, 100, 50, 500)
	_ = obs.RecordABOutcome("ab-1", "variantA", true, 110, 55, 520)
	_ = obs.RecordABOutcome("ab-1", "variantB", false, 90, 45, 600)

	res, err := obs.ABResult("ab-1")
	if err != nil {
		t.Fatalf("计算 A/B 结果失败: %v", err)
	}
	if res.Winner != "A" {
		t.Fatalf("期望 winner=A (2 成功 vs 0 成功), 实际 %s", res.Winner)
	}
	if res.SampleSize != 3 {
		t.Fatalf("期望 sampleSize=3, 实际 %d", res.SampleSize)
	}

	_ = obs.FinishABTest("ab-1")
}

func TestHashPrompt(t *testing.T) {
	h1 := HashPrompt("hello")
	h2 := HashPrompt("hello")
	h3 := HashPrompt("world")
	if h1 != h2 {
		t.Fatalf("相同 prompt 应产生相同 hash")
	}
	if h1 == h3 {
		t.Fatalf("不同 prompt 应产生不同 hash")
	}
	if len(h1) != 16 {
		t.Fatalf("hash 长度应为 16, 实际 %d", len(h1))
	}
}

func TestPromptHookAdapter(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	obs, _ := NewPromptObservatory(dbPath)
	defer obs.Close()

	adapter := NewPromptHookAdapter(obs)
	adapter.OnPromptRender(PromptEvent{
		PromptID:     "p1",
		Version:      "v1",
		PromptHash:   "h1",
		TemplateName: "tpl",
		RenderedLen:  10,
		RenderTimeMs: 5.0,
	})

	got, err := obs.LookupVersion("p1", "v1")
	if err != nil {
		t.Fatalf("适配器写入失败: %v", err)
	}
	if got == nil || got.PromptID != "p1" {
		t.Fatalf("适配器写入内容不匹配")
	}
}
