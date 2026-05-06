# claude-go Agent 沙盒机制现状与方案

日期: 2026-05-06

仓库: `/Users/huaquan.liang/Documents/GitHub/ruflo/claude-go`

## 结论

当前 `claude-go` 没有真正的 OS 级沙盒机制。现有能力主要是权限策略、工具 profile、少量危险命令启发式规则和 `cwd` 工作目录约束；它们可以减少误调用，但不能阻止已经被允许的 `Shell`、文件工具、浏览器或 MCP 工具访问宿主机资源。

本机配置 `/Users/huaquan.liang/.claude-go/config/config.json` 当前为 `permissionMode=bypass`、`cwd=/Users/huaquan.liang/test`，未配置 `sandbox` 字段。换句话说，飞书 Bot 或研发团队一旦进入可执行工具路径，默认更接近“信任执行”，不是“隔离执行”。

当前 Codex 会话自身也运行在 `danger-full-access` 环境下，因此我本次确认和写文档没有受到本地文件系统沙盒限制。

## 当前实现确认

| 机制 | 代码位置 | 已有能力 | 缺口 |
| --- | --- | --- | --- |
| 权限模式 | `pkg/permissions/permissions.go:39`、`cmd/claude-go/main.go:221` | `default/auto/plan/bypass/dontAsk/acceptEdits`，支持 allow/deny/content rule | 只是执行前判断，`bypass` 下默认放行，不提供 OS 隔离 |
| 配置合并 | `cmd/claude-go/main.go:2005` | CLI 默认 `bypass`，配置或 project settings 可覆盖 | 配置 schema 没有 `sandbox`，无法声明文件、网络、进程边界 |
| 飞书默认 | `pkg/feishu/types.go:172` | Bot 默认 `PermissionMode: "bypass"` | 飞书是长时间在线入口，默认高权限风险更大 |
| Shell 工具 | `pkg/tool/builtin/bash.go:88`、`pkg/tool/builtin/bash.go:185` | Plan 模式禁用 Shell，普通模式走权限确认；命令有 30s 默认超时 | 实际直接 `exec.CommandContext(ctx, "bash", "-c", command)` 在宿主机执行，没有 chroot/container/seccomp/cgroup/netns |
| 文件路径 | `pkg/tool/builtin/fileread.go:188` | 相对路径按 `cwd` 展开，支持 `~` | 没有强制路径 jail；绝对路径和 `~/` 可指向工作区外 |
| 工具 Profile | `pkg/tool/builtin/profile.go:5` | `chat/research/coding/team/admin` 缩小工具暴露面 | 减少 prompt 和误用面，但不是安全边界 |
| 规则引擎 | `pkg/agent/adversarial.go:540` | 禁止部分非目标删除、疑似密钥写入、无限循环模式 | 启发式 shell 解析，覆盖面有限，不等价于沙盒 |
| 浏览器/媒体 | `pkg/browser/stealth.go:8`、`pkg/media/engine.go:99` | 可启动 headless Chrome | 显式 `--no-sandbox`，如果处理不可信网页或文件，应视为高风险 |

一句话判断: `claude-go` 目前有“权限门禁”，没有“沙盒边界”。如果模型、prompt injection、恶意仓库脚本或 MCP 工具越过权限门禁，宿主机文件、网络、凭据、桌面环境仍可能暴露。

## 研发团队测试 OOM 专项方案

你现在最痛的不是“模型能不能删文件”，而是研发团队在本地验证时运行错误代码、无限分配内存、无限输出或 fork/并发爆炸，导致 `claude-go` 主进程和宿主机一起被拖垮。这个问题需要优先做“执行隔离沙盒”，并且必须同时限制子进程资源和主进程输出缓冲。

### 当前 OOM 风险点

| 风险点 | 代码位置 | 现状 | OOM 触发方式 |
| --- | --- | --- | --- |
| 本地验证会真实运行 build/test | `pkg/agent/orchestrator.go:1955`、`pkg/agent/orchestrator.go:1960`、`pkg/agent/orchestrator.go:3264`、`pkg/agent/orchestrator.go:3267` | Leaf 和 verification 会调用 `runBuildCheckScoped` / `runTestCheckLang` | 错误测试代码、无限循环、巨量数据结构、并发 goroutine 会真实占用宿主资源 |
| Linux 限制在 macOS 上失效 | `pkg/agent/workflow.go:4451`、`pkg/agent/workflow.go:4497` | 先尝试 `systemd-run` / `prlimit`，失败后直接运行 | 当前机器是 Darwin，`systemd-run` 和 `prlimit` 不存在，最终落到无限制 `exec.CommandContext` |
| 输出先无限进入内存再截断 | `pkg/agent/workflow.go:4471`、`pkg/agent/workflow.go:4488`、`pkg/agent/workflow.go:4500` | `CombinedOutput()` 会把全部 stdout/stderr 收到内存 | 错误测试 `for { fmt.Println(...) }` 会撑爆主进程内存 |
| Shell 工具同样无限缓冲 | `pkg/tool/builtin/bash.go:188` | `bytes.Buffer` 收 stdout/stderr，`truncateToMaxRunes` 在命令结束后才执行 | agent 执行任意刷屏命令时，主进程先 OOM，根本等不到截断 |
| Hook 执行也无限缓冲 | `pkg/hooks/hooks.go:351` | hook stdout/stderr 使用 `bytes.Buffer` | 恶意或错误 hook 输出过大时同样影响主进程 |

所以单独给测试命令加 `go test -timeout=30s` 不够。`go test -timeout` 只限制 Go 测试逻辑超时，不限制进程 RSS、子进程、输出、fork 数、网络和文件写入；而 `CombinedOutput`/`bytes.Buffer` 会让“输出型 OOM”发生在 `claude-go` 主进程内。

### OOM 防护目标

研发团队沙盒的首要目标:

- 错误代码可以 OOM，但只能 OOM 在 sandbox/container/microVM 里，不能拖垮 `claude-go` 主进程。
- 错误代码可以无限输出，但主进程只保留有限 ring buffer，并主动杀掉命令。
- 错误代码可以超时，但必须杀掉整个进程树，而不是只取消外层 `bash`。
- 错误代码失败后要被分类成 `sandbox_oom`、`sandbox_timeout`、`sandbox_output_limit`、`sandbox_pids_limit`，反馈给 coder 修复，不再当成普通编译错误无限重试。
- 飞书 Bot 和研发团队默认必须使用 sandbox runner；没有 sandbox 时不允许自动研发团队跑本地验证。

### 推荐实现: TeamVerificationSandbox

先不要把所有工具一次性沙盒化，第一阶段优先接管研发团队的 build/test/verification 路径。也就是把 `runLimitedCommand()` 从“systemd/prlimit/direct exec”改成“优先 SandboxRunner，最后才兼容 direct exec”。

建议新增:

```go
type SandboxRunner interface {
    Run(ctx context.Context, spec CommandSpec) (*CommandResult, error)
}

type CommandSpec struct {
    Args          []string
    Cwd           string
    Workdir       string
    Profile       string
    ReadRoots     []string
    WriteRoots    []string
    NetworkMode   string
    Limits        ResourceLimits
    OutputLimits  OutputLimits
    Env           map[string]string
    SandboxID     string
    Purpose       string // build | test | verification | shell | hook
}

type ResourceLimits struct {
    TimeoutSec int
    MemoryMB   int
    CPUs       string
    Pids       int
    DiskMB     int
}

type OutputLimits struct {
    MaxStdoutBytes int
    MaxStderrBytes int
    MaxTotalBytes  int
    TailBytes      int
}

type CommandResult struct {
    ExitCode      int
    StdoutPreview string
    StderrPreview string
    OutputPath    string
    TimedOut      bool
    OOMKilled     bool
    OutputLimited bool
    DurationMS    int64
}
```

执行规则:

- `runBuildCheckScoped` 和 `runTestCheckLang` 只拿 `StdoutPreview/StderrPreview` 拼错误，不再拿完整 `[]byte`。
- 完整输出写到 `.claude-go/sandboxes/<run-id>/<task-id>/stdout.log` 和 `stderr.log`，但单文件也有限额，例如 10MB。
- 内存里只保留 `head 64KB + tail 64KB`，或者只保留 tail ring buffer。
- 一旦输出超过 `MaxTotalBytes`，立即终止容器/进程树，返回 `OutputLimited=true`。
- 一旦容器 exit code 为 137 或 `docker inspect` 显示 `OOMKilled=true`，返回 `OOMKilled=true`。

### DockerRunner 基线

当前机器上 Docker 可用，`systemd-run/prlimit` 不可用。因此 macOS 上最现实的第一版是 Docker/Colima/Docker Desktop runner。

Go 测试建议基线:

