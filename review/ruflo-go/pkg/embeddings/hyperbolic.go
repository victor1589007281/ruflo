package embeddings

import "math"

// HyperbolicPoint is a point in the Poincaré ball with sectional curvature c>0.
type HyperbolicPoint struct {
	Coords    []float64
	Curvature float64
}

func l2NormSq64(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return s
}

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

func scale64(v []float64, s float64) []float64 {
	out := make([]float64, len(v))
	for i := range v {
		out[i] = v[i] * s
	}
	return out
}

// EuclideanToHyperbolic maps a Euclidean vector into the Poincaré ball for curvature c>0.
// Coordinates are scaled so that c·||x||² < 1 (tanh radial projection).
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

// HyperbolicDistance is the Poincaré distance:
// d(x,y) = (1/sqrt(c)) * arcosh(1 + 2c * ||x-y||² / ((1-c*||x||²)(1-c*||y||²)))
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

// HyperbolicMidpoint returns the Euclidean chord midpoint projected back into the ball.
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

// MobiusAddition is Möbius addition in the Poincaré ball (same dimension as a.Coords).
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
