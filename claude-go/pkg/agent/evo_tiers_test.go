package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/evolution/replay"
)

// 没配 fallback 时只给 primary 一档 —— 这会让 Smoke 因 MinTiers 不足而拒绝,
// **那是正确行为**: 没有第二档就确实没做过多档验证, 不该放行。
func TestEvoTierFactory_无fallback只给一档(t *testing.T) {
	c := &api.Client{Model: "kimi:k3"}
	f := EvoTierFactory(c)
	if f == nil {
		t.Fatal("有 client 时不该返回 nil")
	}
	tiers := f(context.Background(), "skill-x", "正文")
	if len(tiers) != 1 {
		t.Fatalf("档位数 = %d, 期望 1", len(tiers))
	}
	if tiers[0].Name != "primary" || tiers[0].Model != "kimi:k3" {
		t.Errorf("档位 = %+v", tiers[0])
	}
	if tiers[0].Candidate == nil {
		t.Error("Candidate 为 nil —— 该档会被当成未配置")
	}
}

// 配了 fallback 就给两档, 且**必须是不同的 model** —— 用主模型凑第二档只能证明
// 它不随机崩, 证不了"换个弱一点的模型也还能用", 而后者才是晋升前真正要问的。
func TestEvoTierFactory_两档且模型不同(t *testing.T) {
	c := &api.Client{Model: "kimi:k3", FallbackModels: []string{"kimi:k3", "", "kimi:k2"}}
	tiers := EvoTierFactory(c)(context.Background(), "p", "body")
	if len(tiers) != 2 {
		t.Fatalf("档位数 = %d, 期望 2", len(tiers))
	}
	if tiers[1].Model != "kimi:k2" {
		t.Errorf("第二档 model = %q, 期望 kimi:k2 (与主模型同名的和空串都该被跳过)", tiers[1].Model)
	}
	if tiers[0].Model == tiers[1].Model {
		t.Error("两档同模型 —— 那是假的多档")
	}
}

// nil client 返回 nil, 让调用方据此不注入 (而不是注入一个必然失败的构造器)。
func TestEvoTierFactory_nil客户端(t *testing.T) {
	if EvoTierFactory(nil) != nil {
		t.Error("nil client 应返回 nil")
	}
}

// 候选要真打 LLM: 产物作 system, 任务 objective 作 user。
func TestTierCandidate_真打调用(t *testing.T) {
	var gotSys, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			System   any `json:"system"`
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotSys = fmt.Sprintf("%v", req.System)
		if len(req.Messages) > 0 {
			gotUser = fmt.Sprintf("%v", req.Messages[0].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"产出内容"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	c := api.NewClient(srv.URL, "k", "test-model")
	cand := &tierCandidate{name: "primary:test", client: c, body: "被测技能正文"}
	out, err := cand.Produce(context.Background(), replay.Task{ID: "t1", Objective: "冒烟目标"})
	if err != nil {
		t.Fatalf("真调用失败: %v", err)
	}
	if out != "产出内容" {
		t.Errorf("产出 = %q", out)
	}
	if !strings.Contains(gotSys, "被测技能正文") {
		t.Errorf("产物未作 system 下发: %q", gotSys)
	}
	if !strings.Contains(gotUser, "冒烟目标") {
		t.Errorf("objective 未作 user 下发: %q", gotUser)
	}
}

// 错误绝不能被吞成空串: 空产出会被覆盖率判定当成"跑了但没评分",
// 与"根本没跑起来"混成一档, 冒烟结论就失真了。
func TestTierCandidate_错误不吞(t *testing.T) {
	for _, tc := range []struct {
		name string
		cand *tierCandidate
		task replay.Task
	}{
		{"无客户端", &tierCandidate{name: "x"}, replay.Task{Objective: "o"}},
		{"产物为空", &tierCandidate{name: "x", client: &api.Client{}, body: "   "}, replay.Task{Objective: "o"}},
		{"任务无目标", &tierCandidate{name: "x", client: &api.Client{}, body: "b"}, replay.Task{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.cand.Produce(context.Background(), tc.task)
			if err == nil {
				t.Error("应报错而不是返回空串")
			}
			if out != "" {
				t.Errorf("出错时不该有产出: %q", out)
			}
		})
	}
}
