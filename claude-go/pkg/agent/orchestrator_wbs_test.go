package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type wbsFakeDAG struct {
	tasks map[string]DAGTaskSummary
	order []string
}

func newWBSFakeDAG() *wbsFakeDAG {
	return &wbsFakeDAG{tasks: make(map[string]DAGTaskSummary)}
}

func (d *wbsFakeDAG) AddTask(subject, description, owner string) (string, error) {
	return d.AddTaskWithDeps(subject, description, owner, nil, 0)
}

func (d *wbsFakeDAG) SetTaskStatus(id, status string) error {
	t, ok := d.tasks[id]
	if !ok {
		return fmt.Errorf("task not found: %s", id)
	}
	t.Status = status
	d.tasks[id] = t
	return nil
}

func (d *wbsFakeDAG) AddTaskWithDeps(subject, description, owner string, dependsOn []string, priority int) (string, error) {
	for _, id := range d.order {
		t := d.tasks[id]
		if t.Subject == subject {
			t.Description = description
			t.Owner = owner
			t.DependsOn = append([]string(nil), dependsOn...)
			t.Priority = priority
			if t.Status != "completed" && t.Status != "failed" {
				if len(dependsOn) > 0 {
					t.Status = "blocked"
				} else {
					t.Status = "pending"
				}
			}
			d.tasks[id] = t
			return id, nil
		}
	}
	id := fmt.Sprintf("task-%d", len(d.order)+1)
	d.order = append(d.order, id)
	status := "pending"
	if len(dependsOn) > 0 {
		status = "blocked"
	}
	d.tasks[id] = DAGTaskSummary{
		ID: id, Subject: subject, Description: description,
		Status: status, Owner: owner, DependsOn: append([]string(nil), dependsOn...),
		Priority: priority,
	}
	return id, nil
}

func (d *wbsFakeDAG) ReadyTasks() []DAGTaskSummary {
	var ready []DAGTaskSummary
	for _, id := range d.order {
		t := d.tasks[id]
		if t.Status == "pending" {
			ready = append(ready, t)
		}
	}
	return ready
}

func (d *wbsFakeDAG) SetTaskStatusAndUnblock(id, status string) (int, error) {
	if err := d.SetTaskStatus(id, status); err != nil {
		return 0, err
	}
	if status != "completed" {
		return 0, nil
	}
	unblocked := 0
	for taskID, task := range d.tasks {
		if task.Status != "blocked" {
			continue
		}
		allDone := true
		for _, dep := range task.DependsOn {
			if d.tasks[dep].Status != "completed" {
				allDone = false
				break
			}
		}
		if allDone {
			task.Status = "pending"
			d.tasks[taskID] = task
			unblocked++
		}
	}
	return unblocked, nil
}

func (d *wbsFakeDAG) GetAllTasks() []DAGTaskSummary {
	all := make([]DAGTaskSummary, 0, len(d.order))
	for _, id := range d.order {
		all = append(all, d.tasks[id])
	}
	return all
}

type sleepingRunner struct{}

func (sleepingRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	time.Sleep(50 * time.Millisecond)
	return "late", nil
}

type staticRunner struct {
	output string
	err    error
}

func (r staticRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	return r.output, r.err
}

