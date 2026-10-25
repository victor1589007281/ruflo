package main

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/wechat"
)

func TestApplyWechatEnvOverrides(t *testing.T) {
	t.Setenv("CLAUDE_GO_WECHAT_APP_ID", "wx_env")
	t.Setenv("CLAUDE_GO_WECHAT_APP_SECRET", "env_secret")
	c := wechat.Config{AppID: "wx_file", AppSecret: "file_secret"}
	applyWechatEnvOverrides(&c)
	if c.AppID != "wx_env" || c.AppSecret != "env_secret" {
		t.Fatalf("env 应覆盖配置文件, got appID=%q secret=%q", c.AppID, c.AppSecret)
	}
}

func TestApplyWechatEnvOverridesKeepsFileWhenEmpty(t *testing.T) {
	t.Setenv("CLAUDE_GO_WECHAT_APP_ID", "")
	t.Setenv("CLAUDE_GO_WECHAT_APP_SECRET", "  ")
	c := wechat.Config{AppID: "wx_file", AppSecret: "file_secret"}
	applyWechatEnvOverrides(&c)
	if c.AppID != "wx_file" || c.AppSecret != "file_secret" {
		t.Fatalf("空环境变量不应覆盖配置文件, got appID=%q secret=%q", c.AppID, c.AppSecret)
	}
}
