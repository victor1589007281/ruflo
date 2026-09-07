// pool_quant_handlers_test.go —— 13.7.9/13.8.5 dashboard 新端点单测。
//
// 锁定语义:
//  1. handlePool: GET-only / poolLister 未注入 → workers 缺省 (不报错) /
//     注入后聚合 worker 心跳摘要 / EnvEnabled 由环境位区分;
//  2. handleEvolutionQuant: 空账本 → null 键缺失语义 (NaN/无实验不落 0);
//     带 fixture → 派生比率与告警位正确;
//  3. foldSnapshotToDTO: NaN→null / canaryWinRate 派生 / 四条告警文案。
package dashboard

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/cluster"
	"github.com/anthropic/claude-go/pkg/evolution"
	"github.com/anthropic/claude-go/pkg/tool"
)

func getJSON(t *testing.T, h http.HandlerFunc) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("应答非 JSON: %v; body=%s", err, rec.Body.String())
	}
	return rec.Code, out
}

// ---------------------------------------------------------------------
// handlePool
// ---------------------------------------------------------------------

func TestHandlePool(t *testing.T) {
	s := NewServer(Config{StateDir: t.TempDir()})

	// ① poolLister 未注入 → workers 缺省, 本进程字段照常 (不报 500)。
	code, body := getJSON(t, s.handlePool)
	if code != http.StatusOK {
		t.Fatalf("无注入器应 200, 得 %d", code)
	}
	if _, ok := body["workers"]; !ok {
		t.Error("workers 字段必须存在 (缺省 null/空, 前端渲染'控制面未装配')")
	}
	if env, _ := body["envEnabled"].(bool); env {
		t.Error("测试进程未设池环境位, envEnabled 应 false")
	}

	// ② 环境位打开 → envEnabled true (与本地池装配状态无关)。
	t.Setenv("CLAUDE_GO_TOOLS_POOL", "1")
	_, body = getJSON(t, s.handlePool)
	if env, _ := body["envEnabled"].(bool); !env {
		t.Error("CLAUDE_GO_TOOLS_POOL=1 时 envEnabled 应 true")
	}
	os.Unsetenv("CLAUDE_GO_TOOLS_POOL")

	// ③ 注入 worker 注册表解析器 → 摘要聚合进 workers。
	p := tool.NewPool()
	p.AddMember("evo_status")
	snap := p.Snapshot()
	s.SetPoolLister(func() ([]cluster.WorkerInfo, error) {
		return []cluster.WorkerInfo{{Name: "w1", Caps: []string{"bash"}, TasksDone: 3, Pool: &snap}}, nil
	})
	code, body = getJSON(t, s.handlePool)
	if code != http.StatusOK {
		t.Fatalf("带注入器应 200, 得 %d", code)
	}
	ws, ok := body["workers"].([]any)
	if !ok || len(ws) != 1 {
		t.Fatalf("workers 应聚合 1 条, 得 %v", body["workers"])
	}
	w0, _ := ws[0].(map[string]any)
	if w0["name"] != "w1" {
		t.Errorf("name=%v, want w1", w0["name"])
	}
	pool, _ := w0["pool"].(map[string]any)
	if pool == nil || pool["enabled"] != true || pool["members"] != float64(1) {
		t.Errorf("池摘要未投影到 JSON: %v", w0["pool"])
	}

	// ④ 注入器报错 → 静默降级为空 workers, 而非 500 (聚合器不因单源失败整体失败)。
	s.SetPoolLister(func() ([]cluster.WorkerInfo, error) {
		return nil, contextError
	})
	code, body = getJSON(t, s.handlePool)
	if code != http.StatusOK {
		t.Fatalf("注入器出错应 200 降级, 得 %d", code)
	}
	if ws, _ := body["workers"].([]any); len(ws) != 0 {
		t.Errorf("出错后 workers 应为空, 得 %v", body["workers"])
	}
}

var contextError = errStub{}

type errStub struct{}

func (errStub) Error() string { return "stub" }

// ---------------------------------------------------------------------
// handleEvolutionQuant + foldSnapshotToDTO
// ---------------------------------------------------------------------

// writeQuantFixture 按 ledger_test.go 同款契约铺账本 (rewards/experiments)。
func writeQuantFixture(t *testing.T, stateDir string) {
	t.Helper()
	expDir := filepath.Join(stateDir, "evolution", "experiments")
	os.MkdirAll(expDir, 0o755)
	write := func(p string, v any) { b, _ := json.Marshal(v); os.WriteFile(p, b, 0o644) }
	write(filepath.Join(expDir, "exp-win.json"), map[string]any{"id": "exp-win", "started_at_ms": 5000})
	write(filepath.Join(expDir, "exp-lose.json"), map[string]any{"id": "exp-lose", "started_at_ms": 5000})
	var rewards []string
	for i := 0; i < 3; i++ { // 与 ledger_test TestFoldCanaryWinRate 同款: 两实验都判胜
		rewards = append(rewards, mkQuantReward(4000+int64(i), "runA", 0.2))
		rewards = append(rewards, mkQuantReward(6000+int64(i), "runB", 0.9))
	}
	os.WriteFile(filepath.Join(stateDir, "evolution", "rewards.jsonl"),
		[]byte(strings.Join(rewards, "\n")+"\n"), 0o644)
}

func mkQuantReward(tsMilli int64, runID string, v float64) string {
	b, _ := json.Marshal(map[string]any{"ts": tsMilli, "run_id": runID, "source": "gate.test", "value": v, "weight": 1.0})
	return string(b)
}

