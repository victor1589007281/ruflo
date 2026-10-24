package providers

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestLimitedReaderRejectsOversize 覆盖 5MB 上限与不静默截断。
func TestLimitedReaderRejectsOversize(t *testing.T) {
	big := bytes.Repeat([]byte("a"), maxBodyBytes+10)
	_, err := io.ReadAll(newLimitedReader(bytes.NewReader(big)))
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("want errBodyTooLarge, got %v", err)
	}

	ok := bytes.Repeat([]byte("b"), 1024)
	got, err := readAllLimited(bytes.NewReader(ok))
	if err != nil || len(got) != len(ok) {
		t.Fatalf("small body should pass: len=%d err=%v", len(got), err)
	}
}

// TestStripHTMLAndTruncate 覆盖标签清洗、实体反转义与按字符截断。
func TestStripHTMLAndTruncate(t *testing.T) {
	if got := stripHTML("<p>a &amp; <b>b</b></p>\n c"); got != "a & b c" {
		t.Fatalf("unexpected strip: %q", got)
	}
	if got := snippetText("<i>中文摘要</i>", 2); got != "中文…" {
		t.Fatalf("unexpected truncate: %q", got)
	}
}

// TestParseTimeAny 覆盖 RSS 与 PubMed 的常见日期格式。
func TestParseTimeAny(t *testing.T) {
	cases := map[string]time.Time{
		"Mon, 02 Jan 2006 15:04:05 GMT": time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC),
		"2024-05-06T07:08:09Z":          time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC),
		"2022 Jan 5":                    time.Date(2022, 1, 5, 0, 0, 0, 0, time.UTC),
		"2024 Jan-Feb":                  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for input, want := range cases {
		if got := parseTimeAny(input); !got.Equal(want) {
			t.Errorf("parseTimeAny(%q) = %v, want %v", input, got, want)
		}
	}
	if !parseTimeAny("not a date").IsZero() {
		t.Error("garbage date should return zero value")
	}
}

// TestWithQueryPreservesExisting 覆盖端点已有查询参数的保留。
func TestWithQueryPreservesExisting(t *testing.T) {
	got := withQuery("https://x.example/api?format=rss", map[string][]string{"q": {"a b"}})
	if !strings.Contains(got, "format=rss") || !strings.Contains(got, "q=a+b") {
		t.Fatalf("unexpected url: %s", got)
	}
}
