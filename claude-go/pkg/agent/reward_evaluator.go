package agent

// reward_evaluator.go —— design/03 §4.2 奖励工程第 1 原则 (吸收 Hermes **H1**) 的两个契约:
// `RewardEvaluator` 与 `WorkspaceHandle`。
//
// ---------------------------------------------------------------------------
// H1 到底要什么 (以及此前"事实成立但无契约"是什么意思)
// ---------------------------------------------------------------------------
//
// hermes 的做法是: reward 函数拿 `ToolContext(task_id)` —— rollout 跑完后**那个还活着的
// 沙箱** —— 直接检查真实的文件/进程状态, 不去重放快照、也不去读模型自述的轨迹文本。
//
// 本仓的**事实**早就成立: `runGlobalCompileGate` / `runGlobalTestGate` 就在 `team.Cwd`
// 里真跑 `go build` / `go test -race`, 只信 exit code。但它们是两个各自
// `exec.CommandContext` + 各自 `os.Stat` + 各自拼错误串的私有方法, **没有任何契约**:
//
//   - 想加第三个"事后在工作区上算的奖励" (产物完整性、lint、契约扫描) 只能再抄一遍
//     那 20 行, 而抄漏 `recordGateReward` 就是"奖励有值但轨迹里查不到它凭什么",
//     抄漏 skipped span 就回到 §4.5 那条教训 ("静默跳过会让人以为门禁过了");
//   - 工作区访问是裸 `filepath.Join(team.Cwd, …)`, 谁都能顺手写一次盘 ——
//     而**奖励评估器写工作区**是最脏的一类污染: 它评的是自己刚改过的东西。
//
// 所以本文件做两件事: ① 把"在原始工作区上算一个奖励"这件事收成接口;
// ② 让**既有的两个门禁成为它的第一批实现**, 而不是在旁边另起一套没人调的机制
// (那正是本目录反复出现的"建成未通电")。
//
// ---------------------------------------------------------------------------
// 三条必须这样定的语义
// ---------------------------------------------------------------------------
//
// ① **句柄只读, 且是机械只读**。`WorkspaceHandle` 不提供任何写方法, 路径一律经
//    `resolve` 钳在工作区内 (含 symlink 逃逸: 先 EvalSymlinks 再验前缀 —— 只比
//    字符串前缀的话, 工作区里一条指向 `/etc` 的软链就够越界)。命令走**白名单**:
//    只有 `go build/test/vet/list` 与 `gofmt`。刻意排除的几个值得写下来 ——
//    `go run`/`go generate` 会执行工作区里 AI 刚写的任意代码 (`go test` 也执行,
//    但那是门禁本来的语义, 而 `go generate` 不是); `go get`/`go mod download` 会
//    联网拉依赖, 让"确定性门禁"的结论取决于网络。
//
// ② **接口只放当下有真实调用方的方法**。没有 `ReadFile`/`Glob`/`Stat` ——
//    多出来的方法就是下一轮"建成未通电"的条目。需要它们的那个评估器出现时再加,
//    加的时候连测试一起加。
//
// ③ **跳过 ≠ 通过 ≠ 失败, 三态分开**。`Verdict.Skipped` 记轨迹但**不发奖励**
//    (没跑过就不是证据); 通过/失败发奖励。这一条是照抄既有 `recordGateReward` 的
//    口径, 不是新政策 —— 收契约的时候最容易顺手把三态压成两态。
//
// 为什么不用更直觉的做法:
//   - **让 RewardEvaluator 自己发奖励**: 那么每个实现都要记得调 `recordGateReward`
//     + `writeGateSpan` 两件事, 漏一件就是上面那两种断链。现在实现只回一个 Verdict,
//     发奖励与写 span 在 `runRewardEvaluator` 一处。
//   - **把 team 直接传给评估器**: `*ProductionTeam` 上有 `persist()`、状态字段、
//     `mu` —— 评估器拿到它就能改团队状态, "只读评估"当场破功。它只拿工作区。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 契约
// ---------------------------------------------------------------------------