func TestHandleEvolutionQuantEmpty(t *testing.T) {
	s := NewServer(Config{StateDir: t.TempDir()})
	code, body := getJSON(t, s.handleEvolutionQuant)
	if code != http.StatusOK {
		t.Fatalf("空账本应 200, 得 %d", code)
	}
	// 键缺失语义: 样本不足/无实验 → null 而非 0。
	for _, k := range []string{"rewardDistKS", "canaryWinRate", "promoteSurvival", "injectionUplift"} {
		if v, ok := body[k]; !ok || v != nil {
			t.Errorf("%s 空账本应 null (键缺失), 得 %v (present=%v)", k, v, ok)
		}
	}
	if low, _ := body["canaryWinRateLow"].(bool); low {
		t.Error("无实验不得判 canaryWinRateLow")
	}
	if low, _ := body["promoteSurvivalLow"].(bool); low {
		t.Error("无晋升不得判 promoteSurvivalLow")
	}
	if alerts, ok := body["alerts"]; ok && len(alerts.([]any)) != 0 {
		t.Errorf("空账本不应有告警, 得 %v", alerts)
	}
}

func TestHandleEvolutionQuantWithFixture(t *testing.T) {
	stateDir := t.TempDir()
	writeQuantFixture(t, stateDir)
	s := NewServer(Config{StateDir: stateDir})
	code, body := getJSON(t, s.handleEvolutionQuant)
	if code != http.StatusOK {
		t.Fatalf("带 fixture 应 200, 得 %d", code)
	}
	// 灰度: 2 实验 2 胜 → winRate=1.0, 派生位不告警。
	if wr, _ := body["canaryWinRate"].(float64); math.Abs(wr-1.0) > 1e-9 {
		t.Errorf("canaryWinRate=%v, want 1.0", wr)
	}
	if ct, _ := body["canaryTotal"].(float64); ct != 2 {
		t.Errorf("canaryTotal=%v, want 2", ct)
	}
	if low, _ := body["canaryWinRateLow"].(bool); low {
		t.Error("胜率 1.0 不得告警")
	}
	// Uplift 基线 0.2 vs 候选 0.9: 无注入登记 → 键缺失 null (Buckets=0)。
	if body["injectionUplift"] != nil {
		t.Errorf("无 injections.json 时 uplift 应 null, 得 %v", body["injectionUplift"])
	}
	if b, _ := body["injBuckets"].(float64); b != 0 {
		t.Errorf("injBuckets=0, 得 %v", b)
	}
}

func TestFoldSnapshotToDTO(t *testing.T) {
	now := time.Now()

	// ① NaN 全清理 + 无告警。
	empty := evolution.Snapshot{Now: now}
	empty.RewardDistKS = math.NaN()
	empty.InjectionUplift = evolution.InjectionUpliftPaired{Uplift: math.NaN()}
	d := foldSnapshotToDTO(empty)
	if d.RewardDistKS != nil || d.InjectionUplift != nil || d.CanaryWinRate != nil {
		t.Fatal("NaN/零值必须投影为 null")
	}
	if len(d.Alerts) != 0 {
		t.Errorf("无告警位时 Alerts 应空, 得 %v", d.Alerts)
	}

	// ② 全告警位点亮 → 四条文案齐; 派生比率 = wins/total。
	full := evolution.Snapshot{
		Now: now,
		LearnLLMTokens1h: 30, TotalLLMTokens1h: 100, LearningCostRatio: 0.3, LearningCostOver: true,
		RewardDistKS: 0.6, RewardDistKSHot: true,
		CanaryWins: 1, CanaryTotal: 4, // winRate 0.25 < 0.4
		Promoted: 2, Survived: 1, PromoteSurvival: 0.5, PromoteSurvivalLow: true, // < 0.7
		InjectionUplift: evolution.InjectionUpliftPaired{Uplift: 0.1, Buckets: 2, InjRuns: 3, BaseRuns: 3},
	}
	d = foldSnapshotToDTO(full)
	if d.CanaryWinRate == nil || *d.CanaryWinRate != 0.25 {
		t.Fatalf("canaryWinRate 派生 = wins/total, 得 %v", d.CanaryWinRate)
	}
	if !d.CanaryWinRateLow || !d.PromoteSurvivalLow || !d.RewardDistKSHot || !d.LearningCostOver {
		t.Fatal("前置: 四告警位应点亮")
	}
	if d.InjectionUplift == nil || *d.InjectionUplift != 0.1 || d.InjBuckets != 2 {
		t.Fatalf("uplift 投影错: %+v", d)
	}
	if len(d.Alerts) != 4 {
		t.Fatalf("四告警位应出 4 条文案, 得 %v", d.Alerts)
	}
	for _, kw := range []string{"学习成本", "KS>0.3", "<0.4", "<0.7"} {
		found := false
		for _, a := range d.Alerts {
			if strings.Contains(a, kw) {
				found = true
			}
		}
		if !found {
			t.Errorf("告警文案缺关键字 %q: %v", kw, d.Alerts)
		}
	}

	// ③ 边界内 (无告警) → 无文案, 但 null 语义维持: 有晋升无低标 → survival 有值不告警。
	mid := evolution.Snapshot{
		Now: now,
		CanaryWins: 2, CanaryTotal: 4, // 恰 0.5 > 0.4
		Promoted: 2, Survived: 2, PromoteSurvival: 1.0,
	}
	d = foldSnapshotToDTO(mid)
	if d.CanaryWinRate == nil || *d.CanaryWinRate != 0.5 || d.CanaryWinRateLow {
		t.Errorf("0.5 胜率不得告警: %v low=%v", d.CanaryWinRate, d.CanaryWinRateLow)
	}
	if d.PromoteSurvival == nil || *d.PromoteSurvival != 1.0 || d.PromoteSurvivalLow {
		t.Errorf("生存率 1.0 不得告警: %v low=%v", d.PromoteSurvival, d.PromoteSurvivalLow)
	}
}
