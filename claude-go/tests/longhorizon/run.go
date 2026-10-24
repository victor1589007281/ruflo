// run.go —— 真实 Runner (env-gated) 与基线配置。
//
// # 为什么真实 Runner 也要在仓库里, 而不是"跑的时候临时写一个"
//
// 临时脚本的失效形态是**沉默**的: 它可能根本没设那两个开关 (于是 A/B 两组跑的是
// 同一份配置), 也可能把任务的初始代码忘了展开 (于是每个任务都在一个空目录上
// 从零开始, 成功率恒 0)。这两种都会产出一份看起来完整的报告。
// 放在仓库里, 它至少能被 `go vet` 看住, 且它的开关名/配置路径是**声明**出来的,
// 有断言盯着。
//
// # 它为什么不是默认跑的
//
// 一次真实 run 要模型、要几分钟、要钱。本仓已有的约定就是 env-gated
// (见 pkg/e2eagentdb: 未设 CLAUDE_GO_E2E_BIN 即 skip)。这里沿用同一形态:
// 未设 LONGHORIZON_BIN 就跳过, 于是 `go test ./...` 在没有模型的机器上全绿。
package longhorizon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 环境变量。
const (
	// BinEnv 指向 claude-go 二进制。未设 = 跳过真实 run。
	BinEnv = "LONGHORIZON_BIN"
	// ModelEnv 覆盖模型别名 (默认用配置里的 ai.modelAlias)。
	ModelEnv = "LONGHORIZON_MODEL"
	// TimeoutEnv 单个任务的超时 (Go duration, 默认 20m)。
	TimeoutEnv = "LONGHORIZON_TIMEOUT"
	// KeepEnv 设为 1 时保留每个任务的工作目录 (排查用)。
	KeepEnv = "LONGHORIZON_KEEP"
)

// 被测开关的**声明**。开关名与环境变量取自 pkg/agent/run_interceptors.go 的
// runBuiltinPhases: "gate" 是非 core 环, 该变量的白名单/off 语义由
// parseInterceptorSwitch 定义。
const (
	runInterceptorsEnv = "CLAUDE_GO_RUN_INTERCEPTORS"
	// GatePhaseName 验证闸门在运行级拦截器链里的名字。
	// 与生产代码里的那一份必须一致 —— 不一致时 A/B 的"开"组其实是关的,
	// 而两组输入 diff 仍然好看 (变量确实设了)。
	GatePhaseName = "gate"
)

// GateKnob 描述被测开关: 如何用配置 + 环境变量把它分开。
//
// 注意 ConfigPath 是刻意留空的: 本仓的验证闸门**不在配置文件里**, 它只由
// CLAUDE_GO_RUN_INTERCEPTORS 控制。把不存在的配置键写进声明, 会让报告上看起来
// "配置也查过了", 而其实那一处恒等。空路径的语义是"这个开关不走配置",
// 由 DiffPaths / DiffEnv 如实体现。
func GateKnob() KnobEnv {
	return KnobEnv{
		Name:       "验证闸门 (run phase: gate)",
		Env:        runInterceptorsEnv,
		ConfigPath: "",
		// 白名单形态: 只留 gate 这一环 (finalize 是 core, 恒在)。
		On:  GatePhaseName,
		Off: "off",
	}
}

// GatePair 构造闸门开/关的配对输入。
//
// 两组的**配置文档完全相同** —— 差异只在环境变量上, 这是本仓的实际情况。
// 修一下这个形状 (把闸门做成配置项) 是 H0-3 的接口工作, 不属于本包。
func GatePair(base any) ConfigPair {
	return ConfigPair{
		Base: base,
		A:    RunSpec{Config: base, Env: map[string]string{runInterceptorsEnv: "off"}},
		B:    RunSpec{Config: base, Env: map[string]string{runInterceptorsEnv: GatePhaseName}},
		Knob: GateKnob(),
	}
}

// MaxTurnsKnob H4-3 的开关: 长程档上限。
//
// 与闸门不同, 这一条**在配置里且确实生效** (见 pkg/agent/modelconfig 的
// taskclass.go 与 pkg/feishu 的 stageMaxTurns)。放在这里是为了让 H4-3 的增益
// 也能被同一台 A/B 量出来 —— 方案 §21.5.6 把 H4-3 的验收单列成"轮数中位数 > 100",
// 而"放开之后成功率有没有变"只有配对 A/B 答得了。
func MaxTurnsKnob() KnobEnv {
	return KnobEnv{
		Name:       "长程档 maxTurns (ai.longHorizonMaxTurns)",
		ConfigPath: "/ai/longHorizonMaxTurns",
		On:         "150",
		Off:        "0", // 0 = 未开启分层 = 改造前行为
	}
}

// MaxTurnsPair 构造长程档开/关的配对输入 (两组配置只差这一个键)。
func MaxTurnsPair(base any) (ConfigPair, error) {
	on, err := SetPath(base, MaxTurnsKnob().ConfigPath, 150)
	if err != nil {
		return ConfigPair{}, err
	}
	off, err := SetPath(base, MaxTurnsKnob().ConfigPath, 0)
	if err != nil {
		return ConfigPair{}, err
	}
	return ConfigPair{
		Base: base,
		A:    RunSpec{Config: off},
		B:    RunSpec{Config: on},
		Knob: MaxTurnsKnob(),
	}, nil
}

