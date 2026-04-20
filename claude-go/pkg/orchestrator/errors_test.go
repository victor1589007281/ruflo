package orchestrator

import (
	"testing"
)

func TestClassifyErrorMsg(t *testing.T) {
	tests := []struct {
		msg      string
		expected ErrorKind
	}{
		{"", ErrorPermanent},
		{"429 Too Many Requests", ErrorTransient},
		{"rate limit exceeded", ErrorTransient},
		{"connection refused", ErrorTransient},
		{"timeout waiting for response", ErrorTransient},
		{"deadline exceeded", ErrorTransient},
		{"503 Service Unavailable", ErrorTransient},
		{"temporary failure", ErrorTransient},
		{"限流", ErrorTransient},
		{"invalid api key", ErrorFatal},
		{"unauthorized", ErrorFatal},
		{"quota exceeded", ErrorFatal},
		{"billing issue", ErrorFatal},
		{"code quality: missing error handling", ErrorPermanent},
		{"validation failed: bad input", ErrorPermanent},
	}

	for _, tt := range tests {
		got := ClassifyErrorMsg(tt.msg)
		if got != tt.expected {
			t.Errorf("ClassifyErrorMsg(%q) = %s, want %s", tt.msg, got, tt.expected)
		}
	}
}

func TestRetryPolicy_ShouldRetry_Permanent(t *testing.T) {
	p := DefaultRetryPolicy()
	// MaxRetries = 2 for permanent
	d := p.ShouldRetry("validation error", 0)
	if !d.ShouldRetry {
		t.Error("should retry on first attempt")
	}
	if d.Kind != ErrorPermanent {
		t.Error("should be classified as permanent")
	}

	d = p.ShouldRetry("validation error", 1)
	if !d.ShouldRetry {
		t.Error("should retry on second attempt")
	}

	d = p.ShouldRetry("validation error", 2)
	if d.ShouldRetry {
		t.Error("should not retry after max retries")
	}
}

func TestRetryPolicy_ShouldRetry_Transient(t *testing.T) {
	p := DefaultRetryPolicy()
	// MaxRetries=2 + MaxTransient=5 = 7 total
	for i := 0; i < 7; i++ {
		d := p.ShouldRetry("429 rate limit", i)
		if !d.ShouldRetry {
			t.Errorf("should retry on attempt %d", i)
		}
		if d.Kind != ErrorTransient {
			t.Error("should be classified as transient")
		}
	}

	d := p.ShouldRetry("429 rate limit", 7)
	if d.ShouldRetry {
		t.Error("should not retry after max transient retries")
	}
}

func TestRetryPolicy_ShouldRetry_Fatal(t *testing.T) {
	p := DefaultRetryPolicy()
	d := p.ShouldRetry("invalid api key", 0)
	if d.ShouldRetry {
		t.Error("should never retry fatal errors")
	}
	if d.Kind != ErrorFatal {
		t.Error("should be classified as fatal")
	}
}

func TestNewTaskError(t *testing.T) {
	te := NewTaskError("task-1", nil, 0)
	if te.Kind != ErrorPermanent {
		t.Error("nil error should be permanent")
	}

	te = NewTaskError("task-1", &TaskError{Message: "429"}, 2)
	if te.Kind != ErrorTransient {
		t.Error("429 should be transient")
	}
	if te.Attempt != 2 {
		t.Errorf("expected attempt 2, got %d", te.Attempt)
	}
}

func TestErrorKind_String(t *testing.T) {
	if ErrorTransient.String() != "transient" {
		t.Error("wrong string for transient")
	}
	if ErrorPermanent.String() != "permanent" {
		t.Error("wrong string for permanent")
	}
	if ErrorFatal.String() != "fatal" {
		t.Error("wrong string for fatal")
	}
}
