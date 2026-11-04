package harness

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件覆盖两块验收：
//
//   - Q-H12 回放确定性：录制/回放的请求**字节**一致、帧序一致；录制不够长时
//     必须显式失败而不是回空 200。
//   - Q-H9 兼容性：旧配置（只有 system 稳定段）的 PrefixHash 被一个写死的
//     常量钉住。任何无意改动哈希口径（混入 Version、换分隔符、改字段顺序）
//     都会让这条测试立刻变红。

// ============================================================================
// 测试辅助：一个最小的上游模型服务
// ============================================================================

// fakeUpstream 是测试自己的最小上游。
//
// 不用 harnesstest.FakeModel：这里的回放测试要自己抓帧原文，而 FakeModel 的
// Captured 不保存响应帧、只保存请求；再包一层反而让"回放写出的是什么字节"
// 变得间接。Runner 也不参与 —— 本测试钉的是传输层字节，与 run loop 无关。
type fakeUpstream struct {
	bodies []string
	frames [][]string
}

func (u *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		idx := len(u.bodies)
		u.bodies = append(u.bodies, string(body))

		var fs []string
		if idx < len(u.frames) {
			fs = u.frames[idx]
		} else {
			fs = textFramesForReplay("ok(脚本外)")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range fs {
			_, _ = io.WriteString(w, f)
		}
	})
}

// textFramesForReplay 是一段最小的合法文本 SSE 回复。
func textFramesForReplay(text string) []string {
	return []string{
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"role\":\"assistant\"}}\n\n",
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n",
		"data: {\"type\":\"message_stop\"}\n\n",
	}
}

// toolFramesForReplay 是一次工具调用回复；工具名/参数与顺序都会被回放比较。
func toolFramesForReplay(name, input string) []string {
	return []string{
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"role\":\"assistant\"}}\n\n",
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"" + name + "\"}}\n\n",
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":" + strconvQuote(input) + "}}\n\n",
		"data: {\"type\":\"message_stop\"}\n\n",
	}
}

// strconvQuote 给 JSON 片段加引号（测试辅助，仅处理本文件用到的 ASCII 片段）。
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// post 向 url 发一个请求并把响应体读全。
func post(t *testing.T, url, body string) (status int, respBody string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发请求失败: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	return resp.StatusCode, string(b)
}

// splitSSEFrames 把响应体按 "\n\n" 边界切回帧原文（含分隔符）。
//
// 用于把回放响应还原成 Frames 再与录制比较：如果直接拼成一个字符串比较，
// 单条帧内部的分段差异仍能看到，但帧边界本身（哪几帧）就丢了。
func splitSSEFrames(body string) []string {
	var out []string
	for len(body) > 0 {
		i := strings.Index(body, "\n\n")
		if i < 0 {
			out = append(out, body)
			break
		}
		out = append(out, body[:i+2])
		body = body[i+2:]
	}
	return out
}

// callOf 从一次实际发出的请求构造 RecordedCall（请求侧字段）。
func callOf(t *testing.T, method, path, body string) RecordedCall {
	t.Helper()
	return RecordedCall{Method: method, Path: path, Body: body, BodySHA: hashText(body)}
}

// ============================================================================
// Q-H12：录制 → 落盘 → 加载 → 回放 → 再跑一遍
// ============================================================================

