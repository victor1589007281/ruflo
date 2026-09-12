// Command claude-go-worker 独立的 Agent 运行时进程 (design/02 §3.3 L3, T2/T3 形态)。
//
// 它做的事只有一件: 向控制面注册自己的能力, 拉取 stage 任务, **用与单机模式完全
// 相同的执行路径**真跑, 把事件与终态回报给控制面。
//
//	claude-go-worker --control http://127.0.0.1:18080 --config ~/.claude-go/config.json \
//	                 --name worker-a --caps bash --max-parallel 2
//
// # 为什么是独立二进制而不是 claude-go 的子命令
//
// `claude-go worker` 子命令已经存在, 但它的执行体是桩 (回显 payload)。把真执行体
// 塞进 cmd/claude-go/main.go 需要动那个 3000+ 行、多方同时在改的文件; 独立二进制
// 让 worker 的依赖装配自成一体, 也更贴合容器化部署 (镜像只跑一个进程)。
// 两者可以共存: 旧子命令保持原状 (仍是桩), 生产用本二进制。
//
// # worker 不该承担的中心职责 (启动时显式关掉)
//
// 真执行体来自 `feishu.NewBot` 装配出来的 SessionManager —— 那是全仓唯一装配好
// QueryEngine + prompt + skills + tools + MCP 的地方。但 NewBot 同时会启动
// **cron 调度器**和 (配置允许时) wiki/dashboard HTTP 服务; 一个 worker 若也跑
// 定时任务, 就会与控制面重复触发同一个 job (design/02 单机假设 #7)。所以:
//
//	Wiki.APIPort = 0 + Wiki.Enabled = false + APIExtensions = nil
//	                                          → 不起 HTTP 服务, 不抢 :18080, 不挂 dashboard
//	CronScheduler().Stop()                    → 不重复触发定时任务
//
// ⚠️ 残留的过度装配 (诚实记录, 非本次范围): Bot 仍会构造 teamMgr / 记忆 / 进化 /
// 意图识别等中心组件, 它们只是"构造了没启动"。彻底瘦身属 design/02 §3.5 的
// feishu-adapter 拆分。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/hotreload"
	"github.com/anthropic/claude-go/pkg/worker"
	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var (
		control       string
		name          string
		configPath    string
		cwd           string
		workspace     string
		wsMode        string
		wsVolume      string
		extraCaps     []string
		noBash        bool
		browserCap    bool
		gpuCap        bool
		k8sCap        bool
		maxParallel   int
		pollMS        int
		heartbeatS    int
		keepaliveS    int
		token         string
		claim         string
		claimTimeoutS int
	)
	cmd := &cobra.Command{
		Use:   "claude-go-worker",
		Short: "claude-go 远程 Agent 运行时 (拉取图节点并真实执行)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(control) == "" {
				return fmt.Errorf("--control 必填 (控制面基址, 如 http://127.0.0.1:18080)")
			}
			if name == "" {
				name = defaultWorkerName()
			}
			// 控制面对 /cluster/* 默认要 Bearer (pkg/httpauth DefaultProtectPrefixes)。
			// 配了 wiki.apiSecret 的部署必须给 token, 否则每次拉取都是 401。
			if token == "" {
				token = strings.TrimSpace(os.Getenv("CLAUDE_GO_API_TOKEN"))
			}

			// —— 配置装载: 与 claude-go serve/llm-gateway 完全同一条路径 ——
			botCfg := feishu.DefaultBotConfig()
			jsonCfg, err := feishu.LoadJSONConfig(configPath)
			if err != nil {
				return fmt.Errorf("加载配置失败: %w", err)
			}
			// 热加载监视器要的是"确定存在"的路径: --config 留空时 LoadJSONConfig 走
			// 自动发现, 这里把解析结果记下来喂给监视器。
			cfgPath, _ := feishu.ResolveJSONConfigPath(configPath)
			if jsonCfg != nil {
				jsonCfg.ApplyToBot(botCfg)
			}
			botCfg.Headless = true // 不连飞书
			if cwd != "" {
				botCfg.Cwd = cwd
			}
			if botCfg.Cwd == "" {
				botCfg.Cwd, _ = os.Getwd()
			}
			// 见文件头: worker 不承担中心职责。
			botCfg.Wiki.Enabled = false
			botCfg.Wiki.APIPort = 0
			botCfg.Wiki.APIExtensions = nil
			if botCfg.ModelAlias == "" {
				return fmt.Errorf("配置里没有可用模型 (modelAlias/providers): worker 无法真执行, 拒绝以桩的形式启动")
			}

			// —— cwd 档位 (design/02 §3.3) ——
			//
			// 关键约束: 工具的执行根是**进程级**的 (feishu.SessionManager 用
			// sm.config.Cwd, session.go:654,793,1194), worker 无法逐任务切换它。所以
			// 声明的工作区必须等于 botCfg.Cwd, 否则 Agent 把文件写到 cwd 而工作区里
			// 什么都没有 —— 那正是"阶段成功但下一阶段找不到代码"。启动时就钉死。
			mode, mErr := worker.ParseWorkspaceMode(wsMode)
			if mErr != nil {
				return mErr
			}
			if mode == worker.WorkspaceModePVC || mode == worker.WorkspaceModeGit {
				if workspace == "" {
					workspace = botCfg.Cwd // 未显式声明就用执行 cwd (两者必须一致)
				}
				if strings.TrimRight(workspace, "/") != strings.TrimRight(botCfg.Cwd, "/") {
					return fmt.Errorf("%s 档位要求 --workspace 与执行 cwd 一致, 实得 workspace=%s cwd=%s; "+
						"不一致时 Agent 会把文件写到 cwd 而工作区里什么都没有 (拒绝启动)",
						mode, workspace, botCfg.Cwd)
				}
			}

			bot, err := feishu.NewBot(botCfg)
			if err != nil {
				return fmt.Errorf("装配执行体失败: %w", err)
			}

			// 配置热加载: config.json 里 providers/models/ai 改了立即生效, 不必重启
			// worker。worker 是长跑进程, 重启代价高 (会打断在跑的任务), 所以这条最值。
			hotreload.WatchConfig(cfgPath, 5*time.Second, "claude-go-worker-"+name,
				func(p string) error { return bot.ReloadModelConfig(p) })
			// NewBot 内部无条件 cronSched.Start(), 必须在这里停掉。
			// 注意: 不调用 bot.Shutdown() —— 它会再 Stop 一次 cron, 而
			// CronScheduler.Stop 是 close(stopCh), 二次调用会 panic
			// (pkg/agent/cron.go:113)。进程退出由 OS 回收其余资源。
			if cs := bot.CronScheduler(); cs != nil {
				cs.Stop()
			}

			caps := agent.RuntimeCaps{
				Bash:        !noBash,
				Browser:     browserCap,
				GPU:         gpuCap,
				K8sSandbox:  k8sCap,
				MaxParallel: maxParallel,
			}
			// runtime 名带 local- 前缀: 它在 worker 进程内**就是**本地执行体。
			// 控制面侧看到的名字是 worker 名 (见 worker.RuntimeName)。
			rt := bot.AgentRuntime("local-"+name, caps)
			if rt == nil {
				return fmt.Errorf("执行体为空 (会话管理器未装配), 拒绝启动")
			}

			w, err := worker.New(worker.Options{
				Control:           control,
				Name:              name,
				Runtime:           rt,
				ExtraCaps:         extraCaps,
				Workspace:         workspace,
				WorkspaceMode:     mode,
				WorkspaceVolume:   wsVolume,
				MaxParallel:       maxParallel,
				PollInterval:      time.Duration(pollMS) * time.Millisecond,
				HeartbeatInterval: time.Duration(heartbeatS) * time.Second,
				KeepaliveInterval: time.Duration(keepaliveS) * time.Second,
				AuthToken:         token,
				Logf:              func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
			})
			if err != nil {
				return err
			}

			stateDir := basedir.ResolveDefault(botCfg.StateDir, botCfg.Cwd)
			fmt.Printf("[worker] %s → 控制面 %s\n", name, control)
			fmt.Printf("[worker] caps=%v 并发=%d cwd=%s state=%s\n", w.Caps(), maxParallel, botCfg.Cwd, stateDir)
			if workspace != "" {
				fmt.Printf("[worker] 工作区=%s 档位=%s\n", workspace, mode.Effective())
			} else {
				fmt.Printf("[worker] 未声明工作区 (档位=%s): 指定了 workspace 的任务会被拒绝 (--workspace 声明)\n",
					mode.Effective())
			}
			switch mode.Effective() {
			case worker.WorkspaceModeLocal:
				fmt.Printf("[worker] local 档只校验路径一致, **不证明与控制面是同一份数据**; " +
					"跨机产码请用 --workspace-mode pvc 或 git\n")
			case worker.WorkspaceModePVC:
				fmt.Printf("[worker] pvc 档: 卷=%s; 每个任务都要读到控制面写的握手文件才执行\n", wsVolume)
			case worker.WorkspaceModeGit:
				fmt.Printf("[worker] git 档: 每阶段 fetch+checkout 后执行, 结束后 commit/push 回约定位置 " +
					"(并发上限强制为 1)\n")
			}
			// 同机与控制面共用 stateDir 时的真实风险, 必须提示 (design/02 §3.4.1:
			// FileStore 的桶锁是 per-instance, 跨进程无锁 → 交错写会丢更新)。
			warnSharedState(stateDir)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				s := <-sig
				fmt.Printf("\n[worker] 收到信号 %v, 停止拉取并等在途任务收尾...\n", s)
				cancel()
			}()

			if strings.TrimSpace(claim) != "" {
				return runClaim(ctx, w, strings.TrimSpace(claim), time.Duration(claimTimeoutS)*time.Second,
					time.Duration(pollMS)*time.Millisecond)
			}

			err = w.Run(ctx)
			done, failed := w.Stats()
			fmt.Printf("[worker] 退出: 完成 %d, 失败 %d\n", done, failed)
			if err != nil && !isCanceled(err) {
				return err
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&control, "control", "", "控制面基址 (如 http://127.0.0.1:18080)")
	f.StringVar(&name, "name", "", "worker 名 (默认 POD_NAME 或 worker-<主机名>)")
	f.StringVar(&configPath, "config", "", "JSON 配置文件 (providers/skills/stateDir 等)")
	f.StringVar(&cwd, "cwd", "", "工具执行根目录 (默认配置里的 cwd 或当前目录)")
	f.StringVar(&workspace, "workspace", "", "本 worker 能提供的团队工作区绝对路径 (pvc/git 档必须与 --cwd 一致)")
	f.StringVar(&wsMode, "workspace-mode", "", "cwd 档位: local (默认) | pvc | git; 上报成 ws:<档位> 能力标签")
	f.StringVar(&wsVolume, "workspace-volume", "", "pvc 档: 本 worker 挂的共享卷名 (须与控制面 --workspace-volume 一致)")
	f.StringSliceVar(&extraCaps, "caps", nil, "额外能力标签 (如 mcp:playwright,cli:golangci-lint)")
	f.BoolVar(&noBash, "no-bash", false, "声明本 worker 不提供 shell 执行能力")
	f.BoolVar(&browserCap, "browser", false, "声明具备无头浏览器")
	f.BoolVar(&gpuCap, "gpu", false, "声明具备 GPU")
	f.BoolVar(&k8sCap, "k8s-sandbox", false, "声明可下发 K8s Job 沙箱")
	f.IntVar(&maxParallel, "max-parallel", 1, "并发执行上限")
	f.IntVar(&pollMS, "poll-ms", 1000, "无任务时轮询间隔 (毫秒)")
	f.IntVar(&heartbeatS, "heartbeat-sec", 30, "心跳间隔 (秒); 必须显著小于控制面注册表租约")
	f.IntVar(&keepaliveS, "keepalive-sec", 15, "事件冲刷/续租/取消检查间隔 (秒)")
	f.StringVar(&token, "token", "", "控制面 Bearer token (默认取环境变量 CLAUDE_GO_API_TOKEN; 控制面设了 wiki.apiSecret 时必需)")
	f.StringVar(&claim, "claim", "", "一次性模式: 只认领指定 task id 并在执行完后退出 (k8s-job runtime 下发的 Job 用)")
	f.IntVar(&claimTimeoutS, "claim-timeout-sec", 300, "一次性模式: 等待该任务出现的上限秒数; 超时退出非零 (让 Job 判失败)")
	return cmd
}

