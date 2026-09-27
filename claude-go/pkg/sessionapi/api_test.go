package sessionapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
)

// fakeHost 是 Host 的一个受控实现。
//
// 关键点: **占位 (reserved) 与 Run 的真实在跑状态绑定** —— Reserve 置位、Run 退出时
// 释放, 而不是测试自己拿一个 bool 摆拍。若把它写成一个可随意设置的标志位,
// "忙时 409" 这条就会变成"我在测试里手动设了忙" —— 同义反复, 拿掉 postMessage 里的
// Reserve 检查照样绿。
//
// 另一条: Reserve **不**等待 gate。它必须是同步返回的 —— 那正是它存在的理由
// (若它要等 Run 开始才算占住, 就又回到了竞态)。
type fakeHost struct {
	mu       sync.Mutex
	specs    map[string]Spec
	reserved map[string]bool
	usage    map[string]*Usage
	tokErr   error
	ranText  map[string]string
	runErr   error
	deleteN  int

	reserveSeq int

	gate        chan struct{}
	releaseOnce sync.Once
	entered     chan string

	// beforeRun 非 nil 时, Run 在**进入前**会等它关闭 —— 用于把"检查"与"占位"
	// 之间那个窗口放大成必然。nil = 不等 (绝大多数测试要的是不卡)。
	beforeRun chan struct{}
}

func newFakeHost() *fakeHost {
	h := &fakeHost{
		specs:    map[string]Spec{},
		reserved: map[string]bool{},
		usage:    map[string]*Usage{},
		ranText:  map[string]string{},
		gate:     make(chan struct{}),
		entered:  make(chan string, 16),
	}
	return h
}

// release 放行所有阻塞中的 Run。幂等 —— 测试里显式放行一次、Cleanup 里再放一次
// 是常态, 重复 close 会 panic 并掩盖真正的断言失败。
func (h *fakeHost) release() { h.releaseOnce.Do(func() { close(h.gate) }) }

func (h *fakeHost) Ensure(sp Spec) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.specs[sp.ID] = sp
	return nil
}

func (h *fakeHost) List() []Spec {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Spec, 0, len(h.specs))
	for _, sp := range h.specs {
		out = append(out, sp)
	}
	return out
}

func (h *fakeHost) Get(id string) (Spec, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sp, ok := h.specs[id]
	if !ok {
		return Spec{}, ErrNotFound
	}
	return sp, nil
}

func (h *fakeHost) Delete(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.specs[id]; !ok {
		return ErrNotFound
	}
	delete(h.specs, id)
	h.deleteN++
	return nil
}

func (h *fakeHost) Clear(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.specs[id]; !ok {
		return ErrNotFound
	}
	return nil
}

func (h *fakeHost) Busy(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reserved[id]
}

func (h *fakeHost) Reserve(id string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reserved[id] {
		return "", ErrBusy
	}
	h.reserved[id] = true
	// runID 由实现方分配, 形状与 pkg/feishu 写进 TraceID 的完全一致
	// (run-<会话>-<序号>) —— 它同时是轨迹桶索引。
	h.reserveSeq++
	return fmt.Sprintf("run-%s-%d", id, h.reserveSeq), nil
}

func (h *fakeHost) Run(ctx context.Context, id, text string) (string, error) {
	if br := h.beforeRun; br != nil {
		<-br
	}
	h.mu.Lock()
	h.ranText[id] = text
	h.mu.Unlock()
	h.entered <- id
	<-h.gate
	h.mu.Lock()
	h.reserved[id] = false // Run 退出即释放占位 (生产侧对应 defer)
	err := h.runErr
	h.mu.Unlock()
	if err != nil {
		return "", err
	}
	return "回复:" + text, nil
}

func (h *fakeHost) Tokens(runID string) (*Usage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tokErr != nil {
		return nil, h.tokErr
	}
	return h.usage[runID], nil
}

