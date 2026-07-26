package worker

// k8sjob.go —— k8s-job runtime: 一个图节点一个一次性 K8s Job
// (design/01 §4.9 v1 三个内置 runtime 的最后一个)。
//
// # 这不是"把 K8SRunner 包成 AgentRuntime"
//
// 设计稿的原话是「收编 `sandbox/k8s_runner.go` 的 K8s Job 下发，共享 PVC 工作区」。
// 但 `sandbox.K8SRunner` 跑的是**命令** (`CommandSpec` → `CommandResult`): 它把
// spec.Args 塞进容器 command、等 Job 完成、再从 pod 日志里捞输出。直接把它包成
// `agent.AgentRuntime` 会得到一个"能跑 shell 但不能跑 agent"的假实现 —— agent 执行
// 需要 QueryEngine + prompt 组装 + skills + 工具画像 + MCP, 那一整套只在
// `feishu.SessionManager` 里装配得起来, 而唯一把它容器化的东西是
// `cmd/claude-go-worker`。
//
// 所以本文件收编的是 K8SRunner 的**Job 下发那一半** (清单形状、共享 PVC 约定、
// ttlSecondsAfterFinished、kubectl 通路), 执行体复用既有 worker 二进制:
//
//	Execute ─① 入队一个钉死在 worker:<一次性名字> 上的 stage 任务
//	        ─② 创建一个跑 `claude-go-worker --claim <taskID>` 的 K8s Job
//	             └─ Job 里的 worker 注册 → 只拉得到那一条任务 → 用与单机完全相同的
//	                路径真跑 → 事件回传 → Complete/Fail 落队列 → 进程退出
//	        ─③ 事件与终态处理**一行不改**地复用 remoteRuntime.watch
//
// 一句话: 远程 runtime 的通用形状是"入队 + 等人来拉", k8s-job 只是在入队之后多做
// 一件事 —— 把那个拉取者本身也创建出来。因此 fail-closed 四规则 (终态只认队列 /
// 结果协议标记 / 工作区可见性 / 不静默成功) 自动继承, 不重写。
//
// # 与常驻 worker (deploy/k8s/distributed.yaml) 的分工
//
// 常驻 worker 是**热池**: 进程常驻、配置装载与 skills 注册只做一次, 派任务到执行
// 开始是毫秒级。稳态产码工作流应该继续用它 —— k8s-job 每个节点都要经历
// 调度 → (可能的)拉镜像 → 进程启动 → 装载配置 → 注册 → 拉取, 十几秒到分钟级的
// 冷启延迟是实打实的成本。**这一项不是常驻 worker 的替代品**。
//
// 它多提供的是常驻池给不了的三件事:
//
//	① 按节点弹性: 副本数是 Deployment 里写死的数字, 而 Job 是按需创建的。一次
//	   map 扇出 20 个分片, 热池要排队 20/replicas 轮, k8s-job 直接起 20 个 pod
//	   (受 MaxParallel 与集群容量约束), 跑完即消失, 空闲期零占用。
//	② 特殊能力池不必常驻: 声明 `require:["gpu"]` / `require:["browser"]` 的节点,
//	   在热池模型下要求你**长期挂着**一个 GPU pod 或带浏览器的胖 pod。k8s-job 让
//	   镜像/nodeSelector/资源限额随 runtime 配置走, 用完就还给集群。
//	③ 任务级隔离: 每个节点一个全新 pod —— 上一个节点泄漏的进程、写脏的 /tmp、
//	   撑爆的内存都带不到下一个节点; 内存/CPU 限额由 kubelet 真强制 (常驻 worker
//	   的 MaxParallel 只是并发计数, 不是资源闸)。**git 档下还多一条**: 常驻 worker
//	   在 git 档被强制 MaxParallel=1 (只有一个检出目录, 并发会互相 checkout/reset,
//	   见 worker.go New), 而每个 Job 自带独立检出目录, 天然可并行。
//
// 反过来诚实说: 只跑 pvc 档、只有稳态串行阶段的部署, 用常驻 worker 就够了,
// 这一项对它没有增量价值。
//
// # Job 的生命周期与"重试单层化"
//
//	backoffLimit: 0            —— **必须**。§4.3 重试单层化是本仓成文教训: 图层
//	                              RetryPolicy 已经在重试, 队列刻意设 MaxAttempts=1
//	                              就是为了不做第二层; Job 的 backoffLimit 会是第三层。
//	                              而且它重试也没用: 任务已被上一个 pod 拉走并随租约
//	                              过期判失败, 新 pod 只会空转到 claim 超时。
//	activeDeadlineSeconds      —— 兜底硬超时。没有它, ImagePullBackOff / 不可调度的
//	                              Job 会永远占着不放 (TTL 只回收**已终结**的 Job)。
//	ttlSecondsAfterFinished    —— 兜底回收 (控制面崩了也不留垃圾)。
//	控制面主动删               —— watch 结束时: Job **尚未终结**就删 (它还在烧资源,
//	                              而控制面已经不认它的产出了); **已终结**则留给 TTL,
//	                              因为 `kubectl logs` 是看 worker 崩溃现场的唯一入口。
//
// # 失败必须真失败
//
// Job 起不来 (镜像拉不到 / PVC 挂不上 / 配置错) 时, 队列里的任务只是一直 pending,
// 而 pending 在既有 watch 里要等 DeadWorkerGrace(60s) 才判失败, 归因还会指向
// "worker 掉线" —— 对着一个从来没起来过的 Job 说"它掉线了"是误导。所以本文件给
// watch 装了一个**监视器**: 每个轮询周期看一眼 Job/pod 的真实状态, 拿到
// ImagePullBackOff / CreateContainerConfigError / DeadlineExceeded 这类不可恢复的
// 原因就立刻失败并把原因**原文**带回控制面。
//
// 监视器只在**连续两次**观察到同一个坏状态时才判死: kubectl 抖动、以及"worker 已
// Complete → pod 退出 → Job 变 Complete"与队列状态之间的微小时序差, 都不该被
// 读成失败。读不出状态 (kubectl 报错) 一律**不判死** —— 观测通道故障不是执行故障。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/sandbox"
)

