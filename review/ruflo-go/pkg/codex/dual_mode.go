// 本文件实现双模编排的执行与记忆汇聚：管理 Claude / Codex / ruflo 无头进程的完整调用生命周期。
//
// 设计要点：
//   - SpawnHeadlessWorker：按 platform 选择可执行文件与参数（claude -p / codex -p / ruflo -p），
//     环境变量 RUFL_CLAUDE_BIN、RUFL_CODEX_BIN、RUFL_BIN 可覆盖默认命令名；RUFL_NAMESPACE 传递命名空间。
//   - RunCollaboration：根据 WorkerConfig.DependsOn 构建有向依赖图，按拓扑深度分「波次」；
//     同一波内 goroutine 并行，波次间顺序执行；任一 worker 非零退出或 Error 则整体失败返回。
//   - StoreTaskContext / CollectSharedMemory：通过 ruflo memory store/list/retrieve --format json 与 CLI 交互，
//     实现跨平台共享键值上下文（与 orchestrator 中的 Namespace 默认 collaboration 对齐）。
package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// WorkerConfig 描述双模流水线中的一个工作者：依赖边通过角色名 DependsOn 指向其他 Worker 的 Role。
type WorkerConfig struct {
	Platform  string   // 平台："claude"、"codex" 或 "ruflo"
	Role      string   // 角色名，用于 workerKey 与依赖解析；空则运行时会生成占位名
	Prompt    string   // 传给无头进程的完整自然语言任务描述
	DependsOn []string // 必须先完成的角色名列表（用于拓扑分层）
	Namespace string   // 非空时覆盖编排器默认 Namespace，仅该 worker 使用
}

// WorkerResult 记录一次无头调用的输出、退出码、耗时与错误（Error 不序列化到 JSON）。
type WorkerResult struct {
	WorkerID string        `json:"worker_id"` // 通常为 platform:role
	Platform string        `json:"platform"`
	Role     string        `json:"role"`
	Output   string        `json:"output"`    // 合并 stdout+stderr
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration"`
	Error    error         `json:"-"`
}

// SpawnHeadlessWorker 根据 platform 执行 `claude -p`、`codex -p` 或 `ruflo -p`；提示词带 [platform:role] 前缀。
// 环境变量 RUFL_CLAUDE_BIN、RUFL_CODEX_BIN、RUFL_BIN 可覆盖默认可执行文件名。
func (o *DualModeOrchestrator) SpawnHeadlessWorker(platform, role, prompt string) (*WorkerResult, error) {
	if o == nil {
		return nil, fmt.Errorf("codex: nil orchestrator")
	}
	start := time.Now()
	full := fmt.Sprintf("[%s:%s] %s", strings.TrimSpace(platform), strings.TrimSpace(role), prompt)
	bin, args := headlessArgv(platform, full, o.rufloBin())
	cmd := exec.Command(bin, args...)
	cmd.Dir = workspaceDir()
	cmd.Env = os.Environ()
	if ns := o.Namespace; ns != "" {
		cmd.Env = append(cmd.Env, "RUFL_NAMESPACE="+ns)
	}
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)
	wid := fmt.Sprintf("%s:%s", strings.TrimSpace(platform), strings.TrimSpace(role))
	res := &WorkerResult{
		WorkerID: wid,
		Platform: platform,
		Role:     role,
		Output:   string(out),
		Duration: dur,
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			res.Error = err
			return res, nil
		}
		res.Error = err
		res.ExitCode = -1
		return res, err
	}
	return res, nil
}

// headlessArgv 返回 [可执行文件, 参数...]：codex → -p；ruflo → -p；默认 claude → -p。
func headlessArgv(platform, prompt, rufloDefault string) (bin string, args []string) {
	p := strings.ToLower(strings.TrimSpace(platform))
	switch p {
	case "codex":
		bin = os.Getenv("RUFL_CODEX_BIN")
		if bin == "" {
			bin = "codex"
		}
		return bin, []string{"-p", prompt}
	case "ruflo":
		bin = os.Getenv("RUFL_BIN")
		if bin == "" {
			bin = rufloDefault
		}
		return bin, []string{"-p", prompt}
	default:
		bin = os.Getenv("RUFL_CLAUDE_BIN")
		if bin == "" {
			bin = "claude"
		}
		return bin, []string{"-p", prompt}
	}
}

// RunCollaboration 将 workers 按依赖拓扑分成多层波次：同层并行，层间串行；遇错短路返回已收集结果。
func (o *DualModeOrchestrator) RunCollaboration(workers []WorkerConfig) ([]WorkerResult, error) {
	if o == nil {
		return nil, fmt.Errorf("codex: nil orchestrator")
	}
	levels := topoLevels(workers)
	var acc []WorkerResult
	for _, tier := range levels {
		ch := make(chan WorkerResult, len(tier))
		var wg sync.WaitGroup
		for _, w := range tier {
			w := w
			wg.Add(1)
			go func() {
				defer wg.Done()
				ns := w.Namespace
				if ns == "" {
					ns = o.Namespace
				}
				ow := o
				if ns != o.Namespace {
					cp := *o
					cp.Namespace = ns
					ow = &cp
				}
				res, err := ow.SpawnHeadlessWorker(w.Platform, w.Role, w.Prompt)
				if err != nil {
					ch <- WorkerResult{
						WorkerID: workerKey(w),
						Platform: w.Platform,
						Role:     w.Role,
						Error:    err,
						ExitCode: -1,
					}
					return
				}
				res.WorkerID = workerKey(w)
				ch <- *res
			}()
		}
		wg.Wait()
		close(ch)
		for r := range ch {
			acc = append(acc, r)
			if r.Error != nil {
				return acc, r.Error
			}
			if r.ExitCode != 0 {
				return acc, fmt.Errorf("codex: worker %s exited %d", r.WorkerID, r.ExitCode)
			}
		}
	}
	return acc, nil
}

