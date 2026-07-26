package worker

// k8sjob_test.go —— k8s-job runtime。
//
// 测试策略: kubectl 通路是**假的** (fakeKube), 其余全是真的 —— 真队列、真 HTTP
// 控制面、真 worker 循环、真握手文件。因为这一项的风险不在"kubectl 会不会执行",
// 而在:
//   ① 清单里那几个参数写没写对 (backoffLimit / claim / 工作区三件套 / ephemeral);
//   ② Job 起不来时控制面**看不看得见**失败, 以及归因对不对;
//   ③ Job 的生命周期 (谁删、什么时候删、并发名额还没还)。
// 其中 ① 用"照着自己生成的清单把 worker 真起起来"来验 —— 参数少写一个, 认领就失败。
// 真集群验证另做 (见报告)。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
)

// ─────────────────────────────────────────────────────────────────────────────
// 假 kubectl
// ─────────────────────────────────────────────────────────────────────────────

type fakeKube struct {
	mu       sync.Mutex
	applied  [][]byte
	deleted  []string
	canI     string // auth can-i 的输出, 空 → "yes"
	applyErr error
	// jobBody/podBody 可空 → 默认"Job 活着, pod 无异常"。
	jobBody func() ([]byte, error)
	podBody func() ([]byte, error)
	// onApply 收到清单时的回调 (测试里用它"把 Job 跑起来")。
	onApply func(manifest []byte)
}

func (f *fakeKube) run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("空命令")
	}
	switch {
	case args[0] == "version":
		return []byte(`{"clientVersion":{"gitVersion":"v1.31.0"}}`), nil
	case args[0] == "auth":
		f.mu.Lock()
		v := f.canI
		f.mu.Unlock()
		if v == "" {
			v = "yes"
		}
		if strings.TrimSpace(v) != "yes" {
			return []byte(v + "\n"), fmt.Errorf("exit status 1")
		}
		return []byte(v + "\n"), nil
	case args[0] == "get" && args[1] == "jobs":
		return []byte(""), nil // 探活的读权限检查 (列表可以是空的)
	case args[0] == "get" && args[1] == "job":
		if f.jobBody != nil {
			return f.jobBody()
		}
		return []byte(`{"status":{"active":1}}`), nil
	case args[0] == "get" && args[1] == "pods":
		if f.podBody != nil {
			return f.podBody()
		}
		return []byte(`{"items":[]}`), nil
	case args[0] == "apply":
		f.mu.Lock()
		f.applied = append(f.applied, append([]byte(nil), stdin...))
		cb, err := f.onApply, f.applyErr
		f.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if cb != nil {
			cb(stdin)
		}
		return []byte("job.batch/x created\n"), nil
	case args[0] == "delete":
		f.mu.Lock()
		f.deleted = append(f.deleted, args[2])
		f.mu.Unlock()
		return []byte("deleted\n"), nil
	}
	return nil, fmt.Errorf("fakeKube: 未预期的命令 %v", args)
}

func (f *fakeKube) applyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
}

func (f *fakeKube) deletedJobs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// jobStatusJSON 造一份 Job 状态。
func jobStatusJSON(cond, reason, msg string, active, failed int) func() ([]byte, error) {
	return func() ([]byte, error) {
		body := map[string]any{"status": map[string]any{"active": active, "failed": failed}}
		if cond != "" {
			body["status"].(map[string]any)["conditions"] = []any{
				map[string]any{"type": cond, "status": "True", "reason": reason, "message": msg},
			}
		}
		return json.Marshal(body)
	}
}

// podWaitingJSON 造一份"容器卡在 waiting"的 pod 列表。
func podWaitingJSON(name, reason, msg string) func() ([]byte, error) {
	return func() ([]byte, error) {
		return json.Marshal(map[string]any{"items": []any{map[string]any{
			"metadata": map[string]any{"name": name},
			"status": map[string]any{"phase": "Pending", "containerStatuses": []any{
				map[string]any{"state": map[string]any{
					"waiting": map[string]any{"reason": reason, "message": msg}}},
			}},
		}}})
	}
}

// newJobRuntime 造一个用假 kubectl 的 k8s-job runtime。
func newJobRuntime(t *testing.T, tc *testControl, kube kubeRunner, mut func(*K8SJobOptions)) agent.AgentRuntime {
	t.Helper()
	opt := K8SJobOptions{
		Control: "http://control:18080", Image: "claude-go:test",
		Namespace: "claude-go", kube: kube,
	}
	if mut != nil {
		mut(&opt)
	}
	rt, err := tc.brk.K8SJobRuntime(opt)
	if err != nil {
		t.Fatalf("K8SJobRuntime: %v", err)
	}
	return rt
}

