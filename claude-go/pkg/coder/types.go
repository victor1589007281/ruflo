// Package coder —— 一个长程自主编码 agent 的运行时。
//
// # 它补的是哪一层
//
// `claude-go run` 本身已经是 agent 了 (会读文件、改代码、跑命令), 但它的一次运行
// 是**一次性**的: 跑完就结束, 没有"这一轮没做完, 下一轮接着做"的结构。本包把
// 一次性的 run 变成**有状态的循环**:
//
//	iteration N:  组装 prompt (目标 + 计划 + 进度 + 上轮原始报错)
//	              → 起一个**有界**的 claude-go 进程
//	              → 驱动侧**自己**跑验证命令
//	              → 把原始输出 + 状态落盘
//	iteration N+1: 从落盘状态恢复 ...
//
// # 为什么判据必须落在"驱动侧自己跑验证"上
//
// 实测过一条会造成假绿的路径 (2026-09-24, opencode:space-bunny-free):
// 引擎发往上游的请求被判 400, 这个 400 被吞掉之后 run 以
// `exit=0 / is_error=false / final="我先看看项目结构和测试文件。"` 收尾 ——
// **一行代码都没改, 而所有"成功"信号都是真的**。所以:
//
//   - Agent 的退出码与 is_error 只用来**报错**, 绝不用来判成功;
//   - 唯一的成功判据是 Verify 的退出码, 且由驱动侧执行;
//   - 每个 iteration 开始时**先验一次** —— 于是"恢复"天然正确:
//     上一轮无论怎么死的, 新进程的第一件事就是重新测量世界。
//
// # 为什么不用 claude-go 自己的 --resume / --verify
//
//   - `--resume` 是有损的: transcript 不存 tool_result 块
//     (pkg/session/statestore_transcript.go), 恢复出来的会话缺一半上下文。
//   - `--verify` 只在整轮结束后跑一次, 且**不给模型任何反馈** ——
//     模型看不到失败输出, 就没法自我修正。自我修正需要反馈闭环,
//     闭环必须在驱动侧。
//
// 所以本包的状态载体是**文件系统上的产物** (state.json / PLAN.md / PROGRESS.md /
// 工作区里的代码), 而不是会话内存。这三者都能被下一个进程原样读回。
package coder

import "time"

// Phase 一次长程任务当前处于哪个阶段。
type Phase string

const (
	// PhaseCoding 正在让 agent 改代码。
	PhaseCoding Phase = "coding"
	// PhaseDone 验证通过, 任务完成。
	PhaseDone Phase = "done"
	// PhaseFailed 迭代额度用尽或触发止损, 任务未完成。
	PhaseFailed Phase = "failed"
)

// Config 一次长程任务的全部输入。
type Config struct {
	// Goal 任务目标 (人话, 交给 agent 的意图)。稳定不变 → 放在 prompt 最前面。
	Goal string
	// Workspace 工作目录。agent 改的就是这里, 验证也在这里跑。
	Workspace string
	// VerifyCmd 客观终态: 退出码 0 = 成功。**唯一**的成功判据。
	//
	// 它来自配置而不是工作区 —— agent 改不到它, 也就无法把"判据"改成
	// 自己能过的东西。这一点是刻意的: 判据与产物必须分开存放。
	VerifyCmd []string

	// Bin claude-go 可执行文件路径。
	Bin string
	// Model 模型别名 (如 opencode:space-bunny-free)。
	Model string
	// ConfigPath claude-go 的 --config。空则用 claude-go 自己的发现顺序。
	ConfigPath string
	// Gateway CLAUDE_GO_LLM_GATEWAY。opencode 系模型必须经网关 (直连时
	// claude-go 发的是带 provider 前缀的别名, 上游只认裸名 → 400)。
	Gateway string
	// AllowedTools 工具白名单 (claude-go --allowed-tools)。
	AllowedTools []string

	// MaxTurnsPerIteration 单轮的回合上限。**必须有限** —— 单轮无界会让
	// 一次进程吃掉整个预算, 而且落盘的粒度变成"整轮", 断点粒度太粗。
	MaxTurnsPerIteration int
	// MaxIterations 最多迭代几轮。
	MaxIterations int
	// NoProgressLimit 连续多少轮"工作区指纹没变 且 验证输出没变"就止损。
	//
	// 这条挡的正是那次实测的静默失败: agent 每轮都正常结束、报个像样的话,
	// 但工作区一个字节没动。只看退出码的实现会一路跑到迭代额度用尽,
	// 然后报一句"没成功", 而看不出"它其实什么都没做"。
	NoProgressLimit int
	// IterationTimeout 单轮墙钟上限。
	IterationTimeout time.Duration
	// VerifyTimeout 验证命令的墙钟上限。
	VerifyTimeout time.Duration
}

