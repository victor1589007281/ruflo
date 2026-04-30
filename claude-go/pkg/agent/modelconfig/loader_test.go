package modelconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromBytes(t *testing.T) {
	jsonData := []byte(`{
		"providers": {
			"dashscope": {
				"name": "dashscope",
				"baseUrl": "https://dashscope.example.com/v1",
				"apiKey": "sk-dash",
				"models": {
					"dashscope:qwen3.6-plus": {
						"maxTokens": 32768,
						"promptCacheMode": "auto"
					}
				}
			}
		},
		"ai": {
			"defaultModelAlias": "dashscope:qwen3.6-plus",
			"defaultFallbackAliases": ["dashscope:qwen3.6-plus"],
			"defaultPromptCacheMode": "auto",
			"plans": {
				"development": {
					"modelAlias": "dashscope:qwen3.6-plus",
					"fallbackAliases": ["dashscope:qwen3.6-plus"],
					"roles": {
						"coder": {"modelAlias": "dashscope:qwen3.6-plus"}
					}
				}
			}
		}
	}`)

	registry, resolver, err := LoadFromBytes(jsonData)
	if err != nil {
		t.Fatalf("LoadFromBytes failed: %v", err)
	}

	if registry.GetProvider("dashscope") == nil {
		t.Fatal("expected dashscope provider")
	}

	cfg := resolver.Resolve("development", "coder")
	if cfg.Alias != "dashscope:qwen3.6-plus" {
		t.Fatalf("expected dashscope:qwen3.6-plus, got %s", cfg.Alias)
	}
	if cfg.MaxTokens != 32768 {
		t.Fatalf("expected maxTokens 32768, got %d", cfg.MaxTokens)
	}
}

func TestLoadFromJSON_File(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")
	data := []byte(`{
		"providers": {
			"minimax": {
				"name": "minimax",
				"baseUrl": "https://minimax.example.com/v1",
				"apiKey": "sk-mini",
				"models": {
					"minimax:MiniMax-M2.7": {"maxTokens": 8192}
				}
			}
		},
		"ai": {
			"defaultModelAlias": "minimax:MiniMax-M2.7",
			"plans": {}
		}
	}`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write temp file failed: %v", err)
	}

	registry, resolver, err := LoadFromJSON(path)
	if err != nil {
		t.Fatalf("LoadFromJSON failed: %v", err)
	}

	if registry.GetModel("minimax:MiniMax-M2.7") == nil {
		t.Fatal("expected minimax:MiniMax-M2.7 model")
	}

	cfg := resolver.Resolve("", "")
	if cfg.Alias != "minimax:MiniMax-M2.7" {
		t.Fatalf("expected minimax:MiniMax-M2.7, got %s", cfg.Alias)
	}
}

func TestReloadFromJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")
	initial := []byte(`{
		"providers": {
			"p1": {"name": "p1", "baseUrl": "https://p1.example.com", "apiKey": "k1",
				"models": {"p1:model-1": {"maxTokens": 100}}}
		},
		"ai": {"defaultModelAlias": "p1:model-1", "plans": {}}
	}`)
	if err := os.WriteFile(path, initial, 0644); err != nil {
		t.Fatalf("write temp file failed: %v", err)
	}

	registry, resolver, err := LoadFromJSON(path)
	if err != nil {
		t.Fatalf("LoadFromJSON failed: %v", err)
	}

	if resolver.Resolve("", "").Alias != "p1:model-1" {
		t.Fatal("expected p1:model-1 before reload")
	}

	updated := []byte(`{
		"providers": {
			"p2": {"name": "p2", "baseUrl": "https://p2.example.com", "apiKey": "k2",
				"models": {"p2:model-2": {"maxTokens": 200}}}
		},
		"ai": {"defaultModelAlias": "p2:model-2", "plans": {}}
	}`)
	if err := os.WriteFile(path, updated, 0644); err != nil {
		t.Fatalf("write updated file failed: %v", err)
	}

	if err := ReloadFromJSON(path, registry, resolver); err != nil {
		t.Fatalf("ReloadFromJSON failed: %v", err)
	}

	if registry.GetProvider("p1") != nil {
		t.Fatal("expected p1 to be removed after reload")
	}
	if resolver.Resolve("", "").Alias != "p2:model-2" {
		t.Fatalf("expected p2:model-2 after reload, got %s", resolver.Resolve("", "").Alias)
	}
}

func TestValidate(t *testing.T) {
	r := NewProviderRegistry()
	r.RegisterProvider(ProviderConfig{
		Name: "p1",
		Models: map[string]ModelConfig{
			"p1:model-1": {MaxTokens: 100},
		},
	})

	resolver := NewConfigResolver(r, GlobalConfig{
		DefaultModelAlias:      "p1:model-1",
		DefaultFallbackAliases: []string{"p1:model-1"},
	}, map[string]PlanConfig{
		"dev": {
			ModelAlias:      "p1:model-1",
			FallbackAliases: []string{"p1:model-1"},
			Roles:           map[string]RoleConfig{"coder": {ModelAlias: "p1:model-1"}},
		},
	})

	errs := Validate(r, resolver)
	if len(errs) > 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}

	// 测试无效 alias
	resolver2 := NewConfigResolver(r, GlobalConfig{
		DefaultModelAlias:      "invalid",
		DefaultFallbackAliases: []string{"invalid"},
	}, map[string]PlanConfig{
		"dev": {
			ModelAlias:      "invalid",
			FallbackAliases: []string{"invalid"},
			Roles:           map[string]RoleConfig{"coder": {ModelAlias: "invalid"}},
		},
	})

	errs = Validate(r, resolver2)
	// invalid 同时触发 "未注册" 和 "格式非法" 两个错误
	if len(errs) != 10 {
		t.Fatalf("expected 10 validation errors, got %d: %v", len(errs), errs)
	}
}
