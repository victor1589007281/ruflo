package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
)

var patternSeq int64

func neuralTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{
			Name:        "neural_train",
			Description: "Train / register neural patterns",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
					"score":       map[string]any{"type": "number"},
				},
			},
			Handler: handleNeuralTrain,
		},
		{
			Name:        "neural_predict",
			Description: "Predict best pattern match for a query string",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
			},
			Handler: handleNeuralPredict,
		},
		{
			Name:        "neural_patterns",
			Description: "List learned patterns",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleNeuralPatterns,
		},
		{
			Name:        "neural_status",
			Description: "Neural subsystem status",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleNeuralStatus,
		},
		{
			Name:        "neural_compress",
			Description: "Trim lowest-scoring patterns to cap size",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"max": map[string]any{"type": "number", "description": "max patterns to keep"},
				},
			},
			Handler: handleNeuralCompress,
		},
		{
			Name:        "neural_optimize",
			Description: "Run lightweight neural optimize pass (ordering)",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Handler: handleNeuralOptimize,
		},
	}
}

type neuralTrainArgs struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
}

func handleNeuralTrain(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a neuralTrainArgs
	if err := parseArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	id := fmt.Sprintf("pat-%d", atomic.AddInt64(&patternSeq, 1))
	p := api.Pattern{
		ID:          id,
		Name:        a.Name,
		Description: a.Description,
		Score:       a.Score,
		CreatedAt:   now(),
	}
	globalState.mu.Lock()
	globalState.neural.Patterns = append(globalState.neural.Patterns, p)
	globalState.neural.LastTrain = now()
	globalState.mu.Unlock()
	saveNeuralToDisk()
	return jsonOK(map[string]any{"ok": true, "pattern": p})
}

type neuralPredictArgs struct {
	Query string `json:"query"`
}

func handleNeuralPredict(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a neuralPredictArgs
	_ = parseArgs(args, &a)
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	var best *api.Pattern
	var bestScore float64
	q := strings.ToLower(a.Query)
	for i := range globalState.neural.Patterns {
		p := &globalState.neural.Patterns[i]
		sc := substringScore(q, p.Name+" "+p.Description)
		if sc > bestScore {
			bestScore = sc
			best = p
		}
	}
	return jsonOK(map[string]any{"pattern": best, "score": bestScore})
}

func handleNeuralPatterns(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	list := append([]api.Pattern(nil), globalState.neural.Patterns...)
	globalState.mu.RUnlock()
	return jsonOK(map[string]any{"patterns": list, "count": len(list)})
}

// handleNeuralCompress 按 Score 降序保留最多 max 条，默认 64，并持久化。
func handleNeuralCompress(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var in struct {
		Max int `json:"max"`
	}
	_ = parseArgs(args, &in)
	if in.Max <= 0 {
		in.Max = 64
	}
	globalState.mu.Lock()
	pat := globalState.neural.Patterns
	if len(pat) > in.Max {
		sort.Slice(pat, func(i, j int) bool { return pat[i].Score > pat[j].Score })
		globalState.neural.Patterns = pat[:in.Max]
	}
	n := len(globalState.neural.Patterns)
	globalState.mu.Unlock()
	saveNeuralToDisk()
	return jsonOK(map[string]any{"ok": true, "patterns": n})
}

// handleNeuralOptimize 按 Score 重排模式并更新 LastTrain。
func handleNeuralOptimize(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.Lock()
	pat := globalState.neural.Patterns
	sort.Slice(pat, func(i, j int) bool { return pat[i].Score > pat[j].Score })
	globalState.neural.LastTrain = now()
	n := len(globalState.neural.Patterns)
	globalState.mu.Unlock()
	saveNeuralToDisk()
	return jsonOK(map[string]any{"ok": true, "optimized": true, "patterns": n})
}

// handleNeuralStatus 返回模式数量、上次训练时间与后端标识（进程内）。
func handleNeuralStatus(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	n := len(globalState.neural.Patterns)
	lt := globalState.neural.LastTrain
	globalState.mu.RUnlock()
	return jsonOK(map[string]any{
		"patterns":   n,
		"last_train": lt,
		"backend":    "in-memory",
	})
}
