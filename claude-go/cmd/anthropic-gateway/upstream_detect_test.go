package main

// upstream_detect_test.go — 钉住 anthropic-gateway 的 opencode 上游判据。
//
// # 钉的是什么
//
// 判据本身已移进 api.IsOpenCodeUpstream, 本文件不复测那段逻辑。
// 这里钉的是一个**调用点错误**, 它比逻辑错误隐蔽得多:
//
//	isOpenCodeUpstream(rc) 内部的字段曾写成 rc.ProviderName,
//	而 ResolvedConfig 里 ProviderName 装的是**模型名** ("deepseek-v4.1-flash"),
//	provider 名在同结构的 Provider 字段。
//
// 后果: 判据恒为假。三个 x-opencode-session 注入点全部存在、能编译、能 grep 到,
// 但一次也没执行过 —— 上游 400 MissingSessionID, 而代码看起来完全正常。
// 这类缺陷只能靠**从真实配置解析出的 ResolvedConfig** 去断言, 手搓一个
// ResolvedConfig{Provider:"opencode"} 是测不出来的: 它绕过了真正会出错的
// 字段填充路径 (config → feishu → modelconfig → resolver)。
//
// 所以下面用真实形状的 config JSON 走一遍 LoadFromConfig, 再用 isOpenCodeUpstream
// 断言 —— 这正是生产启动时的那条路径。

import (
	"encoding/json"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/feishu"
)

// opencodeShapedConfig 是 config.json 里 opencode provider 的真实形状。
const opencodeShapedConfig = `{
  "stateDir": "/tmp/none",
  "cwd": "/tmp/none",
  "providers": {
    "opencode": {
      "name": "opencode",
      "baseUrl": "https://opencode.ai/zen/go/v1",
      "apiKey": "sk-test",
      "models": {
        "opencode:deepseek-v4.1-flash": {"maxTokens": 65536, "contextWindow": 1048576}
      }
    }
  }
}`

// loadShapedResolver 走 production 的加载路径 (config JSON → feishu → modelconfig)。
func loadShapedResolver(t *testing.T) *modelconfig.ConfigResolver {
	t.Helper()
	var jc feishu.JSONConfig
	if err := json.Unmarshal([]byte(opencodeShapedConfig), &jc); err != nil {
		t.Fatalf("解析测试配置失败: %v", err)
	}
	_, res, err := modelconfig.LoadFromConfig(jc.ToModelConfigJSON())
	if err != nil {
		t.Fatalf("构建 resolver 失败: %v", err)
	}
	return res
}

// TestIsOpenCodeUpstream_RealResolvedConfig 是本次修复的核心回归。
//
// 断言的对象是**解析出来的 ResolvedConfig**, 不是手搓结构体 —— 这正是原缺陷
// 能藏住的地方。若判据再退回读 ProviderName, 这条会红 (ProviderName 是
// "deepseek-v4.1-flash", 既不是 opencode 也不匹配主机名)。
func TestIsOpenCodeUpstream_RealResolvedConfig(t *testing.T) {
	res := loadShapedResolver(t)
	rc := res.ResolveAlias("opencode:deepseek-v4.1-flash")

	// 先自证夹具有效: 两个字段确实是不同的值。若哪天它们相同, 本测试就退化成
	// 恒真 —— 一个测不出"传错字段"的测试比没有更危险。
	if rc.Provider != "opencode" {
		t.Fatalf("夹具失效: Provider 应为 opencode, 实际 %q", rc.Provider)
	}
	if rc.ProviderName == rc.Provider {
		t.Fatalf("夹具失效: ProviderName 与 Provider 同值 (%q), 本测试无法区分传错字段",
			rc.ProviderName)
	}

	if !isOpenCodeUpstream(rc) {
		t.Fatalf("真实配置解析出的 opencode 模型应识别为 opencode 上游 "+
			"(Provider=%q ProviderName=%q BaseURL=%q) —— 判据为假意味着 "+
			"三个会话头注入点全都不执行, 上游会 400 MissingSessionID",
			rc.Provider, rc.ProviderName, rc.BaseURL)
	}
}

// TestIsOpenCodeUpstream_GatewayOverride 钉住"经网关改指后仍认得出"。
//
// 进程设了 CLAUDE_GO_LLM_GATEWAY 时, ApplyGatewayOverride 把 BaseURL 改指网关
// 自己, 主机名就不再是 opencode.ai。此时只剩 provider 名能证明上游身份。
//
// 网关进程通常不设这个变量, 但"通常"是部署事实而非类型不变量 —— 启一个带着
// 该变量继承下来的子进程 (systemd unit / 容器 envFrom) 就会同时踩中这两件事。
func TestIsOpenCodeUpstream_GatewayOverride(t *testing.T) {
	t.Setenv(modelconfig.GatewayEnvName(), "http://claude-go-gateway:18081")
	res := loadShapedResolver(t)
	rc := res.ResolveAlias("opencode:deepseek-v4.1-flash")

	if rc.BaseURL == "https://opencode.ai/zen/go/v1" {
		t.Fatalf("夹具失效: 设了网关地址后 BaseURL 应已改指, 仍是 %q", rc.BaseURL)
	}
	if !isOpenCodeUpstream(rc) {
		t.Fatalf("BaseURL 被改指网关后仍应凭 provider 名认出 opencode 上游 "+
			"(Provider=%q BaseURL=%q)", rc.Provider, rc.BaseURL)
	}
}

// TestIsOpenCodeUpstream_NonOpencodeProvider 是反方向对照。
//
// 没有它, 一个无条件返回 true 的判据能通过上面两条 —— 而那会把 opencode 专有的
// 会话头带进 kimi 等无关上游。
func TestIsOpenCodeUpstream_NonOpencodeProvider(t *testing.T) {
	rc := modelconfig.ResolvedConfig{
		Provider: "kimi", ProviderName: "k3", BaseURL: "https://api.moonshot.cn/anthropic",
	}
	if isOpenCodeUpstream(rc) {
		t.Fatal("kimi 上游不应被识别为 opencode")
	}
}
