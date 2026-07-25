// Package replay —— 离线轨迹回放评估 harness (design/03 §4.5 + Hermes H12)。
//
// # 它解决什么
//
// 此前 `tests/eval` 是**特性自评分**: 人工列 11 个维度、按"这个能力在不在"打分,
// 及格线 60%。它回答不了进化产物该不该晋升这个问题 —— 一个新技能/新 prompt 是否
// 真的更好, 只能靠"感觉变好了"。设计要求的是: 固定任务集 + 评分 + 确定性断言,
// **任何进化产物晋升前必过**。
//
// # 为什么是"回放"而不是"重跑"
//
// 回放消费的是**历史轨迹**(TraceStore 的 Span / 团队 trajectories), 而不是重新
// 起团队跑一遍。原因是成本与可比性:
//
//   - 重跑要真花 LLM 调用, 每次评测都是一笔账; 而晋升门禁要频繁跑。
//   - 重跑的输入不可控 (LLM 有随机性), 两次结果不同就无法归因到"是产物变好了
//     还是模型这次心情好"。回放固定输入, 差异只来自被评估的产物。
//
// 代价是回放只能评"给定同样的输入, 产物是否让产出更好", 评不了"产物是否让 agent
// 做出不同的动作序列"。后者需要真跑, 属 H6 多档冒烟的范畴 (本文件不做, 见文末)。
//
// # H12 的工程规范(逐条落地, 这些不是锦上添花)
//
//   - **并发信号量**: 不限并发会打爆网关配额, 而配额是全平台共享的。
//   - **每任务硬超时**: 单个任务卡住不能让整轮评测挂死。
//   - **每完成一条立即流式落盘 JSONL**: 中断不丢已完成结果 —— 评测动辄几十分钟,
//     跑到一半被 Ctrl-C 却什么都没留下是最浪费的失败模式。
//   - **续跑按内容指纹而非序号**: 任务集增删后按序号续跑会错位, 把 A 的结果记到
//     B 头上。指纹用任务内容哈希。
//   - **空产出短路**: zero-turn 轨迹 (agent 什么都没做) 直接 0 分, 不启动评估器 ——
//     否则会为一个空字符串付一次 LLM judge 的钱。
package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Task 一条回放任务: 固定输入 + 期望。
type Task struct {
	// ID 人类可读的任务标识 (仅用于展示; 续跑判重用 Fingerprint 而非它)。
	ID string `json:"id"`
	// Objective 任务目标 (回放时原样喂给被评估的产物)。
	Objective string `json:"objective"`
	// Input 历史轨迹里该任务的输入上下文 (prompt 片段/上游产出)。
	Input string `json:"input,omitempty"`
	// Expect 确定性断言: 产出必须包含的子串 (全部命中才算通过)。
	// 产码任务应改用 Gate 跑真门禁而不是靠字符串匹配。
	Expect []string `json:"expect,omitempty"`
	// Gate 确定性门禁命令 (如 "go build ./..."); 非空时在 Workspace 下执行,
	// exit code 0 即通过。这是"产码任务跑真门禁"的落点。
	Gate      string `json:"gate,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// Fingerprint 任务内容指纹 (续跑判重用)。
//
// 用内容而非序号: 任务集增删后按序号续跑会错位, 把 A 的结果记到 B 头上。
func (t Task) Fingerprint() string {
	h := sha256.Sum256([]byte(t.Objective + "\x00" + t.Input + "\x00" + strings.Join(t.Expect, "\x1f") + "\x00" + t.Gate))
	return hex.EncodeToString(h[:8])
}

// Result 一条任务的评测结果 (流式落盘的行格式)。
type Result struct {
	Fingerprint string  `json:"fp"`
	TaskID      string  `json:"task_id"`
	Score       float64 `json:"score"` // [0,1]
	Passed      bool    `json:"passed"`
	Output      string  `json:"output,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	Err         string  `json:"err,omitempty"`
	DurMS       int64   `json:"dur_ms"`
	// ZeroTurn 空产出短路标记: 未启动评估器, 直接 0 分。
	ZeroTurn bool `json:"zero_turn,omitempty"`
}

// Candidate 被评估的进化产物。回放把 Objective+Input 喂给它, 拿回产出。
//
// 用接口而非具体类型: 被评估的可能是"注入了某技能的 agent""某个 prompt 版本"
// "某个图模板", 它们唯一的共性就是"给定输入产出文本"。
type Candidate interface {
	Name() string
	Produce(ctx context.Context, t Task) (string, error)
}