// K8SJobRuntimeName k8s-job runtime 的名字 (design/01 §4.9 冻结的名字)。
//
// 刻意不以 "local-" 打头: `agent.isLocalRuntimeName` 按该前缀判本地,
// 叫 local-* 会让 `Prefer:"local"` 给跨机执行加分 (与 RuntimeName 同一条理由)。
const K8SJobRuntimeName = "k8s-job"

// CapEphemeral 一次性 worker 的自我声明标签。
//
// 为什么必须有它: Job 里的 worker 也会往 cluster.Registry 心跳, 而 Broker.Sync 会把
// 注册表里每个存活 worker 注册成一个可被 Pick 选中的 remoteRuntime。若不把它们排除,
// 一个**只认领一条任务、跑完就退出**的 worker 会被当成常驻算力派进别的节点 ——
// 那些任务会一直 pending 到宽限期结束才失败。
const CapEphemeral = "ephemeral"

// k8s-job 默认参数。
const (
	// DefaultJobActiveDeadline Job 硬超时 (秒)。编码类节点动辄十几分钟, 给 1 小时;
	// 真正的节点超时由图层管, 这里只是兜底。
	DefaultJobActiveDeadline int64 = 3600
	// DefaultJobTTLAfterFinished 已终结 Job 的保留时长 (秒)。留够人去 kubectl logs。
	DefaultJobTTLAfterFinished int32 = 1800
	// DefaultJobClaimTimeout 一次性 worker 等待认领任务的上限 (秒)。
	// 超时即退出非零 → Job 判失败 → 监视器把它翻译成节点失败。
	DefaultJobClaimTimeout = 300
	// DefaultJobWorkDir 非 pvc 档时 Job 里的工作目录 (挂 emptyDir)。
	DefaultJobWorkDir = "/work"
	// DefaultJobStateDir Job 里 worker 的状态目录挂载点。
	// 每个 Job 独占 emptyDir —— FileStore 无跨进程锁 (design/02 §3.4.1)。
	DefaultJobStateDir = "/data"
	// DefaultJobConfigDir 配置 ConfigMap 的挂载点 (与 deploy/k8s/*.yaml 一致)。
	DefaultJobConfigDir = "/etc/claude-go"
	// jobKubectlTimeout 单条 kubectl 命令的超时。
	jobKubectlTimeout = 20 * time.Second
	// jobFatalConfirm 判死前需要连续观察到的次数 (吸收 kubectl 抖动与状态时序差)。
	jobFatalConfirm = 2
)

// K8SJobOptions k8s-job runtime 的部署参数。
//
// 默认值取自 `sandbox.CurrentConfig().K8S` (见 K8SJobOptionsFromEnv) —— 那是仓里
// 既有的 K8s 沙箱配置, 命名空间/PVC/镜像/ServiceAccount/TTL 全在里面, 没必要造
// 第二套。环境变量只做覆盖。
type K8SJobOptions struct {
	// Control 必填: Job 里的 worker 回连控制面的基址 (集群内 Service, 如
	// http://claude-go-control:18080)。**不给默认值**: 猜错的后果是每个 Job 都起来
	// 然后连不上, 白烧一轮调度。
	Control string
	// Image 必填: 含 /usr/local/bin/claude-go-worker 的镜像。
	Image string
	// Namespace Job 创建在哪个命名空间 (空 → sandbox 配置的, 再空 → default)。
	Namespace string
	// ServiceAccount Job pod 的 SA (通常不需要 —— worker 不调 K8s API)。
	ServiceAccount string
	// ImagePullPolicy 空 → IfNotPresent。
	ImagePullPolicy string

	// WorkspacePVC pvc 档必填: 团队工作区的 RWX 卷。
	WorkspacePVC string
	// WorkspaceMountPath 卷挂载点 (空 → /workspace)。控制面的团队 cwd 必须等于它
	// 或在它之下, 否则 Job 里根本看不到那个目录。
	WorkspaceMountPath string
	// WorkspaceSubPath 卷内子路径 (可空)。
	WorkspaceSubPath string

	// ConfigMap 挂到 ConfigMountPath 的配置 (worker --config 指向 <dir>/config.json)。
	// 空 = 不挂; 此时 worker 只能靠环境变量拿 provider —— 多数情况下会因
	// "配置里没有可用模型" 拒绝启动 (那是它该有的行为)。
	ConfigMap string
	// ConfigMountPath 空 → /etc/claude-go。
	ConfigMountPath string
	// ConfigFile 配置文件名 (空 → config.json)。
	ConfigFile string

	// Env 透传给 Job 的环境变量, 形如 "K=V"。
	Env []string
	// EnvSecret 从 Secret 取的环境变量, 形如 "NAME=secretName/key"。
	// LLM key 走这里 —— 写进 Env 会明文出现在 `kubectl get job -o yaml` 里。
	EnvSecret []string

	// Caps 本 runtime 对外声明的能力。**同时**决定 Job 里 worker 的能力开关参数,
	// 两侧因此不可能漂移 (worker 侧的 MissingCaps 自检会当场戳穿不一致)。
	//
	// ⚠️ 它是**声明**不是探测: 说自己有 browser 而镜像里没有浏览器, 症状是
	// "调研节点声称查过了"。与常驻 worker 的 --browser 同一性质, 由部署方负责。
	Caps agent.RuntimeCaps

	// CPULimit/MemoryLimit 容器资源限额 (如 "2" / "4Gi"), 空 = 不设。
	CPULimit    string
	MemoryLimit string
	// NodeSelector 落到哪类节点 (GPU/浏览器池的路由手段)。
	NodeSelector map[string]string

	// TTLSeconds 已终结 Job 的保留时长 (0 → DefaultJobTTLAfterFinished)。
	TTLSeconds int32
	// ActiveDeadline Job 硬超时 (0 → DefaultJobActiveDeadline)。
	ActiveDeadline int64
	// ClaimTimeout 一次性 worker 等待认领的上限秒数 (0 → DefaultJobClaimTimeout)。
	ClaimTimeout int
	// MaxParallel 同时在飞的 Job 上限 (<=0 = 不限)。
	// 达到上限时 Execute **阻塞等待**而不是失败 —— 容量不足是排队问题, 不是节点错误。
	MaxParallel int

	// Kubectl kubectl 可执行文件 (空 → "kubectl")。
	Kubectl string

	// kube 测试注入点 (nil → 真 kubectl)。
	kube kubeRunner
}

