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

func (d *promptDebugCapture) addStreamEvent(data string) {
	if d == nil || data == "" {
		return
	}
	dataCopy := append([]byte(nil), data...)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, json.RawMessage(dataCopy))
}

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

func redactRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	return json.RawMessage(redactSecretsString(string(raw)))
}

func redactSecretsString(s string) string {
	if s == "" {
		return s
	}
	s = jsonSecretPattern.ReplaceAllString(s, `${1}[REDACTED]${3}`)
	s = bearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	s = skPattern.ReplaceAllString(s, "sk-[REDACTED]")
	return s
}

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
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func clientModel(c *Client) string {
	if c == nil {
		return ""
	}
	return c.Model
}

func clientBaseURL(c *Client) string {
	if c == nil {
		return ""
	}
	return c.BaseURL
}
