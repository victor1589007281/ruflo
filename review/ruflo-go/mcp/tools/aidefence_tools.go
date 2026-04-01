package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/mcp"
)

// reInject 用于匹配常见提示注入/越狱类短语（不区分大小写）。
var (
	reInject = regexp.MustCompile(`(?i)(ignore previous|system override|jailbreak|DAN mode|disregard)`)
)

// aidefenceState 为 aidefence 持久化状态：扫描计数、每次评估摘要、用户记录的阻断短语及更新时间。
type aidefenceState struct {
	Scans     int              `json:"scans"`
	Findings  []map[string]any `json:"findings"`
	Learned   []string         `json:"learned"`
	Updated   time.Time        `json:"updated"`
}

var (
	aidefMu   sync.Mutex
	aidefData = &aidefenceState{}
)

func aidefencePath() string {
	return filepath.Join(resolveDataDir(), "aidefence", "state.json")
}

// aidefenceLoad 从磁盘加载状态；若文件不存在或解析失败则保持内存中的默认值。
func aidefenceLoad() {
	aidefMu.Lock()
	defer aidefMu.Unlock()
	b, err := os.ReadFile(aidefencePath())
	if err != nil {
		return
	}
	var s aidefenceState
	if json.Unmarshal(b, &s) == nil {
		aidefData = &s
	}
}

// aidefenceSave 将当前状态快照写入磁盘（深拷贝 Findings/Learned 避免持锁过久）。
func aidefenceSave() error {
	aidefMu.Lock()
	aidefData.Updated = now()
	cp := *aidefData
	cp.Findings = append([]map[string]any(nil), aidefData.Findings...)
	cp.Learned = append([]string(nil), aidefData.Learned...)
	aidefMu.Unlock()
	return writeJSONFile(aidefencePath(), cp)
}

// aidefenceTools 构造并返回 aidefence 系列 MCP 工具定义（含 JSON Schema 与 Handler）。
func aidefenceTools() []*mcp.MCPTool {
	aidefenceLoad()
	return []*mcp.MCPTool{
		{Name: "aidefence_scan", Description: "Scan text for injection / PII signals", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}, Handler: toolHandler(handleAidefenceScan)},
		{Name: "aidefence_analyze", Description: "Structured analysis of prompt safety", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}, Handler: toolHandler(handleAidefenceAnalyze)},
		{Name: "aidefence_stats", Description: "Historical aidefence counters", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handleAidefenceStats)},
		{Name: "aidefence_learn", Description: "Record blocked phrase for future scans", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"phrase": map[string]any{"type": "string"}}, "required": []string{"phrase"}}, Handler: toolHandler(handleAidefenceLearn)},
		{Name: "aidefence_is_safe", Description: "Boolean coarse safety check", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}, Handler: toolHandler(handleAidefenceIsSafe)},
		{Name: "aidefence_has_pii", Description: "Boolean PII heuristic", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}, Handler: toolHandler(handleAidefenceHasPII)},
	}
}

// RegisterAidefenceTools 向注册表登记轻量级提示词/PII 防护相关 MCP 工具。
func RegisterAidefenceTools(reg *mcp.ToolRegistry) error {
	for _, t := range aidefenceTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// aidefenceEvaluate 对 text 执行 PII 检测与注入启发式判断，返回结构化结果（含 safe 布尔值与 UTC 时间戳）。
func aidefenceEvaluate(text string) map[string]any {
	pii := detectPIIIssues(text)
	inj := reInject.FindString(text) != ""
	safe := len(pii) == 0 && !inj
	return map[string]any{
		"pii_types": pii, "injection_suspected": inj, "safe": safe,
		"length": len(text), "at": time.Now().UTC(),
	}
}

// handleAidefenceScan 处理 aidefence_scan：参数 text 为待扫描内容；递增扫描计数、追加 finding（最多保留 200 条）并落盘。
func handleAidefenceScan(_ context.Context, m map[string]any) mcp.MCPToolResult {
	text := strArg(m, "text")
	if text == "" {
		return mcp.MCPToolResult{OK: false, Error: "text required"}
	}
	res := aidefenceEvaluate(text)
	aidefMu.Lock()
	aidefData.Scans++
	aidefData.Findings = append(aidefData.Findings, res)
	if len(aidefData.Findings) > 200 {
		aidefData.Findings = aidefData.Findings[len(aidefData.Findings)-200:]
	}
	aidefMu.Unlock()
	_ = aidefenceSave()
	return mcp.MCPToolResult{OK: true, Data: res}
}

// handleAidefenceAnalyze 处理 aidefence_analyze：在 evaluate 基础上附加 learned_hits（文本是否命中已学习的 phrase）。
func handleAidefenceAnalyze(_ context.Context, m map[string]any) mcp.MCPToolResult {
	text := strArg(m, "text")
	if text == "" {
		return mcp.MCPToolResult{OK: false, Error: "text required"}
	}
	ev := aidefenceEvaluate(text)
	aidefMu.Lock()
	extra := make([]string, 0)
	for _, ph := range aidefData.Learned {
		if ph != "" && strings.Contains(strings.ToLower(text), strings.ToLower(ph)) {
			extra = append(extra, "learned:"+ph)
		}
	}
	aidefMu.Unlock()
	ev["learned_hits"] = extra
	return mcp.MCPToolResult{OK: true, Data: ev}
}

// handleAidefenceStats 处理 aidefence_stats：重新加载磁盘状态后返回历史扫描次数 scans。
func handleAidefenceStats(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	aidefenceLoad()
	aidefMu.Lock()
	n := aidefData.Scans
	aidefMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"scans": n}}
}

// handleAidefenceLearn 处理 aidefence_learn：参数 phrase 为需记录的阻断/敏感短语，追加到 Learned 并保存。
func handleAidefenceLearn(_ context.Context, m map[string]any) mcp.MCPToolResult {
	ph := strArg(m, "phrase")
	if ph == "" {
		return mcp.MCPToolResult{OK: false, Error: "phrase required"}
	}
	aidefMu.Lock()
	aidefData.Learned = append(aidefData.Learned, ph)
	aidefMu.Unlock()
	_ = aidefenceSave()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"stored": true}}
}

// handleAidefenceIsSafe 处理 aidefence_is_safe：参数 text；返回粗粒度 safe 布尔值（无 PII 且未命中注入启发式）。
func handleAidefenceIsSafe(_ context.Context, m map[string]any) mcp.MCPToolResult {
	text := strArg(m, "text")
	ev := aidefenceEvaluate(text)
	safe, _ := ev["safe"].(bool)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"safe": safe}}
}

// handleAidefenceHasPII 处理 aidefence_has_pii：参数 text；返回 has_pii 与 PII 类型列表 types。
func handleAidefenceHasPII(_ context.Context, m map[string]any) mcp.MCPToolResult {
	text := strArg(m, "text")
	pii := detectPIIIssues(text)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"has_pii": len(pii) > 0, "types": pii}}
}