// TestReplayRoundTripByteIdentical 是 Q-H12 的主验收：
//
//	第一次运行（真上游）→ Capture → Save → Load → Handler() 回放 → 第二次运行，
//	两次发出的请求体必须逐字节相同（DiffRequests 返回空），回放写出的帧也必须
//	与录制逐字节、逐顺序相同（DiffResponses 返回空）。
//
// 两次调用的设计带一次工具调用：先 tool_use、后带 tool_result 的第二轮。这样
// "工具调用顺序一致"也在这条测试里被实际走过，而不是只看一条文本回复。
func TestReplayRoundTripByteIdentical(t *testing.T) {
	up := &fakeUpstream{frames: [][]string{
		toolFramesForReplay("memory_search", `{"query":"今天心情"}`),
		textFramesForReplay("我记得你昨天有点累。"),
	}}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()

	// 请求体刻意保留手写的键序与空白：如果哪一层偷偷做了 Marshal/Unmarshal
	// 往返，这些字节会变，测试就会红。
	firstBodies := []string{
		`{"model":"claude-test","messages":[{"role":"user","content":[{"type":"text","text":"今天有点累"}]}],"tools":[{"name":"memory_search"}]}`,
		`{"model":"claude-test","messages":[{"role":"user","content":"今天有点累"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"memory_search","input":{"query":"今天心情"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"找到了"}]}]}`,
	}

	rec := NewRecording()
	path := "/v1/messages"
	var want []RecordedCall
	for _, body := range firstBodies {
		status, respBody := post(t, srv.URL+path, body)
		if status != http.StatusOK {
			t.Fatalf("上游应返回 200，实际 %d", status)
		}
		c := callOf(t, http.MethodPost, path, body)
		c.Status = status
		c.Frames = splitSSEFrames(respBody)
		if err := rec.Capture(c); err != nil {
			t.Fatalf("Capture 失败: %v", err)
		}
		want = append(want, c)
	}
	if len(want) != 2 {
		t.Fatalf("应录制到 2 次调用，实际 %d", len(want))
	}

	// 落盘再加载：这一跳刻意经过 JSON，用来证明 Body 的原始字节没有被
	// 序列化/反序列化改动（含请求体里的空白与键序）。
	file := filepath.Join(t.TempDir(), "recording.json")
	if err := rec.Save(file); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	loaded, err := LoadRecording(file)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got := loaded.Snapshot(); len(got) != len(want) {
		t.Fatalf("加载后调用数 = %d，期望 %d", len(got), len(want))
	}
	if diffs := DiffRequests(want, loaded.Snapshot()); len(diffs) != 0 {
		t.Fatalf("落盘往返改变了请求字节：\n%s", strings.Join(diffs, "\n"))
	}

	// 回放：同一个路径、同样的两次请求。
	replaySrv := httptest.NewServer(loaded.Handler())
	defer replaySrv.Close()

	var gotCalls []RecordedCall
	for _, body := range firstBodies {
		status, respBody := post(t, replaySrv.URL+path, body)
		if status != http.StatusOK {
			t.Fatalf("回放应返回录制的 200，实际 %d：%s", status, respBody)
		}
		c := callOf(t, http.MethodPost, path, body)
		c.Status = status
		c.Frames = splitSSEFrames(respBody)
		gotCalls = append(gotCalls, c)
	}

	// 1) 请求字节一致。
	if diffs := DiffRequests(loaded.Snapshot(), gotCalls); len(diffs) != 0 {
		t.Fatalf("回放前后请求字节不一致：\n%s", strings.Join(diffs, "\n"))
	}
	// 2) 响应帧一致（顺序敏感）。
	if diffs := DiffResponses(loaded.Snapshot(), gotCalls); len(diffs) != 0 {
		t.Fatalf("回放写出的帧与录制不一致：\n%s", strings.Join(diffs, "\n"))
	}
	// 3) 工具调用顺序：第一帧工具名必须是 memory_search，且出现在第二次调用之前。
	if !strings.Contains(gotCalls[0].Frames[1], `"name":"memory_search"`) {
		t.Fatalf("第一次回放不应改写工具名，实际帧: %q", gotCalls[0].Frames[1])
	}
}

