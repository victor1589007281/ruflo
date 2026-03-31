package embeddings

import "math"

// NormalizeL2 returns an L2-normalized copy of vec (zero vector maps to e1).
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

// NormalizeL1 returns an L1-normalized copy (sum of absolute values = 1).
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

// NormalizeMinMax scales each element to [0,1] using min/max of the slice.
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

// NormalizeZScore returns (vec-mean)/std per element; if std==0, returns a copy unchanged.
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
