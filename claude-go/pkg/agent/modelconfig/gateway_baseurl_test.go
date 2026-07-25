package modelconfig

import "testing"

// GatewayBaseURL 是环境变量名的唯一读取点 (design/02 §3.1)。
// 不经 ConfigResolver 的出站端点 (dashboard 的 env 直通路径) 靠它复用同一规则。
func TestGatewayBaseURL_规范化(t *testing.T) {
	cases := []struct{ env, want string }{
		{"", ""},
		{"   ", ""},
		{"http://gw:18081", "http://gw:18081"},
		{"http://gw:18081/", "http://gw:18081"},
		{"http://gw:18081///", "http://gw:18081"},
		{"  http://gw:18081/  ", "http://gw:18081"},
	}
	for _, c := range cases {
		t.Setenv(gatewayEnv, c.env)
		if got := GatewayBaseURL(); got != c.want {
			t.Errorf("env=%q → %q, 期望 %q", c.env, got, c.want)
		}
	}
}

// 与 ApplyGatewayOverride 必须给出一致的结果 —— 两处若各自 TrimRight,
// 迟早出现一处改了另一处没改。
func TestGatewayBaseURL_与ApplyGatewayOverride一致(t *testing.T) {
	t.Setenv(gatewayEnv, "http://gw:18081/")
	got := ApplyGatewayOverride(ResolvedConfig{BaseURL: "https://provider/v1"})
	if got.BaseURL != GatewayBaseURL() {
		t.Errorf("ApplyGatewayOverride=%q 与 GatewayBaseURL=%q 不一致", got.BaseURL, GatewayBaseURL())
	}
}
