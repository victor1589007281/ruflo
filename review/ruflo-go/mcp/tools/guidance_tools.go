package tools

import (
	"context"
	"encoding/json"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/guidance"
)

func guidanceTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{
			Name:        "guidance_capabilities",
			Description: "Return compiled guidance manifest and shard summary",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleGuidanceCapabilities,
		},
		{
			Name:        "guidance_recommend",
			Description: "Retrieve rule shards for a task intent",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"intent": map[string]any{"type": "string"}},
				"required":   []string{"intent"},
			},
			Handler: handleGuidanceRecommend,
		},
		{
			Name:        "guidance_discover",
			Description: "List guidance shard ids and intents from bundle",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleGuidanceDiscover,
		},
		{
			Name:        "guidance_workflow",
			Description: "Describe built-in guidance workflow stages",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleGuidanceWorkflow,
		},
		{
			Name:        "guidance_quickref",
			Description: "Compact capability quick reference",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     handleGuidanceQuickref,
		},
	}
}

func handleGuidanceCapabilities(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	b := globalState.guidancePlane.Bundle()
	return jsonOK(map[string]any{
		"bundle_id": b.ID,
		"version":   b.Version,
		"manifest":  b.Manifest,
		"shards":    len(b.Shards),
	})
}

func handleGuidanceDiscover(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	b := globalState.guidancePlane.Bundle()
	out := make([]map[string]any, 0, len(b.Shards))
	for _, sh := range b.Shards {
		intents := make([]string, 0, len(sh.Intents))
		for _, in := range sh.Intents {
			intents = append(intents, string(in))
		}
		out = append(out, map[string]any{"id": sh.ShardID, "intents": intents})
	}
	return jsonOK(map[string]any{"shards": out, "count": len(out)})
}

func handleGuidanceWorkflow(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	stages := []string{"compile", "retrieve", "gate", "ledger", "optimize"}
	return jsonOK(map[string]any{"stages": stages})
}

func handleGuidanceQuickref(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	return jsonOK(map[string]any{
		"capabilities": []string{"capabilities", "recommend", "discover", "workflow", "quickref"},
		"bundle_id":    globalState.guidancePlane.Bundle().ID,
	})
}

func handleGuidanceRecommend(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var in struct {
		Intent string `json:"intent"`
	}
	if err := parseArgs(args, &in); err != nil {
		return nil, err
	}
	ti := guidance.TaskIntent(in.Intent)
	shards := globalState.guidancePlane.RetrieveForTask(ti)
	return jsonOK(map[string]any{"intent": in.Intent, "shards": shards})
}

// CompileGuidanceMarkdown loads markdown into the control plane and compiles (helper for tests/CLI).
func CompileGuidanceMarkdown(root, local string) {
	globalState.guidancePlane.SetSources(root, local)
	globalState.guidancePlane.Compile()
}

// EvaluateCommandMCP is a thin wrapper for tooling tests.
func EvaluateCommandMCP(cmd string) []guidance.GateResult {
	return globalState.guidancePlane.EvaluateCommand(cmd)
}
