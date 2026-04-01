package tools

// 本文件实现「自动驾驶 / 辅助编排」状态的 MCP 工具（autopilot_*）。
//
// 设计思路：
//   - 将开关、配置、日志、进度条、学习条目等保存在数据目录 autopilot/state.json，供多轮对话或任务流水线读取。
//   - 不执行真实无人驾车式自动化，而是提供可持久化的状态机式占位：启用/禁用、追加日志、KV 进度、极简 predict 提示。
//   - 日志与历史有上限裁剪，避免单文件无限增长。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/mcp"
)

// autopilotState 表示 autopilot 的完整持久化状态：开关、配置、结构化日志、进度、历史字符串与学习记录。
type autopilotState struct {
	Enabled   bool               `json:"enabled"`
	Config    map[string]any     `json:"config"`
	Log       []map[string]any   `json:"log"`
	Progress  map[string]float64 `json:"progress"`
	History   []string           `json:"history"`
	Learned   []map[string]any   `json:"learned"`
	UpdatedAt time.Time          `json:"updated_at"`
}

var (
	autopilotMu   sync.Mutex
	autopilotData = &autopilotState{
		Config:   map[string]any{"mode": "assist"},
		Progress: map[string]float64{},
	}
)

func autopilotPath() string {
	return filepath.Join(resolveDataDir(), "autopilot", "state.json")
}

func autopilotLoad() {
	autopilotMu.Lock()
	defer autopilotMu.Unlock()
	b, err := os.ReadFile(autopilotPath())
	if err != nil {
		return
	}
	var s autopilotState
	if json.Unmarshal(b, &s) == nil {
		if s.Config == nil {
			s.Config = map[string]any{}
		}
		if s.Progress == nil {
			s.Progress = map[string]float64{}
		}
		autopilotData = &s
	}
}

// autopilotSave 深拷贝可变切片后写入 state.json。
func autopilotSave() error {
	autopilotMu.Lock()
	autopilotData.UpdatedAt = now()
	cp := *autopilotData
	if cp.Config == nil {
		cp.Config = map[string]any{}
	}
	if cp.Progress == nil {
		cp.Progress = map[string]float64{}
	}
	cp.Log = append([]map[string]any(nil), autopilotData.Log...)
	cp.History = append([]string(nil), autopilotData.History...)
	cp.Learned = append([]map[string]any(nil), autopilotData.Learned...)
	autopilotMu.Unlock()
	return writeJSONFile(autopilotPath(), cp)
}

// autopilotTools 构造 autopilot_* MCP 工具列表。
func autopilotTools() []*mcp.MCPTool {
	autopilotLoad()
	obj := map[string]any{"type": "object", "properties": map[string]any{}}
	return []*mcp.MCPTool{
		{Name: "autopilot_status", Description: "Autopilot enabled state and summary", InputSchema: obj, Handler: toolHandler(handleAutopilotStatus)},
		{Name: "autopilot_enable", Description: "Enable autopilot", InputSchema: obj, Handler: toolHandler(handleAutopilotEnable)},
		{Name: "autopilot_disable", Description: "Disable autopilot", InputSchema: obj, Handler: toolHandler(handleAutopilotDisable)},
		{Name: "autopilot_config", Description: "Get or merge autopilot config", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"patch": map[string]any{"type": "object"}}}, Handler: toolHandler(handleAutopilotConfig)},
		{Name: "autopilot_reset", Description: "Reset autopilot state", InputSchema: obj, Handler: toolHandler(handleAutopilotReset)},
		{Name: "autopilot_log", Description: "Append or read autopilot log", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}, "limit": map[string]any{"type": "number"}}}, Handler: toolHandler(handleAutopilotLog)},
		{Name: "autopilot_progress", Description: "Set or get progress key", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}, "value": map[string]any{"type": "number"}}}, Handler: toolHandler(handleAutopilotProgress)},
		{Name: "autopilot_learn", Description: "Record learned fact", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"fact": map[string]any{"type": "string"}}}, Handler: toolHandler(handleAutopilotLearn)},
		{Name: "autopilot_history", Description: "List autopilot history strings", InputSchema: obj, Handler: toolHandler(handleAutopilotHistory)},
		{Name: "autopilot_predict", Description: "Lightweight next-step hint", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"context": map[string]any{"type": "string"}}}, Handler: toolHandler(handleAutopilotPredict)},
	}
}

// RegisterAutopilotTools 向注册表登记基于文件持久化的 autopilot MCP 工具。
func RegisterAutopilotTools(reg *mcp.ToolRegistry) error {
	for _, t := range autopilotTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handleAutopilotStatus 处理 autopilot_status：返回 enabled 与当前日志条数 log_entries。
func handleAutopilotStatus(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	en := autopilotData.Enabled
	nlog := len(autopilotData.Log)
	autopilotMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"enabled": en, "log_entries": nlog}}
}

// handleAutopilotEnable 处理 autopilot_enable：打开 autopilot 并持久化。
func handleAutopilotEnable(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	autopilotData.Enabled = true
	autopilotMu.Unlock()
	_ = autopilotSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"enabled": true}}
}

