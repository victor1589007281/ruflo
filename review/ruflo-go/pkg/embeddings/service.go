package embeddings

import (
	"errors"
	"math"
	"sync"
)

// EmbeddingProvider produces dense vectors from text.
type EmbeddingProvider interface {
	Name() string
	Embed(text string) ([]float32, error)
	BatchEmbed(texts []string) ([][]float32, error)
	Dimensions() int
}

// HashEmbeddingProvider wraps HashEmbed384 for EmbeddingProvider.
type HashEmbeddingProvider struct{}

// Name implements EmbeddingProvider.
func (HashEmbeddingProvider) Name() string { return "hash384" }

// Embed implements EmbeddingProvider.
func (HashEmbeddingProvider) Embed(text string) ([]float32, error) {
	return HashEmbed384(text), nil
}

// BatchEmbed implements EmbeddingProvider.
func (HashEmbeddingProvider) BatchEmbed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = HashEmbed384(t)
	}
	return out, nil
}

// Dimensions implements EmbeddingProvider.
func (HashEmbeddingProvider) Dimensions() int { return HashEmbeddingDim }

// EmbeddingService caches embeddings and delegates to a provider.
type EmbeddingService struct {
	mu        sync.RWMutex
	provider  EmbeddingProvider
	cache     map[string][]float32
	cacheSize int
	keyOrder  []string
}

// NewEmbeddingService builds a service with the hash provider and LRU-ish eviction.
func NewEmbeddingService(cacheSize int) *EmbeddingService {
	if cacheSize < 0 {
		cacheSize = 0
	}
	return &EmbeddingService{
		provider:  HashEmbeddingProvider{},
		cache:     make(map[string][]float32),
		cacheSize: cacheSize,
	}
}

func (s *EmbeddingService) rememberLocked(key string, vec []float32) {
	if s.cacheSize <= 0 {
		return
	}
	cp := append([]float32(nil), vec...)
	if _, ok := s.cache[key]; !ok {
		s.keyOrder = append(s.keyOrder, key)
	}
	s.cache[key] = cp
	for len(s.cache) > s.cacheSize && len(s.keyOrder) > 0 {
		old := s.keyOrder[0]
		s.keyOrder = s.keyOrder[1:]
		delete(s.cache, old)
	}
}

func (s *EmbeddingService) embedLocked(text string) ([]float32, error) {
	if s.provider == nil {
		return nil, errors.New("embeddings: no provider")
	}
	if s.cacheSize > 0 {
		if v, ok := s.cache[text]; ok {
			return append([]float32(nil), v...), nil
		}
	}
	v, err := s.provider.Embed(text)
	if err != nil {
		return nil, err
	}
	s.rememberLocked(text, v)
	return append([]float32(nil), v...), nil
}

// Embed returns the embedding for text, using cache when enabled.
func (s *EmbeddingService) Embed(text string) ([]float32, error) {
	if s == nil {
		return nil, errors.New("embeddings: nil service")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.embedLocked(text)
}

// BatchEmbed embeds each string; uses cache per item when enabled.
func (s *EmbeddingService) BatchEmbed(texts []string) ([][]float32, error) {
	if s == nil {
		return nil, errors.New("embeddings: nil service")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := s.embedLocked(t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Compare returns cosine similarity in [−1,1] for unit vectors (dot product).
func (s *EmbeddingService) Compare(a, b string) (float64, error) {
	if s == nil {
		return 0, errors.New("embeddings: nil service")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	va, err := s.embedLocked(a)
	if err != nil {
		return 0, err
	}
	vb, err := s.embedLocked(b)
	if err != nil {
		return 0, err
	}
	if len(va) != len(vb) {
		return 0, errors.New("embeddings: dimension mismatch")
	}
	var dot float64
	for i := range va {
		dot += float64(va[i]) * float64(vb[i])
	}
	if dot > 1 {
		dot = 1
	}
	if dot < -1 {
		dot = -1
	}
	return dot, nil
}

// SetProvider replaces the active provider and clears the cache.
func (s *EmbeddingService) SetProvider(p EmbeddingProvider) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = p
	s.cache = make(map[string][]float32)
	s.keyOrder = nil
}

// GetProvider returns the current provider or nil.
func (s *EmbeddingService) GetProvider() EmbeddingProvider {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.provider
}

// CosineSimilarity returns dot(a,b)/(||a||*||b||) with a safe zero check.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	v := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}
