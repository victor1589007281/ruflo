package swarm

import (
	"fmt"
	"math"
	"sort"
	"sync"
)

const flashBlockSize = 256

// AttentionCoordinator implements block attention, MoE-style routing, and linear pooling.
type AttentionCoordinator struct {
	mu sync.RWMutex
}

// NewAttentionCoordinator creates a coordinator.
func NewAttentionCoordinator() *AttentionCoordinator {
	return &AttentionCoordinator{}
}

// FlashBlock represents a fixed-size block for local attention.
type FlashBlock struct {
	Index  int
	Values []float64
}

// PairwiseAttention computes scaled dot-product style scores between blocks (O(blocks^2)).
func (a *AttentionCoordinator) PairwiseAttention(blocks []FlashBlock, scale float64) [][]float64 {
	if scale == 0 {
		scale = 1.0 / math.Sqrt(float64(flashBlockSize))
	}
	n := len(blocks)
	out := make([][]float64, n)
	for i := range out {
		out[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			out[i][j] = dot(blocks[i].Values, blocks[j].Values) * scale
		}
	}
	return out
}

func dot(a, b []float64) float64 {
	m := len(a)
	if len(b) < m {
		m = len(b)
	}
	var s float64
	for i := 0; i < m; i++ {
		s += a[i] * b[i]
	}
	return s
}

// MoEExpert is a named expert with confidence score.
type MoEExpert struct {
	Name       string
	Confidence float64
	Output     []float64
}

// RouteMoE selects top-K experts by confidence.
func (a *AttentionCoordinator) RouteMoE(experts []MoEExpert, k int) []MoEExpert {
	if k <= 0 {
		k = 2
	}
	cp := append([]MoEExpert(nil), experts...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Confidence > cp[j].Confidence })
	if k > len(cp) {
		k = len(cp)
	}
	return cp[:k]
}

// LinearReLUPool applies ReLU then L1-normalizes weights (O(n)).
func (a *AttentionCoordinator) LinearReLUPool(weights []float64) []float64 {
	out := make([]float64, len(weights))
	var sum float64
	for i, w := range weights {
		v := w
		if v < 0 {
			v = 0
		}
		out[i] = v
		sum += v
	}
	if sum == 0 {
		n := float64(len(out))
		for i := range out {
			out[i] = 1.0 / n
		}
		return out
	}
	for i := range out {
		out[i] /= sum
	}
	return out
}

// WeightedConsensus picks the highest-weight agent output.
func (a *AttentionCoordinator) WeightedConsensus(agentIDs []string, weights []float64, outputs map[string][]float64) ([]float64, error) {
	if len(agentIDs) != len(weights) {
		return nil, fmt.Errorf("attention: ids/weights length mismatch")
	}
	bestIdx := -1
	bestW := math.Inf(-1)
	for i := range agentIDs {
		if weights[i] > bestW {
			bestW = weights[i]
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return nil, fmt.Errorf("attention: no agents")
	}
	id := agentIDs[bestIdx]
	out, ok := outputs[id]
	if !ok {
		return nil, fmt.Errorf("attention: missing output for %s", id)
	}
	cp := append([]float64(nil), out...)
	return cp, nil
}

// BlockTensor splits a flat vector into 256-width blocks (truncate/pad last).
func (a *AttentionCoordinator) BlockTensor(vec []float64) []FlashBlock {
	if len(vec) == 0 {
		return nil
	}
	var blocks []FlashBlock
	for i := 0; i < len(vec); i += flashBlockSize {
		end := i + flashBlockSize
		if end > len(vec) {
			end = len(vec)
		}
		chunk := make([]float64, flashBlockSize)
		copy(chunk, vec[i:end])
		blocks = append(blocks, FlashBlock{Index: len(blocks), Values: chunk})
	}
	return blocks
}