// BaselineConfig 一份 H4-2 的基线配置。
//
// 刻意**不读 /mnt/data 上那份生产 config.json**: 里面是活凭据, 而且它会变 ——
// 报告的"配置 diff 可查"要的是一份能被复现的输入。用最小基线 + 环境变量注入
// provider 凭据 (见 RunnerFromEnv)。
func BaselineConfig(modelAlias string) map[string]any {
	return map[string]any{
		"ai": map[string]any{
			"modelAlias": modelAlias,
			"maxTurns":   25,
			"maxTokens":  32768,
		},
	}
}

// ExecRunner 用真实 claude-go 二进制跑一个任务。
type ExecRunner struct {
	Bin      string
	Model    string
	Timeout  time.Duration
	KeepWork bool
	// Verbose 为真时把每次运行的输出打到 stderr。
	Verbose bool
}

// Run 见 Runner。
func (e *ExecRunner) Run(task Task, spec RunSpec, workDir string) (TaskOutcome, error) {
	var out TaskOutcome
	if e.Bin == "" {
		return out, fmt.Errorf("claude-go 二进制路径为空")
	}

	cfgBytes, err := json.MarshalIndent(spec.Config, "", "  ")
	if err != nil {
		return out, fmt.Errorf("序列化配置失败: %w", err)
	}
	cfgPath := filepath.Join(workDir, "claude-go-config.json")
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		return out, fmt.Errorf("写配置失败: %w", err)
	}

	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	model := e.Model
	if model == "" {
		if ai, ok := spec.Config.(map[string]any)["ai"].(map[string]any); ok {
			model, _ = ai["modelAlias"].(string)
		}
	}

	args := []string{"run", "--config", cfgPath, "--cwd", workDir}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, task.Goal)

	cmd := exec.CommandContext(ctx, e.Bin, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), envSlice(spec.Env)...)
	runOut, runErr := cmd.CombinedOutput()
	if e.Verbose {
		fmt.Fprintf(os.Stderr, "[%s] run 输出:\n%s\n", task.ID, runOut)
	}

	out.Turns = parseTurns(runOut)
	out.CompactSpans = parseCompactSpans(runOut)

	// 客观终态: 由台子**独立复跑**验证命令, 不信 run 的自报。
	// 这一条是"客观"两个字的全部内容 —— 让 agent 自报成功, 测的就是它的措辞。
	verify := exec.Command(task.VerifyCmd[0], task.VerifyCmd[1:]...)
	verify.Dir = workDir
	verify.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	vOut, vErr := verify.CombinedOutput()
	out.Passed = vErr == nil
	if !out.Passed {
		out.Err = truncate(string(vOut), 2000)
	}
	if runErr != nil && !out.Passed {
		out.Err = fmt.Sprintf("运行: %v; 验证: %s", runErr, truncate(string(vOut), 1500))
	}
	return out, nil
}

// envSlice 把环境变量 map 展开成 "k=v" 切片, 供 exec.Cmd.Env 使用。
//
// 注意：map 遍历顺序随机, 展开结果顺序不稳定——env 语义上无所谓, 但不要拿它做相等比较。
func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// truncate 按字节截断字符串并追加「…(截断)」标记。
//
// 注意：截断点是字节下标而非字符边界, 中文可能被切成半个字符, 拼出的报告里会出现乱码。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(截断)"
}

// parseTurns 从运行输出里读回合数。
//
// **读不到就是 0** —— 刻意不给默认值。给个漂亮的默认值会让"输出格式变了、
// 什么都没读到"与"真的跑了那么多轮"在报告上一样, 而 H4-1 的验收正是判这个数。
func parseTurns(out []byte) int {
	return lastIntAfter(out, []string{"turns=", "回合数=", "turn_count="})
}

// parseCompactSpans 从运行输出里读压缩次数; 与 parseTurns 同一条纪律——读不到就是 0, 不给默认值。
func parseCompactSpans(out []byte) int {
	return lastIntAfter(out, []string{"compact_spans=", "压缩次数=", "compactions="})
}

// lastIntAfter 找 markers 里任一标记后紧跟的整数, 取**最后**一次出现
// (汇总行通常在末尾)。
func lastIntAfter(out []byte, markers []string) int {
	s := string(out)
	best := 0
	found := false
	for _, m := range markers {
		idx := 0
		for {
			i := strings.Index(s[idx:], m)
			if i < 0 {
				break
			}
			at := idx + i + len(m)
			idx = at
			n := 0
			digits := 0
			for at < len(s) && s[at] >= '0' && s[at] <= '9' {
				n = n*10 + int(s[at]-'0')
				at++
				digits++
			}
			if digits > 0 {
				best = n
				found = true
			}
		}
	}
	if !found {
		return 0
	}
	return best
}

// RunnerFromEnv 按环境变量装配真实 Runner。未配置时返回 nil (调用方跳过)。
func RunnerFromEnv() (*ExecRunner, error) {
	bin := os.Getenv(BinEnv)
	if bin == "" {
		return nil, nil
	}
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("%s=%s 不可用: %w", BinEnv, bin, err)
	}
	r := &ExecRunner{
		Bin:      bin,
		Model:    os.Getenv(ModelEnv),
		KeepWork: os.Getenv(KeepEnv) == "1",
	}
	if d := os.Getenv(TimeoutEnv); d != "" {
		parsed, err := time.ParseDuration(d)
		if err != nil {
			return nil, fmt.Errorf("%s=%q 不是合法 duration: %w", TimeoutEnv, d, err)
		}
		r.Timeout = parsed
	}
	return r, nil
}
