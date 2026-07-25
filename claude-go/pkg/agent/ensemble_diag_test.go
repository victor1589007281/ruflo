package agent

// ensemble_diag_test.go —— 钉住 design/02 §3.1 例外 ② 的收编:
// 合议扇出的诊断元数据 (stop/outTok/blocks/tail) 在 LLM 被收进 L1 网关之后
// **不许静默降级**。
//
// 这组用例的靶子是一类不会报错的缺陷: 旧代码用 `we.llm.(*api.Client)` 具体类型
// 断言取 CompleteDiag, 一旦注入方换成任何接口包装, 断言失败 → 走 else 分支的
// SimpleComplete → 产出照出、日志照打, 只有 diag 那一栏永远是空的。没有 panic、
// 没有 error、没有编译错误。所以这里必须**逐条断言 diag 非空**, 而不是只看产出。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/llmgw"
)

// *api.Client 必须始终满足该端口 (改造前唯一的诊断来源, 不能因重构掉队)。
var _ DiagLLMClient = (*api.Client)(nil)

// llmgw.SimpleClient 是本轮新收编的注入形态 (pkg/feishu/bot.go 的
// TeamManagerConfig.LLM)。这一行是**编译期**的静默降级防线: 网关侧一旦改名或改
// 签名, 这里立刻红, 而不是等线上诊断变空才发现。
var _ DiagLLMClient = llmgw.SimpleClient{}

// simpleOnlyLLM 只有 SimpleComplete 的 LLM 端口 (测试假件 / 第三方实现的形态)。
type simpleOnlyLLM struct{ reply string }

func (f simpleOnlyLLM) SimpleComplete(_ context.Context, _, _ string) (string, error) {
	return f.reply, nil
}

// diagCapableLLM 同时具备 SimpleComplete 与 CompleteDiag。
// 两条路径回不同的文本, 用来判定"到底走了哪一条"—— 只看 diag 空不空不够,
// 空 diag 也可能是 CompleteDiag 自己返回的空串。
type diagCapableLLM struct{}

func (diagCapableLLM) SimpleComplete(_ context.Context, _, _ string) (string, error) {
	return "走了SimpleComplete", nil
}

func (diagCapableLLM) CompleteDiag(_ context.Context, _, _ string) (string, string, error) {
	return "走了CompleteDiag", "stop=end_turn outTok=7", nil
}

// TestCompleteWithDiag能力判定 三种注入形态 × want 开关的取路。
func TestCompleteWithDiag能力判定(t *testing.T) {
	ctx := context.Background()

	t.Run("有诊断能力且需要诊断_走CompleteDiag", func(t *testing.T) {
		text, diag, err := completeWithDiag(ctx, diagCapableLLM{}, true, "sys", "user")
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if text != "走了CompleteDiag" {
			t.Fatalf("应走 CompleteDiag, got=%q", text)
		}
		if diag == "" {
			t.Fatal("诊断元数据丢失 —— 这正是静默降级的表现")
		}
	})

	t.Run("有诊断能力但不需要_仍走SimpleComplete", func(t *testing.T) {
		// review_panel 历来只调 SimpleComplete, 口径必须照旧: "顺手统一"会让
		// 两个 mode 的请求形态分叉 (见 graph_templates_ensemble.go 文件头第 3 条)。
		text, diag, err := completeWithDiag(ctx, diagCapableLLM{}, false, "sys", "user")
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if text != "走了SimpleComplete" {
			t.Fatalf("want=false 时必须走 SimpleComplete, got=%q", text)
		}
		if diag != "" {
			t.Fatalf("want=false 时不该有诊断元数据: %q", diag)
		}
	})

	t.Run("无诊断能力_回落且不报错", func(t *testing.T) {
		// 真的没有这项能力时回落是**正确行为**, 不是降级 —— 区别在于它本来就没有。
		text, diag, err := completeWithDiag(ctx, simpleOnlyLLM{reply: "ok"}, true, "sys", "user")
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if text != "ok" || diag != "" {
			t.Fatalf("回落路径异常: text=%q diag=%q", text, diag)
		}
	})
}

// TestCompleteWithDiag经L1网关 端到端: pkg/agent → llmgw.SimpleClient →
// LLMGateway.Diag → llmgw.Local → api.Client.CompleteDiag → 假端点。
//
// 这是本轮收编的**反证用例**: 把 completeWithDiag 的断言改回
// `llm.(*api.Client)` 时, 本用例的 diag 会变成空串而其余断言全绿 ——
// 精确复现"套接口后诊断静默消失"。
func TestCompleteWithDiag经L1网关(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant",
			"content":[{"type":"text","text":"经网关的产出"}],
			"model":"fake","stop_reason":"end_turn",
			"usage":{"input_tokens":3,"output_tokens":9}}`)
	}))
	defer srv.Close()

	gw := llmgw.NewLocal(api.NewClient(srv.URL+"/v1", "k", "fake"))
	var llm LLMClient = llmgw.SimpleClient{GW: gw} // 与 bot.go 注入 TeamManagerConfig.LLM 的值同型

	text, diag, err := completeWithDiag(context.Background(), llm, true, "sys", "user")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if text != "经网关的产出" {
		t.Fatalf("产出异常: %q", text)
	}
	if !strings.Contains(diag, "stop=end_turn") || !strings.Contains(diag, "outTok=9") {
		t.Fatalf("经网关后诊断元数据不完整 (静默降级): %q", diag)
	}
}
