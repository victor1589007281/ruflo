package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/feishu"
)

func TestNormalizeProviderModelAlias(t *testing.T) {
	tests := map[string]string{
		"dashscope:qwen3.6-plus": "qwen3.6-plus",
		"minimax:MiniMax-M2.7":   "MiniMax-M2.7",
		"qwen3.6-plus":           "qwen3.6-plus",
		"":                       "",
	}
	for input, want := range tests {
		if got := normalizeProviderModelAlias(input); got != want {
			t.Fatalf("normalizeProviderModelAlias(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveRuntimeModelConfigUsesProviderAlias(t *testing.T) {
	cfg := &feishu.JSONConfig{
		Providers: feishu.ProvidersSection{
			"dashscope": {
				Name:    "dashscope",
				BaseURL: "https://dashscope.example/v1",
				APIKey:  "dash-key",
				Models: map[string]feishu.ProviderModelConfig{
					"dashscope:qwen3.6-plus": {
						MaxTokens:     65536,
						ContextWindow: 1000000,
					},
				},
			},
		},
		AI: &feishu.AISection{
			ModelAlias: "dashscope:qwen3.6-plus",
		},
	}

	resolved, ok, err := resolveRuntimeModelConfig(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected model config to resolve")
	}
	if resolved.Provider != "dashscope" || resolved.ProviderName != "qwen3.6-plus" {
		t.Fatalf("unexpected resolved provider/model: %+v", resolved)
	}
	if resolved.BaseURL != "https://dashscope.example/v1" || resolved.APIKey != "dash-key" {
		t.Fatalf("unexpected endpoint: %+v", resolved)
	}
	if resolved.MaxTokens != 65536 || resolved.ContextWindow != 1000000 {
		t.Fatalf("model params not propagated: %+v", resolved)
	}
}

func TestResolveRuntimeModelConfigNormalizesCLIProviderAlias(t *testing.T) {
	cfg := &feishu.JSONConfig{
		Providers: feishu.ProvidersSection{
			"dashscope": {
				Name:    "dashscope",
				BaseURL: "https://dashscope.example/v1",
				APIKey:  "dash-key",
				Models: map[string]feishu.ProviderModelConfig{
					"dashscope:qwen3.6-plus": {},
				},
			},
		},
		AI: &feishu.AISection{ModelAlias: "dashscope:qwen3.6-plus"},
	}

	resolved, ok, err := resolveRuntimeModelConfig(cfg, "dashscope:qwen3.6-plus")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || resolved.ProviderName != "qwen3.6-plus" {
		t.Fatalf("expected provider alias to normalize, got ok=%v resolved=%+v", ok, resolved)
	}
}

func TestResolveRuntimeModelConfigUsesDefaultProviderForRawCLIModel(t *testing.T) {
	cfg := &feishu.JSONConfig{
		Providers: feishu.ProvidersSection{
			"dashscope": {
				Name:    "dashscope",
				BaseURL: "https://dashscope.example/v1",
				APIKey:  "dash-key",
				Models: map[string]feishu.ProviderModelConfig{
					"dashscope:qwen3.6-plus": {},
				},
			},
		},
		AI: &feishu.AISection{ModelAlias: "dashscope:qwen3.6-plus"},
	}

	resolved, ok, err := resolveRuntimeModelConfig(cfg, "qwen3.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || resolved.ProviderName != "qwen3.5-flash" || resolved.BaseURL == "" {
		t.Fatalf("expected raw model to inherit default provider endpoint, got ok=%v resolved=%+v", ok, resolved)
	}
}

func TestBuildEngineUsesRuntimeConfigProviders(t *testing.T) {
	restore := snapshotGlobalFlags()
	defer restore()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("DASHSCOPE_API_KEY", "")
	t.Setenv("API_BASE_URL", "")

	workDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{
  "cwd": "`+filepath.ToSlash(workDir)+`",
  "permissionMode": "auto",
  "providers": {
    "dashscope": {
      "name": "dashscope",
      "baseUrl": "https://dashscope.example/v1",
      "apiKey": "dash-key",
      "models": {
        "dashscope:qwen3.6-plus": {
          "maxTokens": 65536,
          "maxTurns": 7,
          "promptCacheMode": "off",
          "contextWindow": 1000000
        }
      }
    }
  },
  "ai": {"modelAlias": "dashscope:qwen3.6-plus"}
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	flagConfig = cfgPath
	flagModel = "qwen3.5-plus"
	flagAPIKey = ""
	flagBaseURL = ""
	flagMaxTokens = 16384
	flagMaxTurns = 0
	flagPermission = "bypass"
	flagSystemPrompt = ""
	flagMCPConfig = ""
	flagDebug = false
	flagPrint = false

	eng, err := buildEngine()
	if err != nil {
		t.Fatal(err)
	}
	if eng.APIClient.Model != "qwen3.6-plus" {
		t.Fatalf("expected provider model name, got %q", eng.APIClient.Model)
	}
	if eng.APIClient.BaseURL != "https://dashscope.example/v1" || eng.APIClient.APIKey != "dash-key" {
		t.Fatalf("unexpected client endpoint: base=%q key=%q", eng.APIClient.BaseURL, eng.APIClient.APIKey)
	}
	if eng.APIClient.PromptCacheMode != "off" {
		t.Fatalf("expected prompt cache mode from model config, got %q", eng.APIClient.PromptCacheMode)
	}
	if eng.Config.Cwd != workDir || eng.Config.MaxTokens != 65536 || eng.Config.MaxTurns != 7 || eng.Config.ContextWindow != 1000000 {
		t.Fatalf("config values not applied: %+v", eng.Config)
	}
}

func snapshotGlobalFlags() func() {
	old := struct {
		model        string
		apiKey       string
		baseURL      string
		maxTokens    int
		maxTurns     int
		permission   string
		systemPrompt string
		print        bool
		debug        bool
		mcpConfig    string
		config       string
		resume       string
		continueFlag bool
	}{
		flagModel, flagAPIKey, flagBaseURL, flagMaxTokens, flagMaxTurns,
		flagPermission, flagSystemPrompt, flagPrint, flagDebug, flagMCPConfig,
		flagConfig, flagResume, flagContinue,
	}
	return func() {
		flagModel = old.model
		flagAPIKey = old.apiKey
		flagBaseURL = old.baseURL
		flagMaxTokens = old.maxTokens
		flagMaxTurns = old.maxTurns
		flagPermission = old.permission
		flagSystemPrompt = old.systemPrompt
		flagPrint = old.print
		flagDebug = old.debug
		flagMCPConfig = old.mcpConfig
		flagConfig = old.config
		flagResume = old.resume
		flagContinue = old.continueFlag
	}
}