// Judge 打分器。返回 [0,1] 与理由。
//
// 确定性断言 (Task.Expect / Task.Gate) 由 harness 自己判, 不经 Judge ——
// 能确定性判的就不该花 LLM 的钱。Judge 只用于"质量好不好"这种没有确定答案的维度。
type Judge interface {
	Score(ctx context.Context, t Task, output string) (float64, string, error)
}

// GateRunner 执行确定性门禁命令 (由调用方注入, 便于测试替身)。
type GateRunner func(ctx context.Context, workspace, cmd string) error

// Config harness 参数 (H12 的工程规范都在这)。
type Config struct {
	// Concurrency 并发上限; <=0 取 2。不限并发会打爆全平台共享的网关配额。
	Concurrency int
	// TaskTimeout 单任务硬超时; <=0 取 5 分钟。
	TaskTimeout time.Duration
	// OutPath 流式结果 JSONL 落盘路径; 空则不落盘 (仅内存返回)。
	OutPath string
	// Resume 为 true 时跳过 OutPath 里已有相同指纹的任务。
	Resume bool
	// PassScore 通过线; <=0 取 0.6 (与既有 tests/eval 的 60% 及格线一致)。
	PassScore float64
	// Gate 确定性门禁执行器; nil 时 Task.Gate 被忽略并在 Reason 里注明。
	Gate GateRunner
}

func (c Config) withDefaults() Config {
	if c.Concurrency <= 0 {
		c.Concurrency = 2
	}
	if c.TaskTimeout <= 0 {
		c.TaskTimeout = 5 * time.Minute
	}
	if c.PassScore <= 0 {
		c.PassScore = 0.6
	}
	return c
}

// Report 一轮回放的汇总。
type Report struct {
	Candidate string   `json:"candidate"`
	Total     int      `json:"total"`
	Ran       int      `json:"ran"`     // 本轮实际跑的 (续跑会小于 Total)
	Skipped   int      `json:"skipped"` // 续跑跳过的
	Passed    int      `json:"passed"`
	MeanScore float64  `json:"mean_score"`
	Results   []Result `json:"results"`
}

// Harness 回放执行器。
type Harness struct {
	cfg   Config
	judge Judge

	mu  sync.Mutex
	out *os.File
}

// New 构造 harness。judge 可为 nil —— 此时只做确定性断言 (Expect/Gate),
// 这正是"能确定性判的就不花 LLM 钱"的极端情形。
func New(cfg Config, judge Judge) (*Harness, error) {
	cfg = cfg.withDefaults()
	h := &Harness{cfg: cfg, judge: judge}
	if cfg.OutPath != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.OutPath), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(cfg.OutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		h.out = f
	}
	return h, nil
}

// Close 关闭落盘句柄。
func (h *Harness) Close() error {
	if h == nil || h.out == nil {
		return nil
	}
	return h.out.Close()
}

// LoadTasks 从 JSONL 读任务集 (每行一个 Task)。
func LoadTasks(path string) ([]Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Task
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var t Task
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			return nil, fmt.Errorf("第 %d 行解析失败: %w", i+1, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// doneFingerprints 读已有结果文件里的指纹集 (续跑用)。
func doneFingerprints(path string) map[string]bool {
	done := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return done
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r Result
		if json.Unmarshal([]byte(line), &r) == nil && r.Fingerprint != "" {
			done[r.Fingerprint] = true
		}
	}
	return done
}

// Run 对 candidate 跑一轮回放。
func (h *Harness) Run(ctx context.Context, cand Candidate, tasks []Task) (Report, error) {
	if cand == nil {
		return Report{}, errors.New("replay: candidate 为 nil")
	}
	rep := Report{Candidate: cand.Name(), Total: len(tasks)}

	var done map[string]bool
	if h.cfg.Resume && h.cfg.OutPath != "" {
		done = doneFingerprints(h.cfg.OutPath)
	}

	sem := make(chan struct{}, h.cfg.Concurrency)
	var wg sync.WaitGroup
	results := make([]Result, len(tasks))
	skipped := make([]bool, len(tasks))

	for i, t := range tasks {
		if done[t.Fingerprint()] {
			skipped[i] = true
			continue
		}
		wg.Add(1)
		go func(i int, t Task) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// panic 兜住: 一个任务的评估器炸了不该带走整轮评测。
			defer func() {
				if r := recover(); r != nil {
					results[i] = Result{Fingerprint: t.Fingerprint(), TaskID: t.ID,
						Err: fmt.Sprintf("panic: %v", r)}
				}
			}()
			results[i] = h.runOne(ctx, cand, t)
			h.appendResult(results[i])
		}(i, t)
	}
	wg.Wait()

	var sum float64
	for i := range tasks {
		if skipped[i] {
			rep.Skipped++
			continue
		}
		rep.Ran++
		r := results[i]
		rep.Results = append(rep.Results, r)
		sum += r.Score
		if r.Passed {
			rep.Passed++
		}
	}
	if rep.Ran > 0 {
		rep.MeanScore = sum / float64(rep.Ran)
	}
	// 结果按指纹排序, 保证报告可比 (并发完成顺序不该影响输出)。
	sort.Slice(rep.Results, func(i, j int) bool { return rep.Results[i].Fingerprint < rep.Results[j].Fingerprint })
	return rep, nil
}