// ── 测试脚手架 ──────────────────────────────────────────────────────────────

func newTestServer(t *testing.T, h Host) (*httptest.Server, *Server) {
	t.Helper()
	mux := http.NewServeMux()
	srv := Mount(mux, func() Host { return h })
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, srv
}

func doJSON(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	// 204 无 body —— 解析失败不是错误, 状态码才是判据。
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func post(t *testing.T, url, body string) (int, map[string]any) {
	return doJSON(t, http.MethodPost, url, body)
}
func get(t *testing.T, url string) (int, map[string]any) {
	return doJSON(t, http.MethodGet, url, "")
}

// pollRun 轮询到终态。超时即失败 —— 与调用方在真实场景里做的事情一致。
func pollRun(t *testing.T, url string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		code, body := get(t, url)
		if code != http.StatusOK {
			t.Fatalf("轮询返回 %d: %v", code, body)
		}
		if body["status"] != string(RunRunning) {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("轮询超时, 状态仍为 running: %v", body)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ── 测试 ────────────────────────────────────────────────────────────────────

func TestCreateAndListSessions(t *testing.T) {
	h := newFakeHost()
	t.Cleanup(h.release)
	ts, _ := newTestServer(t, h)

	code, body := post(t, ts.URL+"/api/sessions",
		`{"id":"dba-1","name":"巡检员甲","systemPrompt":"你是 DBA","allowedTools":["Bash"]}`)
	if code != http.StatusCreated {
		t.Fatalf("建会话应返回 201, 得到 %d: %v", code, body)
	}

	code, body = get(t, ts.URL+"/api/sessions")
	if code != http.StatusOK {
		t.Fatalf("列举应返回 200, 得到 %d", code)
	}
	arr, _ := body["sessions"].([]any)
	if len(arr) != 1 {
		t.Fatalf("应有 1 个会话, 得到 %d: %v", len(arr), body)
	}
	item, _ := arr[0].(map[string]any)
	sp, _ := item["spec"].(map[string]any)
	if sp["id"] != "dba-1" || sp["name"] != "巡检员甲" {
		t.Errorf("会话身份没有原样回读: %v", sp)
	}
	// busy 必须在列举里就有: 调用方靠它决定"现在能不能发消息",
	// 缺了它就只能先发再吃 409。
	if _, ok := item["busy"]; !ok {
		t.Errorf("列举项缺 busy 字段: %v", item)
	}
}

func TestMessageIsAsyncAndReachableByRunID(t *testing.T) {
	h := newFakeHost()
	t.Cleanup(h.release)
	ts, _ := newTestServer(t, h)

	post(t, ts.URL+"/api/sessions", `{"id":"dba-1"}`)

	code, body := post(t, ts.URL+"/api/sessions/dba-1/messages", `{"text":"查一下主从延迟"}`)
	if code != http.StatusAccepted {
		t.Fatalf("发消息应返回 202 (异步), 得到 %d: %v", code, body)
	}
	runID, _ := body["runId"].(string)
	if runID == "" {
		t.Fatalf("202 必须带 runId, 否则调用方无从轮询: %v", body)
	}
	if body["status"] != string(RunRunning) {
		t.Errorf("202 的状态应为 %q, 得到 %v", RunRunning, body["status"])
	}
	// runId 的形状 = 桶名形状: 它要能直接当轨迹桶索引去 ReadRun。
	if !strings.HasPrefix(runID, "run-dba-1-") {
		t.Errorf("runId 应以 run-<会话>- 开头 (它同时是轨迹桶索引), 得到 %q", runID)
	}

	// 还没放行 → 仍是 running。这一条是"异步"这个说法的判据:
	// 若接口其实是同步阻塞的, 这里根本走不到 (POST 会一直挂着)。
	_, during := get(t, ts.URL+"/api/sessions/dba-1/runs/"+runID)
	if during["status"] != string(RunRunning) {
		t.Errorf("放行前状态应为 running, 得到 %v", during["status"])
	}
	if _, ok := during["reply"]; ok {
		t.Errorf("未完成不应有 reply 字段: %v", during)
	}

	h.release()
	done := pollRun(t, ts.URL+"/api/sessions/dba-1/runs/"+runID)
	if done["status"] != string(RunCompleted) {
		t.Fatalf("状态应为 completed, 得到 %v", done["status"])
	}
	if done["reply"] != "回复:查一下主从延迟" {
		t.Errorf("reply 不对: %v", done["reply"])
	}

	// Host 真的收到了原文 —— 否则上面那条 reply 可能来自一个空消息。
	h.mu.Lock()
	got := h.ranText["dba-1"]
	h.mu.Unlock()
	if got != "查一下主从延迟" {
		t.Errorf("Host 收到的文本不对: %q", got)
	}
}

func TestBusySessionReturns409WithRunningRunID(t *testing.T) {
	h := newFakeHost()
	t.Cleanup(h.release)
	ts, _ := newTestServer(t, h)

	post(t, ts.URL+"/api/sessions", `{"id":"dba-1"}`)

	_, first := post(t, ts.URL+"/api/sessions/dba-1/messages", `{"text":"第一条"}`)
	firstRun, _ := first["runId"].(string)

	// 等 Run 真的进门 (Busy 只在 Run 内部为 true), 否则这条会变成竞态。
	select {
	case <-h.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 一直没被调用")
	}

	code, second := post(t, ts.URL+"/api/sessions/dba-1/messages", `{"text":"第二条"}`)
	if code != http.StatusConflict {
		t.Fatalf("忙时应返回 409, 得到 %d: %v", code, second)
	}
	if second["runningRunId"] != firstRun {
		t.Errorf("409 必须带上是**哪一次**在跑 (runningRunId=%v), 否则调用方只能盲重试", second["runningRunId"])
	}
}

// TestConcurrentMessagesOnlyOneIsAccepted 是 Reserve 存在的**唯一理由**的判据。
//
// 为什么不能只靠"先 Busy 探测、再起 goroutine": 探测与真正的占位之间有一个窗口,
// 两条同时到达的消息会双双通过检查、双双拿到 202 —— 然后其中一条在会话内部被
// 静默丢弃 (runSessionMessage 看到 IsProcessing 直接返回"上一条还在处理中"当成
// **成功**返回)。调用方手里握着两个 runId, 等一个永远不会完成的运行。
//
// 判别力: 把 Reserve 换成 Busy 探测 (即"检查后到 Run 开始前"的窗口重新打开),
// 这条断言会红 —— 下面用一个刻意放大的窗口来稳定复现, 不依赖运气。
func TestConcurrentMessagesOnlyOneIsAccepted(t *testing.T) {
	h := newFakeHost()
	t.Cleanup(h.release)

	// ★ 放大窗口: 让 Run **在进入前**先停一下。真实的 flapping 窗口是几微秒,
	// 靠并发压力去撞它是碰运气; 这里把它变成必然。
	h.beforeRun = make(chan struct{})
	ts, _ := newTestServer(t, h)
	post(t, ts.URL+"/api/sessions", `{"id":"dba-1"}`)

	const n = 8
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := post(t, ts.URL+"/api/sessions/dba-1/messages", `{"text":"并发"}`)
			codes <- code
		}()
	}
	wg.Wait()
	close(h.beforeRun) // 放行, 免得 Run 卡住
	close(codes)

	accepted, conflicted := 0, 0
	for c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflicted++
		default:
			t.Errorf("意外的状态码 %d", c)
		}
	}
	if accepted != 1 {
		t.Fatalf("%d 条并发消息里应恰好 1 条被接受, 实际 %d 条 (conflicted=%d)\n"+
			"★ 多于 1 条 = 检查不是原子的: 调用方会等一个永远不会完成的 run", accepted, accepted, conflicted)
	}
	if conflicted != n-1 {
		t.Errorf("其余 %d 条都应 409, 实际 %d 条", n-1, conflicted)
	}
}

