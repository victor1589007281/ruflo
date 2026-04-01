// Package memory 提供统一记忆服务：三层架构组合 SQLite 持久化、HNSW 向量近似最近邻索引与 LRU 热缓存。
// 写入路径：先落库再更新 HNSW，并回填缓存；读取优先缓存，未命中则查库。
// 语义搜索将查询文本哈希为固定维度向量，经 HNSW 检索候选 ID 后回表过滤命名空间/标签/元数据，并可按分数或时间排序。
// 精确查找通过 (namespace, key) 或主键 ID，与向量检索互补。
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
	// 各方法语义与 UnifiedMemoryService 同名实现一致，此处不重复赘述。
}

// UnifiedMemoryService 组合 SQLite 持久层、内存 HNSW 索引与 LRU 缓存，对外提供统一语义/精确检索。
type UnifiedMemoryService struct {
	mu          sync.RWMutex   // 保护关闭标志与写路径；读多写少场景配合 RLock
	sql         *SQLiteBackend // 关系型持久化与按 ID/键查询
	index       *HNSWIndex     // 近似最近邻向量索引，与 SQLite 行 id 对齐
	cache       *LRUCache      // (namespace+key) 精确读缓存
	dim         int            // 向量维度，须与 embedding 一致
	ef          int            // HNSW 搜索时的动态候选规模参数（越大越准越慢）
	hits        atomic.Int64   // 缓存命中计数（统计用）
	misses      atomic.Int64   // 缓存未命中计数
	closed      bool           // Close 后置 true，拒绝新操作
	initOnce    sync.Once      // Initialize 仅执行一次
	initErr     error          // 首次初始化错误
	initialized atomic.Bool    // 初始化成功标记
}

// NewUnifiedMemoryService 打开 SQLite 并构造空 HNSW 与 LRU；使用前须调用 Initialize 将已有向量灌入索引。
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

// Initialize 从库中扫描全部非空 embedding 行，按行 id 插入 HNSW，使重启后索引与磁盘一致。
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

// IsInitialized 返回 Initialize 是否已成功完成（并发安全读原子标记）。
func (s *UnifiedMemoryService) IsInitialized() bool {
	if s == nil {
		return false
	}
	return s.initialized.Load()
}

// Close 关闭 SQLite；置 closed 防止后续读写。
func (s *UnifiedMemoryService) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.sql.Close()
}

// cacheKey 用 NUL 拼接命名空间与业务键，避免不同 namespace 下键名碰撞。
func (s *UnifiedMemoryService) cacheKey(key, ns string) string {
	return ns + "\x00" + key
}

// Store Upsert 数据库行，向 HNSW 插入/更新同 id 向量，并写穿缓存。
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

// Retrieve 先查 LRU，未命中再读 SQLite 并回填缓存（命中/未命中分别累加原子计数）。
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

// Search 将 query 哈希为查询向量，HNSW 取宽候选集后按命名空间/标签/元数据过滤，分数为 1-距离，支持排序与 offset/limit 切片。
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

// searchByVectorLocked 在已持读锁下执行向量检索：扩大 breadth 再截断 topK，按分数与 key 稳定排序。
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

// GetByID 按 SQLite 主键读取整行（不经 LRU）。
func (s *UnifiedMemoryService) GetByID(id int64) (*api.MemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	return s.sql.GetByID(id)
}

// Update 按 id 重写 value 与由 value 派生的 embedding，更新 HNSW 同 id 点，并失效对应缓存键。
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

// BulkInsert 顺序调用 Store，遇错即停并返回已成功条数。
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

// BulkDelete 先查 id-key 对再删库行，同步从 HNSW 与缓存移除，保证三层一致。
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

// ClearNamespace 清空某命名空间全部记录并逐条清理索引与缓存。
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

// Count namespace 为空时统计全表行数，否则统计该命名空间行数。
func (s *UnifiedMemoryService) Count(namespace string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, errors.New("memory: service closed")
	}
	n, err := s.sql.Count(namespace)
	return int(n), err
}

// HealthCheck 对 SQLite 连接 Ping。
func (s *UnifiedMemoryService) HealthCheck() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return errors.New("memory: service closed")
	}
	return s.sql.Ping()
}

// FindSimilar 先 Retrieve 目标条目，用其 embedding（空则对 value 哈希）在命名空间内做 K 近邻。
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

// SearchWithEmbedding 使用调用方提供的查询向量直接检索，适用于已在外部完成编码的场景。
func (s *UnifiedMemoryService) SearchWithEmbedding(embedding []float32, namespace string, topK int) ([]api.SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	return s.searchByVectorLocked(embedding, namespace, topK, s.ef)
}

// entryHasTags 要求条目 tags 包含所有指定标签（大小写不敏感、忽略空白）。
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

// entryMetadataMatches 要求 Metadata 中每个 filter 键值与条目完全一致。
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

// Delete 按 (key,namespace) 删行；若无行则忽略；同步删索引点与缓存。
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

// List 按 updated_at 倒序分页列出命名空间条目（委托 SQLite）。
func (s *UnifiedMemoryService) List(namespace string, limit, offset int) ([]api.MemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	return s.sql.List(namespace, limit, offset)
}

// ListNamespaces 返回去重排序的命名空间列表；prefix 非空时用 LIKE 'prefix%' 过滤。
func (s *UnifiedMemoryService) ListNamespaces(prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("memory: service closed")
	}
	return s.sql.ListDistinctNamespaces(prefix)
}

// Stats 汇总总条数、索引规模与缓存命中/未命中。
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
