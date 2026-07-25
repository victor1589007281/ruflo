package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// 探针: 图路径下 {prev_result} 是否真被替换 (stageNodeRunner 不设 DependsOn)
func TestProbePrevResult(t *testing.T) {
	var seen []string
	we := &WorkflowExecutor{
		factory: func(_ context.Context, role, _ string) (AgentRunner, error) {
			return &probeRunner{role: role, seen: &seen}, nil
		},
		notify: func(_, _ string) {},
	}
	wf := &WorkflowDef{Name: "probe", Mode: "pipeline", Stages: []StageDef{
		{Name: "a", Role: "researcher", Prompt: "第一阶段 {objective}"},
		{Name: "b", Role: "writer", Prompt: "第二阶段 上游=[{prev_result}] 具名=[{a}]", DependsOn: []string{"a"}},
	}}
	_, err := we.executeGraph(context.Background(), wf, "目标X", newStubTeam(t, "probe"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range seen {
		if strings.Contains(s, "第二阶段") {
			t.Logf("GRAPH b prompt: %q", s)
		}
	}
	seen = nil
	_, err = we.executePipeline(context.Background(), wf, "目标X", newStubTeam(t, "probe2"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range seen {
		if strings.Contains(s, "第二阶段") {
			t.Logf("PIPE  b prompt: %q", s)
		}
	}
	_ = atomic.Int64{}
}

type probeRunner struct {
	role string
	seen *[]string
}

func (p *probeRunner) Execute(_ context.Context, userPrompt string) (string, error) {
	*p.seen = append(*p.seen, userPrompt)
	return "[" + p.role + "] done\n\n## 分析\n- 结论: 探针产出, 足够长度以通过阶段产出结构化校验, 含标记与正文。", nil
}
