// Package embeddings 提供不依赖 ONNX / 深度模型的轻量向量管线：哈希嵌入、文本分块、多种归一化与 Poincaré 双曲工具。
//
// 哈希嵌入子系统（本文件）：用 FNV-1a 等确定性哈希将任意 UTF-8 文本映射到固定维度（默认 384，与常见 sentence-transformers 维度对齐），
// 再 L2 归一化到单位球面；适合离线、无 GPU、或仅需稳定向量键的场景。语义相似性弱于学习型嵌入，但零外部模型成本。
package embeddings

import (
	"encoding/binary"
	"hash/fnv"
	"math"
)

// HashEmbeddingDim 与常见 MiniLM-L6 等 384 维嵌入对齐，便于与学习型向量并存于同一索引维度。
const HashEmbeddingDim = 384

// HashEmbed384 将文本映射为 HashEmbeddingDim 维 L2 单位向量。
// 算法：① 空串返回 e₀=1 的平凡基向量；② h₁ := FNV-1a(text) 得 64 位 seed；③ 对每个维度 i，构造 16 字节小端缓冲 (seed‖i)，
// 与 text 字节拼接后再 FNV-1a，将 64 位哈希映射到 [-1,1] 浮点（高位丢弃符号位线性缩放）；④ 全维生成后 normalizeL2。
// 性质：确定性、O(维度×文本长) 时间，适合缓存键与可复现实验。
func HashEmbed384(text string) []float32 {
	if text == "" {
		v := make([]float32, HashEmbeddingDim)
		v[0] = 1
		return v
	}
	vec := make([]float32, HashEmbeddingDim)
	h1 := fnv.New64a()
	_, _ = h1.Write([]byte(text))
	seed := h1.Sum64()
	for i := 0; i < HashEmbeddingDim; i++ {
		h := fnv.New64a()
		var buf [16]byte
		binary.LittleEndian.PutUint64(buf[:8], seed)
		binary.LittleEndian.PutUint64(buf[8:], uint64(i))
		_, _ = h.Write(buf[:])
		_, _ = h.Write([]byte(text))
		u := h.Sum64()
		x := (float64(u>>1)/float64(1<<63))*2 - 1
		vec[i] = float32(x)
	}
	normalizeL2(vec)
	return vec
}

// normalizeL2 原地计算 L2 范数 ‖v‖₂ = √(Σxᵢ²)；若范数为 0 则将 v[0]=1（与包内 NormalizeL2 零向量策略一致），否则各分量乘以 1/‖v‖₂。
func normalizeL2(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		if len(v) > 0 {
			v[0] = 1
		}
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}
