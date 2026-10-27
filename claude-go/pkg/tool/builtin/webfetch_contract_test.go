package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/search"
)

type contractFetcher struct {
	req        search.FetchRequest
	legacyCall int
	err        error
}

func (f *contractFetcher) FetchJSON(context.Context, string, int) (string, error) {
	f.legacyCall++
	return `{"legacy":true}`, nil
}

func (f *contractFetcher) FetchRequestJSON(_ context.Context, req search.FetchRequest) (string, error) {
	f.req = req
	if f.err != nil {
		return "", f.err
	}
	return `{"url":"https://example.com","markdown":"selected","selection":"relevant","next_offset":9}`, nil
}

type legacyOnlyFetcher struct{ calls int }

func (f *legacyOnlyFetcher) FetchJSON(context.Context, string, int) (string, error) {
	f.calls++
	return `{"legacy":true}`, nil
}

func TestWebFetchForwardsPromptBudgetAndContinuation(t *testing.T) {
	f := &contractFetcher{}
	tool := &WebFetchTool{fetcher: f}
	got, err := tool.Call(context.Background(), json.RawMessage(
		`{"url":"https://example.com/article","prompt":"find the verdict","max_content_tokens":250,"offset":40}`,
	), nil)
	if err != nil || got.IsError {
		t.Fatalf("Call: %+v, %v", got, err)
	}
	if f.req.URL != "https://example.com/article" || f.req.Prompt != "find the verdict" ||
		f.req.MaxContentTokens != 250 || f.req.Offset != 40 || f.req.MaxChars != maxWebFetchChars {
		t.Fatalf("lost fetch parameters: %+v", f.req)
	}
	if f.legacyCall != 0 || !strings.Contains(got.Content, `"selection":"relevant"`) {
		t.Fatalf("wrong data path or response: legacy=%d content=%s", f.legacyCall, got.Content)
	}
}

func TestWebFetchRejectsUnknownTrailingAndInvalidInput(t *testing.T) {
	tool := &WebFetchTool{fetcher: &contractFetcher{}}
	for _, input := range []string{
		`{"url":"https://example.com","unknown":true}`,
		`{"url":"https://example.com"} {"url":"https://example.com"}`,
		`{"url":"https://example.com"} trailing`,
		`{"url":"https://example.com","max_content_tokens":-1}`,
		`{"url":"https://example.com","offset":-1}`,
	} {
		got, err := tool.Call(context.Background(), json.RawMessage(input), nil)
		if err != nil || got == nil || !got.IsError {
			t.Fatalf("invalid input accepted: %q -> %+v, %v", input, got, err)
		}
	}
}

func TestWebFetchLegacyCannotSilentlyIgnoreAdvancedParameters(t *testing.T) {
	legacy := &legacyOnlyFetcher{}
	tool := &WebFetchTool{fetcher: legacy}
	for _, input := range []string{
		`{"url":"https://example.com","prompt":"select this"}`,
		`{"url":"https://example.com","max_content_tokens":10}`,
		`{"url":"https://example.com","offset":1}`,
	} {
		got, _ := tool.Call(context.Background(), json.RawMessage(input), nil)
		if got == nil || !got.IsError {
			t.Fatalf("legacy fetcher ignored advanced input %q: %+v", input, got)
		}
	}
	if legacy.calls != 0 {
		t.Fatalf("legacy fetcher called %d times", legacy.calls)
	}

	got, _ := tool.Call(context.Background(), json.RawMessage(`{"url":"https://example.com"}`), nil)
	if got == nil || got.IsError || legacy.calls != 1 {
		t.Fatalf("simple legacy compatibility broken: %+v calls=%d", got, legacy.calls)
	}
}

func TestWebFetchDataPlaneErrorsRemainErrors(t *testing.T) {
	tool := &WebFetchTool{fetcher: &contractFetcher{err: errors.New("blocked")}}
	got, err := tool.Call(context.Background(), json.RawMessage(`{"url":"https://example.com","prompt":"x"}`), nil)
	if err != nil || got == nil || !got.IsError || !strings.Contains(got.Content, "blocked") {
		t.Fatalf("fetch error was hidden: %+v, %v", got, err)
	}
}
