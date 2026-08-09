package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestRunIsolated_PathB工具消费回合路由到执行模型
//
// 回归: RunIsolated 是团队 stage 的执行路径 (cliAgentRunner.Execute), 此前
// 循环里单模型 client 直调, 配了 ExecutionModel 也完全不生效。修复后:
//   - 无 tool_result 的推理回合 → 主模型
//   - 尾部是 tool_result 的工具消费回合 → ExecutionModel (lfm)
//
// 假上游记录每轮请求体的 model, 先回一轮 tool_use (用 registry 里的工具),
// 模型下一轮带 tool_result 再来 → 第二轮必须用 lfm。
func TestRunIsolated_PathB工具消费回合路由到执行模型(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string            `json:"role"`
				Content json.RawMessage   `json:"content"`
				Content0 []json.RawMessage `json:"-"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req.Model)
		mu.Unlock()

		// 第一轮: 无 tool_result, 回 tool_use 触发工具循环。
		hasToolResult := false
		for _, m := range req.Messages {
			var blocks []map[string]any
			if json.Unmarshal(m.Content, &blocks) == nil {
				for _, b := range blocks {
					if b["type"] == "tool_result" {
						hasToolResult = true
					}
				}
			}
		}
		var resp types.APIResponse
		if !hasToolResult {
			resp = types.APIResponse{
				ID: "msg_1", Type: "message", Role: "assistant", Model: req.Model,
				Content: []types.ContentBlock{{
					Type: types.ContentBlockToolUse,
					ID:   "tu1", Name: "noop_tool",
					Input: json.RawMessage(`{}`),
				}},
				StopReason: "tool_use",
				Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
			}
		} else {
			resp = types.APIResponse{
				ID: "msg_2", Type: "message", Role: "assistant", Model: req.Model,
				Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "完成"}},
				StopReason: "end_turn",
				Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "gemma4:26b-a4b-it-qat",
		Client: srv.Client(), RetryCount: 0}
	e := NewQueryEngine(&Config{
		Model:          "gemma4:26b-a4b-it-qat",
		ExecutionModel: "lfm2.5:2.6b-q4_k_m",
	}, client, reg, hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeAcceptEdits), nil, nil)

	out, err := e.RunIsolated(context.Background(), "分析一下",
		IsolatedRunOptions{MaxTurns: 5, DisableTools: false})
	if err != nil {
		t.Fatalf("RunIsolated 失败: %v", err)
	}
	if !strings.Contains(out, "完成") {
		t.Fatalf("最终产出不对: %q", out)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("应至少 2 次 LLM 调用 (推理+工具消费), got %d: %v", len(seen), seen)
	}
	if seen[0] != "gemma4:26b-a4b-it-qat" {
		t.Fatalf("推理回合应为主模型, got %q", seen[0])
	}
	if seen[len(seen)-1] != "lfm2.5:2.6b-q4_k_m" {
		t.Fatalf("工具消费回合应路由到执行模型 lfm, got %q (seen=%v)", seen[len(seen)-1], seen)
	}
}

// routeRecorder 记录假上游收到的请求 model (线程安全快照)。
type routeRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *routeRecorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, s)
}

func (r *routeRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// TestPlanModelRouting 规划期独立模型路由 (plan flag ON → PlanClient)
//
// Plan mode 类似 advisor 独立客户端: 会话 plan flag ON 时 queryLoop 全程路由到
// PlanClient (独立 provider/模型, Tag=plan), 优先级高于 ExecutionModel 相位路由。
// 判定放在 PhasePreRequest 之后, 首个规划回合 (TurnCount==0) 也能命中。
// 两个假上游各自记录收到的 model 名: plan active 时只有 plan server 收到请求,
// plan OFF 时只有主 server 收到请求 (回归: 不启用时行为与旧版一致)。
func TestPlanModelRouting(t *testing.T) {
	newSrv := func(t *testing.T) (*httptest.Server, *routeRecorder) {
		rec := &routeRecorder{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			rec.add(req.Model)
			resp := types.APIResponse{
				ID: "msg_x", Type: "message", Role: "assistant", Model: req.Model,
				Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "规划完成"}},
				StopReason: "end_turn",
				Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		t.Cleanup(srv.Close)
		return srv, rec
	}

	run := func(t *testing.T, mainSrv, planSrv *httptest.Server, planActive bool) {
		reg := tool.NewRegistry()
		reg.Register(noopTool{})
		mainClient := &api.Client{BaseURL: mainSrv.URL, APIKey: "k", Model: "gemma4:26b-a4b-it-qat",
			Client: mainSrv.Client(), RetryCount: 0}
		planClient := &api.Client{BaseURL: planSrv.URL, APIKey: "k", Model: "kimi:kimi-k2",
			Client: planSrv.Client(), RetryCount: 0}
		e := NewQueryEngine(&Config{
			Model:            "gemma4:26b-a4b-it-qat",
			PlanClient:       planClient,
			DynamicPlanCheck: func() bool { return planActive },
		}, mainClient, reg, hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeAcceptEdits), nil, nil)
		out, err := e.RunIsolated(context.Background(), "开始规划",
			IsolatedRunOptions{MaxTurns: 5, DisableTools: false})
		if err != nil {
			t.Fatalf("RunIsolated 失败: %v", err)
		}
		if !strings.Contains(out, "规划完成") {
			t.Fatalf("最终产出不对: %q", out)
		}
	}

	t.Run("plan-active路由到PlanClient", func(t *testing.T) {
		mainSrv, mainSeen := newSrv(t)
		planSrv, planSeen := newSrv(t)
		run(t, mainSrv, planSrv, true)
		if got := planSeen.snapshot(); len(got) != 1 || got[0] != "kimi:kimi-k2" {
			t.Fatalf("plan 期应路由到 PlanClient (kimi:kimi-k2), got %v", got)
		}
		if got := mainSeen.snapshot(); len(got) != 0 {
			t.Fatalf("plan 期不应打到主客户端, got %v", got)
		}
	})

	t.Run("plan关闭沿用主模型", func(t *testing.T) {
		mainSrv, mainSeen := newSrv(t)
		planSrv, planSeen := newSrv(t)
		run(t, mainSrv, planSrv, false)
		if got := mainSeen.snapshot(); len(got) != 1 || got[0] != "gemma4:26b-a4b-it-qat" {
			t.Fatalf("plan 关闭应沿用主模型, got %v", got)
		}
		if got := planSeen.snapshot(); len(got) != 0 {
			t.Fatalf("plan 关闭不应打到 PlanClient, got %v", got)
		}
	})
}

// noopTool 空操作工具: 注册进 registry 让 RunIsolated 的工具循环能真正执行。
type noopTool struct{}

func (noopTool) Name() string { return "noop_tool" }
func (noopTool) Description() string {
	return "No-op tool for Path B routing test."
}
func (noopTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (noopTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (noopTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (noopTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (noopTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: "noop ok"}, nil
}