func TestParseWBSFromJSONNewFields(t *testing.T) {
	plan := `{
	  "tasks": [{
	    "id": "1",
	    "title": "实现事务状态",
	    "role": "coder",
	    "taskType": "leaf",
	    "parentId": "M1",
	    "dependsOn": [],
	    "designRef": "MVCC",
	    "constraints": ["C1"],
	    "acceptance": "go test ./internal/storage/...",
	    "priority": 2,
	    "complexity": "medium",
	    "estimatedMinutes": 3,
	    "riskLevel": "medium",
	    "verifyCommand": "go test ./internal/storage/...",
	    "parallelGroup": "mvcc-core",
	    "blockingPolicy": "fail_blocks_dependents",
	    "splitReason": "manual",
	    "targetFiles": ["internal/storage/mvcc.go"],
	    "targetPackages": ["./internal/storage/..."]
	  }]
	}`

	tasks := parseWBSFromJSON(plan)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	got := tasks[0]
	if got.taskType != wbsTaskTypeLeaf || got.parentID != "M1" || got.estimatedMin != 3 {
		t.Fatalf("new WBS fields not parsed: %+v", got)
	}
	if got.riskLevel != wbsRiskMedium || got.parallelGroup != "mvcc-core" || got.blockingPolicy != wbsBlockingFailBlocks {
		t.Fatalf("risk/group/blocking fields not parsed: %+v", got)
	}
	if len(got.targetFiles) != 1 || got.verifyCommand == "" {
		t.Fatalf("target files or verify command missing: %+v", got)
	}
}

func TestRawTasksToDAGSerializesSharedTargetFiles(t *testing.T) {
	dag := newWBSFakeDAG()
	o := NewOrchestrator(OrchestratorConfig{MaxParallel: 3}, dag, nil, func(string, string) {}, nil, "")
	raw := []rawTask{
		{num: "1", title: "更新配置结构", role: "coder", taskType: wbsTaskTypeLeaf, targetFiles: []string{"internal/config/config.go"}},
		{num: "2", title: "补充配置校验", role: "coder", taskType: wbsTaskTypeLeaf, targetFiles: []string{"internal/config/config.go"}},
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
		t.Fatalf("expected second shared-file task to depend on first, got %+v", second.DependsOn)
	}
	if o.dagMaxWidth != 1 {
		t.Fatalf("expected serialized DAG width 1, got %d", o.dagMaxWidth)
	}
}

