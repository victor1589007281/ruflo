package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// 测试脚手架
// ============================================================================

// newTestSessionManager 造一个最小可用的 SessionManager。
//
// 注意: NewSessionManager 会起一个 5 分钟 tick 的后台 cleanup goroutine 且没有停止
// 入口。测试进程活不到第一次 tick, 且该 goroutine 只在 tick 后才碰 sm.sessions,
// 所以对 -race 无影响 (下面的并发用例实测通过 -race)。
func newTestSessionManager(t *testing.T, ss statestore.StateStore, tweak func(*BotConfig)) *SessionManager {
	t.Helper()
	return newTestSessionManagerWithLLM(t, ss, "http://127.0.0.1:1", tweak)
}

func newTestSessionManagerWithLLM(t *testing.T, ss statestore.StateStore, baseURL string, tweak func(*BotConfig)) *SessionManager {
	t.Helper()
	cfg := DefaultBotConfig()
	cfg.Cwd = t.TempDir()
	cfg.StateDir = filepath.Join(cfg.Cwd, ".claude-go")
	cfg.EnableFrontierOptimizations = false // 测试只验持久化, 不引入前沿组件噪声
	if tweak != nil {
		tweak(cfg)
	}
	client := api.NewClient(baseURL, "test-key", "test-model")
	client.Tag = "test"
	return NewSessionManager(cfg, client, nil, nil, nil, nil, nil, nil, nil, nil,
		WithStateStore(ss))
}

// forEachBackend 对 file 与 mem 两个后端各跑一遍 (照 statestore_test.go 的双跑范式)。
// open() 每次返回"指向同一份存储"的 StateStore: file 后端返回同目录的新实例
// (= 模拟进程重启), mem 后端只能复用同一实例。
func forEachBackend(t *testing.T, fn func(t *testing.T, open func() statestore.StateStore)) {
	t.Helper()
	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		fn(t, func() statestore.StateStore { return statestore.NewFileStore(dir) })
	})
	t.Run("mem", func(t *testing.T) {
		mem := statestore.NewMemStore()
		fn(t, func() statestore.StateStore { return mem })
	})
}

// richHistory 一条含 text / thinking / tool_use / tool_result 的完整链条。
// 时间统一用固定 UTC 值, 保证 JSON 往返后可精确比较。
func richHistory(marker string) []types.Message {
	ts := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	return []types.Message{
		{
			Type: types.MessageTypeUser, UUID: "u-" + marker, CreatedAt: ts,
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "帮我看下 " + marker}},
		},
		{
			Type: types.MessageTypeAssistant, UUID: "a-" + marker, Model: "test-model", CreatedAt: ts,
			Content: []types.ContentBlock{
				{Type: types.ContentBlockThinking, Thinking: "思考 " + marker},
				{Type: types.ContentBlockText, Text: "先读文件"},
				{Type: types.ContentBlockToolUse, ID: "tu-" + marker, Name: "Read", Input: json.RawMessage(`{"path":"a.go"}`)},
			},
		},
		{
			Type: types.MessageTypeUser, UUID: "r-" + marker, CreatedAt: ts,
			Content: []types.ContentBlock{
				{Type: types.ContentBlockToolResult, ToolUseID: "tu-" + marker, Content: "内容 " + marker},
			},
		},
		{
			Type: types.MessageTypeAssistant, UUID: "a2-" + marker, Model: "test-model", CreatedAt: ts,
			IsCompactBoundary: true,
			Content:           []types.ContentBlock{{Type: types.ContentBlockText, Text: "结论 " + marker}},
		},
	}
}

// assertMessagesEqual 深度比较消息链 (逐字段, 失败时能指出具体位置)。
func assertMessagesEqual(t *testing.T, want, got []types.Message) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("消息条数不等: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.Type != g.Type || w.UUID != g.UUID || w.Model != g.Model ||
			w.IsCompactBoundary != g.IsCompactBoundary || w.IsMeta != g.IsMeta {
			t.Fatalf("msg[%d] 元信息不等:\n want %+v\n got  %+v", i, w, g)
		}
		if !w.CreatedAt.Equal(g.CreatedAt) {
			t.Fatalf("msg[%d] CreatedAt 不等: want %v, got %v", i, w.CreatedAt, g.CreatedAt)
		}
		if len(w.Content) != len(g.Content) {
			t.Fatalf("msg[%d] 内容块数不等: want %d, got %d", i, len(w.Content), len(g.Content))
		}
		for j := range w.Content {
			wb, gb := w.Content[j], g.Content[j]
			if wb.Type != gb.Type || wb.Text != gb.Text || wb.Thinking != gb.Thinking ||
				wb.ID != gb.ID || wb.Name != gb.Name || string(wb.Input) != string(gb.Input) ||
				wb.ToolUseID != gb.ToolUseID || wb.Content != gb.Content || wb.IsError != gb.IsError {
				t.Fatalf("msg[%d].block[%d] 不等:\n want %+v\n got  %+v", i, j, wb, gb)
			}
			if (wb.Source == nil) != (gb.Source == nil) {
				t.Fatalf("msg[%d].block[%d] Source 存在性不等", i, j)
			}
			if wb.Source != nil && *wb.Source != *gb.Source {
				t.Fatalf("msg[%d].block[%d] Source 不等:\n want %+v\n got  %+v", i, j, *wb.Source, *gb.Source)
			}
		}
	}
}

