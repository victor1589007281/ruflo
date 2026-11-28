package search

// telemetry_test.go —— 使用记录与源覆盖的离线验证。
//
// 这些用例只断言"面板要读的事实确实被写下来了": 逐条记录能落成 JSONL、旧文件
// 会被清理、Search/Fetch 的现场字段齐全、生效快照不泄露密钥、覆盖文件真的
// 改到配置。它们不碰网络。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installTestRecorder 把进程级记录器换成临时目录, 测试结束恢复。
func installTestRecorder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	r, err := NewRecorder(dir)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	SetRecorder(r)
	t.Cleanup(func() { SetRecorder(nil) })
	return dir
}

// readAllRecords 读出目录下所有记录 (按文件名排序)。
func readAllRecords(t *testing.T, dir string) []Record {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "telemetry"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "telemetry", e.Name()))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var rec Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("坏记录行 %q: %v", line, err)
			}
			out = append(out, rec)
		}
	}
	return out
}

func TestRecorderAppendsDailyJSONL(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRecorder(dir)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	defer r.Close()

	r.Record(Record{Kind: "search", Query: "第一个 问题", Status: "success", LatencyMS: 12, Results: 3})
	r.Record(Record{Kind: "fetch", URL: "https://example.com/a", Status: "success", Bytes: 2048})

	name := "search-" + time.Now().Local().Format("2006-01-02") + ".jsonl"
	if _, err := os.Stat(filepath.Join(dir, "telemetry", name)); err != nil {
		t.Fatalf("当日文件不存在 %s: %v", name, err)
	}
	recs := readAllRecords(t, dir)
	if len(recs) != 2 {
		t.Fatalf("记录条数 = %d, 期望 2", len(recs))
	}
	if recs[0].Query != "第一个 问题" || recs[0].Results != 3 {
		t.Fatalf("第一条记录字段丢失: %+v", recs[0])
	}
	if recs[1].Kind != "fetch" || recs[1].Bytes != 2048 {
		t.Fatalf("第二条记录字段丢失: %+v", recs[1])
	}
	if recs[0].Time.IsZero() {
		t.Fatalf("时间戳未自动填充")
	}
}

func TestRecorderPrunesOldDays(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRecorder(dir)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	defer r.Close()

	old := filepath.Join(dir, "telemetry", "search-2001-01-01.jsonl")
	if err := os.WriteFile(old, []byte(`{"kind":"search"}`+"\n"), 0o644); err != nil {
		t.Fatalf("预置旧文件: %v", err)
	}
	recent := time.Now().AddDate(0, 0, -3).Local().Format("2006-01-02")
	keep := filepath.Join(dir, "telemetry", "search-"+recent+".jsonl")
	if err := os.WriteFile(keep, []byte(`{"kind":"search"}`+"\n"), 0o644); err != nil {
		t.Fatalf("预置近期文件: %v", err)
	}
	// 非记录文件 (面板的 sources.json) 不能被清理。
	other := filepath.Join(dir, "telemetry", "notes.json")
	if err := os.WriteFile(other, []byte("{}"), 0o644); err != nil {
		t.Fatalf("预置其它文件: %v", err)
	}

	r.Record(Record{Kind: "search"})

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("30 天前的记录文件未被清理")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("3 天前的记录被误删: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("非记录文件被误删: %v", err)
	}
}