```bash
docker run --rm \
  --name claude-go-test-${RUN_ID}-${TASK_ID} \
  --network none \
  --cpus 2 \
  --memory 2g \
  --memory-swap 2g \
  --pids-limit 256 \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --read-only \
  --tmpfs /tmp:rw,nosuid,size=512m \
  --tmpfs /home/agent:rw,nosuid,size=512m \
  -e HOME=/home/agent \
  -e GOCACHE=/tmp/go-cache \
  -e GOTMPDIR=/tmp \
  -v "${WORKTREE}:/workspace:rw" \
  -w /workspace \
  golang:1.24 \
  go test -count=1 -timeout=30s -parallel=4 ./...
```

关键点:

- `--memory` 和 `--memory-swap` 固定相同值，避免容器靠 swap 把宿主机拖慢到假死。
- `--pids-limit` 防 fork bomb 和无限子进程。
- `--network none` 防错误代码访问内网或外传数据；依赖下载单独走 allowlist 阶段。
- `--read-only + tmpfs` 防测试污染容器根文件系统。
- workspace 是唯一可写挂载，后续再加 path jail。
- `docker kill` 可以清理整个容器进程树，比 `exec.CommandContext` 只杀父进程更可靠。

### macOS / Ubuntu 自动适配

沙盒选型不能只看“安全强度”，还要看启动速度、资源占用和运维成本。研发团队测试场景的最佳策略是 `sandbox.mode=auto`: 启动时探测平台能力，按优先级选择最轻但足够保护主进程的 runner；如果找不到可靠资源限制，就拒绝自动研发团队验证，而不是裸跑。

当前本机探测结果:

- `GOOS=darwin`，是 macOS。
- Docker CLI 存在: `/opt/homebrew/bin/docker`。
- Docker daemon 当前未启动，连接 `/Users/huaquan.liang/.colima/default/docker.sock` 失败。
- `systemd-run` / `prlimit` 在 macOS 不可用，所以当前代码里的 Linux 限制会回退到无限制 direct exec。

平台推荐矩阵:

| 平台 | 最优默认 | 启动速度 | 资源占用 | 管理成本 | 适用场景 | 不足 |
| --- | --- | --- | --- | --- | --- | --- |
| macOS, Docker/Colima 已启动 | `DockerRunner` | warm 时快 | 中等，一个 Linux VM 常驻 | 低 | 团队 build/test/verification 默认 | daemon 冷启动慢 |
| macOS, Docker 未启动但允许自启 | `ColimaManagedRunner` 或 Docker Desktop auto-start | 首次慢，后续快 | 可配置小 VM | 中低 | 飞书 Bot 长时间在线，按需启动 | 需要处理启动等待和失败提示 |
| macOS, Docker 不可用 | `OutputLimiter + ProcessRunner` 仅限人工显式允许 | 快 | 低 | 低 | 本地手动调试、非团队自动验证 | 不能防测试 OOM，只能防输出 OOM |
| Ubuntu, Docker rootful/cgroup 可用 | `DockerRunner` | warm 时快 | 中等 | 低 | 默认通用方案，隔离强且一致 | 需要 Docker daemon 权限 |
| Ubuntu, systemd cgroup + bubblewrap 可用 | `SystemdBwrapRunner` | 最快 | 最低 | 中 | 已安装本机工具链的 Go 测试，追求低开销 | 依赖宿主 Go/工具链，环境复现弱于容器 |
| Ubuntu, rootless Docker | `DockerRunner(rootless)`，仅当 cgroup v2 + systemd delegation 生效 | warm 时快 | 中等 | 中 | 多用户环境降低 daemon 权限 | 资源限制可能因 cgroup 条件不满足而失效 |
| Ubuntu, 无 Docker/无 systemd cgroup | unsafe fallback 禁止 team auto | 快 | 低 | 低 | 只能手工调试 | 不能可靠解决 OOM |

自动选择算法:

```text
1. 读取 sandbox.mode:
   - off: 只允许 chat/research，不允许 team verification 自动执行。
   - auto: 按平台探测选择。
   - docker/systemd-bwrap/process/remote: 使用指定 runner，不满足则 fail fast。

2. 探测 OS:
   - darwin: 进入 macOS 策略。
   - linux: 进入 Linux 策略。

3. macOS 策略:
   a. 2 秒内执行 docker info 成功，并且 image 已缓存 -> DockerRunner(warm)。
   b. docker CLI 存在但 daemon 未启动:
      - 如果 sandbox.autoStart=true 且 colima 存在 -> colima start --profile claude-go-sandbox --cpu 2 --memory 4 --disk 20。
      - 如果 Docker Desktop 可被启动且用户允许 -> 尝试启动并等待 readiness。
      - 否则 fail fast: "Docker daemon 未启动，研发团队验证需要沙盒"。
   c. Docker 不可用 -> 仅允许 OutputLimiter ProcessRunner，不允许 team auto。

4. Ubuntu Linux 策略:
   a. docker info 成功，并通过一次 canary 验证 --memory/--pids-limit 生效 -> DockerRunner。
   b. 若 systemd-run --user 可用，/sys/fs/cgroup 为 cgroup v2，且 bwrap 可用 -> SystemdBwrapRunner。
   c. 若 rootless docker 可用但 cgroup delegation 不满足 -> 不作为 OOM 防护 runner。
   d. 全部失败 -> fail fast。

5. 每次选择都写入 metrics:
   sandbox_selected_runner, sandbox_probe_latency_ms, sandbox_daemon_warm, sandbox_resource_limit_verified。
```

### Runner 选择细节

`DockerRunner` 适合作为跨平台统一层，因为 macOS Docker/Colima 本质也是 Linux VM，Ubuntu Docker 直接使用 cgroup。为了低管理成本，`claude-go` 只依赖 Docker CLI，不直接绑定 Docker Desktop、Colima 或 Podman API。

macOS 建议:

- 默认使用当前 `docker context`，不要强行切换用户环境。
- 如果 daemon 未启动，先返回清晰错误；只有配置 `sandbox.autoStart=true` 时才尝试启动 Colima/Docker Desktop。
- 推荐单独 Colima profile: `claude-go-sandbox`，避免污染用户日常 Docker。
- Colima 资源默认 `2 CPU / 4GB RAM / 20GB disk`，只服务研发团队测试；大型 C++/Rust 任务再按任务提升。
- 设置 `keepWarmSec`: 飞书 Bot 可 10-30 分钟内保持 daemon warm；CLI 单次执行默认不常驻，降低笔记本耗电。
- 如果使用 Docker Desktop，可利用 Resource Saver 减少空闲 CPU/内存，但要接受冷启动延迟。

Ubuntu 建议:

- 首选 rootful Docker 或受控 CI runner，因为 `--memory`、`--cpus`、`--pids-limit` 语义最清楚。
- 对“已安装 Go 工具链、只需要快速跑测试”的机器，可用 `SystemdBwrapRunner`:
  - `systemd-run --user --scope -p MemoryMax=2G -p CPUQuota=200% -p TasksMax=256 ...`
  - 外层用 `bubblewrap` 做 mount namespace/path jail。
  - 输出仍走 `OutputLimiter`。
- rootless Docker 只有在 cgroup v2 + systemd delegation 生效时才可作为 OOM 防护；否则资源限制可能不生效，必须在 probe 阶段识别并降级为不可用于 team auto。
- Ubuntu server 上可以配置一个长期运行的 `claude-go-sandbox-worker`，减少每次探测成本；macOS 笔记本不建议默认常驻 worker。

### 快启动与低资源策略

快启动不是简单地“常驻容器”，否则 macOS 笔记本会一直吃内存。建议分入口处理:

| 入口 | 策略 |
| --- | --- |
| 飞书 Bot | 启动时只做轻量 probe，不拉大镜像；第一次 `/go development` 再 lazy warm；完成后 `keepWarmSec=1800` |
| CLI 单次 run | 不 auto-start Docker；如果 daemon 未启动，提示用户或使用显式 `--sandbox=process-unsafe` |
| Dashboard/长期研发团队 | 可开启 warm worker pool，默认 1 个，最大 2 个 |
| CI/Ubuntu server | Docker daemon 常驻，镜像预拉取，cache volume 常驻 |

低资源默认值:

```json
{
  "sandbox": {
    "mode": "auto",
    "autoStart": false,
    "keepWarmSec": 1800,
    "probeTimeoutMs": 2000,
    "allowUnsafeProcessForTeam": false,
    "macos": {
      "preferred": ["docker-warm", "colima-managed"],
      "colimaProfile": "claude-go-sandbox",
      "colimaCPU": 2,
      "colimaMemoryGB": 4,
      "colimaDiskGB": 20
    },
    "ubuntu": {
      "preferred": ["docker", "systemd-bwrap"],
      "requireCgroupLimitVerification": true
    }
  }
}
```

镜像与缓存策略:

