package internal_hook_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agentdbsemantic"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/types"
	"gitee.com/lorydb/agentDB/pkg/embedding"
	"gitee.com/lorydb/agentDB/pkg/memory"
	"gitee.com/lorydb/agentDB/pkg/server"
)

// newSemanticServe 与 pkg/agentdbsemantic 测试同构: 进程内 agentDB serve (层2 语义后端)。
func newSemanticServe(t *testing.T) *agentdbsemantic.Client {
	t.Helper()
	mem, err := memory.NewStore(128)
	if err != nil {
		t.Fatal(err)
	}
	emb := embedding.NewService()
	emb.Register("default", embedding.NewHashStubProvider("default", 128, 512))
	emb.SetDefault("default")
	srv := server.New(server.Backends{Memory: mem, Embedding: emb})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return agentdbsemantic.New(ts.URL)
}

// TestAgentDBSemanticInjectE2E M2 验收: MemoryInject 检索注入。
// agentDB 存有与用户请求相关的记忆 → 引擎首轮请求的 system prompt 注入
// <memory_context source="agentdb">, 模型在回答前感知该记忆。
func TestAgentDBSemanticInjectE2E(t *testing.T) {
	client := newSemanticServe(t)
	ctx := context.Background()
	content := "用户偏好使用 Go 编写高并发服务"
	if _, err := client.StoreMemory(ctx, agentdbsemantic.Memory{
		Content: content,
		Type:    agentdbsemantic.MemorySemantic,
	}); err != nil {
		t.Fatalf("StoreMemory: %v", err)
	}

	h := internal_hook.NewAgentDBSemanticInjectHook(client, 5, 600, 200)
	hc := &internal_hook.HookContext{
		Ctx:          ctx,
		Phase:        internal_hook.PhasePreRequest,
		TurnCount:    0,
		Messages:     []types.Message{{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: content}}}},
		SystemPrompt: []string{"你是软件工程师助手。"},
	}
	res, err := h.Execute(hc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil {
		t.Fatal("复杂任务应产生注入结果")
	}
	joined := strings.Join(res.SystemPrompt, "\n")
	if !strings.Contains(joined, "<memory_context source=\"agentdb\"") {
		t.Fatalf("system prompt 应含 agentdb 记忆上下文, got %q", joined)
	}
	if !strings.Contains(joined, content) {
		t.Fatalf("注入内容应含检索到的记忆, got %q", joined)
	}

	// 非首轮不注入 (多轮 tool_use 循环避免上下文膨胀)。
	hc.TurnCount = 1
	res2, err := h.Execute(hc)
	if err != nil {
		t.Fatalf("Execute(轮2): %v", err)
	}
	if res2 != nil {
		t.Fatal("非首轮不应注入")
	}
}
