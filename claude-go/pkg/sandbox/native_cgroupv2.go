package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type NativeCgroupV2Runner struct {
	BasePath string
}

// Name 返回 runner 标识名 "native-cgroupv2"。
func (r *NativeCgroupV2Runner) Name() string { return "native-cgroupv2" }

// Probe 逐级验证本机 cgroup v2 是否真的能用来做资源隔离, 任一步失败即判不可用。
//
// 算法解读：
//
//	原理：探针不是在问"这台机器有没有 cgroup v2", 而是在问"**我**能不能在这一层
//	      真正施加限制"。因此它按由弱到强的顺序做四件事: ①平台/统一层级存在性;
//	      ②基目录可写 (用一个真实的控制文件 cgroup.freeze 试写, 而不是建普通文件 ——
//	      cgroupfs 上后者不代表前者); ③subtree_control 委派 + 子 cgroup 里
//	      memory.max/pids.max 可写; ④**把真实进程迁进子 cgroup** (写 cgroup.procs)。
//	关键不变量：只要 memory 与 pids 两个控制器都写不进去, 就必须判失败 —— 因为此时
//	      本 runner 相对 process-unsafe 没有任何额外价值, 而"看起来有隔离"比
//	      "明确没有隔离"更危险 (manager 会据此选它, 却拿不到任何约束)。
//	      第 ④ 步单独存在也是同一个理由: 前三步全过而 cgroup.procs 写不进去时
//	      (systemd 委派缺口, 见下方注释), 每条命令都会在运行期以 "sandbox setup
//	      failed" 报错 —— 探针必须在这里就把它拦成"不可用", 好让 manager 回退 docker。
//	代价与边界：探针有副作用 (建/删子 cgroup、起一个 sleep 1 的进程), 且依赖
//	      /sys/fs/cgroup 的写权限; 非 Linux 直接短路。Latency 记录全程耗时。
//	设计权衡：用"真的迁一个进程进去"而不是读 /proc/self/cgroup 推断 —— 推断在
//	      rootless/systemd 委派场景下会给出乐观的错误答案, 而错误答案是这里最贵的。
func (r *NativeCgroupV2Runner) Probe(ctx context.Context) ProbeResult {
	start := time.Now()
	if runtime.GOOS != "linux" {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "native cgroup v2 is linux-only", Latency: time.Since(start)}
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cgroup v2 unified hierarchy not detected", Latency: time.Since(start)}
	}
	base := r.basePath()
	if err := os.MkdirAll(base, 0o755); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cgroup base not writable: " + err.Error(), Latency: time.Since(start)}
	}
	// cgroup v2 is a special filesystem — cannot write arbitrary files.
	// Test by writing to actual control files and creating a sub-cgroup.
	if err := os.WriteFile(filepath.Join(base, "cgroup.freeze"), []byte("1"), 0o644); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cgroup base not delegated/writable: " + err.Error(), Latency: time.Since(start)}
	}
	_ = os.WriteFile(filepath.Join(base, "cgroup.freeze"), []byte("0"), 0o644)
	// Enable subtree_control so child cgroups can use memory/pids/cpu controllers.
	// Without this, writes to child's memory.max etc fail with EACCES.
	canMem, canPids, canCPU := ensureSubtreeControl(base)
	subTest := filepath.Join(base, ".claude-go-probe-sub")
	if err := os.MkdirAll(subTest, 0o755); err != nil {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cannot create sub-cgroup: " + err.Error(), Latency: time.Since(start)}
	}
	defer os.Remove(subTest)
	// Verify we can actually configure resource limits on the sub-cgroup.
	// This catches the case where subtree_control delegation is incomplete.
	if canMem {
		if err := os.WriteFile(filepath.Join(subTest, "memory.max"), []byte("max"), 0o644); err != nil {
			canMem = false
		}
	}
	if canPids {
		if err := os.WriteFile(filepath.Join(subTest, "pids.max"), []byte("max"), 0o644); err != nil {
			canPids = false
		}
	}
	// If neither memory nor pids can be enforced, this runtime offers no isolation
	// benefit over process-unsafe — fail the probe so a stronger runtime is picked.
	if !canMem && !canPids {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "no resource controllers (memory/pids) writable in sub-cgroup at " + base, Latency: time.Since(start)}
	}
	// Verify that we can migrate processes into the sub-cgroup. systemd cgroup
	// delegation requires the writer to have write permission on cgroup.procs of
	// the common ancestor of the source (our own cgroup) and destination cgroups.
	// In rootless setups where claude-go was launched from a user session scope
	// (e.g. session-N.scope), the common ancestor with app.slice/claude-go-sandbox
	// is user-1000.slice, which is owned by root. The earlier controller writes
	// (memory.max, pids.max) succeed because they don't migrate processes, but
	// the AfterStart hook's cgroup.procs write fails with EACCES at runtime —
	// every command then errors as "sandbox setup failed". Detect this here and
	// fail the probe so the manager falls back to docker.
	procsWriteOK := true
	procsDetail := ""
	testCmd := exec.CommandContext(ctx, "sleep", "1")
	if err := testCmd.Start(); err == nil {
		childPid := testCmd.Process.Pid
		if werr := os.WriteFile(filepath.Join(subTest, "cgroup.procs"), []byte(strconv.Itoa(childPid)), 0o644); werr != nil {
			procsWriteOK = false
			procsDetail = werr.Error()
		}
		_ = testCmd.Process.Kill()
		_, _ = testCmd.Process.Wait()
	}
	if !procsWriteOK {
		return ProbeResult{Runtime: r.Name(), OK: false, Detail: "cgroup.procs migration not permitted (systemd delegation gap): " + procsDetail, Latency: time.Since(start)}
	}
	return ProbeResult{Runtime: r.Name(), OK: true, Detail: base, Latency: time.Since(start), CanMemory: canMem, CanPids: canPids, CanCPU: canCPU}
}

