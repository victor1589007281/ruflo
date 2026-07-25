package feishu

// llm_gateway_test.go — design/02 §3.1「上层只依赖 LLMGateway 接口」在飞书主进程
// (生产进程) 这一侧的断言。
//
// 两类断言:
//  ① 编译期: llmgw.SimpleClient 必须满足全部被注入的最小 LLM 端口。少一个方法,
//     这里就编译不过 —— 而不是让 NewBot 的注入点被悄悄改回 *api.Client;
//  ② 运行期: DashboardLLMComplete 真经网关发出请求。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/dreaming"
	"github.com/anthropic/claude-go/pkg/llmgw"
	"github.com/anthropic/claude-go/pkg/skills"
	swarm_intel "github.com/anthropic/claude-go/pkg/swarm_intel"
	"github.com/anthropic/claude-go/pkg/vision"
	"github.com/anthropic/claude-go/pkg/wiki"
)

// ① 编译期断言: NewBot 里这六个注入点都以 llmgw.SimpleClient 传入,
// 任一端口新增方法都会在这里先炸, 而不是在注入点被改回具体客户端时无声无息。
var (
	_ agent.LLMClient       = llmgw.SimpleClient{}
	_ skills.LLMClient      = llmgw.SimpleClient{}
	_ vision.LLMClient      = llmgw.SimpleClient{}
	_ dreaming.LLMClient    = llmgw.SimpleClient{}
	_ swarm_intel.LLMClient = llmgw.SimpleClient{}
	_ wiki.LLMClient        = llmgw.SimpleClient{}
)

func TestBot_DashboardLLMComplete经网关发出(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","content":[{"type":"text","text":"编排草案"}],"usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	client := api.NewClient(upstream.URL, "sk", "m")
	b := &Bot{apiClient: client, llmGW: llmgw.NewLocal(client)}

	out, err := b.DashboardLLMComplete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("DashboardLLMComplete: %v", err)
	}
	if !strings.Contains(out, "编排草案") {
		t.Errorf("输出 = %q", out)
	}
	if hits != 1 {
		t.Errorf("上游命中 %d 次, 期望 1 —— 链路没真走通", hits)
	}
}

// 网关未初始化时报错而不是 panic (与此前 apiClient==nil 的守卫等价)。
func TestBot_网关未初始化时报错(t *testing.T) {
	b := &Bot{}
	if _, err := b.DashboardLLMComplete(context.Background(), "s", "u"); err == nil {
		t.Error("llmGW 为 nil 时应报错")
	}
}

// LLMGateway() 只读暴露给需要 LLM 但不需要具体客户端的子系统。
func TestBot_LLMGateway只读暴露(t *testing.T) {
	client := api.NewClient("http://x", "sk", "m")
	b := &Bot{llmGW: llmgw.NewLocal(client)}
	gw := b.LLMGateway()
	if gw == nil {
		t.Fatal("LLMGateway() 返回 nil")
	}
	local, ok := gw.(*llmgw.Local)
	if !ok || local.Client != client {
		t.Errorf("暴露的网关未包着 bot 自己的客户端: %T", gw)
	}
}
