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

// WorkerConfig describes one dual-mode worker with dependency edges by role name.
type WorkerConfig struct {
	Platform    string   // "claude", "codex", or "ruflo"
	Role        string
	Prompt      string
	DependsOn   []string
	Namespace   string
}

// WorkerResult captures the outcome of a headless worker invocation.
type WorkerResult struct {
	WorkerID string        `json:"worker_id"`
	Platform string        `json:"platform"`
	Role     string        `json:"role"`
	Output   string        `json:"output"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration"`
	Error    error         `json:"-"`
}

// SpawnHeadlessWorker runs `claude -p`, `codex -p`, or `ruflo -p` depending on platform.
// Environment: RUFL_CLAUDE_BIN, RUFL_CODEX_BIN, RUFL_BIN override command names.
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

// RunCollaboration executes workers in topological waves; within each wave, workers run in parallel.
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

func workerKey(w WorkerConfig) string {
	r := strings.TrimSpace(w.Role)
	if r == "" {
		return strings.TrimSpace(w.Platform) + ":anon"
	}
	return strings.TrimSpace(w.Platform) + ":" + r
}

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

// StoreTaskContext writes a key into the shared memory namespace via the ruflo CLI.
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

// CollectSharedMemory lists keys in the orchestrator namespace and retrieves each value.
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

// WaitForAll drains one result per channel in order.
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