// ensureSubtreeControl 在基 cgroup 上开启 memory/pids/cpu 三个控制器的子树委派, 返回哪些可用。
//
// ensureSubtreeControl enables memory/pids/cpu controllers in the given cgroup's
// cgroup.subtree_control file, so that child cgroups inherit those controllers
// and can configure resource limits (memory.max, pids.max, cpu.max).
//
// Returns booleans indicating which controllers are available after the operation.
// Best-effort: failures are silent (caller verifies via the probe step).
//
// 算法解读：
//
//	原理：cgroup v2 的控制器是**逐层委派**的: 父层的 cgroup.subtree_control 里没有
//	      写 +memory, 子 cgroup 就不会出现 memory.max, 往里写必然 EACCES。所以这里
//	      做的是"读现状 → 求差集 → 只补缺的那几个"的收敛动作, 而不是无条件覆写。
//	关键不变量：只允许写**本层 cgroup.controllers 里确实存在**的控制器。往
//	      subtree_control 写一个本层没有的控制器会让整个写操作失败 (内核原子处理
//	      整行), 于是连本来能开的那几个也一起开不了 —— 这正是"只写差集"要防的事。
//	代价与边界：best-effort, 写失败静默返回; 判定权交给调用方 (Probe 会用子 cgroup
//	      里的真实写入来复核)。返回值是**写成功之后**的 enabled 快照, 不是写入结果。
//	设计权衡：幂等且廉价, 因此 Run 里在每次建子 cgroup 前都重跑一遍 —— 用一次多余的
//	      读换掉"上一次运行把委派重置了"这种跨进程残留导致的偶发 EACCES。
func ensureSubtreeControl(base string) (canMemory, canPids, canCPU bool) {
	// Read controllers available in this cgroup (inherited from parent's subtree_control).
	availData, err := os.ReadFile(filepath.Join(base, "cgroup.controllers"))
	if err != nil {
		return false, false, false
	}
	avail := map[string]bool{}
	for _, c := range strings.Fields(string(availData)) {
		avail[c] = true
	}
	// Read what's currently enabled in subtree_control.
	curData, _ := os.ReadFile(filepath.Join(base, "cgroup.subtree_control"))
	enabled := map[string]bool{}
	for _, c := range strings.Fields(string(curData)) {
		enabled[c] = true
	}
	// Enable available controllers that aren't yet enabled.
	wants := []string{"memory", "pids", "cpu"}
	var toAdd []string
	for _, w := range wants {
		if avail[w] && !enabled[w] {
			toAdd = append(toAdd, "+"+w)
		}
	}
	if len(toAdd) > 0 {
		if err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte(strings.Join(toAdd, " ")), 0o644); err == nil {
			for _, w := range wants {
				if avail[w] {
					enabled[w] = true
				}
			}
		}
	}
	return enabled["memory"], enabled["pids"], enabled["cpu"]
}