func (h *Harness) runOne(ctx context.Context, cand Candidate, t Task) Result {
	start := time.Now()
	res := Result{Fingerprint: t.Fingerprint(), TaskID: t.ID}
	cctx, cancel := context.WithTimeout(ctx, h.cfg.TaskTimeout)
	defer cancel()

	// **硬**超时: 把 Produce 放进 goroutine 并 select ctx —— 只把带超时的 ctx 传给
	// candidate 是不够的, 那只在 candidate **配合** ctx 时才生效。不配合的实现
	// (或阻塞在某个不认 ctx 的系统调用上) 会拖住整轮评测, 而这正是 H12 要防的
	// 失败模式。
	//
	// 代价写明: 超时后那个 goroutine 会一直挂到 Produce 自己返回, 即**泄漏**一个
	// goroutine。这是不配合 ctx 的实现无法避免的代价 —— 我们选择"泄漏一个 goroutine
	// 但整轮继续"而不是"整轮挂死"。缓冲通道保证泄漏的 goroutine 最终能发送完退出,
	// 不会永久阻塞在 send 上。
	type produced struct {
		out string
		err error
	}
	ch := make(chan produced, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- produced{err: fmt.Errorf("panic: %v", r)}
			}
		}()
		o, e := cand.Produce(cctx, t)
		ch <- produced{out: o, err: e}
	}()

	var out string
	select {
	case p := <-ch:
		out, res.Err = p.out, ""
		if p.err != nil {
			res.Err = p.err.Error()
		}
	case <-cctx.Done():
		res.DurMS = time.Since(start).Milliseconds()
		res.Err = fmt.Sprintf("硬超时 %s 未返回 (candidate 未响应 ctx 取消)", h.cfg.TaskTimeout)
		return res
	}
	res.DurMS = time.Since(start).Milliseconds()
	if res.Err != "" {
		return res
	}
	res.Output = out

	// 空产出短路: zero-turn 直接 0 分, **不启动评估器** ——
	// 否则会为一个空字符串付一次 LLM judge 的钱。
	if strings.TrimSpace(out) == "" {
		res.ZeroTurn = true
		res.Reason = "空产出 (zero-turn), 短路为 0 分, 未启动评估器"
		return res
	}

	// ① 确定性断言优先, 且是**硬否决**: 断言不过直接 0 分, 不问 Judge。
	// 能确定性判的就不该花 LLM 的钱, 也不该让 LLM 的宽容盖过硬事实。
	for _, want := range t.Expect {
		if !strings.Contains(out, want) {
			res.Reason = fmt.Sprintf("确定性断言未命中: 缺少 %q", want)
			return res
		}
	}
	if t.Gate != "" {
		if h.cfg.Gate == nil {
			res.Reason = "任务声明了 Gate 但未注入 GateRunner, 该门禁被跳过"
		} else if err := h.cfg.Gate(cctx, t.Workspace, t.Gate); err != nil {
			res.Reason = fmt.Sprintf("确定性门禁失败: %v", err)
			return res
		}
	}

	// ② 无 Judge 时: 确定性断言全过即满分。
	if h.judge == nil {
		res.Score, res.Passed = 1, true
		if res.Reason == "" {
			res.Reason = "确定性断言全过 (未配置 Judge)"
		}
		return res
	}

	score, why, err := h.judge.Score(cctx, t, out)
	if err != nil {
		res.Err = "judge: " + err.Error()
		return res
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	res.Score = score
	res.Reason = why
	res.Passed = score >= h.cfg.PassScore
	return res
}

// appendResult 流式落盘一行 (H12: 中断不丢已完成结果)。
// 落盘失败只记在返回值之外的日志语义里——评测结果比落盘更重要, 不因写盘失败中断。
func (h *Harness) appendResult(r Result) {
	if h == nil || h.out == nil {
		return
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, _ = h.out.Write(append(line, '\n'))
}

// Compare 配对对照: 同一任务集下 baseline 与 candidate 的 uplift。
//
// 这是 design/03 §4.5「uplift 因果评估」的落点: 用配对对照替代"感觉变好了"。
// 返回 uplift = 候选均分 - 基线均分, 正值表示更好。
func Compare(base, cand Report) float64 { return cand.MeanScore - base.MeanScore }