// snapshotNow 模拟"一轮结束": 把历史塞进引擎并触发 createSession 接线好的 SnapshotFn。
func snapshotNow(t *testing.T, s *Session, msgs []types.Message) {
	t.Helper()
	if s.Engine.SnapshotFn == nil {
		t.Fatal("createSession 未给 Engine.SnapshotFn 接线, 会话历史不会落盘")
	}
	s.Engine.Messages = msgs
	s.Engine.SnapshotFn(msgs)
}

// ============================================================================
// 1. 快照读回 (design/02 §七 R2 验收: kill -9 后会话可续)
// ============================================================================

func TestSessionHistorySurvivesRestart(t *testing.T) {
	forEachBackend(t, func(t *testing.T, open func() statestore.StateStore) {
		const chatID = "oc_restart"
		want := richHistory("restart")

		sm1 := newTestSessionManager(t, open(), nil)
		s1 := sm1.GetOrCreate(chatID)
		if len(s1.Engine.Messages) != 0 {
			t.Fatalf("首次创建的会话应无历史, 得到 %d 条", len(s1.Engine.Messages))
		}
		snapshotNow(t, s1, want)

		// "进程重启": 同一份存储上新建 SessionManager (file 后端连 FileStore 实例都是新的)
		sm2 := newTestSessionManager(t, open(), nil)
		s2 := sm2.GetOrCreate(chatID)
		assertMessagesEqual(t, want, s2.Engine.Messages)

		// 别的 chat 不该被串到
		if other := sm2.GetOrCreate("oc_other").Engine.Messages; len(other) != 0 {
			t.Fatalf("未写过快照的 chat 读到了 %d 条历史", len(other))
		}
	})
}

// ============================================================================
// 2. /clear 语义: 清了就不能复活
// ============================================================================

func TestClearSessionRemovesPersistedHistory(t *testing.T) {
	forEachBackend(t, func(t *testing.T, open func() statestore.StateStore) {
		const chatID = "oc_clear"
		sm1 := newTestSessionManager(t, open(), nil)
		snapshotNow(t, sm1.GetOrCreate(chatID), richHistory("clear"))

		sm1.ClearSession(chatID)

		// 同一个 manager 内重建
		if got := sm1.GetOrCreate(chatID).Engine.Messages; len(got) != 0 {
			t.Fatalf("/clear 后同进程重建仍读到 %d 条历史", len(got))
		}
		// 重启后重建
		sm2 := newTestSessionManager(t, open(), nil)
		if got := sm2.GetOrCreate(chatID).Engine.Messages; len(got) != 0 {
			t.Fatalf("/clear 后重启重建仍读到 %d 条历史 (历史复活了)", len(got))
		}
	})
}

// TestClearSessionOnNonResidentChat 会话已被淘汰出内存时, /clear 仍要能删盘。
func TestClearSessionOnNonResidentChat(t *testing.T) {
	dir := t.TempDir()
	const chatID = "oc_gone"
	sm := newTestSessionManager(t, statestore.NewFileStore(dir), nil)
	snapshotNow(t, sm.GetOrCreate(chatID), richHistory("gone"))

	// 手动把会话踢出内存 (模拟已被 evict/cleanup 回收), 再 /clear
	sm.mu.Lock()
	delete(sm.sessions, chatID)
	sm.mu.Unlock()
	sm.ClearSession(chatID)

	sm2 := newTestSessionManager(t, statestore.NewFileStore(dir), nil)
	if got := sm2.GetOrCreate(chatID).Engine.Messages; len(got) != 0 {
		t.Fatalf("对非常驻会话执行 /clear 未删盘, 仍读到 %d 条", len(got))
	}
}

// ============================================================================
// 3. 淘汰 ≠ 遗忘: evictOldest / cleanup 只卸内存, 不删盘
// ============================================================================

