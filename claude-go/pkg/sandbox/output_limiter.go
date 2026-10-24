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

// NewOutputLimiter 创建输出限流器: 补齐零值选项 (总量 4MiB / 预览 128KiB / 日志 16MiB),
// 建出日志目录, 并为 stdout 与 stderr 各开一个日志文件。
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

// Stdout 返回 stdout 流, 可直接当 io.Writer 挂给子进程。
func (l *OutputLimiter) Stdout() *limitedStream { return l.stdout }

// Stderr 返回 stderr 流。
func (l *OutputLimiter) Stderr() *limitedStream { return l.stderr }

// Close 关闭两个日志文件; 两个都会尝试关闭, 返回先出现的那个错误。
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

// Result 汇总本次执行的结果: 两侧预览、合并文本、日志目录、是否截断与总字节数。
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

// addBytes 累加全局已写字节数; 一旦超过总量上限就触发限流 (回调至多一次)。
func (l *OutputLimiter) addBytes(n int64) {
	if n <= 0 {
		return
	}
	total := l.total.Add(n)
	if total > l.maxTotal {
		l.triggerLimit()
	}
}

// triggerLimit 置位限流标记, 并用 sync.Once 保证 onLimit 回调全局只触发一次。
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

// Write 实现 io.Writer: 先计入全局总量, 再把字节同时喂给预览缓冲与日志文件, 两级各自封顶。
//
// 算法解读：
//
//	原理：一份输出流要同时满足三个互相独立的预算 —— 全局总量 (maxTotal, stdout+stderr
//	  合算)、内存预览 (maxPreview, 回给模型看的片段)、落盘日志 (maxLog, 事后排障用)。
//	  每次 Write 对三者分别裁剪, 任一被裁就把 truncated 置位。
//	关键不变量：**恒返回 (len(p), nil)**。若按实际留存字节数返回短写, os/exec 会把它
//	  当成写失败并中断子进程 —— 限流是"我们主动丢字节", 不能伪装成 IO 错误。
//	代价与边界：每次写都要拿一次互斥锁 (预览是 bytes.Buffer, 非并发安全); 字节照收照
//	  算, 只是不再留存 —— 所以它限的是**留存**, 不是进程的输出速率。
//	设计权衡：超限后不关管道、不杀进程, 只静默丢弃并打 truncated 标记。相比"写满就断",
//	  长输出的任务仍能跑完 (不影响结果正确性), 代价是丢了尾部输出。
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

// previewString 加锁读回已缓存的预览文本 (与并发中的 Write 安全)。
func (s *limitedStream) previewString() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preview.String()
}
