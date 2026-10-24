package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var promptDebugSeq atomic.Int64

type promptDebugCapture struct {
	mu        sync.Mutex
	dir       string
	id        string
	startedAt time.Time
	stream    bool
	request   string
	sampled   bool // 命中采样的正常请求才落盘; 失败请求无论是否采样都强制落盘

	attempts []promptDebugAttempt
	events   []json.RawMessage
	response *promptDebugResponse
}

type promptDebugAttempt struct {
	Attempt int             `json:"attempt"`
	URL     string          `json:"url"`
	Model   string          `json:"model"`
	Body    json.RawMessage `json:"body"`
}

type promptDebugResponse struct {
	HTTPStatus int    `json:"http_status,omitempty"`
	Body       string `json:"body,omitempty"`
}

type promptDebugFile struct {
	ID          string                 `json:"id"`
	Timestamp   string                 `json:"timestamp"`
	DurationMS  int64                  `json:"duration_ms"`
	Stream      bool                   `json:"stream"`
	Request     string                 `json:"request"`
	Model       string                 `json:"model"`
	BaseURL     string                 `json:"base_url"`
	Source      string                 `json:"source,omitempty"`
	Purpose     string                 `json:"purpose,omitempty"`
	Workflow    string                 `json:"workflow,omitempty"`
	Role        string                 `json:"role,omitempty"`
	Status      string                 `json:"status,omitempty"`
	StopReason  string                 `json:"stop_reason,omitempty"`
	Error       string                 `json:"error,omitempty"`
	Usage       map[string]int         `json:"usage,omitempty"`
	PromptParts PromptComponentMetrics `json:"prompt_components"`
	Attempts    []promptDebugAttempt   `json:"attempts,omitempty"`
	Events      []json.RawMessage      `json:"stream_events,omitempty"`
	Response    *promptDebugResponse   `json:"response,omitempty"`
}

// newPromptDebugCapture 为一次 LLM 调用开一个观测记录; 未启用时返回 nil (后续方法全部 nil-safe)。
//
// 采样用全局自增序号 seq 对 10000 取模判定, 而不是随机数: 序号单调递增,
// 所以采样比例在任意长度的窗口内都是**确定性均匀**的, 便于复现"第 N 次调用是否落盘"。
func (c *Client) newPromptDebugCapture(request string, stream bool) *promptDebugCapture {
	if c == nil || !c.PromptDebugEnabled {
		return nil
	}
	dir := strings.TrimSpace(c.PromptDebugDir)
	if dir == "" {
		dir = "prompt-debug"
	}
	now := time.Now()
	seq := promptDebugSeq.Add(1)
	// 采样判定: 即使未命中采样也创建 capture, 以便失败请求在 finish() 时强制落盘 (永不漏掉失败样本)。
	sampled := true
	if c.PromptDebugSampleRate > 0 && c.PromptDebugSampleRate < 1 {
		threshold := int64(c.PromptDebugSampleRate * 10000)
		if threshold <= 0 || seq%10000 >= threshold {
			sampled = false
		}
	}
	id := fmt.Sprintf("%s-%06d", now.Format("20060102-150405.000000"), seq)
	return &promptDebugCapture{
		dir:       dir,
		id:        id,
		startedAt: now,
		stream:    stream,
		request:   request,
		sampled:   sampled,
	}
}

// addAttempt 记录一次上游请求尝试 (重试时会有多条), body 先深拷贝再入列。
func (d *promptDebugCapture) addAttempt(attempt int, url, model string, body []byte) {
	if d == nil {
		return
	}
	bodyCopy := append([]byte(nil), body...)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.attempts = append(d.attempts, promptDebugAttempt{
		Attempt: attempt,
		URL:     url,
		Model:   model,
		Body:    json.RawMessage(bodyCopy),
	})
}

// addStreamEvent 追加一条原始 SSE data 帧 (空串忽略)。
func (d *promptDebugCapture) addStreamEvent(data string) {
	if d == nil || data == "" {
		return
	}
	dataCopy := append([]byte(nil), data...)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, json.RawMessage(dataCopy))
}

// setResponse 记录非流式响应的 HTTP 状态码与响应体。
func (d *promptDebugCapture) setResponse(status int, body []byte) {
	if d == nil {
		return
	}
	bodyCopy := append([]byte(nil), body...)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.response = &promptDebugResponse{
		HTTPStatus: status,
		Body:       string(bodyCopy),
	}
}

