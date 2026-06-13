package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStagesFromTarget(t *testing.T) {
	wf := &WorkflowDef{Stages: []StageDef{
		{Name: "research"}, {Name: "design"}, {Name: "write"}, {Name: "critic"},
	}}
	got := stagesFromTarget(wf, "write")
	want := []string{"write", "critic"}
	if len(got) != len(want) {
		t.Fatalf("stagesFromTarget(write)=%v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stagesFromTarget(write)=%v, 期望 %v", got, want)
		}
	}
	if s := stagesFromTarget(wf, "nope"); s != nil {
		t.Errorf("未知阶段应返回 nil, 得到 %v", s)
	}
	names := stageNamesOf(wf)
	if len(names) != 4 || names[0] != "research" || names[3] != "critic" {
		t.Errorf("stageNamesOf 错误: %v", names)
	}
}

func TestInvalidateCheckpoints(t *testing.T) {
	dir := t.TempDir()
	// 构造与 Coordinator 持久化兼容的检查点文件 (map[stageName]*Checkpoint)
	cps := map[string]*Checkpoint{
		"research": {StageName: "research", Status: "completed", Output: "r"},
		"design":   {StageName: "design", Status: "completed", Output: "d"},
		"write":    {StageName: "write", Status: "completed", Output: "w"},
		"critic":   {StageName: "critic", Status: "completed", Output: "c"},
	}
	data, _ := json.MarshalIndent(cps, "", "  ")
	path := filepath.Join(dir, "checkpoints.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	// 从 write 起失效 (write+critic), 保留 research/design
	if err := InvalidateCheckpoints(dir, []string{"write", "critic"}); err != nil {
		t.Fatalf("InvalidateCheckpoints: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var got map[string]*Checkpoint
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["research"]; !ok {
		t.Error("research 检查点不应被删除 (早于目标)")
	}
	if _, ok := got["design"]; !ok {
		t.Error("design 检查点不应被删除 (早于目标)")
	}
	if _, ok := got["write"]; ok {
		t.Error("write 检查点应被失效")
	}
	if _, ok := got["critic"]; ok {
		t.Error("critic 检查点应被失效")
	}

	// 无文件时不应报错
	if err := InvalidateCheckpoints(t.TempDir(), []string{"x"}); err != nil {
		t.Errorf("无检查点文件时应静默返回, 得到 %v", err)
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := map[string]string{
		`{"score":80,"pass":true}`:                              `{"score":80,"pass":true}`,
		"前缀 ```json\n{\"score\":60}\n``` 后缀":                    `{"score":60}`,
		`噪声 {"a":{"b":"}"},"c":1} 尾巴`:                            `{"a":{"b":"}"},"c":1}`,
		`含转义 {"s":"a\"b}","n":2}`:                               `{"s":"a\"b}","n":2}`,
	}
	for in, want := range cases {
		if got := extractJSONObject(in); got != want {
			t.Errorf("extractJSONObject(%q)=%q, 期望 %q", in, got, want)
		}
	}
	if got := extractJSONObject("no json here"); got != "" {
		t.Errorf("无 JSON 应返回空, 得到 %q", got)
	}
}

func TestPrimaryContentStageIndex(t *testing.T) {
	results := []StageResult{
		{Name: "research", Role: "researcher", Status: TaskCompleted, Output: "short"},
		{Name: "article-writing", Role: "tech-writer", Status: TaskCompleted, Output: "this is the long deliverable content body"},
		{Name: "self-critique", Role: "tech-critic", Status: TaskCompleted, Output: "a very long critique that should be excluded even though it is long enough to win"},
	}
	idx := primaryContentStageIndex(results)
	if idx != 1 {
		t.Fatalf("primaryContentStageIndex=%d, 期望 1 (article-writing, 排除 critic)", idx)
	}
	// 全是评审/空 → -1
	none := []StageResult{
		{Name: "review", Role: "reviewer", Status: TaskCompleted, Output: "x"},
		{Name: "w", Role: "writer", Status: TaskFailed, Output: ""},
	}
	if i := primaryContentStageIndex(none); i != -1 {
		t.Errorf("无有效主产出应返回 -1, 得到 %d", i)
	}
}

func TestContentQualityGated(t *testing.T) {
	for _, w := range []string{"techblog", "research", "TechBlog"} {
		if !contentQualityGated(w) {
			t.Errorf("%q 应启用内容质量门禁", w)
		}
	}
	for _, w := range []string{"development", "creative", "trading-v2"} {
		if contentQualityGated(w) {
			t.Errorf("%q 不应启用内容质量门禁", w)
		}
	}
}