// Run 为本次命令建一个专属子 cgroup, 施加资源限制后在其中执行, 退出后回收。
//
// 三个 hook 是隔离语义的落点: AfterStart 把子进程迁进子 cgroup (在此之前它跑在
// 父 cgroup 里, 限制一概不生效); OnKill 用 cgroup.kill 一次性杀光整棵进程树
// (逐个 kill 会漏掉孙进程); AfterWait 读 memory.events / pids.events 把 OOM 与
// pids 上限这两类"看起来只是命令失败"的死法还原成具体 FailureKind。
func (r *NativeCgroupV2Runner) Run(ctx context.Context, spec CommandSpec) (*CommandResult, error) {
	spec = spec.normalized()
	if probe := r.Probe(ctx); !probe.OK {
		result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args, FailureKind: FailureRuntime, RuntimeDetail: probe.Detail}
		result.Err = fmt.Errorf("%s: %s", FailureRuntime, probe.Detail)
		return result, result.Err
	}

	// Defensive: re-ensure subtree_control before creating child cgroup, in case
	// another process or restart reset it. Idempotent and cheap.
	ensureSubtreeControl(r.basePath())

	cgPath := filepath.Join(r.basePath(), spec.ID)
	if err := os.MkdirAll(cgPath, 0o755); err != nil {
		result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args, FailureKind: FailureSetup}
		result.Err = err
		return result, err
	}
	// 清理失败被忽略: 子 cgroup 里还留着进程时 rmdir 会 EBUSY, 而这条路径通常正是
	// 命令被 kill 之后 —— 残留的空 cgroup 无害 (下次同名 ID 会复用), 不该因此让整次运行报错。
	defer os.Remove(cgPath)

	if err := configureCgroup(cgPath, spec.Limits); err != nil {
		result := &CommandResult{ID: spec.ID, Runtime: r.Name(), Purpose: spec.Purpose, Cwd: spec.Cwd, Args: spec.Args, FailureKind: FailureSetup}
		result.Err = err
		return result, err
	}

	hooks := &localProcessHooks{
		AfterStart: func(cmd *exec.Cmd) error {
			return os.WriteFile(filepath.Join(cgPath, "cgroup.procs"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
		},
		OnKill: func(cmd *exec.Cmd) {
			_ = os.WriteFile(filepath.Join(cgPath, "cgroup.kill"), []byte("1"), 0o644)
		},
		AfterWait: func(cmd *exec.Cmd, result *CommandResult) {
			if cgroupEventCount(filepath.Join(cgPath, "memory.events"), "oom_kill") > 0 || cgroupEventCount(filepath.Join(cgPath, "memory.events"), "oom") > 0 {
				result.FailureKind = FailureOOM
				result.Err = fmt.Errorf("%s", FailureOOM)
			}
			if cgroupEventCount(filepath.Join(cgPath, "pids.events"), "max") > 0 {
				result.FailureKind = FailurePidsLimit
				result.Err = fmt.Errorf("%s", FailurePidsLimit)
			}
		},
	}
	return runLocalProcess(ctx, spec, r.Name(), hooks, nil)
}

// basePath 解析基 cgroup 目录: 结构体字段 > CLAUDE_GO_CGROUP_BASE 环境变量 > /sys/fs/cgroup/claude-go-<uid>。
//
// 默认值带 uid 后缀是为了让同一台机器上的多个用户各占一层, 避免互相抢 subtree_control。
func (r *NativeCgroupV2Runner) basePath() string {
	if r != nil && r.BasePath != "" {
		return r.BasePath
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_CGROUP_BASE")); v != "" {
		return v
	}
	return filepath.Join("/sys/fs/cgroup", "claude-go-"+strconv.Itoa(os.Getuid()))
}

// configureCgroup 把 ResourceLimits 逐项写进 cgroup 控制文件; 每项都是"0 表示不设限"。
//
// memory.swap.max 与 memory.max 写同一个值 (swap 不限的话, 内存上限会被 swap 绕过去);
// cpu.max 的格式是 "<quota> <period>", 且 quota 有 1000us 的下限 —— 内核不接受比
// 单次调度片还小的配额。这两个都是格式约束, 不是策略选择。
func configureCgroup(path string, limits ResourceLimits) error {
	if limits.MemoryMaxMB > 0 {
		bytes := int64(limits.MemoryMaxMB) * 1024 * 1024
		if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte(strconv.FormatInt(bytes, 10)), 0o644); err != nil {
			return fmt.Errorf("write memory.max: %w", err)
		}
		_ = os.WriteFile(filepath.Join(path, "memory.swap.max"), []byte(strconv.FormatInt(bytes, 10)), 0o644)
	}
	if limits.PidsMax > 0 {
		if err := os.WriteFile(filepath.Join(path, "pids.max"), []byte(strconv.Itoa(limits.PidsMax)), 0o644); err != nil {
			return fmt.Errorf("write pids.max: %w", err)
		}
	}
	if limits.CPUQuotaPercent > 0 {
		periodUS := 100000
		quotaUS := limits.CPUQuotaPercent * periodUS / 100
		if quotaUS < 1000 {
			quotaUS = 1000
		}
		value := fmt.Sprintf("%d %d", quotaUS, periodUS)
		if err := os.WriteFile(filepath.Join(path, "cpu.max"), []byte(value), 0o644); err != nil {
			return fmt.Errorf("write cpu.max: %w", err)
		}
	}
	return nil
}

// cgroupEventCount 读 cgroup.events 类文件里某个计数键的值 (如 memory.events 的 oom_kill)。
//
// 读不到文件或键不存在都返回 0: 这些事件文件不是所有内核版本/所有控制器都有,
// 缺失应当被当作"没有发生", 而不是让整次运行失败。
func cgroupEventCount(path, key string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			return n
		}
	}
	return 0
}
