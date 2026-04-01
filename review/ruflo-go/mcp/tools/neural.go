package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/embeddings"
	nlp "github.com/ruflo/ruflo-go/pkg/neural"
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

	content := strings.TrimSpace(a.Name + " " + a.Description)
	emb := embeddings.HashEmbed384(content)
	verdict := "success"
	if a.Score < 0 {
		verdict = "failure"
	}
	if globalState.sona != nil {
		tid := globalState.sona.BeginTrajectory("")
		md := map[string]string{
			"name":        a.Name,
			"description": a.Description,
			"score":       fmt.Sprintf("%g", a.Score),
			"legacy_id":   id,
		}
		globalState.sona.RecordStep(tid, nlp.TrajectoryStep{
			Type:      nlp.StepThought,
			Content:   content,
			Embedding: emb,
			Metadata:  md,
		})
		_ = globalState.sona.EndTrajectory(tid, verdict)
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

const neuralPredictTopK = 8

func handleNeuralPredict(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var a neuralPredictArgs
	_ = parseArgs(args, &a)
	var best *api.Pattern
	var bestScore float64
	q := strings.TrimSpace(a.Query)
	emb := embeddings.HashEmbed384(q)
	if globalState.sona != nil && len(emb) > 0 {
		matches := globalState.sona.FindSimilarPatterns(emb, neuralPredictTopK)
		if len(matches) > 0 && matches[0] != nil && len(matches[0].Embedding) == len(emb) {
			top := matches[0]
			conf := cosineFloat32(emb, top.Embedding)
			ap := apiPatternFromSONAPredict(top, conf)
			best = &ap
			bestScore = conf
		}
	}
	if best == nil {
		globalState.mu.RLock()
		qLower := strings.ToLower(q)
		for i := range globalState.neural.Patterns {
			pt := &globalState.neural.Patterns[i]
			sc := substringScore(qLower, pt.Name+" "+pt.Description)
			if sc > bestScore {
				bestScore = sc
				best = pt
			}
		}
		globalState.mu.RUnlock()
	}
	return jsonOK(map[string]any{"pattern": best, "score": bestScore})
}

func handleNeuralPatterns(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	base := append([]api.Pattern(nil), globalState.neural.Patterns...)
	globalState.mu.RUnlock()
	var sonaList []*nlp.Pattern
	if globalState.sona != nil {
		sonaList = globalState.sona.Patterns()
	}
	list := mergeAPIPatternsWithSONA(base, sonaList)
	return jsonOK(map[string]any{"patterns": list, "count": len(list)})
}

// handleNeuralCompress 先 SONA ConsolidatePatterns，再按 Score 降序保留最多 max 条（默认 64），并持久化 legacy 列表。
func handleNeuralCompress(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	var in struct {
		Max int `json:"max"`
	}
	_ = parseArgs(args, &in)
	if in.Max <= 0 {
		in.Max = 64
	}
	merged := 0
	if globalState.sona != nil {
		merged = globalState.sona.ConsolidatePatterns(0)
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
	return jsonOK(map[string]any{"ok": true, "patterns": n, "sona_consolidated_removed": merged})
}

// handleNeuralOptimize 调用 SONA 模式整合，并按 Score 重排 legacy 列表、更新 LastTrain。
func handleNeuralOptimize(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	merged := 0
	if globalState.sona != nil {
		merged = globalState.sona.ConsolidatePatterns(0)
	}
	globalState.mu.Lock()
	pat := globalState.neural.Patterns
	sort.Slice(pat, func(i, j int) bool { return pat[i].Score > pat[j].Score })
	globalState.neural.LastTrain = now()
	n := len(globalState.neural.Patterns)
	globalState.mu.Unlock()
	saveNeuralToDisk()
	return jsonOK(map[string]any{"ok": true, "optimized": true, "patterns": n, "sona_consolidated_removed": merged})
}

// handleNeuralStatus 聚合 SONA GetStats 与 legacy neural 列表指标。
func handleNeuralStatus(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	_ = ctx
	_ = args
	globalState.mu.RLock()
	nLegacy := len(globalState.neural.Patterns)
	lt := globalState.neural.LastTrain
	globalState.mu.RUnlock()
	out := map[string]any{
		"patterns":   nLegacy,
		"last_train": lt,
		"backend":    "sona+in-memory",
	}
	if globalState.sona != nil {
		st := globalState.sona.GetStats()
		out["sona"] = map[string]any{
			"total_patterns":       st.TotalPatterns,
			"total_trajectories":   st.TotalTrajectories,
			"active_trajectories":  st.ActiveTrajectories,
			"avg_confidence":       st.AvgConfidence,
			"signal_count":         st.SignalCount,
		}
	}
	return jsonOK(out)
}

func cosineFloat32(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func apiPatternFromSONAPredict(p *nlp.Pattern, cosineConfidence float64) api.Pattern {
	ap := apiPatternFromSONAList(p)
	ap.Score = cosineConfidence
	return ap
}

func apiPatternFromSONAList(p *nlp.Pattern) api.Pattern {
	if p == nil {
		return api.Pattern{}
	}
	name := ""
	desc := ""
	if p.Metadata != nil {
		name = p.Metadata["name"]
		desc = p.Metadata["description"]
	}
	if name == "" {
		name = strings.TrimSpace(p.Content)
		if name == "" {
			name = p.Type
		}
	}
	if desc == "" {
		desc = p.Content
	}
	return api.Pattern{
		ID:          p.ID,
		Name:        name,
		Description: desc,
		Score:       p.Confidence,
		CreatedAt:   p.CreatedAt,
	}
}

func mergeAPIPatternsWithSONA(neural []api.Pattern, sona []*nlp.Pattern) []api.Pattern {
	seen := make(map[string]struct{}, len(neural)+len(sona))
	out := make([]api.Pattern, 0, len(neural)+len(sona))
	for _, p := range neural {
		if _, ok := seen[p.ID]; ok {
			continue
		}
		seen[p.ID] = struct{}{}
		out = append(out, p)
	}
	for _, sp := range sona {
		if sp == nil {
			continue
		}
		ap := apiPatternFromSONAList(sp)
		if _, ok := seen[ap.ID]; ok {
			continue
		}
		seen[ap.ID] = struct{}{}
		out = append(out, ap)
	}
	return out
}