func TestDeleteThenGetIs404(t *testing.T) {
	h := newFakeHost()
	t.Cleanup(h.release)
	ts, _ := newTestServer(t, h)

	post(t, ts.URL+"/api/sessions", `{"id":"dba-1"}`)

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/sessions/dba-1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("删除应返回 204, 得到 %d", resp.StatusCode)
	}

	if code, body := get(t, ts.URL+"/api/sessions/dba-1"); code != http.StatusNotFound {
		t.Errorf("删除后 GET 应 404, 得到 %d: %v", code, body)
	}
	// 反向对照: 删一个从没存在过的会话也必须 404 ——
	// 若实现是"删了就当成功", 上面那条会绿而这条会红。
	if code, _ := get(t, ts.URL+"/api/sessions/从没存在过"); code != http.StatusNotFound {
		t.Errorf("不存在的会话 GET 应 404, 得到 %d", code)
	}
}

// TestTokensAbsentIsNotZero 是这一组里**最容易被写错**的一条。
//
// 采不到 token 与 token 为 0 在轮询侧必须可区分: 前者是"没接轨迹底座",
// 后者是"跑了但没消耗"。若把采不到的 nil 直接写进 JSON, 结果是
// `"tokens": null` —— 而一个真的跑了 0 次调用的会话, 字段会是
// `"tokens": {"in":0,"out":0,"calls":0}`, 两者在阅读代码的人眼里**长得一样**。
//
// 所以判据是**键在不在**, 不是值等于几。
func TestTokensAbsentIsNotZero(t *testing.T) {
	h := newFakeHost()
	t.Cleanup(h.release)
	ts, _ := newTestServer(t, h)
	post(t, ts.URL+"/api/sessions", `{"id":"dba-1"}`)

	_, b1 := post(t, ts.URL+"/api/sessions/dba-1/messages", `{"text":"甲"}`)
	run1, _ := b1["runId"].(string)
	h.release()
	done1 := pollRun(t, ts.URL+"/api/sessions/dba-1/runs/"+run1)
	if _, ok := done1["tokens"]; ok {
		t.Errorf("Host 报 nil (=采不到) 时不该出现 tokens 键, 得到 %v", done1["tokens"])
	}

	// 反向对照: 采得到时必须有, 且值原样透出。
	h.mu.Lock()
	h.usage[run1] = &Usage{In: 1200, Out: 34, Calls: 2}
	h.mu.Unlock()
	done1b := pollRun(t, ts.URL+"/api/sessions/dba-1/runs/"+run1)
	tok, ok := done1b["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("采得到时必须有 tokens: %v", done1b)
	}
	if tok["in"] != float64(1200) || tok["out"] != float64(34) || tok["calls"] != float64(2) {
		t.Errorf("tokens 值未原样透出: %v", tok)
	}
}

