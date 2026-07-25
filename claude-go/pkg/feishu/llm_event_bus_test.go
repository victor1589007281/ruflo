package feishu

// llm_event_bus_test.go —— design/02 §3.4.2 EventBus "通电"证据。
//
// 这里刻意**不**直接调 Publish 自证自话: 那只能证明总线本身能跑, 证明不了
// "生产代码真的会往上面发东西"。所以用例走完整真实链路 ——
// 起一个必然失败的假 LLM 端点 → 让 api.Client 真跑一次重试循环 →
// fireEvent → 全局 sink → eventbus → 订阅方 —— 只有中间任何一环没接上,
// 用例才会失败。

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/eventbus"
)

// syncBuf 并发安全的日志缓冲 (消费循环在另一个 goroutine 里写日志)。
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// failingLLMServer 恒返回 500 的假端点: api.Client 会重试并在重试前发 retry 事件,
// 重试耗尽后发 fatal 事件。两者都是生产 fireEvent 调用点。
func failingLLMServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
}

// TestLLM事件真链路抵达EventBus订阅方 端到端: 真 api.Client 的重试路径 → 总线 → 消费方。
func TestLLM事件真链路抵达EventBus订阅方(t *testing.T) {
	srv := failingLLMServer(t)
	defer srv.Close()

	var logs syncBuf
	oldOut := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOut)

	// 第二个订阅者: 既是断言点, 也顺带证明总线是**多订阅者广播**而不是单回调
	// (旧 OnLLMEvent 字段只能挂一个消费方, 这正是它被换掉的原因之一)。
	probe, unsubProbe, err := eventbus.Default().Subscribe(llmEventPattern, "")
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer unsubProbe()

	b := &Bot{}
	b.startLLMEventBridge()
	defer b.stopLLMEventBridge()

	// 这个客户端**不是** bot 的主客户端, 也没赋 OnLLMEvent —— 改造前它的
	// 重试/致命失败没有任何人收到 (连日志都没有)。
	cli := api.NewClient(srv.URL, "test-key", "fake-model")
	cli.Tag = "advisor"
	cli.RetryCount = 1
	cli.RetryBase = time.Millisecond
	cli.RetryMax = 2 * time.Millisecond

	if _, err := cli.SimpleComplete(context.Background(), "sys", "user"); err == nil {
		t.Fatal("假端点恒 500, SimpleComplete 应当失败")
	}

	var got []eventbus.Event
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case ev := <-probe:
			got = append(got, ev)
			if len(got) >= 2 { // retry + fatal
				break collect
			}
		case <-deadline:
			break collect
		}
	}
	if len(got) == 0 {
		t.Fatal("EventBus 上零事件 —— 生产 fireEvent 没有接到总线上 (建成未通电)")
	}

	var sawRetry, sawFatal bool
	for _, ev := range got {
		if !strings.HasPrefix(ev.Subject, llmEventSubjectPrefix) {
			t.Fatalf("subject 不符命名约定: %q", ev.Subject)
		}
		if ev.TS == 0 {
			t.Fatal("总线未补时间戳")
		}
		if src, _ := ev.Data["source"].(string); src != "advisor" {
			t.Fatalf("来源标签丢失, 无法判断事件出自哪个 Client: %+v", ev.Data)
		}
		if d, _ := ev.Data["detail"].(string); strings.TrimSpace(d) == "" {
			t.Fatalf("detail 为空, 事件没有诊断价值: %+v", ev.Data)
		}
		switch ev.Subject {
		case llmEventSubjectPrefix + "retry":
			sawRetry = true
		case llmEventSubjectPrefix + "fatal":
			sawFatal = true
		}
	}
	if !sawRetry || !sawFatal {
		t.Fatalf("retry/fatal 两类生产事件应当都到达 (retry=%v fatal=%v)", sawRetry, sawFatal)
	}

	// 消费方**真的跑了**的证据: 日志由 consumeLLMEvents 这个 goroutine 打出。
	// 只断言总线收到不够 —— 那只证明发, 不证明有人收。
	var logged string
	for i := 0; i < 100; i++ {
		logged = logs.String()
		if strings.Contains(logged, "[LLM事件]") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logged, "[LLM事件]") || !strings.Contains(logged, "source=advisor") {
		t.Fatalf("消费方未处理事件 (日志里没有它的产物): %q", logged)
	}
}

// TestLLM事件播报口径不变 播报范围必须与改造前逐字一致, 免得 :18080 的飞书消息量凭空变多。
func TestLLM事件播报口径不变(t *testing.T) {
	cases := []struct {
		source string
		want   bool
		why    string
	}{
		{"feishu", true, "主模型客户端 (bot.go 赋过 OnLLMEvent)"},
		{"feishu:kimi-k2", true, "WithModel 克隆, Tag = 主 Tag + \":\" + model"},
		{"advisor", false, "advisor 客户端改造前根本发不出事件"},
		{"dashboard", false, "dashboard 诊断客户端同上"},
		{"unknown", false, "没打标签的客户端不进飞书"},
		{"", false, "空来源不进飞书"},
	}
	for _, c := range cases {
		if got := shouldBroadcastLLMEvent(c.source); got != c.want {
			t.Errorf("source=%q 播报判定 %v, 期望 %v (%s)", c.source, got, c.want, c.why)
		}
	}
}

// TestLLM事件图标与subject 文案与 subject 拼装的边界。
func TestLLM事件图标与subject(t *testing.T) {
	// 图标表逐字照搬改造前的 switch, default 为 ℹ️ (model_switch/model_restore 落此)。
	for typ, want := range map[string]string{
		"retry": "🔄", "circuit_open": "🔴", "circuit_close": "🟢",
		"fatal": "🚨", "model_switch": "ℹ️", "": "ℹ️",
	} {
		if got := llmEventIcon(typ); got != want {
			t.Errorf("图标 %q → %q, 期望 %q", typ, got, want)
		}
	}
	// 空类型必须兜成一段实名: "llm.event." 这种末段为空的 subject 不会被
	// "llm.event.*" 命中 (通配要求至少一段), 直接拼就成了永远没人收到的事件。
	if s := llmEventSubject(""); s != "llm.event.unknown" {
		t.Fatalf("空事件类型未兜住: %q", s)
	}
	if s := llmEventSubject(" retry "); s != "llm.event.retry" {
		t.Fatalf("subject 未去空白: %q", s)
	}
}
