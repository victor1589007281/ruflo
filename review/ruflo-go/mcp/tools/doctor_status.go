package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/ruflo/ruflo-go/mcp"
)

// 本文件：Doctor 环境检查与编排运行时状态概览 MCP 工具。
//
// 设计思路：doctor_check 通过子进程探测 node/git、配置文件存在性、内存占位、当前工作目录可用磁盘空间（依赖
// diskFreeBytes 平台实现）、Go 运行时版本；daemon 项为进程内占位说明。status_overview 聚合 globalState
// 中代理、蜂群、会话、记忆条目与神经模式数量，并提示配置路径线索。

// doctorTool 注册 doctor_check：可选 config_path，执行本地多维度健康检查并返回 checks 数组。
func doctorTool() *mcp.MCPTool {
	return &mcp.MCPTool{
		Name:        "doctor_check",
		Description: "Run local environment checks (Node, Git, config, daemon, memory DB, disk)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"config_path": map[string]any{"type": "string"},
			},
		},
		Handler: handleDoctorCheck,
	}
}

// statusTool 注册 status_overview：无入参，返回编排运行时聚合计数与路径提示。
func statusTool() *mcp.MCPTool {
	return &mcp.MCPTool{
		Name:        "status_overview",
		Description: "System overview for orchestration runtime",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: handleStatusOverview,
	}
}

// doctorArgs 为 doctor_check 的可选 JSON 入参：config_path 指定要检查的配置文件路径，空则默认 claude-flow.config.json。
type doctorArgs struct {
	ConfigPath string `json:"config_path"`
}

// handleDoctorCheck 逐项收集检查结果（node、git、config_file、daemon、memory_db、disk、go_runtime），
// 解析失败不中断；config_path 空时使用默认文件名；磁盘检查基于 Getwd 与 diskFreeBytes。
func handleDoctorCheck(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a doctorArgs
	_ = parseArgs(args, &a)
	checks := []map[string]any{}

	// Node version (optional — many users may not have node on PATH)
	nodeV := ""
	if out, err := exec.CommandContext(ctx, "node", "--version").CombinedOutput(); err == nil {
		nodeV = string(out)
	}
	checks = append(checks, map[string]any{
		"name": "node", "ok": nodeV != "", "detail": nodeV,
	})

	gitV := ""
	if out, err := exec.CommandContext(ctx, "git", "--version").CombinedOutput(); err == nil {
		gitV = string(out)
	}
	checks = append(checks, map[string]any{
		"name": "git", "ok": gitV != "", "detail": gitV,
	})

	cfg := a.ConfigPath
	if cfg == "" {
		cfg = "claude-flow.config.json"
	}
	_, err := os.Stat(cfg)
	checks = append(checks, map[string]any{
		"name": "config_file", "ok": err == nil, "path": cfg, "detail": errString(err),
	})

	checks = append(checks, map[string]any{
		"name": "daemon", "ok": true, "detail": "not managed in-process (stub)",
	})

	globalState.mu.RLock()
	memOk := globalState.memoryInitialized || len(globalState.memory) > 0
	globalState.mu.RUnlock()
	checks = append(checks, map[string]any{
		"name": "memory_db", "ok": memOk, "detail": "in-memory store",
	})

	diskOk := false
	freeDetail := ""
	wd, wdErr := os.Getwd()
	if wdErr != nil {
		freeDetail = wdErr.Error()
	} else {
		n, err := diskFreeBytes(wd)
		if err == nil {
			diskOk = true
			gb := float64(n) / (1024 * 1024 * 1024)
			freeDetail = fmt.Sprintf("%.1f GB free", gb)
		} else {
			freeDetail = err.Error()
		}
	}
	checks = append(checks, map[string]any{
		"name": "disk", "ok": diskOk, "detail": freeDetail,
	})

	checks = append(checks, map[string]any{
		"name": "go_runtime", "ok": true, "detail": runtime.Version(),
	})

	return jsonOK(map[string]any{"checks": checks})
}

// errString 将 error 转为字符串，nil 时返回空串，供检查结果 detail 字段使用。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// handleStatusOverview 在读锁下统计 agents/swarms/sessions、跨 namespace 的 memory 条目总数、
// neural.Patterns 数量，并返回 cwd 与 claude-flow.config.json 的拼接提示路径。
func handleStatusOverview(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	agents := len(globalState.agents)
	swarms := len(globalState.swarms)
	sessions := len(globalState.sessions)
	entries := 0
	for _, m := range globalState.memory {
		entries += len(m)
	}
	patterns := len(globalState.neural.Patterns)
	globalState.mu.RUnlock()
	cwd, _ := os.Getwd()
	return jsonOK(map[string]any{
		"agents":          agents,
		"swarms":          swarms,
		"memory_entries":  entries,
		"sessions":        sessions,
		"neural_patterns": patterns,
		"cwd":             cwd,
		"config_hint":     filepath.Join(cwd, "claude-flow.config.json"),
	})
}
