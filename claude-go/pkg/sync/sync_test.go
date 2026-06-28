package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeAdapter struct {
	items []ExternalItem
}

func (f *fakeAdapter) Source() string { return "fake" }

func (f *fakeAdapter) List() ([]ExternalItem, error) {
	return f.items, nil
}

func (f *fakeAdapter) Fetch(item ExternalItem) (ExternalItem, error) {
	item.Body = "body for " + item.ExternalID
	return item, nil
}

func TestRunSync(t *testing.T) {
	repo := t.TempDir()
	cfg := Config{KnowledgeRepo: repo}
	adapter := &fakeAdapter{
		items: []ExternalItem{
			{ExternalID: "a", Type: "note", Title: "First Note", URL: "https://example.com/a", UpdatedAt: time.Now().UTC()},
		},
	}

	res, err := RunSync(cfg, adapter.Source(), adapter)
	if err != nil {
		t.Fatalf("RunSync failed: %v", err)
	}
	if res.Created != 1 {
		t.Fatalf("expected 1 created, got %+v", res)
	}

	idx, err := LoadIndex(repo, "fake")
	if err != nil {
		t.Fatalf("LoadIndex failed: %v", err)
	}
	if len(idx.Items) != 1 {
		t.Fatalf("expected 1 index item, got %d", len(idx.Items))
	}
	item := idx.Items["a"]
	if item.Path == "" {
		t.Fatal("index item path empty")
	}
	abs := filepath.Join(repo, item.Path)
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read raw file failed: %v", err)
	}
	if string(data) == "" {
		t.Fatal("raw file empty")
	}

	// 再次同步应无变化
	res, err = RunSync(cfg, adapter.Source(), adapter)
	if err != nil {
		t.Fatalf("second RunSync failed: %v", err)
	}
	if res.Unchanged != 1 {
		t.Fatalf("expected 1 unchanged, got %+v", res)
	}

	// 删除外部条目
	adapter.items = nil
	res, err = RunSync(cfg, adapter.Source(), adapter)
	if err != nil {
		t.Fatalf("third RunSync failed: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deleted, got %+v", res)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatal("raw file should have been moved")
	}
}

func TestParseCron(t *testing.T) {
	s, err := parseCron("0 * * * *")
	if err != nil {
		t.Fatalf("parseCron failed: %v", err)
	}
	if !s.minute[0] || s.minute[1] {
		t.Fatal("minute field wrong")
	}
}

func TestScheduler(t *testing.T) {
	cfg := Config{KnowledgeRepo: t.TempDir()}
	sched := NewScheduler(cfg)
	called := make(chan struct{}, 1)
	if err := sched.Register("test", "* * * * *", func(ctx context.Context) error {
		called <- struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	sched.Start()
	defer sched.Stop()

	// 手动触发
	job := sched.RunNow("test", func(ctx context.Context) error {
		called <- struct{}{}
		return nil
	})
	if job.ID == "" {
		t.Fatal("job id empty")
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("job not executed")
	}
	if _, ok := sched.JobStatusByID(job.ID); !ok {
		t.Fatal("job status not found")
	}
}
