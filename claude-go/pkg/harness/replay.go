package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"unicode/utf8"
)

// 本文件实现 Q-H12（回放确定性）的录制/回放载体。
//
// # 为什么存原始字节而不是解码后的结构
//
// Q-H12 要求的是「请求字节一致」，不是「语义等价」。把请求体
// `json.Unmarshal` 成 map 再比较（或再 Marshal 回去）会把键序、空白、
// 数字字面量（1e3 vs 1000）全部抹平 —— 而这些恰恰是真实上游看到的差异。
// 抹平之后测试恒绿，线上缓存却每轮失效。因此 RecordedCall.Body 保存原始
// 字符串，DiffRequests 直接做字节比较。
//
// # 为什么回放时还要校验摘要
//
// 只按调用序号返回录制帧的回放器，在「第二次运行发出的请求已经变了」时仍然
// 会给出第一次的回复：测试通过，结论却是假的。所以 Handler 会把来包的
// Method/Path/请求体摘要与录制逐项比对，不一致就返回明确错误而不是旧回复。
//
// # 不覆盖的边界
//
// 录制里不含请求头与 URL query，回放也不重放它们。模型请求的身份由 method +
// path + body 决定；请求头（如鉴权）属于部署环境，不该被录制钉死。需要钉请求头
// 的测试应当直接断言 FakeModel 的 Captured.Header。

// RecordingVersion 是录制文件的格式版本。
//
// 加载时不兼容的版本会**报错**而不是尽力解析：静默忽略未知字段会让"新录制被旧
// 二进制读走一部分"看起来像正常回放，那比直接失败更难查。
const RecordingVersion = 1

// 回放失败时使用的非标准 5xx 状态码。
//
// 都落在 5xx：调用方（以及重试策略）会把它当失败，而不是像空 200 那样被当成
// "模型回了一个空回复"。三个码分开是为了让"录制不够长"、"请求变了"、"录制本身
// 有洞"三件事在日志里一眼可辨。
const (
	// ReplayStatusIncomplete 表示录制本身不完整（例如 2xx 却没有帧）。
	ReplayStatusIncomplete = 597
	// ReplayStatusMismatch 表示回放请求与录制不一致（method/path/body 字节）。
	ReplayStatusMismatch = 598
	// ReplayStatusExhausted 表示录制里的调用数不够，序号已越界。
	ReplayStatusExhausted = 599
)

// replayMaxBodyBytes 是回放读入请求体的上限。
//
// 超限时明确报错而不是截断：截断后的摘要必然不匹配，但报"摘要不匹配"会让人
// 去查序列化，而真正的原因是请求体过大。
const replayMaxBodyBytes = 32 << 20

// Recording 是一段可回放的模型交互录制。
//
// 字段导出是为了让录制文件本身可读、可手改（golden 场景）；并发安全由内部锁
// 保证 Capture 与 Handler 可以同时被调用。直接改 Calls 切片不受锁保护 ——
// 需要读取时用 Snapshot。
type Recording struct {
	Version int            `json:"version"`
	Calls   []RecordedCall `json:"calls"`

	mu     sync.Mutex
	cursor int // Handler 已回放的调用数
}

// RecordedCall 是一次被录制的上游调用。
//
// Body 是请求体的**原始字节**（字符串在 Go 里就是字节序列，不做任何解码）。
// Status/Frames 是这次调用的响应：Frames 是 SSE 帧原文，按序写出，不重新编码 ——
// 重新编码会改变 JSON 键序，回放就不再是逐字节回放了。
type RecordedCall struct {
	Method  string   `json:"method,omitempty"`
	Path    string   `json:"path,omitempty"`
	Body    string   `json:"body"`
	BodySHA string   `json:"bodySha"`
	Status  int      `json:"status"`
	Frames  []string `json:"frames,omitempty"`
}

// NewRecording 构造一段空录制。
func NewRecording() *Recording {
	return &Recording{Version: RecordingVersion}
}

// Capture 追加一次调用，并计算 BodySHA。
//
// 返回 error 表示这次调用**没有被记录**，调用方不能忽略：
//   - status 不在 [100, 599]；
//   - 调用方显式给了 BodySHA 但与 Body 实算不符（说明拿错了请求体或录错了摘要，
//     静默覆盖会让"录制内容"与"录制方以为的内容"分叉）；
//   - 2xx 却没有任何帧 —— 这是最危险的一种洞：回放时会表现成"模型回了空"，
//     而真相是录制漏了响应。宁可在录制时失败。
//
// Frames 会被复制，调用方之后复用/修改自己的切片不会影响录制。
func (r *Recording) Capture(c RecordedCall) error {
	if r == nil {
		return fmt.Errorf("harness: 不能向 nil Recording 录制")
	}

	sum := hashText(c.Body)
	if c.BodySHA != "" && c.BodySHA != sum {
		return fmt.Errorf(
			"harness: 录制的请求体与声明的摘要不一致（body %d 字节）：声明 %s，实算 %s",
			len(c.Body), c.BodySHA, sum)
	}
	c.BodySHA = sum
	c.Frames = append([]string(nil), c.Frames...)

	if err := checkCallShape(-1, c); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Version == 0 {
		r.Version = RecordingVersion
	}
	if r.Version != RecordingVersion {
		return fmt.Errorf("harness: 录制版本 %d 与本程序支持的 %d 不一致", r.Version, RecordingVersion)
	}
	r.Calls = append(r.Calls, c)
	return nil
}