// handleAutopilotDisable 处理 autopilot_disable：关闭 autopilot 并持久化。
func handleAutopilotDisable(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	autopilotData.Enabled = false
	autopilotMu.Unlock()
	_ = autopilotSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"enabled": false}}
}

// handleAutopilotConfig 处理 autopilot_config：若入参含 patch 对象则合并到 Config 并保存；始终返回当前 Config 副本。
func handleAutopilotConfig(_ context.Context, m map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	if patch, ok := m["patch"].(map[string]any); ok {
		for k, v := range patch {
			autopilotData.Config[k] = v
		}
	}
	cp := make(map[string]any, len(autopilotData.Config))
	for k, v := range autopilotData.Config {
		cp[k] = v
	}
	autopilotMu.Unlock()
	if _, ok := m["patch"].(map[string]any); ok {
		_ = autopilotSave()
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"config": cp}}
}

// handleAutopilotReset 处理 autopilot_reset：恢复默认状态（关闭、默认 config、空进度）并保存。
func handleAutopilotReset(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	autopilotData = &autopilotState{
		Enabled:  false,
		Config:   map[string]any{"mode": "assist"},
		Progress: map[string]float64{},
	}
	autopilotMu.Unlock()
	_ = autopilotSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"reset": true}}
}

// handleAutopilotLog 处理 autopilot_log：message 非空则追加日志与 history；limit 控制返回最近条数（默认 50）。
func handleAutopilotLog(_ context.Context, m map[string]any) mcp.MCPToolResult {
	msg := strArg(m, "message")
	autopilotMu.Lock()
	if msg != "" {
		autopilotData.Log = append(autopilotData.Log, map[string]any{"at": now(), "message": msg})
		if len(autopilotData.Log) > 500 {
			autopilotData.Log = autopilotData.Log[len(autopilotData.Log)-500:]
		}
		autopilotData.History = append(autopilotData.History, msg)
		if len(autopilotData.History) > 200 {
			autopilotData.History = autopilotData.History[len(autopilotData.History)-200:]
		}
	}
	limit := 50
	if v, ok := m["limit"]; ok {
		switch t := v.(type) {
		case float64:
			limit = int(t)
		case int:
			limit = t
		}
	}
	logs := append([]map[string]any(nil), autopilotData.Log...)
	autopilotMu.Unlock()
	if msg != "" {
		_ = autopilotSave()
	}
	if limit > 0 && len(logs) > limit {
		logs = logs[len(logs)-limit:]
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"log": logs}}
}

// handleAutopilotProgress 处理 autopilot_progress：若提供 key 与 value 则写入 Progress[key]；仅 key 时查询该键；无 key 时返回整张进度表。
func handleAutopilotProgress(_ context.Context, m map[string]any) mcp.MCPToolResult {
	key := strArg(m, "key")
	autopilotMu.Lock()
	if v, ok := m["value"]; ok {
		var f float64
		switch t := v.(type) {
		case float64:
			f = t
		case int:
			f = float64(t)
		}
		if key != "" {
			autopilotData.Progress[key] = f
		}
	}
	if key != "" {
		v := autopilotData.Progress[key]
		needSave := false
		if _, ok := m["value"]; ok && key != "" {
			needSave = true
		}
		autopilotMu.Unlock()
		if needSave {
			_ = autopilotSave()
		}
		return mcp.MCPToolResult{OK: true, Data: map[string]any{"key": key, "value": v}}
	}
	cp := make(map[string]float64, len(autopilotData.Progress))
	for k, v := range autopilotData.Progress {
		cp[k] = v
	}
	autopilotMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"progress": cp}}
}

// handleAutopilotLearn 处理 autopilot_learn：参数 fact（必填）写入 Learned 列表（带时间戳），超长裁剪。
func handleAutopilotLearn(_ context.Context, m map[string]any) mcp.MCPToolResult {
	fact := strArg(m, "fact")
	if fact == "" {
		return mcp.MCPToolResult{OK: false, Error: "fact required"}
	}
	autopilotMu.Lock()
	autopilotData.Learned = append(autopilotData.Learned, map[string]any{"at": now(), "fact": fact})
	if len(autopilotData.Learned) > 200 {
		autopilotData.Learned = autopilotData.Learned[len(autopilotData.Learned)-200:]
	}
	autopilotMu.Unlock()
	_ = autopilotSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"stored": true}}
}

// handleAutopilotHistory 处理 autopilot_history：返回 History 字符串列表副本。
func handleAutopilotHistory(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	autopilotMu.Lock()
	h := append([]string(nil), autopilotData.History...)
	autopilotMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"history": h}}
}

// handleAutopilotPredict 处理 autopilot_predict：参数 context 可选；返回固定风格的下一步 hint 与固定 confidence（非真实模型预测）。
func handleAutopilotPredict(_ context.Context, m map[string]any) mcp.MCPToolResult {
	ctx := strArg(m, "context")
	hint := "continue_with_next_task"
	if ctx != "" {
		hint = "review_" + ctx
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"hint": hint, "confidence": 0.35}}
}