// K8SJobOptionsFromEnv 从 sandbox 配置 + 环境变量装配参数。
//
// 为什么用环境变量而不是 CLI flag: `cmd/claude-go/main.go` 是多方同时在改的
// 3000+ 行文件, 这一项只需要在既有接线块里插一小段。环境变量还天然适配 K8s
// (清单里就是 env 列表)。
func K8SJobOptionsFromEnv() K8SJobOptions {
	k := sandbox.CurrentConfig().K8S
	opt := K8SJobOptions{
		Control:            strings.TrimSpace(os.Getenv("CLAUDE_GO_K8SJOB_CONTROL")),
		Image:              firstNonEmptyStr(os.Getenv("CLAUDE_GO_K8SJOB_IMAGE"), k.Image),
		Namespace:          firstNonEmptyStr(os.Getenv("CLAUDE_GO_K8SJOB_NAMESPACE"), k.Namespace),
		ServiceAccount:     firstNonEmptyStr(os.Getenv("CLAUDE_GO_K8SJOB_SERVICE_ACCOUNT"), k.ServiceAccount),
		ImagePullPolicy:    strings.TrimSpace(os.Getenv("CLAUDE_GO_K8SJOB_PULL_POLICY")),
		WorkspacePVC:       firstNonEmptyStr(os.Getenv("CLAUDE_GO_K8SJOB_PVC"), k.WorkspacePVC),
		WorkspaceMountPath: firstNonEmptyStr(os.Getenv("CLAUDE_GO_K8SJOB_MOUNT_PATH"), k.WorkspaceMountPath),
		WorkspaceSubPath:   firstNonEmptyStr(os.Getenv("CLAUDE_GO_K8SJOB_SUB_PATH"), k.WorkspaceSubPath),
		ConfigMap:          strings.TrimSpace(os.Getenv("CLAUDE_GO_K8SJOB_CONFIGMAP")),
		ConfigMountPath:    strings.TrimSpace(os.Getenv("CLAUDE_GO_K8SJOB_CONFIG_DIR")),
		ConfigFile:         strings.TrimSpace(os.Getenv("CLAUDE_GO_K8SJOB_CONFIG_FILE")),
		Env:                splitList(os.Getenv("CLAUDE_GO_K8SJOB_ENV")),
		EnvSecret:          splitList(os.Getenv("CLAUDE_GO_K8SJOB_ENV_SECRET")),
		CPULimit:           strings.TrimSpace(os.Getenv("CLAUDE_GO_K8SJOB_CPU")),
		MemoryLimit:        strings.TrimSpace(os.Getenv("CLAUDE_GO_K8SJOB_MEMORY")),
		NodeSelector:       parseKV(splitList(os.Getenv("CLAUDE_GO_K8SJOB_NODE_SELECTOR"))),
		TTLSeconds:         k.TTLSecondsAfterFinished,
		Kubectl:            strings.TrimSpace(os.Getenv("CLAUDE_GO_K8SJOB_KUBECTL")),
	}
	// 能力: 不声明 ⇒ 只有 bash (一个 claude-go 镜像里本来就有 shell)。
	// 显式声明 ⇒ 以声明为准, 想保留 bash 就必须把 bash 写进去 —— 不替使用者猜。
	if caps := splitList(os.Getenv("CLAUDE_GO_K8SJOB_CAPS")); len(caps) > 0 {
		opt.Caps = RuntimeCapsFromLabels(caps)
	} else {
		opt.Caps = agent.RuntimeCaps{Bash: true}
	}
	if v := envInt("CLAUDE_GO_K8SJOB_TTL_SEC"); v > 0 {
		opt.TTLSeconds = int32(v)
	}
	if v := envInt("CLAUDE_GO_K8SJOB_DEADLINE_SEC"); v > 0 {
		opt.ActiveDeadline = int64(v)
	}
	if v := envInt("CLAUDE_GO_K8SJOB_CLAIM_TIMEOUT_SEC"); v > 0 {
		opt.ClaimTimeout = v
	}
	if v := envInt("CLAUDE_GO_K8SJOB_MAX_PARALLEL"); v != 0 {
		opt.MaxParallel = v
	} else {
		// 默认给一个有限值: 一次 map 扇出几十个分片时, 无上限地朝集群灌 pod
		// 会把别人的工作负载挤掉。想不限就显式写 -1。
		opt.MaxParallel = 8
	}
	if opt.MaxParallel < 0 {
		opt.MaxParallel = 0
	}
	return opt
}