func TestExecuteRunnerBoundedReturnsTypedTimeout(t *testing.T) {
	_, err := executeRunnerBounded(context.Background(), sleepingRunner{}, "prompt", 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !isAgentExecutionTimeout(err) {
		t.Fatalf("expected typed timeout error, got %T: %v", err, err)
	}
}

func TestHandleTaskTimeoutUsesLLMSplitPlanner(t *testing.T) {
	dag := newWBSFakeDAG()
	parentID, err := dag.AddTaskWithDeps("[team] [orch] MVCC 事务管理器", "parent", "coder", nil, 1)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	downstreamID, err := dag.AddTaskWithDeps("[team] [orch] 下游集成", "downstream", "coder", []string{parentID}, 1)
	if err != nil {
		t.Fatalf("create downstream: %v", err)
	}
	llmPlan := `{
	  "tasks": [
	    {
	      "id": "s1",
	      "title": "MVCC 事务状态最小实现",
	      "role": "coder",
	      "taskType": "leaf",
	      "dependsOn": [],
	      "acceptance": "scoped build 通过",
	      "estimatedMinutes": 2,
	      "riskLevel": "medium",
	      "blockingPolicy": "fail_blocks_dependents",
	      "targetFiles": ["internal/storage/mvcc_state.go"],
	      "targetPackages": ["./internal/storage/..."]
	    },
	    {
	      "id": "s2",
	      "title": "MVCC 本地验证",
	      "role": "tester",
	      "taskType": "verification",
	      "dependsOn": ["s1"],
	      "acceptance": "go test ./internal/storage/...",
	      "estimatedMinutes": 2,
	      "riskLevel": "low",
	      "blockingPolicy": "fail_blocks_dependents",
	      "targetPackages": ["./internal/storage/..."]
	    }
	  ]
	}`
	factory := func(ctx context.Context, role, systemPrompt string) (AgentRunner, error) {
		if role != "planner" {
			t.Fatalf("expected planner role, got %s", role)
		}
		return staticRunner{output: llmPlan}, nil
	}
	o := NewOrchestrator(OrchestratorConfig{}, dag, factory, func(string, string) {}, nil, "")
	o.teamName = "team"
	parent := &TaskNode{
		V2TaskID: parentID, Title: "MVCC 事务管理器", Role: "coder",
		TaskType: wbsTaskTypeLeaf, EstimatedMin: 6, RiskLevel: wbsRiskHigh,
		BlockingPolicy: wbsBlockingFailBlocks,
		TargetFiles:    []string{"internal/storage/mvcc.go"},
		TargetPackages: []string{"./internal/storage/..."},
	}
	downstream := &TaskNode{V2TaskID: downstreamID, Title: "下游集成", Role: "coder", TaskType: wbsTaskTypeLeaf}
	o.nodes[parentID] = parent
	o.nodes[downstreamID] = downstream
	o.totalCount = 2

	sr := o.handleTaskTimeout(context.Background(), parent, "开发 MVCC 事务管理器", &ProductionTeam{Name: "team", Workflow: "development"}, time.Now().Add(-6*time.Minute), &AgentExecutionTimeoutError{Timeout: coderCallTimeout, Cause: context.DeadlineExceeded})
	if sr.Status != TaskCompleted {
		t.Fatalf("timeout parent should complete after LLM split, got %s: %s", sr.Status, sr.Error)
	}
	if !strings.Contains(sr.Output, "llm-split-planner") {
		t.Fatalf("expected llm split source in output, got %q", sr.Output)
	}
	if o.totalCount != 4 {
		t.Fatalf("expected 2 injected LLM children plus original 2 tasks, got total=%d", o.totalCount)
	}
	rewired := dag.tasks[downstreamID].DependsOn
	if len(rewired) != 1 || rewired[0] != "task-4" {
		t.Fatalf("downstream should depend on LLM verification child task-4, got %+v", rewired)
	}
	if dag.tasks["task-3"].Status != "pending" || dag.tasks["task-4"].Status != "blocked" {
		t.Fatalf("unexpected child statuses: task-3=%s task-4=%s", dag.tasks["task-3"].Status, dag.tasks["task-4"].Status)
	}
}

func TestInferObjectiveTargetRoot(t *testing.T) {
	cases := []struct {
		name      string
		objective string
		want      string
	}{
		{
			name:      "chinese workdir target",
			objective: "用 golang 实现 agentDB，输出到工作目录的agentDBV1目录下",
			want:      "agentDBV1",
		},
		{
			name:      "explicit directory target",
			objective: "开发一个 CLI，放到 MyTool 目录下",
			want:      "MyTool",
		},
		{
			name:      "no target root",
			objective: "优化 claude-go 的编排器",
			want:      "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inferObjectiveTargetRoot(tc.objective); got != tc.want {
				t.Fatalf("inferObjectiveTargetRoot()=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateParsedWBSRejectsMetaTasksForTargetRoot(t *testing.T) {
	tasks := []rawTask{
		{num: "1", title: "Review complete design document", role: "coder"},
		{num: "2", title: "Check existing project structure", role: "coder"},
	}
	objective := "用 golang 实现 agentDB，输出到工作目录的agentDBV1目录下"
	if err := validateParsedWBSForObjective(tasks, objective); err == nil {
		t.Fatal("expected meta-only WBS to be rejected")
	}
}

func TestSynthesizeObjectiveWBSCreatesGoProjectSkeleton(t *testing.T) {
	tasks := synthesizeObjectiveWBS("用 golang 实现 agentDB，输出到工作目录的agentDBV1目录下")
	if len(tasks) == 0 {
		t.Fatal("expected synthesized WBS")
	}
	if tasks[0].targetFiles[0] != "agentDBV1/go.mod" {
		t.Fatalf("first task should create go.mod under target root, got %+v", tasks[0].targetFiles)
	}
	if err := validateParsedWBSForObjective(tasks, "用 golang 实现 agentDB，输出到工作目录的agentDBV1目录下"); err != nil {
		t.Fatalf("synthesized WBS should be valid: %v", err)
	}
}
