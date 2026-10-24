package coder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AgentResult 一次有界编码尝试的读数。
//
// 注意字段的用途: ExitCode / IsError 描述的是**进程**, 不是**任务**。实测证明
// 一个什么都没做的 run 可以是 exit=0 / is_error=false, 所以调用方不得用它们判成功。
type AgentResult struct {
	ExitCode  int
	IsError   bool
	SessionID string
	Final     string
	// Raw stderr+stdout 尾部, 出错时用来归因 (进程起不来的原因通常在这)。
	Raw string
}

// Agent 一轮有界的编码尝试。抽成接口是为了让运行时在**不起真实 run** 的前提下
// 被单测 —— 恢复逻辑/止损逻辑/落盘时机都是纯逻辑, 不该等一次真实 LLM 调用才发现错。
type Agent interface {
	Run(ctx context.Context, prompt string) (AgentResult, error)
	// SetArtifacts 告诉实现把本轮产物落到哪 (prompt 正文、stdout/stderr)。
	//
	// 放**接口里**而不是当成 *ClaudeGoAgent 的可选能力, 是因为可选能力失败时
	// 是静默的: 类型断言不中 ⇒ 什么都不设 ⇒ prompt 不落盘 ⇒ agent 拿不到本轮
	// 指示 ⇒ 表现成"agent 什么都不干", 而没有任何一处报错。实测踩过一次
	// (值接收者 + 指针断言)。
	//
	// 实现可以忽略它 (测试替身就不需要落盘), 但"忽略"必须是个明确的选择。
	SetArtifacts(promptFile, logDir string)
}

// AgentFunc 适配普通函数。
type AgentFunc func(ctx context.Context, prompt string) (AgentResult, error)

// Run 见 Agent。
func (f AgentFunc) Run(ctx context.Context, prompt string) (AgentResult, error) {
	return f(ctx, prompt)
}

// SetArtifacts 见 Agent。纯函数没有产物目录, 显式忽略。
func (f AgentFunc) SetArtifacts(string, string) {}

// SetArtifacts 见 Agent。
func (a *ClaudeGoAgent) SetArtifacts(promptFile, logDir string) {
	a.PromptFile = promptFile
	a.LogDir = logDir
}

// ClaudeGoAgent 通过 claude-go CLI 跑一轮。
//
// 注意 Run 用的是**指针**接收者: 运行时每一轮都会改写 PromptFile / LogDir
// (见 setPromptFile), 值接收者会让那两次赋值落在一个马上被丢掉的副本上 ——
// 表现是 prompt 永远不落盘、agent 拿不到本轮指示, 而且**不报任何错**。
type ClaudeGoAgent struct {
	Bin          string
	Model        string
	ConfigPath   string
	Gateway      string
	Cwd          string
	AllowedTools []string
	MaxTurns     int
	// PromptFile 落盘 prompt 的位置 (取证 + 断点可读)。
	// 不走 stdin: claude-go run 只接受位置参数, 而超长 prompt 走 argv 会撞
	// argv 长度上限 —— 长程任务的 prompt 一定会变长。
	PromptFile string
	// LogDir 每轮的 stdout/stderr 落盘目录。
	LogDir string
	// RunLabel 本次运行在轨迹页上的显示名 (透传给 `claude-go run --run-label`)。
	//
	// 为什么需要它: 驱动起的每一轮都是独立的一次 `claude-go run`, 各自落一个
	// trace-run-* 桶。不标注的话, 这些运行在轨迹页上只有一串 session 哈希 ——
	// 长程任务跑到第 8 轮时, 没人能把"这 8 个桶"和"我在跑的那个任务"对上。
	//
	// 标注粒度是**任务**而不是轮次: 每一轮本来就是独立文件, 靠时长与 span 数分得开;
	// 而按任务名一致, 反而让"这个任务跑过的所有轮次"一次搜全。
	RunLabel string
}

