package feishu

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
)

// session_model_test.go — 会话身份声明里的 modelAlias 真的通到了客户端上。
//
// # 为什么这条值得单独一测
//
// "模型配上了" 与 "模型没配上" 在建会话这一步的**外部表现完全一样**: 会话建出来了、
// 消息也回了。区别只在发出去的请求打到哪个端点、带哪个模型名。所以这里不断言任何
// 间接效果, 只直接问那个被构造出来的客户端。

// newTestResolver 造一个只含两个 provider 的解析器。
func newTestResolver(t *testing.T) *modelconfig.ConfigResolver {
	t.Helper()
	reg := modelconfig.NewProviderRegistry()
	reg.RegisterProvider(modelconfig.ProviderConfig{
		Name:    "kimi",
		BaseURL: "https://api.kimi.example/coding/v1",
		APIKey:  "sk-kimi",
		Models:  map[string]modelconfig.ModelConfig{"kimi:kimi-for-coding": {}},
	})
	reg.RegisterProvider(modelconfig.ProviderConfig{
		Name:    "ollama",
		BaseURL: "http://127.0.0.1:11434/v1",
		APIKey:  "ollama",
		Models:  map[string]modelconfig.ModelConfig{"ollama:lfm2.5:2.6b-q4_k_m": {}},
	})
	return modelconfig.NewConfigResolver(reg, modelconfig.GlobalConfig{}, nil)
}

// TestClientForAliasBuildsTheAlmightyClient —
//
// 守的是"别名 → 客户端"这一步换的是**整条链路**而不只是模型名字。
// 只改 cli.Model 的实现会把 kimi 的模型名发到默认端点上, 而那个错误要到真的发起
// 请求时才会以 4xx 的形式暴露 —— 在会话构造这一步完全看不出来。
func TestClientForAliasBuildsTheAlmightyClient(t *testing.T) {
	sm := &SessionManager{}
	sm.SetModelResolver(newTestResolver(t))
	// 生产里 apiClient 一定有值。这里也给它一个 —— 否则"只换模型名不换端点"那种
	// 实现会以空指针 panic 的形式失败, 而不是让**断言**说话: 那样这条测试证明的
	// 只是"没崩", 而它要证明的是"端点跟着别名换了"。
	sm.apiClient = api.NewClient("https://default.example/v1", "sk-default", "default-model")

	cli := sm.clientForAlias("kimi:kimi-for-coding")
	if cli == nil {
		t.Fatal("可解析的别名应构造出客户端 —— 构造不出来意味着 spec 里的模型声明是个假开关")
	}
	if cli.BaseURL != "https://api.kimi.example/coding/v1" {
		t.Errorf("端点没跟着别名换 (只换了模型名的话请求会打到默认端点): %q", cli.BaseURL)
	}
	if cli.APIKey != "sk-kimi" {
		t.Errorf("API key 没跟着别名换: %q", cli.APIKey)
	}
	if cli.Model != "kimi-for-coding" {
		t.Errorf("模型名应为厂商裸名, 得到 %q", cli.Model)
	}
}

// TestClientForAliasRejectsUnusableAlias —
//
// 解析不出端点时必须返回 nil, 让调用方**记日志并说明**。返回一个半成品客户端
// (比如只有模型名、端点是空的) 会让请求打到空 URL; 静默回落默认客户端则更糟 ——
// 那正是"配了没生效"伪装成"配了生效了"。
func TestClientForAliasRejectsUnusableAlias(t *testing.T) {
	sm := &SessionManager{}
	sm.SetModelResolver(newTestResolver(t))

	if cli := sm.clientForAlias("不存在的别名"); cli != nil {
		t.Errorf("解析不出的别名不该给出客户端, 得到 %+v", cli)
	}

	// 反向对照: **没注入解析器** (测试/降级装配) 也必须返回 nil, 而不是 panic
	// 或回落 —— 装配缺失时 spec 的模型声明不生效, 这件事要可观测。
	bare := &SessionManager{}
	if cli := bare.clientForAlias("kimi:kimi-for-coding"); cli != nil {
		t.Error("没有解析器时不该凭空造出客户端")
	}
}

// TestClientForAliasDoesNotTouchTheDefaultClient —
//
// 每个会话一个客户端, 构造过程**不得**改动共享的默认客户端。否则给一个会话配上
// 别的模型, 会把同一进程里所有会话的模型一起换掉 —— 而这种污染在单会话测试里
// 完全看不见, 只有并发跑着两个会话时才会显形。
func TestClientForAliasDoesNotTouchTheDefaultClient(t *testing.T) {
	reg := modelconfig.NewProviderRegistry()
	reg.RegisterProvider(modelconfig.ProviderConfig{
		Name: "ollama", BaseURL: "http://127.0.0.1:11434/v1", APIKey: "ollama",
		Models: map[string]modelconfig.ModelConfig{"ollama:small": {}},
	})
	sm := &SessionManager{}
	sm.SetModelResolver(modelconfig.NewConfigResolver(reg, modelconfig.GlobalConfig{}, nil))

	def := api.NewClient("https://default.example/v1", "sk-default", "default-model")
	sm.apiClient = def

	if cli := sm.clientForAlias("ollama:small"); cli == nil {
		t.Fatal("应能解析出 ollama:small")
	}
	if def.BaseURL != "https://default.example/v1" || def.Model != "default-model" {
		t.Errorf("默认客户端被改动了: url=%q model=%q", def.BaseURL, def.Model)
	}
}