// TestReplayExhaustedIsExplicitNotEmpty200 钉住"录制不完整"的可见性：
// 序号越界的请求必须拿到明确错误状态，绝不能是空 200。
//
// 空 200 会让"录制只录了一半"看起来像"模型回了空内容"——测试可能因此变绿
// （空回复也是合法回复），而真相是回放器缺数据。
func TestReplayExhaustedIsExplicitNotEmpty200(t *testing.T) {
	rec := NewRecording()
	if err := rec.Capture(RecordedCall{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   `{}`,
		Status: http.StatusOK,
		Frames: textFramesForReplay("只有一次"),
	}); err != nil {
		t.Fatalf("Capture 失败: %v", err)
	}

	srv := httptest.NewServer(rec.Handler())
	defer srv.Close()

	if status, _ := post(t, srv.URL+"/v1/messages", `{}`); status != http.StatusOK {
		t.Fatalf("第一次（录制内）应返回 200，实际 %d", status)
	}

	status, body := post(t, srv.URL+"/v1/messages", `{}`)
	if status == http.StatusOK {
		t.Fatalf("序号越界却返回了 200（空回复会伪装成正常收口）：body=%q", body)
	}
	if status != ReplayStatusExhausted {
		t.Fatalf("序号越界应返回 %d，实际 %d", ReplayStatusExhausted, status)
	}
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("回放错误响应不是合法 JSON: %v (body=%q)", err, body)
	}
	if e.Error.Type != "replay_exhausted" {
		t.Fatalf("错误类型 = %q，期望 replay_exhausted", e.Error.Type)
	}
	if !strings.Contains(e.Error.Message, "超出录制范围") {
		t.Fatalf("错误信息应说明序号越界，实际: %q", e.Error.Message)
	}
}

// TestReplayRejectsChangedRequestBytes 钉住：回放不是"按序号放录像"，
// 请求字节变了必须拒绝，否则测试会拿到上一次的回复而误以为一致。
//
// 这里用**语义等价但字节不同**的请求体（键序不同）：如果实现先 Unmarshal
// 再比较，这条会误判为一致。
func TestReplayRejectsChangedRequestBytes(t *testing.T) {
	recordedBody := `{"a":1,"b":2}`
	rec := NewRecording()
	if err := rec.Capture(RecordedCall{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   recordedBody,
		Status: http.StatusOK,
		Frames: textFramesForReplay("hi"),
	}); err != nil {
		t.Fatalf("Capture 失败: %v", err)
	}
	srv := httptest.NewServer(rec.Handler())
	defer srv.Close()

	status, body := post(t, srv.URL+"/v1/messages", `{"b":2,"a":1}`)
	if status != ReplayStatusMismatch {
		t.Fatalf("字节不同的请求应返回 %d，实际 %d：%s", ReplayStatusMismatch, status, body)
	}

	// 失败的回放不应消费录制：同一个字节正确的请求仍应拿到第一次的回复。
	// 否则"发错一次"会永久错位后续调用，差异被掩盖在别处。
	status, resp := post(t, srv.URL+"/v1/messages", recordedBody)
	if status != http.StatusOK {
		t.Fatalf("修正后的请求应命中录制，实际 %d：%s", status, resp)
	}
	if !strings.Contains(resp, "hi") {
		t.Fatalf("回放内容不对: %q", resp)
	}
}

// TestReplayCaptureRejectsEmptySuccess 钉住录制入口的防洞：
// 2xx 却没有帧，必须在录制时失败（见 checkCallShape 的理由）。
func TestReplayCaptureRejectsEmptySuccess(t *testing.T) {
	rec := NewRecording()
	err := rec.Capture(RecordedCall{Status: http.StatusOK})
	if err == nil {
		t.Fatal("2xx 且无帧的调用被录进去了：回放时会变成空 200")
	}
	if !strings.Contains(err.Error(), "没有任何帧") {
		t.Fatalf("错误信息没说明原因: %v", err)
	}
	if len(rec.Snapshot()) != 0 {
		t.Fatalf("失败的 Capture 不应入册，实际 %d 条", len(rec.Snapshot()))
	}
}

// TestReplayCaptureRejectsWrongBodySHA 钉住摘要校验：
// 调用方给了一个与请求体不符的摘要时，不能静默覆盖成自己算的值 ——
// 那会让"录制方以为的内容"与"实际录制的内容"分叉。
func TestReplayCaptureRejectsWrongBodySHA(t *testing.T) {
	rec := NewRecording()
	err := rec.Capture(RecordedCall{
		Body:    `{"x":1}`,
		BodySHA: "deadbeef",
		Status:  http.StatusOK,
		Frames:  textFramesForReplay("hi"),
	})
	if err == nil {
		t.Fatal("摘要与请求体不符却录制成功")
	}
	if !strings.Contains(err.Error(), "摘要不一致") {
		t.Fatalf("错误信息没说明摘要不一致: %v", err)
	}
}

