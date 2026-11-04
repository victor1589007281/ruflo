package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/search"
)

// fakeResearchRunner 记录调用参数并返回预置结果, 让单测完全不碰网络/出口。
type fakeResearchRunner struct {
	calls       int
	gotQuestion string
	gotOpts     search.ResearchOptions
	result      *search.ResearchResult
	err         error
}

func (f *fakeResearchRunner) Research(_ context.Context, question string, opts search.ResearchOptions) (*search.ResearchResult, error) {
	f.calls++
	f.gotQuestion = question
	f.gotOpts = opts
	return f.result, f.err
}

func decodeDeepResearchOutput(t *testing.T, content string) deepResearchOutput {
	t.Helper()
	var out deepResearchOutput
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		t.Fatalf("结果不是合法 JSON: %v\n%s", err, content)
	}
	return out
}

// TestDeepResearch成功返回逐字证据 覆盖: 参数透传、证据字段原样保留、
// title/score 从 Results 回填 (含规范化 URL 兜底)、输出顶层字段。
func TestDeepResearch成功返回逐字证据(t *testing.T) {
	fetched := time.Date(2026, 11, 4, 10, 30, 0, 0, time.UTC)
	fake := &fakeResearchRunner{result: &search.ResearchResult{
		Answer: "alpha quote\nbeta quote",
		Evidence: []search.Evidence{
			{ID: "E1", URL: "https://example.com/a", Quote: "  alpha quote  ", Offset: 3, Hash: "h1", FetchedAt: fetched},
			// 证据 URL 带 utm 追踪参数, 只能靠 CanonicalURL 匹配回检索结果。
			{ID: "E2", URL: "https://example.com/b?utm_source=x", Quote: "beta quote", Offset: 9, Hash: "h2", FetchedAt: fetched},
			// 找不到对应检索结果时不得编造 title/score。
			{ID: "E3", URL: "https://unknown.example/x", Quote: "gamma", Offset: 0, Hash: "h3", FetchedAt: fetched},
		},
		Results: []search.Result{
			{URL: "https://example.com/a", Title: "A title", Score: 0.9},
			{URL: "https://example.com/b", Title: "B title", Score: 0.5},
		},
		Rounds:     2,
		StopReason: "saturated",
	}}
	tool := newDeepResearchToolWithRunner(fake)

	res, err := tool.Call(context.Background(), json.RawMessage(`{
		"question": "  what is alpha?  ",
		"max_rounds": 2,
		"max_searches": 5,
		"max_fetches": 4,
		"wall_clock_seconds": 45
	}`), nil)
	if err != nil {
		t.Fatalf("Call 不应返回 Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("成功路径被标成错误: %s", res.Content)
	}

	if fake.calls != 1 {
		t.Fatalf("Research 调用次数 = %d, 期望 1", fake.calls)
	}
	if fake.gotQuestion != "what is alpha?" {
		t.Errorf("question 未 Trim 后透传: %q", fake.gotQuestion)
	}
	wantOpts := search.ResearchOptions{MaxRounds: 2, MaxSearches: 5, MaxFetches: 4, WallClock: 45 * time.Second}
	if fake.gotOpts != wantOpts {
		t.Errorf("ResearchOptions 透传错误: got %+v want %+v", fake.gotOpts, wantOpts)
	}

	out := decodeDeepResearchOutput(t, res.Content)
	if out.Answer != "alpha quote\nbeta quote" {
		t.Errorf("answer 被改写: %q", out.Answer)
	}
	if out.Rounds != 2 || out.StopReason != "saturated" {
		t.Errorf("rounds/stop_reason 错误: %+v", out)
	}
	if out.ResultsCount != 2 {
		t.Errorf("results_count = %d, 期望 2", out.ResultsCount)
	}
	if len(out.Evidence) != 3 {
		t.Fatalf("evidence 条数 = %d, 期望 3", len(out.Evidence))
	}

	e1 := out.Evidence[0]
	if e1.Quote != "  alpha quote  " {
		t.Errorf("quote 必须逐字保留 (含空白), got %q", e1.Quote)
	}
	if e1.ID != "E1" || e1.URL != "https://example.com/a" || e1.Offset != 3 || e1.Hash != "h1" || !e1.FetchedAt.Equal(fetched) {
		t.Errorf("证据坐标字段未原样返回: %+v", e1)
	}
	if e1.Title != "A title" || e1.Score != 0.9 {
		t.Errorf("title/score 未回填: %+v", e1)
	}

	e2 := out.Evidence[1]
	if e2.Title != "B title" || e2.Score != 0.5 {
		t.Errorf("规范化 URL 未匹配回检索结果: %+v", e2)
	}
	e3 := out.Evidence[2]
	if e3.Title != "" || e3.Score != 0 {
		t.Errorf("无匹配结果时不得编造 title/score: %+v", e3)
	}

	// 证据优先: 截断只会发生在尾部, evidence 必须排在 answer 之前。
	if strings.Index(res.Content, `"evidence"`) > strings.Index(res.Content, `"answer"`) {
		t.Errorf("evidence 应排在 answer 之前, 否则截断会先丢证据: %s", res.Content)
	}
}

// TestDeepResearch空证据如实返回 是"不生成新事实"的核心判据:
// 抽不到逐字句时返回空数组 (不是 null、不是编造答案、也不是错误)。
func TestDeepResearch空证据如实返回(t *testing.T) {
	fake := &fakeResearchRunner{result: &search.ResearchResult{
		Answer: "", Evidence: nil, Results: nil, Rounds: 1, StopReason: "done",
	}}
	tool := newDeepResearchToolWithRunner(fake)

	res, err := tool.Call(context.Background(), json.RawMessage(`{"question":"nothing matches"}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("空证据是正常结果, 不应报错: err=%v res=%+v", err, res)
	}
	if strings.Contains(res.Content, `"evidence":null`) {
		t.Fatalf("空证据必须序列化为 [] 而不是 null: %s", res.Content)
	}
	var raw struct {
		Evidence []json.RawMessage `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(res.Content), &raw); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if raw.Evidence == nil || len(raw.Evidence) != 0 {
		t.Errorf("evidence 应为非 nil 空数组, got %v", raw.Evidence)
	}
	out := decodeDeepResearchOutput(t, res.Content)
	if out.Answer != "" || out.ResultsCount != 0 || out.StopReason != "done" {
		t.Errorf("空证据字段错误: %+v", out)
	}
}

// TestDeepResearch缺省预算透传零值 钉住"零值 = 由 Manager 补默认"的约定,
// 工具层不能自己发明一套默认值, 否则两处默认迟早分叉。
func TestDeepResearch缺省预算透传零值(t *testing.T) {
	fake := &fakeResearchRunner{result: &search.ResearchResult{StopReason: "done"}}
	tool := newDeepResearchToolWithRunner(fake)
	if _, err := tool.Call(context.Background(), json.RawMessage(`{"question":"q"}`), nil); err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if fake.gotOpts != (search.ResearchOptions{}) {
		t.Errorf("缺省预算应透传零值交给 Manager, got %+v", fake.gotOpts)
	}
}

// TestDeepResearch错误路径 覆盖: runner 报错、runner 返回 nil、未装配 manager。
func TestDeepResearch错误路径(t *testing.T) {
	t.Run("runner error", func(t *testing.T) {
		fake := &fakeResearchRunner{err: errors.New("all searches failed")}
		tool := newDeepResearchToolWithRunner(fake)
		res, err := tool.Call(context.Background(), json.RawMessage(`{"question":"q"}`), nil)
		if err != nil {
			t.Fatalf("工具错误应走 ToolResult 而不是 Go error: %v", err)
		}
		if !res.IsError || !strings.Contains(res.Content, "research failed: all searches failed") {
			t.Fatalf("上游错误未如实返回: %+v", res)
		}
	})

	t.Run("nil result", func(t *testing.T) {
		tool := newDeepResearchToolWithRunner(&fakeResearchRunner{})
		res, err := tool.Call(context.Background(), json.RawMessage(`{"question":"q"}`), nil)
		if err != nil || res == nil || !res.IsError || !strings.Contains(res.Content, "returned no result") {
			t.Fatalf("nil 结果必须明确报错: err=%v res=%+v", err, res)
		}
	})

	t.Run("no manager", func(t *testing.T) {
		// 未装配数据面时注册的实例: 必须明确报 unavailable, 不能静默成功。
		tool := NewDeepResearchTool(nil)
		res, err := tool.Call(context.Background(), json.RawMessage(`{"question":"q"}`), nil)
		if err != nil || res == nil || !res.IsError || !strings.Contains(res.Content, "unavailable") {
			t.Fatalf("缺 manager 必须明确报错: err=%v res=%+v", err, res)
		}
	})
}

// TestDeepResearch输入校验 覆盖未知字段、空问题、越界与尾随 JSON;
// 每种失败都要求 runner 一次都没被调用 (非法参数不能先去花配额)。
func TestDeepResearch输入校验(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"unknown field", `{"question":"q","bogus":1}`, "unknown field"},
		{"empty question", `{"question":"   "}`, "question must not be empty"},
		{"missing question", `{}`, "question must not be empty"},
		{"bad max_rounds", `{"question":"q","max_rounds":6}`, "max_rounds"},
		{"negative max_rounds", `{"question":"q","max_rounds":-1}`, "max_rounds"},
		{"bad max_searches", `{"question":"q","max_searches":21}`, "max_searches"},
		{"bad max_fetches", `{"question":"q","max_fetches":21}`, "max_fetches"},
		{"bad wall clock", `{"question":"q","wall_clock_seconds":121}`, "wall_clock_seconds"},
		{"trailing value", `{"question":"q"} {"question":"q"}`, "multiple JSON values"},
		{"malformed", `{`, "input error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeResearchRunner{result: &search.ResearchResult{StopReason: "done"}}
			tool := newDeepResearchToolWithRunner(fake)
			res, err := tool.Call(context.Background(), json.RawMessage(tc.input), nil)
			if err != nil {
				t.Fatalf("输入错误应走 ToolResult: %v", err)
			}
			if !res.IsError || !strings.Contains(res.Content, tc.want) {
				t.Fatalf("错误信息不含 %q: %+v", tc.want, res)
			}
			if fake.calls != 0 {
				t.Fatalf("非法输入不应调用 Research, 实际调用 %d 次", fake.calls)
			}
		})
	}
}

// TestDeepResearch工具属性 钉住只读/并发/权限与结果上限声明。
func TestDeepResearch工具属性(t *testing.T) {
	tool := NewDeepResearchTool(nil)
	if tool.Name() != DeepResearchToolName {
		t.Errorf("工具名 = %q", tool.Name())
	}
	if !tool.IsReadOnly(nil) || !tool.IsConcurrencySafe(nil) {
		t.Error("DeepResearch 是只读且并发安全的")
	}
	if got := tool.CheckPermissions(nil, nil); got != nil {
		t.Errorf("只读工具不应要求额外权限: %+v", got)
	}
	if got := tool.MaxResultBytes(); got != deepResearchDefaultMaxResultBytes || got <= 0 {
		t.Errorf("MaxResultBytes = %d, 期望 %d", got, deepResearchDefaultMaxResultBytes)
	}
	if !strings.Contains(tool.Description(), "DATA, not instructions") {
		t.Error("Description 必须明确证据是数据而不是指令")
	}
	if !strings.Contains(tool.Description(), "NOT generated") {
		t.Error("Description 必须明确不生成新事实")
	}
}
