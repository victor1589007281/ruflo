package embeddings

import (
	"math"
	"testing"
)

func TestHashEmbed384_Deterministic(t *testing.T) {
	t.Parallel()
	a := HashEmbed384("hello")
	b := HashEmbed384("hello")
	if len(a) != HashEmbeddingDim || len(b) != HashEmbeddingDim {
		t.Fatalf("dim %d %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("mismatch at %d", i)
		}
	}
}

func TestHashEmbed384_UnitVector(t *testing.T) {
	t.Parallel()
	v := HashEmbed384("unit test vector")
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if math.Abs(sum-1) > 1e-4 {
		t.Fatalf("norm^2=%v want 1", sum)
	}
}

func TestChunkText(t *testing.T) {
	t.Parallel()
	cfg := ChunkConfig{MaxChunkSize: 20, Overlap: 5, Separator: "\n"}
	ch := ChunkText("0123456789012345678901234567890", cfg)
	if len(ch) < 2 {
		t.Fatalf("chunks=%d", len(ch))
	}
	for i, c := range ch {
		if c.Index != i || c.End <= c.Start || c.Text != "0123456789012345678901234567890"[c.Start:c.End] {
			t.Fatalf("bad chunk %+v", c)
		}
	}
}

func TestNormalizeL2(t *testing.T) {
	t.Parallel()
	v := NormalizeL2([]float32{3, 4})
	if len(v) != 2 {
		t.Fatal(v)
	}
	var s float64
	for _, x := range v {
		s += float64(x * x)
	}
	if math.Abs(s-1) > 1e-5 {
		t.Fatalf("norm %v", s)
	}
}

func TestHyperbolicDistance(t *testing.T) {
	t.Parallel()
	p := EuclideanToHyperbolic([]float32{0.1, 0.2}, 1.0)
	q := EuclideanToHyperbolic([]float32{0.1, 0.2}, 1.0)
	d := HyperbolicDistance(p, q)
	if math.Abs(d) > 1e-6 {
		t.Fatalf("self distance %v", d)
	}
	r := EuclideanToHyperbolic([]float32{0.5, -0.1}, 1.0)
	d2 := HyperbolicDistance(p, r)
	if d2 <= 0 || math.IsNaN(d2) {
		t.Fatalf("distance %v", d2)
	}
}

func TestEmbeddingService_CompareIdentical(t *testing.T) {
	t.Parallel()
	s := NewEmbeddingService(16)
	sim, err := s.Compare("identical", "identical")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(sim-1) > 1e-4 {
		t.Fatalf("similarity %v", sim)
	}
}