// workerKey 生成工作者唯一键：platform:role（role 空时为 anon）。
func workerKey(w WorkerConfig) string {
	r := strings.TrimSpace(w.Role)
	if r == "" {
		return strings.TrimSpace(w.Platform) + ":anon"
	}
	return strings.TrimSpace(w.Platform) + ":" + r
}

// topoLevels 将 WorkerConfig 列表按依赖深度分桶：先为每个 role 建索引，再对每个节点计算 roleDepth（记忆化+环检测），最后按深度聚合为非空桶序列。
func topoLevels(workers []WorkerConfig) [][]WorkerConfig {
	byRole := make(map[string]WorkerConfig)
	order := make([]string, 0, len(workers))
	seen := make(map[string]struct{})
	for _, w := range workers {
		k := strings.TrimSpace(w.Role)
		if k == "" {
			k = fmt.Sprintf("_w%d", len(byRole))
		}
		if _, ok := seen[k]; ok {
			k = fmt.Sprintf("%s_%d", k, len(byRole))
		}
		seen[k] = struct{}{}
		wc := w
		if strings.TrimSpace(wc.Role) == "" {
			wc.Role = k
		}
		byRole[k] = wc
		order = append(order, k)
	}
	memo := make(map[string]int)
	depth := make(map[string]int)
	for _, k := range order {
		depth[k] = roleDepth(byRole[k], byRole, memo, make(map[string]bool))
	}
	maxD := 0
	for _, d := range depth {
		if d > maxD {
			maxD = d
		}
	}
	buckets := make([][]WorkerConfig, maxD+1)
	for _, k := range order {
		d := depth[k]
		buckets[d] = append(buckets[d], byRole[k])
	}
	var out [][]WorkerConfig
	for _, b := range buckets {
		if len(b) > 0 {
			out = append(out, b)
		}
	}
	return out
}

// roleDepth 递归计算某角色在依赖 DAG 中的最大深度：memo 缓存结果，visiting 检测环（环上节点深度视为 0 分支）。
func roleDepth(w WorkerConfig, byRole map[string]WorkerConfig, memo map[string]int, visiting map[string]bool) int {
	k := strings.TrimSpace(w.Role)
	if k == "" {
		return 0
	}
	if d, ok := memo[k]; ok {
		return d
	}
	if visiting[k] {
		return 0
	}
	visiting[k] = true
	maxD := 0
	for _, dep := range w.DependsOn {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		dw, ok := byRole[dep]
		if !ok {
			continue
		}
		d := roleDepth(dw, byRole, memo, visiting) + 1
		if d > maxD {
			maxD = d
		}
	}
	visiting[k] = false
	memo[k] = maxD
	return maxD
}

// StoreTaskContext 通过 ruflo memory store 将键值写入共享命名空间（Namespace 空时默认 "collaboration"）。
func (o *DualModeOrchestrator) StoreTaskContext(key, value string) error {
	if o == nil {
		return fmt.Errorf("codex: nil orchestrator")
	}
	ns := o.Namespace
	if ns == "" {
		ns = "collaboration"
	}
	cmd := exec.Command(o.rufloBin(), "--format", "json", "memory", "store",
		"--key", key, "--value", value, "--namespace", ns)
	cmd.Dir = workspaceDir()
	cmd.Env = os.Environ()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codex: memory store: %w: %s", err, strings.TrimSpace(buf.String()))
	}
	return nil
}

type memListJSON struct {
	Keys []string `json:"keys"`
}

type memRetrieveJSON struct {
	Entry struct {
		Value string `json:"value"`
	} `json:"entry"`
}

// CollectSharedMemory 列出命名空间下键（limit 500）并逐个 retrieve，返回 key→value 映射（解析失败键跳过）。
func (o *DualModeOrchestrator) CollectSharedMemory() (map[string]string, error) {
	if o == nil {
		return nil, fmt.Errorf("codex: nil orchestrator")
	}
	ns := o.Namespace
	if ns == "" {
		ns = "collaboration"
	}
	cmd := exec.Command(o.rufloBin(), "--format", "json", "memory", "list",
		"--namespace", ns, "--limit", "500")
	cmd.Dir = workspaceDir()
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("codex: memory list: %w", err)
	}
	var list memListJSON
	if err := json.Unmarshal(bytes.TrimSpace(out), &list); err != nil {
		return nil, fmt.Errorf("codex: parse memory list: %w", err)
	}
	res := make(map[string]string, len(list.Keys))
	sort.Strings(list.Keys)
	for _, key := range list.Keys {
		cmd := exec.Command(o.rufloBin(), "--format", "json", "memory", "retrieve",
			"--key", key, "--namespace", ns)
		cmd.Dir = workspaceDir()
		cmd.Env = os.Environ()
		raw, err := cmd.Output()
		if err != nil {
			continue
		}
		var ret memRetrieveJSON
		if err := json.Unmarshal(bytes.TrimSpace(raw), &ret); err != nil {
			continue
		}
		res[key] = ret.Entry.Value
	}
	return res, nil
}

// WaitForAll 按参数顺序从每个 channel 读取一个 WorkerResult（nil channel 对应零值）。
func WaitForAll(chs ...<-chan WorkerResult) []WorkerResult {
	out := make([]WorkerResult, len(chs))
	for i, ch := range chs {
		if ch == nil {
			continue
		}
		out[i] = <-ch
	}
	return out
}