func TestEvictionKeepsPersistedHistory(t *testing.T) {
	dir := t.TempDir()
	sm := newTestSessionManager(t, statestore.NewFileStore(dir), func(c *BotConfig) {
		c.MaxSessions = 1 // 任何新会话都会挤掉上一个
	})

	want := richHistory("evicted")
	snapshotNow(t, sm.GetOrCreate("oc_a"), want)

	// 触发 LRU 淘汰
	sm.GetOrCreate("oc_b")
	sm.mu.RLock()
	_, stillResident := sm.sessions["oc_a"]
	sm.mu.RUnlock()
	if stillResident {
		t.Fatal("maxSessions=1 时 oc_a 应已被淘汰出内存")
	}

	// 重新说话 → 历史必须回来
	assertMessagesEqual(t, want, sm.GetOrCreate("oc_a").Engine.Messages)
}

func TestCleanupKeepsPersistedHistory(t *testing.T) {
	dir := t.TempDir()
	sm := newTestSessionManager(t, statestore.NewFileStore(dir), func(c *BotConfig) {
		c.SessionTimeout = time.Minute
	})
	const chatID = "oc_idle"
	want := richHistory("idle")
	s := sm.GetOrCreate(chatID)
	snapshotNow(t, s, want)

	// 把 LastActive 拨到很久以前, 触发超时清理
	s.mu.Lock()
	s.LastActive = time.Now().Add(-2 * time.Hour)
	s.mu.Unlock()
	sm.cleanup()

	if sm.Get(chatID) != nil {
		t.Fatal("超时会话应已被 cleanup 卸载出内存")
	}
	assertMessagesEqual(t, want, sm.GetOrCreate(chatID).Engine.Messages)
}

// ============================================================================
// 4. 并发 (go test -race): N chat × M 轮, 各 chat 数据不串
// ============================================================================

func TestConcurrentSnapshotsPerChatIsolation(t *testing.T) {
	dir := t.TempDir()
	// 共享单个 FileStore 实例 —— 坑 5.1: FileStore 的 bucket 锁是 per-instance,
	// 同一 root 建多个实例等于没有互斥。
	sm := newTestSessionManager(t, statestore.NewFileStore(dir), nil)

	const chats, rounds = 8, 20
	var wg sync.WaitGroup
	for c := 0; c < chats; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			chatID := fmt.Sprintf("oc_%d", c)
			s := sm.GetOrCreate(chatID)
			for r := 0; r < rounds; r++ {
				msgs := richHistory(fmt.Sprintf("%d-%d", c, r))
				if s.Engine.SnapshotFn == nil {
					t.Errorf("chat %s 未接线 SnapshotFn", chatID)
					return
				}
				s.Engine.SnapshotFn(msgs)
			}
		}(c)
	}
	wg.Wait()

	// 重启后逐 chat 校验: 只能看到自己最后一轮的数据
	sm2 := newTestSessionManager(t, statestore.NewFileStore(dir), nil)
	for c := 0; c < chats; c++ {
		chatID := fmt.Sprintf("oc_%d", c)
		want := richHistory(fmt.Sprintf("%d-%d", c, rounds-1))
		assertMessagesEqual(t, want, sm2.GetOrCreate(chatID).Engine.Messages)
	}
}

// ============================================================================
// 7. fail-open: 落盘必失败时对话照常返回, 只增错误计数
// ============================================================================

// alwaysFailStateStore 让 KV 恒定报错 (走 filestore.go 的 badBucketKV 路径),
// Log/Blob 正常, 用来验证"快照写失败绝不能影响回复"。
type alwaysFailStateStore struct{ inner statestore.StateStore }

func (f alwaysFailStateStore) KV(string) statestore.KVStore {
	return f.inner.KV("非法/bucket 名") // validateBucket 必然拒绝 → 所有方法报错
}
func (f alwaysFailStateStore) Log(b string) statestore.AppendLog { return f.inner.Log(b) }
func (f alwaysFailStateStore) Blob() statestore.BlobStore        { return f.inner.Blob() }

// newFakeLLMServer 返回一条最小合法 SSE 流 (单个文本块, end_turn, 无工具调用)。
func newFakeLLMServer(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		send := func(d types.StreamDelta) {
			b, err := json.Marshal(d)
			if err != nil {
				t.Errorf("序列化 SSE 事件失败: %v", err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		send(types.StreamDelta{Type: "message_start", Message: &types.APIResponse{
			Usage: &types.Usage{InputTokens: 12},
		}})
		send(types.StreamDelta{Type: "content_block_start",
			ContentBlock: &types.ContentBlock{Type: types.ContentBlockText}})
		send(types.StreamDelta{Type: "content_block_delta",
			Delta: &types.DeltaContent{Type: "text_delta", Text: reply}})
		send(types.StreamDelta{Type: "content_block_stop"})
		send(types.StreamDelta{Type: "message_delta",
			Delta: &types.DeltaContent{StopReason: "end_turn"},
			Usage: &types.Usage{OutputTokens: 7}})
		send(types.StreamDelta{Type: "message_stop"})
	}))
}

