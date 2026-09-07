package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// TestClusterHTTPRoundTrip 端到端: 控制面挂端点, worker Client 心跳→拉取→回报, 观测端点可见。
func TestClusterHTTPRoundTrip(t *testing.T) {
	ss := statestore.NewMemStore()
	q := NewQueue(ss, time.Minute)
	reg := NewRegistry(ss, time.Minute)
	mux := http.NewServeMux()
	Mount(mux, q, reg)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 控制面入队一个任务
	if _, err := q.Enqueue(Task{Kind: "stage", RunID: "run-1", NodeID: "draft",
		Payload: json.RawMessage(`{"role":"writer"}`)}); err != nil {
		t.Fatal(err)
	}

	client := NewClient(srv.URL, "w-test")

	// 心跳注册
	if err := client.Heartbeat([]string{"bash"}, []string{"stage"}, nil); err != nil {
		t.Fatal(err)
	}
	alive, _ := reg.Alive()
	if len(alive) != 1 || alive[0].Name != "w-test" {
		t.Fatalf("worker 未注册可见: %+v", alive)
	}

	// 拉取
	task, err := client.Pull([]string{"stage"})
	if err != nil || task == nil {
		t.Fatalf("拉取失败: task=%v err=%v", task, err)
	}
	if task.RunID != "run-1" || task.NodeID != "draft" {
		t.Fatalf("任务字段透传错误: %+v", task)
	}

	// 回报成功
	if err := client.Complete(task.ID, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	done, _ := q.Get(task.ID)
	if done.Status != "completed" || string(done.Result) != `{"ok":true}` {
		t.Fatalf("完成状态错误: %+v", done)
	}

	// 无更多任务: 拉取返回 nil
	if task2, _ := client.Pull([]string{"stage"}); task2 != nil {
		t.Fatalf("不应再有任务: %+v", task2)
	}
}

func TestClusterHTTPFailRequeue(t *testing.T) {
	ss := statestore.NewMemStore()
	q := NewQueue(ss, time.Minute)
	reg := NewRegistry(ss, time.Minute)
	mux := http.NewServeMux()
	Mount(mux, q, reg)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	id, _ := q.Enqueue(Task{Kind: "stage", MaxAttempts: 2})
	client := NewClient(srv.URL, "w1")
	task, _ := client.Pull(nil)
	if err := client.Fail(task.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	got, _ := q.Get(id)
	if got.Status != "pending" { // 未达上限回队
		t.Fatalf("失败应回队: %s", got.Status)
	}
}
