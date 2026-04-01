// 本文件提供常用向量归一化：L2（单位球面）、L1（概率单纯形）、Min-Max 到 [0,1]、以及给定均值方差的 Z-Score。
package embeddings

import "math"

// NormalizeL2 返回 L2 范数为 1 的拷贝；零向量时置第一维为 1 其余为 0（与哈希嵌入内部 normalizeL2 行为对齐思路）。
func NormalizeL2(vec []float32) []float32 {
	if len(vec) == 0 {
		return nil
	}
	out := append([]float32(nil), vec...)
	var sum float64
	for _, x := range out {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		out[0] = 1
		for i := 1; i < len(out); i++ {
			out[i] = 0
		}
		return out
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range out {
		out[i] *= inv
	}
	return out
}

// NormalizeL1 将各分量按绝对值之和缩放，使 ||v||_1=1；全零时仅 v[0]=1。
func NormalizeL1(vec []float32) []float32 {
	if len(vec) == 0 {
		return nil
	}
	out := append([]float32(nil), vec...)
	var sum float64
	for _, x := range out {
		sum += math.Abs(float64(x))
	}
	if sum == 0 {
		if len(out) > 0 {
			out[0] = 1
		}
		return out
	}
	inv := float32(1 / sum)
	for i := range out {
		out[i] *= inv
	}
	return out
}

// NormalizeMinMax 用切片内 min/max 做线性仿射映射到 [0,1]；极差为 0 时全置 0。
func NormalizeMinMax(vec []float32) []float32 {
	if len(vec) == 0 {
		return nil
	}
	out := append([]float32(nil), vec...)
	minV, maxV := out[0], out[0]
	for _, x := range out[1:] {
		if x < minV {
			minV = x
		}
		if x > maxV {
			maxV = x
		}
	}
	d := float64(maxV - minV)
	if d == 0 {
		for i := range out {
			out[i] = 0
		}
		return out
	}
	for i, x := range out {
		out[i] = float32((float64(x - minV)) / d)
	}
	return out
}

// NormalizeZScore 逐元素减 mean 再除以 std；std==0 时直接返回拷贝，避免 NaN。mean/std 通常来自训练集统计量。
func NormalizeZScore(vec []float32, mean, std float32) []float32 {
	if len(vec) == 0 {
		return nil
	}
	out := append([]float32(nil), vec...)
	if std == 0 {
		return out
	}
	for i, x := range out {
		out[i] = (x - mean) / std
	}
	return out
}