// TestReplayLoadRejectsCorruptRecording 钉住 Load 的自洽校验：
// 被手改坏的录制（摘要与 body 不符）不能加载成功，否则回放层会拿着
// 一个错误摘要去拒绝所有正确请求，而报错点离录制文件很远。
func TestReplayLoadRejectsCorruptRecording(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "corrupt.json")
	raw := `{"version":1,"calls":[{"body":"{\"x\":1}","bodySha":"deadbeef","status":200,"frames":["data: {}\n\n"]}]}`
	if err := os.WriteFile(file, []byte(raw), 0o644); err != nil {
		t.Fatalf("写坏录制失败: %v", err)
	}
	if _, err := LoadRecording(file); err == nil {
		t.Fatal("bodySha 与 body 不符的录制被加载成功")
	}

	// 版本不认识时也必须失败，而不是尽力解析。
	file2 := filepath.Join(dir, "future.json")
	raw2 := `{"version":999,"calls":[]}`
	if err := os.WriteFile(file2, []byte(raw2), 0o644); err != nil {
		t.Fatalf("写未来版本录制失败: %v", err)
	}
	if _, err := LoadRecording(file2); err == nil {
		t.Fatal("不支持的录制版本被加载成功")
	}
}

// TestReplayDiffRequestsIsByteExact 钉住 DiffRequests 的判据本身：
// 语义相同、字节不同必须报差异；字节相同必须零差异。
//
// 这是 Q-H12「请求字节一致」的直接单测。若有人把实现改成比 Method 或先
// Unmarshal 再比，这条会红。
func TestReplayDiffRequestsIsByteExact(t *testing.T) {
	a := []RecordedCall{
		{Method: "POST", Path: "/v1/messages", Body: `{"a":1,"b":2}`},
	}
	b := []RecordedCall{
		{Method: "POST", Path: "/v1/messages", Body: `{"b":2,"a":1}`},
	}
	diffs := DiffRequests(a, b)
	if len(diffs) == 0 {
		t.Fatal("键序不同（语义相同）的请求体被当成一致：DiffRequests 没有做字节比较")
	}
	if !strings.Contains(strings.Join(diffs, "\n"), "请求体字节不同") {
		t.Fatalf("差异信息应指出请求体字节不同，实际: %v", diffs)
	}

	// 空白差异同样必须现形。
	c := []RecordedCall{
		{Method: "POST", Path: "/v1/messages", Body: `{"a": 1, "b": 2}`},
	}
	if diffs := DiffRequests(a, c); len(diffs) == 0 {
		t.Fatal("空白差异被忽略：DiffRequests 做了规范化")
	}

	// 字节相同时必须零差异，否则这条判据会把正确回放判死。
	if diffs := DiffRequests(a, a); len(diffs) != 0 {
		t.Fatalf("完全相同的请求出现差异: %v", diffs)
	}

	// Method/Path 也要比。
	d := []RecordedCall{{Method: "GET", Path: "/v1/messages", Body: `{"a":1,"b":2}`}}
	if diffs := DiffRequests(a, d); len(diffs) == 0 || !strings.Contains(strings.Join(diffs, "\n"), "Method 不同") {
		t.Fatalf("Method 差异未被检出: %v", diffs)
	}
	e := []RecordedCall{{Method: "POST", Path: "/v1/other", Body: `{"a":1,"b":2}`}}
	if diffs := DiffRequests(a, e); len(diffs) == 0 || !strings.Contains(strings.Join(diffs, "\n"), "Path 不同") {
		t.Fatalf("Path 差异未被检出: %v", diffs)
	}

	// 调用次数不同也必须报，而不是只比公共前缀。
	two := append(append([]RecordedCall{}, a...), a...)
	if diffs := DiffRequests(a, two); len(diffs) == 0 {
		t.Fatal("调用数不同未被检出：多出来的调用会被静默忽略")
	}
}

