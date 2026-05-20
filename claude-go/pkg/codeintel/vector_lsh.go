// vector_lsh.go — MinHash LSH 近似最近邻索引（超大规模缓存 >100K）。
//
// 设计:
//   - 将高维稀疏特征向量通过 MinHash 压缩为低维签名
//   - 使用 Banding 技术分桶：相似向量高概率落入同一桶
//   - 查询时只需检查匹配桶中的候选集，O(1) 桶查找 + O(k) 候选计算
//   - 仅使用标准库（hash/fnv）
package codeintel

import (
	"hash/fnv"
	"math"
)

const (
	defaultNumHashFunctions = 128
	defaultNumBands         = 16
	defaultRowsPerBand      = 8 // numHashFunctions / numBands
	lshThreshold            = 100000
	lshPrime                = 4294967311 // 略大于 2^32 的素数
)

// ============================================================================
// MinHash LSH
// ============================================================================

// MinHashLSH MinHash + LSH 近似最近邻索引。
type MinHashLSH struct {
	numHashFunctions int
	numBands         int
	rowsPerBand      int
	// hashCoeffs[i] = {a, b} for hash_i(x) = (a*x + b) % prime
	hashCoeffs [][2]uint64
	// buckets[bandIndex][bucketKey] = []entryID
	buckets []map[string][]int
}

// newMinHashLSH 创建 MinHash LSH 索引。
func newMinHashLSH() *MinHashLSH {
	lsh := &MinHashLSH{
		numHashFunctions: defaultNumHashFunctions,
		numBands:         defaultNumBands,
		rowsPerBand:      defaultRowsPerBand,
		hashCoeffs:       make([][2]uint64, defaultNumHashFunctions),
		buckets:          make([]map[string][]int, defaultNumBands),
	}
	// 使用确定性伪随机系数（保证重建一致性）
	// 基于 SplitMix64 算法生成
	seed := uint64(0x9e3779b97f4a7c15)
	for i := 0; i < defaultNumHashFunctions; i++ {
		seed += 0x9e3779b97f4a7c15
		z := seed
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		a := z ^ (z >> 31)
		seed += 0x9e3779b97f4a7c15
		z = seed
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		b := z ^ (z >> 31)
		lsh.hashCoeffs[i] = [2]uint64{a | 1, b} // a 必须是奇数
	}
	for i := 0; i < defaultNumBands; i++ {
		lsh.buckets[i] = make(map[string][]int)
	}
	return lsh
}

// hashFeature 计算单个特征的 FNV-1a 哈希值。
func hashFeature(feature string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(feature))
	return h.Sum64()
}

// computeSignature 计算特征向量的 MinHash 签名。
func (lsh *MinHashLSH) computeSignature(features map[string]float64) []uint64 {
	sig := make([]uint64, lsh.numHashFunctions)
	for i := range sig {
		sig[i] = math.MaxUint64
	}

	for feat := range features {
		hv := hashFeature(feat)
		for i := 0; i < lsh.numHashFunctions; i++ {
			coeff := lsh.hashCoeffs[i]
			// hash_i(x) = (a*x + b) % prime
			hashed := (coeff[0]*hv + coeff[1]) % lshPrime
			if hashed < sig[i] {
				sig[i] = hashed
			}
		}
	}
	return sig
}

// bandKey 计算指定 band 的桶键。
func (lsh *MinHashLSH) bandKey(signature []uint64, bandIdx int) string {
	start := bandIdx * lsh.rowsPerBand
	end := start + lsh.rowsPerBand
	if end > len(signature) {
		end = len(signature)
	}
	// 将 band 内签名拼接为字符串键
	// 使用 FNV 压缩
	h := fnv.New64a()
	for i := start; i < end; i++ {
		b := []byte{
			byte(signature[i]),
			byte(signature[i] >> 8),
			byte(signature[i] >> 16),
			byte(signature[i] >> 24),
			byte(signature[i] >> 32),
			byte(signature[i] >> 40),
			byte(signature[i] >> 48),
			byte(signature[i] >> 56),
		}
		h.Write(b)
	}
	// 转为 16 进制字符串
	return uint64ToHex(h.Sum64())
}

// uint64ToHex 将 uint64 转为 16 进制字符串。
func uint64ToHex(v uint64) string {
	const hexDigits = "0123456789abcdef"
	buf := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		buf[i] = hexDigits[v&0xf]
		v >>= 4
	}
	return string(buf)
}

// Add 将条目加入 LSH 索引。
func (lsh *MinHashLSH) Add(entryID int, features map[string]float64) {
	if lsh == nil {
		return
	}
	sig := lsh.computeSignature(features)
	for bandIdx := 0; bandIdx < lsh.numBands; bandIdx++ {
		key := lsh.bandKey(sig, bandIdx)
		lsh.buckets[bandIdx][key] = append(lsh.buckets[bandIdx][key], entryID)
	}
}

// Query 返回候选条目 ID 列表（可能包含 false positive，需二次精确验证）。
func (lsh *MinHashLSH) Query(features map[string]float64) []int {
	if lsh == nil {
		return nil
	}
	sig := lsh.computeSignature(features)
	candidateSet := make(map[int]struct{})
	for bandIdx := 0; bandIdx < lsh.numBands; bandIdx++ {
		key := lsh.bandKey(sig, bandIdx)
		if ids, ok := lsh.buckets[bandIdx][key]; ok {
			for _, id := range ids {
				candidateSet[id] = struct{}{}
			}
		}
	}
	candidates := make([]int, 0, len(candidateSet))
	for id := range candidateSet {
		candidates = append(candidates, id)
	}
	return candidates
}

// Rebuild 重建整个 LSH 索引。
func (lsh *MinHashLSH) Rebuild(entries []vectorCacheEntry) {
	if lsh == nil {
		return
	}
	// 清空桶
	for i := 0; i < lsh.numBands; i++ {
		lsh.buckets[i] = make(map[string][]int)
	}
	// 重新插入
	for i, ent := range entries {
		lsh.Add(i, ent.Features)
	}
}

// EstimateSimilarityThreshold 根据 band 参数估计可检测的最小相似度。
// 公式: s = (1/bands)^(1/rows)
func (lsh *MinHashLSH) EstimateSimilarityThreshold() float64 {
	return math.Pow(1.0/float64(lsh.numBands), 1.0/float64(lsh.rowsPerBand))
}
