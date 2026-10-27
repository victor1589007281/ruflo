package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestSharedFetchAcrossManagersCachesFullDocument(t *testing.T) {
	allowAnyFetchURL(t)
	server := miniredis.RunT(t)
	const body = "0123456789abcdefghijklmnopqrstuvwxyz"
	var calls atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, body)
	}))
	defer httpServer.Close()

	cfg := newSharedTestConfig(server.Addr())
	manager1 := NewManager(cfg, testEgress(t), nil)
	manager2 := NewManager(cfg, testEgress(t), nil)
	t.Cleanup(func() { _ = manager1.Close(); _ = manager2.Close() })
	first, err := manager1.Fetch(context.Background(), FetchRequest{URL: httpServer.URL + "/private?token=secret", MaxChars: 10})
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	originalHash := first.Document.Hash
	if first.Document.Markdown != body[:10] || first.Document.TotalChars != len(body) {
		t.Fatalf("first document was not selected after full caching: %+v", first.Document)
	}
	first.Document.Markdown = "caller mutation"
	first.Document.Title = "caller mutation"

	second, err := manager2.Fetch(context.Background(), FetchRequest{
		URL: httpServer.URL + "/private?token=secret", MaxChars: 7, Offset: 5,
	})
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if calls.Load() != 1 || !second.Document.CacheHit {
		t.Fatalf("HTTP calls=%d cacheHit=%v", calls.Load(), second.Document.CacheHit)
	}
	if second.Document.Markdown != body[5:12] || second.Document.TotalChars != len(body) {
		t.Fatalf("shared cache did not preserve full text: %+v", second.Document)
	}
	if second.Document.Hash != originalHash || second.Document.Title == "caller mutation" {
		t.Fatalf("shared document was mutated: %+v", second.Document)
	}
	for _, key := range server.Keys() {
		if strings.Contains(key, "token=secret") || strings.Contains(key, "/private") {
			t.Fatalf("plaintext URL in Redis key: %q", key)
		}
	}
}

func TestSharedFetchStaleOnlyForTemporaryFailure(t *testing.T) {
	allowAnyFetchURL(t)
	server := miniredis.RunT(t)
	var status atomic.Int32
	status.Store(http.StatusOK)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		code := int(status.Load())
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = fmt.Fprint(w, "cached full body")
		}
	}))
	defer httpServer.Close()

	cfg := newSharedTestConfig(server.Addr())
	cfg.CacheTTL = 30 * time.Millisecond
	cfg.RedisStaleTTL = time.Hour
	manager1 := NewManager(cfg, testEgress(t), nil)
	manager2 := NewManager(cfg, testEgress(t), nil)
	t.Cleanup(func() { _ = manager1.Close(); _ = manager2.Close() })
	if _, err := manager1.Fetch(context.Background(), FetchRequest{URL: httpServer.URL}); err != nil {
		t.Fatalf("seed Fetch: %v", err)
	}
	status.Store(http.StatusInternalServerError)
	time.Sleep(50 * time.Millisecond)

	stale, err := manager2.Fetch(context.Background(), FetchRequest{URL: httpServer.URL})
	if err != nil {
		t.Fatalf("temporary failure should use stale: %v", err)
	}
	if !stale.Document.Stale || !stale.Document.CacheHit || stale.Document.Markdown != "cached full body" {
		t.Fatalf("unexpected stale response: %+v", stale)
	}
	if !contains(stale.Degraded, "stale_if_error:unavailable") {
		t.Fatalf("missing stale cause: %v", stale.Degraded)
	}

	status.Store(http.StatusForbidden)
	permanent, err := manager2.Fetch(context.Background(), FetchRequest{URL: httpServer.URL})
	if KindOf(err) != ErrBlocked {
		t.Fatalf("permanent failure borrowed stale: resp=%+v err=%v", permanent, err)
	}
	if permanent != nil && permanent.Document != nil {
		t.Fatalf("permanent failure returned stale document: %+v", permanent.Document)
	}
}
