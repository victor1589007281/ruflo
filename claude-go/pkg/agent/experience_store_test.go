package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgentDB 实现 /v1/store/{bucket}/{key} 与 /keys 的最小语义。
type fakeAgentDB struct {
	mu    sync.Mutex
	store map[string][]byte
	puts  int
	gets  int
}

func newFakeAgentDB() *fakeAgentDB {
	return &fakeAgentDB{store: map[string][]byte{}}
}

func (f *fakeAgentDB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/v1/store/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.SplitN(rest, "/", 2)
	bucket, _ := url.PathUnescape(parts[0])
	if bucket == "" {
		http.Error(w, "bucket required", http.StatusBadRequest)
		return
	}
	key := ""
	if len(parts) == 2 {
		key, _ = url.PathUnescape(parts[1])
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case key == "" && r.Method == http.MethodGet:
		// keys 列表不带 bucket 前缀：让 storeURL("")+"/keys" 也落到这里。
		http.Error(w, "key required", http.StatusBadRequest)
	case key == "keys" && r.Method == http.MethodGet:
		keys := make([]string, 0, len(f.store))
		for k := range f.store {
			keys = append(keys, k)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	case r.Method == http.MethodGet:
		f.gets++
		raw, ok := f.store[key]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(raw)
	case r.Method == http.MethodPut:
		raw, err := readAll(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.puts++
		f.store[key] = raw
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		delete(f.store, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func testExperience(id, content string, at time.Time) *Experience {
	return &Experience{
		ID:        id,
		Category:  "general",
		Content:   content,
		Quality:   0.7,
		CreatedAt: at.Add(-time.Hour),
		UpdatedAt: at,
		Lifecycle: "validated",
		Symptom:   "现象",
		RootCause: "根因",
		Action:    "动作",
	}
}

func TestAgentDBExperienceStoreSyncLoadAndTombstone(t *testing.T) {
	fake := newFakeAgentDB()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	store := NewAgentDBExperienceStore(srv.URL, "test-exp")
	ctx := context.Background()

	t1 := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	e1 := testExperience("exp-1", "v1", t1)
	e2 := testExperience("exp-2", "other", t1)

	res, err := store.Sync(ctx, []*Experience{e1, e2}, nil)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Written != 2 || res.Skipped != 0 || res.Deleted != 0 {
		t.Fatalf("first sync = %+v, want 2 written", res)
	}

	snap, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if snap.LiveCount() != 2 || snap.TombstoneCount() != 0 {
		t.Fatalf("snapshot = %d live/%d tomb, want 2/0", snap.LiveCount(), snap.TombstoneCount())
	}
	if got := snap.Records["exp-1"].Content; got != "v1" {
		t.Fatalf("exp-1 content = %q, want v1", got)
	}

	// 旧版本不覆盖新版本。
	older := testExperience("exp-1", "stale", t1.Add(-time.Minute))
	res, err = store.Sync(ctx, []*Experience{older}, nil)
	if err != nil {
		t.Fatalf("Sync older: %v", err)
	}
	if res.Skipped != 1 {
		t.Fatalf("older sync = %+v, want 1 skipped", res)
	}

	// 新版本覆盖。
	newer := testExperience("exp-1", "v2", t2)
	res, err = store.Sync(ctx, []*Experience{newer}, nil)
	if err != nil {
		t.Fatalf("Sync newer: %v", err)
	}
	if res.Written != 1 {
		t.Fatalf("newer sync = %+v, want 1 written", res)
	}

	// 墓碑。
	res, err = store.Sync(ctx, nil, []ExperienceTombstone{{ID: "exp-2", UpdatedAt: t2}})
	if err != nil {
		t.Fatalf("Sync tombstone: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("tombstone sync = %+v, want 1 deleted", res)
	}
	snap, err = store.Load(ctx)
	if err != nil {
		t.Fatalf("Load after tombstone: %v", err)
	}
	if snap.LiveCount() != 1 || snap.TombstoneCount() != 1 {
		t.Fatalf("after tombstone = %d live/%d tomb, want 1/1", snap.LiveCount(), snap.TombstoneCount())
	}
	if _, ok := snap.Records["exp-2"]; ok {
		t.Fatalf("exp-2 should be tombstoned")
	}
}

func TestAgentDBExperienceStoreCompact(t *testing.T) {
	fake := newFakeAgentDB()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	store := NewAgentDBExperienceStore(srv.URL, "test-compact")
	ctx := context.Background()

	old := time.Now().Add(-100 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	if _, err := store.Sync(ctx, nil, []ExperienceTombstone{
		{ID: "old", UpdatedAt: old},
		{ID: "recent", UpdatedAt: recent},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := store.Sync(ctx, []*Experience{testExperience("live", "ok", recent)}, nil); err != nil {
		t.Fatalf("Sync live: %v", err)
	}

	n, err := store.Compact(ctx, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if n != 1 {
		t.Fatalf("Compact removed %d, want 1", n)
	}
	snap, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := snap.Tombstones["old"]; ok {
		t.Fatalf("old tombstone should be physically removed")
	}
	if _, ok := snap.Tombstones["recent"]; !ok {
		t.Fatalf("recent tombstone should be kept")
	}
	if _, ok := snap.Records["live"]; !ok {
		t.Fatalf("live record should be kept")
	}
}

func TestMergeExperienceState(t *testing.T) {
	t1 := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	t3 := t2.Add(time.Hour)

	localNewer := testExperience("local-newer", "local", t2)
	remoteOlder := testExperience("local-newer", "remote", t1)
	remoteNewer := testExperience("remote-newer", "remote", t2)
	localOlder := testExperience("remote-newer", "local", t1)
	localOnly := testExperience("local-only", "only", t1)
	tombstoned := testExperience("deleted", "gone", t1)

	merged, push, synced := mergeExperienceState(
		[]*Experience{localNewer, localOlder, localOnly, tombstoned},
		&ExperienceSnapshot{
			Records:    map[string]*Experience{"local-newer": remoteOlder, "remote-newer": remoteNewer},
			Tombstones: map[string]time.Time{"deleted": t3},
		},
	)
	byID := map[string]*Experience{}
	for _, e := range merged {
		byID[e.ID] = e
	}
	if got := byID["local-newer"].Content; got != "local" {
		t.Fatalf("local-newer content = %q, want local", got)
	}
	if got := byID["remote-newer"].Content; got != "remote" {
		t.Fatalf("remote-newer content = %q, want remote", got)
	}
	if _, ok := byID["deleted"]; ok {
		t.Fatalf("deleted entry should be gone")
	}
	if len(push) != 2 {
		t.Fatalf("push %d entries, want 2 (local-newer + local-only)", len(push))
	}
	for _, e := range push {
		if e.ID == "remote-newer" || e.ID == "deleted" {
			t.Fatalf("should not push %s", e.ID)
		}
	}
	if len(synced) != 1 || synced["remote-newer"] == "" {
		t.Fatalf("synced = %+v, want only remote-newer", synced)
	}

	// 墓碑比本地旧：本地经验应复活并回推。
	resurrected := testExperience("deleted", "newer", t3.Add(time.Hour))
	merged, push, _ = mergeExperienceState(
		[]*Experience{resurrected},
		&ExperienceSnapshot{Tombstones: map[string]time.Time{"deleted": t3}},
	)
	if len(merged) != 1 || merged[0].Content != "newer" {
		t.Fatalf("resurrect merged = %+v", merged)
	}
	if len(push) != 1 {
		t.Fatalf("resurrect push = %d, want 1", len(push))
	}
}

func TestEvolutionEngineSharedStore(t *testing.T) {
	fake := newFakeAgentDB()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	t.Setenv(experienceStoreModeEnv, "agentdb")
	t.Setenv("CLAUDE_GO_AGENTDB_URL", srv.URL)
	t.Setenv(experienceBucketEnv, "test-engine")

	t1 := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	// 远端已有一条，且比本地同 ID 新。
	remote := testExperience("shared", "remote-newer", t2)
	store := NewAgentDBExperienceStore(srv.URL, "test-engine")
	if _, err := store.Sync(context.Background(), []*Experience{remote}, nil); err != nil {
		t.Fatalf("seed remote: %v", err)
	}

	dir := t.TempDir()
	local := []*Experience{
		testExperience("shared", "local-stale", t1),
		testExperience("local-only", "local", t1),
	}
	raw, _ := json.MarshalIndent(local, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "experiences.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}

	ee := NewEvolutionEngine(dir, nil)
	if len(ee.experiences) != 2 {
		t.Fatalf("engine loaded %d experiences, want 2", len(ee.experiences))
	}
	byID := map[string]*Experience{}
	for _, e := range ee.experiences {
		byID[e.ID] = e
	}
	if got := byID["shared"].Content; got != "remote-newer" {
		t.Fatalf("shared content = %q, want remote-newer", got)
	}
	if _, ok := byID["local-only"]; !ok {
		t.Fatalf("local-only should survive merge")
	}

	// 本地独有条目应已回推 agentDB。
	snap, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load after engine init: %v", err)
	}
	if _, ok := snap.Records["local-only"]; !ok {
		t.Fatalf("local-only was not pushed to agentDB")
	}

	// migrateV2Fields 会补齐 Lifecycle/Pattern/MinHash，先刷一次让差异基线稳定。
	ee.persistExperiences()

	// 只改一条：只有它产生写入。
	putsBefore := fake.puts
	ee.mu.Lock()
	byID["local-only"].Content = "changed"
	byID["local-only"].UpdatedAt = t2.Add(time.Hour)
	ee.mu.Unlock()
	ee.persistExperiences()
	if fake.puts != putsBefore+1 {
		t.Fatalf("put count = %d, want %d (only the changed entry)", fake.puts, putsBefore+1)
	}

	// 删一条：写墓碑而不是物理删。
	putsBefore = fake.puts
	ee.mu.Lock()
	kept := ee.experiences[:0]
	for _, e := range ee.experiences {
		if e.ID != "local-only" {
			kept = append(kept, e)
		}
	}
	ee.experiences = kept
	ee.mu.Unlock()
	ee.persistExperiences()
	if fake.puts != putsBefore+1 {
		t.Fatalf("put count = %d, want %d (one tombstone)", fake.puts, putsBefore+1)
	}
	snap, err = store.Load(context.Background())
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if _, ok := snap.Records["local-only"]; ok {
		t.Fatalf("local-only should be tombstoned")
	}
	if _, ok := snap.Tombstones["local-only"]; !ok {
		t.Fatalf("local-only tombstone missing")
	}

	// 本地缓存已原子写回合并结果。
	data, err := os.ReadFile(filepath.Join(dir, "experiences.json"))
	if err != nil {
		t.Fatalf("read local cache: %v", err)
	}
	var cached []*Experience
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatalf("local cache invalid json: %v", err)
	}
	if len(cached) != 1 || cached[0].ID != "shared" {
		t.Fatalf("local cache = %s, want only shared", string(data))
	}
	if _, err := os.Stat(filepath.Join(dir, "experiences.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file should not remain: %v", err)
	}
}

func TestExperienceFingerprintIgnoresUpdatedAt(t *testing.T) {
	t1 := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)
	a := testExperience("x", "same", t1)
	b := *a
	b.UpdatedAt = t1.Add(48 * time.Hour)
	if ExperienceFingerprint(a) != ExperienceFingerprint(&b) {
		t.Fatalf("fingerprint should ignore UpdatedAt")
	}
	b.Content = "different"
	if ExperienceFingerprint(a) == ExperienceFingerprint(&b) {
		t.Fatalf("fingerprint should change with content")
	}
}

func ExampleAgentDBExperienceStore() {
	fmt.Println(defaultExperienceBucket)
	// Output: claude-go-experience
}
