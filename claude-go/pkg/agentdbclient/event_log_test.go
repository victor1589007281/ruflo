// A6 Event Log 客户端验收测试。
//
// 起进程内 agentDB serve (Backends{KV} only, 与 vkv 测试同款 helper),
// 走真实 HTTP 全链路验证: append 返回 seq、Read 游标续读、batch 连续序号、
// 流间隔离、404/503 错误面。

package agentdbclient

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"gitee.com/lorydb/agentDB/pkg/kv"
	"gitee.com/lorydb/agentDB/pkg/server"
)

func newEventLogServer(t *testing.T) *httptest.Server {
	t.Helper()
	kvDB, err := kv.Open(t.TempDir(), kv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kvDB.Close() })
	srv := server.New(server.Backends{KV: kvDB})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestEventLog_AppendReadRoundTrip(t *testing.T) {
	ts := newEventLogServer(t)
	el := New(ts.URL).EventLog("resolutions")

	s1, err := el.Append([]byte(`{"outcome":"ok"}`))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	s2, err := el.Append([]byte(`{"outcome":"fail"}`))
	if err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if s1 != 1 || s2 != 2 {
		t.Fatalf("seq 应从 1 起, 实际 %d, %d", s1, s2)
	}

	events, next, err := el.Read(0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 2 || next != 2 {
		t.Fatalf("应 2 条 next=2, 实际 %d 条 next=%d", len(events), next)
	}
	if events[0].Seq != 1 || string(events[0].Payload) != `{"outcome":"ok"}` {
		t.Fatalf("首条 seq/payload 错: %+v", events[0])
	}

	// 游标续读: since=1 → 只剩 seq=2
	events2, next2, err := el.Read(1, 0)
	if err != nil {
		t.Fatalf("Read since=1: %v", err)
	}
	if len(events2) != 1 || events2[0].Seq != 2 || next2 != 2 {
		t.Fatalf("续读应 [2], 实际 %+v next=%d", events2, next2)
	}
}

func TestEventLog_BatchAndCursorLoop(t *testing.T) {
	ts := newEventLogServer(t)
	el := New(ts.URL).EventLog("many")

	// batch 5 条
	payloads := make([][]byte, 5)
	for i := range payloads {
		payloads[i] = []byte(fmt.Sprintf(`{"i":%d}`, i))
	}
	first, last, err := el.AppendBatch(payloads)
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if first != 1 || last != 5 {
		t.Fatalf("batch 应 first=1 last=5, 实际 %d..%d", first, last)
	}

	// limit=2 循环游标读完全部 (模拟消费方 at-least-once 主循环)
	var got []int64
	since := int64(0)
	for {
		events, next, err := el.Read(since, 2)
		if err != nil {
			t.Fatalf("cursor loop: %v", err)
		}
		for _, e := range events {
			got = append(got, e.Seq)
		}
		if len(events) == 0 {
			break
		}
		since = next
	}
	if len(got) != 5 {
		t.Fatalf("游标循环应读满 5 条, 实际 %d", len(got))
	}
	for i, seq := range got {
		if seq != int64(i+1) {
			t.Fatalf("seq 应升序 1..5, 实际 %v", got)
		}
	}
}

func TestEventLog_StreamIsolation(t *testing.T) {
	ts := newEventLogServer(t)
	cli := New(ts.URL)
	if _, err := cli.EventLog("a").Append([]byte("a1")); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.EventLog("b").Append([]byte("b1")); err != nil {
		t.Fatal(err)
	}
	events, _, err := cli.EventLog("a").Read(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || string(events[0].Payload) != "a1" {
		t.Fatalf("流 a 应只有 a1, 实际 %+v", events)
	}
}

func TestEventLog_NotConfigured(t *testing.T) {
	// 无 KV backend 的 serve: append 必须报错 (503 NOT_CONFIGURED), 不得假成功。
	srv := server.New(server.Backends{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	_, err := New(ts.URL).EventLog("x").Append([]byte("y"))
	if err == nil {
		t.Fatalf("KV 未装配时 append 应报错")
	}
}
