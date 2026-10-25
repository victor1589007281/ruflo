package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/sandbox"
	"github.com/anthropic/claude-go/pkg/session"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
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

func TestStripReportPreamble(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "planning then report",
			in:   "Now write the report. Body 1500-2500 字.\n\nStructure:\n1. 结论\n\n# 真正的报告\n\n正文",
			want: "# 真正的报告\n\n正文",
		},
		{
			name: "planning heading is skipped",
			in:   "Let me plan.\n\n## Plan\n\n1. search\n\n## 结论\n\n正文",
			want: "## 结论\n\n正文",
		},
		{
			name: "normal intro is preserved",
			in:   "这是一段摘要。\n\n# 报告\n\n正文",
			want: "这是一段摘要。\n\n# 报告\n\n正文",
		},
		{
			name: "report only",
			in:   "# 报告\n\n正文",
			want: "# 报告\n\n正文",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripReportPreamble(tc.in); got != tc.want {
				t.Fatalf("stripReportPreamble() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStripReportPreambleLargePlanningBlock 钉住 4KB 上限的真实回归:
// CBAM 的一次运行写了 4.2KB 的“数字复核/口径提醒”才出标题, 旧的 4KB 闸门
// 直接放行, 整段计划原样进 final。
func TestStripReportPreambleLargePlanningBlock(t *testing.T) {
	planning := "Now write the report. 字数目标 1500-2500.\n" + strings.Repeat("核对数字与口径，避免把估算写成官方数据。\n", 200)
	in := planning + "\n# 报告标题\n\n正文"
	got := stripReportPreamble(in)
	if got != "# 报告标题\n\n正文" {
		t.Fatalf("4KB 以上的计划段未被裁掉: got %d bytes, prefix %q", len(got), got[:80])
	}
}

func TestKeepLastCompleteReport(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "two drafts keep last",
			in:   "# 第一稿\n\n## 一、结论\n\n旧\n\n# 第二稿\n\n## 一、结论\n\n新",
			want: "# 第二稿\n\n## 一、结论\n\n新",
		},
		{
			name: "appendix is not a new draft",
			in:   "# 报告\n\n## 一、结论\n\n正文\n\n# 附录\n\n表",
			want: "# 报告\n\n## 一、结论\n\n正文\n\n# 附录\n\n表",
		},
		{
			// 第二稿省略 H1 时, 只按“## 结论”切会丢掉唯一的报告标题。
			name: "single h1 is kept when second draft omits it",
			in:   "# 报告\n\n## 一、结论\n\n旧\n\n## 一、结论\n\n新",
			want: "# 报告\n\n## 一、结论\n\n新",
		},
		{
			name: "single report unchanged",
			in:   "# 报告\n\n## 一、结论\n\n正文",
			want: "# 报告\n\n## 一、结论\n\n正文",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepLastCompleteReport(tc.in); got != tc.want {
				t.Fatalf("keepLastCompleteReport() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveRuntimeModelConfigUsesProviderAlias(t *testing.T) {
	// 本机 shell 可能带着 CLAUDE_GO_LLM_GATEWAY; 不隔离会把 BaseURL 改指
	// 网关, 让"解析到 provider 端点"的断言在开发机上假失败。
	t.Setenv("CLAUDE_GO_LLM_GATEWAY", "")
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
	// 同上: 网关 env 会把出站 BaseURL 改指 127.0.0.1:18989。
	t.Setenv("CLAUDE_GO_LLM_GATEWAY", "")
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

// TestCLISessionTraceKeepsSpansOutOfOrphanBucket 钉住交互式会话的轨迹归属。
//
// 这是**修掉的泄漏**的判据。改造前 chat 的主循环每轮 `ctx := context.Background()`,
// 于是整段交互式会话的 llm_call / turn 经 tracestore.bucketName("") 全部落进
// trace-orphan 桶, 而 webapp 的轨迹页只列 trace-run-* —— 那一屏什么都没有,
// 且**没有任何一层会报错**。实测: 一次 chat 往 orphan 写 2 条, 该桶累积到 1.8 万条。
//
// 为什么要真的走 TraceStore 而不是只断言 ctx 里有 RunID: RunID 进了 ctx 而落盘
// 那一步仍用局部空值, 是完全可能的实现 —— 两种写法在"ctx 里有没有 RunID"上同绿。
// 所以判据落在**桶文件**上, 那是这条链唯一的成品形态。
func TestCLISessionTraceKeepsSpansOutOfOrphanBucket(t *testing.T) {
	dir := t.TempDir()
	ss := statestore.NewFileStore(dir)
	ts := tracestore.New(ss)

	ctx, runID := beginCLISessionTrace(nil) // nil store: 拿不到会话 id
	if runID != "" {
		t.Fatalf("store 为 nil 时不该发号 (硬编号会造出假归属), 却拿到 %q", runID)
	}
	if trace.From(ctx).RunID != "" {
		t.Fatalf("store 为 nil 时 ctx 里不该有 RunID")
	}

	// TraceStore 未装配时也照写不误 —— recordRunSpan 自己守卫, 不该 panic。
	recordCLISessionRunSpan(nil, runID, "", nil, "m", tracestore.RunStatusOK, "", time.Now())

	// 真会话: 用一个真 SessionStore 换出 RunID, 再按 chat 主循环的写法落一条 span。
	store, err := session.NewSessionStore(dir)
	if err != nil {
		t.Skipf("本环境建不出 SessionStore: %v", err)
	}
	defer store.Close()
	ctx, runID = beginCLISessionTrace(store)
	if runID == "" {
		t.Fatal("有会话时 RunID 不该为空 —— 空 TraceID 会被归进 trace-orphan 桶")
	}
	if got := trace.From(ctx).RunID; got != runID {
		t.Fatalf("ctx 里的 RunID=%q 与返回值 %q 不一致", got, runID)
	}

	// ★ 主循环那条形态: span 的 TraceID 必须取自 **ctx**, 不是局部空值。
	ts.Write(tracestore.Span{
		TraceID: trace.From(ctx).RunID,
		SpanID:  "span-1",
		Kind:    tracestore.KindLLMCall,
		Name:    "test-model",
		TS:      time.Now().UnixMilli(),
	})
	recordCLISessionRunSpan(ts, runID, "会话名", store, "test-model", tracestore.RunStatusOK, "", time.Now())

	files, err := os.ReadDir(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatalf("读不到桶目录: %v", err)
	}
	var sawRunBucket bool
	for _, f := range files {
		if f.Name() == "trace-orphan.jsonl" {
			t.Fatalf("交互式会话的 span 落进了 trace-orphan —— webapp 轨迹页只列 trace-run-*, "+
				"这条运行在页面上不存在 (桶目录: %v)", files)
		}
		if strings.HasPrefix(f.Name(), "trace-run-") {
			sawRunBucket = true
		}
	}
	if !sawRunBucket {
		t.Fatalf("没有 trace-run-* 桶 —— 说明 span 根本没写出去; 桶目录: %v", files)
	}

	// RunID 不得重复: 每条 REPL 进程/run 必须唯一, 否则奖励归因会把两次记到一起。
	if _, runID2 := beginCLISessionTrace(store); runID2 == runID {
		t.Fatalf("两次 beginCLISessionTrace 拿到同一个 RunID %q", runID)
	}
}
