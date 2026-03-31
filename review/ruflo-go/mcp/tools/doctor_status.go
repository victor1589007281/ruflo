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

type doctorArgs struct {
	ConfigPath string `json:"config_path"`
}

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

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

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
