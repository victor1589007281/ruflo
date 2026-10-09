package coder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Runner 长程编码运行时。
type Runner struct {
	Config Config
	Agent  Agent
	Verify Verifier
	// Now 可注入的时钟 (测试里需要确定的顺序)。
	Now func() time.Time
	// Logf 进度输出, 默认打到 stderr。
	Logf func(format string, args ...any)
	// Sleep 可注入 (测试里不真等)。
	Sleep func(d time.Duration)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func (r *Runner) sleep(d time.Duration) {
	if r.Sleep != nil {
		r.Sleep(d)
		return
	}
	time.Sleep(d)
}

// statePath 状态文件路径。
func statePath(ws string) string { return filepath.Join(ws, RuntimeDir, stateFile) }

// LoadState 读取已有状态。第二个返回值为 false 表示"没有历史, 全新开始"。
//
// 显式区分"没有状态文件"与"状态文件坏了": 前者是正常冷启动, 后者是异常,
// 但**两者都不该让任务停下** —— 状态是可重建的缓存, 世界(工作区+验证命令)
// 才是真相。最坏情况是重跑一轮。
func LoadState(ws string) (State, bool, error) {
	b, err := os.ReadFile(statePath(ws))
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, false, nil
		}
		return State{}, false, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		// 坏状态不致命: 当作没有状态, 从头开始 (Verify 会重新测量世界)。
		return State{}, false, fmt.Errorf("状态文件损坏, 将从头开始: %w", err)
	}
	return st, true, nil
}

// saveState 原子落盘: 先写临时文件, 再 rename。
//
// 直接 WriteFile 会让"写到一半断电"留下一个截断的 JSON —— 下次启动读不出来,
// 而**读不出来**与"从没跑过"在下游是同一个分支 (都会从头开始), 于是断点恢复
// 会静默退化成重跑, 且没有任何错误信息。rename 在同一文件系统上是原子的。
func saveState(ws string, st State) error {
	dir := filepath.Join(ws, RuntimeDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	st.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(ws) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(ws))
}

// Result 一次长程任务的结果。
type Result struct {
	State State
	// Passed 终态验收结论 —— 在导出副本里独立复跑得出。
	Passed bool
	// VerifyOutput 终态验收的原始输出。
	VerifyOutput string
	// CleanDir 终态验收用的干净目录 (留给调用方复查)。
	CleanDir string
	// Reason 结束原因: done / max_iterations / no_progress / verify_error / agent_error。
	Reason string
}