func TestSearchRecordsUsage(t *testing.T) {
	dir := installTestRecorder(t)
	p := newFake("alpha", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return []Result{
			{Title: "A", URL: "https://a.example.com/1", Snippet: "这是一段足够长的描述, 用来通过质量门的片段长度检查。"},
			{Title: "B", URL: "https://b.example.com/2", Snippet: "另一段足够长的描述, 用来构成域名多样性。"},
			{Title: "C", URL: "https://c.example.com/3", Snippet: "第三段足够长的描述, 保持结果数量。"},
		}, nil
	})
	m := NewManager(DefaultConfig(), nil, []Provider{p})
	defer m.Close()

	if _, err := m.Search(context.Background(), SearchRequest{Query: "如何理解检索质量"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// 第二次应命中缓存, 记录里必须能看出 cacheHit。
	if _, err := m.Search(context.Background(), SearchRequest{Query: "如何理解检索质量"}); err != nil {
		t.Fatalf("Search 2: %v", err)
	}

	recs := readAllRecords(t, dir)
	if len(recs) != 2 {
		t.Fatalf("记录条数 = %d, 期望 2 (每次 Search 各一条)", len(recs))
	}
	first := recs[0]
	if first.Kind != "search" || first.Query != "如何理解检索质量" {
		t.Fatalf("记录字段错误: %+v", first)
	}
	if first.Status != "success" {
		t.Fatalf("status = %q, 期望 success", first.Status)
	}
	if len(first.Providers) != 1 || first.Providers[0] != "alpha" {
		t.Fatalf("providers = %v, 期望 [alpha]", first.Providers)
	}
	if first.Domains != 3 || len(first.TopDomains) == 0 {
		t.Fatalf("域名统计缺失: domains=%d top=%v", first.Domains, first.TopDomains)
	}
	if len(first.ProviderMS) != 1 {
		t.Fatalf("逐源耗时缺失: %v", first.ProviderMS)
	}
	if first.GateReason == "" {
		t.Fatalf("质量门原因缺失")
	}
	if recs[1].CacheHit != true {
		t.Fatalf("第二次检索未记录 cacheHit: %+v", recs[1])
	}
}

func TestSearchRecordsZeroResult(t *testing.T) {
	dir := installTestRecorder(t)
	p := newFake("empty", TierL0, func(context.Context, SearchRequest) ([]Result, error) {
		return nil, nil
	})
	m := NewManager(DefaultConfig(), nil, []Provider{p})
	defer m.Close()

	resp, err := m.Search(context.Background(), SearchRequest{Query: "没有任何结果的查询"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Status != "empty" {
		t.Fatalf("响应当前状态 = %q, 期望 empty", resp.Status)
	}
	recs := readAllRecords(t, dir)
	if len(recs) != 1 || recs[0].Status != "empty" || recs[0].Results != 0 {
		t.Fatalf("零结果记录错误: %+v", recs)
	}
}

func TestFetchRecordsUsage(t *testing.T) {
	dir := installTestRecorder(t)
	allowAnyFetchURL(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := "<html><head><title>测试文章</title></head><body><article><h1>测试文章</h1>"
		body += "<p>" + strings.Repeat("这是一段足够长的正文, 用来验证抓取记录。", 40) + "</p>"
		body += "</article></body></html>"
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	resp, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/article"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	recs := readAllRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("记录条数 = %d, 期望 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != "fetch" || rec.URL != srv.URL+"/article" {
		t.Fatalf("抓取记录字段错误: %+v", rec)
	}
	if rec.Status != "success" || rec.Bytes != resp.Document.TotalChars || rec.Bytes == 0 {
		t.Fatalf("抓取记录状态/字节错误: %+v (doc=%d)", rec, resp.Document.TotalChars)
	}
}

func TestFetchRecordsError(t *testing.T) {
	dir := installTestRecorder(t)
	allowAnyFetchURL(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	m := NewManager(DefaultConfig(), testEgress(t), nil)
	defer m.Close()

	if _, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/slow"}); err == nil {
		t.Fatalf("期望抓取失败")
	}
	recs := readAllRecords(t, dir)
	if len(recs) != 1 || recs[0].Status != "error" || recs[0].Error == "" {
		t.Fatalf("失败抓取未正确记录: %+v", recs)
	}
}

func TestWriteEffectiveSourcesOmitsKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEARCH_STATE_DIR", dir)

	cfg := DefaultConfig()
	cfg.Providers["exa"] = ProviderConfig{Enabled: true, Tier: TierL1, Weight: 1.2, APIKey: "super-secret-key", Egress: ModeDirect}
	cfg.Providers["off"] = ProviderConfig{Enabled: false, Tier: TierL2, Weight: 0.4, Egress: ModeProxy}
	p := newFake("exa", TierL1, nil)

	WriteEffectiveSources(cfg, []Provider{p})

	path := filepath.Join(dir, "providers.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("providers.json 未写出: %v", err)
	}
	if strings.Contains(string(raw), "super-secret-key") {
		t.Fatalf("生效快照泄露了 API key")
	}
	var snap EffectiveSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("providers.json 不合法: %v", err)
	}
	if snap.MaxTier == "" || len(snap.Sources) != 2 {
		t.Fatalf("快照字段不全: %+v", snap)
	}
	byName := map[string]EffectiveSource{}
	for _, s := range snap.Sources {
		byName[s.Name] = s
	}
	if !byName["exa"].Enabled || !byName["exa"].APIKeyConfigured || byName["exa"].Weight != 1.2 {
		t.Fatalf("exa 快照错误: %+v", byName["exa"])
	}
	// 配置里启用但在本进程没有构造出来的源必须显式 enabled=false。
	if byName["off"].Enabled {
		t.Fatalf("未构造的源不应显示为启用: %+v", byName["off"])
	}
}

func TestApplySourcesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEARCH_STATE_DIR", dir)

	sf := SourcesFile{
		UpdatedBy: "panel",
		Providers: map[string]SourceOverride{
			"tavily": {Enabled: boolPtr(false)},
			"exa":    {Weight: floatPtr(0.25), Tier: strPtr("L3")},
		},
		ProviderPriority: []string{"exa", "tavily"},
	}
	data, _ := json.Marshal(sf)
	if err := os.WriteFile(filepath.Join(dir, "sources.json"), data, 0o644); err != nil {
		t.Fatalf("写覆盖文件: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Providers["tavily"] = ProviderConfig{Enabled: true, Weight: 1.0, Tier: TierL1}
	cfg.Providers["exa"] = ProviderConfig{Enabled: true, Weight: 1.0, Tier: TierL1, APIKey: "keep-me"}
	applySourcesFile(&cfg)

	if cfg.Providers["tavily"].Enabled {
		t.Fatalf("tavily 未被停用")
	}
	if !cfg.Providers["tavily"].Disabled {
		t.Fatalf("tavily 的 Disabled 未同步")
	}
	exa := cfg.Providers["exa"]
	if exa.Weight != 0.25 || exa.Tier != TierL3 || !exa.Enabled {
		t.Fatalf("exa 覆盖未生效: %+v", exa)
	}
	if exa.APIKey != "keep-me" {
		t.Fatalf("覆盖文件不应改动密钥")
	}
	if len(cfg.ProviderPriority) != 2 || cfg.ProviderPriority[0] != "exa" {
		t.Fatalf("优先级覆盖未生效: %v", cfg.ProviderPriority)
	}
}

func TestApplySourcesFileIgnoresBrokenJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEARCH_STATE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "sources.json"), []byte(`{broken`), 0o644); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}
	cfg := DefaultConfig()
	cfg.Providers["exa"] = ProviderConfig{Enabled: true, Weight: 1.0, Tier: TierL1}
	applySourcesFile(&cfg)
	if !cfg.Providers["exa"].Enabled || cfg.Providers["exa"].Weight != 1.0 {
		t.Fatalf("坏覆盖文件不应改动配置: %+v", cfg.Providers["exa"])
	}
}

