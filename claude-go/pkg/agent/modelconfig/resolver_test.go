package modelconfig

import (
	"reflect"
	"testing"
)

func buildTestRegistry() *ProviderRegistry {
	r := NewProviderRegistry()
	r.RegisterProvider(ProviderConfig{
		Name:    "dashscope",
		BaseURL: "https://dashscope.example.com/v1",
		APIKey:  "sk-dash",
		Models: map[string]ModelConfig{
			"dashscope:qwen3.6-plus": {MaxTokens: 32768, PromptCacheMode: "auto"},
			"dashscope:qwen3.5-plus": {MaxTokens: 16384, PromptCacheMode: "auto"},
			"dashscope:kimi-k2.5":    {MaxTokens: 32768, PromptCacheMode: "auto"},
			"dashscope:glm-5":        {MaxTokens: 8192, PromptCacheMode: "off"},
		},
	})
	r.RegisterProvider(ProviderConfig{
		Name:    "minimax",
		BaseURL: "https://minimax.example.com/v1",
		APIKey:  "sk-mini",
		Models: map[string]ModelConfig{
			"minimax:MiniMax-M2.7-highspeed": {MaxTokens: 16384},
			"minimax:MiniMax-M2.7":           {MaxTokens: 8192},
		},
	})
	return r
}

func TestConfigResolver_Priority_GlobalOnly(t *testing.T) {
	r := buildTestRegistry()
	resolver := NewConfigResolver(r, GlobalConfig{
		DefaultModelAlias:      "dashscope:qwen3.6-plus",
		DefaultFallbackAliases: []string{"dashscope:qwen3.5-plus", "dashscope:glm-5"},
		DefaultPromptCacheMode: "auto",
	}, nil)

	cfg := resolver.Resolve("", "")
	if cfg.Alias != "dashscope:qwen3.6-plus" {
		t.Fatalf("expected alias dashscope:qwen3.6-plus, got %s", cfg.Alias)
	}
	if cfg.ProviderName != "qwen3.6-plus" {
		t.Fatalf("expected providerName qwen3.6-plus, got %s", cfg.ProviderName)
	}
	if cfg.BaseURL != "https://dashscope.example.com/v1" {
		t.Fatalf("expected dashscope URL, got %s", cfg.BaseURL)
	}
	if !reflect.DeepEqual(cfg.FallbackModels, []string{"qwen3.5-plus", "glm-5"}) {
		t.Fatalf("expected fallback [qwen3.5-plus glm-5], got %v", cfg.FallbackModels)
	}
	if cfg.PromptCacheMode != "auto" {
		t.Fatalf("expected promptCacheMode auto, got %s", cfg.PromptCacheMode)
	}
}

func TestConfigResolver_Priority_PlanOverridesGlobal(t *testing.T) {
	r := buildTestRegistry()
	resolver := NewConfigResolver(r, GlobalConfig{
		DefaultModelAlias:      "dashscope:qwen3.6-plus",
		DefaultFallbackAliases: []string{"dashscope:qwen3.5-plus"},
	}, map[string]PlanConfig{
		"development": {
			ModelAlias:      "minimax:MiniMax-M2.7-highspeed",
			FallbackAliases: []string{"minimax:MiniMax-M2.7"},
		},
	})

	cfg := resolver.Resolve("development", "")
	if cfg.Alias != "minimax:MiniMax-M2.7-highspeed" {
		t.Fatalf("expected alias minimax:MiniMax-M2.7-highspeed, got %s", cfg.Alias)
	}
	if cfg.ProviderName != "MiniMax-M2.7-highspeed" {
		t.Fatalf("expected providerName MiniMax-M2.7-highspeed, got %s", cfg.ProviderName)
	}
	if cfg.BaseURL != "https://minimax.example.com/v1" {
		t.Fatalf("expected minimax URL, got %s", cfg.BaseURL)
	}
	if !reflect.DeepEqual(cfg.FallbackModels, []string{"MiniMax-M2.7"}) {
		t.Fatalf("expected fallback [MiniMax-M2.7], got %v", cfg.FallbackModels)
	}
}