// WorkspaceHandle 一次运行结束时其**原始工作区**的只读句柄
// (design/03 §4.2 原则 1 里"等价 hermes ToolContext(task_id)"的那个东西)。
//
// 生命周期约定: 句柄只在 run 结束、工作区清理**之前**有效。design/01 的
// `run.finished` 是"先评奖励后清理"的那个次序点。
type WorkspaceHandle interface {
	// Dir 工作区根目录绝对路径 (只用于日志/轨迹里如实记下判定对象)。
	Dir() string
	// Exists 判断工作区内某相对路径是否存在。越界路径返回 false 而不是 panic。
	Exists(rel string) bool
	// Verify 在工作区内跑一条白名单验证命令。
	Verify(ctx context.Context, cmd VerifyCmd) VerifyResult
}

// VerifyCmd 一条验证命令。Timeout <= 0 时用 defaultVerifyTimeout。
type VerifyCmd struct {
	Name    string
	Args    []string
	Timeout time.Duration
}

// VerifyResult 验证命令的结果。
//
// TimedOut 与 Err 分开: 超时的门禁失败与"编译真的不过"在给 AI 的修复提示上是两回事
// (前者八成是生成代码里的死循环), 既有两个门禁的错误串就是这么分的。
type VerifyResult struct {
	Output   string
	TimedOut bool
	Err      error
}

// RewardVerdict 一次奖励评估的结论。
type RewardVerdict struct {
	// Pass 判定通过。
	Pass bool
	// Value 归一化 [-1,1] 的奖励值。零值时由 runRewardEvaluator 按 Pass 取 ±1。
	Value float64
	// Raw 原始值 (pass / 0-100 分 / 报错摘要), 进 rewards.jsonl 与 gate Span。
	Raw any
	// Detail 判定明细 (编译报错全文等), 进 gate Span 的 OutputRef 供蒸馏读。
	Detail string
	// Skipped 门禁未真跑 (前置条件不满足): 记轨迹但不发奖励。
	Skipped bool
	// SkipReason Skipped 为 true 时的原因, 会写进轨迹。
	SkipReason string
}

// RewardEvaluator 在原始工作区上算一个奖励 (H1)。
//
// 实现方**不发奖励也不写轨迹**, 只回结论 —— 发奖励与写 span 由
// runRewardEvaluator 统一做 (见文件头"为什么不用更直觉的做法")。
type RewardEvaluator interface {
	// Source 奖励源名, 同时用作 gate Span 的 gate/node 名。
	// 取值必须在 RewardSourceWeight 的权重表里登记过, 否则会落到 unknown 权重。
	Source() string
	// Evaluate 在 ws 上做判定。返回 error 表示评估器自己坏了 (与"门禁不通过"
	// 是两回事): 调用方按"没有证据"处理, 不发奖励。
	Evaluate(ctx context.Context, ws WorkspaceHandle) (RewardVerdict, error)
}

// ---------------------------------------------------------------------------
// 运行器: 契约与既有奖励/轨迹发射点之间的唯一接缝
// ---------------------------------------------------------------------------

// runRewardEvaluator 在 team 的工作区上跑一个评估器, 发奖励 + 写 gate Span,
// 返回门禁错误串 ("" = 通过或跳过), 语义与改造前的 runGlobalXxxGate 逐字节一致。
//
// 三处刻意保持原样 (改了就是行为变更, 而且是下游看得见的):
//   - team 为 nil 或 Cwd 为空 → 返回 "" 且**不写任何轨迹**。这条在改造前就是
//     静默返回, 加一条 span 会让既有部署的 trace 文件凭空多出行。
//   - Skipped → 写 skipped span, 不发奖励。
//   - 评估器自身出错 → 当"无证据": 记一条 skipped span (原因带上错误), 不发奖励,
//     且**返回 ""**。返回非空会把"评估器坏了"变成"门禁不通过"从而判团队失败 ——
//     观测设施故障不该杀交付 (fail-closed 的是治理, fail-open 的是交付)。
func (ptm *ProductionTeamManager) runRewardEvaluator(ctx context.Context, team *ProductionTeam, ev RewardEvaluator) string {
	if ptm == nil || team == nil || team.Cwd == "" || ev == nil {
		return ""
	}
	source := ev.Source()
	ws, err := newWorkspaceHandle(team.Cwd)
	if err != nil {
		ptm.writeGateSpan(ctx, team, gateSpanInput{
			Gate: source, Node: source, Input: team.Cwd,
			Detail: "工作区句柄不可用: " + err.Error(), Skipped: true,
		})
		return ""
	}
	start := time.Now()
	v, err := ev.Evaluate(ctx, ws)
	if err != nil {
		ptm.writeGateSpan(ctx, team, gateSpanInput{
			Gate: source, Node: source, Input: ws.Dir(),
			Detail: "评估器出错: " + err.Error(), Skipped: true, Start: start,
		})
		return ""
	}
	if v.Skipped {
		ptm.writeGateSpan(ctx, team, gateSpanInput{
			Gate: source, Node: source, Input: ws.Dir(),
			Detail: v.SkipReason, Skipped: true, Start: start,
		})
		return ""
	}
	gateErr := ""
	if !v.Pass {
		gateErr = v.Detail
		if strings.TrimSpace(gateErr) == "" {
			// 失败必须有可读理由: 空串会让 tryGateWithRemediation 拿"" 当通过。
			gateErr = source + " 未通过 (评估器未给理由)"
		}
	}
	// 发奖励 + gate Span 仍走既有的那一处 (recordGateReward), 不在这里另写一份 ——
	// 两份实现必然漂移, 而漂移的表现是"某类门禁的奖励忽然没了权重/没了 span"。
	ptm.recordGateReward(ctx, team, source, gateErr)
	return gateErr
}