// K8SJobEnabled 环境变量是否要求启用 k8s-job runtime。
func K8SJobEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_GO_K8SJOB"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (o *K8SJobOptions) normalize() {
	o.Control = strings.TrimRight(strings.TrimSpace(o.Control), "/")
	o.Namespace = firstNonEmptyStr(strings.TrimSpace(o.Namespace), "default")
	o.ImagePullPolicy = firstNonEmptyStr(strings.TrimSpace(o.ImagePullPolicy), "IfNotPresent")
	o.WorkspaceMountPath = firstNonEmptyStr(strings.TrimSpace(o.WorkspaceMountPath), "/workspace")
	o.ConfigMountPath = firstNonEmptyStr(strings.TrimSpace(o.ConfigMountPath), DefaultJobConfigDir)
	o.ConfigFile = firstNonEmptyStr(strings.TrimSpace(o.ConfigFile), "config.json")
	o.Kubectl = firstNonEmptyStr(strings.TrimSpace(o.Kubectl), "kubectl")
	if o.TTLSeconds <= 0 {
		o.TTLSeconds = DefaultJobTTLAfterFinished
	}
	if o.ActiveDeadline <= 0 {
		o.ActiveDeadline = DefaultJobActiveDeadline
	}
	if o.ClaimTimeout <= 0 {
		o.ClaimTimeout = DefaultJobClaimTimeout
	}
	// 对外声明的并发上限就是真实的那个闸 —— 两个数字不一致时, 看 caps 的人会以为
	// 还有余量。RuntimeCaps.MaxParallel 目前只是声明 (Pick 不读它), 但既然要报,
	// 就得报真的。
	o.Caps.MaxParallel = o.MaxParallel
}

// Validate 参数自洽性。缺项一律在**启动时**报错: 一个装配错的 k8s-job runtime 会让
// 每个派给它的节点各起一个注定失败的 pod, 而症状要等到跑工作流时才出现。
func (o *K8SJobOptions) Validate(ws *WorkspacePolicy) error {
	if strings.TrimSpace(o.Control) == "" {
		return fmt.Errorf("worker: k8s-job 必须声明控制面基址 (CLAUDE_GO_K8SJOB_CONTROL), " +
			"Job 里的 worker 靠它回连拉取任务")
	}
	if strings.TrimSpace(o.Image) == "" {
		return fmt.Errorf("worker: k8s-job 必须声明镜像 (CLAUDE_GO_K8SJOB_IMAGE 或 sandbox.k8s.image), " +
			"且镜像里要有 /usr/local/bin/claude-go-worker")
	}
	for _, e := range o.Env {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("worker: k8s-job 环境变量 %q 形如 K=V", e)
		}
	}
	for _, e := range o.EnvSecret {
		name, ref, ok := strings.Cut(e, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("worker: k8s-job Secret 环境变量 %q 形如 NAME=secretName/key", e)
		}
		if s, k, ok2 := strings.Cut(ref, "/"); !ok2 || strings.TrimSpace(s) == "" || strings.TrimSpace(k) == "" {
			return fmt.Errorf("worker: k8s-job Secret 环境变量 %q 形如 NAME=secretName/key", e)
		}
	}
	switch ws.effectiveMode() {
	case WorkspaceModeLocal:
		// local 档在 k8s-job 下**必然是错的**, 所以拒绝启动而不是"尽力而为"。
		// local 的语义是"worker 用自己的本地目录, 靠团队亲和落回同一台机器";
		// 而每个 Job 都是一个全新 pod、一个全新空目录 —— 上一阶段写的代码 100%
		// 消失, 而 checkWorkspace 只比路径字符串, 会**全程通过**。
		// 那正是最难归因的一类静默故障, 宁可在启动时说清楚。
		return fmt.Errorf("worker: k8s-job 不支持 local 档工作区 —— 每个 Job 都是新 pod/新空目录, " +
			"上一阶段的产物必然消失而路径校验又会通过 (静默失败); 请用 pvc (共享 RWX 卷) 或 git 档")
	case WorkspaceModePVC:
		if strings.TrimSpace(o.WorkspacePVC) == "" {
			return fmt.Errorf("worker: pvc 档的 k8s-job 必须声明工作区 PVC " +
				"(CLAUDE_GO_K8SJOB_PVC 或 sandbox.k8s.workspacePVC), 否则 Job 里看不到团队 cwd")
		}
	}
	return nil
}

// effectiveMode 空策略视作"未启用"。
func (p *WorkspacePolicy) effectiveMode() WorkspaceMode {
	if !p.Enabled() {
		return WorkspaceModeUnset
	}
	return p.Mode
}

// ─────────────────────────────────────────────────────────────────────────────
// kubectl 通路
// ─────────────────────────────────────────────────────────────────────────────

// kubeRunner 执行一条 kubectl 命令。抽成接口只为一件事: 让清单构造、失败翻译、
// 生命周期这些**真逻辑**能在没有集群的机器上被测到 (真集群验证另做, 见报告)。
type kubeRunner interface {
	run(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
}

// kubectlRunner 真 kubectl。
//
// 为什么是 exec kubectl 而不是 client-go: 仓里零 K8s 依赖 (go.mod 无 client-go),
// 而 `pkg/sandbox/k8s_runner.go` 早就是 exec kubectl —— 引入 client-go 会为了一个
// runtime 拖进上百个间接依赖。代价是**镜像里必须有 kubectl**, 所以 probe 会在启动时
// 显式检查它 (见 deploy/Dockerfile.k8sjob)。
type kubectlRunner struct{ bin string }

func (k kubectlRunner) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, jobKubectlTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, k.bin, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.Bytes(), fmt.Errorf("kubectl %s: %w (%s)", strings.Join(args, " "), err, truncate(msg, 400))
	}
	return out.Bytes(), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// jobRuntime —— AgentRuntime 实现
