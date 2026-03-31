// Package embeddings provides deterministic vector fallbacks when ONNX is unavailable.
package embeddings

import (
	"encoding/binary"
	"hash/fnv"
	"math"
)

// HashEmbeddingDim matches common MiniLM-L6 style models (384).
const HashEmbeddingDim = 384

// HashEmbed384 produces a 384-dimensional unit vector from text using deterministic hashing.
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
