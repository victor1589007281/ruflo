package agent

import "testing"

// param: 来源在生产必须真有值 —— 改造前 executeGraph 不传 Params,
// 于是 map 节点拿到空集合被判 skipped, 图层支持了但没通电。
func TestGraphRunParams_非空且键名带前缀(t *testing.T) {
	team := &ProductionTeam{Name: "t1", Objective: "做点事", Cwd: "/tmp/x", Language: "go", PendingFeedback: "改这里"}
	wf := &WorkflowDef{Name: "development", Mode: "pipeline"}
	p := graphRunParams(team, wf, "run-1")
	for k, want := range map[string]string{
		"run.id": "run-1", "team.name": "t1", "team.objective": "做点事",
		"team.cwd": "/tmp/x", "team.language": "go", "team.feedback": "改这里",
		"wf.name": "development", "wf.mode": "pipeline",
	} {
		if p[k] != want {
			t.Errorf("params[%q] = %q, 期望 %q", k, p[k], want)
		}
	}
	// 空字段不该塞进去 (空串参数会让 param: 来源"有键但空集合", 比没有键更难排查)
	p2 := graphRunParams(&ProductionTeam{Name: "t2"}, nil, "r")
	for _, k := range []string{"team.cwd", "team.language", "team.feedback", "wf.name"} {
		if _, ok := p2[k]; ok {
			t.Errorf("空字段 %q 不该出现在参数里", k)
		}
	}
	// nil team 不能 panic
	if got := graphRunParams(nil, nil, "r"); got["run.id"] != "r" {
		t.Errorf("nil team 时 = %v", got)
	}
}