// jobArgs 从生成的清单里取出容器参数 (断言与"照着它起 worker"都要用)。
func jobArgs(t *testing.T, manifest []byte) (jobName string, args []string, spec map[string]any) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatalf("清单不是合法 JSON: %v", err)
	}
	jobName = m["metadata"].(map[string]any)["name"].(string)
	spec = m["spec"].(map[string]any)
	c := spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	for _, a := range c["args"].([]any) {
		args = append(args, a.(string))
	}
	return jobName, args, spec
}

// argVal 取 --flag 的值 (没有则返回空)。
func argVal(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func hasArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// 清单形状
// ─────────────────────────────────────────────────────────────────────────────

// 断言的是"这份清单足以让一个一次性 worker 认领并干对活":
// 钉住的名字、认领的任务、工作区三件套、ephemeral 自我声明、以及 backoffLimit=0。
func TestK8SJob_清单把一次性worker钉在唯一任务上(t *testing.T) {
	shared := t.TempDir()
	tc := newTestControl(t, controlOpts{ws: &WorkspacePolicy{Mode: WorkspaceModePVC, Volume: "vol-a"}})
	kube := &fakeKube{}
	rt := newJobRuntime(t, tc, kube, func(o *K8SJobOptions) {
		o.WorkspacePVC = "claude-go-teams"
		o.WorkspaceMountPath = shared
		o.ConfigMap = "claude-go-config"
		o.EnvSecret = []string{"KIMI_API_KEY=claude-go-llm/kimi-api-key"}
	})

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := rt.Execute(ctx, agent.RuntimeNodeTask{
		RunID: "run-1", NodeID: "node-1", Role: "coder", UserPrompt: "写代码",
		Workspace: shared, Placement: &agent.Placement{AffinityKey: "团队甲"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 本用例只看清单; 没人会去执行这条任务, 取消掉让 watch 收尾。
	defer func() { cancel(); drain(t, ch, 5*time.Second) }()

	if kube.applyCount() != 1 {
		t.Fatalf("期望创建 1 个 Job, 实得 %d", kube.applyCount())
	}
	jobName, args, spec := jobArgs(t, kube.applied[0])

	// ① Job 的重试必须关死 (§4.3 重试单层化)。
	if bl, _ := spec["backoffLimit"].(float64); bl != 0 {
		t.Errorf("backoffLimit 必须是 0 (图层已经在重试, 队列 MaxAttempts=1), 实得 %v", spec["backoffLimit"])
	}
	if _, ok := spec["activeDeadlineSeconds"]; !ok {
		t.Error("缺 activeDeadlineSeconds: 拉不到镜像的 Job 永不终结, TTL 也回收不掉它")
	}
	// ② 钉住: worker 名 = Job 名, 且队列任务的 RequireCaps 含 worker:<它>。
	if argVal(args, "--name") != jobName {
		t.Errorf("worker 名 (%s) 必须与 Job 名 (%s) 一致", argVal(args, "--name"), jobName)
	}
	taskID := firstTaskID(t, tc)
	if got := argVal(args, "--claim"); got != taskID {
		t.Errorf("--claim 应为队列里那条任务 %s, 实得 %s", taskID, got)
	}
	tasks, _ := tc.q.List()
	if !hasArg(tasks[0].RequireCaps, WorkerCap(jobName)) {
		t.Errorf("任务未钉住一次性 worker: RequireCaps=%v", tasks[0].RequireCaps)
	}
	// ③ ephemeral 自我声明 (否则控制面会把它当常驻算力)。
	if !strings.Contains(argVal(args, "--caps"), CapEphemeral) {
		t.Errorf("--caps 必须含 %s, 实得 %q", CapEphemeral, argVal(args, "--caps"))
	}
	// ④ 工作区三件套 + cwd 必须等于工作区 (worker 侧会强校验)。
	if argVal(args, "--workspace-mode") != "pvc" || argVal(args, "--workspace-volume") != "vol-a" {
		t.Errorf("工作区档位参数不对: %v", args)
	}
	if argVal(args, "--workspace") != shared || argVal(args, "--cwd") != shared {
		t.Errorf("--workspace/--cwd 都必须是团队 cwd %s, 实得 %s/%s",
			shared, argVal(args, "--workspace"), argVal(args, "--cwd"))
	}
	// ⑤ 卷与 Secret 真进了 pod。
	raw := string(kube.applied[0])
	for _, want := range []string{"claude-go-teams", "claude-go-config", "kimi-api-key", `"restartPolicy":"Never"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("清单里缺 %q:\n%s", want, raw)
		}
	}
}

func TestK8SJob_local档必须拒绝构造(t *testing.T) {
	tc := newTestControl(t, controlOpts{ws: &WorkspacePolicy{Mode: WorkspaceModeLocal}})
	_, err := tc.brk.K8SJobRuntime(K8SJobOptions{
		Control: "http://c:1", Image: "i", kube: &fakeKube{}})
	if err == nil {
		t.Fatal("local 档下必须拒绝: 每个 Job 都是新 pod/新空目录, 上一阶段的产物必然消失, " +
			"而 checkWorkspace 只比路径字符串会全程通过 (静默失败)")
	}
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("错误信息应说清是 local 档的问题, 实得: %v", err)
	}
}

func TestK8SJob_团队cwd不在挂载点下必须拒绝(t *testing.T) {
	tc := newTestControl(t, controlOpts{ws: &WorkspacePolicy{Mode: WorkspaceModePVC, Volume: "v"}})
	rt := newJobRuntime(t, tc, &fakeKube{}, func(o *K8SJobOptions) {
		o.WorkspacePVC = "pvc-a"
		o.WorkspaceMountPath = "/workspace"
	})
	// 控制面这一侧目录是真的 (握手写得进去), 但它不在 Job 的 PVC 挂载点之下 ——
	// 这正是"两边都说得通、Job 里却是空目录"的配置错。
	_, err := rt.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "r", NodeID: "n", Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("团队 cwd 不在 PVC 挂载点之下时必须拒绝 —— Job 里那个路径下什么都没有, " +
			"而 worker 的路径校验会通过")
	}
	if !strings.Contains(err.Error(), "/workspace") {
		t.Errorf("错误信息应给出挂载点, 实得: %v", err)
	}
}

func TestK8SJob_没有建Job权限时拒绝构造(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	_, err := tc.brk.K8SJobRuntime(K8SJobOptions{
		Control: "http://c:1", Image: "i", kube: &fakeKube{canI: "no"}})
	if err == nil {
		t.Fatal("RBAC 不足时必须拒绝构造 —— 注册一个跑不通的 runtime 意味着 Pick 会把节点" +
			"派给它然后逐个失败, 而 Placement 是 fail-closed 的")
	}
	if !strings.Contains(err.Error(), "RBAC") && !strings.Contains(err.Error(), "权限") {
		t.Errorf("错误信息应指向权限, 实得: %v", err)
	}
}

func TestK8SJob_创建Job失败必须同步报错(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	rt := newJobRuntime(t, tc, &fakeKube{applyErr: fmt.Errorf("connection refused")}, nil)
	ch, err := rt.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n"})
	if err == nil {
		t.Fatal("创建 Job 失败必须同步报错, 而不是返回一个永远不出事件的通道")
	}
	if ch != nil {
		t.Error("报错时不该返回通道")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 真执行: 照着自己生成的清单把 worker 起起来
// ─────────────────────────────────────────────────────────────────────────────

// workerFromManifest 按清单里的参数起一个真 worker (模拟 Job 里那个进程)。
//
// 刻意**只**从清单取参数: 少写一个 --workspace-volume 或 --caps ephemeral,
// 这个 worker 就拉不到任务, 测试会红。这是"清单写对了"的唯一可信证据。
func workerFromManifest(t *testing.T, tc *testControl, manifest []byte, rt agent.AgentRuntime) *Worker {
	t.Helper()
	jobName, args, _ := jobArgs(t, manifest)
	mode, err := ParseWorkspaceMode(argVal(args, "--workspace-mode"))
	if err != nil {
		t.Fatalf("清单里的档位不合法: %v", err)
	}
	w, err := New(Options{
		Control: tc.srv.URL, Name: jobName, Runtime: rt,
		ExtraCaps:       splitList(argVal(args, "--caps")),
		Workspace:       argVal(args, "--workspace"),
		WorkspaceMode:   mode,
		WorkspaceVolume: argVal(args, "--workspace-volume"),
		MaxParallel:     1,
		PollInterval:    5 * time.Millisecond,
		Logf:            func(f string, a ...any) { t.Logf("[job-worker] "+f, a...) },
	})
	if err != nil {
		t.Fatalf("按清单造 worker 失败 (说明清单参数不自洽): %v", err)
	}
	return w
}

func TestK8SJob_端到端_Job里的worker认领并完成节点(t *testing.T) {
	shared := t.TempDir()
	tc := newTestControl(t, controlOpts{
		ws:    &WorkspacePolicy{Mode: WorkspaceModePVC, Volume: "vol-a"},
		grace: 60 * time.Second, // 掉线判定关到很远: 本用例不许靠它兜底
	})
	exec := echoRuntime("local-in-job", agent.RuntimeCaps{Bash: true},
		func(_ context.Context, role, prompt string) (string, error) {
			return "由 " + role + " 完成: " + prompt, nil
		})

	var started sync.WaitGroup
	started.Add(1)
	kube := &fakeKube{}
	kube.onApply = func(manifest []byte) {
		// "Job 起来了": 照着清单把 worker 跑起来, 认领它那一条任务。
		w := workerFromManifest(t, tc, manifest, exec)
		_, args, _ := jobArgs(t, manifest)
		taskID := argVal(args, "--claim")
		go func() {
			defer started.Done()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				ok, err := w.ClaimOnce(context.Background(), taskID)
				if ok {
					return
				}
				if err != nil {
					t.Logf("认领 %s: %v", taskID, err)
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Errorf("Job 里的 worker 在 5s 内没能认领到 %s", taskID)
		}()
	}
	rt := newJobRuntime(t, tc, kube, func(o *K8SJobOptions) {
		o.WorkspacePVC = "claude-go-teams"
		o.WorkspaceMountPath = shared
	})

	ch, err := rt.Execute(context.Background(), agent.RuntimeNodeTask{
		RunID: "run-9", NodeID: "node-9", Role: "coder", UserPrompt: "干活",
		Workspace: shared, Placement: &agent.Placement{AffinityKey: "团队甲"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	evs := drain(t, ch, 15*time.Second)
	term := terminal(t, evs)
	if term.Kind != agent.NodeEventDone {
		t.Fatalf("期望 done, 实得 %s (%s)", term.Kind, term.Err)
	}
	if !strings.Contains(term.Output, "由 coder 完成") {
		t.Errorf("产出不对: %q", term.Output)
	}
	started.Wait()

	// 握手文件是 pvc 档"真的是同一份数据"的证据; 任务结束后不该留在卷上。
	if ents, _ := os.ReadDir(filepath.Join(shared, handshakeDir)); len(ents) != 0 {
		t.Errorf("共享卷上残留握手文件: %v", ents)
	}
	// Job 尚未终结 (worker 刚退出) ⇒ 控制面应主动删掉它, 别让它继续烧 pod。
	if got := kube.deletedJobs(); len(got) != 1 {
		t.Errorf("期望收尾时删掉 1 个 Job, 实得 %v", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 失败必须真失败
// ─────────────────────────────────────────────────────────────────────────────

func TestK8SJob_镜像拉不到必须立刻失败且原因是真原因(t *testing.T) {
	// 掉线宽限期设得很长: 若实现退回"等 worker 掉线"那条路, 这个用例会超时红。
	tc := newTestControl(t, controlOpts{grace: 10 * time.Minute, poll: 10 * time.Millisecond})
	kube := &fakeKube{podBody: podWaitingJSON("claude-go-job-x-abc", "ImagePullBackOff",
		`Back-off pulling image "claude-go:typo"`)}
	rt := newJobRuntime(t, tc, kube, nil)

	start := time.Now()
	ch, err := rt.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	term := terminal(t, drain(t, ch, 10*time.Second))
	if term.Kind != agent.NodeEventFailed {
		t.Fatalf("Job 起不来时必须失败, 实得 %s", term.Kind)
	}
	if !strings.Contains(term.Err, "ImagePullBackOff") {
		t.Errorf("失败原因必须是**真原因**而不是 'worker 掉线': %q", term.Err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("耗时 %s: 不该等到掉线宽限期", d)
	}
	// 还没终结的 Job 会一直重试拉镜像 —— 控制面既然不认它了就得删掉。
	if got := kube.deletedJobs(); len(got) != 1 {
		t.Errorf("期望删掉那个起不来的 Job, 实得 %v", got)
	}
}

func TestK8SJob_Job结束了却没有终态必须判失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{grace: 10 * time.Minute, poll: 10 * time.Millisecond})
	kube := &fakeKube{jobBody: jobStatusJSON("Complete", "", "", 0, 0)}
	rt := newJobRuntime(t, tc, kube, nil)
	ch, err := rt.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	term := terminal(t, drain(t, ch, 10*time.Second))
	if term.Kind != agent.NodeEventFailed {
		t.Fatalf("Job 结束但队列无终态 = worker 没把结果交回来, 必须判失败, 实得 %s", term.Kind)
	}
	if !strings.Contains(term.Err, "没能把结果交回") {
		t.Errorf("归因不对: %q", term.Err)
	}
	// 已终结的 Job **不删**: kubectl logs 是看 worker 崩溃现场的唯一入口。
	if got := kube.deletedJobs(); len(got) != 0 {
		t.Errorf("已终结的 Job 应留给 TTL 以便排障, 实得删了 %v", got)
	}
}

func TestK8SJob_Job失败原因原样带回控制面(t *testing.T) {
	tc := newTestControl(t, controlOpts{grace: 10 * time.Minute, poll: 10 * time.Millisecond})
	kube := &fakeKube{jobBody: jobStatusJSON("Failed", "DeadlineExceeded", "Job was active longer than specified deadline", 0, 1)}
	rt := newJobRuntime(t, tc, kube, nil)
	ch, err := rt.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	term := terminal(t, drain(t, ch, 10*time.Second))
	if term.Kind != agent.NodeEventFailed || !strings.Contains(term.Err, "DeadlineExceeded") {
		t.Fatalf("期望带 DeadlineExceeded 的失败, 实得 %s / %q", term.Kind, term.Err)
	}
}

func TestK8SJob_kubectl读不出状态时不判死(t *testing.T) {
	// 观测通道故障 ≠ 执行故障。若把 kubectl 抖动读成节点失败, 一次 apiserver
	// 限流就会把整批在跑的节点杀光。
	tc := newTestControl(t, controlOpts{grace: 10 * time.Minute, poll: 10 * time.Millisecond})
	kube := &fakeKube{
		jobBody: func() ([]byte, error) { return nil, fmt.Errorf("the server is currently unable to handle the request") },
	}
	rt := newJobRuntime(t, tc, kube, nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := rt.Execute(ctx, agent.RuntimeNodeTask{RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Kind == agent.NodeEventFailed {
			cancel()
			t.Fatalf("kubectl 读不出状态不该判死, 却失败了: %q", ev.Err)
		}
	case <-time.After(500 * time.Millisecond): // ≈50 个轮询周期
	}
	cancel()
	term := terminal(t, drain(t, ch, 5*time.Second))
	if !strings.Contains(term.Err, "已取消") {
		t.Errorf("期望因取消而结束, 实得 %q", term.Err)
	}
}

func TestK8SJob_并发达上限时排队而不是失败(t *testing.T) {
	tc := newTestControl(t, controlOpts{grace: 10 * time.Minute, poll: 10 * time.Millisecond})
	rt := newJobRuntime(t, tc, &fakeKube{}, func(o *K8SJobOptions) { o.MaxParallel = 1 })

	first, err := rt.Execute(context.Background(), agent.RuntimeNodeTask{RunID: "r", NodeID: "n1"})
	if err != nil {
		t.Fatalf("第一个 Execute: %v", err)
	}
	// 第二个必须**等**而不是立刻失败: 容量不足是排队问题, 把它变成节点失败等于
	// 用错误掩盖排队。用一个短 ctx 证明它确实在等。
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := rt.Execute(ctx, agent.RuntimeNodeTask{RunID: "r", NodeID: "n2"}); err == nil {
		t.Fatal("上限为 1 时第二个 Execute 不该成功")
	} else if !strings.Contains(err.Error(), "上限") {
		t.Errorf("应说清是并发名额问题, 实得: %v", err)
	}
	// 第一个收尾后名额必须还回来 (defer 漏了就永久少一个名额)。
	cancelAll := make(chan struct{})
	go func() { defer close(cancelAll); drain(t, first, 10*time.Second) }()
	tc.brk.markCancel(firstTaskID(t, tc))
	<-cancelAll
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if _, err := rt.Execute(ctx2, agent.RuntimeNodeTask{RunID: "r", NodeID: "n3"}); err != nil {
		t.Fatalf("名额没还回来: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 一次性 worker 与控制面的关系
// ─────────────────────────────────────────────────────────────────────────────

func TestSync_一次性worker不进RuntimeRegistry(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	if err := tc.reg.Heartbeat(clusterWorker("worker-常驻", []string{CapBash})); err != nil {
		t.Fatal(err)
	}
	if err := tc.reg.Heartbeat(clusterWorker("claude-go-job-task-1", []string{CapBash, CapEphemeral})); err != nil {
		t.Fatal(err)
	}
	names := tc.syncOnce(t, time.Minute)
	for _, n := range names {
		if strings.Contains(n, "job-task-1") {
			t.Fatalf("一次性 worker 被当成常驻算力注册了 (%v): 它只认领钉给自己的那一条任务, "+
				"派别的节点过去会一直 pending 到宽限期结束", names)
		}
	}
	if len(names) != 1 || names[0] != "worker-常驻" {
		t.Errorf("常驻 worker 应照常注册, 实得 %v", names)
	}
}

func TestClaimOnce_拉到非指派任务必须拒绝执行(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	rt := echoRuntime("local-x", agent.RuntimeCaps{Bash: true},
		func(context.Context, string, string) (string, error) {
			t.Error("不该执行别人的任务")
			return "", nil
		})
	w, err := New(Options{Control: tc.srv.URL, Name: "job-1", Runtime: rt,
		ExtraCaps: []string{CapEphemeral}, Logf: func(f string, a ...any) { t.Logf(f, a...) }})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := EncodeStageTask(StageTask{Role: "coder", UserPrompt: "别人的活"})
	if _, err := tc.q.Enqueue(clusterTask("task-别人的", payload, []string{WorkerCap("job-1")})); err != nil {
		t.Fatal(err)
	}
	ok, err := w.ClaimOnce(context.Background(), "task-我的")
	if ok || err == nil {
		t.Fatal("拉到不属于自己的任务时必须失败: 跑了它就是既干错了活又饿死了自己那条任务")
	}
	got, _ := tc.q.Get("task-别人的")
	if got.Status != "failed" {
		t.Errorf("拒绝执行的同时必须把任务交回去 (否则它要等一整个租约周期), 实得 status=%s", got.Status)
	}
}

func TestK8SJobOptions_环境变量装配(t *testing.T) {
	t.Setenv("CLAUDE_GO_K8SJOB", "1")
	t.Setenv("CLAUDE_GO_K8SJOB_CONTROL", "http://claude-go-control:18080")
	t.Setenv("CLAUDE_GO_K8SJOB_IMAGE", "claude-go:e2e")
	t.Setenv("CLAUDE_GO_K8SJOB_CAPS", "bash,browser,mcp:playwright")
	t.Setenv("CLAUDE_GO_K8SJOB_ENV_SECRET", "KIMI_API_KEY=claude-go-llm/kimi-api-key")
	t.Setenv("CLAUDE_GO_K8SJOB_MAX_PARALLEL", "3")
	if !K8SJobEnabled() {
		t.Fatal("CLAUDE_GO_K8SJOB=1 应启用")
	}
	opt := K8SJobOptionsFromEnv()
	if opt.Control != "http://claude-go-control:18080" || opt.Image != "claude-go:e2e" {
		t.Errorf("基本参数没读到: %+v", opt)
	}
	if !opt.Caps.Bash || !opt.Caps.Browser || len(opt.Caps.Extra) != 1 {
		t.Errorf("能力声明解析不对: %+v", opt.Caps)
	}
	if opt.MaxParallel != 3 {
		t.Errorf("并发上限没读到: %d", opt.MaxParallel)
	}
	// 声明了 browser 的 runtime 必须把同一句话传给 Job 里的 worker, 否则两侧漂移。
	tc := newTestControl(t, controlOpts{})
	opt.kube = &fakeKube{}
	rt, err := tc.brk.K8SJobRuntime(opt)
	if err != nil {
		t.Fatalf("K8SJobRuntime: %v", err)
	}
	if !rt.Capabilities().Browser {
		t.Error("runtime 应对外声明 browser")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := rt.Execute(ctx, agent.RuntimeNodeTask{RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); drain(t, ch, 5*time.Second) }()
	_, args, _ := jobArgs(t, opt.kube.(*fakeKube).applied[0])
	if !hasArg(args, "--browser") {
		t.Errorf("Job 里的 worker 也必须声明 browser (否则控制面说有、执行体说没有): %v", args)
	}
	if !strings.Contains(argVal(args, "--caps"), "mcp:playwright") {
		t.Errorf("额外能力标签应透传: %q", argVal(args, "--caps"))
	}
}
