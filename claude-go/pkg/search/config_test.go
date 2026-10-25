package search

import "testing"

// TestLoadConfigGitHubTokenPrecedence 钉住配置优先于环境变量的语义。
//
// 配置值来自 config.json 的 search.githubToken; 环境变量只作为旧部署的兜底。
// 两者都设时必须用配置值 —— 否则 settings.json 的 env 合并会把部署态悄悄
// 覆盖回一个空/旧 token, 表现为"配置改了但限额没变"。
func TestLoadConfigGitHubTokenPrecedence(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "env-token")
	t.Cleanup(func() { SetGitHubToken("") })

	SetGitHubToken("config-token")
	if got := LoadConfig().Providers["github"].APIKey; got != "config-token" {
		t.Fatalf("配置 token 未生效: got %q, want %q", got, "config-token")
	}

	// 清除配置值后回退到环境变量, 保证尚未迁移的部署不被这次改动打断。
	SetGitHubToken("")
	if got := LoadConfig().Providers["github"].APIKey; got != "env-token" {
		t.Fatalf("环境变量兜底未生效: got %q, want %q", got, "env-token")
	}
}

// TestLoadConfigGitHubTokenTrimmed 钉住两端空白被裁掉。
//
// 从文件/Secret 读出来的 token 常带换行; 带着空白的 Authorization 头会被
// GitHub 判 401, 而错误信息看起来像"token 无效", 掩盖真正原因是换行。
func TestLoadConfigGitHubTokenTrimmed(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Cleanup(func() { SetGitHubToken("") })

	SetGitHubToken("  config-token\n")
	if got := LoadConfig().Providers["github"].APIKey; got != "config-token" {
		t.Fatalf("token 未去空白: got %q", got)
	}
}