- Go 默认镜像可先使用 `golang:1.24` 保证兼容；后续构建 `ghcr.io/ruflo/claude-go-sandbox-go:1.24`，预置 `git/ca-certificates/ripgrep/make`，减少每次安装成本。
- 使用 per-project named volume 保存 `GOMODCACHE`，避免每次下载依赖；按项目 hash 隔离，设置 TTL 清理。
- 依赖下载和测试分离: 下载阶段可短暂 allowlist 网络，测试阶段 `network=none`。
- 容器加 label: `claude-go.sandbox=true`、`teamRunID`、`taskID`，便于崩溃后清理。

### 健康检查与降级

新增 `claude-go doctor sandbox`:

```text
OS: darwin
Docker CLI: found
Docker daemon: not running
Colima: found/not found
Selected runner: none
Team verification: blocked
Reason: no active sandbox runner; process runner cannot enforce memory limits on macOS
Fix: start Docker/Colima or set sandbox.autoStart=true
```

Ubuntu 输出:

```text
OS: linux
cgroup: v2
docker: running
memory limit canary: passed
pids limit canary: passed
selected runner: docker
team verification: enabled
```

降级原则:

- 可以为了速度从 Docker 降级到 `SystemdBwrapRunner`，前提是内存、进程数、输出限制都验证通过。
- 不可以为了速度从 Docker 降级到裸 `exec.Command` 跑 team verification。
- 如果只剩裸进程 runner，允许 chat/research，允许人工显式单次 Shell，但禁止 `/go development` 自动测试。

Go 任务资源建议:

| 场景 | Memory | CPU | PIDs | Timeout | Output |
| --- | --- | --- | --- | --- | --- |
| Leaf build | 1.5GB | 2 | 256 | 45s | 2MB preview / 10MB file |
| Leaf test | 2GB | 2 | 256 | 60s | 2MB preview / 10MB file |
| Final verification | 4GB | 4 | 512 | 120s | 4MB preview / 20MB file |
| C++/Rust 重型构建 | 8-16GB | 4 | 1024 | 180-600s | 8MB preview / 50MB file |

### 依赖下载策略

研发团队测试不应默认开放网络，但 Go/Rust/Node 项目有时需要下载依赖。建议拆两段:

1. `dependency-sync` 阶段: 使用 `network=allowlist`，只允许 `proxy.golang.org`、`sum.golang.org`、`github.com` 等必要域名，执行 `go mod download` 或对应语言下载命令。
2. `build/test` 阶段: 使用 `network=none`，只挂载已准备好的依赖缓存。

Go 可挂载受控 cache:

```bash
-v claude-go-gomodcache-${PROJECT_HASH}:/go/pkg/mod:rw
-e GOMODCACHE=/go/pkg/mod
```

这个 cache 需要配额、定期清理和按项目隔离，避免一个项目污染另一个项目。

### 主进程输出限流

这是必须和 Docker 同步做的，否则容器不 OOM，主进程仍可能因为日志 OOM。

不要再使用:

```go
out, err := cmd.CombinedOutput()
var stdout, stderr bytes.Buffer
cmd.Stdout = &stdout
cmd.Stderr = &stderr
```

改为:

```go
stdout, _ := cmd.StdoutPipe()
stderr, _ := cmd.StderrPipe()
limiter := NewOutputLimiter(OutputLimits{
    MaxTotalBytes:  2 << 20,
    TailBytes:      64 << 10,
    MaxFileBytes:   10 << 20,
})
go limiter.Copy("stdout", stdout)
go limiter.Copy("stderr", stderr)
```

`OutputLimiter` 行为:

- 流式写入受控日志文件。
- 内存只保留 ring buffer。
- 超过限制时调用 `cancel()` 和 `runner.Kill(sandboxID)`。
- 返回错误摘要: `output exceeded 2MB; killed sandbox; see stdout.log/stderr.log`。

### 失败分类和研发团队反馈

不要把 OOM 当成普通测试失败。建议分类:

| 信号 | 分类 | 任务状态 | 给 coder 的反馈 |
| --- | --- | --- | --- |
| Docker `OOMKilled=true` 或 exit 137 | `sandbox_oom` | failed，阻塞依赖 | 测试/代码超过内存预算；优先检查无限增长 slice/map、无界 goroutine、递归、全量载入数据 |
| output limit exceeded | `sandbox_output_limit` | failed，阻塞依赖 | 测试输出无界；删除循环日志，使用断言和小样本 |
| timeout | `sandbox_timeout` | failed 或 split | 检查死锁、无限循环、长轮询；必要时拆分 leaf |
| pids limit | `sandbox_pids_limit` | failed | 检查 goroutine/子进程爆炸、fork bomb |
| network denied | `sandbox_network_denied` | failed 或 dependency-sync | 明确依赖下载需求，不允许测试阶段联网 |

反馈 prompt 示例:

```text
本地验证在沙盒中失败，分类: sandbox_oom。
容器内存上限: 2048MB，exit code: 137，OOMKilled=true。
这不是普通编译错误。请只修改本 Leaf 相关文件，优先排查:
1. 无界 slice/map/cache 增长
2. 无限 goroutine / worker pool 未关闭
3. 测试一次性构造过大数据集
4. 递归或循环退出条件错误
5. 日志输出过多导致 output limit
```

### 配置草案补充

```json
{
  "sandbox": {
    "enabled": true,
    "requiredForTeam": true,
    "requiredForVerification": true,
    "runner": "docker",
    "image": "golang:1.24",
    "outputDir": ".claude-go/sandboxes",
    "profiles": {
      "team-go-test": {
        "network": {"mode": "none"},
        "limits": {
          "memoryMB": 2048,
          "cpus": "2",
          "pids": 256,
          "timeoutSec": 60
        },
        "outputLimits": {
          "maxTotalBytes": 2097152,
          "tailBytes": 65536,
          "maxFileBytes": 10485760
        }
      },
      "team-go-final-verification": {
        "network": {"mode": "none"},
        "limits": {
          "memoryMB": 4096,
          "cpus": "4",
          "pids": 512,
          "timeoutSec": 120
        },
        "outputLimits": {
          "maxTotalBytes": 4194304,
          "tailBytes": 131072,
          "maxFileBytes": 20971520
        }
      }
    }
  }
}
```

### 接入顺序

建议按这个顺序做，收益最大:

1. 先实现 `OutputLimiter`，替换 `BashTool`、`runLimitedCommand`、hook command 的 `bytes.Buffer/CombinedOutput`。这一步即使还没 Docker，也能立刻降低主进程 OOM。
2. 实现 `DockerRunner`，先只接管 `runLimitedCommand`，覆盖研发团队 build/test/verification。
3. `feishu` 和 team 启动时，如果 `sandbox.requiredForTeam=true` 但 Docker 不可用，直接拒绝研发团队执行，并提示 `docker` 未安装/未启动。
4. metrics 记录 `sandbox_oom_count`、`sandbox_output_limit_count`、`sandbox_timeout_count`、`sandbox_peak_memory_mb`、`sandbox_exit_code`、`sandbox_output_bytes`。
5. Dashboard 增加 `delivered_with_remediation` 和 `failed_by_sandbox_guard`，避免把“沙盒保护成功杀掉错误测试”误读为系统异常。

### OOM 回归测试

最小回归集:

- `TestSandbox_OOMKilled`: 生成 Go 测试分配 8GB slice，在 512MB sandbox 下应 exit 137/`OOMKilled=true`，`claude-go` 主进程 RSS 不明显增长。
- `TestSandbox_OutputLimited`: Go 测试无限 `fmt.Println(strings.Repeat("x", 1024))`，应触发 `OutputLimited=true` 并杀容器。
- `TestSandbox_TimeoutKillsTree`: shell 启动子进程 `sleep 999 & wait`，timeout 后容器内无残留进程。
- `TestSandbox_NetworkDenied`: 测试中 `http.Get("https://example.com")` 在 `network=none` 下失败。
- `TestSandbox_PathJail`: 测试尝试写 `~/.zshrc` 或 `../outside`，应失败或只能影响容器临时 HOME。

## 风险模型

需要保护的资产:

- 宿主机文件: `$HOME`、SSH key、浏览器 profile、公司代码、`.env`、`~/.claude-go`、`~/.config`。
- 凭据: 模型 API key、飞书 token、GitHub token、MCP server token、cloud CLI token。
- 网络: 内网服务、metadata endpoint、本地数据库、代理、企业系统。
- 持久化能力: `cron`、launch agent、后台进程、shell rc 文件、git hooks、IDE 配置。
- 团队任务状态: `.claude-go` metrics、blackboard、prompt debug、agent 输出目录。

主要威胁:

- Prompt injection 诱导读取敏感文件或上传到外网。
- 恶意仓库脚本在 `go test`、`npm install`、`make` 中执行任意命令。
- 长时间飞书 Bot/研发团队在 `bypass` 下被误触发高危命令。
- MCP 工具 schema 或第三方工具扩大权限面，绕过内置工具规则。
- 浏览器 `--no-sandbox` 访问不可信页面时扩大进程逃逸风险。
- 多 agent 并发写同一目录，破坏 workspace 或互相污染凭据/缓存。