// Snapshot 返回当前录制的深拷贝。
//
// 用拷贝而不是直接暴露 Calls：Handler 与 Capture 都在改内部状态，调用方拿到
// 切片引用再读就是数据竞争；而且 Frames 是切片，浅拷贝仍会共享底层数组。
func (r *Recording) Snapshot() []RecordedCall {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecordedCall, len(r.Calls))
	for i, c := range r.Calls {
		out[i] = c
		out[i].Frames = append([]string(nil), c.Frames...)
	}
	return out
}

// Handler 返回回放用的处理器：按调用序号返回录制的响应。
//
// 每一层失败都返回明确的 5xx（见 ReplayStatus*），**绝不**静默回空 200 ——
// 那会把"录制不完整/请求变了"伪装成"模型回了空内容"，是最难查的一类假绿。
//
// 并发说明：序号在锁内自增，但并发请求拿到哪个录制是调度决定的。回放确定性
// 的前提是调用方按序发请求；并发回放本身不承诺顺序。
func (r *Recording) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r == nil {
			writeReplayError(w, ReplayStatusExhausted, "replay_nil_recording",
				"回放器没有绑定任何录制")
			return
		}

		body, err := io.ReadAll(io.LimitReader(req.Body, replayMaxBodyBytes+1))
		if err != nil {
			writeReplayError(w, http.StatusBadRequest, "replay_read_body",
				fmt.Sprintf("读取请求体失败: %v", err))
			return
		}
		if len(body) > replayMaxBodyBytes {
			writeReplayError(w, http.StatusRequestEntityTooLarge, "replay_body_too_large",
				fmt.Sprintf("请求体超过回放上限 %d 字节（未截断，避免摘要误判）", replayMaxBodyBytes))
			return
		}

		gotSHA := hashText(string(body))

		// 比对成功才推进序号。失败时不推进是刻意的：一个发错内容的请求不应顺带
		// "消费"掉一条录制，否则调用方修正请求重试时会跳到下一条，差异被错位掩盖。
		// 另外，哈希与静态字段比对都在锁内完成 —— 两个并发请求不能各自校验同一条
		// 录制后都推进游标。
		r.mu.Lock()
		idx := r.cursor
		total := len(r.Calls)
		if idx >= total {
			r.mu.Unlock()
			writeReplayError(w, ReplayStatusExhausted, "replay_exhausted",
				fmt.Sprintf("回放请求序号 %d 超出录制范围：录制只有 %d 次调用。多出来的调用不会被自动编造", idx, total))
			return
		}
		call := r.Calls[idx]
		var mismatch string
		switch {
		case call.Method != "" && req.Method != call.Method:
			mismatch = fmt.Sprintf("调用 #%d 的 Method 与录制不符：want %q, got %q", idx, call.Method, req.Method)
		case call.Path != "" && req.URL.Path != call.Path:
			mismatch = fmt.Sprintf("调用 #%d 的 Path 与录制不符：want %q, got %q（query 不参与录制）", idx, call.Path, req.URL.Path)
		case gotSHA != call.BodySHA:
			mismatch = fmt.Sprintf("调用 #%d 的请求体与录制不是同一串字节：want sha %s（%d 字节），got sha %s（%d 字节）。语义相同但键序/空白不同也会走到这里，这正是 Q-H12 要抓的",
				idx, call.BodySHA, len(call.Body), gotSHA, len(body))
		}
		if mismatch == "" {
			r.cursor++
		}
		r.mu.Unlock()

		if mismatch != "" {
			writeReplayError(w, ReplayStatusMismatch, "replay_request_mismatch", mismatch)
			return
		}
		if call.Status >= 200 && call.Status < 300 && len(call.Frames) == 0 {
			writeReplayError(w, ReplayStatusIncomplete, "replay_incomplete",
				fmt.Sprintf("调用 #%d 录制为 %d 但没有任何帧：录制不完整，不拿空 200 冒充模型回复", idx, call.Status))
			return
		}

		if call.Status < 200 || call.Status >= 300 {
			// 录制不保存错误响应体（上游错误页没有回放价值），只还原状态码。
			writeReplayError(w, call.Status, "replay_recorded_error",
				fmt.Sprintf("回放调用 #%d 录制下来的上游错误状态 %d", idx, call.Status))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(call.Status)
		for _, fr := range call.Frames {
			if _, err := io.WriteString(w, fr); err != nil {
				return
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	})
}

