package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	id := fmt.Sprintf("%s-%06d", now.Format("20060102-150405.000000"), promptDebugSeq.Add(1))
	return &promptDebugCapture{
		dir:       dir,
		id:        id,
		startedAt: now,
		stream:    stream,
		request:   request,
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

	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(d.dir, d.id+".json"), data, 0o600)
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
