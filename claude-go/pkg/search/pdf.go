package search

// pdf.go —— 通过 pdftotext 的 stdin/stdout 提取 PDF 正文。
//
// 不创建临时文件也不经过 shell。输入、输出和进程生命周期都有上限；
// pdftotext 的任何命令错误都会向上返回，不会被误当成“空正文成功”。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultPDFMaxInputBytes  = maxFetchBodyBytes
	defaultPDFMaxOutputBytes = maxFetchBodyBytes
	defaultPDFTimeout        = 15 * time.Second
	maxPDFStderrBytes        = 4096
)

type pdfLimits struct {
	input  int
	output int
}

type pdfCommandFactory func(context.Context) *exec.Cmd

// ExtractPDF 从 PDF 字节流提取规范化正文，供 Fetch 的 application/pdf 分支调用。
// sourceURL 只写入结果，不会作为参数传给 pdftotext。
func ExtractPDF(ctx context.Context, sourceURL string, data []byte) (*Document, error) {
	return extractPDF(ctx, sourceURL, data, pdfLimits{}, nil)
}

func extractPDF(ctx context.Context, sourceURL string, data []byte, limits pdfLimits, factory pdfCommandFactory) (*Document, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, wrapPDFError(err)
	}
	if limits.input <= 0 {
		limits.input = defaultPDFMaxInputBytes
	}
	if limits.output <= 0 {
		limits.output = defaultPDFMaxOutputBytes
	}
	if len(data) == 0 {
		return nil, Wrap("pdftotext", ErrInvalid, errors.New("empty PDF input"))
	}
	if len(data) > limits.input {
		return nil, Wrap("pdftotext", ErrInvalid,
			fmt.Errorf("PDF input exceeds %d bytes", limits.input))
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultPDFTimeout)
		defer cancel()
	}
	if factory == nil {
		factory = func(ctx context.Context) *exec.Cmd {
			return exec.CommandContext(ctx, "pdftotext", "-enc", "UTF-8", "-nopgbrk", "-", "-")
		}
	}

	cmd := factory(ctx)
	if cmd == nil {
		return nil, Wrap("pdftotext", ErrUnavailable, errors.New("nil PDF command"))
	}
	cmd.Stdin = bytes.NewReader(data)
	stderr := &pdfStderr{}
	cmd.Stderr = stderr
	// 即使子进程在 context 取消后遗留管道，也要让 Wait 有退出边界。
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, Wrap("pdftotext", ErrUnavailable, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, Wrap("pdftotext", ErrUnavailable, err)
	}

	output, readErr := io.ReadAll(io.LimitReader(stdout, int64(limits.output)+1))
	if len(output) > limits.output || readErr != nil {
		// 输出超限或读管道失败时不能让进程继续写；随后始终 Wait 回收。
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if len(output) > limits.output {
		return nil, Wrap("pdftotext", ErrUnavailable,
			fmt.Errorf("PDF text exceeds %d bytes", limits.output))
	}
	if err := ctx.Err(); err != nil {
		return nil, wrapPDFError(err)
	}
	if readErr != nil {
		return nil, Wrap("pdftotext", ErrUnavailable, readErr)
	}
	if waitErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			waitErr = fmt.Errorf("%w: %s", waitErr, detail)
		}
		return nil, Wrap("pdftotext", ErrInvalid, waitErr)
	}

	markdown := strings.TrimSpace(normalizeMarkdown(string(output)))
	if markdown == "" {
		return nil, Wrap("pdftotext", ErrUnsupported,
			errors.New("PDF contains no extractable text"))
	}
	return &Document{
		URL:       sourceURL,
		Title:     firstLine(markdown),
		Markdown:  markdown,
		FetchedAt: time.Now(),
		Hash:      hashDocument(markdown),
		Via:       "pdftotext",
	}, nil
}

func wrapPDFError(err error) error {
	kind := ErrUnavailable
	if errors.Is(err, context.DeadlineExceeded) {
		kind = ErrTimeout
	}
	return Wrap("pdftotext", kind, err)
}

// pdfStderr 保留诊断开头，同时持续消费输出，避免子进程阻塞。
type pdfStderr struct {
	bytes.Buffer
}

func (w *pdfStderr) Write(p []byte) (int, error) {
	n := len(p)
	if remain := maxPDFStderrBytes - w.Len(); remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = w.Buffer.Write(p)
	}
	return n, nil
}
