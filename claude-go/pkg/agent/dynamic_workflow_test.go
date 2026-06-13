package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type stubGenLLM struct {
	out     string
	gotUser string
}

func (s *stubGenLLM) SimpleComplete(_ context.Context, _, user string) (string, error) {
	s.gotUser = user
	return s.out, nil
}

func TestGenerateWorkflowDef(t *testing.T) {
	llm := &stubGenLLM{out: "这是结果 ```json\n" +
		`{"name":"gen-flow","mode":"pipeline","stages":[{"name":"a","role":"researcher","prompt":"调研"},{"name":"b","prompt":"写","dependsOn":["a"]}]}` +
		"\n``` 完毕"}
	def, err := GenerateWorkflowDef(context.Background(), llm, "调研并撰写某主题", []string{"researcher", "tech-writer"})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if def.Name != "gen-flow" || def.Mode != "pipeline" || len(def.Stages) != 2 {
		t.Fatalf("解析错误: %+v", def)
	}
	if !strings.Contains(llm.gotUser, "researcher") {
		t.Error("生成 prompt 应把可用角色名喂给 LLM (否则 LLM 会编造角色致 Validate 失败)")
	}
	if err := def.Validate(nil); err != nil {
		t.Errorf("生成的 def 应能通过 Validate(均有内联prompt): %v", err)
	}
}

func TestPlanToWorkflowDef(t *testing.T) {
	plan := &DecompositionPlan{Strategy: "parallel", Rationale: "并行拆解", SubTasks: []SubTask{
		{ID: "t1", Description: "做A", Role: "researcher"},
		{ID: "t2", Description: "做B", Role: "writer", DependsOn: []string{"t1"}},
	}}
	wf := PlanToWorkflowDef(plan, "swarm-saved")
	if wf.Name != "swarm-saved" || len(wf.Stages) != 2 {
		t.Fatalf("转换错误: %+v", wf)
	}
	if wf.Stages[0].Name != "t1" || wf.Stages[0].Prompt != "做A" || !wf.Stages[0].Parallel {
		t.Errorf("t1 转换错(无依赖+parallel策略应 Parallel): %+v", wf.Stages[0])
	}
	if len(wf.Stages[1].DependsOn) != 1 || wf.Stages[1].DependsOn[0] != "t1" || wf.Stages[1].Parallel {
		t.Errorf("t2 转换错: %+v", wf.Stages[1])
	}
	if err := wf.Validate(nil); err != nil {
		t.Errorf("转换出的 wf 应有效(有内联prompt): %v", err)
	}
}

func goodPipeline(name string) *WorkflowDef {
	return &WorkflowDef{
		Name: name, Mode: "pipeline", QualityGate: "content",
		Stages: []StageDef{
			{Name: "research", Role: "researcher", Prompt: "调研 {objective}"},
			{Name: "write", Role: "writer", Prompt: "写作 {prev_result}", DependsOn: []string{"research"}},
		},
	}
}

func TestWorkflowValidate(t *testing.T) {
	// 合法 (内联 prompt, roles=nil 也能过)
	if err := goodPipeline("dyn-ok").Validate(nil); err != nil {
		t.Fatalf("合法 pipeline 不应被拒: %v", err)
	}

	cases := map[string]*WorkflowDef{
		"空名":      {Name: "", Mode: "pipeline", Stages: []StageDef{{Name: "a", Prompt: "x"}}},
		"无阶段":     {Name: "n", Mode: "pipeline"},
		"非纯数据模式":  {Name: "n", Mode: "creative_media", Stages: []StageDef{{Name: "a", Prompt: "x"}}},
		"角色缺失无内联": {Name: "n", Mode: "pipeline", Stages: []StageDef{{Name: "a", Role: "nope"}}},
		"依赖不存在":   {Name: "n", Mode: "pipeline", Stages: []StageDef{{Name: "a", Prompt: "x", DependsOn: []string{"ghost"}}}},
		"阶段名重复": {Name: "n", Mode: "pipeline", Stages: []StageDef{
			{Name: "a", Prompt: "x"}, {Name: "a", Prompt: "y"}}},
		"循环依赖": {Name: "n", Mode: "pipeline", Stages: []StageDef{
			{Name: "a", Prompt: "x", DependsOn: []string{"b"}},
			{Name: "b", Prompt: "y", DependsOn: []string{"a"}}}},
	}
	for label, def := range cases {
		if err := def.Validate(nil); err == nil {
			t.Errorf("[%s] 应被 Validate 拒绝, 但通过了", label)
		}
	}
}

