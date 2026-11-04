package feishu

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
)

// TestRetryConfigFlowsToResolvedConfig 钉住模型级重试配置的完整透传链:
// config.json (ProviderModelConfig) → ToModelConfigJSON → registry/resolver →
// ResolvedConfig → api.Client (由调用方映射)。
//
// 这条链上任何一环漏映射都不会报错: 字段安静地变成零值, 网关照常启动, 只是
// 重试配置不生效。所以判据必须是解析出口的值, 不是"结构体里有没有这个字段"。
func TestRetryConfigFlowsToResolvedConfig(t *testing.T) {
	jc := &JSONConfig{
		Providers: map[string]ProviderConfig{
			"p": {
				Name:    "p",
				BaseURL: "https://example.invalid/v1",
				APIKey:  "sk-test",
				Models: map[string]ProviderModelConfig{
					"p:m": {
						RetryCount:    3,
						RetryBaseSec:  2,
						RetryMaxSec:   20,
						RetryStatuses: []int{400, 502},
					},
				},
			},
		},
		AI: &AISection{ModelAlias: "p:m"},
	}

	cfg := jc.ToModelConfigJSON()
	_, resolver, err := modelconfig.LoadFromConfig(cfg)
	if err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	rc := resolver.ResolveAlias("p:m")
	if rc.RetryCount != 3 || rc.RetryBaseSec != 2 || rc.RetryMaxSec != 20 {
		t.Fatalf("重试标量未透传: %+v", rc)
	}
	if len(rc.RetryStatuses) != 2 || rc.RetryStatuses[0] != 400 || rc.RetryStatuses[1] != 502 {
		t.Fatalf("retryStatuses 未透传: %+v", rc.RetryStatuses)
	}
}

// TestRetryConfigNegativeMeansDisable 校验 -1 能穿过 omitempty 与解析链保留下来
// (0 会被当成"未配置", -1 必须原样到达 api.Client)。
func TestRetryConfigNegativeMeansDisable(t *testing.T) {
	jc := &JSONConfig{
		Providers: map[string]ProviderConfig{
			"p": {
				Name:    "p",
				BaseURL: "https://example.invalid/v1",
				APIKey:  "sk-test",
				Models: map[string]ProviderModelConfig{
					"p:m": {RetryCount: -1},
				},
			},
		},
	}
	cfg := jc.ToModelConfigJSON()
	if got := cfg.Providers["p"].Models["p:m"].RetryCount; got != -1 {
		t.Fatalf("retryCount=-1 应在转换中保留, got %d", got)
	}
	_, resolver, err := modelconfig.LoadFromConfig(cfg)
	if err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	if got := resolver.ResolveAlias("p:m").RetryCount; got != -1 {
		t.Fatalf("ResolvedConfig.RetryCount 应为 -1, got %d", got)
	}
}
