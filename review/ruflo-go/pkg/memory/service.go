package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/pkg/embeddings"
)

// MemoryService is the unified memory facade used across orchestration modules.
type MemoryService interface {
	Store(entry MemoryEntryInput) error
	Retrieve(key, namespace string) (*api.MemoryEntry, error)
	GetByID(id int64) (*api.MemoryEntry, error)
	Update(id int64, value string) error
	BulkInsert(entries []MemoryEntryInput) (int, error)
	BulkDelete(keys []string, namespace string) (int, error)
	ClearNamespace(namespace string) (int, error)
	Count(namespace string) (int, error)
	HealthCheck() error
	Search(query string, opts api.SearchOptions) ([]api.SearchResult, error)
	FindSimilar(key, namespace string, topK int) ([]api.SearchResult, error)
	SearchWithEmbedding(embedding []float32, namespace string, topK int) ([]api.SearchResult, error)
	Delete(key, namespace string) error
	List(namespace string, limit, offset int) ([]api.MemoryEntry, error)
	ListNamespaces(prefix string) ([]string, error)
	Stats() *MemoryStats
	Initialize() error
	IsInitialized() bool
	Close() error
}

// UnifiedMemoryService combines SQLite persistence, HNSW vector search, and an LRU cache.
type UnifiedMemoryService struct {
	mu       sync.RWMutex
	sql      *SQLiteBackend
	index    *HNSWIndex
	cache    *LRUCache
	dim      int
	ef       int
	hits     atomic.Int64
	misses   atomic.Int64
	closed   bool
	initOnce      sync.Once
	initErr       error
	initialized   atomic.Bool
}

// NewUnifiedMemoryService constructs a service. Call Initialize before use.
func NewUnifiedMemoryService(sqlPath string, dim int, ef int) (*UnifiedMemoryService, error) {
	if dim <= 0 {
		dim = embeddings.HashEmbeddingDim
	}
	if ef <= 0 {
		ef = 64
	}
	sqlb, err := OpenSQLite(sqlPath)
	if err != nil {
		return nil, err
	}
	return &UnifiedMemoryService{
		sql:   sqlb,
		index: NewHNSWIndex(dim, CosineDistance),
		cache: NewLRUCache(4096, 5*time.Minute),
		dim:   dim,
		ef:    ef,
	}, nil
}

// Initialize loads existing embeddings into the HNSW graph.
func (s *UnifiedMemoryService) Initialize() error {
	s.initOnce.Do(func() {
		rows, err := s.sql.ListEmbeddings()
		if err != nil {
			s.initErr = err
			return
		}
		for _, r := range rows {
			if len(r.Embedding) != s.dim {
				continue
			}
			if err := s.index.Insert(uint64(r.ID), r.Embedding); err != nil {
				s.initErr = err
				return
			}
		}
		if s.initErr == nil {
			s.initialized.Store(true)
		}
	})
	return s.initErr
}

// IsInitialized reports whether Initialize completed without error.
func (s *UnifiedMemoryService) IsInitialized() bool {
	if s == nil {
		return false
	}
	return s.initialized.Load()
}

// Close releases backend resources.
func (s *UnifiedMemoryService) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.sql.Close()
}

func (s *UnifiedMemoryService) cacheKey(key, ns string) string {
	return ns + "\x00" + key
}

// Store persists an entry and updates the vector index.
func (s *UnifiedMemoryService) Store(entry MemoryEntryInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("memory: service closed")
	}
	vec := entry.Embedding
	if len(vec) == 0 {
		vec = embeddings.HashEmbed384(entry.Value)
	}
	if len(vec) != s.dim {
		return fmt.Errorf("memory: embedding dim %d, expected %d", len(vec), s.dim)
	}
	rec, err := s.sql.UpsertEntry(entry, vec)
	if err != nil {
		return err
	}
	if err := s.index.Insert(uint64(rec.ID), vec); err != nil {
		return err
	}
	s.cache.Set(s.cacheKey(entry.Key, entry.Namespace), rec)
	return nil
}

// Retrieve fetches by key with cache.
func (s *UnifiedMemoryService) Retrieve(key, namespace string) (*api.MemoryEntry, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, errors.New("memory: service closed")
	}
	s.mu.RUnlock()
	ck := s.cacheKey(key, namespace)
	if v, ok := s.cache.Get(ck); ok {
		if e, ok2 := v.(*api.MemoryEntry); ok2 {
			s.hits.Add(1)
			return e, nil
		}
	}
	s.misses.Add(1)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	e, err := s.sql.GetByKey(key, namespace)
	if err != nil {
		return nil, err
	}
	s.cache.Set(ck, e)
	return e, nil
}

