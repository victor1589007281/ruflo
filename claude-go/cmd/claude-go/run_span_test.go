// run_span_test.go — CLI `run` 路径必须产出 KindRun span。
//
// 背景. run span 此前只有两个产生方: 团队路径 (pkg/agent/run_span.go) 与飞书会话
// 路径 (pkg/feishu/session_trace.go)。CLI 的 `run` 一个都没有 —— 而 webapp 的轨迹页
// 只从第一条 kind:"run" 的 span 派生 name/workflow/status/duration
// (webapp-service/backend/src/lib/traces.ts 的 summarizeTrace)。缺了它, 这些运行
// 在页面上就是一行裸哈希 + 一个灰点, 按任何关键字都搜不到。
//
// 实测读数 (2026-09-25): /mnt/data/claude-go/statestore/log/trace-run-*.jsonl
// 共 234 个, 其中 24 个不含 run span, 且这 24 个的 mtime 恰好落在 coder 长跑那一晚。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
)

// readRunSpans 读回桶里所有 kind:"run" 的 span。走的是消费方的读法 (逐行 JSON),
// 不是内存里的对象 —— 判据要落在"盘上真的写了什么"上。
func readRunSpans(t *testing.T, root, runID string) []map[string]any {
	t.Helper()
	pat := filepath.Join(root, "log", "trace-run-"+strings.TrimPrefix(runID, "run-")+"*.jsonl")
	files, _ := filepath.Glob(pat)
	var out []map[string]any
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var d map[string]any
			if json.Unmarshal([]byte(line), &d) != nil {
				continue
			}
			if d["kind"] == "run" {
				out = append(out, d)
			}
		}
	}
	return out
}

func newTestStore(t *testing.T) (*tracestore.Store, string) {
	t.Helper()
	root := t.TempDir()
	return tracestore.New(statestore.NewFileStore(root)), root
}

func TestRecordRunSpanWritesSearchableRun(t *testing.T) {
	ts, root := newTestStore(t)
	start := time.Now().Add(-3 * time.Second)

	recordRunSpan(ts, "run-abc-1", "coder:capstone-toolbox", "",
		"opencode:space-bunny-free", tracestore.RunStatusOK, "", "把 capstone 跑通", start)

	spans := readRunSpans(t, root, "abc-1")
	if len(spans) != 1 {
		t.Fatalf("期望恰好 1 条 run span, 实得 %d 条 —— "+
			"少了它, 这次运行在轨迹页上就是一行裸哈希", len(spans))
	}
	s := spans[0]

	// name 是轨迹页唯一可搜的显示名字段 (TracesPanel.tsx 的检索串含 t.name)。
	if s["name"] != "coder:capstone-toolbox" {
		t.Fatalf("name = %v, 期望 coder:capstone-toolbox", s["name"])
	}
	attrs, _ := s["attrs"].(map[string]any)
	if attrs == nil {
		t.Fatal("attrs 为空 —— 轨迹页的 status/workflow/model 全从这里读")
	}
	// 这三个字面量是跨仓库契约: 页面按 'failed' 判红点, 按 workflow 分组。
	if attrs["status"] != tracestore.RunStatusFailed && attrs["status"] != tracestore.RunStatusOK {
		t.Fatalf("status = %v, 不是页面认的两个字面量之一", attrs["status"])
	}
	if attrs["status"] != "completed" {
		t.Fatalf("status = %v, 期望 completed (这是成功路径)", attrs["status"])
	}
	if attrs["workflow"] != "session" {
		t.Fatalf("workflow = %v, 期望 session —— 留空会让它落进「(未知工作流)」桶", attrs["workflow"])
	}
	if attrs["model"] != "opencode:space-bunny-free" {
		t.Fatalf("model = %v", attrs["model"])
	}
	// 成功路径不该带 error 键: 写了空串会让"没有错误"与"错误就是空"不可分。
	if _, has := attrs["error"]; has {
		t.Fatalf("成功路径不该有 error 键, 实得 %v", attrs["error"])
	}
	// 耗时与 objective 也得在 —— 它们是页面显示"跑了多久"与回放任务的全部依据。
	if dur, _ := s["dur_ms"].(float64); dur < 2900 || dur > 5000 {
		t.Fatalf("dur_ms = %v, 期望约 3000 (start 设在 3 秒前)", s["dur_ms"])
	}
	inRef, _ := s["input_ref"].(map[string]any)
	if inRef == nil || inRef["inline"] != "把 capstone 跑通" {
		t.Fatalf("input_ref 没有记下 objective, 实得 %v", s["input_ref"])
	}
}

