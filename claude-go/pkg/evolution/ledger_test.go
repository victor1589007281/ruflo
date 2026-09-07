package evolution

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 每个内置 FoldSpec 的单测锁定语义（doc 13.8.3：输入样例 → 期望输出）。

// metrics 行（ts=RFC3339 字符串）样例构造器。
func mEntry(ts time.Time, module, name string, value float64, labels map[string]string) Entry {
	b, _ := json.Marshal(map[string]any{"ts": ts.Format(time.RFC3339Nano), "module": module, "name": name, "value": value, "labels": labels})
	var row struct {
		TS     json.RawMessage   `json:"ts"`
		Module string            `json:"module"`
		Name   string            `json:"name"`
		Value  float64           `json:"value"`
		Labels map[string]string `json:"labels"`
	}
	_ = json.Unmarshal(b, &row)
	return Entry{TS: parseEntryTS(row.TS), Module: module, Name: name, Value: value, Labels: labels}
}

// rewards 行（ts=unix-milli）样例构造器。
func rEntry(tsMilli int64, source string, value, weight float64) Entry {
	return Entry{TS: tsMilli * int64(time.Millisecond), Source: source, Value: value, Weight: weight}
}

func TestFoldDeterminism(t *testing.T) {
	entries := []Entry{
		mEntry(time.UnixMilli(1000), "llm", "llm_total_tokens", 10, map[string]string{"source": "evolution"}),
		mEntry(time.UnixMilli(2000), "llm", "llm_total_tokens", 20, map[string]string{"source": "chat"}),
		mEntry(time.UnixMilli(3000), "llm", "llm_total_tokens", 5, map[string]string{"source": "evolution"}),
	}
	from := time.UnixMilli(0)
	to := time.UnixMilli(10_000)
	specs := []FoldSpec{FoldLearnLLMTokens(), FoldTotalLLMTokens()}
	a := foldEntries(entries, from, to, specs...)
	b := foldEntries(entries, from, to, specs...)
	if a["learn_llm_tokens"] != 15 || b["learn_llm_tokens"] != 15 {
		t.Errorf("learn_llm_tokens 应确定性=15, 得 %v/%v", a["learn_llm_tokens"], b["learn_llm_tokens"])
	}
	if a["total_llm_tokens"] != 35 {
		t.Errorf("total_llm_tokens 应=35, 得 %v", a["total_llm_tokens"])
	}
}

func TestFoldTimeWindowHalfOpen(t *testing.T) {
	// [from, to): 边界条目 from 入、to 不入 —— 区间相接不重不漏。
	entries := []Entry{
		mEntry(time.UnixMilli(1000), "llm", "llm_total_tokens", 1, map[string]string{"source": "evolution"}),
		mEntry(time.UnixMilli(5000), "llm", "llm_total_tokens", 2, map[string]string{"source": "evolution"}),
		mEntry(time.UnixMilli(9000), "llm", "llm_total_tokens", 4, map[string]string{"source": "evolution"}),
		mEntry(time.UnixMilli(9500), "llm", "llm_total_tokens", 8, map[string]string{"source": "evolution"}),
	}
	got := foldEntries(entries, time.UnixMilli(1000), time.UnixMilli(9000), FoldTotalLLMTokens())
	if got["total_llm_tokens"] != 1+2 {
		t.Errorf("半开区间应含 1000/5000 不含 9000: %v", got["total_llm_tokens"])
	}
}

func TestFoldSpecTokenSourceFilter(t *testing.T) {
	// 非 evolution source 与非 llm_total_tokens 名一律不参与（NaN 排除）。
	entries := []Entry{
		mEntry(time.UnixMilli(1), "llm", "llm_total_tokens", 100, map[string]string{"source": "chat"}),
		mEntry(time.UnixMilli(2), "llm", "llm_input_tokens", 999, map[string]string{"source": "evolution"}),
		mEntry(time.UnixMilli(3), "llm", "llm_total_tokens", 7, map[string]string{"source": "evolution"}),
	}
	got := foldEntries(entries, time.UnixMilli(0), time.UnixMilli(10), FoldLearnLLMTokens())
	if got["learn_llm_tokens"] != 7 {
		t.Errorf("只应计 evolution 源 total: %v", got["learn_llm_tokens"])
	}
}

func TestFoldRewardKS(t *testing.T) {
	// 两窗口分布接近 → D 小; 分布漂移（前半窗中低分、后半窗全满分）→ D 大。
	// 窗口 [0, 100ms) 对半切于 50ms: 事件必须铺满两半窗, 否则单侧样本不足 → NaN。
	entries := []Entry{
		rEntry(10, "gate.compile", 0.5, 1.0), rEntry(20, "gate.test", 0.6, 1.0),
		rEntry(30, "gate.compile", 0.4, 1.0), rEntry(40, "gate.test", 0.7, 1.0),
		rEntry(60, "gate.compile", 1.0, 1.0), rEntry(70, "gate.test", 1.0, 1.0),
		rEntry(80, "gate.compile", 1.0, 1.0), rEntry(90, "gate.test", 1.0, 1.0),
	}
	from := time.UnixMilli(0)
	to := time.UnixMilli(100)
	ks := ksRewardWindow(entries, from, to)
	if math.IsNaN(ks) {
		t.Fatal("样本充分时 KS 不应为 NaN")
	}
	if ks <= 0.3 {
		t.Errorf("漂移样例 KS 应 > 0.3, 得 %v", ks)
	}

	// 稳定样例: 两窗同分布 → D=0
	stable := []Entry{
		rEntry(10, "s", 0.5, 1.0), rEntry(20, "s", 0.7, 1.0),
		rEntry(60, "s", 0.5, 1.0), rEntry(70, "s", 0.7, 1.0),
	}
	if d := ksRewardWindow(stable, time.UnixMilli(0), time.UnixMilli(100)); math.IsNaN(d) || d != 0 {
		t.Errorf("同分布 KS 应为 0, 得 %v", d)
	}
}