// Search performs vector search merged with optional namespace/tag filters.
func (s *UnifiedMemoryService) Search(query string, opts api.SearchOptions) ([]api.SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	k := opts.K
	if k <= 0 {
		k = 10
	}
	ef := opts.EF
	if ef <= 0 {
		ef = s.ef
	}
	qv := embeddings.HashEmbed384(query)
	if len(qv) != s.dim {
		return nil, fmt.Errorf("memory: query embedding dim %d, expected %d", len(qv), s.dim)
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	breadth := (k + offset + 8) * 4
	if breadth < k*4 {
		breadth = k * 4
	}
	if breadth > 2000 {
		breadth = 2000
	}
	hits := s.index.Search(qv, breadth, ef)
	candidates := make([]api.SearchResult, 0, breadth)
	for _, h := range hits {
		ent, err := s.sql.GetByID(int64(h.ID))
		if err != nil {
			continue
		}
		if opts.Namespace != "" && ent.Namespace != opts.Namespace {
			continue
		}
		if len(opts.Tags) > 0 && !entryHasTags(ent, opts.Tags) {
			continue
		}
		if len(opts.Filters) > 0 && !entryMetadataMatches(ent, opts.Filters) {
			continue
		}
		score := 1 - float64(h.Distance)
		if opts.MinScore > 0 && score < opts.MinScore {
			continue
		}
		candidates = append(candidates, api.SearchResult{Entry: ent, Score: score})
	}
	order := strings.TrimSpace(strings.ToLower(opts.OrderBy))
	if order != "" {
		desc := opts.Descending
		sort.SliceStable(candidates, func(i, j int) bool {
			var less bool
			switch order {
			case "updated_at", "updated":
				less = candidates[i].Entry.UpdatedAt.Before(candidates[j].Entry.UpdatedAt)
			case "created_at", "created":
				less = candidates[i].Entry.CreatedAt.Before(candidates[j].Entry.CreatedAt)
			case "score":
				less = candidates[i].Score < candidates[j].Score
			default:
				less = candidates[i].Score < candidates[j].Score
			}
			if desc {
				return !less
			}
			return less
		})
	}
	if offset >= len(candidates) {
		return nil, nil
	}
	end := offset + k
	if end > len(candidates) {
		end = len(candidates)
	}
	return candidates[offset:end], nil
}

func (s *UnifiedMemoryService) searchByVectorLocked(embedding []float32, namespace string, topK, ef int) ([]api.SearchResult, error) {
	if len(embedding) != s.dim {
		return nil, fmt.Errorf("memory: embedding dim %d, expected %d", len(embedding), s.dim)
	}
	if topK <= 0 {
		topK = 10
	}
	if ef <= 0 {
		ef = s.ef
	}
	breadth := (topK + 8) * 4
	if breadth < topK*4 {
		breadth = topK * 4
	}
	if breadth > 2000 {
		breadth = 2000
	}
	hits := s.index.Search(embedding, breadth, ef)
	candidates := make([]api.SearchResult, 0, len(hits))
	for _, h := range hits {
		ent, err := s.sql.GetByID(int64(h.ID))
		if err != nil {
			continue
		}
		if namespace != "" && ent.Namespace != namespace {
			continue
		}
		score := 1 - float64(h.Distance)
		candidates = append(candidates, api.SearchResult{Entry: ent, Score: score})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].Entry.Key < candidates[j].Entry.Key
	})
	if len(candidates) > topK {
		candidates = candidates[:topK]
	}
	return candidates, nil
}

// GetByID loads a row by SQLite primary key.
func (s *UnifiedMemoryService) GetByID(id int64) (*api.MemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	return s.sql.GetByID(id)
}

// Update replaces the stored value (and derived embedding) for a row id.
func (s *UnifiedMemoryService) Update(id int64, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("memory: service closed")
	}
	ent, err := s.sql.GetByID(id)
	if err != nil {
		return err
	}
	vec := embeddings.HashEmbed384(value)
	if len(vec) != s.dim {
		return fmt.Errorf("memory: embedding dim %d, expected %d", len(vec), s.dim)
	}
	if err := s.sql.UpdateEntryByID(id, value, vec); err != nil {
		return err
	}
	if err := s.index.Insert(uint64(id), vec); err != nil {
		return err
	}
	s.cache.Delete(s.cacheKey(ent.Key, ent.Namespace))
	return nil
}

