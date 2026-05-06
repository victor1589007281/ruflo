package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

type OutputOptions struct {
	ID              string
	LogRoot         string
	MaxTotalBytes   int64
	MaxPreviewBytes int
	MaxLogBytes     int64
	OnLimit         func()
}

type OutputResult struct {
	StdoutPreview string
	StderrPreview string
	Combined      string
	LogDir        string
	Truncated     bool
	TotalBytes    int64
}

type OutputLimiter struct {
	id       string
	logDir   string
	maxTotal int64
	total    atomic.Int64
	limited  atomic.Bool
	once     sync.Once
	onLimit  func()

	stdout *limitedStream
	stderr *limitedStream
}

func NewOutputLimiter(opts OutputOptions) (*OutputLimiter, error) {
	if opts.ID == "" {
		opts.ID = newID()
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = 4 * 1024 * 1024
	}
	if opts.MaxPreviewBytes <= 0 {
		opts.MaxPreviewBytes = 128 * 1024
	}
	if opts.MaxLogBytes <= 0 {
		opts.MaxLogBytes = 16 * 1024 * 1024
	}
	logRoot := opts.LogRoot
	if logRoot == "" {
		logRoot = filepath.Join(".", ".claude-go", "sandboxes")
	}
	logDir := filepath.Join(logRoot, opts.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	l := &OutputLimiter{
		id:       opts.ID,
		logDir:   logDir,
		maxTotal: opts.MaxTotalBytes,
		onLimit:  opts.OnLimit,
	}
	stdoutFile, err := os.OpenFile(filepath.Join(logDir, "stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	stderrFile, err := os.OpenFile(filepath.Join(logDir, "stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		_ = stdoutFile.Close()
		return nil, err
	}
	l.stdout = &limitedStream{name: "stdout", parent: l, file: stdoutFile, maxPreview: opts.MaxPreviewBytes, maxLog: opts.MaxLogBytes}
	l.stderr = &limitedStream{name: "stderr", parent: l, file: stderrFile, maxPreview: opts.MaxPreviewBytes, maxLog: opts.MaxLogBytes}
	return l, nil
}

func (l *OutputLimiter) Stdout() *limitedStream { return l.stdout }

func (l *OutputLimiter) Stderr() *limitedStream { return l.stderr }

func (l *OutputLimiter) Close() error {
	var first error
	if l.stdout != nil && l.stdout.file != nil {
		if err := l.stdout.file.Close(); err != nil {
			first = err
		}
	}
	if l.stderr != nil && l.stderr.file != nil {
		if err := l.stderr.file.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (l *OutputLimiter) Result() OutputResult {
	stdout := l.stdout.previewString()
	stderr := l.stderr.previewString()
	combined := stdout
	if stderr != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += stderr
	}
	return OutputResult{
		StdoutPreview: stdout,
		StderrPreview: stderr,
		Combined:      combined,
		LogDir:        l.logDir,
		Truncated:     l.limited.Load() || l.stdout.truncated.Load() || l.stderr.truncated.Load(),
		TotalBytes:    l.total.Load(),
	}
}

func (l *OutputLimiter) addBytes(n int64) {
	if n <= 0 {
		return
	}
	total := l.total.Add(n)
	if total > l.maxTotal {
		l.triggerLimit()
	}
}

func (l *OutputLimiter) triggerLimit() {
	l.limited.Store(true)
	l.once.Do(func() {
		if l.onLimit != nil {
			l.onLimit()
		}
	})
}

type limitedStream struct {
	name       string
	parent     *OutputLimiter
	file       *os.File
	maxPreview int
	maxLog     int64
	writtenLog int64
	mu         sync.Mutex
	preview    bytes.Buffer
	truncated  atomic.Bool
}

func (s *limitedStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.parent.addBytes(int64(len(p)))

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.preview.Len() < s.maxPreview {
		remain := s.maxPreview - s.preview.Len()
		if remain > len(p) {
			remain = len(p)
		}
		_, _ = s.preview.Write(p[:remain])
		if remain < len(p) {
			s.truncated.Store(true)
		}
	} else {
		s.truncated.Store(true)
	}

	if s.file != nil && s.writtenLog < s.maxLog {
		remain := s.maxLog - s.writtenLog
		chunk := p
		if int64(len(chunk)) > remain {
			chunk = chunk[:remain]
			s.truncated.Store(true)
		}
		if len(chunk) > 0 {
			if n, err := s.file.Write(chunk); err == nil {
				s.writtenLog += int64(n)
			}
		}
	} else {
		s.truncated.Store(true)
	}
	return len(p), nil
}

func (s *limitedStream) previewString() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preview.String()
}
