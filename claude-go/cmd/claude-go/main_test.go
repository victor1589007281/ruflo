package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/sandbox"
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
	defer sandbox.ResetConfigForTest()
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

func TestBuildEngineAppliesSandboxConfig(t *testing.T) {
	restore := snapshotGlobalFlags()
	defer restore()
	defer sandbox.ResetConfigForTest()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("DASHSCOPE_API_KEY", "")
	t.Setenv("API_BASE_URL", "")

	workDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{
  "cwd": "`+filepath.ToSlash(workDir)+`",
  "stateDir": "`+filepath.ToSlash(filepath.Join(workDir, ".claude-go"))+`",
  "providers": {
    "dashscope": {
      "name": "dashscope",
      "baseUrl": "https://dashscope.example/v1",
      "apiKey": "dash-key",
      "models": {"dashscope:qwen3.6-plus": {}}
    }
  },
  "ai": {"modelAlias": "dashscope:qwen3.6-plus"},
  "sandbox": {
    "mode": "docker",
    "requiredForTeam": false,
    "allowUnsafeFallback": true,
    "outputMaxBytes": 12345,
    "previewMaxBytes": 2345,
    "logMaxBytes": 34567,
    "pidsMax": 77,
    "docker": {"image": "golang:1.24", "tempSize": "256m"}
  }
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

	if _, err := buildEngine(); err != nil {
		t.Fatal(err)
	}
	cfg := sandbox.CurrentConfig()
	if cfg.Mode != "docker" || cfg.RequiredForTeam || !cfg.AllowUnsafeFallback {
		t.Fatalf("sandbox config not applied: %+v", cfg)
	}
	if cfg.OutputMaxBytes != 12345 || cfg.PreviewMaxBytes != 2345 || cfg.LogMaxBytes != 34567 || cfg.PidsMax != 77 {
		t.Fatalf("sandbox limits not applied: %+v", cfg)
	}
	if cfg.Docker.TempSize != "256m" || cfg.Docker.Image != "golang:1.24" {
		t.Fatalf("docker config not applied: %+v", cfg.Docker)
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

func TestParseAllowedTools(t *testing.T) {
	if parseAllowedTools("") != nil {
		t.Fatal("empty input must yield nil (no whitelist)")
	}
	// A set-but-blank flag is deny-all (non-nil empty set), NOT nil: a template
	// that renders blank must fail closed instead of exposing every tool.
	if got := parseAllowedTools("  ,  "); got == nil || len(got) != 0 {
		t.Fatalf("blank-only input must yield a non-nil empty set (deny-all), got %v", got)
	}
	set := parseAllowedTools("sql_query, submit_finding ,heartbeat")
	for _, want := range []string{"sql_query", "submit_finding", "heartbeat"} {
		if !set[want] {
			t.Fatalf("expected %q in whitelist set %v", want, set)
		}
	}
	if len(set) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(set))
	}
}

func TestClassifyRunError(t *testing.T) {
	cases := map[string]string{
		"context deadline exceeded":    "timeout",
		"HTTP 429 rate limit reached":  "rate_limit",
		"model overloaded (529)":       "overloaded",
		"prompt is too long for model": "prompt_too_long",
		"some unknown failure":         "error",
	}
	for msg, want := range cases {
		if got := classifyRunError(fmt.Errorf("%s", msg)); got != want {
			t.Fatalf("classifyRunError(%q)=%q, want %q", msg, got, want)
		}
	}
	if classifyRunError(nil) != "" {
		t.Fatal("nil error must classify to empty string")
	}
}