// BulkInsert stores each entry in order; stops on first error and returns how many succeeded.
func (s *UnifiedMemoryService) BulkInsert(entries []MemoryEntryInput) (int, error) {
	n := 0
	for _, e := range entries {
		if err := s.Store(e); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// BulkDelete removes keys in a namespace from SQLite, index, and cache.
func (s *UnifiedMemoryService) BulkDelete(keys []string, namespace string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("memory: service closed")
	}
	pairs, err := s.sql.ListIDKeysForKeys(namespace, keys)
	if err != nil {
		return 0, err
	}
	del, err := s.sql.BulkDeleteKeys(namespace, keys)
	if err != nil {
		return int(del), err
	}
	for _, p := range pairs {
		s.index.Delete(uint64(p.ID))
		s.cache.Delete(s.cacheKey(p.Key, namespace))
	}
	return int(del), nil
}

// ClearNamespace deletes every entry in the namespace.
func (s *UnifiedMemoryService) ClearNamespace(namespace string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("memory: service closed")
	}
	pairs, err := s.sql.ListIDKeysInNamespace(namespace)
	if err != nil {
		return 0, err
	}
	del, err := s.sql.DeleteAllInNamespace(namespace)
	if err != nil {
		return int(del), err
	}
	for _, p := range pairs {
		s.index.Delete(uint64(p.ID))
		s.cache.Delete(s.cacheKey(p.Key, namespace))
	}
	return int(del), nil
}

// Count returns row count for a namespace, or total rows if namespace is empty.
func (s *UnifiedMemoryService) Count(namespace string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, errors.New("memory: service closed")
	}
	n, err := s.sql.Count(namespace)
	return int(n), err
}

// HealthCheck pings the SQLite backend.
func (s *UnifiedMemoryService) HealthCheck() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return errors.New("memory: service closed")
	}
	return s.sql.Ping()
}

// FindSimilar searches using the embedding of an existing key (or hashes value if missing).
func (s *UnifiedMemoryService) FindSimilar(key, namespace string, topK int) ([]api.SearchResult, error) {
	ent, err := s.Retrieve(key, namespace)
	if err != nil {
		return nil, err
	}
	vec := ent.Embedding
	if len(vec) == 0 {
		vec = embeddings.HashEmbed384(ent.Value)
	}
	if len(vec) != s.dim {
		return nil, fmt.Errorf("memory: embedding dim %d, expected %d", len(vec), s.dim)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	return s.searchByVectorLocked(vec, namespace, topK, s.ef)
}

// SearchWithEmbedding runs vector search with a caller-supplied query vector.
func (s *UnifiedMemoryService) SearchWithEmbedding(embedding []float32, namespace string, topK int) ([]api.SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	return s.searchByVectorLocked(embedding, namespace, topK, s.ef)
}

func entryHasTags(e *api.MemoryEntry, tags []string) bool {
	set := make(map[string]struct{}, len(e.Tags))
	for _, t := range e.Tags {
		set[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
	}
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if _, ok := set[t]; !ok {
			return false
		}
	}
	return true
}

func entryMetadataMatches(e *api.MemoryEntry, filters map[string]string) bool {
	if len(filters) == 0 {
		return true
	}
	if e.Metadata == nil {
		return false
	}
	for k, v := range filters {
		if e.Metadata[k] != v {
			return false
		}
	}
	return true
}

// Delete removes a record from SQLite and the index.
func (s *UnifiedMemoryService) Delete(key, namespace string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("memory: service closed")
	}
	ent, err := s.sql.GetByKey(key, namespace)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if err := s.sql.DeleteKey(key, namespace); err != nil {
		return err
	}
	s.index.Delete(uint64(ent.ID))
	s.cache.Delete(s.cacheKey(key, namespace))
	return nil
}

// List returns paginated entries for a namespace.
func (s *UnifiedMemoryService) List(namespace string, limit, offset int) ([]api.MemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	return s.sql.List(namespace, limit, offset)
}

// ListNamespaces returns distinct namespace values, optionally filtered by prefix.
func (s *UnifiedMemoryService) ListNamespaces(prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	return s.sql.ListDistinctNamespaces(prefix)
}

// Stats returns aggregate service statistics.
func (s *UnifiedMemoryService) Stats() *MemoryStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int64
	if s.sql != nil && s.sql.db != nil {
		n, _ = s.sql.Count("")
	}
	return &MemoryStats{
		TotalEntries: n,
		IndexSize:    s.index.Size(),
		CacheHits:    s.hits.Load(),
		CacheMisses:  s.misses.Load(),
	}
}
