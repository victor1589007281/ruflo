package memory

import (
	"testing"
	"time"
)

func TestCacheSetGet(t *testing.T) {
	t.Parallel()
	c := NewLRUCache(10, 0)
	c.Set("k", "v")
	v, ok := c.Get("k")
	if !ok || v != "v" {
		t.Fatalf("Get: ok=%v v=%v", ok, v)
	}
}

func TestCacheEviction(t *testing.T) {
	t.Parallel()
	c := NewLRUCache(2, 0)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)
	if _, ok := c.Get("a"); ok {
		t.Fatal("oldest key a should be evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should remain")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should remain")
	}
}

func TestCacheTTL(t *testing.T) {
	t.Parallel()
	c := NewLRUCache(10, 30*time.Millisecond)
	c.Set("x", 42)
	if _, ok := c.Get("x"); !ok {
		t.Fatal("expected key before expiry")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.Get("x"); ok {
		t.Fatal("expected key expired after sleep")
	}
}

func TestCacheDelete(t *testing.T) {
	t.Parallel()
	c := NewLRUCache(10, 0)
	c.Set("d", "gone")
	c.Delete("d")
	if _, ok := c.Get("d"); ok {
		t.Fatal("expected key deleted")
	}
}