// Run 跑一次长程任务, 支持从断点恢复。
//
// 主循环的每一步都刻意"先测量、再行动": 每轮开头**先跑一次验证**。这带来两个性质:
//
//   - **恢复天然正确**: 上一轮无论怎么死的 (崩溃/被杀/断电), 新进程第一件事
//     就是重新测量世界, 而不是相信落盘的自述;
//   - **空转可判**: 把"这轮改动前的验证输出"与"这轮结束后的验证输出"比一下,
//     再加上工作区指纹, 就能区分"做了但没成功"与"什么都没做"。
func (r *Runner) Run(ctx context.Context) (Result, error) {
	cfg := r.Config
	var res Result
	if cfg.Workspace == "" {
		return res, fmt.Errorf("未指定工作区")
	}
	if r.Agent == nil {
		return res, fmt.Errorf("未指定 Agent")
	}
	if r.Verify == nil {
		return res, fmt.Errorf("未指定 Verifier —— 没有客观终态就无法判定任务完成")
	}
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		return res, err
	}

	st, existed, lerr := LoadState(cfg.Workspace)
	if lerr != nil {
		r.logf("[coder] %v", lerr)
	}
	if !existed {
		st = State{
			Goal:      cfg.Goal,
			Workspace: cfg.Workspace,
			VerifyCmd: cfg.VerifyCmd,
			Phase:     PhaseCoding,
			StartedAt: r.now(),
		}
	} else {
		// 目标变了等于换了任务: 旧状态不适用, 但工作区保留 (产物可能还有用)。
		if st.Goal != cfg.Goal {
			r.logf("[coder] 目标与落盘状态不一致, 按新目标重开 (工作区保留)")
			st = State{
				Goal:      cfg.Goal,
				Workspace: cfg.Workspace,
				VerifyCmd: cfg.VerifyCmd,
				Phase:     PhaseCoding,
				StartedAt: r.now(),
			}
		} else {
			r.logf("[coder] 从断点恢复: 已完成 %d 轮, 连续空转 %d 轮",
				st.Iteration, st.NoProgressStreak)
		}
	}
	// resumed: 这一次进程是不是"接着上一次的进度跑"。
	//
	// 它是**整个 Run 调用**的属性, 不是"这一轮之后的那一轮"的属性 —— 所以
	// 在循环里每次迭代后把它置真会让去重失效 (第一轮之后全是 true)。
	// 实测踩过: 一个进程内连跑 4 轮, 后 3 轮全被标成 Resumed。
	resumed := existed && st.Iteration > 0

	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = 20
	}

	var lastFingerprint, _ = Fingerprint(cfg.Workspace)

	for st.Iteration < maxIter {
		if err := ctx.Err(); err != nil {
			res.State, res.Reason = st, "canceled"
			return res, err
		}
		st.Iteration++
		iter := st.Iteration
		started := r.now()

		// ── 1) 每轮先测量世界 ──────────────────────────────────────────
		// 放在 agent 动手**之前**是刻意的: 上一轮的产物可能已经让任务过了
		// (尤其在被杀掉之后), 这时不该再烧一轮。
		passed, vout, verr := r.Verify.Verify(ctx, cfg.Workspace)
		if verr != nil {
			// 验证命令跑不起来是环境问题, 不是"还没写好"。停下并说清楚 ——
			// 继续迭代只会把同一个环境错误重复 N 遍。
			st.Phase = PhaseFailed
			rec := IterationRecord{
				N: iter, StartedAt: started, EndedAt: r.now(),
				Err: "验证命令无法执行: " + verr.Error(), Resumed: resumed,
			}
			st.History = append(st.History, rec)
			_ = saveState(cfg.Workspace, st)
			res.State, res.Reason = st, "verify_error"
			return res, fmt.Errorf("验证命令无法执行 (环境问题, 不是代码问题): %w", verr)
		}
		if passed {
			st.Phase = PhaseDone
			st.VerifyPassed = true
			st.LastVerifyOutput = vout
			rec := IterationRecord{
				N: iter, StartedAt: started, EndedAt: r.now(),
				VerifyPassed: true, VerifyOutput: truncate(vout, 8000), Resumed: resumed,
			}
			st.History = append(st.History, rec)
			_ = saveState(cfg.Workspace, st)
			r.logf("[coder] 第 %d 轮: 验证已通过", iter)
			return r.finalize(ctx, st, res)
		}

		// ── 2) 组装 prompt 并跑一轮有界的 agent ────────────────────────
		prompt := buildPrompt(cfg, st, vout, !resumed && iter == 1)
		// 走接口而不是类型断言: 断言不中时是**静默**失效 (prompt 不落盘、
		// agent 拿不到本轮指示, 而没有任何报错)。实测踩过一次。
		r.Agent.SetArtifacts(
			filepath.Join(cfg.Workspace, RuntimeDir, promptFile),
			filepath.Join(cfg.Workspace, RuntimeDir, logDir, fmt.Sprintf("iter-%03d", iter)),
		)

		ictx := ctx
		var cancel context.CancelFunc
		if cfg.IterationTimeout > 0 {
			ictx, cancel = context.WithTimeout(ctx, cfg.IterationTimeout)
		}
		ares, aerr := r.Agent.Run(ictx, prompt)
		if cancel != nil {
			cancel()
		}

		rec := IterationRecord{
			N: iter, StartedAt: started,
			ExitCode: ares.ExitCode, IsError: ares.IsError,
			SessionID: ares.SessionID, Final: truncate(ares.Final, 2000),
			Resumed: resumed,
		}
		if aerr != nil {
			rec.Err = aerr.Error()
		}

		// ── 3) 驱动侧自己验一次 (唯一的成功判据) ────────────────────────
		passedAfter, voutAfter, verrAfter := r.Verify.Verify(ctx, cfg.Workspace)
		if verrAfter != nil {
			rec.Err = strings.TrimSpace(rec.Err + " | 验证命令无法执行: " + verrAfter.Error())
			rec.EndedAt = r.now()
			st.History = append(st.History, rec)
			_ = saveState(cfg.Workspace, st)
			st.Phase = PhaseFailed
			res.State, res.Reason = st, "verify_error"
			return res, fmt.Errorf("验证命令无法执行: %w", verrAfter)
		}
		rec.VerifyPassed = passedAfter
		rec.VerifyOutput = truncate(voutAfter, 8000)

		fp, _ := Fingerprint(cfg.Workspace)
		rec.FingerprintChanged = fp != lastFingerprint
		lastFingerprint = fp
		rec.EndedAt = r.now()

		// ── 4) 空转判定 ────────────────────────────────────────────────
		// "没动手"= 工作区没变 且 验证输出也没变。只判指纹会漏掉"改了又改回去",
		// 只判输出会漏掉"改了但报错一样"。
		if !rec.DidWork() && normalizeOutput(voutAfter) == normalizeOutput(vout) {
			st.NoProgressStreak++
		} else {
			st.NoProgressStreak = 0
		}
		st.LastVerifyOutput = voutAfter
		st.LastFingerprint = fp
		st.History = append(st.History, rec)
		if err := saveState(cfg.Workspace, st); err != nil {
			// 状态落不下去就继续跑下去是危险的 (断点恢复会失真)。
			return res, fmt.Errorf("状态落盘失败, 中止: %w", err)
		}

		r.logf("[coder] 第 %d 轮: 验证=%v 工作区变化=%v 空转连击=%d",
			iter, passedAfter, rec.FingerprintChanged, st.NoProgressStreak)

		if passedAfter {
			st.Phase = PhaseDone
			st.VerifyPassed = true
			_ = saveState(cfg.Workspace, st)
			return r.finalize(ctx, st, res)
		}

		limit := cfg.NoProgressLimit
		if limit <= 0 {
			limit = 3
		}
		if st.NoProgressStreak >= limit {
			// 这是本次实测那类静默失败的止损点: agent 每轮都"正常结束"、
			// 报一段像样的话, 却一个字节没改。没有这条就会一路烧到迭代额度。
			st.Phase = PhaseFailed
			_ = saveState(cfg.Workspace, st)
			res.State, res.Reason = st, "no_progress"
			res.VerifyOutput = voutAfter
			return res, fmt.Errorf(
				"连续 %d 轮既没改动工作区也没改变验证输出 —— agent 没有真的动手, 提前止损 "+
					"(最后一轮的自我报告: %s)", st.NoProgressStreak, truncate(ares.Final, 300))
		}

		if aerr != nil {
			// 单轮失败不致命: 下一轮会带着原始报错重来。但要退避一下,
			// 否则"上游持续 5xx"会变成高频重试把额度烧光。
			r.sleep(2 * time.Second)
		}
	}

	st.Phase = PhaseFailed
	_ = saveState(cfg.Workspace, st)
	res.State, res.Reason = st, "max_iterations"
	res.VerifyOutput = st.LastVerifyOutput
	return res, fmt.Errorf("迭代额度用尽 (%d 轮) 仍未通过验证", maxIter)
}

