package memory

// MemoryEntryInput is the payload for storing a memory record.
type MemoryEntryInput struct {
	Key        string
	Value      string
	Namespace  string
	Tags       []string
	Embedding  []float32
	TTLSeconds *int64
	Metadata   map[string]string
}

// MemoryStats summarizes backend utilization.
type MemoryStats struct {
	TotalEntries int64
	IndexSize    int
	CacheHits    int64
	CacheMisses  int64
}

// SearchResult is one vector search hit from the HNSW index (ID is SQLite row id).
type SearchResult struct {
	ID       uint64
	Distance float32
}