// ─────────────────────────────────────────────────────────────────────────────

type jobRuntime struct {
	b    *Broker
	opt  K8SJobOptions
	kube kubeRunner
	sem  chan struct{} // MaxParallel 闸 (nil = 不限)

	mu       sync.Mutex
	inflight map[string]string // "runID/nodeID" → taskID (Cancel 用)
}

// K8SJobRuntime 构造 k8s-job runtime。
//
// 构造即**探活**: kubectl 在不在、命名空间访问得到吗、有没有建 Job 的权限。
// 探不通就返回错误, 调用方**不要注册它** —— 注册一个跑不通的 runtime 意味着
// Pick 会把节点派给它然后逐个失败, 而 Placement 是 fail-closed 的, 没有兜底。
func (b *Broker) K8SJobRuntime(opt K8SJobOptions) (agent.AgentRuntime, error) {
	opt.normalize()
	if err := opt.Validate(b.ws); err != nil {
		return nil, err
	}
	kube := opt.kube
	if kube == nil {
		kube = kubectlRunner{bin: opt.Kubectl}
	}
	r := &jobRuntime{b: b, opt: opt, kube: kube, inflight: map[string]string{}}
	if opt.MaxParallel > 0 {
		r.sem = make(chan struct{}, opt.MaxParallel)
	}
	if err := r.probe(context.Background()); err != nil {
		return nil, err
	}
	return r, nil
}

// probe 启动探活 (三条, 每条对应一类"上线后才会暴露"的部署错误)。
//
// ⚠️ 刻意**不**探 `get namespace`: 命名空间是集群级资源, 一个只有 namespaced Role
// 的 ServiceAccount 拿不到它 —— 拿它当探活项等于要求给控制面一个 ClusterRole,
// 为了一次探活扩大权限面。真机上这一条当场把控制面挡在了 CrashLoop 里, 已改。
// 下面三条全部落在 deploy/k8s/k8s-job.yaml 那个最小 Role 之内。
func (r *jobRuntime) probe(ctx context.Context) error {
	if _, err := r.kube.run(ctx, nil, "version", "--client=true", "-o", "json"); err != nil {
		return fmt.Errorf("worker: k8s-job 探活失败 —— 跑不动 %s (%v); "+
			"控制面镜像里必须有 kubectl (见 deploy/Dockerfile.k8sjob)", r.opt.Kubectl, err)
	}
	// 写权限: 没有它, 每个派过来的节点都会在创建 Job 那一步失败。
	out, err := r.kube.run(ctx, nil, "auth", "can-i", "create", "jobs", "-n", r.opt.Namespace)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "yes") {
		return fmt.Errorf("worker: k8s-job 探活失败 —— 当前身份在命名空间 %s 里没有创建 Job 的权限 "+
			"(kubectl auth can-i create jobs → %q, %v); 见 deploy/k8s/k8s-job.yaml 的 RBAC",
			r.opt.Namespace, strings.TrimSpace(string(out)), err)
	}
	// 读权限 + 命名空间真的能用: 监视器全靠 get job/pods 才能看见"Job 起不来"。
	// 只有写没有读的话, Job 会被创建出来而失败永远归因不了。
	if _, err := r.kube.run(ctx, nil, "get", "jobs", "-n", r.opt.Namespace, "-o", "name"); err != nil {
		return fmt.Errorf("worker: k8s-job 探活失败 —— 在命名空间 %s 里读不到 Job 列表 (%v); "+
			"失败归因 (ImagePullBackOff/OOMKilled) 全靠它, 缺读权限时 Job 起不来也看不见",
			r.opt.Namespace, err)
	}
	return nil
}

func (r *jobRuntime) Name() string                    { return K8SJobRuntimeName }
func (r *jobRuntime) Capabilities() agent.RuntimeCaps { return r.opt.Caps }

// Execute 入队 + 起 Job, 事件流复用 remoteRuntime.watch。
func (r *jobRuntime) Execute(ctx context.Context, task agent.RuntimeNodeTask) (<-chan agent.NodeEvent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 并发闸: 满了就等。容量不足是排队问题, 把它变成节点失败等于用错误掩盖排队。
	if r.sem != nil {
		select {
		case r.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, fmt.Errorf("%s: 等待并发名额时上游取消 (%v; 在飞 Job 已达上限 %d)",
				K8SJobRuntimeName, ctx.Err(), r.opt.MaxParallel)
		}
	}
	released := false
	release := func() {
		if r.sem != nil && !released {
			released = true
			<-r.sem
		}
	}

	key := inflightKey(task.RunID, task.NodeID)
	// 一次性 worker 与 Job 同名: 排障时 `worker:<名字>` 标签与 pod 名字对得上。
	// 名字要到 launch 里才知道 (依赖 taskID), 所以这里只准备闭包。
	ephemeral := &remoteRuntime{
		b: r.b, name: K8SJobRuntimeName, caps: r.opt.Caps,
		inflight: map[string]string{}, // 本次执行专用; Cancel 走 jobRuntime 自己的表
	}
	ephemeral.launch = func(lctx context.Context, taskID string) (taskSupervisor, error) {
		name := jobNameFor(taskID)
		manifest, err := r.buildJobManifest(name, taskID, task)
		if err != nil {
			return nil, err
		}
		if _, err := r.kube.run(lctx, manifest, "apply", "-f", "-"); err != nil {
			return nil, fmt.Errorf("创建 Job %s 失败: %w", name, err)
		}
		r.mu.Lock()
		r.inflight[key] = taskID
		r.mu.Unlock()
		r.b.logf("[k8s-job] 已创建 Job %s (ns=%s task=%s node=%s/%s)",
			name, r.opt.Namespace, taskID, task.RunID, task.NodeID)
		return &jobSupervisor{rt: r, job: name, taskID: taskID, key: key, release: release}, nil
	}
	// worker 名必须在入队之前定下来 (RequireCaps 要钉住它), 而 taskID 是 Execute
	// 内部生成的 —— 于是 worker 名由 taskID 推导, 两边用同一个函数。
	ephemeral.workerFor = func(taskID string) string { return jobNameFor(taskID) }

	ch, err := ephemeral.Execute(ctx, task)
	if err != nil {
		release()
		return nil, err
	}
	return ch, nil
}