// finish 收尾: 组装记录、按需脱敏、落盘一个 <id>.json, 然后触发保留策略清理。
//
// 算法解读：
//
//	原理：把"要不要落盘"拆成**两个独立判据的析取** —— 命中采样 (sampled) 或这是一次
//		失败 (isFailure)。失败样本永远落盘, 与采样率无关。
//	关键不变量：**失败请求不得被采样率丢掉**。采样是有损的, 而失败样本正是最需要被
//		留存的那一小撮; 若把判据写成 "sampled && ..." 这类合取, 排查线上问题时
//		会发现在 1% 采样下几乎没有失败记录可看 —— 这个析取方向是刻意的。
//	代价与边界：先加锁拷出 attempts/events/response 再解锁, 落盘 (Marshal + WriteFile
//		+ 目录清理) 全程不持锁, 所以写盘慢不会阻塞并发的 addStreamEvent。代价是
//		finish 之后到达的事件会被丢弃 (capture 已收尾, 不该再有事件)。
//	设计权衡：落盘错误全部静默吞掉 (MkdirAll/Marshal/WriteFile 的 err 都不返回) ——
//		观测通道**绝不能**因为磁盘问题让主调用链失败。
func (d *promptDebugCapture) finish(client *Client, rec LLMCallRecord, errMsg string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	attempts := append([]promptDebugAttempt(nil), d.attempts...)
	events := append([]json.RawMessage(nil), d.events...)
	response := d.response
	d.mu.Unlock()

	if errMsg == "" {
		errMsg = rec.ErrorMessage
	}
	// 失败请求 (有错误 / 状态 error / HTTP>=400) 无论是否命中采样都落盘; 未采样的成功请求跳过。
	isFailure := errMsg != "" || rec.Status == "error" ||
		(response != nil && response.HTTPStatus >= 400)
	if !d.sampled && !isFailure {
		return
	}
	usage := map[string]int{}
	if rec.InputTokens > 0 {
		usage["input_tokens"] = rec.InputTokens
	}
	if rec.OutputTokens > 0 {
		usage["output_tokens"] = rec.OutputTokens
	}
	if rec.CacheReadTokens > 0 {
		usage["cache_read_tokens"] = rec.CacheReadTokens
	}
	if rec.CacheCreationTokens > 0 {
		usage["cache_creation_tokens"] = rec.CacheCreationTokens
	}
	if rec.TotalTokens > 0 {
		usage["total_tokens"] = rec.TotalTokens
	}
	if len(usage) == 0 {
		usage = nil
	}

	out := promptDebugFile{
		ID:          d.id,
		Timestamp:   d.startedAt.Format(time.RFC3339Nano),
		DurationMS:  time.Since(d.startedAt).Milliseconds(),
		Stream:      d.stream,
		Request:     d.request,
		Model:       firstNonEmpty(rec.Model, clientModel(client)),
		BaseURL:     firstNonEmpty(rec.BaseURL, clientBaseURL(client)),
		Source:      rec.Source,
		Purpose:     rec.Purpose,
		Workflow:    rec.Workflow,
		Role:        rec.Role,
		Status:      rec.Status,
		StopReason:  rec.StopReason,
		Error:       errMsg,
		Usage:       usage,
		PromptParts: rec.PromptComponents,
		Attempts:    attempts,
		Events:      events,
		Response:    response,
	}
	if client != nil && client.PromptDebugRedact {
		redactPromptDebugFile(&out)
	}

	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(d.dir, d.id+".json"), data, 0o600); err == nil && client != nil {
		enforcePromptDebugRetention(d.dir, client.PromptDebugMaxFiles, client.PromptDebugMaxBytes)
	}
}

// redactPromptDebugFile 就地脱敏整份记录: BaseURL、每次尝试的 URL/Body、全部流事件、响应体。
//
// 注意：脱敏在 **Marshal 之前、finish 内部**执行, 所以只有走 finish 落盘的路径被覆盖;
// 直接把 capture 的内存字段喂给别的出口 (日志/API) 不会经过这里。
func redactPromptDebugFile(out *promptDebugFile) {
	if out == nil {
		return
	}
	out.BaseURL = redactSecretsString(out.BaseURL)
	for i := range out.Attempts {
		out.Attempts[i].URL = redactSecretsString(out.Attempts[i].URL)
		out.Attempts[i].Body = redactRawMessage(out.Attempts[i].Body)
	}
	for i := range out.Events {
		out.Events[i] = redactRawMessage(out.Events[i])
	}
	if out.Response != nil {
		out.Response.Body = redactSecretsString(out.Response.Body)
	}
}