// IterationRecord 一轮迭代的读数。
type IterationRecord struct {
	N         int       `json:"n"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	// ExitCode / IsError 只用于诊断, 不参与成功判定 (见包注释)。
	ExitCode  int    `json:"exit_code"`
	IsError   bool   `json:"is_error"`
	SessionID string `json:"session_id"`
	// Err 驱动侧观察到的错误 (进程起不来/超时/输出解析不了)。
	Err string `json:"err,omitempty"`
	// Final 模型这一轮的最后一段话 (截断), 给人看的。
	Final string `json:"final,omitempty"`
	// VerifyPassed 这一轮跑完驱动侧自己验的结果 —— 唯一的成功判据。
	VerifyPassed bool `json:"verify_passed"`
	// VerifyOutput 验证命令的原始输出 (截断)。下一轮的反馈就是它。
	VerifyOutput string `json:"verify_output,omitempty"`
	// FingerprintChanged 这一轮结束时工作区指纹是否变化过。
	//
	// 它和 VerifyPassed 一起构成"agent 到底动没动手"的判据: 两者皆否 ⇒
	// 这一轮是空转 (无论它自己说得多么像做完了)。
	FingerprintChanged bool `json:"fingerprint_changed"`
	// Resumed 这一轮是不是从落盘状态恢复后开跑的。
	Resumed bool `json:"resumed"`
}

// State 落盘的任务状态。
//
// 它是**可重建的缓存**而不是唯一真相: 唯一真相是工作区里的代码 + 验证命令的退出码。
// 所以状态文件损坏/被 agent 改坏都不会让任务做错 (最坏是重跑一轮), 因为每个
// iteration 开始时都会重新验一次世界。这个性质是刻意设计的。
type State struct {
	Goal      string   `json:"goal"`
	Workspace string   `json:"workspace"`
	VerifyCmd []string `json:"verify_cmd"`

	Phase        Phase `json:"phase"`
	Iteration    int   `json:"iteration"`
	VerifyPassed bool  `json:"verify_passed"`

	// NoProgressStreak 连续空转轮数。
	NoProgressStreak int `json:"no_progress_streak"`
	// LastFingerprint 上一轮结束时的工区指纹 (用来判"动没动手")。
	LastFingerprint string `json:"last_fingerprint"`
	// LastVerifyOutput 上一轮的原始验证输出 (下一轮的反馈来源)。
	LastVerifyOutput string `json:"last_verify_output"`

	StartedAt time.Time         `json:"started_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	History   []IterationRecord `json:"history"`
}

// LastN 最近 n 轮的记录 (n<=0 表示全部)。报告用。
func (s State) LastN(n int) []IterationRecord {
	if n <= 0 || n > len(s.History) {
		return s.History
	}
	return s.History[len(s.History)-n:]
}

// DidWork 本轮的"agent 真的动了手"判据: 验证过了 **或** 工作区变了。
//
// 注意"验证过了"也算动手 —— 那是唯一真正重要的动手。
func (r IterationRecord) DidWork() bool {
	return r.VerifyPassed || r.FingerprintChanged
}