// finalize 终态验收: 在导出副本里**独立**复跑验证。
//
// 为什么不在原目录里判: 原目录里有 .coder (会话/日志/状态), 依赖它的验证会
// 假装通过; 而且"agent 自己跑过一次"与"我们独立跑一次"是两件事 —— 后者才是
// "最后的程序可以正常运行"这句话的证据。
func (r *Runner) finalize(ctx context.Context, st State, res Result) (Result, error) {
	res.State = st
	res.Passed = true
	res.Reason = "done"

	clean := filepath.Join(os.TempDir(), fmt.Sprintf("coder-clean-%d", r.now().UnixNano()))
	if err := os.RemoveAll(clean); err != nil {
		return res, err
	}
	if err := ExportClean(r.Config.Workspace, clean); err != nil {
		// 导出失败不该把"任务已完成"改写成"任务失败" —— 它是验收手段的问题。
		r.logf("[coder] 警告: 导出干净副本失败, 终态验收退回原目录: %v", err)
		return res, nil
	}
	res.CleanDir = clean

	passed, out, err := r.Verify.Verify(ctx, clean)
	res.VerifyOutput = out
	if err != nil {
		res.Passed = false
		res.Reason = "clean_verify_error"
		return res, fmt.Errorf("干净副本里验证命令跑不起来: %w", err)
	}
	if !passed {
		// 原目录过、副本不过 ⇒ 产物依赖了运行时目录。这是真实缺陷, 必须报出来,
		// 不能因为"原目录过了"就放行。
		res.Passed = false
		res.Reason = "clean_verify_failed"
		return res, fmt.Errorf("原目录验证通过, 但**干净副本**里没通过 —— "+
			"代码依赖了 %s/ 里的东西 (日志/状态/临时产物), 这个程序不能独立运行", RuntimeDir)
	}
	r.logf("[coder] 终态验收通过: 干净副本 %s", clean)
	return res, nil
}

// Report 人可读的一次长程任务报告。
func (r Result) Report() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "任务: %s\n", r.State.Goal)
	fmt.Fprintf(&sb, "结果: %v (原因: %s)\n", r.Passed, r.Reason)
	fmt.Fprintf(&sb, "轮数: %d\n", r.State.Iteration)
	fmt.Fprintf(&sb, "判据: %s\n\n", strings.Join(r.State.VerifyCmd, " "))
	sb.WriteString("逐轮读数:\n")
	for _, it := range r.State.History {
		fmt.Fprintf(&sb, "  #%-3d 验证=%-5v 工作区变化=%-5v 退出码=%-3d 空转=%s\n",
			it.N, it.VerifyPassed, it.FingerprintChanged, it.ExitCode,
			boolMark(!it.DidWork()))
	}
	if r.CleanDir != "" {
		fmt.Fprintf(&sb, "\n干净副本: %s\n", r.CleanDir)
	}
	return sb.String()
}

func boolMark(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