// Cancel 取消一个在途节点: 与远程 runtime 同一条通路 (标记取消 → worker 下次
// keepalive 取回指令), Job 本身由 watch 收尾时删除。
//
// 为什么不直接删 Job: 删 pod 是 SIGKILL 级的, 正在写共享卷的 Agent 会留下半个文件;
// 让 worker 自己收到取消指令后正常退出更干净。控制面这一侧不等它 —— 与
// remoteRuntime.Cancel 一致, 立刻判失败。
func (r *jobRuntime) Cancel(runID, nodeID string) error {
	key := inflightKey(runID, nodeID)
	r.mu.Lock()
	taskID, ok := r.inflight[key]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s: 无进行中的节点 %s/%s", K8SJobRuntimeName, runID, nodeID)
	}
	r.b.markCancel(taskID)
	return nil
}

func (r *jobRuntime) forget(key string) {
	r.mu.Lock()
	delete(r.inflight, key)
	r.mu.Unlock()
}

// ─────────────────────────────────────────────────────────────────────────────
// Job 清单
// ─────────────────────────────────────────────────────────────────────────────

var jobNameSanitize = regexp.MustCompile(`[^a-z0-9-]+`)

// jobNameFor 由任务 ID 推导 Job 名 (= 一次性 worker 名)。DNS-1123: 小写字母数字与
// 短横, 首尾必须是字母数字, ≤63 字符。
func jobNameFor(taskID string) string {
	n := "claude-go-job-" + jobNameSanitize.ReplaceAllString(strings.ToLower(strings.TrimSpace(taskID)), "-")
	if len(n) > 63 {
		n = n[:63]
	}
	return strings.Trim(n, "-")
}

