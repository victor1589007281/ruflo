package memory

import (
	"path/filepath"
	"testing"

	"github.com/ruflo/ruflo-go/pkg/embeddings"
)

func newTestMemoryService(t *testing.T) (*UnifiedMemoryService, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.db")
	svc, err := NewUnifiedMemoryService(path, embeddings.HashEmbeddingDim, 64)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_ = svc.Close()
	}
	return svc, cleanup
}

func TestGetByID(t *testing.T) {
	t.Parallel()
	svc, cleanup := newTestMemoryService(t)
	defer cleanup()
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	in := MemoryEntryInput{Key: "k1", Value: "hello world", Namespace: "ns"}
	if err := svc.Store(in); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Retrieve("k1", "ns")
	if err != nil || got == nil {
		t.Fatalf("retrieve: %v %#v", err, got)
	}
	byID, err := svc.GetByID(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if byID == nil || byID.Key != "k1" || byID.Value != "hello world" {
		t.Fatalf("GetByID: %#v", byID)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	svc, cleanup := newTestMemoryService(t)
	defer cleanup()
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store(MemoryEntryInput{Key: "u1", Value: "before", Namespace: "n"}); err != nil {
		t.Fatal(err)
	}
	ent, _ := svc.Retrieve("u1", "n")
	if err := svc.Update(ent.ID, "after"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetByID(ent.ID)
	if err != nil || got.Value != "after" {
		t.Fatalf("update: %v %#v", err, got)
	}
}

func TestBulkInsert(t *testing.T) {
	t.Parallel()
	svc, cleanup := newTestMemoryService(t)
	defer cleanup()
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	entries := []MemoryEntryInput{
		{Key: "a", Value: "va", Namespace: "bulk"},
		{Key: "b", Value: "vb", Namespace: "bulk"},
		{Key: "c", Value: "vc", Namespace: "bulk"},
	}
	n, err := svc.BulkInsert(entries)
	if err != nil || n != 3 {
		t.Fatalf("BulkInsert: n=%d err=%v", n, err)
	}
	cnt, err := svc.Count("bulk")
	if err != nil || cnt != 3 {
		t.Fatalf("Count: %d err=%v", cnt, err)
	}
}

func TestBulkDelete(t *testing.T) {
	t.Parallel()
	svc, cleanup := newTestMemoryService(t)
	defer cleanup()
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	_ = svc.Store(MemoryEntryInput{Key: "d1", Value: "x", Namespace: "bd"})
	_ = svc.Store(MemoryEntryInput{Key: "d2", Value: "y", Namespace: "bd"})
	del, err := svc.BulkDelete([]string{"d1"}, "bd")
	if err != nil || del != 1 {
		t.Fatalf("BulkDelete: %d err=%v", del, err)
	}
	if _, err := svc.Retrieve("d1", "bd"); err == nil {
		t.Fatal("expected d1 gone")
	}
	if _, err := svc.Retrieve("d2", "bd"); err != nil {
		t.Fatalf("d2 should remain: %v", err)
	}
}

func TestClearNamespace(t *testing.T) {
	t.Parallel()
	svc, cleanup := newTestMemoryService(t)
	defer cleanup()
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = svc.Store(MemoryEntryInput{Key: string(rune('a' + i)), Value: "v", Namespace: "clear-me"})
	}
	n, err := svc.ClearNamespace("clear-me")
	if err != nil || n != 3 {
		t.Fatalf("ClearNamespace: %d err=%v", n, err)
	}
	c, _ := svc.Count("clear-me")
	if c != 0 {
		t.Fatalf("namespace not empty: %d", c)
	}
}

func TestCount(t *testing.T) {
	t.Parallel()
	svc, cleanup := newTestMemoryService(t)
	defer cleanup()
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	_ = svc.Store(MemoryEntryInput{Key: "c1", Value: "1", Namespace: "cnt"})
	_ = svc.Store(MemoryEntryInput{Key: "c2", Value: "2", Namespace: "cnt"})
	total, err := svc.Count("")
	if err != nil || total < 2 {
		t.Fatalf("total count: %d err=%v", total, err)
	}
	ns, err := svc.Count("cnt")
	if err != nil || ns != 2 {
		t.Fatalf("namespace count: %d err=%v", ns, err)
	}
}

func TestHealthCheck(t *testing.T) {
	t.Parallel()
	svc, cleanup := newTestMemoryService(t)
	defer cleanup()
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	if err := svc.HealthCheck(); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
}

func TestFindSimilar(t *testing.T) {
	t.Parallel()
	svc, cleanup := newTestMemoryService(t)
	defer cleanup()
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	ns := "sim"
	_ = svc.Store(MemoryEntryInput{Key: "alpha", Value: "the quick brown fox", Namespace: ns})
	_ = svc.Store(MemoryEntryInput{Key: "beta", Value: "unrelated quantum physics", Namespace: ns})
	hits, err := svc.FindSimilar("alpha", ns, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 1 {
		t.Fatal("expected similar hits")
	}
	if hits[0].Entry.Key != "alpha" {
		t.Fatalf("first hit should be self-similar: %#v", hits[0].Entry.Key)
	}
}

func TestSearchWithEmbedding(t *testing.T) {
	t.Parallel()
	svc, cleanup := newTestMemoryService(t)
	defer cleanup()
	if err := svc.Initialize(); err != nil {
		t.Fatal(err)
	}
	ns := "emb"
	_ = svc.Store(MemoryEntryInput{Key: "q1", Value: "embedding query text", Namespace: ns})
	vec := embeddings.HashEmbed384("embedding query text")
	res, err := svc.SearchWithEmbedding(vec, ns, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) < 1 || res[0].Entry.Key != "q1" {
		t.Fatalf("SearchWithEmbedding: %#v", res)
	}
}

func TestIsInitialized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "init.db")
	svc, err := NewUnifiedMemoryService(path, embeddings.HashEmbeddingDim, 64)
	if err != nil {
		t.Fatal(err)
	}
	if svc.IsInitialized() {
		_ = svc.Close()
		t.Fatal("expected false before Initialize")
	}
	if err := svc.Initialize(); err != nil {
		_ = svc.Close()
		t.Fatal(err)
	}
	if !svc.IsInitialized() {
		_ = svc.Close()
		t.Fatal("expected true after Initialize")
	}
	_ = svc.Close()

	svc2, err := NewUnifiedMemoryService(path, embeddings.HashEmbeddingDim, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc2.Close() }()
	if err := svc2.Initialize(); err != nil {
		t.Fatal(err)
	}
	if !svc2.IsInitialized() {
		t.Fatal("expected true on fresh instance after Initialize")
	}
}