// ---------------------------------------------------------------------------
// 第一批实现: 既有的两个确定性门禁
// ---------------------------------------------------------------------------

// compileGateEvaluator `go build ./...` (design/03 §4.2 价值排第 1 的确定性信号)。
type compileGateEvaluator struct{}

var _ RewardEvaluator = compileGateEvaluator{}

func (compileGateEvaluator) Source() string { return RewardSourceGateCompile }

func (compileGateEvaluator) Evaluate(_ context.Context, ws WorkspaceHandle) (RewardVerdict, error) {
	// 兜底: 无 go.mod 的目录不是 Go 模块, go build ./... 必然报 "no main module", 跳过。
	if !ws.Exists("go.mod") {
		return RewardVerdict{Skipped: true, SkipReason: "无 go.mod, 非 Go 模块, 跳过编译门禁"}, nil
	}
	// 执行超时刻意不挂在上游 ctx 上 (保持原语义: 门禁自己限时, 不受上游取消影响);
	// 上游 ctx 只用来取 trace RunID 给奖励事件归因 —— 那一步在 recordGateReward 里。
	res := ws.Verify(context.Background(), VerifyCmd{
		Name: "go", Args: []string{"build", "./..."}, Timeout: 5 * time.Minute,
	})
	switch {
	case res.TimedOut:
		return RewardVerdict{Raw: "timeout", Detail: fmt.Sprintf(
			"go build ./... timed out after 5m\n%s", res.Output)}, nil
	case res.Err != nil:
		return RewardVerdict{Raw: truncateResult(res.Output, 300), Detail: fmt.Sprintf(
			"go build ./... failed: %v\n%s", res.Err, res.Output)}, nil
	}
	return RewardVerdict{Pass: true, Raw: "pass"}, nil
}

// testGateEvaluator `go test -race -timeout 120s ./...`。
type testGateEvaluator struct{}

var _ RewardEvaluator = testGateEvaluator{}

func (testGateEvaluator) Source() string { return RewardSourceGateTest }

func (testGateEvaluator) Evaluate(_ context.Context, ws WorkspaceHandle) (RewardVerdict, error) {
	// 12 分钟: 足够正常单元测试跑完, 又能在 AI 生成死循环 (如 kmeans 中 k > len(vectors)
	// 的 for{}) 时及时止血。
	res := ws.Verify(context.Background(), VerifyCmd{
		Name: "go", Args: []string{"test", "-race", "-timeout", "120s", "./..."}, Timeout: 12 * time.Minute,
	})
	switch {
	case res.TimedOut:
		return RewardVerdict{Raw: "timeout", Detail: fmt.Sprintf(
			"go test -race ./... timed out after 12m (very likely an infinite loop in generated code)\n%s",
			res.Output)}, nil
	case res.Err != nil:
		return RewardVerdict{Raw: truncateResult(res.Output, 300), Detail: fmt.Sprintf(
			"go test -race ./... failed: %v\n%s", res.Err, res.Output)}, nil
	}
	return RewardVerdict{Pass: true, Raw: "pass"}, nil
}

// ---------------------------------------------------------------------------
// WorkspaceHandle 的 file 实现
// ---------------------------------------------------------------------------