// TestReplayDiffResponsesIsFrameOrderSensitive 钉住响应侧的"工具调用顺序一致"：
// 同样的帧换个顺序必须报差异，帧原文有一个字节不同也必须报。
func TestReplayDiffResponsesIsFrameOrderSensitive(t *testing.T) {
	f1 := "data: {\"tool\":\"memory_search\"}\n\n"
	f2 := "data: {\"tool\":\"file_read\"}\n\n"

	want := []RecordedCall{{Status: 200, Frames: []string{f1, f2}}}
	same := []RecordedCall{{Status: 200, Frames: []string{f1, f2}}}
	if diffs := DiffResponses(want, same); len(diffs) != 0 {
		t.Fatalf("相同的帧序出现差异: %v", diffs)
	}

	swapped := []RecordedCall{{Status: 200, Frames: []string{f2, f1}}}
	if diffs := DiffResponses(want, swapped); len(diffs) == 0 {
		t.Fatal("帧顺序调换未被检出：工具调用顺序不一致会漏过")
	}

	changed := []RecordedCall{{Status: 200, Frames: []string{f1, "data: {\"tool\":\"file_read\"}\n\nX"}}}
	if diffs := DiffResponses(want, changed); len(diffs) == 0 {
		t.Fatal("帧字节不同未被检出")
	}
}

// ============================================================================
// Q-H9：旧配置 golden
// ============================================================================

// goldenOldConfigStable 是"旧配置"的稳定前缀：只有 persona / profile / system
// 三段，没有任何新能力（无临时上下文、无工具扩展、无断点外的稳定块）。
//
// 这些字符串是**契约的一部分**：改它们等于换了一个 golden 用例，而不是
// "修好了一个测试"。
func goldenOldConfigStable() []SegmentInput {
	return []SegmentInput{
		{ID: "persona", Version: "2026-01-01", Text: "你是墨七，一个陪伴型角色。"},
		{ID: "profile", Version: "u1", Text: "用户偏好简短回答。"},
		{ID: "system", Version: "v1", Text: "工具说明：只读记忆，不执行代码。"},
	}
}

// goldenOldConfigPrefixHash 钉住旧配置的 PrefixHash 字节值。
//
// **这个常量不是随便填的**：它钉的是哈希口径 —— 稳定段的 ID/Version/Text、
// 字段顺序、8 字节大端长度前缀、以及"先写段数"这条防碰撞编码。任何一项被
// 无意改动（把 Version 从 PrefixHash 里拿掉、换分隔符、改成 JSON 拼接、
// 调整字段顺序），这条测试立刻变红，而那种改动会让所有存量会话的缓存
// 一次性失效（R-H2 要防的正是这个）。
//
// 只有当**刻意**升级哈希口径（并接受一次全量缓存失效）时才允许改这个值；
// 改的时候必须在 PR 里说明失效范围和迁移。
const goldenOldConfigPrefixHash = "7e010f2bbc0560440116a4585013eb033e65b690d2363f21bcbccd72cb436c39"

// TestGoldenOldConfigPrefixHash 覆盖 Q-H9 第 1 条：旧配置的布局 hash 100% 稳定。
//
// 除了 PrefixHash，还钉住稳定段的 ID/Version/Hash/Bytes —— 只看聚合 hash 时，
// 段的边界漂移（例如把 system 拆成两段但总字节不变）可能被其他变化掩盖；
// 逐段断言让"布局形状也变了"这一点无处可藏。
func TestGoldenOldConfigPrefixHash(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	layout, err := b.Build(SegmentSpec{
		Stable: goldenOldConfigStable(),
		Dynamic: []SegmentInput{
			{ID: "turn", Text: "今天有点累"},
		},
		BreakpointCount: 1,
	})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}

	if layout.PrefixHash != goldenOldConfigPrefixHash {
		t.Fatalf("旧配置 PrefixHash 变了：\n  want %s\n  got  %s\n哈希口径被改动，或 golden 用例输入被改。若是有意升级口径，请同时更新常量并在 PR 说明缓存失效范围。",
			goldenOldConfigPrefixHash, layout.PrefixHash)
	}

	// 逐段钉住形状。
	wantIDs := []string{"persona", "profile", "system"}
	if len(layout.StableSegments) != len(wantIDs) {
		t.Fatalf("稳定段数量 = %d，期望 %d", len(layout.StableSegments), len(wantIDs))
	}
	for i, id := range wantIDs {
		seg := layout.StableSegments[i]
		if seg.ID != id {
			t.Fatalf("稳定段 #%d ID = %q，期望 %q（顺序也是契约：调换会改变请求字节）", i, seg.ID, id)
		}
		if seg.Hash != hashText(goldenOldConfigStable()[i].Text) {
			t.Fatalf("稳定段 %q 的段内 hash 变了：%q", id, seg.Hash)
		}
		if seg.Bytes != len(goldenOldConfigStable()[i].Text) {
			t.Fatalf("稳定段 %q 的字节数 = %d，期望 %d", id, seg.Bytes, len(goldenOldConfigStable()[i].Text))
		}
	}

	// 旧配置下只请求了 1 个断点：断点位置也必须稳定（落在动态段尾部）。
	if len(layout.Breakpoints) != 1 || layout.Breakpoints[0] != len(layout.DynamicSegments)-1 {
		t.Fatalf("断点位置变了: %v（动态段 %d 个）", layout.Breakpoints, len(layout.DynamicSegments))
	}
}

