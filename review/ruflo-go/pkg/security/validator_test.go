package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidString(t *testing.T) {
	t.Parallel()
	v := NewInputValidator()
	res := v.ValidateString("msg", "hello world 你好")
	if !res.OK || len(res.Issues) != 0 {
		t.Fatalf("expected OK, got %#v", res)
	}
}

func TestSQLInjection(t *testing.T) {
	t.Parallel()
	v := NewInputValidator()
	res := v.ValidateString("q", "foo UNION SELECT * FROM users")
	found := false
	for _, i := range res.Issues {
		if i.Message == "suspected SQL-like payload" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected SQL issue, got %#v", res.Issues)
	}
}

func TestXSS(t *testing.T) {
	t.Parallel()
	v := NewInputValidator()
	res := v.ValidateString("html", `<script>alert(1)</script>`)
	found := false
	for _, i := range res.Issues {
		if i.Message == "contains shell metacharacters" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected detection of angle-bracket payload, got %#v", res.Issues)
	}
}

func TestPathTraversal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sub := filepath.Join(root, "safe")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	pv := NewPathValidator(root)
	outside := filepath.Join(root, "..", "..", "etc", "passwd")
	if _, err := pv.Validate(outside, false); err == nil {
		t.Fatal("expected error for path outside root")
	}
	inside := filepath.Join(sub, "file.txt")
	got, err := pv.Validate(inside, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty cleaned path")
	}
}

func TestSanitizeString(t *testing.T) {
	t.Parallel()
	s := SanitizeString("  hello\x00world\n")
	if s != "helloworld" {
		t.Fatalf("got %q", s)
	}
}

func TestSanitizeHTML(t *testing.T) {
	t.Parallel()
	s := SanitizeHTML("<p>hi</p> <script>x</script>")
	if s != "hi x" {
		t.Fatalf("got %q", s)
	}
}

func TestSanitizePath(t *testing.T) {
	t.Parallel()
	got := SanitizePath(`a/../b/./c`)
	if strings.Contains(got, "..") {
		t.Fatalf("traversal leaked: %q", got)
	}
	if got != "b/c" {
		t.Fatalf("SanitizePath: want b/c, got %q", got)
	}
}

func TestValidateEmail(t *testing.T) {
	t.Parallel()
	if r := ValidateEmail(""); r == nil || r.OK {
		t.Fatalf("empty: %#v", r)
	}
	if r := ValidateEmail("not-an-email"); r == nil || r.OK {
		t.Fatalf("invalid: %#v", r)
	}
	if r := ValidateEmail("user.name+tag@example.com"); r == nil || !r.OK {
		t.Fatalf("valid: %#v", r)
	}
}
