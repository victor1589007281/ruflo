package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestAutoMode_EngineComplexTaskPlanToExecution 端到端验证引擎级 AutoMode:
//
//	复杂任务 → 首轮注入 system-reminder + 自动开启会话级 plan (只读)
//	→ 规划期写工具被拒 (tool_result IsError)
//	→ 模型调 ExitPlanMode → 自动放行 → 写工具执行成功
//
// 假上游按状态机回包:
//
//	req1: tool_use test_write          (规划期, 应被只读拒)
//	req2: tool_use ExitPlanMode        (规划完成, 清 flag)
//	req3: tool_use test_write          (实施期, 应放行)
//	req4: end_turn 完成
func TestAutoMode_EngineComplexTaskPlanToExecution(t *testing.T) {
	const session = "t"

	var (
		mu   sync.Mutex
		recs []reqRecord
		// 会话 flag 在服务端 handler 侧的采样
		planActiveDuringFirstExec bool
		planActiveAfterExit       bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string          `json:"model"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		rec := reqRecord{reqNo: len(recs) + 1}
		for _, m := range req.Messages {
			var blocks []map[string]any
			if json.Unmarshal(m.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch b["type"] {
				case "text":
					if s, ok := b["text"].(string); ok {
						rec.bodies = append(rec.bodies, s)
					}
				case "tool_result":
					isErr, _ := b["is_error"].(bool)
					content, _ := b["content"].(string)
					rec.toolResults = append(rec.toolResults, toolResultSeen{isErr: isErr, content: content})
				}
			}
		}
		reqNo := len(recs)
		recs = append(recs, rec)
		mu.Unlock()

		// flag 采样: req2 处理时 (第一次写工具已被拒), req3 处理时 (ExitPlanMode 后)。
		if reqNo == 1 {
			planActiveDuringFirstExec = builtin.PlanModeActiveForSession(session)
		}
		if reqNo == 2 {
			planActiveAfterExit = builtin.PlanModeActiveForSession(session)
		}

		var resp types.APIResponse
		switch reqNo {
		case 0: // 规划期: 尝试写工具
			resp = toolUseResp(req.Model, "tu1", "test_write", `{}`)
		case 1: // 规划完成: ExitPlanMode
			resp = toolUseResp(req.Model, "tu2", builtin.ExitPlanModeToolName, `{"plan":"方案: 1. 调研 2. 实现 3. 测试"}`)
		case 2: // 实施期: 写工具
			resp = toolUseResp(req.Model, "tu3", "test_write", `{}`)
		default: // 完成
			resp = types.APIResponse{
				ID: "msg_4", Type: "message", Role: "assistant", Model: req.Model,
				Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "完成"}},
				StopReason: "end_turn",
				Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEResponse(w, resp)
	}))
	defer srv.Close()

	writes := &testWriteTool{writes: 0}
	reg := tool.NewRegistry()
	reg.Register(builtin.NewEnterPlanModeTool())
	reg.Register(builtin.NewExitPlanModeTool())
	reg.Register(writes)

	planDir := t.TempDir()
	cfg := &Config{
		Model:            "test-model",
		SessionID:        session,
		PermissionMode:   types.PermissionModeBypass,
		DynamicPlanCheck: builtin.NewPlanModeChecker(session),
		PlanModeSetter:   func(on bool) { builtin.SetPlanModeForSession(session, on) },
		AutoPlanMode:     true,
		ComplexityMode:   "heuristic",
		PlanFileDir:      planDir, // ExitPlanMode 计划落盘
	}
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "test-model",
		Client: srv.Client(), RetryCount: 0}
	e := NewQueryEngine(cfg, client, reg, hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeBypass), nil, prompt.NewManager(""))

	for m := range e.SubmitMessage(context.Background(),
		"帮我设计并实现一个多文件功能，并且需要数据库迁移，同时要补充测试。") {
		_ = m
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recs) != 4 {
		t.Fatalf("应 4 次 LLM 调用, got %d: %+v", len(recs), recs)
	}

	// 1. 首轮注入 system-reminder + 规划引导
	if !strings.Contains(strings.Join(recs[0].bodies, "\n"), "<system-reminder>") ||
		!strings.Contains(strings.Join(recs[0].bodies, "\n"), "已进入规划模式") {
		t.Fatalf("首轮应含 system-reminder 规划引导: %q", recs[0].bodies)
	}

	// 2. 规划期写工具被拒
	if len(recs[1].toolResults) != 1 || !recs[1].toolResults[0].isErr {
		t.Fatalf("规划期 test_write 应被只读拒绝 (IsError): %+v", recs[1].toolResults)
	}
	if !planActiveDuringFirstExec {
		t.Fatal("第一次写工具执行时会话 plan flag 应为 true")
	}

	// 3. ExitPlanMode 后自动放行, flag 已清
	if planActiveAfterExit {
		t.Fatal("ExitPlanMode 后会话 plan flag 应已清空 (false)")
	}
	trs := recs[3].toolResults
	if len(trs) == 0 || trs[len(trs)-1].isErr {
		t.Fatalf("实施期 test_write 应放行 (最新 tool_result IsError=false): %+v", recs[3].toolResults)
	}
	if writes.writes != 1 {
		t.Fatalf("test_write 应只执行 1 次 (规划期被拒不执行), got %d", writes.writes)
	}
	if builtin.PlanModeActiveForSession(session) {
		t.Fatal("任务结束后会话 plan flag 应已清空")
	}

	// 4. ExitPlanMode 已把计划落盘到 PlanFileDir, 实施阶段可读取
	entries, err := os.ReadDir(planDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("应生成计划文件: err=%v entries=%d", err, len(entries))
	}
	found := false
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(planDir, e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "方案: 1. 调研 2. 实现 3. 测试") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("计划文件应包含提交的 plan 文本")
	}
}

// TestAutoMode_EngineSimpleTaskNotTriggered 简单任务不注入、不开 plan。
func TestAutoMode_EngineSimpleTaskNotTriggered(t *testing.T) {
	const session = "s"
	var (
		mu          sync.Mutex
		firstBodies []string
	)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		for _, m := range req.Messages {
			var blocks []map[string]any
			if json.Unmarshal(m.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b["type"] == "text" {
					if s, ok := b["text"].(string); ok {
						firstBodies = append(firstBodies, s)
					}
				}
			}
		}
		calls++
		n := calls
		mu.Unlock()

		// 首轮回 test_write (工具回合), 之后回 end_turn 终止, 避免无限工具循环。
		var resp types.APIResponse
		if n == 1 {
			resp = toolUseResp(req.Model, "tu1", "test_write", `{}`)
		} else {
			resp = types.APIResponse{
				ID: "msg_simple", Type: "message", Role: "assistant", Model: req.Model,
				Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "你好"}},
				StopReason: "end_turn",
				Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEResponse(w, resp)
	}))
	defer srv.Close()

	reg := tool.NewRegistry()
	reg.Register(builtin.NewEnterPlanModeTool())
	reg.Register(builtin.NewExitPlanModeTool())
	reg.Register(&testWriteTool{writes: 0})

	cfg := &Config{
		Model:            "test-model",
		SessionID:        session,
		PermissionMode:   types.PermissionModeBypass,
		DynamicPlanCheck: builtin.NewPlanModeChecker(session),
		PlanModeSetter:   func(on bool) { builtin.SetPlanModeForSession(session, on) },
		AutoPlanMode:     true,
		ComplexityMode:   "heuristic",
	}
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "test-model",
		Client: srv.Client(), RetryCount: 0}
	e := NewQueryEngine(cfg, client, reg, hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeBypass), nil, prompt.NewManager(""))

	for m := range e.SubmitMessage(context.Background(), "你好") {
		_ = m
	}
	if strings.Contains(strings.Join(firstBodies, "\n"), "<system-reminder>") {
		t.Fatalf("简单任务不应注入 system-reminder: %q", firstBodies)
	}
	if builtin.PlanModeActiveForSession(session) {
		t.Fatal("简单任务不应开 plan")
	}
}

// ---- helpers ----

type reqRecord struct {
	reqNo       int
	bodies      []string
	toolResults []toolResultSeen
}

type toolResultSeen struct {
	isErr   bool
	content string
}

func toolUseResp(model, id, name, input string) types.APIResponse {
	return types.APIResponse{
		ID: "msg_" + id, Type: "message", Role: "assistant", Model: model,
		Content: []types.ContentBlock{{
			Type: types.ContentBlockToolUse, ID: id, Name: name,
			Input: json.RawMessage(input),
		}},
		StopReason: "tool_use",
		Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
	}
}

// writeSSEResponse 把一次 APIResponse 编码为 queryLoop 消费的 SSE 事件序列
// (message_start → content_block_start → content_block_delta → content_block_stop
// → message_delta), 格式对齐 api/client.go StreamMessage 的 "data: {json}\n\n" 解析器。
func writeSSEResponse(w http.ResponseWriter, resp types.APIResponse) {
	sse := func(d types.StreamDelta) {
		b, _ := json.Marshal(d)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	}
	// message_start: 携带 usage (input/output tokens 采集)
	sse(types.StreamDelta{Type: "message_start", Message: &resp})
	for i, blk := range resp.Content {
		cb := blk
		// content_block_start
		sse(types.StreamDelta{Type: "content_block_start", Index: i, ContentBlock: &cb})
		// content_block_delta
		switch blk.Type {
		case types.ContentBlockToolUse:
			input := "{}"
			if len(blk.Input) > 0 {
				input = string(blk.Input)
			}
			sse(types.StreamDelta{Type: "content_block_delta", Index: i,
				Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: input}})
		case types.ContentBlockText:
			sse(types.StreamDelta{Type: "content_block_delta", Index: i,
				Delta: &types.DeltaContent{Type: "text_delta", Text: blk.Text}})
		}
		// content_block_stop
		sse(types.StreamDelta{Type: "content_block_stop", Index: i})
	}
	// message_delta: 结束 (stop_reason + 累计 usage)
	sse(types.StreamDelta{Type: "message_delta", Usage: resp.Usage,
		Delta: &types.DeltaContent{StopReason: resp.StopReason}})
}

// testWriteTool 模拟一个"写工具": Plan 模式自拒 (镜像 filewrite.go), Call 计数。
type testWriteTool struct {
	writes int
}

func (t *testWriteTool) Name() string { return "test_write" }
func (t *testWriteTool) Description() string {
	return "Test write tool for AutoMode gating."
}
func (t *testWriteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *testWriteTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *testWriteTool) IsReadOnly(_ json.RawMessage) bool        { return false }

func (t *testWriteTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.PermissionMode == types.PermissionModePlan {
		return &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "Plan mode: 写入操作不可用"}
	}
	return nil
}

func (t *testWriteTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	t.writes++
	return &tool.ToolResult{Content: "wrote ok"}, nil
}
