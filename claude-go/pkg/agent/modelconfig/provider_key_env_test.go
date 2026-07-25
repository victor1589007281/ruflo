package modelconfig

import "testing"

// 回归：deploy/k8s 的 ConfigMap 里 apiKey 写的是 PLACEHOLDER_OVERRIDE_VIA_ENV、
// Deployment 从 Secret 注入 KIMI_API_KEY，但此前全仓没有代码读 <PROVIDER>_API_KEY
// ⇒ 占位符原样进了 provider 配置，Pod 拿着假 key 调 LLM。
func TestProviderKeyEnv_名字映射(t *testing.T) {
	for in, want := range map[string]string{
		"kimi":         "KIMI_API_KEY",
		"deepseek":     "DEEPSEEK_API_KEY",
		"azure-openai": "AZURE_OPENAI_API_KEY", // 连字符转下划线
		"gpt4o":        "GPT4O_API_KEY",
		"Mixed_Case":   "MIXED_CASE_API_KEY",
	} {
		if got := providerKeyEnv(in); got != want {
			t.Errorf("providerKeyEnv(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestApplyProviderKeyFromEnv(t *testing.T) {
	t.Run("环境变量覆盖占位符", func(t *testing.T) {
		t.Setenv("KIMI_API_KEY", "sk-from-secret")
		got := applyProviderKeyFromEnv(ProviderConfig{Name: "kimi", APIKey: "PLACEHOLDER_OVERRIDE_VIA_ENV"})
		if got.APIKey != "sk-from-secret" {
			t.Errorf("APIKey = %q, 期望取自环境变量", got.APIKey)
		}
	})
	t.Run("环境变量未设则保留配置值", func(t *testing.T) {
		t.Setenv("KIMI_API_KEY", "")
		got := applyProviderKeyFromEnv(ProviderConfig{Name: "kimi", APIKey: "sk-from-config"})
		if got.APIKey != "sk-from-config" {
			t.Errorf("APIKey = %q, 期望保留配置值", got.APIKey)
		}
	})
	t.Run("仅空白等于未设", func(t *testing.T) {
		t.Setenv("KIMI_API_KEY", "   ")
		got := applyProviderKeyFromEnv(ProviderConfig{Name: "kimi", APIKey: "sk-from-config"})
		if got.APIKey != "sk-from-config" {
			t.Errorf("APIKey = %q, 空白环境变量不应覆盖", got.APIKey)
		}
	})
	t.Run("provider 名为空时不处理", func(t *testing.T) {
		t.Setenv("_API_KEY", "x")
		got := applyProviderKeyFromEnv(ProviderConfig{APIKey: "keep"})
		if got.APIKey != "keep" {
			t.Errorf("APIKey = %q, 期望不动", got.APIKey)
		}
	})
	t.Run("其余字段不受影响", func(t *testing.T) {
		t.Setenv("KIMI_API_KEY", "sk-new")
		in := ProviderConfig{Name: "kimi", BaseURL: "https://x/v1", APIKey: "old"}
		got := applyProviderKeyFromEnv(in)
		if got.Name != "kimi" || got.BaseURL != "https://x/v1" {
			t.Errorf("非 APIKey 字段被改动: %+v", got)
		}
	})
}

// LoadFromConfig 端到端: 注册进 registry 的 provider 必须已带上环境变量里的 key。
func TestLoadFromConfig_注册时已套用环境变量(t *testing.T) {
	t.Setenv("KIMI_API_KEY", "sk-e2e")
	reg, _, err := LoadFromConfig(ConfigJSON{
		Providers: map[string]ProviderConfig{
			"kimi": {Name: "kimi", BaseURL: "https://x/v1", APIKey: "PLACEHOLDER_OVERRIDE_VIA_ENV",
				Models: map[string]ModelConfig{"kimi:k3": {}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := reg.GetProvider("kimi")
	if p == nil {
		t.Fatal("provider kimi 未注册")
	}
	if p.APIKey != "sk-e2e" {
		t.Errorf("registry 里的 APIKey = %q, 期望 sk-e2e (说明覆盖没在注册前生效)", p.APIKey)
	}
}
