package wiki

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/sync"
)

func TestSyncAPIEndpoints(t *testing.T) {
	if os.Getenv("IMA_CLIENT_ID") == "" {
		t.Skip("set IMA_CLIENT_ID and IMA_API_KEY to run live sync API test")
	}
	repo := t.TempDir()
	cfg := sync.Config{
		KnowledgeRepo: repo,
		IMA: sync.IMAConfig{
			ClientID: os.Getenv("IMA_CLIENT_ID"),
			APIKey:   os.Getenv("IMA_API_KEY"),
		},
	}
	eng := NewEngine(repo, "", "", "")
	api := NewAPIServer(eng, "")
	sched := sync.NewScheduler(cfg)
	api.SetScheduler(sched)

	req := httptest.NewRequest(http.MethodPost, "/sync/ima", nil)
	rr := httptest.NewRecorder()
	api.Mux().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}

	time.Sleep(15 * time.Second)

	idx, err := sync.LoadIndex(repo, "ima")
	if err != nil {
		t.Fatalf("LoadIndex failed: %v", err)
	}
	if len(idx.Items) == 0 {
		t.Fatal("expected at least one IMA item after sync")
	}
	for _, item := range idx.Items {
		if _, err := os.Stat(filepath.Join(repo, item.Path)); err != nil {
			t.Fatalf("raw file missing: %s", item.Path)
		}
	}
}