// buildJobManifest 造一个"跑一次性 worker"的 Job。
func (r *jobRuntime) buildJobManifest(jobName, taskID string, task agent.RuntimeNodeTask) ([]byte, error) {
	opt := r.opt
	mode := r.b.ws.effectiveMode()

	// ① 工作目录与卷
	workDir := DefaultJobWorkDir
	var volumes []any
	var mounts []any
	switch mode {
	case WorkspaceModePVC:
		mount := strings.TrimRight(opt.WorkspaceMountPath, "/")
		want := strings.TrimRight(strings.TrimSpace(task.Workspace), "/")
		if want == "" {
			return nil, fmt.Errorf("pvc 档: 控制面没有给出团队 cwd, Job 无从确定工作目录")
		}
		// 团队 cwd 必须落在挂载点里, 否则 Job 里那个路径下什么都没有 —— 而
		// worker 的 checkWorkspace 只比路径字符串, 会通过。fail-closed。
		if want != mount && !strings.HasPrefix(want+"/", mount+"/") {
			return nil, fmt.Errorf("pvc 档: 团队 cwd %s 不在 PVC 挂载点 %s 之下, "+
				"Job 里看不到它 (调整 CLAUDE_GO_K8SJOB_MOUNT_PATH 或控制面 --cwd)", want, mount)
		}
		workDir = want
		vm := map[string]any{"name": "workspace", "mountPath": mount}
		if opt.WorkspaceSubPath != "" {
			vm["subPath"] = opt.WorkspaceSubPath
		}
		mounts = append(mounts, vm)
		volumes = append(volumes, map[string]any{
			"name":                  "workspace",
			"persistentVolumeClaim": map[string]any{"claimName": opt.WorkspacePVC},
		})
	default:
		// git 档与"未启用档位": 每个 Job 一个全新 emptyDir。
		// git 档下这正是 k8s-job 相对常驻 worker 的增量 —— 常驻 worker 只有一个
		// 检出目录, 被迫 MaxParallel=1; 这里每个 Job 自带一个。
		mounts = append(mounts, map[string]any{"name": "work", "mountPath": workDir})
		volumes = append(volumes, map[string]any{"name": "work", "emptyDir": map[string]any{}})
	}
	// 状态目录: 每个 Job 独占 (FileStore 无跨进程锁)。
	mounts = append(mounts, map[string]any{"name": "state", "mountPath": DefaultJobStateDir})
	volumes = append(volumes, map[string]any{"name": "state", "emptyDir": map[string]any{}})
	if opt.ConfigMap != "" {
		mounts = append(mounts, map[string]any{"name": "config", "mountPath": opt.ConfigMountPath})
		volumes = append(volumes, map[string]any{
			"name": "config", "configMap": map[string]any{"name": opt.ConfigMap}})
	}

	// ② worker 参数
	args := []string{
		"--control", opt.Control,
		"--name", jobName,
		"--claim", taskID,
		"--claim-timeout-sec", strconv.Itoa(opt.ClaimTimeout),
		"--cwd", workDir,
		"--max-parallel", "1",
		"--poll-ms", "500", // 一次性 worker 的首要任务是尽快认领, 不必省这点请求
	}
	if opt.ConfigMap != "" {
		args = append(args, "--config", filepath.Join(opt.ConfigMountPath, opt.ConfigFile))
	}
	if mode == WorkspaceModePVC || mode == WorkspaceModeGit {
		args = append(args, "--workspace", workDir, "--workspace-mode", string(mode))
	}
	if mode == WorkspaceModePVC && strings.TrimSpace(r.b.ws.Volume) != "" {
		args = append(args, "--workspace-volume", r.b.ws.Volume)
	}
	// 能力开关与 runtime 声明同源, 两侧不可能漂移。
	if !opt.Caps.Bash {
		args = append(args, "--no-bash")
	}
	if opt.Caps.Browser {
		args = append(args, "--browser")
	}
	if opt.Caps.GPU {
		args = append(args, "--gpu")
	}
	if opt.Caps.K8sSandbox {
		args = append(args, "--k8s-sandbox")
	}
	// ephemeral 标签让控制面的 Sync 不把这个一次性 worker 当常驻算力 (见 CapEphemeral)。
	args = append(args, "--caps", strings.Join(MergeCaps([]string{CapEphemeral}, opt.Caps.Extra), ","))

	// ③ 环境
	env := []any{
		map[string]any{"name": "POD_NAME", "valueFrom": map[string]any{
			"fieldRef": map[string]any{"fieldPath": "metadata.name"}}},
		map[string]any{"name": "CLAUDE_GO_STATE_DIR", "value": DefaultJobStateDir + "/.claude-go"},
	}
	for _, e := range opt.Env {
		k, v, _ := strings.Cut(e, "=")
		env = append(env, map[string]any{"name": strings.TrimSpace(k), "value": v})
	}
	for _, e := range opt.EnvSecret {
		name, ref, _ := strings.Cut(e, "=")
		sec, key, _ := strings.Cut(ref, "/")
		env = append(env, map[string]any{"name": strings.TrimSpace(name), "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": strings.TrimSpace(sec), "key": strings.TrimSpace(key)}}})
	}

	container := map[string]any{
		"name":            "worker",
		"image":           opt.Image,
		"imagePullPolicy": opt.ImagePullPolicy,
		"command":         []string{"/usr/local/bin/claude-go-worker"},
		"args":            args,
		"workingDir":      workDir,
		"env":             env,
		"volumeMounts":    mounts,
	}
	if opt.CPULimit != "" || opt.MemoryLimit != "" {
		lim := map[string]string{}
		if opt.CPULimit != "" {
			lim["cpu"] = opt.CPULimit
		}
		if opt.MemoryLimit != "" {
			lim["memory"] = opt.MemoryLimit
		}
		container["resources"] = map[string]any{"limits": lim, "requests": lim}
	}

	podSpec := map[string]any{
		// Never: 容器退出就是退出。重启会让一个已经把任务跑完的 worker 再起一次,
		// 它认领不到东西, 只会空转到 claim 超时。
		"restartPolicy": "Never",
		"containers":    []any{container},
		"volumes":       volumes,
	}
	if opt.ServiceAccount != "" {
		podSpec["serviceAccountName"] = opt.ServiceAccount
	}
	if len(opt.NodeSelector) > 0 {
		podSpec["nodeSelector"] = opt.NodeSelector
	}

	labels := map[string]string{
		"claude-go.runtime": K8SJobRuntimeName,
		"claude-go.task":    sanitizeLabel(taskID),
	}
	// run/node 进 annotation 而不是 label: 节点 ID 允许出现 label 不接受的字符
	// (`.`/`:`/中文…), 塞进 label 会让整个 Job 被 API server 拒绝 —— 那会把
	// "节点名起得花哨"变成"这个节点永远跑不了"。
	annotations := map[string]string{
		"claude-go.run":  task.RunID,
		"claude-go.node": task.NodeID,
		"claude-go.role": task.Role,
	}

	manifest := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name": jobName, "namespace": opt.Namespace,
			"labels": labels, "annotations": annotations,
		},
		"spec": map[string]any{
			// 见文件头「重试单层化」: 这里**不能**是别的值。
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": opt.TTLSeconds,
			"activeDeadlineSeconds":   opt.ActiveDeadline,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels, "annotations": annotations},
				"spec":     podSpec,
			},
		},
	}
	return json.Marshal(manifest)
}

// ─────────────────────────────────────────────────────────────────────────────
// jobSupervisor —— 把 Job/pod 的真实状态翻译成"节点失败"
// ─────────────────────────────────────────────────────────────────────────────

type jobSupervisor struct {
	rt      *jobRuntime
	job     string
	taskID  string
	key     string
	release func()

	lastFatal string
	streak    int
	terminal  bool // 最近一次观察到的 Job 是否已终结 (决定 cleanup 删不删)
}

// check 每个轮询周期一次。返回非 nil ⇒ 控制面立刻判该节点失败。
func (s *jobSupervisor) check() error {
	st, err := s.rt.jobStatus(context.Background(), s.job)
	if err != nil {
		// 读不出状态**不判死**: kubectl 抖动/apiserver 限流是观测故障, 不是执行故障。
		// 真的挂了的话, Job 的 activeDeadlineSeconds 与队列租约仍然兜得住。
		s.streak, s.lastFatal = 0, ""
		return nil
	}
	s.terminal = st.terminal
	if st.fatal == "" {
		s.streak, s.lastFatal = 0, ""
		return nil
	}
	if st.fatal == s.lastFatal {
		s.streak++
	} else {
		s.lastFatal, s.streak = st.fatal, 1
	}
	if s.streak < jobFatalConfirm {
		return nil
	}
	return fmt.Errorf("Job %s 无法交付该节点: %s", s.job, st.fatal)
}