// Run 见 Agent。
func (a *ClaudeGoAgent) Run(ctx context.Context, prompt string) (AgentResult, error) {
	var res AgentResult
	if a.Bin == "" {
		return res, fmt.Errorf("claude-go 可执行文件未指定")
	}
	// prompt 落盘: 进程的 argv 有长度上限, 长程 prompt 迟早撞上; 而且落盘之后
	// "这一轮到底喂了什么"是可复查的 (取证)。
	if a.PromptFile != "" {
		if err := os.MkdirAll(filepath.Dir(a.PromptFile), 0o755); err != nil {
			return res, fmt.Errorf("建 prompt 目录失败: %w", err)
		}
		if err := os.WriteFile(a.PromptFile, []byte(prompt), 0o644); err != nil {
			return res, fmt.Errorf("写 prompt 失败: %w", err)
		}
	}
	// 位置参数仍要传 (CLI 需要它非空), 但传一个短的指路语: 正文在文件里。
	shortPrompt := "请严格按 .coder/prompt.md 里的指示执行本轮工作。"

	args := []string{
		"run", shortPrompt,
		"--cwd", a.Cwd,
		"--print",
		"--permission-mode", "bypass",
		"--output-format", "json",
		"--final-only",
		"--emit-session-id",
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.ConfigPath != "" {
		args = append(args, "--config", a.ConfigPath)
	}
	if a.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprint(a.MaxTurns))
	}
	if len(a.AllowedTools) > 0 {
		args = append(args, "--allowed-tools", strings.Join(a.AllowedTools, ","))
	}
	// 空标签不传旗标, 而不是传一个空串: 空串在 CLI 侧与"没传"走同一条回落
	// (用 session id), 但多一个显式的空 argv 会让"这一轮没标签"和"标签就是空"
	// 在日志里长得不一样, 徒增解释成本。
	if a.RunLabel != "" {
		args = append(args, "--run-label", a.RunLabel)
	}

	cmd := exec.CommandContext(ctx, a.Bin, args...)
	cmd.Dir = a.Cwd
	cmd.Env = os.Environ()
	if a.Gateway != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_GO_LLM_GATEWAY="+a.Gateway)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			// 进程根本没起来 (二进制不存在/权限不对)。归因留给调用方,
			// 但要把它和"跑完了但退出码非零"区分开。
			res.Raw = truncate(stderr.String()+"\n"+stdout.String(), 4000)
			return res, fmt.Errorf("启动 claude-go 失败: %w", err)
		}
	}
	res.ExitCode = code

	if a.LogDir != "" {
		_ = os.MkdirAll(a.LogDir, 0o755)
		_ = os.WriteFile(filepath.Join(a.LogDir, "stdout.log"), stdout.Bytes(), 0o644)
		_ = os.WriteFile(filepath.Join(a.LogDir, "stderr.log"), stderr.Bytes(), 0o644)
	}

	env, ok := parseEnvelope(stdout.String())
	if ok {
		res.IsError = env.IsError
		res.SessionID = env.SessionID
		res.Final = env.Final
	}
	res.Raw = truncate(stderr.String(), 4000)
	return res, nil
}