// runClaim 一次性认领模式 (k8s-job runtime 起的 Job 走这条路)。
//
// 三条与常驻模式不同的语义, 每条都影响控制面能不能看见失败:
//
//  1. **退出码 = 认领与回报是否发生**, 不是任务成功与否。任务的成败经队列回报
//     (终态只认队列, 见 pkg/worker 文件头); 用退出码表达它会变成第二个真源。
//     所以: 拉到并跑完 → 0 (哪怕 Agent 失败了); 没拉到 / 拉到别人的 → 非 0。
//  2. **认领不到必须超时退出非零**。若一直挂着, Job 永远不终结, 控制面的监视器
//     只能等 activeDeadlineSeconds —— 那可能是一小时。
//  3. **拉到非指派任务立刻退出**, 不重试: 那是 worker 名撞车 (部署级错误), 重试
//     只会把别人的任务也搅坏。
func runClaim(ctx context.Context, w *worker.Worker, taskID string, timeout, poll time.Duration) error {
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	fmt.Printf("[worker] 一次性模式: 只认领任务 %s (最长等 %s)\n", taskID, timeout)
	deadline := time.Now().Add(timeout)
	for {
		ok, err := w.ClaimOnce(ctx, taskID)
		if ok {
			done, failed := w.Stats()
			fmt.Printf("[worker] 任务 %s 已执行并回报 (完成 %d, 失败 %d), 退出\n", taskID, done, failed)
			return nil
		}
		if err != nil {
			if errors.Is(err, worker.ErrTaskNotAssigned) {
				return err // 部署级错误, 不重试
			}
			// 控制面还没起来 / 网络抖动: 在超时窗口内继续试。
			fmt.Printf("[worker] 认领 %s 暂未成功: %v\n", taskID, err)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("认领 %s 期间收到停止信号: %w", taskID, ctx.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 %s 仍未认领到任务 %s: 控制面没有把它派给本 worker "+
				"(名字不符/能力标签不匹配/任务已被判失败)", timeout, taskID)
		}
		time.Sleep(poll)
	}
}

func defaultWorkerName() string {
	if pod := strings.TrimSpace(os.Getenv("POD_NAME")); pod != "" {
		return pod
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "worker"
	}
	return "worker-" + host
}

// warnSharedState 同机共用 stateDir 的告警。
func warnSharedState(stateDir string) {
	marker := filepath.Join(stateDir, "statestore")
	if fi, err := os.Stat(marker); err == nil && fi.IsDir() {
		fmt.Printf("[worker] ⚠️ stateDir 已存在 statestore/ (%s): 若控制面同机共用该目录, "+
			"FileStore 无跨进程锁 (design/02 §3.4.1), 建议给 worker 单独的 --config/stateDir\n", marker)
	}
}

func isCanceled(err error) bool {
	return err == context.Canceled || strings.Contains(err.Error(), "context canceled")
}