// defaultVerifyTimeout 未指定 Timeout 时的兜底。
// 不设"无超时"这个档: 一个没有超时的验证命令能把团队卡到租约过期。
const defaultVerifyTimeout = 5 * time.Minute

// verifyAllowlist 允许在工作区里跑的验证命令。
//
// 值是允许的子命令集合; 空集合 = 该程序的任意参数都允许 (gofmt 只有一种用法)。
// 加条目前先问一句: 它会不会**执行工作区里的代码**或**联网**? 会就别加 ——
// 前者让奖励可被生成代码操纵, 后者让"确定性门禁"的结论取决于网络。
var verifyAllowlist = map[string]map[string]bool{
	"go":    {"build": true, "test": true, "vet": true, "list": true},
	"gofmt": {},
}

// workspaceFS 基于本地文件系统的 WorkspaceHandle。
type workspaceFS struct{ root string }

var _ WorkspaceHandle = (*workspaceFS)(nil)

// newWorkspaceHandle 造一个钳在 dir 内的只读句柄。
//
// 构造时就 EvalSymlinks 定死 root: 之后每次 resolve 都拿真实路径比前缀,
// 否则 `<cwd>` 本身是软链时任何相对路径都会被判越界 (macOS 的 /tmp 就是这样)。
func newWorkspaceHandle(dir string) (WorkspaceHandle, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("工作区路径为空")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("工作区 %s 不可用: %w", abs, err)
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("工作区 %s 不是目录", real)
	}
	return &workspaceFS{root: real}, nil
}

func (w *workspaceFS) Dir() string { return w.root }

// resolve 把相对路径解析成工作区内的绝对路径; 越界或绝对路径一律拒绝。
//
// 两道闸都要: `filepath.Clean` 挡 `../..` 这类字面越界, `EvalSymlinks` 挡"工作区里
// 一条指向 /etc 的软链"。第二道对不存在的路径会失败, 那时退回按 Clean 后的字面
// 结果判前缀 —— 不存在的路径读不出内容, 判前缀足够。
func (w *workspaceFS) resolve(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("路径为空")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("拒绝绝对路径 %q (只读句柄只接受工作区内相对路径)", rel)
	}
	joined := filepath.Clean(filepath.Join(w.root, rel))
	if !withinRoot(w.root, joined) {
		return "", fmt.Errorf("路径 %q 越出工作区", rel)
	}
	if real, err := filepath.EvalSymlinks(joined); err == nil {
		if !withinRoot(w.root, real) {
			return "", fmt.Errorf("路径 %q 经软链越出工作区", rel)
		}
		return real, nil
	}
	return joined, nil
}

// withinRoot p 是否在 root 之内 (含 root 自身)。按路径分隔符比较, 不是裸前缀 ——
// 裸前缀会把 `/ws-evil` 当成 `/ws` 的子路径。
func withinRoot(root, p string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(os.PathSeparator))
}

func (w *workspaceFS) Exists(rel string) bool {
	p, err := w.resolve(rel)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Verify 跑一条白名单命令; 不在白名单里的直接返回 error, 不执行。
func (w *workspaceFS) Verify(ctx context.Context, c VerifyCmd) VerifyResult {
	subs, ok := verifyAllowlist[c.Name]
	if !ok {
		return VerifyResult{Err: fmt.Errorf("命令 %q 不在验证白名单内 (只读句柄拒绝执行)", c.Name)}
	}
	if len(subs) > 0 {
		if len(c.Args) == 0 || !subs[c.Args[0]] {
			sub := ""
			if len(c.Args) > 0 {
				sub = c.Args[0]
			}
			return VerifyResult{Err: fmt.Errorf("%s 的子命令 %q 不在白名单内 (只允许 %s)",
				c.Name, sub, allowedSubs(subs))}
		}
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultVerifyTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, c.Name, c.Args...)
	cmd.Dir = w.root
	out, err := cmd.CombinedOutput()
	return VerifyResult{
		Output:   string(out),
		TimedOut: cctx.Err() == context.DeadlineExceeded,
		Err:      err,
	}
}

func allowedSubs(subs map[string]bool) string {
	names := make([]string, 0, len(subs))
	for s := range subs {
		names = append(names, s)
	}
	sort.Strings(names)
	return strings.Join(names, "/")
}
