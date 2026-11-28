package main

// MCP 握手预算的测试。为什么值得单独测：connectMCP 的调用方传 context.Background()，
// 没有预算的话一个哑掉的 server 会把 buildEngine 永久挂住（实测过一次：npx 拉包卡死,
// run 三分钟一条输出都没有, 看着像 claude-go 自己死了）。这些用例锁住"可选依赖坏掉
// 只降级、不连坐"这条不变量。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/mcp"
)

func TestMCPConnectTimeoutParsing(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultMCPConnectTimeout},
		{"45s", 45 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"0", 0},        // 显式关掉超时
		{"-1s", -time.Second},
		{"不是时长", defaultMCPConnectTimeout}, // 解析失败退回默认
	}
	for _, c := range cases {
		t.Setenv("CLAUDE_GO_MCP_CONNECT_TIMEOUT", c.env)
		if got := mcpConnectTimeout(); got != c.want {
			t.Fatalf("env=%q: got %v, want %v", c.env, got, c.want)
		}
	}
}

// 一个永远不回 initialize 的 server（这里是 sleep）必须在预算内被放弃,
// 且错误文案点明"握手超时"与放宽办法。
func TestConnectMCPOneTimesOutOnSilentServer(t *testing.T) {
	cfg := mcp.ServerConfig{Name: "silent", Command: "sleep", Args: []string{"30"}, Transport: "stdio"}
	started := time.Now()
	conn, err := connectMCPOne(context.Background(), mcp.NewClient(), cfg, 300*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("静默 server 不应连接成功")
	}
	if !strings.Contains(err.Error(), "握手超时") {
		t.Fatalf("错误文案要说明是握手超时, got: %v", err)
	}
	if !strings.Contains(err.Error(), "CLAUDE_GO_MCP_CONNECT_TIMEOUT") {
		t.Fatalf("错误文案要给出放宽办法, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("预算没生效: 耗时 %s", elapsed)
	}
}

// budget<=0 走不限时分支（不改行为, 只验证不会立刻误判超时）。
// 用一个真实存在的握手应答者代价太高, 这里只验证空命令路径不被预算逻辑干扰。
func TestConnectMCPEmptyPathIsNotAnError(t *testing.T) {
	conns, err := connectMCP(context.Background(), "")
	if err != nil || conns != nil {
		t.Fatalf("空路径 = 没配 MCP, 应返回 (nil, nil), got (%v, %v)", conns, err)
	}
}
