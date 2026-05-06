package feishu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadJSONConfigFromClaudeGoConfigEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{
  "cwd": "/tmp/work",
  "providers": {
    "dashscope": {
      "name": "dashscope",
      "baseUrl": "https://dashscope.example/v1",
      "apiKey": "key",
      "models": {
        "dashscope:qwen3.6-plus": {"maxTokens": 65536}
      }
    }
  },
  "ai": {"modelAlias": "dashscope:qwen3.6-plus"}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_GO_CONFIG", cfgPath)

	cfg, err := LoadJSONConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.Cwd != "/tmp/work" {
		t.Fatalf("expected config from env, got %#v", cfg)
	}
	if cfg.AI == nil || cfg.AI.ModelAlias != "dashscope:qwen3.6-plus" {
		t.Fatalf("expected ai model alias, got %#v", cfg.AI)
	}
}