func TestConfigResolver_Priority_RoleOverridesPlan(t *testing.T) {
	r := buildTestRegistry()
	resolver := NewConfigResolver(r, GlobalConfig{
		DefaultModelAlias:      "dashscope:qwen3.6-plus",
		DefaultFallbackAliases: []string{"dashscope:qwen3.5-plus"},
	}, map[string]PlanConfig{
		"development": {
			ModelAlias:      "minimax:MiniMax-M2.7-highspeed",
			FallbackAliases: []string{"minimax:MiniMax-M2.7"},
			Roles: map[string]RoleConfig{
				"planner": {ModelAlias: "dashscope:qwen3.6-plus", FallbackAliases: []string{"dashscope:kimi-k2.5"}},
			},
		},
	})

	// 无 role: 用 plan 级别
	cfg := resolver.Resolve("development", "")
	if cfg.Alias != "minimax:MiniMax-M2.7-highspeed" {
		t.Fatalf("expected plan alias, got %s", cfg.Alias)
	}

	// 有 role: 用 role 级别
	cfg = resolver.Resolve("development", "planner")
	if cfg.Alias != "dashscope:qwen3.6-plus" {
		t.Fatalf("expected role alias dashscope:qwen3.6-plus, got %s", cfg.Alias)
	}
	if !reflect.DeepEqual(cfg.FallbackModels, []string{"kimi-k2.5"}) {
		t.Fatalf("expected role fallback [kimi-k2.5], got %v", cfg.FallbackModels)
	}
}

func TestConfigResolver_FallbackCrossProvider(t *testing.T) {
	r := buildTestRegistry()
	resolver := NewConfigResolver(r, GlobalConfig{
		DefaultModelAlias:      "dashscope:qwen3.6-plus",
		DefaultFallbackAliases: []string{"minimax:MiniMax-M2.7-highspeed"},
	}, nil)

	cfg := resolver.Resolve("", "")
	// fallback 包含 minimax 的模型，应该解析为 minimax 的 providerName
	if len(cfg.FallbackModels) != 1 || cfg.FallbackModels[0] != "MiniMax-M2.7-highspeed" {
		t.Fatalf("expected cross-provider fallback, got %v", cfg.FallbackModels)
	}
	// 跨 provider fallback 应自动填充 FallbackBaseURL / FallbackAPIKey
	if cfg.FallbackBaseURL != "https://minimax.example.com/v1" {
		t.Fatalf("expected fallback baseURL from minimax, got %s", cfg.FallbackBaseURL)
	}
	if cfg.FallbackAPIKey != "sk-mini" {
		t.Fatalf("expected fallback apiKey from minimax, got %s", cfg.FallbackAPIKey)
	}
}

func TestConfigResolver_FallbackSameProvider(t *testing.T) {
	r := buildTestRegistry()
	resolver := NewConfigResolver(r, GlobalConfig{
		DefaultModelAlias:      "dashscope:qwen3.6-plus",
		DefaultFallbackAliases: []string{"dashscope:qwen3.5-plus"},
	}, nil)

	cfg := resolver.Resolve("", "")
	if len(cfg.FallbackModels) != 1 || cfg.FallbackModels[0] != "qwen3.5-plus" {
		t.Fatalf("expected same-provider fallback, got %v", cfg.FallbackModels)
	}
	// 同 provider fallback 不应填充 FallbackBaseURL / FallbackAPIKey
	if cfg.FallbackBaseURL != "" {
		t.Fatalf("expected empty fallback baseURL for same provider, got %s", cfg.FallbackBaseURL)
	}
	if cfg.FallbackAPIKey != "" {
		t.Fatalf("expected empty fallback apiKey for same provider, got %s", cfg.FallbackAPIKey)
	}
}

func TestConfigResolver_UnknownAlias(t *testing.T) {
	r := NewProviderRegistry()
	resolver := NewConfigResolver(r, GlobalConfig{
		DefaultModelAlias: "unknown:model",
	}, nil)

	cfg := resolver.Resolve("", "")
	if cfg.Alias != "unknown:model" {
		t.Fatalf("expected alias unknown:model, got %s", cfg.Alias)
	}
	if cfg.ProviderName != "model" {
		t.Fatalf("expected providerName 'model' parsed from alias, got %s", cfg.ProviderName)
	}
}

func TestConfigResolver_DynamicUpdate(t *testing.T) {
	r := buildTestRegistry()
	resolver := NewConfigResolver(r, GlobalConfig{
		DefaultModelAlias: "dashscope:qwen3.6-plus",
	}, map[string]PlanConfig{
		"dev": {ModelAlias: "minimax:MiniMax-M2.7-highspeed"},
	})

	cfg := resolver.Resolve("dev", "")
	if cfg.Alias != "minimax:MiniMax-M2.7-highspeed" {
		t.Fatalf("expected minimax before update, got %s", cfg.Alias)
	}

	// 动态更新 plan
	resolver.SetPlans(map[string]PlanConfig{
		"dev": {ModelAlias: "dashscope:qwen3.6-plus"},
	})

	cfg = resolver.Resolve("dev", "")
	if cfg.Alias != "dashscope:qwen3.6-plus" {
		t.Fatalf("expected qwen after update, got %s", cfg.Alias)
	}
}