func TestRegisterAndGetWorkflow(t *testing.T) {
	name := "dyn-reg-test"
	if err := RegisterWorkflow(goodPipeline(name), nil); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	got := GetWorkflow(name)
	if got == nil || got.Name != name || !got.Custom || got.QualityGate != "content" {
		t.Fatalf("GetWorkflow 未返回注册的动态工作流: %+v", got)
	}
	// 返回副本: 改动不应影响注册表
	got.Stages[0].Name = "mutated"
	if g2 := GetWorkflow(name); g2.Stages[0].Name != "research" {
		t.Error("GetWorkflow 应返回副本, 不应被外部改动污染")
	}
	// 与内置名冲突应被拒
	if err := RegisterWorkflow(&WorkflowDef{Name: "techblog", Mode: "pipeline", Stages: []StageDef{{Name: "a", Prompt: "x"}}}, nil); err == nil {
		t.Error("与内置工作流同名应被拒绝")
	}
	// 出现在 ListWorkflows
	found := false
	for _, w := range ListWorkflows() {
		if w.Name == name {
			found = true
		}
	}
	if !found {
		t.Error("动态工作流应出现在 ListWorkflows")
	}
}

func TestGatingReadsDeclarativeFields(t *testing.T) {
	codeWF := goodPipeline("dyn-code")
	codeWF.ProducesCode = true
	codeWF.QualityGate = ""
	if err := RegisterWorkflow(codeWF, nil); err != nil {
		t.Fatal(err)
	}
	contentWF := goodPipeline("dyn-content") // QualityGate=content
	if err := RegisterWorkflow(contentWF, nil); err != nil {
		t.Fatal(err)
	}

	if !workflowProducesCode("dyn-code") {
		t.Error("producesCode=true 的自定义工作流应触发代码门禁")
	}
	if contentQualityGated("dyn-code") {
		t.Error("dyn-code 未声明 content 门禁")
	}
	if !contentQualityGated("dyn-content") {
		t.Error("qualityGate=content 的自定义工作流应触发内容门禁")
	}
	if workflowProducesCode("dyn-content") {
		t.Error("dyn-content 未声明 producesCode")
	}
	// 内置工作流不受影响
	if !workflowProducesCode("development") || workflowProducesCode("techblog") {
		t.Error("内置门禁判定被破坏")
	}
}

func TestLoadWorkflowsFromDir(t *testing.T) {
	dir := t.TempDir()
	// 合法
	ok := goodPipeline("dyn-loaded")
	data, _ := json.MarshalIndent(ok, "", "  ")
	os.WriteFile(filepath.Join(dir, "ok.json"), data, 0644)
	// 非法 (非纯数据模式)
	bad := &WorkflowDef{Name: "dyn-bad", Mode: "creative_media", Stages: []StageDef{{Name: "a", Prompt: "x"}}}
	bd, _ := json.MarshalIndent(bad, "", "  ")
	os.WriteFile(filepath.Join(dir, "bad.json"), bd, 0644)
	// 坏 JSON
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0644)

	loaded, failed, details := LoadWorkflowsFromDir(dir, nil)
	if loaded != 1 {
		t.Errorf("应加载 1 个合法工作流, got %d", loaded)
	}
	if failed != 2 {
		t.Errorf("应有 2 个失败(非法模式+坏JSON), got %d (%v)", failed, details)
	}
	if GetWorkflow("dyn-loaded") == nil {
		t.Error("合法工作流应已注册")
	}
	if GetWorkflow("dyn-bad") != nil {
		t.Error("非法工作流不应被注册")
	}

	// 往返: Save 后能 Load
	dir2 := t.TempDir()
	if err := SaveWorkflowToDir(dir2, goodPipeline("dyn-roundtrip")); err != nil {
		t.Fatal(err)
	}
	l2, _, _ := LoadWorkflowsFromDir(dir2, nil)
	if l2 != 1 || GetWorkflow("dyn-roundtrip") == nil {
		t.Error("Save→Load 往返失败")
	}
}

// TestRegisterWorkflowRace: 并发注册 + 读 (go test -race 验证无数据竞争)。
func TestRegisterWorkflowRace(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(n int) { defer wg.Done(); _ = RegisterWorkflow(goodPipeline(fmt.Sprintf("dyn-race-%d", n)), nil) }(i)
		go func(n int) { defer wg.Done(); _ = GetWorkflow(fmt.Sprintf("dyn-race-%d", n)); _ = ListWorkflows() }(i)
	}
	wg.Wait()
}
