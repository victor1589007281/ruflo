package modelconfig

import (
	"testing"
)

func TestProviderRegistry_RegisterAndGet(t *testing.T) {
	r := NewProviderRegistry()

	r.RegisterProvider(ProviderConfig{
		Name:    "dashscope",
		BaseURL: "https://dashscope.example.com/v1",
		APIKey:  "sk-dash",
		Models: map[string]ModelConfig{
			"dashscope:qwen3.6-plus": {MaxTokens: 32768},
			"dashscope:qwen3.5-plus": {MaxTokens: 16384},
		},
	})

	r.RegisterProvider(ProviderConfig{
		Name:    "minimax",
		BaseURL: "https://minimax.example.com/v1",
		APIKey:  "sk-mini",
		Models: map[string]ModelConfig{
			"minimax:MiniMax-M2.7": {MaxTokens: 8192},
		},
	})

	// 测试 provider 查询
	p := r.GetProvider("dashscope")
	if p == nil || p.BaseURL != "https://dashscope.example.com/v1" {
		t.Fatalf("expected dashscope provider, got %+v", p)
	}

	// 测试 alias 查询
	entry := r.GetModel("dashscope:qwen3.6-plus")
	if entry == nil || entry.ProviderName != "qwen3.6-plus" {
		t.Fatalf("expected qwen3.6-plus, got %+v", entry)
	}
	if entry.Provider.Name != "dashscope" {
		t.Fatalf("expected provider dashscope, got %s", entry.Provider.Name)
	}

	// 测试 providerName 反查
	if got := r.LookupProviderName("dashscope:qwen3.6-plus"); got != "qwen3.6-plus" {
		t.Fatalf("LookupProviderName: expected qwen3.6-plus, got %s", got)
	}

	// 测试 baseURL/apiKey 反查
	if got := r.LookupBaseURL("minimax:MiniMax-M2.7"); got != "https://minimax.example.com/v1" {
		t.Fatalf("LookupBaseURL: expected minimax URL, got %s", got)
	}
	if got := r.LookupAPIKey("minimax:MiniMax-M2.7"); got != "sk-mini" {
		t.Fatalf("LookupAPIKey: expected sk-mini, got %s", got)
	}

	// 测试未知 alias 兜底
	if got := r.LookupProviderName("unknown"); got != "unknown" {
		t.Fatalf("expected fallback to raw name, got %s", got)
	}
}

func TestProviderRegistry_Reload(t *testing.T) {
	r := NewProviderRegistry()
	r.RegisterProvider(ProviderConfig{
		Name:    "old",
		BaseURL: "https://old.example.com",
		Models:  map[string]ModelConfig{"old:old-model": {MaxTokens: 100}},
	})

	r.Reload([]ProviderConfig{
		{
			Name:    "new",
			BaseURL: "https://new.example.com",
			Models:  map[string]ModelConfig{"new:new-model": {MaxTokens: 200}},
		},
	})

	if r.GetProvider("old") != nil {
		t.Fatal("expected old provider to be removed after reload")
	}
	if r.GetModel("old:old-model") != nil {
		t.Fatal("expected old-model to be removed after reload")
	}
	if r.GetModel("new:new-model") == nil {
		t.Fatal("expected new-model to exist after reload")
	}
}

func TestProviderRegistry_AllAliases(t *testing.T) {
	r := NewProviderRegistry()
	r.RegisterProvider(ProviderConfig{
		Name: "p1",
		Models: map[string]ModelConfig{
			"p1:a": {},
			"p1:b": {},
		},
	})

	aliases := r.AllAliases()
	if len(aliases) != 2 {
		t.Fatalf("expected 2 aliases, got %d: %v", len(aliases), aliases)
	}
}