func TestStateDirEnvOverride(t *testing.T) {
	t.Setenv("SEARCH_STATE_DIR", "/tmp/custom-search-state")
	if got := StateDir(); got != "/tmp/custom-search-state" {
		t.Fatalf("StateDir = %q", got)
	}
	if got := SourcesPath(); got != "/tmp/custom-search-state/sources.json" {
		t.Fatalf("SourcesPath = %q", got)
	}
	if got := EffectiveSourcesPath(); got != "/tmp/custom-search-state/providers.json" {
		t.Fatalf("EffectiveSourcesPath = %q", got)
	}
}

func boolPtr(v bool) *bool        { return &v }
func floatPtr(v float64) *float64 { return &v }
func strPtr(v string) *string     { return &v }

// 测试二进制默认不许碰真实状态目录: 否则一次 go test 就会把假查询写进
// 用户面板里, 看起来像是"今天的真实使用记录"。
func TestTelemetryOffForGoTestBinary(t *testing.T) {
	t.Setenv("SEARCH_TELEMETRY_DISABLED", "")
	t.Setenv("SEARCH_STATE_DIR", "")
	if !telemetryOff() {
		t.Fatalf("go test 二进制 (os.Args[0]=%q) 未指定状态目录时应当跳过落盘", os.Args[0])
	}
	// 显式指定目录的测试照常落盘
	t.Setenv("SEARCH_STATE_DIR", t.TempDir())
	if telemetryOff() {
		t.Fatal("显式给了 SEARCH_STATE_DIR 就不该跳过")
	}
	// 显式关闭优先
	t.Setenv("SEARCH_TELEMETRY_DISABLED", "true")
	if !telemetryOff() {
		t.Fatal("SEARCH_TELEMETRY_DISABLED=true 应当关闭落盘")
	}
}

func TestTelemetryDisabledSkipsEffectiveSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEARCH_STATE_DIR", dir)
	t.Setenv("SEARCH_TELEMETRY_DISABLED", "1")

	WriteEffectiveSources(DefaultConfig(), nil)

	if _, err := os.Stat(filepath.Join(dir, "providers.json")); !os.IsNotExist(err) {
		t.Fatalf("关闭使用记录后不应写出生效快照, stat err=%v", err)
	}
}
