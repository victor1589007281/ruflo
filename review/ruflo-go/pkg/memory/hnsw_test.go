package memory

import (
	"math"
	"math/rand"
	"sync"
	"testing"
)

func TestHNSWInsertAndSearch(t *testing.T) {
	t.Parallel()
	const dim = 16
	idx := NewHNSWIndex(dim, CosineDistance)
	rng := rand.New(rand.NewSource(42))

	for id := uint64(1); id <= 100; id++ {
		v := make([]float32, dim)
		for i := range v {
			v[i] = rng.Float32()
		}
		if err := idx.Insert(id, v); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
	}

	query := make([]float32, dim)
	for i := range query {
		query[i] = rng.Float32()
	}
	hits := idx.Search(query, 5, 32)
	if len(hits) != 5 {
		t.Fatalf("expected 5 hits, got %d", len(hits))
	}
	seen := make(map[uint64]struct{})
	for _, h := range hits {
		if h.ID < 1 || h.ID > 100 {
			t.Errorf("unexpected id %d", h.ID)
		}
		if _, ok := seen[h.ID]; ok {
			t.Errorf("duplicate id %d", h.ID)
		}
		seen[h.ID] = struct{}{}
		if h.Distance < 0 || math.IsNaN(float64(h.Distance)) {
			t.Errorf("invalid distance %v", h.Distance)
		}
	}
}

func TestHNSWCosineDistance(t *testing.T) {
	t.Parallel()
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if d := distanceCosine(a, b); d > 1e-5 {
		t.Fatalf("identical direction: want ~0, got %v", d)
	}
	if d := distanceCosine(a, []float32{0, 1, 0}); math.Abs(float64(d-1)) > 1e-5 {
		t.Fatalf("orthogonal: want 1, got %v", d)
	}
	if d := distanceCosine(a, []float32{-1, 0, 0}); math.Abs(float64(d-2)) > 1e-5 {
		t.Fatalf("opposite: want 2, got %v", d)
	}
}

func TestHNSWDelete(t *testing.T) {
	t.Parallel()
	idx := NewHNSWIndex(3, CosineDistance)
	_ = idx.Insert(1, []float32{1, 0, 0})
	_ = idx.Insert(2, []float32{0, 1, 0})
	_ = idx.Insert(3, []float32{0, 0, 1})

	idx.Delete(2)
	hits := idx.Search([]float32{0, 1, 0}, 5, 16)
	for _, h := range hits {
		if h.ID == 2 {
			t.Fatalf("deleted id 2 still returned")
		}
	}
}

func TestHNSWEmptySearch(t *testing.T) {
	t.Parallel()
	idx := NewHNSWIndex(4, CosineDistance)
	if hits := idx.Search([]float32{1, 0, 0, 0}, 5, 16); hits != nil {
		t.Fatalf("empty index: want nil, got %#v", hits)
	}
}

func TestHNSWConcurrency(t *testing.T) {
	t.Parallel()
	const dim = 8
	idx := NewHNSWIndex(dim, EuclideanDistance)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(base uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(base)))
			for i := uint64(0); i < 50; i++ {
				id := base*1000 + i
				v := make([]float32, dim)
				for j := range v {
					v[j] = rng.Float32()
				}
				_ = idx.Insert(id, v)
				q := make([]float32, dim)
				for j := range q {
					q[j] = rng.Float32()
				}
				_ = idx.Search(q, 3, 24)
			}
		}(uint64(w))
	}
	wg.Wait()
}

func TestHNSWStats(t *testing.T) {
	t.Parallel()
	idx := NewHNSWIndex(4, CosineDistance)
	_ = idx.Insert(1, []float32{1, 0, 0, 0})
	_ = idx.Insert(2, []float32{0, 1, 0, 0})
	st := idx.Stats()
	if st.NodeCount != 2 || st.Dimensions != 4 {
		t.Fatalf("Stats: %#v", st)
	}
	if st.M <= 0 || st.EfSearch <= 0 {
		t.Fatalf("expected positive hyperparams: %#v", st)
	}
}

func TestHNSWClear(t *testing.T) {
	t.Parallel()
	idx := NewHNSWIndex(3, CosineDistance)
	_ = idx.Insert(1, []float32{1, 0, 0})
	idx.Clear()
	if idx.Size() != 0 {
		t.Fatalf("Size after Clear: %d", idx.Size())
	}
	if hits := idx.Search([]float32{1, 0, 0}, 3, 8); hits != nil {
		t.Fatalf("search after clear: %#v", hits)
	}
}

func TestHNSWHas(t *testing.T) {
	t.Parallel()
	idx := NewHNSWIndex(3, CosineDistance)
	_ = idx.Insert(42, []float32{1, 0, 0})
	if !idx.Has(42) {
		t.Fatal("expected Has(42)")
	}
	if idx.Has(99) {
		t.Fatal("unexpected Has(99)")
	}
	idx.Delete(42)
	if idx.Has(42) {
		t.Fatal("deleted id should not Has")
	}
}

func TestHNSWSize(t *testing.T) {
	t.Parallel()
	idx := NewHNSWIndex(2, EuclideanDistance)
	for i := uint64(1); i <= 7; i++ {
		_ = idx.Insert(i, []float32{float32(i), 0})
	}
	if idx.Size() != 7 {
		t.Fatalf("Size: %d", idx.Size())
	}
}

func TestHNSWRebuild(t *testing.T) {
	t.Parallel()
	idx := NewHNSWIndex(4, CosineDistance)
	for i := uint64(1); i <= 30; i++ {
		v := []float32{float32(i) * 0.01, float32(i) * 0.02, 0.5, 0.25}
		_ = idx.Insert(i, v)
	}
	q := []float32{0.15, 0.3, 0.5, 0.25}
	before := idx.Search(q, 5, 64)
	if len(before) != 5 {
		t.Fatalf("before rebuild: %d hits", len(before))
	}
	idx.Rebuild()
	after := idx.Search(q, 5, 64)
	if len(after) != 5 {
		t.Fatalf("after rebuild: %d hits", len(after))
	}
	seen := make(map[uint64]struct{})
	for _, h := range after {
		seen[h.ID] = struct{}{}
	}
	if len(seen) != 5 {
		t.Fatalf("duplicate or missing ids after rebuild")
	}
}