func TestSnapshotFailureDoesNotBreakReply(t *testing.T) {
	const reply = "已收到, 这是回复正文。"
	srv := newFakeLLMServer(t, reply)
	defer srv.Close()

	ss := alwaysFailStateStore{inner: statestore.NewFileStore(t.TempDir())}
	sm := newTestSessionManagerWithLLM(t, ss, srv.URL, nil)

	const chatID = "oc_failopen"
	// createSession 里的 Load 就会失败 (只记日志), 会话仍要能建出来
	s := sm.GetOrCreate(chatID)
	if s == nil || s.Engine == nil {
		t.Fatal("Load 失败时 GetOrCreate 仍必须返回可用会话")
	}
	if s.transcript == nil {
		t.Fatal("transcript 句柄未接线")
	}
	if s.transcript.ReadErrors() == 0 {
		t.Fatal("Load 失败应计入 ReadErrors")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := sm.ProcessMessage(ctx, chatID, "你好")
	if err != nil {
		t.Fatalf("快照写失败时对话不应报错: %v", err)
	}
	if !strings.Contains(got, reply) {
		t.Fatalf("对话未正常返回, got %q", got)
	}
	if s.transcript.WriteErrors() == 0 {
		t.Fatal("快照写失败应计入 WriteErrors (fail-open 但可观测)")
	}
	// 引擎内存里的历史照旧完整 (落盘失败不回滚内存态)
	if len(s.Engine.Messages) < 2 {
		t.Fatalf("内存历史应含 user+assistant, 得到 %d 条", len(s.Engine.Messages))
	}
}

// TestSubmitMessageTriggersSnapshotOncePerTurn 真走一轮 SubmitMessage, 验证
// 引擎在轮末确实调了 SnapshotFn (每轮恰好一次), 且落盘内容可被下一次启动读回。
func TestSubmitMessageTriggersSnapshotOncePerTurn(t *testing.T) {
	const reply = "第一轮回复"
	srv := newFakeLLMServer(t, reply)
	defer srv.Close()

	dir := t.TempDir()
	sm := newTestSessionManagerWithLLM(t, statestore.NewFileStore(dir), srv.URL, nil)
	const chatID = "oc_realturn"
	s := sm.GetOrCreate(chatID)

	// 在真 SnapshotFn 外面套一层计数
	inner := s.Engine.SnapshotFn
	if inner == nil {
		t.Fatal("SnapshotFn 未接线")
	}
	var calls int
	s.Engine.SnapshotFn = func(msgs []types.Message) {
		calls++
		inner(msgs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := sm.ProcessMessage(ctx, chatID, "你好")
	if err != nil {
		t.Fatalf("ProcessMessage 失败: %v", err)
	}
	if !strings.Contains(got, reply) {
		t.Fatalf("回复不符: %q", got)
	}
	if calls != 1 {
		t.Fatalf("一轮应恰好落一次快照, 实际 %d 次", calls)
	}

	// "重启"读回: 至少含用户那句与助手回复
	sm2 := newTestSessionManagerWithLLM(t, statestore.NewFileStore(dir), srv.URL, nil)
	restored := sm2.GetOrCreate(chatID).Engine.Messages
	if len(restored) < 2 {
		t.Fatalf("读回历史过短: %d 条", len(restored))
	}
	var sawUser, sawAssistant bool
	for _, m := range restored {
		for _, b := range m.Content {
			if m.Type == types.MessageTypeUser && b.Text == "你好" {
				sawUser = true
			}
			if m.Type == types.MessageTypeAssistant && strings.Contains(b.Text, reply) {
				sawAssistant = true
			}
		}
	}
	if !sawUser || !sawAssistant {
		t.Fatalf("读回历史缺内容: sawUser=%v sawAssistant=%v (%d 条)", sawUser, sawAssistant, len(restored))
	}
}

// TestNoStateStoreDisablesPersistence 未注入 StateStore 时行为退化为老样子
// (不落盘、不接线), 但绝不能 panic。
func TestNoStateStoreDisablesPersistence(t *testing.T) {
	cfg := DefaultBotConfig()
	cfg.Cwd = t.TempDir()
	client := api.NewClient("http://127.0.0.1:1", "k", "test-model")
	sm := NewSessionManager(cfg, client, nil, nil, nil, nil, nil, nil, nil, nil)
	s := sm.GetOrCreate("oc_nostore")
	if s.Engine.SnapshotFn != nil {
		t.Fatal("未注入 StateStore 时不应接线 SnapshotFn")
	}
	if s.transcript != nil {
		t.Fatal("未注入 StateStore 时不应有 transcript 句柄")
	}
	sm.ClearSession("oc_nostore") // 不得 panic
}