// TestFailedRunCarriesError —
func TestFailedRunCarriesError(t *testing.T) {
	h := newFakeHost()
	t.Cleanup(h.release)
	h.runErr = context.DeadlineExceeded
	ts, _ := newTestServer(t, h)
	post(t, ts.URL+"/api/sessions", `{"id":"dba-1"}`)

	_, b := post(t, ts.URL+"/api/sessions/dba-1/messages", `{"text":"甲"}`)
	runID, _ := b["runId"].(string)
	h.release()

	done := pollRun(t, ts.URL+"/api/sessions/dba-1/runs/"+runID)
	if done["status"] != string(RunFailed) {
		t.Fatalf("错误路径的状态应为 failed, 得到 %v", done["status"])
	}
	if errText, _ := done["error"].(string); !strings.Contains(errText, "deadline exceeded") {
		t.Errorf("error 文本应透出, 得到 %v", done["error"])
	}
}

// TestNilHostIsServiceUnavailable 挡的是装配顺序:
// 扩展闭包在 serve 装配期注册, 那时 Bot 还没构造出来 (botRef == nil)。
// 若这里 panic, **整个进程起不来** —— 而不是"会话接口暂时不可用"。
func TestNilHostIsServiceUnavailable(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	if code, body := get(t, ts.URL+"/api/sessions"); code != http.StatusServiceUnavailable {
		t.Errorf("Host 未就绪应返回 503, 得到 %d: %v", code, body)
	}
	if code, _ := post(t, ts.URL+"/api/sessions", `{"id":"x"}`); code != http.StatusServiceUnavailable {
		t.Errorf("Host 未就绪时建会话也应 503, 得到 %d", code)
	}
	if code, _ := get(t, ts.URL+"/api/sessions/x"); code != http.StatusServiceUnavailable {
		t.Errorf("Host 未就绪时子路径也应 503, 得到 %d", code)
	}
}