// envelope claude-go --output-format json 的顶层结构。
type envelope struct {
	Final     string `json:"final"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
}

// parseEnvelope 从 stdout 里取出 JSON envelope。
//
// 为什么要"从后往前逐行试"而不是直接 Unmarshal 整个 stdout: `--final-only` 之下
// stdout 里仍会有别的前缀行 (实测有 "[computer-use] 已注册 1 个工具 ..."), 而且
// 模型自己输出的内容也可能带花括号。真正可靠的判据是"这一行是一个带 session_id
// 或 final 的 JSON 对象"。
func parseEnvelope(out string) (envelope, bool) {
	var env envelope
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var cand envelope
		if json.Unmarshal([]byte(line), &cand) != nil {
			continue
		}
		// 必须有 envelope 的标志性字段之一, 否则可能只是碰巧是 JSON 的一行。
		if cand.SessionID == "" && cand.Final == "" && !strings.Contains(line, "is_error") {
			continue
		}
		return cand, true
	}
	return env, false
}

// RuntimeDir 任务运行时的落盘目录 (相对工作区)。
const RuntimeDir = ".coder"

// 运行时写给 agent 的辅助文件。
const (
	// PlanFile agent 自己维护的计划。
	PlanFile = "PLAN.md"
	// ProgressFile agent 自己维护的进度 (每轮更新, 是跨轮记忆的主要载体)。
	ProgressFile = "PROGRESS.md"
	// promptFile 本轮实际喂进去的 prompt (取证)。
	promptFile = "prompt.md"
	// stateFile 状态。
	stateFile = "state.json"
	// logDir 每轮输出。
	logDir = "logs"
)

// buildPrompt 组装一轮的 prompt。
//
// 三段式的理由:
//
//  1. **目标在最前且逐轮不变** —— 前缀稳定, 上游提示缓存才有机会命中。
//  2. **中间是 agent 自己写的计划与进度** —— 这是跨轮记忆。运行时不去"总结"
//     agent 的历史 (那会引入一个会漂移的转述), 而是让它自己写、自己读回来。
//  3. **最后才是每一轮都会变的那段** (上一轮的原始报错) —— 按"逐轮变化的文字
//     必须放在最后"的纪律, 前面的前缀才不会被它打散。
func buildPrompt(cfg Config, st State, lastVerifyOut string, firstRun bool) string {
	var sb strings.Builder
	sb.WriteString("# 任务\n\n")
	sb.WriteString(strings.TrimSpace(cfg.Goal))
	sb.WriteString("\n\n")

	sb.WriteString("# 完成判据 (机器判定, 不接受自我报告)\n\n")
	sb.WriteString("你的工作是否完成, 由下面这条命令的**退出码**决定:\n\n")
	fmt.Fprintf(&sb, "```\n%s\n```\n\n", strings.Join(cfg.VerifyCmd, " "))
	sb.WriteString("退出码 0 才算完成。你自称「做完了」不算数 —— 判据是这条命令。\n\n")

	sb.WriteString("# 你必须维护的两个文件\n\n")
	fmt.Fprintf(&sb, "- `%s/%s` —— 计划。开工前先写下来, 之后可修订。\n", RuntimeDir, PlanFile)
	fmt.Fprintf(&sb, "- `%s/%s` —— 进度。**每轮结束前必须更新**, 写清: 已完成什么、"+
		"下一步做什么、已知的坑。它是你唯一的跨轮记忆 —— 下一个进程看不到你现在的对话。\n\n", RuntimeDir, ProgressFile)

	sb.WriteString("# 工作方式\n\n")
	sb.WriteString("1. 先读 `" + RuntimeDir + "/" + ProgressFile + "` 和 `" + RuntimeDir + "/" + PlanFile + "` (若存在), 从中恢复上下文。\n")
	sb.WriteString("2. 用 Read/Grep/Glob 搞清楚现状, 不要凭猜测改代码。\n")
	sb.WriteString("3. 动手实现。一次改小而完整的一步, 别把大改动堆在一起。\n")
	sb.WriteString("4. **自己跑一遍完成判据那条命令**, 看到失败就继续修 —— 不要把它留给下一轮。\n")
	sb.WriteString("5. 更新 `" + RuntimeDir + "/" + ProgressFile + "`, 然后结束本轮。\n")
	sb.WriteString("6. 如果这一轮确实做不完: 把做到哪、下一步该干什么写进进度文件。\n\n")

	// 逐轮变化的部分放最后 (前缀稳定)。
	if firstRun || strings.TrimSpace(lastVerifyOut) == "" {
		sb.WriteString("# 上一轮的结果\n\n本轮是第一次执行, 还没有历史。先从读代码开始。\n")
	} else {
		sb.WriteString("# 上一轮验证命令的原始输出 (这是你这一轮的起点)\n\n")
		sb.WriteString("```\n")
		sb.WriteString(truncate(lastVerifyOut, 6000))
		sb.WriteString("\n```\n\n")
		sb.WriteString("上面是**未通过**的原始输出。先看懂它, 再动手。\n")
	}
	return sb.String()
}

// truncate 按**字节**截断 (不是 rune), 超限时追加"已截断 + 原长度"提示。
//
// 注意：按字节切会落在 UTF-8 序列中间, 中文内容被截断处可能产生一个乱码尾字符。
// 当前调用点 (拼接给模型的验证输出) 容忍这一点, 但换成需要展示给人看的文案时应改用 rune。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n... [已截断, 原文 %d 字节]", len(s))
}

// asExitError 包装 errors.As: 区分"跑完了但退出码非零"与"进程根本没起来"。
func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}
