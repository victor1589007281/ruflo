package statestore

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func newSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLiteKVRoundtrip(t *testing.T) {
	s := newSQLite(t)
	kv := s.KV("teams")
	if err := kv.Put("t1", map[string]any{"name": "alpha", "n": 3}); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	ok, err := kv.Get("t1", &out)
	if err != nil || !ok || out["name"] != "alpha" {
		t.Fatalf("roundtrip 失败: ok=%v err=%v out=%v", ok, err, out)
	}
	// 不存在
	if ok, _ := kv.Get("missing", &out); ok {
		t.Fatal("不存在的 key 应返回 false")
	}
	// 覆盖
	_ = kv.Put("t1", map[string]any{"name": "beta"})
	_, _ = kv.Get("t1", &out)
	if out["name"] != "beta" {
		t.Fatal("覆盖写失败")
	}
	// Delete 幂等
	if err := kv.Delete("t1"); err != nil {
		t.Fatal(err)
	}
	if err := kv.Delete("t1"); err != nil {
		t.Fatal("重复删除应幂等")
	}
}

func TestSQLiteKeysSorted(t *testing.T) {
	s := newSQLite(t)
	kv := s.KV("b")
	for _, k := range []string{"c", "a", "b"} {
		_ = kv.Put(k, 1)
	}
	keys, _ := kv.Keys()
	if len(keys) != 3 || keys[0] != "a" || keys[2] != "c" {
		t.Fatalf("Keys 应字典序: %v", keys)
	}
}

func TestSQLiteBadBucket(t *testing.T) {
	s := newSQLite(t)
	if err := s.KV("bad/bucket").Put("k", 1); err == nil {
		t.Fatal("非法 bucket 名应报错")
	}
}

func TestSQLiteLogAppendOrder(t *testing.T) {
	s := newSQLite(t)
	lg := s.Log("journal")
	for i := 0; i < 5; i++ {
		if err := lg.Append(map[string]int{"seq": i}); err != nil {
			t.Fatal(err)
		}
	}
	var got []int
	err := lg.ReadAll(func(line []byte) error {
		var m map[string]int
		if err := json.Unmarshal(line, &m); err == nil {
			got = append(got, m["seq"])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[0] != 0 || got[4] != 4 {
		t.Fatalf("Log 应按追加顺序: %v", got)
	}
}

func TestSQLiteBlobDedup(t *testing.T) {
	s := newSQLite(t)
	blob := s.Blob()
	h1, _ := blob.Put([]byte("prompt content"))
	h2, _ := blob.Put([]byte("prompt content"))
	if h1 != h2 {
		t.Fatalf("相同内容应同 hash: %q vs %q", h1, h2)
	}
	data, err := blob.Get(h1)
	if err != nil || string(data) != "prompt content" {
		t.Fatalf("Get 失败: %v %q", err, data)
	}
	if !blob.Has(h1) {
		t.Fatal("Has 应为 true")
	}
	if _, err := blob.Get("nonexistent"); err == nil {
		t.Fatal("不存在 blob 应报错")
	}
}

func TestSQLitePersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s1, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.KV("k").Put("x", "persisted")
	_ = s1.Log("l").Append("line1")
	h, _ := s1.Blob().Put([]byte("blobdata"))
	_ = s1.Close()

	// 重开
	s2, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v string
	if ok, _ := s2.KV("k").Get("x", &v); !ok || v != "persisted" {
		t.Fatal("重开后 KV 应仍在")
	}
	n := 0
	_ = s2.Log("l").ReadAll(func([]byte) error { n++; return nil })
	if n != 1 {
		t.Fatalf("重开后 Log 应有 1 条, got %d", n)
	}
	if !s2.Blob().Has(h) {
		t.Fatal("重开后 Blob 应仍在")
	}
}