func TestBadRequests(t *testing.T) {
	h := newFakeHost()
	t.Cleanup(h.release)
	ts, _ := newTestServer(t, h)

	if code, _ := post(t, ts.URL+"/api/sessions", `{"id":"  "}`); code != http.StatusBadRequest {
		t.Errorf("空 id 应 400, 得到 %d", code)
	}
	post(t, ts.URL+"/api/sessions", `{"id":"dba-1"}`)
	if code, _ := post(t, ts.URL+"/api/sessions/dba-1/messages", `{"text":""}`); code != http.StatusBadRequest {
		t.Errorf("空 text 应 400, 得到 %d", code)
	}
	if code, _ := post(t, ts.URL+"/api/sessions/不存在/messages", `{"text":"a"}`); code != http.StatusNotFound {
		t.Errorf("给不存在的会话发消息应 404, 得到 %d", code)
	}
	if code, _ := get(t, ts.URL+"/api/sessions/dba-1/runs/run-假的"); code != http.StatusNotFound {
		t.Errorf("不存在的 run 应 404, 得到 %d", code)
	}
	if code, _ := get(t, ts.URL+"/api/sessions/dba-1/不认识的子路径"); code != http.StatusNotFound {
		t.Errorf("未知子路径应 404, 得到 %d", code)
	}
}

// TestMountUsesTheGivenMux —
//
// 这条守的是**鉴权覆盖面**, 不是路由本身: 生产上 httpauth.Middleware 是在
// pkg/wiki/api.go 的 Start 里对整个 mux 施加的 (api.go:126)。只要路由挂在那一个
// mux 上, 就自动被覆盖; 而若 Mount 自己起 listener, 那条中间件就绕过去了 ——
// 本仓在 dashboard 与 cluster 路由上踩过完全同型的坑。
//
// 所以判据是: 请求确实**穿过**了套在那个 mux 外面的处理器。判别力有限
// (中间件是测试自己套的), 它挡的是"Mount 悄悄自己开了一个端口"这一类改动。
func TestMountUsesTheGivenMux(t *testing.T) {
	h := newFakeHost()
	t.Cleanup(h.release)

	mux := http.NewServeMux()
	Mount(mux, func() Host { return h })

	var seen []string
	var mu sync.Mutex
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		mux.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(wrapped)
	t.Cleanup(ts.Close)

	if code, _ := get(t, ts.URL+"/api/sessions"); code != http.StatusOK {
		t.Fatalf("经外层处理器访问应正常, 得到 %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("请求没有穿到外层处理器 —— 说明 Mount 绕开了这个 mux")
	}
}

// ── Spec 三态 ───────────────────────────────────────────────────────────────

// TestAllowedToolsThreeStateSurvivesJSONRoundTrip 守的是**落盘-读回**这一跳。
//
// 三个状态在语义上是递进的: 不限 > 白名单 > 全拒。用值类型 + `omitempty` 时,
// "全拒" (&[]string{}) 会被序列化成**没有这个字段**, 读回就是"不限" ——
// 声明越严、重启后越松。这条断言的就是那个方向不被翻转。
func TestAllowedToolsThreeStateSurvivesJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		in    *[]string
		state string // "unlimited" | "denyAll" | "whitelist"
	}{
		{"字段缺席 = 不限", nil, "unlimited"},
		{"空数组 = 全拒", ToolsDenyAll(), "denyAll"},
		{"具名 = 白名单", ToolsAllow("Bash", "Read"), "whitelist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(Spec{ID: "s", AllowedTools: tc.in})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back Spec
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			gotState := "denyAll"
			switch m, ok := back.AllowedToolsMap(); {
			case !ok:
				gotState = "unlimited"
			case len(m) > 0:
				gotState = "whitelist"
			}
			if gotState != tc.state {
				t.Fatalf("经一次 JSON 往返, 三态从 %q 变成了 %q (原始 JSON: %s)\n"+
					"★ 「全拒」变成「不限」是最危险的一种退化: 声明越严, 重启后越松, 且无提示",
					tc.state, gotState, raw)
			}

			// 反向对照: 白名单的工具名也要原样回来 —— 状态对了但名单丢了同样是放宽。
			if tc.state == "whitelist" {
				m, _ := back.AllowedToolsMap()
				for _, n := range *tc.in {
					if !m[n] {
						t.Errorf("白名单丢了 %q: %s", n, raw)
					}
				}
			}
		})
	}
}