// Save 把录制原子地写到 path。
//
// 先写同目录临时文件再 rename：测试或 CI 被中断时，目标文件要么是完整的旧版本，
// 要么是完整的新版本，不会留下半个 JSON 让下一次 Load 报"语法错误"（那种报错
// 会掩盖真正的录制内容问题）。
//
// 保存前会校验一遍：一段自己都通不过 Load 的录制不该被写出去。
func (r *Recording) Save(path string) error {
	if r == nil {
		return fmt.Errorf("harness: 不能保存 nil Recording")
	}
	r.mu.Lock()
	snap := &Recording{Version: r.Version, Calls: make([]RecordedCall, len(r.Calls))}
	for i, c := range r.Calls {
		snap.Calls[i] = c
		snap.Calls[i].Frames = append([]string(nil), c.Frames...)
	}
	r.mu.Unlock()

	if snap.Version == 0 {
		snap.Version = RecordingVersion
	}
	if err := snap.validate(); err != nil {
		return fmt.Errorf("harness: 拒绝保存不完整的录制 %s: %w", path, err)
	}

	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("harness: 序列化录制失败: %w", err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".recording-*.tmp")
	if err != nil {
		return fmt.Errorf("harness: 在 %s 创建临时录制失败: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("harness: 写临时录制失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("harness: 同步临时录制失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("harness: 关闭临时录制失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("harness: 落盘录制 %s 失败: %w", path, err)
	}
	tmpName = ""
	return nil
}

// LoadRecording 读取并校验一段录制。
//
// 校验包括：版本、每次调用的状态码范围、BodySHA 与 Body 是否一致、2xx 是否真的
// 有帧。任何一项不过都直接失败 —— 一段坏录制在回放时只会以"模型行为异常"的
// 面貌出现，很难回溯到录制文件本身。
func LoadRecording(path string) (*Recording, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("harness: 读取录制 %s 失败: %w", path, err)
	}

	var rec Recording
	dec := json.NewDecoder(bytes.NewReader(b))
	// 不认识的字段直接报错：旧二进制读新录制时静默丢字段，会让"回放"少了语义
	// 却依然"成功"。宁可失败并提示升级版本。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("harness: 解析录制 %s 失败: %w", path, err)
	}
	var trailing struct{}
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("harness: 录制 %s 在第一个 JSON 对象之后还有多余内容", path)
	}
	if err := rec.validate(); err != nil {
		return nil, fmt.Errorf("harness: 录制 %s 校验失败: %w", path, err)
	}
	rec.cursor = 0
	return &rec, nil
}

// validate 检查整段录制是否自洽。idx 从 0 起，只用于报错定位。
func (r *Recording) validate() error {
	if r.Version != RecordingVersion {
		return fmt.Errorf("录制版本 %d 不受支持（本程序为 %d）", r.Version, RecordingVersion)
	}
	for i := range r.Calls {
		if err := checkCallShape(i, r.Calls[i]); err != nil {
			return err
		}
		if r.Calls[i].BodySHA == "" {
			return fmt.Errorf("调用 #%d 缺少 bodySha：没有摘要就无法证明回放请求是同一串字节", i)
		}
		if got := hashText(r.Calls[i].Body); got != r.Calls[i].BodySHA {
			return fmt.Errorf("调用 #%d 的 bodySha 与 body 不一致：声明 %s，实算 %s",
				i, r.Calls[i].BodySHA, got)
		}
	}
	return nil
}

// checkCallShape 校验与摘要无关的形状约束。i 为 -1 表示尚未入册（Capture 阶段）。
func checkCallShape(i int, c RecordedCall) error {
	where := "待录制调用"
	if i >= 0 {
		where = fmt.Sprintf("调用 #%d", i)
	}
	if c.Status < 100 || c.Status > 599 {
		return fmt.Errorf("%s 的 status=%d 不在 [100,599]：回放一个非法状态码等于编造响应", where, c.Status)
	}
	if c.Status >= 200 && c.Status < 300 && len(c.Frames) == 0 {
		return fmt.Errorf("%s 录制为 %d 却没有任何帧：这会变成「模型回了空」的假绿，拒绝录制", where, c.Status)
	}
	for j, fr := range c.Frames {
		if fr == "" {
			return fmt.Errorf("%s 的第 %d 个帧是空串：空帧无法表达任何事件，只能是录制错误", where, j)
		}
	}
	return nil
}

