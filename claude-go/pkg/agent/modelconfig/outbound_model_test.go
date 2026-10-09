package modelconfig

import "testing"

// 直连 provider: 出站必须是**裸厂商模型名**。
//
// providers 的 baseUrl 已经指向具体厂商, 厂商端点只认自己的模型 ID。
// 把 "opencode:deepseek-v4.1-flash" 直发 opencode 会被 401 ModelError 拒 ——
// 这是硬失败, 不是"少了个标签"。
func TestOutboundModelName_DirectIsBare(t *testing.T) {
	c := ResolvedConfig{Alias: "opencode:deepseek-v4.1-flash", ProviderName: "deepseek-v4.1-flash"}
	if got := c.OutboundModelName(); got != "deepseek-v4.1-flash" {
		t.Errorf("直连应发裸名, got %q", got)
	}
}

// 经网关: 出站给完整别名 (网关的查找键)。
func TestOutboundModelName_ViaGatewayIsAlias(t *testing.T) {
	c := ResolvedConfig{Alias: "opencode:deepseek-v4.1-flash", ProviderName: "deepseek-v4.1-flash", ViaGateway: true}
	if got := c.OutboundModelName(); got != "opencode:deepseek-v4.1-flash" {
		t.Errorf("经网关应发完整别名, got %q", got)
	}
}

// 无别名的裸配置: 两种链路形态下都只能发 ProviderName (没别的可发)。
func TestOutboundModelName_NoAliasFallsBack(t *testing.T) {
	for _, via := range []bool{false, true} {
		c := ResolvedConfig{ProviderName: "gemma4:26b-a4b-it-qat", ViaGateway: via}
		if got := c.OutboundModelName(); got != "gemma4:26b-a4b-it-qat" {
			t.Errorf("(ViaGateway=%v) 无别名应发 ProviderName, got %q", via, got)
		}
	}
}

// ApplyGatewayOverride 置位 ViaGateway —— 这是判据的唯一来源。
// env 未设时**不得**置位, 否则直连进程会开始发别名并被厂商拒。
func TestApplyGatewayOverride_SetsViaGateway(t *testing.T) {
	base := ResolvedConfig{Alias: "kimi:kimi-for-coding", ProviderName: "kimi-for-coding", BaseURL: "https://api.kimi.com/coding/v1"}

	t.Setenv(gatewayEnv, "")
	off := ApplyGatewayOverride(base)
	if off.ViaGateway {
		t.Error("未设网关地址时不该置位 ViaGateway")
	}
	if off.OutboundModelName() != "kimi-for-coding" {
		t.Errorf("直连应发裸名, got %q", off.OutboundModelName())
	}

	t.Setenv(gatewayEnv, "http://claude-go-gateway:18081")
	on := ApplyGatewayOverride(base)
	if !on.ViaGateway {
		t.Fatal("设了网关地址应置位 ViaGateway")
	}
	if on.OutboundModelName() != "kimi:kimi-for-coding" {
		t.Errorf("经网关应发别名, got %q", on.OutboundModelName())
	}
}

// BaseURL 为空时覆盖不发生 (ApplyGatewayOverride 会提前返回),
// ViaGateway 也必须**保持 false** —— 否则就成了"env 设了但请求仍直连"
// 却自称经网关, 下游会照着发别名然后被拒。
func TestApplyGatewayOverride_NoOverrideWhenBaseURLEmpty(t *testing.T) {
	t.Setenv(gatewayEnv, "http://claude-go-gateway:18081")
	got := ApplyGatewayOverride(ResolvedConfig{Alias: "a:b", ProviderName: "b"})
	if got.ViaGateway {
		t.Error("BaseURL 为空时覆盖不发生, ViaGateway 不应置位")
	}
}

// Resolve 出口的配置也要带上正确的 ViaGateway, 而不是只有直接调用
// ApplyGatewayOverride 的路径才有 —— resolver 的每个出口都过它。
func TestResolve_OutboundModelNameFollowsLinkShape(t *testing.T) {
	r := buildTestRegistry()
	resolver := NewConfigResolver(r, GlobalConfig{DefaultModelAlias: "dashscope:qwen3.6-plus"}, nil)

	t.Setenv(gatewayEnv, "")
	direct := resolver.Resolve("plan", "role")
	if got := direct.OutboundModelName(); got != "qwen3.6-plus" {
		t.Errorf("直连应发裸名, got %q", got)
	}

	t.Setenv(gatewayEnv, "http://gw:18081")
	viaGW := resolver.Resolve("plan", "role")
	if got := viaGW.OutboundModelName(); got != "dashscope:qwen3.6-plus" {
		t.Errorf("经网关应发别名, got %q", got)
	}
}
