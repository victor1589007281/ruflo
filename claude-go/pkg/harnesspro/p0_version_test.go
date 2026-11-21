package harnesspro

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/graph"
)

type fixedOutputLLM struct{ output string }

func (f fixedOutputLLM) Model() string { return "fixed-test-model" }
func (f fixedOutputLLM) Complete(context.Context, string, string) (string, Usage, error) {
	return f.output, Usage{Input: 3, Output: 5}, nil
}

func versionedTestHarness(t *testing.T, runID string) Harness {
	t.Helper()
	h, err := IdentifyHarness(Harness{ID: "h0", Instructions: " obey the current task contract "}, runID)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestFailureAssertionsSurviveRolloutEvalFloorAndAnalyzer(t *testing.T) {
	tasks := []Task{{ID: "task-a", Prompt: "输出必须包含 required"}, {ID: "task-b", Prompt: "输出必须包含 required"}}
	store := NewStore(tasks)
	for _, task := range tasks {
		if err := store.FreezeTests(task.ID, []FrozenTest{{
			TestID: task.ID + "-t01", Assert: Assertion{Kind: AssertContains, Param: "required"},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	h := versionedTestHarness(t, "run-p0")
	store.RegisterHarness(h)
	r := &Runner{LLM: fixedOutputLLM{output: "wrong"}, Store: store, RunID: "run-p0", K: 1, ValidIDs: []string{"task-b"}}

	node := r.runRolloutsShard(context.Background(), graph.NodeSpec{ID: "rollouts#0"}, graph.NodeInput{
		Shard: &graph.ShardInput{Value: mustJSON(rolloutShardIn{C: h.ID, T: "task-a"})},
	})
	if node.Status != graph.NodeStatusCompleted {
		t.Fatalf("rollout failed: %s", node.Err)
	}
	var rollouts []RolloutResult
	if err := jsonUnmarshalForTest(node.Output, &rollouts); err != nil {
		t.Fatal(err)
	}
	if len(rollouts) != 1 || len(rollouts[0].FailedTests) != 1 ||
		!strings.Contains(rollouts[0].FailedTests[0], "task-a-t01") ||
		rollouts[0].VersionID != h.VersionID || rollouts[0].ContentID != h.ContentID {
		t.Fatalf("rollout 丢失失败断言或版本坐标: %+v", rollouts)
	}
	analyzer := buildAnalyzerUser(h, metricsFor(h.ID, rollouts), rollouts)
	if !strings.Contains(analyzer, "failed_assertions") || !strings.Contains(analyzer, "task-a-t01") {
		t.Fatalf("Analyzer 输入缺失败断言:\n%s", analyzer)
	}

	ev, _, err := r.evalOne(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Details) != 1 || len(ev.Details[0].FailedTests) != 1 || ev.VersionID != h.VersionID {
		t.Fatalf("eval 丢失败断言/版本: %+v", ev)
	}
	o := &Orchestrator{llm: r.LLM, store: store}
	floor, err := o.evalPassRate(context.Background(), h.ID, []string{"task-a", "task-b"})
	if err != nil {
		t.Fatal(err)
	}
	if floor.PassRate != 0 || floor.PassFrac != 0 || len(floor.Details) != 2 {
		t.Fatalf("地板聚合错误: %+v", floor)
	}
	for _, detail := range floor.Details {
		if len(detail.FailedTests) != 1 || detail.VersionID != h.VersionID {
			t.Fatalf("地板逐题明细缺失: %+v", detail)
		}
	}
}

func TestPlannerEditorProtectTaskContract(t *testing.T) {
	for name, prompt := range map[string]string{"planner": sysPlanner, "editor": sysEditor} {
		for _, anchor := range []string{"唯一来源", "不得", "字段名", "JSON schema"} {
			if !strings.Contains(prompt, anchor) {
				t.Errorf("%s 契约保护缺锚点 %q", name, anchor)
			}
		}
	}
}

// jsonUnmarshalForTest 避免测试为了单行解析重复写错误包装。
func jsonUnmarshalForTest(text string, v any) error {
	return json.Unmarshal([]byte(text), v)
}