func TestFoldCanaryWinRate(t *testing.T) {
	stateDir := t.TempDir()
	expDir := filepath.Join(stateDir, "evolution", "experiments")
	os.MkdirAll(expDir, 0o755)
	// 实验起点 5000; 基线 3 run score~0.3, 候选 3 run score~0.8 → uplift>0 胜。
	write := func(p string, v any) { b, _ := json.Marshal(v); os.WriteFile(p, b, 0o644) }
	write(filepath.Join(expDir, "exp-win.json"), map[string]any{"id": "exp-win", "started_at_ms": 5000})
	write(filepath.Join(expDir, "exp-lose.json"), map[string]any{"id": "exp-lose", "started_at_ms": 5000})
	// rewards: run A (before, 0.2), B (after, 0.9) —— 两个实验共用同一 rewards 时序。
	var rewards []string
	for i := 0; i < 3; i++ {
		rewards = append(rewards, mkReward(4000+int64(i), "runA", 0.2))
		rewards = append(rewards, mkReward(6000+int64(i), "runB", 0.9))
	}
	os.WriteFile(filepath.Join(stateDir, "evolution", "rewards.jsonl"), []byte(strings.Join(rewards, "\n")+"\n"), 0o644)

	wins, total := FoldCanaryWinRate(stateDir)
	if wins != 2 || total != 2 {
		t.Errorf("两个实验都应判定胜 (共享同一 rewards 分割), 得 wins=%d total=%d", wins, total)
	}
}

func mkReward(tsMilli int64, runID string, v float64) string {
	b, _ := json.Marshal(map[string]any{"ts": tsMilli, "run_id": runID, "source": "gate.test", "value": v, "weight": 1.0})
	return string(b)
}

func TestFoldPromoteSurvival(t *testing.T) {
	stateDir := t.TempDir()
	skillRoot := filepath.Join(stateDir, "skills")
	// 晋升后 10 天退场 → 不生存; 晋升 40 天后退场 → 生存; 仍在 active → 生存。
	mk := func(name, body string) {
		os.MkdirAll(filepath.Join(skillRoot, name), 0o755)
		os.WriteFile(filepath.Join(skillRoot, name, "SKILL.md"), []byte(body), 0o644)
	}
	t0 := "2026-01-01T00:00:00Z"
	mk("died", "---\nname: died\nstatus: archived\naudit: active@"+t0+"\naudit: archived@2026-01-11T00:00:00Z\n---\nbody")
	mk("lived", "---\nname: lived\nstatus: archived\naudit: active@"+t0+"\naudit: archived@2026-02-10T00:00:00Z\n---\nbody")
	mk("young", "---\nname: young\nstatus: active\naudit: active@"+t0+"\n---\nbody")

	survived, total := FoldPromoteSurvival(stateDir, time.Now())
	if total != 3 {
		t.Errorf("3 个晋升过技能都应参与, 得 %d", total)
	}
	if survived != 2 {
		t.Errorf("died 不生存, lived/young 生存, 得 %d", survived)
	}
}

func TestParseAuditLineage(t *testing.T) {
	content := "---\nname: x\nstatus: active\naudit: active@2026-01-01T00:00:00Z reward_avg=0.500 samples=10\naudit: archived@2026-01-05T00:00:00Z reward_avg=0.300 samples=4\n---\n"
	hist := parseAuditLineage(content)
	if len(hist) != 2 {
		t.Fatalf("应解析出 2 条谱系, 得 %d", len(hist))
	}
	if hist[0].status != "active" || hist[1].status != "archived" {
		t.Errorf("谱系序应 active→archived, 得 %v → %v", hist[0].status, hist[1].status)
	}
}

func TestParseEntryTSDual(t *testing.T) {
	rfc := json.RawMessage(`"2026-04-14T09:51:11.930424084+08:00"`)
	if parseEntryTS(rfc) == 0 {
		t.Error("RFC3339(含纳秒+时区)应可解析")
	}
	milli := json.RawMessage(`1785804837755`)
	if parseEntryTS(milli) == 0 {
		t.Error("unix-milli 应可解析")
	}
	if parseEntryTS(json.RawMessage(`null`)) != 0 || parseEntryTS(json.RawMessage(`"banana"`)) != 0 {
		t.Error("非法 ts 应返回 0")
	}
}

func TestFoldNoDataMeansMissingKey(t *testing.T) {
	// 空账本 → 键缺失而非 0（0 是合法折叠值, 与「无数据」必须可区分）。
	got := foldEntries(nil, time.UnixMilli(0), time.UnixMilli(1), FoldTotalLLMTokens())
	if _, ok := got["total_llm_tokens"]; ok {
		t.Error("空账本不应产出键")
	}
}