## 业界方案对照

| 产品/方案 | 关键做法 | 对 claude-go 的启发 |
| --- | --- | --- |
| OpenAI Codex CLI | 本地 CLI 可读写和运行目录内代码；官方文档把 approval mode 作为用户控制面，Windows 也有 sandbox；Full Auto 强调沙盒和网络限制 | `bypass/auto` 必须和 OS 沙盒解耦；“允许自动执行”不能等于“宿主机全权限” |
| Claude Code | 细粒度权限、模式、allow/ask/deny；官方明确 `bypassPermissions` 只应在容器或 VM 等隔离环境中使用 | `claude-go` 当前默认 bypass 与飞书长时运行组合不安全，至少应默认 `auto` 或要求 sandbox |
| GitHub Copilot cloud agent | 在 GitHub Actions powered ephemeral environment 中执行；有防火墙/allowlist，但官方也说明 firewall 不是完整安全方案 | 飞书/研发团队适合“远端 ephemeral runner + 网络出口策略”，不要只靠本机 prompt 规则 |
| Cursor | 默认对敏感动作要求手动批准；终端命令默认需批准；allowlist 被描述为 best effort，不应当成安全控制 | 本地 IDE 类工具的权限提示可保留，但不能替代 OS 沙盒 |
| Devin for Terminal | `--sandbox` 下进入 Autonomous，Shell/Fetch 自动批准依赖 OS-level sandbox 约束；direct edit 仍单独处理 | `claude-go` 可以采用同样思想: 自动模式只有在 sandbox active 时才允许全自动 Shell |
| Google Jules | 将仓库 clone 到安全 Google Cloud VM，异步执行，隔离数据并展示 plan/diff | 飞书复杂研发任务应优先云 VM 或微 VM，不应长期跑在用户主机 |
| OpenHands | V1 明确 sandbox provider: Docker 推荐、Process 不安全但快、Remote 用于托管 | `claude-go` 可实现 `process/docker/remote` 三种 runner，逐步迁移 |
| Docker Sandboxes | 面向 AI coding agents 的微 VM sandbox，每个 sandbox 有独立 Docker daemon、文件系统和网络；密钥由宿主代理注入，不直接放进 sandbox | 适合本机 macOS/Linux 的短期落地路径，尤其适合 Claude/Codex/Go agent |
| E2B | 为 agent 创建按需 Linux VM，提供文件、进程、桌面、CI 场景 | 适合飞书 Bot 和后台团队任务的 remote sandbox provider |
| gVisor | 通过 userspace application kernel 隔离容器 syscall 面，兼容 Docker/Kubernetes | 比普通 Docker 强，适合多租户或高风险代码执行 |
| Firecracker/Kata | 微 VM/轻量 VM，硬件虚拟化隔离，降低攻击面 | 高风险、企业飞书、长期后台研发团队建议最终采用微 VM |
| nsjail/bubblewrap | Linux namespace、cgroup、rlimit、seccomp，轻量进程沙盒 | 适合 Linux 本机快速实现 `SandboxRunner`，但 macOS 不能直接复用 |
| WebContainers | 在浏览器安全沙盒内运行 Node.js/WASM，不能覆盖 Go/系统级构建 | 适合前端 demo/文档，不适合 `claude-go` 的通用 Go 研发团队 |

## 沙盒方案实现原理

业界这些方案看起来名字很多，但底层原理可以拆成几个可组合的 primitive。`claude-go` 自研时不应该把“沙盒”理解成一个大黑盒，而应该把它拆成文件系统、进程、资源、网络、系统调用、虚拟化边界、输出背压七层。

### 核心原语

| 原语 | 解决什么问题 | 典型实现 | 对 OOM/错误测试的价值 |
| --- | --- | --- | --- |
| Namespace | 让进程“看不见”宿主资源 | Linux mount/pid/net/user/ipc/uts namespace，Docker、bubblewrap、nsjail 都使用 | 隔离进程树、网络、挂载视图，避免测试扫到宿主机 |
| cgroup / resource control | 限制进程组资源 | cgroup v2 `memory.max`、`pids.max`、`cpu.max`；systemd `MemoryMax/TasksMax/CPUQuota`；Docker `--memory/--pids-limit/--cpus` | 防止错误代码吃满宿主内存、fork bomb、CPU 打满 |
| rlimit | 单进程资源限制 | `RLIMIT_AS`、`RLIMIT_CPU`、`RLIMIT_NOFILE`、`RLIMIT_NPROC` | 简单但不完整；对 Go/多进程树和 macOS 不够可靠 |
| Mount / rootfs jail | 限制文件可见性和可写性 | `pivot_root/chroot`、bind mount、tmpfs、read-only rootfs、Docker image rootfs | 防止测试写 `~/.zshrc`、读密钥、污染系统目录 |
| Capability drop / no_new_privs | 降低容器内 root 能力 | Docker `--cap-drop ALL`、`no-new-privileges`、user namespace | 即使测试拿到容器 root，也不能轻易扩大权限 |
| Seccomp / syscall filter | 限制系统调用面 | Docker seccomp profile、nsjail Kafel、bubblewrap seccomp、Firecracker thread seccomp | 降低内核攻击面；不是 OOM 主解，但对不可信代码重要 |
| LSM / MAC | 强制访问控制 | AppArmor、SELinux、Landlock | 作为 path jail 的硬边界增强；Ubuntu 上比 macOS 更可落地 |
| VM / microVM | 独立 guest kernel | Docker Desktop/Colima/Lima 的 Linux VM、Kata、Firecracker | 比共享宿主 Linux kernel 更强隔离；macOS 上天然需要 Linux VM 跑容器 |
| Syscall interposition | 不直接把 syscall 交给宿主 kernel | gVisor Sentry/application kernel | 比 Docker 强、比 VM 轻，但兼容性和部署复杂度更高 |
| Network namespace / proxy | 限制出口网络 | Docker `--network none`、bwrap `--unshare-net`、代理 allowlist | 防止测试访问内网或外传数据 |
| Output backpressure | 防止主进程被日志撑爆 | pipe streaming、ring buffer、log file quota、超限 kill | 这是 agent 测试场景必备；容器不 OOM 不代表主进程不会因输出 OOM |

### 各方案如何组合这些原语

| 方案 | 组合方式 | 优势 | 限制 |
| --- | --- | --- | --- |
| Docker / OCI container | namespaces + cgroups + image rootfs + capabilities + seccomp + network driver | 跨 macOS/Ubuntu 最容易统一，生态成熟，资源限制清晰 | 共享 Linux kernel；macOS 需要 daemon/VM 冷启动 |
| Docker Desktop / Colima / Lima on macOS | macOS host -> Linux VM -> Docker/containerd -> container primitives | macOS 上可获得 Linux cgroup 和容器语义；VM 是额外边界 | VM 常驻有资源成本，冷启动慢 |
| systemd-run + bubblewrap | systemd cgroup 管资源，bubblewrap 管 mount/pid/net/user namespace | Ubuntu 上启动很快、资源消耗低、无需镜像拉取 | 环境复现弱；需要宿主工具链和 cgroup/user namespace 可用 |
| nsjail | namespaces + cgroups + rlimit + seccomp-bpf + chroot/pivot_root | 功能接近轻量容器 runtime，适合 CI/判题/单命令隔离 | 需要安装/配置，macOS 不可用 |
| gVisor | OCI runtime + 用户态 application kernel 拦截 Linux 系统接口 | 比普通容器更抗 kernel escape，资源模型仍偏进程化 | syscall/proc/sys 兼容性可能有坑，部署成本高于 Docker |
| Kata Containers | OCI runtime 外观 + 每个 pod/容器跑进轻量 VM | 硬件虚拟化隔离强，兼容容器生态 | 启动/资源成本高于 Docker/bwrap，适合高风险多租户 |
| Firecracker | KVM microVM + minimalist VMM + jailer(cgroup/namespace/seccomp/drop privileges) | 隔离强、攻击面小，适合企业/云端高风险 agent | Linux+KVM 要求高；自建网络/rootfs/快照管理成本高 |
| E2B/远端沙盒 | 云端 VM/容器/桌面封装 + API | 主机零污染、适合飞书长期运行 | 成本、网络、数据合规和供应商依赖 |

### 自研原则

`claude-go` 不建议从第一天开始自写完整容器引擎或 microVM 管理器。更稳的自研边界是: 自研控制平面、策略、输出限流、资源验证、失败分类、跨平台自动选择；底层隔离先复用 Docker/systemd/bubblewrap/nsjail/Firecracker 这些成熟原语。后续如果要做纯 Go Linux runner，再把 namespace/cgroup/seccomp 逐步内置。

自研目标:

- 不让研发团队错误测试拖垮主进程。
- macOS 和 Ubuntu 自动选择最小可用隔离层。
- 用同一套 `SandboxRunner` API 屏蔽 Docker、systemd-bwrap、remote/microVM 差异。
- 所有 runner 都必须先通过 canary 验证 memory/pids/output limit 生效。
- 失败结果结构化，能反馈给 coder 并进入 metrics/dashboard。

## 自研实现方案: RufloSandbox

建议在 `claude-go` 内新增 `pkg/sandbox`，实现一个轻量自研 sandbox control plane。它不替代 Docker/Firecracker 等 runtime，而是把这些 runtime 的隔离能力变成研发团队可依赖、可观测、可自动适配的执行层。

### 模块结构

```text
pkg/sandbox/
  config.go          # SandboxConfig/Profile/ResourceLimits/OutputLimits
  probe.go           # macOS/Ubuntu capability probe + canary
  runner.go          # Runner interface + CommandSpec/CommandResult
  registry.go        # 持久化 sandbox registry + 状态机 + 文件锁
  lease.go           # heartbeat/lease/deadline 管理
  reaper.go          # 启动恢复、周期清理、孤儿 sandbox 回收
  output_limiter.go  # pipe streaming + ring buffer + log quota + kill callback
  path_policy.go     # readRoots/writeRoots + symlink/canonical path jail
  docker_runner.go   # docker create/start/attach/wait/inspect/rm
  systemd_bwrap.go   # Ubuntu fast runner: systemd-run + bubblewrap
  process_runner.go  # unsafe fallback, only manual/debug
  cleanup.go         # stale container/cgroup/log cleanup
  classify.go        # oom/timeout/output_limit/pids/network 分类
```

统一接口:

```go
type Runner interface {
    Name() string
    Probe(ctx context.Context) ProbeResult
    Run(ctx context.Context, spec CommandSpec) (*CommandResult, error)
    Kill(ctx context.Context, sandboxID string) error
    Cleanup(ctx context.Context, selector CleanupSelector) error
}

type CommandSpec struct {
    ID           string
    Purpose      string // build | test | verification | shell | hook
    Args         []string
    Cwd          string
    Workdir      string
    Env          map[string]string
    ReadRoots    []string
    WriteRoots   []string
    Network      NetworkPolicy
    Limits       ResourceLimits
    OutputLimits OutputLimits
    Labels       map[string]string
}

type CommandResult struct {
    ExitCode      int
    StdoutPreview string
    StderrPreview string
    LogDir        string
    TimedOut      bool
    OOMKilled     bool
    OutputLimited bool
    PidsLimited   bool
    NetworkDenied bool
    Duration      time.Duration
    PeakMemoryMB  int
    Runner        string
}
```

### DockerRunner 自研控制流程

不要用 `docker run` 一条命令黑盒执行，建议拆成 `create -> attach -> start -> wait -> inspect -> rm`，这样才能流式限流、超时 kill、读取 OOM 状态。

流程:

1. `docker create`:
   - `--name claude-go-${runID}-${taskID}`
   - `--label claude-go.sandbox=true`
   - `--memory`、`--memory-swap`、`--cpus`、`--pids-limit`
   - `--network none` 或受控网络
   - `--cap-drop ALL`
   - `--security-opt no-new-privileges`
   - `--read-only`
   - `--tmpfs /tmp`、`--tmpfs /home/agent`
   - `-v <worktree>:/workspace:rw`
2. `docker attach`:
   - stdout/stderr 接 `OutputLimiter`。
   - 超过输出限额时调用 `docker kill`。
3. `docker start` 后等待:
   - context timeout 时 `docker kill`，再 `docker wait`。
4. `docker inspect`:
   - 读取 `State.ExitCode`、`State.OOMKilled`、`State.Error`。
   - exit 137 或 OOMKilled -> `sandbox_oom`。
5. `docker rm -f`:
   - 默认清理容器，保留 `.claude-go/sandboxes/<id>/` 日志和摘要。

核心点:

- `OutputLimiter` 在宿主进程内工作，不依赖 Docker 日志驱动；这样无限输出不会撑爆 `claude-go`。
- DockerRunner 必须有 canary: 启动一个 64MB 容器跑 256MB 分配程序，确认 OOMKilled；启动 `:(){ :|:& };:` 或简单 fork 测试确认 pids limit。
- macOS 上如果 daemon 未启动，不自动裸跑；只有 `autoStart=true` 才启动 Colima/Docker Desktop。

### SystemdBwrapRunner 自研控制流程

Ubuntu 上为了快启动、低资源，可以自研一个 `systemd-run + bubblewrap` runner。它不拉镜像，直接用宿主机已安装的 Go/Rust/Python 工具链；适合你当前“研发团队本地测试不要拖垮主进程”的目标。

外层资源控制:

```bash
systemd-run --user --scope --quiet \
  -p MemoryMax=2G \
  -p MemorySwapMax=0 \
  -p CPUQuota=200% \
  -p TasksMax=256 \
  -- bwrap ...
```

内层文件和 namespace:

```bash
bwrap \
  --unshare-user \
  --unshare-pid \
  --unshare-ipc \
  --unshare-uts \
  --unshare-net \
  --new-session \
  --die-with-parent \
  --ro-bind /usr /usr \
  --ro-bind /bin /bin \
  --ro-bind /lib /lib \
  --ro-bind /lib64 /lib64 \
  --proc /proc \
  --dev /dev \
  --tmpfs /tmp \
  --tmpfs /home/agent \
  --bind "$WORKTREE" /workspace \
  --chdir /workspace \
  --setenv HOME /home/agent \
  --setenv GOCACHE /tmp/go-cache \
  -- "$@"
```

实现要点:

- `systemd-run` 必须包在最外层，让 bwrap 和其子进程都进入同一个 cgroup。
- `bubblewrap` 管“看见什么”，`systemd` 管“最多用多少”。
- 如果 `systemd-run --user` 没有 cgroup delegation 或 memory controller，probe 必须判定失败。
- 如果 `bwrap --unshare-user` 不可用，不能用于自动 team verification。
- 输出依然走 `OutputLimiter`，不能回到 `CombinedOutput()`。

这个 runner 的优势是启动快、资源低、管理成本低；缺点是环境复现不如 Docker image，适合作为 Ubuntu trusted workspace 的快速默认，或者作为 Docker 不可用时的受控 fallback。

### NativeLinuxRunner 长期方案

如果后续希望减少外部依赖，可以把 `SystemdBwrapRunner` 中的部分能力下沉为纯 Go `NativeLinuxRunner`。这不是 P0，但可以作为自研路线:

1. 创建 cgroup v2 目录:
   - 写 `memory.max`、`memory.swap.max`、`pids.max`、`cpu.max`。
   - 把子进程 PID 写入 `cgroup.procs`。
   - cleanup 时写 `cgroup.kill` 或遍历 kill。
2. 通过 `exec.Cmd.SysProcAttr` / re-exec child 设置 namespace:
   - `CLONE_NEWNS`、`CLONE_NEWPID`、`CLONE_NEWNET`、`CLONE_NEWIPC`、`CLONE_NEWUTS`、必要时 `CLONE_NEWUSER`。
3. child 进程 setup:
   - `mount --make-rprivate /`
   - 构造 tmpfs root 或 chroot/pivot_root。
   - bind mount `/workspace`，ro-bind 系统工具链路径。
   - mount `/proc`、tmpfs `/tmp`、tmpfs `$HOME`。
   - drop capabilities，`prctl(PR_SET_NO_NEW_PRIVS)`。
4. seccomp:
   - 初期复用 Docker/default 或 bwrap/nsjail。
   - 真正 native 时引入 libseccomp 或生成 BPF profile；先做 deny 高危 syscall，再逐步收紧。

风险:

- 纯 Go namespace/mount/seccomp runner 对权限、发行版差异和安全细节要求高。
- 一旦实现不完整，安全性可能不如成熟 Docker/bwrap。
- 因此它适合 P3，不适合先于 DockerRunner/OutputLimiter。

### 网络自研模型

研发团队测试默认 `network=none`。如果必须下载依赖，走单独 `dependency-sync` 阶段:

```text
dependency-sync:
  runner: docker/systemd-bwrap
  network: allowlist
  domains: proxy.golang.org, sum.golang.org, github.com
  command: go mod download

build/test:
  network: none
  mounts: workspace + warmed dependency cache
```

自研网络网关可分阶段做:

- P0: Docker `--network none` / bwrap `--unshare-net`。
- P1: dependency-sync 用显式 `network=default` + 命令白名单，不允许任意 shell。
- P2: 本地 HTTP CONNECT/HTTPS proxy + DNS allowlist + egress log。
- P3: per-sandbox network namespace + nftables/pf/iptables 规则。

### 失败分类自研模型

所有 runner 都要输出统一 failure kind:

| failure kind | 判定来源 | 处理 |
| --- | --- | --- |
| `sandbox_oom` | Docker `OOMKilled`、exit 137、cgroup `memory.events oom/oom_kill` | Leaf failed，阻塞依赖，给 coder OOM 修复提示 |
| `sandbox_output_limit` | OutputLimiter 超限 | Leaf failed，要求减少日志/修复无限循环 |
| `sandbox_timeout` | context deadline + runner kill 成功 | 可触发 split-on-timeout 或 failed |
| `sandbox_pids_limit` | cgroup `pids.events max`、fork EAGAIN、TasksMax hit | failed，提示 goroutine/进程爆炸 |
| `sandbox_network_denied` | network none 下连接失败或 proxy deny | failed 或转 dependency-sync |
| `sandbox_setup_failed` | probe/canary/daemon/image/mount 失败 | 不执行任务，提示运维动作 |

### 与现有研发团队代码的接入点

优先改这些地方:

- `pkg/agent/workflow.go:4451` 的 `runLimitedCommand` 改为 `sandbox.RunCommand`，禁止回退到无限制 direct exec。
- `pkg/agent/workflow.go:4471`、`pkg/agent/workflow.go:4488`、`pkg/agent/workflow.go:4500` 去掉 `CombinedOutput()`，统一用 `OutputLimiter`。
- `pkg/tool/builtin/bash.go:188` 把 `bytes.Buffer` 改成流式 limiter；Shell 是否进 sandbox 由 profile 决定。
- `pkg/hooks/hooks.go:351` 同样改成 limiter，避免 hook 输出 OOM。
- `pkg/agent/orchestrator.go:1955/1960/3264/3267` 的 build/test 调用传入 `Purpose=build/test/verification`，便于 profile 选择资源。

实施顺序:

1. `OutputLimiter` + `CommandResult` + failure classification。
2. `DockerRunner`，先覆盖 macOS/Ubuntu team verification。
3. `doctor sandbox` 和 startup probe/canary。
4. `SystemdBwrapRunner`，Ubuntu 快速低资源路径。
5. `PathPolicy` 接入文件工具和 Shell working directory。
6. 远端/microVM runner，用于高风险飞书长期任务。

### 主进程重启/异常退出时的生命周期管理

沙盒不是只在 `Run()` 调用里存在。`claude-go` 主进程可能被 `kill -9`、panic、机器睡眠、飞书 Bot 重启、Docker daemon 重启或系统 OOM 杀掉。如果没有生命周期管理，最坏情况是错误测试继续在后台跑、Docker 容器残留、cgroup 不释放、日志持续增长、worktree 半写入，下一次启动还误判任务状态。

必须把“沙盒生命周期”设计成 crash-safe 状态机。

#### 失败模式

| 失败模式 | 可能后果 | 必须处理 |
| --- | --- | --- |
| 主进程 panic/kill | 容器或 systemd scope 继续运行 | lease 过期后自动 kill；启动时 reconcile |
| 主进程重启 | registry 显示 running，但新进程没有 stdout/stderr 管道 | 默认 kill transient build/test，标记 `interrupted_by_restart` |
| Docker daemon 重启 | container 状态丢失或变为 exited | inspect/list by label 后修正 registry |
| 机器睡眠/恢复 | heartbeat 长时间停止，deadline 超过 | 按 wall-clock deadline 清理 |
| OutputLimiter 崩溃前未写完摘要 | 日志文件可能只有部分内容 | 日志写入按 chunk flush，registry 保留 log path |
| cache volume 长期保留 | 磁盘膨胀 | TTL + quota + label-based prune |
| systemd scope/bwrap 残留 | Ubuntu 上子进程继续占资源 | systemd RuntimeMaxSec + startup cleanup |

#### Sandbox Registry

新增持久化 registry，建议优先用 SQLite；如果暂时不想引入依赖，可用 JSONL + `flock`。位置:

```text
<stateDir>/.claude-go/sandboxes/registry.sqlite
<stateDir>/.claude-go/sandboxes/<sandboxID>/
  stdout.log
  stderr.log
  result.json
  spec.json
```

记录结构:

```go
type SandboxRecord struct {
    ID              string
    Runner          string // docker | systemd-bwrap | process | remote
    State           string // reserved | creating | running | stopping | exited | cleaned | orphaned | cleanup_failed
    TeamRunID        string
    TaskID           string
    Purpose          string // build | test | verification | shell | hook
    Worktree         string
    LogDir           string
    ContainerID      string
    CgroupPath       string
    SystemdUnit      string
    RemoteID         string
    OwnerPID         int
    OwnerStartTime   time.Time
    CreatedAt        time.Time
    StartedAt        time.Time
    LastHeartbeatAt  time.Time
    LeaseExpiresAt   time.Time
    DeadlineAt       time.Time
    CleanupAfter     time.Time
    ExitCode         int
    FailureKind      string
    ResourceLimits   ResourceLimits
    OutputLimits     OutputLimits
    Labels           map[string]string
}
```

状态机:

```text
reserved -> creating -> running -> stopping -> exited -> cleaned
                         |             |        |
                         |             |        -> cleanup_failed
                         |             -> orphaned
                         -> setup_failed
```

关键规则:

- 先写 `reserved`，再创建底层 sandbox。不能先创建容器后写 registry，否则主进程在中间崩溃会产生“无记录孤儿”。
- 每次状态变更都要持久化，并写 `updated_at`。
- registry 写入必须带文件锁或 SQLite transaction，避免多个 team run 并发清理互相踩。
- `Run()` 返回前不依赖 `defer` 清理；必须显式进入 `stopping/exited/cleaned`，异常时由 reaper 兜底。

#### Label 与命名规范

所有底层资源都必须可被“无 registry 状态”反向发现。

Docker:

```bash
--name claude-go-${sandboxID}
--label claude-go.sandbox=true
--label claude-go.sandbox.id=${sandboxID}
--label claude-go.team_run_id=${TEAM_RUN_ID}
--label claude-go.task_id=${TASK_ID}
--label claude-go.owner_pid=${PID}
--label claude-go.deadline_at=${RFC3339}
```

命名资源:

- 容器: `claude-go-${sandboxID}`
- cache volume: `claude-go-gomod-${projectHash}`，带 `claude-go.cache=true` 和 TTL label
- 临时 network: `claude-go-net-${sandboxID}`，默认不创建，只有 allowlist/proxy 需要
- systemd scope: `claude-go-${sandboxID}.scope`
- cgroup path: `/sys/fs/cgroup/.../claude-go-${sandboxID}`

这样即使 registry 损坏，也能用 Docker/systemd/cgroup 列表找回残留资源。

#### Lease、Heartbeat 与 Deadline

每个 running sandbox 有两个时间概念:

- `lease`: 表示主进程还活着并负责这个 sandbox。
- `deadline`: 表示命令绝对不能超过的 wall-clock 最大运行时间。

建议默认:

```json
{
  "sandbox": {
    "lifecycle": {
      "heartbeatSec": 5,
      "leaseTTLSeconds": 20,
      "startupReconcile": true,
      "orphanPolicy": "kill-transient",
      "maxCleanupConcurrency": 4,
      "logRetentionHours": 72,
      "cacheRetentionHours": 168,
      "maxSandboxDiskMB": 10240
    }
  }
}
```

执行期间:

- 主进程每 5 秒更新 `LastHeartbeatAt/LeaseExpiresAt`。
- command timeout 要写入 `DeadlineAt`，并由 runner 和 reaper 双重执行。
- Docker 容器内部也可包一层 `timeout -k 5s <seconds>`，防止宿主控制面断开后长期运行。
- Ubuntu `systemd-run` 应设置 `RuntimeMaxSec` 或等价 deadline，避免只靠主进程清理。

主进程重启后:

- 旧进程不会再续 lease。
- 新进程启动时发现 `LeaseExpiresAt < now`，将 running sandbox 标为 stale。
- 对 build/test/verification 这类 transient 任务，默认 kill 并标记 `interrupted_by_restart`，让 orchestrator 重新执行 leaf 或走失败恢复。
- 对 dependency cache、日志、worktree，不立刻删除，按 TTL/quota 清理。

#### Reaper 设计

需要两个 reaper:

1. `StartupReaper`: 每次 `claude-go` 启动或飞书 Bot 启动时运行一次。
2. `PeriodicReaper`: 主进程运行期间每 30-60 秒运行一次。

可选第三个:

3. `DetachedReaper`: 每次运行 sandbox 时额外启动一个很小的外部 watchdog，例如 `claude-go sandbox-reaper --id <id> --deadline <ts>`。它不处理复杂逻辑，只在 deadline 后 kill 指定资源。这样主进程被 kill 时仍有最后一道兜底。macOS/Docker 场景尤其有价值，因为 Docker daemon 不会因为 CLI 客户端退出而自动停止容器。

启动恢复流程:

