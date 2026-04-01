// 本文件实现 Poincaré 球（截面曲率 c>0）下的点表示、欧氏向量投影入球、双曲距离、欧氏弦中点回投影及 Möbius 加法。
// 层次/树状结构在双曲空间中可容纳更小的失真，适合作为层次语义的几何先验。
package embeddings

import "math"

// HyperbolicPoint 将一点表示为 Poincaré 球内的欧氏坐标 Coords 与正曲率 Curvature c；距离与 Möbius 运算均依赖 c。
type HyperbolicPoint struct {
	Coords    []float64 // 欧氏 ℝⁿ 中的位置向量；理论上应满足 c·‖x‖²<1
	Curvature float64   // 截面曲率 c>0；越大则球「越小」、边界越「陡」
}

// l2NormSq64 返回 Σxᵢ²，供距离公式与范数约束使用。
func l2NormSq64(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return s
}

// sub64 逐元素差 a-b，长度截断为 min(len(a),len(b))，避免越界。
func sub64(a, b []float64) []float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] - b[i]
	}
	return out
}

// add64Scaled 计算 a + sb·b，用于 Möbius 分子线性组合。
func add64Scaled(a, b []float64, sb float64) []float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] + sb*b[i]
	}
	return out
}

// dot64 截断长度后的欧氏内积 ⟨a,b⟩。
func dot64(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s float64
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}

// scale64 返回 s·v 的新切片。
func scale64(v []float64, s float64) []float64 {
	out := make([]float64, len(v))
	for i := range v {
		out[i] = v[i] * s
	}
	return out
}

// EuclideanToHyperbolic 将欧氏向量径向压缩：先算范数，用 tanh 与最大允许半径 1/√c-ε 控制落在球内，再按尺度写回各维。
func EuclideanToHyperbolic(vec []float32, curvature float64) HyperbolicPoint {
	c := curvature
	if c <= 0 {
		c = 1e-6
	}
	coords := make([]float64, len(vec))
	var norm float64
	for i, v := range vec {
		coords[i] = float64(v)
		norm += coords[i] * coords[i]
	}
	norm = math.Sqrt(norm)
	maxR := 1/math.Sqrt(c) - 1e-9
	if maxR <= 0 {
		maxR = 1e-6
	}
	var scale float64
	if norm == 0 {
		scale = 0
	} else {
		r := math.Tanh(norm) * maxR / norm
		if norm > 0 {
			scale = r / norm
		}
	}
	for i := range coords {
		coords[i] *= scale
	}
	return HyperbolicPoint{Coords: coords, Curvature: c}
}

// HyperbolicDistance 实现 Poincaré 距离公式 d=(1/√c)·acosh(1+2c·||x-y||²/((1-c||x||²)(1-c||y||²)))；分母≤0 或维数不等返回 NaN。
func HyperbolicDistance(a, b HyperbolicPoint) float64 {
	c := a.Curvature
	if c <= 0 {
		c = b.Curvature
	}
	if c <= 0 {
		c = 1e-6
	}
	if len(a.Coords) != len(b.Coords) {
		return math.NaN()
	}
	x2 := l2NormSq64(a.Coords)
	y2 := l2NormSq64(b.Coords)
	d2 := l2NormSq64(sub64(a.Coords, b.Coords))
	den := (1 - c*x2) * (1 - c*y2)
	if den <= 0 {
		return math.NaN()
	}
	arg := 1 + 2*c*d2/den
	if arg < 1 {
		arg = 1
	}
	return (1 / math.Sqrt(c)) * math.Acosh(arg)
}

// HyperbolicMidpoint 取 a、b 的欧氏算术平均，若超出允许球半径则径向缩放略小于边界，作为实用的「中点」近似。
func HyperbolicMidpoint(a, b HyperbolicPoint) HyperbolicPoint {
	c := a.Curvature
	if c <= 0 {
		c = b.Curvature
	}
	if c <= 0 {
		c = 1e-6
	}
	n := len(a.Coords)
	if len(b.Coords) < n {
		n = len(b.Coords)
	}
	m := make([]float64, n)
	for i := 0; i < n; i++ {
		m[i] = (a.Coords[i] + b.Coords[i]) / 2
	}
	x2 := l2NormSq64(m)
	maxR2 := 1/c - 1e-12
	if maxR2 <= 0 {
		maxR2 = 1e-12
	}
	if x2 >= maxR2 && x2 > 0 {
		s := math.Sqrt(maxR2/x2) * 0.999
		for i := range m {
			m[i] *= s
		}
	}
	return HyperbolicPoint{Coords: m, Curvature: c}
}

// MobiusAddition 计算 x⊕y 的标准闭式；分子为 (1+2⟨x,y⟩+‖y‖²)x + (1-‖x‖²)y，分母为 1+2⟨x,y⟩+‖x‖²‖y‖²。
// den=0 时返回零向量占位；若 c‖out‖²≥1 则径向乘以 0.999/√(c‖out‖²) 数值回投影。
func MobiusAddition(a, b HyperbolicPoint) HyperbolicPoint {
	c := a.Curvature
	if c <= 0 {
		c = b.Curvature
	}
	if c <= 0 {
		c = 1e-6
	}
	x, y := a.Coords, b.Coords
	n := len(x)
	if len(y) < n {
		n = len(y)
	}
	x = x[:n]
	y = y[:n]
	x2 := l2NormSq64(x)
	y2 := l2NormSq64(y)
	xy := dot64(x, y)
	num := add64Scaled(scale64(x, 1+2*xy+y2), y, 1-x2)
	den := 1 + 2*xy + x2*y2
	if den == 0 {
		return HyperbolicPoint{Coords: make([]float64, n), Curvature: c}
	}
	out := scale64(num, 1/den)
	// project if numerical error pushed outside ball
	if l2NormSq64(out)*c >= 1 {
		s := 0.999 / math.Sqrt(c*l2NormSq64(out))
		out = scale64(out, s)
	}
	return HyperbolicPoint{Coords: out, Curvature: c}
}
