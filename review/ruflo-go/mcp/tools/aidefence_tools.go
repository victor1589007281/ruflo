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

var (
	reInject = regexp.MustCompile(`(?i)(ignore previous|system override|jailbreak|DAN mode|disregard)`)
)

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

func aidefenceSave() error {
	aidefMu.Lock()
	aidefData.Updated = now()
	cp := *aidefData
	cp.Findings = append([]map[string]any(nil), aidefData.Findings...)
	cp.Learned = append([]string(nil), aidefData.Learned...)
	aidefMu.Unlock()
	return writeJSONFile(aidefencePath(), cp)
}

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

// RegisterAidefenceTools registers lightweight prompt/PII guard tools.
func RegisterAidefenceTools(reg *mcp.ToolRegistry) error {
	for _, t := range aidefenceTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func aidefenceEvaluate(text string) map[string]any {
	pii := detectPIIIssues(text)
	inj := reInject.FindString(text) != ""
	safe := len(pii) == 0 && !inj
	return map[string]any{
		"pii_types": pii, "injection_suspected": inj, "safe": safe,
		"length": len(text), "at": time.Now().UTC(),
	}
}

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

func handleAidefenceStats(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	aidefenceLoad()
	aidefMu.Lock()
	n := aidefData.Scans
	aidefMu.Unlock()
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"scans": n}}
}

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

func handleAidefenceIsSafe(_ context.Context, m map[string]any) mcp.MCPToolResult {
	text := strArg(m, "text")
	ev := aidefenceEvaluate(text)
	safe, _ := ev["safe"].(bool)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"safe": safe}}
}

func handleAidefenceHasPII(_ context.Context, m map[string]any) mcp.MCPToolResult {
	text := strArg(m, "text")
	pii := detectPIIIssues(text)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"has_pii": len(pii) > 0, "types": pii}}
}
