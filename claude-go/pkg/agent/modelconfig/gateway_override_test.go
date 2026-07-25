package modelconfig

import "testing"

// 回归：CLAUDE_GO_LLM_GATEWAY 此前是死环境变量——deploy/k8s/distributed.yaml
// 给 control 与 worker 都注入了它，但全仓 Go 代码零读取，于是分布式拓扑里的
// gateway pod 是装饰品，各副本仍各自直连 provider。
func TestApplyGatewayOverride(t *testing.T) {
	cases := []struct {
		name, env, in, want string
	}{
		{"未设则不动", "", "https://api.moonshot.cn/anthropic", "https://api.moonshot.cn/anthropic"},
		{"设了则改指网关", "http://claude-go-gateway:18081", "https://api.moonshot.cn/anthropic", "http://claude-go-gateway:18081"},
		{"去掉尾部斜杠", "http://gw:18081/", "https://x/anthropic", "http://gw:18081"},
		{"两侧空白容错", "  http://gw:18081  ", "https://x/anthropic", "http://gw:18081"},
		{"BaseURL 为空时不注入", "http://gw:18081", "", ""},
		{"仅空白等于未设", "   ", "https://x/anthropic", "https://x/anthropic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(gatewayEnv, c.env)
			got := ApplyGatewayOverride(ResolvedConfig{BaseURL: c.in})
			if got.BaseURL != c.want {
				t.Errorf("BaseURL = %q, 期望 %q", got.BaseURL, c.want)
			}
		})
	}
}

// 只改 BaseURL，其余字段（尤其 APIKey 与 Model）必须原样透传：
// 网关负责向真实 provider 注入凭据，但按 model 路由需要 Model 字段完好。
// fallback 端点必须一并改指网关: 否则主端点走网关、降级直连 provider,
// 集中记账在最需要时失效。
func TestApplyGatewayOverride_fallback端点一并覆盖(t *testing.T) {
	t.Setenv(gatewayEnv, "http://gw:18081")
	got := ApplyGatewayOverride(ResolvedConfig{
		BaseURL: "https://a/anthropic", FallbackBaseURL: "https://b/anthropic",
	})
	if got.FallbackBaseURL != "http://gw:18081" {
		t.Errorf("FallbackBaseURL = %q, 期望改指网关", got.FallbackBaseURL)
	}
}

// 原本没有 fallback 端点的不应被凭空注入。
func TestApplyGatewayOverride_无fallback不注入(t *testing.T) {
	t.Setenv(gatewayEnv, "http://gw:18081")
	got := ApplyGatewayOverride(ResolvedConfig{BaseURL: "https://a/anthropic"})
	if got.FallbackBaseURL != "" {
		t.Errorf("FallbackBaseURL 被凭空注入: %q", got.FallbackBaseURL)
	}
}

func TestApplyGatewayOverride_只改BaseURL(t *testing.T) {
	t.Setenv(gatewayEnv, "http://gw:18081")
	in := ResolvedConfig{BaseURL: "https://x/anthropic", APIKey: "sk-real", ProviderName: "kimi-k2", MaxTokens: 4096}
	got := ApplyGatewayOverride(in)
	if got.APIKey != "sk-real" || got.ProviderName != "kimi-k2" || got.MaxTokens != 4096 {
		t.Errorf("非 BaseURL 字段被改动: %+v", got)
	}
}