```text
1. 加 registry lock。
2. Probe runner 能力: docker/systemd-bwrap/remote。
3. 扫描 registry 中 running/creating/stopping 的记录。
4. 扫描底层资源:
   - docker ps -a --filter label=claude-go.sandbox=true
   - docker volume ls --filter label=claude-go.cache=true
   - systemctl --user list-units 'claude-go-*.scope'
   - cgroup path / remote provider list
5. 合并 registry 与实际资源，生成 reconcile plan。
6. 对 stale transient sandbox:
   - docker kill/rm 或 systemctl stop。
   - registry 标记 orphaned -> cleaned。
   - result.json 写 interrupted_by_restart。
7. 对 registry 没记录但 label 属于 claude-go 的资源:
   - 标记 discovered_orphan。
   - 超过 grace period 直接清理。
8. 对 cache/log/worktree:
   - 按 TTL/quota 清理，不影响当前任务恢复。
```

#### 不同资源的保留策略

| 资源 | 重启后默认策略 | 原因 |
| --- | --- | --- |
| running build/test container | kill | 构建/测试是可重跑的，且没有 stdout 管道时不可控 |
| exited container | rm | 结果已写 result/log；容器本身不必保留 |
| stdout/stderr log | 保留 72h | 便于复盘 OOM/timeout |
| result.json/spec.json | 保留 7-30 天 | dashboard 和质量复盘需要 |
| dependency cache volume | 保留 7 天或按 quota | 提升速度，但不能无限增长 |
| worktree/artifacts | 保留到 team run 完结或用户清理 | 可能包含已生成代码 |
| remote microVM | stale 后 terminate | 云端成本高，不能残留 |
| network/proxy rule | 立即删除 | 网络规则残留风险高 |

#### 与 Orchestrator 的恢复语义

沙盒被 reaper 清理后，研发团队不应把它误判为成功。建议新增失败状态:

- `TaskFailed` + `FailureKind=interrupted_by_restart`
- `TaskFailed` + `FailureKind=sandbox_orphan_reaped`
- `TaskFailed` + `FailureKind=sandbox_setup_failed`

恢复规则:

- 如果 leaf 尚未物化关键文件，重启后可重新排队执行。
- 如果 leaf 已物化但 verification 被中断，重启后先跑 verification sandbox，再决定是否完成。
- 如果 final verification 被中断，直接重新跑 final verification。
- 如果 crash 发生在 deterministic patch 中，重启后不要假设 patch 完成，必须重新 build/test。

#### 运维命令

建议新增 CLI:

```bash
claude-go sandbox status
claude-go sandbox doctor
claude-go sandbox ps
claude-go sandbox logs <sandboxID>
claude-go sandbox kill <sandboxID>
claude-go sandbox cleanup --stale
claude-go sandbox cleanup --all --older-than 72h
```

`doctor sandbox` 必须输出:

```text
runner selected: docker
docker daemon: running
resource canary: memory=pass pids=pass output=pass
active sandboxes: 0
stale sandboxes: 1
cache usage: 1.2GB / 10GB
last cleanup: 2026-05-06T...
team verification: enabled
```

#### 指标与告警

新增 metrics:

- `sandbox_active_count`
- `sandbox_stale_count`
- `sandbox_orphan_reaped_count`
- `sandbox_cleanup_failed_count`
- `sandbox_registry_reconcile_ms`
- `sandbox_cache_disk_bytes`
- `sandbox_log_disk_bytes`
- `sandbox_interrupted_by_restart_count`
- `sandbox_detached_reaper_kill_count`

Dashboard 要区分:

- `sandbox_guard_failed_task`: 沙盒成功保护了主进程，但任务失败。
- `sandbox_system_failure`: 沙盒自身启动/清理失败，需要运维处理。
- `interrupted_by_restart`: 主进程生命周期事件，不是 coder 质量问题。

## 推荐架构

### 分层原则

沙盒要按层做，不能只做一个配置开关:

| 层 | 名称 | 目标 |
| --- | --- | --- |
| L0 | Policy Gate | 权限模式、tool profile、allow/deny、MCP tool allowlist，决定“是否允许请求执行” |
| L1 | Workspace Jail | 所有文件工具和 Shell working directory 强制落在允许根目录内，拒绝 `~`、绝对路径逃逸、symlink 逃逸 |
| L2 | Process Sandbox | Shell/build/test/package install 在 Docker/rootless Docker/nsjail/gVisor/微 VM 内执行 |
| L3 | Network Gateway | 默认 deny，按 profile/domain/tool 授权；WebSearch 走代理工具，不允许 Shell 直接任意 curl |
| L4 | Secret Broker | API key、GitHub token、飞书 token 只由宿主代理按域注入，不写入 sandbox env/files |
| L5 | Audit & Replay | 每次命令记录 sandbox_id、policy_id、mounts、network decision、exit code、diff、资源用量 |

### 运行模型

建议新增统一接口:

```go
type SandboxRunner interface {
    Run(ctx context.Context, spec CommandSpec) (*CommandResult, error)
}

type CommandSpec struct {
    Command          string
    Cwd              string
    Profile          string
    ReadRoots        []string
    WriteRoots       []string
    NetworkPolicy    NetworkPolicy
    EnvPolicy        EnvPolicy
    Timeout          time.Duration
    CPUQuota         string
    MemoryMB         int
    PidsLimit        int
    SandboxID        string
}
```

推荐 provider:

- `process`: 当前行为，仅用于兼容和开发调试，启动时必须打印高危提示。
- `docker`: 本机首选，使用 rootless Docker/Podman 或 Docker Desktop/Colima，挂载 workspace 到 `/workspace`。
- `docker-gvisor`: Linux 多租户增强版，Docker runtime 使用 `runsc`。
- `nsjail`: Linux 轻量版，适合单命令快速执行和 CI worker。
- `microvm`: Firecracker/Kata/Docker Sandboxes，适合飞书 Bot、企业后台、长时间研发团队。
- `remote`: E2B/自建 K8s/自建 runner，适合可伸缩队列和异步任务。

### Profile 策略

| Profile | Shell | 文件写 | 网络 | MCP | 用途 |
| --- | --- | --- | --- | --- | --- |
| `chat` | deny | deny | websearch-only | search/discover-only | 普通飞书问答 |
| `research` | deny 或只读命令 | deny | allowlisted web | search/read-only MCP | 调研、总结 |
| `coding` | sandboxed | workspace rw | default deny，包管理按需 allow | 最小工具集 | 本地编码、测试 |
| `team` | sandboxed + per-agent worktree | agent worktree rw | default deny，按任务授权 | role-scoped MCP | `/go development` |
| `admin` | process 或 sandboxed-open | 显式 allow | 显式 allow | 显式 allow | 人工维护，不给飞书默认使用 |

关键规则:

- `bypass` 只能表示“跳过 prompt”，不能表示“无沙盒执行”。当 `permissionMode=bypass` 且 `sandbox.enabled=false` 时，飞书和 team 应拒绝启动或强提示。
- `auto/autonomous` 只有在 `sandbox.active=true` 时才允许自动运行 Shell/Fetch。
- 文件工具也要走 L1 path jail，不能只保护 Shell；否则模型可以通过 `Read("/etc/passwd")` 或 `Write("~/.zshrc")` 绕过 Shell sandbox。
- MCP server 默认只暴露 discover/search，具体工具 schema 和调用必须经过 profile + allowlist。

### 配置草案

```json
{
  "permissionMode": "auto",
  "sandbox": {
    "enabled": true,
    "defaultProfile": "coding",
    "warnOnProcessRunner": true,
    "denyBypassWithoutSandbox": true,
    "profiles": {
      "chat": {
        "runner": "none",
        "shell": "deny",
        "readRoots": ["${cwd}"],
        "writeRoots": [],
        "network": {"mode": "tool-only", "tools": ["WebSearch", "WebFetch"]},
        "mcp": {"mode": "discover-only"}
      },
      "coding": {
        "runner": "docker",
        "image": "ghcr.io/ruflo/claude-go-sandbox:go1.24",
        "workdir": "/workspace",
        "readRoots": ["${cwd}"],
        "writeRoots": ["${cwd}"],
        "mounts": [
          {"src": "${cwd}", "dst": "/workspace", "mode": "rw"},
          {"type": "tmpfs", "dst": "/tmp"},
          {"type": "tmpfs", "dst": "/home/agent"}
        ],
        "network": {"mode": "deny"},
        "limits": {"timeoutSec": 360, "memoryMB": 4096, "pids": 512, "cpus": "2"}
      },
      "team": {
        "runner": "microvm",
        "perAgentWorktree": true,
        "network": {"mode": "allowlist", "domains": ["proxy.golang.org", "sum.golang.org", "github.com"]},
        "limits": {"timeoutSec": 360, "memoryMB": 8192, "pids": 1024, "cpus": "4"}
      }
    }
  }
}
```

### Docker 执行基线

