package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ruflo/ruflo-go/internal/config"
	"github.com/ruflo/ruflo-go/pkg/providers"
	"github.com/spf13/cobra"
)

// 本文件实现 providers 命令组：列举各 LLM 提供商在配置文件与环境变量中的密钥配置情况，支持写入/清除 claude-flow.config.json 中的密钥，并对各 provider 做 HealthCheck。

// projectConfigPath 返回当前应使用的配置文件路径：优先全局 ConfigPath，否则为工作目录下 claude-flow.config.json。
func projectConfigPath() (string, error) {
	if ConfigPath != "" {
		return ConfigPath, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, "claude-flow.config.json"), nil
}

func newProvidersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "Manage LLM provider credentials and connectivity",
	}
	cmd.AddCommand(providersListCmd(), providersAddCmd(), providersRemoveCmd(), providersTestCmd())
	return cmd
}

// providersListCmd 读取项目配置与环境变量，表格或 JSON 输出各提供商是否「像已配置」。
func providersListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List providers and whether keys appear configured",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProjectConfig()
			if err != nil {
				return err
			}
			rows := []map[string]string{
				{"name": "anthropic", "config": truth(cfg.APIKeys.Anthropic != ""), "env": truth(os.Getenv("ANTHROPIC_API_KEY") != "")},
				{"name": "openai", "config": truth(cfg.APIKeys.OpenAI != ""), "env": truth(os.Getenv("OPENAI_API_KEY") != "")},
				{"name": "google", "config": truth(cfg.APIKeys.Google != ""), "env": truth(os.Getenv("GOOGLE_API_KEY") != "")},
				{"name": "ollama", "config": "n/a", "env": truth(os.Getenv("OLLAMA_BASE_URL") != "")},
				{"name": "cohere", "config": "n/a", "env": truth(os.Getenv("COHERE_API_KEY") != "")},
				{"name": "ruvector", "config": "n/a", "env": truth(os.Getenv("RUVECTOR_BASE_URL") != "")},
			}
			if OutputFormat == "json" {
				enc := json.NewEncoder(Stdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"providers": rows})
			}
			for _, r := range rows {
				_, _ = fmt.Fprintf(Stdout(), "%-12s  config_key=%s  env=%s\n", r["name"], r["config"], r["env"])
			}
			return nil
		},
	}
}

// truth 将布尔值格式化为 yes/no 供列表展示。
func truth(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// loadProjectConfig 通过 projectConfigPath 加载配置，不存在时使用 config.Load 的宽容行为。
func loadProjectConfig() (config.RufloConfig, error) {
	p, err := projectConfigPath()
	if err != nil {
		return config.RufloConfig{}, err
	}
	return config.Load(p, true)
}

// providersAddCmd 将 API 密钥写入配置文件对应字段（anthropic/openai/google），并确保目录存在。
func providersAddCmd() *cobra.Command {
	var name, key string
	c := &cobra.Command{
		Use:   "add",
		Short: "Store API key in claude-flow.config.json (by provider name)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || key == "" {
				return fmt.Errorf("provider name and key are required (--name, --key)")
			}
			p, err := projectConfigPath()
			if err != nil {
				return err
			}
			cfg, err := config.Load(p, true)
			if err != nil {
				return err
			}
			switch strings.ToLower(name) {
			case "anthropic":
				cfg.APIKeys.Anthropic = key
			case "openai":
				cfg.APIKeys.OpenAI = key
			case "google":
				cfg.APIKeys.Google = key
			default:
				return fmt.Errorf("unknown provider %q (use anthropic, openai, google)", name)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			return config.Save(p, cfg)
		},
	}
	c.Flags().StringVar(&name, "name", "", "Provider: anthropic, openai, google")
	c.Flags().StringVar(&key, "key", "", "API key value")
	return c
}

// providersRemoveCmd 清空指定提供商在配置文件中的密钥字段。
func providersRemoveCmd() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "remove",
		Short: "Clear API key for a provider in claude-flow.config.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name required")
			}
			p, err := projectConfigPath()
			if err != nil {
				return err
			}
			cfg, err := config.Load(p, true)
			if err != nil {
				return err
			}
			switch strings.ToLower(name) {
			case "anthropic":
				cfg.APIKeys.Anthropic = ""
			case "openai":
				cfg.APIKeys.OpenAI = ""
			case "google":
				cfg.APIKeys.Google = ""
			default:
				return fmt.Errorf("unknown provider %q", name)
			}
			return config.Save(p, cfg)
		},
	}
	c.Flags().StringVar(&name, "name", "", "Provider name")
	return c
}

func providersTestCmd() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "test",
		Short: "Run a lightweight health check for a provider",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if name == "" {
				return fmt.Errorf("--name required (anthropic, openai, google, ollama, cohere, ruvector)")
			}
			var err error
			switch strings.ToLower(name) {
			case "google":
				err = providers.NewGoogleProvider().HealthCheck(ctx)
			case "ollama":
				err = providers.NewOllamaProvider().HealthCheck(ctx)
			case "openai":
				p := &providers.OpenAIProvider{APIKey: os.Getenv("OPENAI_API_KEY")}
				err = p.HealthCheck(ctx)
			case "anthropic":
				p := &providers.AnthropicProvider{APIKey: os.Getenv("ANTHROPIC_API_KEY")}
				err = p.HealthCheck(ctx)
			case "cohere":
				err = providers.NewCohereProviderFromEnv().HealthCheck(ctx)
			case "ruvector":
				err = providers.NewRuVectorProviderFromEnv().HealthCheck(ctx)
			default:
				return fmt.Errorf("unknown provider %q", name)
			}
			if OutputFormat == "json" {
				st := "ok"
				if err != nil {
					st = err.Error()
				}
				return json.NewEncoder(Stdout()).Encode(map[string]any{"provider": name, "status": st})
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(Stdout(), "provider %s: ok\n", name)
			return err
		},
	}
	c.Flags().StringVar(&name, "name", "", "Provider name")
	return c
}