// TestGoldenPrefixHashStableAcrossTenBuilds 覆盖 Q-H9 第 2 条与"逐轮微调"：
// 同一组输入连续 Build 10 次，PrefixHash 必须每次相同。
//
// 用同一个 builder（跨轮状态被真实走过）而不是每次 new 一个：逐轮微调的
// 典型来源就是 builder 内部状态泄漏（prev 快照、观测回调、时间戳混入 hash）。
// 每次 new builder 的测试恰好会漏掉这一类。
func TestGoldenPrefixHashStableAcrossTenBuilds(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	spec := SegmentSpec{
		Stable:          goldenOldConfigStable(),
		Dynamic:         []SegmentInput{{ID: "turn", Text: "今天有点累"}},
		BreakpointCount: 1,
	}

	first := ""
	for i := 0; i < 10; i++ {
		layout, err := b.Build(spec)
		if err != nil {
			t.Fatalf("第 %d 次 Build 失败: %v", i+1, err)
		}
		if i == 0 {
			first = layout.PrefixHash
			if first != goldenOldConfigPrefixHash {
				t.Fatalf("第 1 次 Build 的 PrefixHash 已与 golden 常量不符：%s", first)
			}
			continue
		}
		if layout.PrefixHash != first {
			t.Fatalf("第 %d 次 Build 的 PrefixHash 变了：%s != %s（逐轮微调会让缓存全失效）",
				i+1, layout.PrefixHash, first)
		}
		if layout.InvalidationReason != "" {
			t.Fatalf("第 %d 次 Build 前缀未变却报了失效原因 %q", i+1, layout.InvalidationReason)
		}
	}
}

// TestGoldenDisabledFeaturesAddNothing 覆盖 Q-H9 的"新能力关闭时请求字节不变"：
//
// 旧配置的调用形态（无动态段、0 断点）必须产出与以前一样的形状：
// 动态段为空、断点为空、PrefixHash 仍等于打死的常量。
// 新能力若默认往稳定前缀里塞东西（例如一条空的工具扩展段），PrefixHash 会变，
// 这条会红。
func TestGoldenDisabledFeaturesAddNothing(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	layout, err := b.Build(SegmentSpec{
		Stable:          goldenOldConfigStable(),
		BreakpointCount: 0,
	})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if len(layout.DynamicSegments) != 0 {
		t.Fatalf("未提供动态段却产出了 %d 段：新能力在关闭时不得注入内容", len(layout.DynamicSegments))
	}
	if len(layout.Breakpoints) != 0 {
		t.Fatalf("BreakpointCount=0 却产出了断点 %v", layout.Breakpoints)
	}
	if layout.PrefixHash != goldenOldConfigPrefixHash {
		t.Fatalf("关闭新能力后 PrefixHash 变了：want %s, got %s（旧配置请求字节必须不变）",
			goldenOldConfigPrefixHash, layout.PrefixHash)
	}
}
