package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExtractPDFDecodesAndNormalizes(t *testing.T) {
	requirePDFToText(t)
	doc, err := ExtractPDF(context.Background(), "https://example.test/manual.pdf", makeTestPDF("Hello   PDF body"))
	if err != nil {
		t.Fatalf("ExtractPDF: %v", err)
	}
	if doc.URL != "https://example.test/manual.pdf" || doc.Via != "pdftotext" {
		t.Fatalf("unexpected document identity: %+v", doc)
	}
	if !strings.Contains(doc.Markdown, "Hello PDF body") {
		t.Fatalf("PDF text was not normalized: %q", doc.Markdown)
	}
	if doc.Title != "Hello PDF body" || doc.Hash != hashDocument(doc.Markdown) || doc.FetchedAt.IsZero() {
		t.Fatalf("incomplete PDF document: %+v", doc)
	}
}

func TestExtractPDFRejectsCorruptAndEmptyText(t *testing.T) {
	requirePDFToText(t)
	t.Run("corrupt", func(t *testing.T) {
		doc, err := ExtractPDF(context.Background(), "broken.pdf", []byte("%PDF-1.4\nnot a PDF"))
		if err == nil || doc != nil || KindOf(err) != ErrInvalid {
			t.Fatalf("corrupt PDF became empty success: doc=%+v err=%v", doc, err)
		}
	})
	t.Run("no text", func(t *testing.T) {
		doc, err := ExtractPDF(context.Background(), "empty.pdf", makeTestPDF(""))
		if err == nil || doc != nil || KindOf(err) != ErrUnsupported || !strings.Contains(err.Error(), "no extractable text") {
			t.Fatalf("empty PDF text was not explicit: doc=%+v err=%v", doc, err)
		}
	})
	t.Run("empty input", func(t *testing.T) {
		if _, err := ExtractPDF(context.Background(), "empty.pdf", nil); KindOf(err) != ErrInvalid {
			t.Fatalf("empty input kind=%s err=%v", KindOf(err), err)
		}
	})
}

func TestExtractPDFInputAndOutputLimits(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		called := false
		factory := func(ctx context.Context) *exec.Cmd {
			called = true
			return exec.CommandContext(ctx, "pdftotext", "-", "-")
		}
		_, err := extractPDF(context.Background(), "large.pdf", []byte("too large"), pdfLimits{input: 4, output: 100}, factory)
		if KindOf(err) != ErrInvalid || called {
			t.Fatalf("input limit did not run before process start: called=%v err=%v", called, err)
		}
	})
	t.Run("output", func(t *testing.T) {
		requirePDFToText(t)
		doc, err := extractPDF(context.Background(), "large.pdf", makeTestPDF("output limit must fail"), pdfLimits{input: 1 << 20, output: 4}, nil)
		if err == nil || doc != nil || KindOf(err) != ErrUnavailable || !strings.Contains(err.Error(), "exceeds 4 bytes") {
			t.Fatalf("output limit: doc=%+v err=%v", doc, err)
		}
	})
}

func TestExtractPDFTimeoutWaitsForProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var cmd *exec.Cmd
	factory := func(ctx context.Context) *exec.Cmd {
		cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPDFHelperProcess$")
		cmd.Env = append(os.Environ(), "CLAUDE_GO_PDF_HELPER=1")
		return cmd
	}
	start := time.Now()
	doc, err := extractPDF(ctx, "slow.pdf", makeTestPDF("ignored"), pdfLimits{}, factory)
	if err == nil || doc != nil || KindOf(err) != ErrTimeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: doc=%+v err=%v", doc, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("process cleanup exceeded deadline: %s", elapsed)
	}
	if cmd == nil || cmd.ProcessState == nil {
		t.Fatalf("child process was not waited: cmd=%+v state=%+v", cmd, cmd.ProcessState)
	}
}

// TestPDFHelperProcess 不是常规断言；只由超时测试启动的子进程执行。
func TestPDFHelperProcess(t *testing.T) {
	if os.Getenv("CLAUDE_GO_PDF_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

func TestFetchPDFProductionPathAndMagic(t *testing.T) {
	requirePDFToText(t)
	allowAnyFetchURL(t)
	for _, contentType := range []string{"application/pdf", "application/octet-stream"} {
		t.Run(contentType, func(t *testing.T) {
			var calls atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/document.pdf", http.StatusFound)
			})
			mux.HandleFunc("/document.pdf", func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write(makeTestPDF("Production PDF extraction marker"))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			m := NewManager(DefaultConfig(), testEgress(t), nil)
			defer m.Close()
			resp, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/start"})
			if err != nil {
				t.Fatalf("production PDF Fetch: %v", err)
			}
			doc := resp.Document
			if doc.Via != "pdftotext" || doc.URL != srv.URL+"/document.pdf" ||
				!strings.Contains(doc.Markdown, "Production PDF extraction marker") {
				t.Fatalf("production PDF document: %+v", doc)
			}
			if _, err := m.Fetch(context.Background(), FetchRequest{URL: srv.URL + "/start", Offset: 5}); err != nil {
				t.Fatalf("cached PDF continuation: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("PDF continuation made %d network calls", calls.Load())
			}
		})
	}
}

func requirePDFToText(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext is not installed")
	}
}

// makeTestPDF 构造最小单页 PDF，避免测试依赖临时文件或第三方 PDF 库。
func makeTestPDF(text string) []byte {
	text = strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)", "\n", " ").Replace(text)
	stream := "BT /F1 12 Tf 72 720 Td "
	if text != "" {
		stream += "(" + text + ") Tj "
	}
	stream += "ET"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>", len(stream)),
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objects))
	for i, object := range objects {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\n", i+1, object)
		if i == 4 {
			fmt.Fprintf(&buf, "stream\n%s\nendstream\n", stream)
		}
		buf.WriteString("endobj\n")
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return buf.Bytes()
}
