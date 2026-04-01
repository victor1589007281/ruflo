// 注意力协调器（swarm 包）：对多 Agent 向量块做类 scaled dot-product 打分、MoE Top-K 路由、ReLU+L1 归一化权重与加权共识输出选择，
// 用于融合并行子代理结果或路由专家网络（实现为纯数值例程，无外部 IO）。
package swarm

import (
	"fmt"
	"math"
	"sort"
	"sync"
)

const flashBlockSize = 256

// AttentionCoordinator 占位互斥体，便于未来扩展有状态注意力缓存。
type AttentionCoordinator struct {
	mu sync.RWMutex
}

// NewAttentionCoordinator 构造无状态协调器实例。
func NewAttentionCoordinator() *AttentionCoordinator {
	return &AttentionCoordinator{}
}

// FlashBlock 固定宽度（与 flashBlockSize 对齐）的注意力块，Values 可截断/补零。
type FlashBlock struct {
	Index  int       // 块序号
	Values []float64 // 块内向量（长度通常为 flashBlockSize）
}

// PairwiseAttention 计算所有块对之间的点积并乘以 scale，复杂度 O(n^2·d)；scale 默认 1/sqrt(blockSize)。
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

// dot 取两向量较短长度上的内积。
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

// MoEExpert 混合专家中的一路：名称、置信度与输出向量。
type MoEExpert struct {
	Name       string    // 专家标识
	Confidence float64   // 路由打分依据
	Output     []float64 // 专家输出嵌入
}

// RouteMoE 按 Confidence 降序排序后截断前 k 个（k 默认 2）。
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

// LinearReLUPool 对权重做 ReLU 后 L1 归一化；若全零则均匀分布。
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

// WeightedConsensus 选权重最大 Agent 的输出向量拷贝（非加权求和）。
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

// BlockTensor 将一维向量按 flashBlockSize 分块，最后一块右侧零填充至块宽。
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
