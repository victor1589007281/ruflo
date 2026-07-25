package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
)

// newFeedbackServer 造一个带真实 teams/ 状态的 dashboard。
func newFeedbackServer(t *testing.T, teamName, runID string) (*Server, string) {
	t.Helper()
	state := t.TempDir()
	dir := filepath.Join(state, "teams", teamName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"` + teamName + `","workflow":"development","status":"completed","lastRunId":"` + runID + `"}`
	if err := os.WriteFile(filepath.Join(dir, "team.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewServer(Config{StateDir: state, Addr: "127.0.0.1:0"}), state
}

// post 打一次 feedback 请求 (走真实 mux, 不直调 handler —— 路由本身也是要验的东西)。
func post(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// readRewardLines 读回 rewards.jsonl。
func readRewardLines(t *testing.T, state string) []agent.RewardEvent {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "evolution", "rewards.jsonl"))
	if err != nil {
		return nil
	}
	var out []agent.RewardEvent
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if ln == "" {
			continue
		}
		var ev agent.RewardEvent
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("非法奖励行: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func TestRunFeedback_二值门禁落盘并带满权重(t *testing.T) {
	s, state := newFeedbackServer(t, "tm", "run-42")
	rec := post(t, s, "/api/runs/run-42/feedback",
		`{"pass":true,"suite":"testforge:regression","detail":"12/12 通过"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	evs := readRewardLines(t, state)
	if len(evs) != 1 {
		t.Fatalf("应写出 1 条奖励, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Source != agent.RewardSourceGateE2E || ev.Value != 1 {
		t.Errorf("source/value 不对: %+v", ev)
	}
	if ev.RunID != "run-42" || ev.Team != "tm" {
		t.Errorf("归因必须齐 (AggregateRewards 按 run+team 过滤): %+v", ev)
	}
	if ev.Weight != agent.RewardSourceWeight(agent.RewardSourceGateE2E) {
		t.Errorf("权重应落盘为 e2e 的满权重, got %v", ev.Weight)
	}
}

func TestRunFeedback_连续分优先且映射与content门禁一致(t *testing.T) {
	for _, c := range []struct {
		score float64
		want  float64
	}{{0, -1}, {50, 0}, {80, 0.6}, {100, 1}} {
		s, state := newFeedbackServer(t, "tm", "run-s")
		// pass 同时给成 false: 断言"给了 score 就以 score 为准"。
		rec := post(t, s, "/api/runs/run-s/feedback",
			fmt.Sprintf(`{"score":%v,"pass":false}`, c.score))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("score=%v: 状态码 %d body=%s", c.score, rec.Code, rec.Body.String())
		}
		evs := readRewardLines(t, state)
		if len(evs) != 1 || !nearlyEqual(evs[0].Value, c.want) {
			t.Errorf("score=%v ⇒ value 应为 %v, got %+v", c.score, c.want, evs)
		}
	}
}

// source 是锁定字段: 不接受调用方自称高权重的源 (design/03 §4.6 防 reward hacking)。
func TestRunFeedback_拒绝自选奖励源(t *testing.T) {
	s, state := newFeedbackServer(t, "tm", "run-42")
	rec := post(t, s, "/api/runs/run-42/feedback", `{"pass":true,"source":"gate.compile"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("自选 source 必须 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(readRewardLines(t, state)) != 0 {
		t.Error("被拒的请求不得留下任何奖励")
	}
	// 显式写对的那个值应当放行 (幂等友好)。
	if rec := post(t, s, "/api/runs/run-42/feedback", `{"pass":true,"source":"gate.e2e"}`); rec.Code != http.StatusAccepted {
		t.Errorf("显式写 gate.e2e 应放行, got %d", rec.Code)
	}
}

// 认领不到 run ⇒ 404 且不落盘 (否则写出的是永远聚合不到的死数据)。
func TestRunFeedback_未知run不落盘(t *testing.T) {
	s, state := newFeedbackServer(t, "tm", "run-42")
	rec := post(t, s, "/api/runs/run-不存在/feedback", `{"pass":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 run 应 404, got %d", rec.Code)
	}
	if len(readRewardLines(t, state)) != 0 {
		t.Error("未知 run 不得写出无归因奖励")
	}
}

// 声明的 team 与本机认领到的不一致 ⇒ 409, 不静默记到别人头上。
func TestRunFeedback_team断言不一致时拒绝(t *testing.T) {
	s, state := newFeedbackServer(t, "tm", "run-42")
	rec := post(t, s, "/api/runs/run-42/feedback", `{"pass":true,"team":"别的团队"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("team 不一致应 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(readRewardLines(t, state)) != 0 {
		t.Error("冲突请求不得落盘")
	}
}

// 无判据 = 没有信号, 不能补默认值。
func TestRunFeedback_缺判据时400(t *testing.T) {
	s, state := newFeedbackServer(t, "tm", "run-42")
	for _, body := range []string{`{}`, `{"suite":"x"}`, `{"score":120}`, `{"score":-1}`} {
		rec := post(t, s, "/api/runs/run-42/feedback", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s 应 400, got %d", body, rec.Code)
		}
	}
	if len(readRewardLines(t, state)) != 0 {
		t.Error("400 的请求不得落盘")
	}
}

func TestRunFeedback_路径与方法(t *testing.T) {
	s, _ := newFeedbackServer(t, "tm", "run-42")
	// 形状不对 ⇒ 404 (不顺手提供别的语义)
	for _, p := range []string{"/api/runs/", "/api/runs/run-42", "/api/runs/run-42/other", "/api/runs/a/b/c"} {
		if rec := post(t, s, p, `{"pass":true}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s 应 404, got %d", p, rec.Code)
		}
	}
	// GET ⇒ 405
	req := httptest.NewRequest(http.MethodGet, "/api/runs/run-42/feedback", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 应 405, got %d", rec.Code)
	}
}

func nearlyEqual(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