Shell 命令初版可以翻译成类似策略:

```bash
docker run --rm \
  --network none \
  --cpus 2 \
  --memory 4g \
  --pids-limit 512 \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,size=512m \
  --tmpfs /home/agent:rw,nosuid,size=512m \
  -v "$CWD:/workspace:rw" \
  -w /workspace \
  claude-go-sandbox:go1.24 \
  bash -lc "$COMMAND"
```

说明:

- `--network none` 是 coding 默认值；需要下载依赖时，切到 `network.allowlist` 或通过代理。
- `--read-only` 降低容器根文件系统污染。
- `--cap-drop ALL` 和 `no-new-privileges` 降低提权面。
- macOS 上 Docker 本身在 Linux VM 内运行，安全边界比直接宿主机进程好，但仍应结合 workspace mount 和密钥代理。

## 分阶段落地计划

### P0: 明确当前无沙盒并阻断高危默认

- 增加 `sandbox` 配置结构，但先支持 `enabled=false/process` 和 dry-run 日志。
- 启动时打印安全摘要: `permissionMode`、`sandbox.enabled`、`runner`、`cwd`、`network`、`writeRoots`。
- 飞书 Bot 和 `/go development` 如果 `permissionMode=bypass` 且 `sandbox.enabled=false`，默认降级为 `auto` 或拒绝 team autonomous 执行。
- 所有文件工具接入 `PathPolicy`: `EvalSymlinks + filepath.Rel` 校验，拒绝逃逸 `readRoots/writeRoots`。
- 浏览器/媒体工具在 `--no-sandbox` 时打高危指标，并默认只在 `research/browser` profile 可用。

### P1: 引入 SandboxRunner 并接管 Shell

- 新增 `pkg/sandbox`:
  - `Runner` 接口。
  - `ProcessRunner` 保持兼容。
  - `DockerRunner` 负责构造容器命令、mount、network、limits、env scrub。
  - `PathPolicy` 统一给 Read/Write/StrReplace/Glob/Grep/Shell working_directory 使用。
- `BashTool.Call` 不再直接 `exec.CommandContext("bash", "-c")`，改为 `tctx.SandboxRunner.Run(...)`。
- `ToolContext` 增加 `Sandbox *sandbox.Context` 或 `SandboxRunner sandbox.Runner`。
- metrics 增加 `sandbox_runner`、`sandbox_profile`、`sandbox_network_mode`、`sandbox_denied_count`、`sandbox_command_duration_sec`、`sandbox_escape_denied_count`。

### P2: 网络和密钥代理

- `WebSearch/WebFetch` 走独立网络代理，保留审计记录。
- Shell sandbox 默认无网络；需要依赖下载时按域临时授权。
- 禁止将宿主 `ANTHROPIC_API_KEY`、`DASHSCOPE_API_KEY`、飞书 token、GitHub token 原样注入容器。
- 实现 Secret Broker: 只有对模型 provider/GitHub API 的指定请求由宿主代理代发或注入短期 token。

### P3: Team/Feishu 微 VM 化

- 研发团队每个 team run 创建一个 sandbox namespace。
- 每个 agent 使用独立 worktree 和独立 sandbox，只有 artifacts/summary 通过 orchestrator 共享。
- 高风险任务如数据库、事务、并发、文件系统、索引默认 `microvm` 或 `docker-gvisor`。
- 长期飞书 Bot 不直接持有高权限 shell；任务提交到 sandbox worker 队列执行，Bot 主进程只负责消息、状态和审批。

### P4: 企业治理和可审计

- 支持 org policy: 禁止无沙盒 bypass、禁止开放网络、强制 prompt debug 脱敏。
- Dashboard 展示 sandbox 状态、网络拒绝、文件 diff、命令审计、资源消耗。
- 提供 red-team 测试集: 读 `~/.ssh`、读 `.env`、curl 外传、访问内网、fork bomb、后台持久化、修改 shell rc、git hook 注入。

## 验证方案

单元测试:

- `PathPolicy` 阻止 `../`、绝对路径、`~/`、symlink 指向 workspace 外。
- `permissionMode=bypass + sandbox.disabled` 在 feishu/team 入口触发拒绝或降级。
- `ToolProfileChat` 不注册 Shell/Write/StrReplace。
- `DenyRules` 高于 `AllowRules`，且 sandbox policy deny 高于 permission allow。

集成测试:

- `coding` profile 下 `go test ./...` 可运行，但 `curl https://example.com` 默认失败。
- `coding` profile 下 `cat ~/.ssh/id_rsa` 失败。
- `Write("/tmp/outside")`、`Write("~/.zshrc")` 失败。
- `npm install` 在网络 deny 下失败，在 allowlist `registry.npmjs.org` 后成功。
- `team` 两个 agent 写不同 worktree 不互相污染。

红队场景:

- 恶意 `Makefile` 在 `make test` 中读取 `$HOME/.claude-go/config/config.json` 并 curl 外传，应被文件和网络两层拦截。
- 恶意仓库包含 `.claude/settings.json` 或提示词诱导切换 bypass，应被 org policy 阻止。
- 不可信网页触发浏览器下载/执行，应只在 browser sandbox/profile 内影响临时目录。

## 推荐决策

短期建议先做 P0+P1，先把 `Shell` 和文件工具放到统一的 `PathPolicy/SandboxRunner` 后面。这样风险立刻从“模型一旦被允许就能碰宿主机”降到“模型只能碰 workspace 和被授权网络”。

中期建议把飞书 Bot 的普通问答、复杂计划、研发团队拆成不同 profile。普通问答默认 `chat`，不暴露 Shell；研发团队默认 `team + sandbox.required`，每个 agent 独立 worktree。

长期建议给企业/飞书长时间运行使用 remote/microVM provider。持续在线 agent 的风险来自时间、权限和上下文累积，不适合长期直接跑在开发者主机。

## 参考资料

- [OpenAI Codex CLI](https://developers.openai.com/codex/cli)
- [Claude Code permissions](https://code.claude.com/docs/en/permissions)
- [GitHub Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent)
- [GitHub Copilot cloud agent firewall](https://docs.github.com/en/copilot/how-tos/copilot-on-github/customize-copilot/customize-cloud-agent/customize-the-agent-firewall)
- [GitHub Copilot cloud agent environment](https://docs.github.com/en/enterprise-cloud@latest/copilot/how-tos/use-copilot-agents/cloud-agent/customize-the-agent-environment)
- [Cursor Agent Security](https://docs.cursor.com/account/agent-security)
- [Cursor CLI permissions](https://docs.cursor.com/cli/reference/permissions)
- [Devin for Terminal permissions](https://cli.devin.ai/docs/reference/permissions)
- [Google Jules](https://blog.google/innovation-and-ai/models-and-research/google-labs/jules/)
- [OpenHands sandboxes](https://docs.openhands.dev/openhands/usage/sandboxes/overview)
- [OpenHands runtime architecture](https://docs.openhands.dev/openhands/usage/architecture/runtime)
- [Docker Engine security](https://docs.docker.com/engine/security/)
- [Docker Sandboxes for AI agents](https://docs.docker.com/ai/sandboxes/get-started/)
- [Docker Desktop Resource Saver](https://docs.docker.com/desktop/use-desktop/resource-saver/)
- [Docker Engine resource constraints](https://docs.docker.com/engine/containers/resource_constraints/)
- [Docker Desktop macOS permission requirements](https://docs.docker.com/desktop/mac/permission-requirements/)
- [Colima configuration](https://colima.run/docs/configuration/)
- [Colima commands and resource flags](https://colima.run/docs/commands/)
- [Lima Linux Machines](https://lima-vm.io/docs/)
- [Docker rootless resource-limit tips](https://docs.docker.com/engine/security/rootless/tips/)
- [Kubernetes cgroup v2 overview](https://kubernetes.io/docs/concepts/architecture/cgroups)
- [Linux kernel cgroup v2](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html)
- [systemd resource control](https://www.freedesktop.org/software/systemd/man/249/systemd.resource-control.html)
- [bubblewrap](https://github.com/containers/bubblewrap)
- [E2B docs](https://www.e2b.dev/docs)
- [gVisor docs](https://gvisor.dev/docs/)
- [Firecracker](https://github.com/firecracker-microvm/firecracker)
- [Firecracker jailer](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md)
- [Kata Containers architecture](https://github.com/kata-containers/kata-containers/blob/main/docs/design/architecture/README.md)
- [Kata Containers virtualization](https://github.com/kata-containers/kata-containers/blob/main/docs/design/virtualization.md)
- [Docker rootless mode](https://docs.docker.com/engine/security/rootless/)
- [nsjail](https://github.com/google/nsjail)
- [Kubernetes seccomp](https://kubernetes.io/docs/reference/node/seccomp/)
- [StackBlitz WebContainers](https://blog.stackblitz.com/posts/introducing-webcontainers/)
