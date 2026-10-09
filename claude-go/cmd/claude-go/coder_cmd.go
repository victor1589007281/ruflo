package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/anthropic/claude-go/pkg/coder"
	"github.com/spf13/cobra"
)

// coderCmd —— 长程自主编码任务的运行时。
//
// 与 `claude-go run` 的区别是"一次"与"很多次": `run` 起一个 agent 做完一轮就结束;
// `coder` 把很多轮串起来, 每轮之间由**驱动侧**客观地验证产物、把原始报错喂回下一轮、
// 并把状态落盘到工作区外的 .coder/, 于是进程被杀之后能接着跑。
//
// 用法的核心是三个参数: --goal (要做什么)、--verify (机器判定做完了没)、--workspace
// (在哪做)。其中 --verify 是唯一的成功判据 —— 模型自称完成不算数。
func coderCmd() *cobra.Command {
	var (
		goal      string
		goalFile  string
		verify    string
		workspace string

		model      string
		configPath string
		gateway    string
		tools      string

		maxIterations int
		maxTurns      int
		noProgress    int
		iterTimeout   time.Duration
		verifyTimeout time.Duration

		reset  bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "coder",
		Short: "长程自主编码: 可规划、可断点恢复、可自测、有客观终态验收",
		Long: `把一次性的 agent run 串成有状态的长程任务。

每一轮:
  1. 驱动侧**先跑一次 --verify** 测量现状 (这一步让"断点恢复"天然正确:
     无论上一轮怎么死的, 新进程第一件事是重新测量世界, 而不是相信落盘的自述)
  2. 组装 prompt (目标 + 上轮原始报错) 并起一个**有界**的 claude-go 进程
  3. 驱动侧**再跑一次 --verify** —— 这是唯一的成功判据, 模型自称完成不算数
  4. 状态落盘 (工作区内的 .coder/state.json), 供下一次进程接着跑

止损: 连续 N 轮"工作区没变 且 验证输出没变"就停下。这条挡的是"进程 exit=0、
is_error=false、模型说了一段像样的话, 而一个字节没改"的静默失败。

通过 --verify 之后会在一个**干净副本**(排除 .coder/)里再验一次 —— 那才叫
"程序可以独立运行"。

状态在 <workspace>/.coder/: state.json(状态) / PLAN.md(计划) / PROGRESS.md(进度)
/ prompt.md(本轮实际喂进去的 prompt) / logs/iter-NNN/(每轮 stdout+stderr)。
杀掉进程再原样跑同一条命令即可恢复。`,
		Example: `  # 让工作区里的 go test ./... 通过
  claude-go coder --goal "实现 Greet 函数, 让 go test ./... 通过" \
    --verify "go test ./..." --workspace /tmp/demo

  # 断了之后: 原样再跑一次, 会从落盘状态接着跑
  claude-go coder --goal "..." --verify "go test ./..." --workspace /tmp/demo`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if goal == "" && goalFile != "" {
				b, err := os.ReadFile(goalFile)
				if err != nil {
					return fmt.Errorf("读 --goal-file 失败: %w", err)
				}
				goal = string(b)
			}
			goal = strings.TrimSpace(goal)
			if goal == "" {
				return fmt.Errorf("必须给 --goal 或 --goal-file")
			}
			if strings.TrimSpace(verify) == "" {
				return fmt.Errorf("必须给 --verify: 没有客观终态就无法判定任务是否完成 " +
					"(agent 的自我报告不算判据)")
			}
			if workspace == "" {
				return fmt.Errorf("必须给 --workspace")
			}
			ws, err := filepath.Abs(workspace)
			if err != nil {
				return err
			}

			self, err := os.Executable()
			if err != nil {
				return fmt.Errorf("定位 claude-go 可执行文件失败: %w", err)
			}

			if reset {
				if err := os.RemoveAll(filepath.Join(ws, coder.RuntimeDir)); err != nil {
					return fmt.Errorf("清状态失败: %w", err)
				}
			}

			cfg := coder.Config{
				Goal:      goal,
				Workspace: ws,
				// 判据来自命令行而不是工作区 —— agent 改不到它,
				// 也就无法把"判据"改成自己能过的东西。
				VerifyCmd:            strings.Fields(verify),
				Bin:                  self,
				Model:                model,
				ConfigPath:           configPath,
				Gateway:              gateway,
				AllowedTools:         splitList(tools),
				MaxTurnsPerIteration: maxTurns,
				MaxIterations:        maxIterations,
				NoProgressLimit:      noProgress,
				IterationTimeout:     iterTimeout,
				VerifyTimeout:        verifyTimeout,
			}

			// Ctrl-C 要有用: 收到信号后取消 ctx, 让本轮收尾并把状态落盘。
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			r := &coder.Runner{
				Config: cfg,
				Agent: &coder.ClaudeGoAgent{
					Bin: self, Model: model, ConfigPath: configPath,
					Gateway: gateway, Cwd: ws, AllowedTools: cfg.AllowedTools,
					MaxTurns: maxTurns,
					// 每一轮子进程在轨迹页上的显示名。用**工作区名**而不是轮次号:
					// 长程任务跑起来后, 人要回答的是"我那个任务跑成什么样了", 而不是
					// "第 7 轮和第 8 轮分别叫什么"。同名让该任务的所有轮次一次搜全。
					RunLabel: "coder:" + filepath.Base(ws),
				},
				Verify: coder.CommandVerifier{
					Cmd: cfg.VerifyCmd, Timeout: verifyTimeout,
				},
				Logf: func(format string, a ...any) {
					fmt.Fprintf(os.Stderr, format+"\n", a...)
				},
			}

			res, runErr := r.Run(ctx)
			if asJSON {
				b, _ := json.MarshalIndent(res, "", "  ")
				fmt.Println(string(b))
			} else {
				fmt.Println(res.Report())
			}
			if runErr != nil {
				// 报告先打出来 (它是交付物), 再以非零码退出。
				fmt.Fprintf(os.Stderr, "\n失败: %v\n", runErr)
				os.Exit(1)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&goal, "goal", "", "任务目标 (人话); 或配 --goal-file")
	f.StringVar(&goalFile, "goal-file", "", "从文件读任务目标 (长目标用这个)")
	f.StringVar(&verify, "verify", "", "完成判据命令 (退出码 0 = 完成)。**唯一**的成功判据")
	f.StringVar(&workspace, "workspace", "", "工作目录 (agent 改这里, 验证也在这里跑)")

	f.StringVar(&model, "model", "", "模型别名, 如 opencode:space-bunny-free")
	f.StringVar(&configPath, "config", "", "claude-go 配置文件")
	f.StringVar(&gateway, "gateway", "", "LLM 网关地址 (opencode 系模型必须经网关)")
	f.StringVar(&tools, "allowed-tools", "Read,Write,StrReplace,Bash,Glob,Grep",
		"工具白名单 (逗号分隔)。名字必须是注册表里的真名: Bash/Read/Write/StrReplace/Glob/Grep")

	f.IntVar(&maxIterations, "max-iterations", 20, "最多迭代几轮")
	f.IntVar(&maxTurns, "max-turns", 40, "单轮 agent 的回合上限 (必须有限)")
	f.IntVar(&noProgress, "no-progress-limit", 3, "连续多少轮没有任何改动就止损")
	f.DurationVar(&iterTimeout, "iteration-timeout", 20*time.Minute, "单轮墙钟上限")
	f.DurationVar(&verifyTimeout, "verify-timeout", 10*time.Minute, "验证命令墙钟上限")

	f.BoolVar(&reset, "reset", false, "丢弃已有状态, 从头开始 (工作区代码保留)")
	f.BoolVar(&asJSON, "json", false, "以 JSON 输出结果")

	return cmd
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