// cleanup watch 结束时收尾 (成功/失败/取消都会走到)。
func (s *jobSupervisor) cleanup() {
	s.rt.forget(s.key)
	if s.release != nil {
		s.release()
	}
	if s.terminal {
		// 已终结的 Job 留给 ttlSecondsAfterFinished: `kubectl logs` 是看 worker
		// 崩溃现场的唯一入口, 立刻删掉等于把证据一起删了。
		s.rt.b.logf("[k8s-job] Job %s 已终结, 留待 TTL(%ds) 回收以便排障", s.job, s.rt.opt.TTLSeconds)
		return
	}
	// 尚未终结: 控制面已经不认它的产出了, 它还在烧 pod ——删。
	ctx, cancel := context.WithTimeout(context.Background(), jobKubectlTimeout)
	defer cancel()
	if _, err := s.rt.kube.run(ctx, nil, "delete", "job", s.job,
		"-n", s.rt.opt.Namespace, "--ignore-not-found=true", "--wait=false"); err != nil {
		s.rt.b.logf("[k8s-job] 删除 Job %s 失败: %v (TTL 仍会兜底)", s.job, err)
	}
}

// jobState 一次状态观察的归约结果。
type jobState struct {
	terminal bool   // Job 已终结 (Complete 或 Failed)
	fatal    string // 非空 = 该节点交付不出来了, 原文带回控制面
}

// jobStatus 读 Job 与它的 pod, 归约成"能不能交付"。
//
// 两个来源缺一不可:
//   - Job 的 conditions 只在**终结之后**才有 (DeadlineExceeded/BackoffLimitExceeded),
//     而 ImagePullBackOff 的 Job 永远不终结, 只能从 pod 的 containerStatuses 里看到;
//   - pod 的 waiting.reason 又看不到"Job 已经成功了但 worker 没上报"这种情况。
func (r *jobRuntime) jobStatus(ctx context.Context, jobName string) (jobState, error) {
	var st jobState
	raw, err := r.kube.run(ctx, nil, "get", "job", jobName, "-n", r.opt.Namespace, "-o", "json")
	if err != nil {
		return st, err
	}
	var job struct {
		Status struct {
			Active     int `json:"active"`
			Succeeded  int `json:"succeeded"`
			Failed     int `json:"failed"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return st, fmt.Errorf("解析 Job %s 状态失败: %w", jobName, err)
	}
	for _, c := range job.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Failed":
			st.terminal = true
			st.fatal = fmt.Sprintf("Job 已失败 (reason=%s %s)", c.Reason, truncate(c.Message, 200))
			return st, nil
		case "Complete":
			st.terminal = true
			// Job 成功但任务还没终态 = worker 没把结果交回来 (被 OOMKill、
			// 上报被拒、或者根本没认领到)。这是**必须**判失败的场景: 不判就等着
			// 队列租约超时, 归因还会指向"worker 掉线"。
			st.fatal = "Job 已结束但队列里没有终态 —— worker 退出前没能把结果交回控制面"
			return st, nil
		}
	}
	if job.Status.Failed > 0 {
		st.terminal = true
		st.fatal = fmt.Sprintf("Job 的 pod 失败了 %d 次 (backoffLimit=0, 不重试)", job.Status.Failed)
		return st, nil
	}
	// Job 还活着: 看 pod 是不是卡在起不来的状态上。
	if reason := r.podFatalReason(ctx, jobName); reason != "" {
		st.fatal = reason
	}
	return st, nil
}

// podBlockers pod 里"再等也好不了"的容器等待原因。
//
// 刻意**不含** Unschedulable / Pending: 排队等资源是正常的, 把它判死会让集群一忙
// 就整片节点失败; 那种情况由 activeDeadlineSeconds 兜底。
var podBlockers = map[string]bool{
	"ImagePullBackOff": true,
	"ErrImagePull":     true,
	"InvalidImageName": true,
	// imagePullPolicy=Never 且节点上没这个镜像 —— kind/minikube 部署的常见拼写错,
	// 真机上就是这一条 (它不会退化成 ImagePullBackOff, kubelet 根本不去拉)。
	"ErrImageNeverPull":          true,
	"CreateContainerConfigError": true, // 典型: ConfigMap/Secret 不存在
	"CreateContainerError":       true,
}

func (r *jobRuntime) podFatalReason(ctx context.Context, jobName string) string {
	raw, err := r.kube.run(ctx, nil, "get", "pods", "-n", r.opt.Namespace,
		"-l", "job-name="+jobName, "-o", "json")
	if err != nil {
		return ""
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					State struct {
						Waiting *struct {
							Reason  string `json:"reason"`
							Message string `json:"message"`
						} `json:"waiting"`
						Terminated *struct {
							Reason   string `json:"reason"`
							ExitCode int    `json:"exitCode"`
						} `json:"terminated"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return ""
	}
	for _, p := range list.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && podBlockers[w.Reason] {
				return fmt.Sprintf("pod %s 起不来: %s (%s)", p.Metadata.Name, w.Reason, truncate(w.Message, 200))
			}
			if tm := cs.State.Terminated; tm != nil && tm.Reason == "OOMKilled" {
				return fmt.Sprintf("pod %s 被 OOMKilled (退出码 %d); 调大 CLAUDE_GO_K8SJOB_MEMORY",
					p.Metadata.Name, tm.ExitCode)
			}
		}
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────────
// 小工具
// ─────────────────────────────────────────────────────────────────────────────

var labelSanitize = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeLabel(s string) string {
	v := labelSanitize.ReplaceAllString(strings.TrimSpace(s), "-")
	if len(v) > 63 {
		v = v[:63]
	}
	return strings.Trim(v, "-._")
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

func parseKV(items []string) map[string]string {
	if len(items) == 0 {
		return nil
	}
	m := map[string]string{}
	for _, it := range items {
		if k, v, ok := strings.Cut(it, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func envInt(name string) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return 0
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