// DiffRequests 比较两组调用，返回可读差异；空切片表示一致。
//
// 只比 Method/Path/Body，且 Body 是**原始字节**比较：不 json.Unmarshal、不重新
// 编码。键序、空白、数字字面量的差异都必须现形 —— 那正是 Q-H12「请求字节一致」
// 的字面含义。
//
// 不比 Status/Frames：它们是响应侧事实，由回放的往返测试（响应字节比对）覆盖；
// 放进这里会让"请求是否一致"的判据掺进响应差异。
func DiffRequests(want, got []RecordedCall) []string {
	var diffs []string
	if len(want) != len(got) {
		diffs = append(diffs, fmt.Sprintf(
			"调用数不同：want %d 次，got %d 次（多出的调用不会被自动忽略，少的调用无法补齐）",
			len(want), len(got)))
	}

	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		w, g := want[i], got[i]
		if w.Method != g.Method {
			diffs = append(diffs, fmt.Sprintf("调用 #%d Method 不同：want %q，got %q", i, w.Method, g.Method))
		}
		if w.Path != g.Path {
			diffs = append(diffs, fmt.Sprintf("调用 #%d Path 不同：want %q，got %q", i, w.Path, g.Path))
		}
		if w.Body != g.Body {
			at := firstByteDiff(w.Body, g.Body)
			diffs = append(diffs, fmt.Sprintf(
				"调用 #%d 请求体字节不同：want %d 字节，got %d 字节，首个差异在字节 %d\n  want: %s\n  got:  %s",
				i, len(w.Body), len(g.Body), at,
				bodyExcerpt(w.Body, at), bodyExcerpt(g.Body, at)))
		}
	}
	return diffs
}

// DiffResponses 比较两组调用的响应侧 replay 事实：Status 与 Frames 原文。
//
// 与 DiffRequests 分开是刻意的：请求侧钉的是"我们发出去的字节"，响应侧钉的是
// "回放出去的字节"。合并成一个函数后，一次失败无法区分是请求变了还是回放变了，
// 而这两者的修法完全不同。
//
// Frames 按序、逐字节比较：顺序不同就是不同 —— Q-H12 的「工具调用顺序一致」
// 要求回放帧序与录制帧序完全相同，对帧做排序/去重都会把顺序错误洗白。
func DiffResponses(want, got []RecordedCall) []string {
	var diffs []string
	if len(want) != len(got) {
		diffs = append(diffs, fmt.Sprintf("调用数不同：want %d 次，got %d 次", len(want), len(got)))
	}
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		w, g := want[i], got[i]
		if w.Status != g.Status {
			diffs = append(diffs, fmt.Sprintf("调用 #%d Status 不同：want %d，got %d", i, w.Status, g.Status))
		}
		if len(w.Frames) != len(g.Frames) {
			diffs = append(diffs, fmt.Sprintf("调用 #%d 帧数不同：want %d，got %d", i, len(w.Frames), len(g.Frames)))
		}
		m := len(w.Frames)
		if len(g.Frames) < m {
			m = len(g.Frames)
		}
		for j := 0; j < m; j++ {
			if w.Frames[j] != g.Frames[j] {
				diffs = append(diffs, fmt.Sprintf("调用 #%d 的第 %d 帧不同：\n  want: %q\n  got:  %q",
					i, j, w.Frames[j], g.Frames[j]))
			}
		}
	}
	return diffs
}

// firstByteDiff 返回两串第一个不同字节的下标；一方是另一方前缀时返回较短的长度。
func firstByteDiff(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// bodyExcerpt 取 at 附近的片段并加引号，避免把整个请求体倒进测试日志。
//
// 边界退到 rune 起点：中文请求体被从半个字符处切开时，%q 会把残字节显示成
// \xNN，读起来像"编码坏了"，而真正的问题是这里切错了位置。
func bodyExcerpt(s string, at int) string {
	const window = 40
	start := at - window
	if start < 0 {
		start = 0
	}
	for start > 0 && start < len(s) && !utf8.RuneStart(s[start]) {
		start--
	}
	end := at + window
	if end > len(s) {
		end = len(s)
	}
	for end < len(s) && !utf8.RuneStart(s[end]) {
		end++
	}

	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(s) {
		suffix = "…"
	}
	return prefix + strconv.Quote(s[start:end]) + suffix
}

// writeReplayError 写一个可读的回放错误响应。
//
// 用 JSON 而不是纯文本：调用方多半按 SSE/JSON 解析，纯文本会先撞上解析错误，
// 真正的回放诊断信息（"序号越界""请求体不同"）就淹没在"unexpected token"里了。
func writeReplayError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Replay-Error", kind)
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    kind,
			"message": msg,
		},
	})
	_, _ = w.Write(b)
}
