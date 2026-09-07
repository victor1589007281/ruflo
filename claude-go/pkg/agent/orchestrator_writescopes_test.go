package agent

import (
	"testing"
)

// F13 writeScopes: 规划层写域串行化与观测。

func TestRawTasksToDAGSerializesSharedWriteScopes(t *testing.T) {
	dag := newWBSFakeDAG()
	o := NewOrchestrator(OrchestratorConfig{MaxParallel: 3}, dag, nil, func(string, string) {}, nil, "")
	raw := []rawTask{
		{num: "1", title: "契约定义", role: "coder", taskType: wbsTaskTypeLeaf, writeScopes: []string{"contract:store"}},
		{num: "2", title: "契约实现", role: "coder", taskType: wbsTaskTypeLeaf, writeScopes: []string{"contract:store"}},
	}

	nodes, err := o.rawTasksToDAG(raw, "test-team")
	if err != nil {
		t.Fatalf("rawTasksToDAG failed: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	second := dag.tasks[nodes[1].V2TaskID]
	if len(second.DependsOn) != 1 || second.DependsOn[0] != nodes[0].V2TaskID {
		t.Fatalf("expected second same-scope task to depend on first, got %+v", second.DependsOn)
	}
	if o.dagMaxWidth != 1 {
		t.Fatalf("expected serialized DAG width 1, got %d", o.dagMaxWidth)
	}
	if len(nodes[0].WriteScopes) != 1 || nodes[0].WriteScopes[0] != "contract:store" {
		t.Fatalf("expected node WriteScopes persisted, got %+v", nodes[0].WriteScopes)
	}
}

func TestRawTasksToDAGReadOnlyDoesNotSerializeScopes(t *testing.T) {
	dag := newWBSFakeDAG()
	o := NewOrchestrator(OrchestratorConfig{MaxParallel: 3}, dag, nil, func(string, string) {}, nil, "")
	raw := []rawTask{
		// 两个 verification 任务 (isReadOnlyRawTask=true) 即使残留写域声明也不串行化;
		// 写域语义 = 写意图, 只读/验证任务不应声明, 残留也跳过。
		{num: "1", title: "本地验证 A", role: "tester", taskType: wbsTaskTypeVerification, writeScopes: []string{"contract:store"}},
		{num: "2", title: "本地验证 B", role: "tester", taskType: wbsTaskTypeVerification, writeScopes: []string{"contract:store"}},
	}

	nodes, err := o.rawTasksToDAG(raw, "test-team")
	if err != nil {
		t.Fatalf("rawTasksToDAG failed: %v", err)
	}
	second := dag.tasks[nodes[1].V2TaskID]
	if len(second.DependsOn) != 0 {
		t.Fatalf("verification tasks sharing scopes must not serialize, got %+v", second.DependsOn)
	}
}

func TestRawTasksToDAGDistinctScopesRunParallel(t *testing.T) {
	dag := newWBSFakeDAG()
	o := NewOrchestrator(OrchestratorConfig{MaxParallel: 3}, dag, nil, func(string, string) {}, nil, "")
	raw := []rawTask{
		{num: "1", title: "实现存储", role: "coder", taskType: wbsTaskTypeLeaf, writeScopes: []string{"state:mvcc"}},
		{num: "2", title: "实现网关", role: "coder", taskType: wbsTaskTypeLeaf, writeScopes: []string{"net:gateway"}},
	}

	nodes, err := o.rawTasksToDAG(raw, "test-team")
	if err != nil {
		t.Fatalf("rawTasksToDAG failed: %v", err)
	}
	second := dag.tasks[nodes[1].V2TaskID]
	if len(second.DependsOn) != 0 {
		t.Fatalf("distinct-scope write tasks must stay parallel, got %+v", second.DependsOn)
	}
	if o.dagMaxWidth != 2 {
		t.Fatalf("expected DAG width 2, got %d", o.dagMaxWidth)
	}
}

func TestAddTaskFullWriteScopesRoundTrip(t *testing.T) {
	// 可选接口断言: AddTaskFull 透传写域, AddTaskWithDeps 薄包装不设写域
	full := newWBSFakeDAG()
	ok := func() bool {
		_, canFull := interface{}(full).(interface {
			AddTaskFull(subject, description, owner string, dependsOn []string, priority int, writeScopes []string) (string, error)
		})
		return canFull
	}()
	if ok {
		t.Fatalf("wbsFakeDAG 不实现 AddTaskFull, 应走 fallback 路径")
	}
	fallbackDAG := newWBSFakeDAG()
	o := NewOrchestrator(OrchestratorConfig{MaxParallel: 2}, fallbackDAG, nil, func(string, string) {}, nil, "")
	nodes, err := o.rawTasksToDAG([]rawTask{
		{num: "1", title: "任务A", role: "coder", taskType: wbsTaskTypeLeaf, writeScopes: []string{"contract:store"}},
	}, "test-team")
	if err != nil {
		t.Fatalf("fallback rawTasksToDAG failed: %v", err)
	}
	if len(nodes) != 1 || fallbackDAG.tasks[nodes[0].V2TaskID].Subject == "" {
		t.Fatalf("fallback AddTaskWithDeps must create the task")
	}
	// fallback 后任务上仍应保留写域作为 TaskNode 观测元数据 (但 fake dag 不存 scopes)
	if len(nodes[0].WriteScopes) != 1 || nodes[0].WriteScopes[0] != "contract:store" {
		t.Fatalf("TaskNode.WriteScopes must persist even via fallback, got %+v", nodes[0].WriteScopes)
	}
}

func TestShouldKeepRawDependencyWriteScopeOverlap(t *testing.T) {
	cur := rawTask{num: "1", writeScopes: []string{"state:mvcc"}}
	dep := rawTask{num: "0", writeScopes: []string{"state:mvcc"}}
	if !shouldKeepRawDependency(cur, dep) {
		t.Fatalf("writeScopes overlap must keep dependency (anti-relax)")
	}
	dep2 := rawTask{num: "0b", writeScopes: []string{"net:gateway"}}
	if shouldKeepRawDependency(cur, dep2) {
		t.Fatalf("distinct writeScopes must not force dependency")
	}
}

func TestWBSJSONWriteScopesRoundTrip(t *testing.T) {
	plan := `{
	  "tasks": [{
	    "id": "1",
	    "title": "共享核心状态",
	    "role": "coder",
	    "taskType": "leaf",
	    "acceptance": "done",
	    "writeScopes": ["state:mvcc", "contract:store"]
	  }]
	}`
	tasks := parseWBSFromJSON(plan)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if len(tasks[0].writeScopes) != 2 || tasks[0].writeScopes[0] != "state:mvcc" {
		t.Fatalf("writeScopes not parsed from WBS JSON: %+v", tasks[0].writeScopes)
	}
	tasks[0] = normalizeRawTaskDefaults(tasks[0])
	if len(tasks[0].writeScopes) != 2 {
		t.Fatalf("normalize must preserve unique writeScopes, got %+v", tasks[0].writeScopes)
	}

	// marshal → parse 往返
	out, err := marshalRawTasksAsWBSJSON(tasks)
	if err != nil {
		t.Fatalf("marshalRawTasksAsWBSJSON failed: %v", err)
	}
	back := parseWBSFromJSON(out)
	if len(back) != 1 || len(back[0].writeScopes) != 2 || back[0].writeScopes[1] != "contract:store" {
		t.Fatalf("WBS JSON writeScopes round-trip failed: %q / %+v", out, back)
	}
}