var (
	jsonSecretPattern = regexp.MustCompile(`(?i)("?(?:api[_-]?key|authorization|x-api-key|app[_-]?secret|access[_-]?token|refresh[_-]?token|token|secret)"?\s*:\s*")([^"]+)(")`)
	bearerPattern     = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=-]+`)
	skPattern         = regexp.MustCompile(`\bsk-[A-Za-z0-9._-]{12,}\b`)
)

// redactRawMessage 对一段原始 JSON 做**文本级**脱敏; 空值原样返回。
//
// 注意：它不做 JSON 解析, 直接当字符串替换 —— 好处是即使这段 JSON 已被截断/非法也能生效,
// 代价是替换后的内容可能不再是合法 JSON (例如转义序列被改写)。
func redactRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	return json.RawMessage(redactSecretsString(string(raw)))
}

// redactSecretsString 依次套用三条正则抹掉密钥: JSON 字段值、Bearer 头、sk- 前缀 token。
//
// 顺序有依赖：JSON 字段那条先跑 (它靠引号定界最精确), Bearer 与 sk- 两条后跑兜住
// 非 JSON 形态的裸文本。三条都只替换**值**, 保留字段名, 便于事后判断"这里本该有密钥"。
func redactSecretsString(s string) string {
	if s == "" {
		return s
	}
	s = jsonSecretPattern.ReplaceAllString(s, `${1}[REDACTED]${3}`)
	s = bearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	s = skPattern.ReplaceAllString(s, "sk-[REDACTED]")
	return s
}

// enforcePromptDebugRetention 按"文件数"与"总字节"两个上限淘汰最旧记录; 两者都为 0 表示不限制。
//
// 算法解读：
//
//	原理：先按 mtime 升序把 .json 记录排好, 然后**依次**用两道闸门从队首 (最旧) 删除,
//		先削文件数, 再削总字节。
//	关键不变量：淘汰顺序必须与排序顺序一致 —— 只删队首。否则删掉新文件、留下旧文件,
//		观测目录就退化成"越查越旧"。两道闸门是**串行**而非并行: 第一道删完要重新
//		累计 total, 第二道接着在剩下的集合上判。
//	代价与边界：整目录读一遍 + 排序, O(n log n); 只在**每次成功落盘后**调用, 所以
//		并发写会各自跑一遍清理, 重复删除由 os.Remove 的失败被忽略兜住。目录不存在
//		或不可读时直接返回, 不报错。
//	设计权衡：不做后台定时清理 —— 那样需要在别处维护生命周期; 挂在写入路径上虽然
//		把成本摊给写方, 但保证了"目录大小恒有上界"这一条不依赖任何外部调度。
func enforcePromptDebugRetention(dir string, maxFiles int, maxBytes int64) {
	if maxFiles <= 0 && maxBytes <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		path string
		mod  time.Time
		size int64
	}
	var files []fileInfo
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{
			path: filepath.Join(dir, entry.Name()),
			mod:  info.ModTime(),
			size: info.Size(),
		})
		total += info.Size()
	}
	// 只删队首 (最旧): 顺序与排序一致, 否则会删掉新文件而留下旧的, 目录越查越旧。
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	// 两道闸门**串行**而非并排: 第一道按文件数削完, 第二道再在剩下的集合上按字节削。
	for len(files) > 0 && maxFiles > 0 && len(files) > maxFiles {
		_ = os.Remove(files[0].path)
		total -= files[0].size
		files = files[1:]
	}
	for len(files) > 0 && maxBytes > 0 && total > maxBytes {
		_ = os.Remove(files[0].path)
		total -= files[0].size
		files = files[1:]
	}
}

// firstNonEmpty 返回第一个非空字符串; 全空时返回空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// clientModel 取 client 上的模型名; nil client 返回空串 (不 panic)。
func clientModel(c *Client) string {
	if c == nil {
		return ""
	}
	return c.Model
}

// clientBaseURL 取 client 上的 BaseURL; nil client 返回空串 (不 panic)。
func clientBaseURL(c *Client) string {
	if c == nil {
		return ""
	}
	return c.BaseURL
}