// TestRecordRunSpanFailureCarriesError 失败路径必须带 status=failed 与 error 正文。
//
// 失败的运行恰恰是学习最想要的那一批 (见 pkg/agent/run_span.go 文件头的论证);
// 把它写成 completed 会让页面的失败计数永远为 0。
func TestRecordRunSpanFailureCarriesError(t *testing.T) {
	ts, root := newTestStore(t)
	recordRunSpan(ts, "run-def-1", "coder:x", "", "m",
		tracestore.RunStatusFailed, "上游 400: model not found", "目标", time.Now())

	spans := readRunSpans(t, root, "def-1")
	if len(spans) != 1 {
		t.Fatalf("期望 1 条 run span, 实得 %d", len(spans))
	}
	attrs, _ := spans[0]["attrs"].(map[string]any)
	if attrs["status"] != "failed" {
		t.Fatalf("status = %v, 期望 failed", attrs["status"])
	}
	if !strings.Contains(attrs["error"].(string), "model not found") {
		t.Fatalf("error = %v, 期望带上原始报错", attrs["error"])
	}
}

// TestRecordRunSpanCarriesAgent 显示名与 agent 身份必须分开落盘。
//
// 调用方显式给 --run-label 时 name 是任务名（coder:<任务>），从它推不出 agent；
// webapp 的「按 Agent」维度只能读 attrs.agent。这个键缺失时整条运行会落进
// 「(无 Agent)」桶 —— 不是没数据，是写了没读/读了没写。
func TestRecordRunSpanCarriesAgent(t *testing.T) {
	ts, root := newTestStore(t)
	recordRunSpan(ts, "run-ghi-1", "coder:任务名", "ops-rca", "m",
		tracestore.RunStatusOK, "", "目标", time.Now())

	spans := readRunSpans(t, root, "ghi-1")
	if len(spans) != 1 {
		t.Fatalf("期望 1 条 run span, 实得 %d", len(spans))
	}
	attrs, _ := spans[0]["attrs"].(map[string]any)
	if attrs["agent"] != "ops-rca" {
		t.Fatalf("agent = %v, 期望 ops-rca —— 缺了它页面按 Agent 分组认不出这条运行", attrs["agent"])
	}
	if spans[0]["name"] != "coder:任务名" {
		t.Fatalf("name = %v, 显示名不该被 agent 覆盖", spans[0]["name"])
	}

	// 空 agent 不写键: 与 model 同一纪律，空串会和"没有 agent"不可分。
	recordRunSpan(ts, "run-ghi-2", "plain", "", "m", tracestore.RunStatusOK, "", "目标", time.Now())
	plain := readRunSpans(t, root, "ghi-2")
	attrs2, _ := plain[0]["attrs"].(map[string]any)
	if _, has := attrs2["agent"]; has {
		t.Fatalf("无 agent 的运行不该有 agent 键, 实得 %v", attrs2["agent"])
	}
}

// TestRecordRunSpanFailsOpen 底座未装配 / 无 RunID 时**不写孤儿 span**。
//
// 与 pkg/agent/run_span.go:50 同一守卫: 空 TraceID 会被 tracestore 归进
// trace-orphan 桶, 而那个桶**不在**任何页面的 glob 里 —— 写进去等于既没被看见,
// 又给了一个还在增长的泄漏桶添砖。
func TestRecordRunSpanFailsOpen(t *testing.T) {
	start := time.Now()

	// store 为 nil: 不能 panic。
	recordRunSpan(nil, "run-x-1", "n", "", "m", tracestore.RunStatusOK, "", "o", start)

	// runID 为空: 不能写出一个挂不上任何桶的孤儿。
	ts, root := newTestStore(t)
	recordRunSpan(ts, "", "n", "", "m", tracestore.RunStatusOK, "", "o", start)
	recordRunSpan(ts, "", "", "", "", "", "", "", start)

	files, _ := filepath.Glob(filepath.Join(root, "log", "*.jsonl"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if len(strings.TrimSpace(string(b))) > 0 {
			t.Fatalf("空 RunID 不该写出任何 span, 但 %s 有内容:\n%s", f, b)
		}
	}
}