// ── TokensFromTrace ─────────────────────────────────────────────────────────

func TestTokensFromTraceSumsLLMCallsOnly(t *testing.T) {
	ss := statestore.NewFileStore(t.TempDir())
	store := tracestore.New(ss)
	const runID = "run-dba-1-7"

	write := func(kind string, attrs map[string]any) {
		store.Write(tracestore.Span{
			TraceID: runID,
			SpanID:  runID + "-" + kind + "-" + time.Now().Format("150405.000000"),
			Kind:    kind,
			Name:    kind,
			Attrs:   attrs,
		})
	}
	// run Span 也在这条轨迹里, 但它没有 token 字段 —— 不该被算进去 (也不该让它
	// 变成"采到了")。
	write(tracestore.KindRun, map[string]any{"team": "巡检员甲"})
	write(tracestore.KindLLMCall, map[string]any{"input_tokens": 100, "output_tokens": 10, "cache_read_tokens": 5})
	write(tracestore.KindLLMCall, map[string]any{"input_tokens": 200, "output_tokens": 20, "cache_creation_tokens": 7})

	u, err := TokensFromTrace(store, runID)
	if err != nil {
		t.Fatalf("TokensFromTrace: %v", err)
	}
	if u == nil {
		t.Fatal("有 llm_call 却报采不到")
	}
	if u.In != 300 || u.Out != 30 || u.CacheRead != 5 || u.CacheCreate != 7 || u.Calls != 2 {
		t.Errorf("累加不对: %+v", u)
	}
}

// TestTokensFromTraceEmptyIsNil —
// 空轨迹返回 nil 而不是 &Usage{}: 见 TestTokensAbsentIsNotZero 的理由。
func TestTokensFromTraceEmptyIsNil(t *testing.T) {
	ss := statestore.NewFileStore(t.TempDir())
	store := tracestore.New(ss)

	u, err := TokensFromTrace(store, "run-从没跑过-1")
	if err != nil {
		t.Fatalf("读不到不该报错 (轮询不该因为采不到而失败): %v", err)
	}
	if u != nil {
		t.Errorf("采不到必须返回 nil, 得到 %+v —— 恒 0 与没采集会长得一样", u)
	}

	if u, err := TokensFromTrace(nil, "x"); err != nil || u != nil {
		t.Errorf("store 为 nil 时也应是 (nil, nil), 得到 (%v, %v)", u, err)
	}
}
