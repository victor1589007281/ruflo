// evotools.go —— 进化操作台的 agent 自助工具 (design/03 §4.7 的 evo_* 七件套)。
//
// # 为什么必须是工具而不是 CLI 子命令
//
// 此前实现是 `claude-go evo {status,audit,promote,rollback}` 四个**人**敲的 CLI。
// 设计要的是另一回事: hermes 的顶层洞见是"agent 即后训练工程师" —— 整条管线
// (发现环境 → 读 verifier → 造候选 → 冒烟 → 实验 → 限速监控 → 晋升) 由 agent 用
// 10 个 rl_* 工具自己驱动, 人只下目标。工具与 CLI 的差别不是包装, 是**谁能调用**:
// CLI 只有人能敲, 于是 E4 的验收项「agent 经 evo_* 全自助完成 propose→smoke→
// experiment→promote」结构上不可能达成。
//
// # H7 锁定字段护栏 (本文件最重要的一段)
//
// agent 能改的只有安全字段: 实验样本量、任务集选择、shadow 比例、描述文本。
// **治理参数一律锁定** —— 晋升阈值、uplift 显著性、judge 模型、生命周期规则。
// 请求改锁定字段不是"忽略", 而是**显式拒绝并说明原因** (hermes rl_edit_config 对
// LOCKED_FIELDS 的语义)。见 lockedFields 与 evo_propose 的 config 分支。
//
// 这把 §4.6 的四律从约定变成机械强制: agent 可以自由做实验, 但闸门标准本身不在
// 它的动作空间里。
//
// # H9 限速做进工具层
//
// evo_status 对同一实验 30 分钟内只答一次, 超出直接返回 rate_limited + 剩余秒数。
// 这是"别轮询"这条纪律的**机械实现**, 而不是提示词里的一句请求 —— 提示词里的纪律
// 在长会话里必然被遗忘。限速时间戳持久化进实验文件, 所以进程重启不会重置它。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/evolution/console"
	"github.com/anthropic/claude-go/pkg/evolution/govern"
	"github.com/anthropic/claude-go/pkg/evolution/learners"
	"github.com/anthropic/claude-go/pkg/evolution/replay"
	"github.com/anthropic/claude-go/pkg/evolution/skillaudit"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// lockedFields H7 锁定字段: agent 请求修改这些一律拒绝。
//
// 取值就是 design/03 §4.7 那句"晋升阈值、uplift 显著性标准、judge 模型选择、
// 学习预算上限、生命周期规则"。它们在代码里是不可导出常量 (skillaudit.promoteThreshold /
// learners.awmMinScore / replay 的 MaxFailRate 默认值), 本表只负责把"为什么拒绝"讲清楚。
var lockedFields = map[string]string{
	"promote_threshold":  "晋升阈值锁定为 skillaudit 的不可导出常量 —— 闸门标准不在被优化系统的动作空间里",
	"retire_threshold":   "退役阈值同上",
	"min_samples":        "样本量下限锁定: 放宽它等于允许用一次偶然成功晋升产物",
	"uplift_threshold":   "uplift 显著性标准锁定",
	"judge_model":        "judge/反思器档位锁定 (§4.2 H3: 禁止用被评估的主模型自评)",
	"learn_budget":       "学习预算上限锁定 (§4.5 学习花费 ≤ 总花费 10%)",
	"lifecycle":          "生命周期规则锁定 (proposed→shadow→active→archived 的迁移条件)",
	"max_fail_rate":      "冒烟失败率上限锁定",
	"min_coverage":       "冒烟奖励覆盖率下限锁定",
	"min_tiers":          "多档冒烟的最小档位数锁定 —— 单档冒烟不是 H6",
	"authority_baseline": "不越权基线锁定 (§4.6 四律之不越权)",
}

// safeFields agent 可改的安全字段 (白名单, 与 lockedFields 互补)。
var safeFields = map[string]bool{
	"samples":      true, // 实验样本量目标
	"env":          true, // 任务集选择
	"shadow_ratio": true, // shadow 注入比例 (上限 0.5, 见 clampShadowRatio)
	"description":  true,
	"note":         true,
}

// maxShadowRatio design/03 §4.7 明写的 shadow 比例上限。
const maxShadowRatio = 0.5

// evoStatusMinInterval H9: 同一实验的状态查询最小间隔。
const evoStatusMinInterval = 30 * time.Minute

// ---------------------------------------------------------------------------
// 多档冒烟的候选构造器注入点
// ---------------------------------------------------------------------------

// EvoTierFactory 按产物构造多档冒烟候选。宿主装配时注入。
//
// 为什么必须由宿主给: 只有宿主知道"primary/fallback/local 各是哪个 provider+model",
// 以及怎么把被测产物 (技能正文 / prompt 版本) 注进一次真实运行。本包不猜, 也不
// 拿主模型凑一个假的第二档 —— 那会让 H6 的多档要求形同虚设。
type EvoTierFactory func(ctx context.Context, product string, body string) []replay.Tier

var evoTierFactory EvoTierFactory

// SetEvoTierFactory 注入多档冒烟候选构造器 (幂等, 后者覆盖前者)。
func SetEvoTierFactory(f EvoTierFactory) { evoTierFactory = f }

// ---------------------------------------------------------------------------
// 公共辅助
// ---------------------------------------------------------------------------

// resolveStateDir 统一解析状态目录。tctx.Cwd 优先 (工具运行在某个项目下),
// 与 `claude-go evo` 的 basedir.ResolveDefault 同一套规则。
func resolveStateDir(tctx *tool.ToolContext) string {
	cwd := ""
	if tctx != nil {
		cwd = tctx.Cwd
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return basedir.ResolveDefault("", cwd)
}

func okResult(v any) (*tool.ToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("序列化失败: %v", err), IsError: true}, nil
	}
	return &tool.ToolResult{Content: string(data)}, nil
}

func errResult(format string, args ...any) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}, nil
}

// evoToolBase 七个工具共享的样板 (只读、并发安全、无需权限确认)。
//
// 为什么全部标只读: 它们要么真只读 (list/inspect/status), 要么写的是 <state>/evolution
// 下的治理产物且**每一处写都自带闸** (propose 只产 proposed 态; promote 过不越权检查)。
// 标成需确认会让 agent 每一步都卡住人, 那 H8 的"全自助"就不成立; 而放行的安全性由
// 闸门而不是权限提示来保证 —— 这正是把治理机械化的意义。
type evoToolBase struct{}

func (evoToolBase) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (evoToolBase) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// ---------------------------------------------------------------------------
// ① evo_list_envs
// ---------------------------------------------------------------------------

// EvoListEnvsTool 列出 EvalEnv (回放任务集 + 验证器种类)。
type EvoListEnvsTool struct{ evoToolBase }

func NewEvoListEnvsTool() *EvoListEnvsTool { return &EvoListEnvsTool{} }

func (t *EvoListEnvsTool) Name() string { return "evo_list_envs" }
func (t *EvoListEnvsTool) Description() string {
	return "列出可用的进化评测环境 EvalEnv (离线回放任务集 + 验证器), 含任务数与验证器种类。" +
		"任务集放在 <state>/evolution/evalenv/*.jsonl, 每行一个 {id,objective,input,expect,gate} 任务。" +
		"进化实验的第一步: 先看有哪些环境可用, 再决定用哪个做冒烟/回放。"
}
func (t *EvoListEnvsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}
func (t *EvoListEnvsTool) IsReadOnly(_ json.RawMessage) bool { return true }

// EvalEnvInfo 一个任务集的概览。
type EvalEnvInfo struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Tasks         int    `json:"tasks"`
	WithExpect    int    `json:"with_expect"` // 有确定性断言的任务数
	WithGate      int    `json:"with_gate"`   // 有真门禁命令的任务数
	VerifierKinds string `json:"verifier_kinds"`
}

func (t *EvoListEnvsTool) Call(_ context.Context, _ json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	stateDir := resolveStateDir(tctx)
	dir := filepath.Join(stateDir, "evolution", "evalenv")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return okResult(map[string]any{
			"state_dir": stateDir,
			"envs":      []EvalEnvInfo{},
			"note": "任务集目录不存在: " + dir +
				" —— EvalEnv 需要先沉淀 (从历史高置信轨迹导出), 见 design/03 §4.5。没有任务集时冒烟与回放都无从进行。",
		})
	}
	var envs []EvalEnvInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		tasks, err := replay.LoadTasks(p)
		info := EvalEnvInfo{Name: strings.TrimSuffix(e.Name(), ".jsonl"), Path: p}
		if err != nil {
			info.VerifierKinds = "解析失败: " + err.Error()
			envs = append(envs, info)
			continue
		}
		info.Tasks = len(tasks)
		for _, tk := range tasks {
			if len(tk.Expect) > 0 {
				info.WithExpect++
			}
			if tk.Gate != "" {
				info.WithGate++
			}
		}
		var kinds []string
		if info.WithExpect > 0 {
			kinds = append(kinds, "确定性断言")
		}
		if info.WithGate > 0 {
			kinds = append(kinds, "真门禁命令")
		}
		if len(kinds) == 0 {
			kinds = append(kinds, "无确定性验证器 (仅靠 judge 打分)")
		}
		info.VerifierKinds = strings.Join(kinds, "+")
		envs = append(envs, info)
	}
	sort.Slice(envs, func(i, j int) bool { return envs[i].Name < envs[j].Name })
	return okResult(map[string]any{"state_dir": stateDir, "envs": envs})
}

// ---------------------------------------------------------------------------
// ② evo_inspect
// ---------------------------------------------------------------------------

// EvoInspectTool 查看进化产物的谱系/状态/证据, 以及学习闭环健康度。
type EvoInspectTool struct{ evoToolBase }

func NewEvoInspectTool() *EvoInspectTool { return &EvoInspectTool{} }

func (t *EvoInspectTool) Name() string { return "evo_inspect" }
func (t *EvoInspectTool) Description() string {
	return "查看进化产物与学习闭环状态。不带参数=闭环健康度总览 (轨迹/奖励/经验/shadow 技能);" +
		"带 id=看某份草案的谱系(父版本)、归纳依据(样本量/奖励均值/参与的 run)与当前状态。" +
		"晋升任何东西之前应当先 inspect 它的证据。"
}
func (t *EvoInspectTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"id":{"type":"string","description":"草案 ID (如 wf-xxxx / pr-xxxx); 省略则给闭环健康度总览"}
},"additionalProperties":false}`)
}
func (t *EvoInspectTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *EvoInspectTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(input, &in)
	stateDir := resolveStateDir(tctx)

	if strings.TrimSpace(in.ID) != "" {
		p, err := learners.GetProposal(stateDir, in.ID)
		if err != nil {
			return errResult("%v (可先用 evo_inspect 不带参数看总览, 或 evo_propose 产生草案)", err)
		}
		return okResult(p)
	}
	rep, err := console.Inspect(stateDir)
	if err != nil {
		return errResult("检视失败: %v", err)
	}
	props := learners.ListProposals(stateDir)
	byStatus := map[string]int{}
	ids := make([]string, 0, len(props))
	for _, p := range props {
		byStatus[p.Status]++
		ids = append(ids, p.ID)
	}
	return okResult(map[string]any{
		"loop_health":        rep.LoopHealth,
		"trace_runs":         rep.TraceRuns,
		"trace_spans":        rep.TraceSpans,
		"rewards":            rep.Rewards,
		"reward_by_source":   rep.RewardBySource,
		"reward_mean":        rep.RewardMean,
		"experiences":        rep.Experiences,
		"trajectories":       rep.Trajectories,
		"shadow_skills":      rep.ShadowSkills,
		"proposals_total":    len(props),
		"proposals_by_state": byStatus,
		"proposal_ids":       ids,
		"notes":              rep.Notes,
	})
}

// ---------------------------------------------------------------------------
// ③ evo_propose
// ---------------------------------------------------------------------------

// EvoProposeTool 提交候选产物进 proposed 态 / 触发确定性归纳。
type EvoProposeTool struct{ evoToolBase }

func NewEvoProposeTool() *EvoProposeTool { return &EvoProposeTool{} }

func (t *EvoProposeTool) Name() string { return "evo_propose" }
func (t *EvoProposeTool) Description() string {
	return "提交进化候选。kind=induce: 立即跑一次确定性工作流归纳 (AWM, 零 LLM 成本), " +
		"从高奖励的真实运行里归纳可复用阶段序列; kind=prompt: 提交一份 prompt 候选正文。" +
		"产物一律落 proposed 态, 不影响运行期行为, 必须经 evo_smoke + evo_promote 才生效。" +
		"注意: 治理参数 (晋升阈值/uplift 标准/judge 模型/预算/生命周期) 是锁定字段, 请求修改会被拒绝。"
}
func (t *EvoProposeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"kind":{"type":"string","enum":["induce","prompt"],"description":"induce=确定性工作流归纳; prompt=提交 prompt 候选"},
"target":{"type":"string","description":"kind=prompt 必填, 形如 workflow/stage"},
"body":{"type":"string","description":"kind=prompt 必填, 候选 prompt 正文"},
"description":{"type":"string","description":"可选说明 (安全字段)"},
"config":{"type":"object","description":"可选参数覆盖; 只接受安全字段 (samples/env/shadow_ratio/description/note), 治理参数会被显式拒绝"}
},"required":["kind"],"additionalProperties":false}`)
}
func (t *EvoProposeTool) IsReadOnly(_ json.RawMessage) bool { return false }

func (t *EvoProposeTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Kind        string          `json:"kind"`
		Target      string          `json:"target"`
		Body        string          `json:"body"`
		Description string          `json:"description"`
		Config      json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("参数解析失败: %v", err)
	}
	// H7 锁定字段: 先查再干活 —— 请求里带了锁定字段就整体拒绝, 不做"忽略该字段但照常执行"。
	// 部分执行会让 agent 以为改动生效了, 那比明确失败更糟。
	if rej := rejectLockedFields(in.Config); rej != "" {
		return errResult("%s", rej)
	}
	stateDir := resolveStateDir(tctx)

	switch strings.ToLower(strings.TrimSpace(in.Kind)) {
	case "induce":
		res, err := learners.InduceWorkflows(stateDir)
		if err != nil {
			return errResult("归纳失败: %v", err)
		}
		return okResult(res)
	case "prompt":
		if strings.TrimSpace(in.Target) == "" || strings.TrimSpace(in.Body) == "" {
			return errResult("kind=prompt 需要 target 与 body")
		}
		p, err := learners.ProposePrompt(stateDir, in.Target, in.Body, in.Description)
		if err != nil {
			return errResult("提交失败: %v", err)
		}
		return okResult(p)
	default:
		return errResult("未知 kind %q (仅 induce / prompt)", in.Kind)
	}
}

// rejectLockedFields 检查 config 里是否含锁定字段; 返回非空即拒绝理由 (H7)。
func rejectLockedFields(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		// 解析不了就拒绝: 治理路径上"看不懂的输入"必须当作不合法 (fail-closed)。
		return "config 无法解析为对象, 拒绝 (治理路径不接受看不懂的输入)"
	}
	var locked, unknown []string
	for k := range m {
		key := strings.ToLower(strings.TrimSpace(k))
		if reason, ok := lockedFields[key]; ok {
			locked = append(locked, fmt.Sprintf("%s (%s)", k, reason))
			continue
		}
		if !safeFields[key] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(locked)
	sort.Strings(unknown)
	if len(locked) > 0 {
		return "拒绝: 请求修改锁定的治理参数 —— " + strings.Join(locked, "; ") +
			"。可改的安全字段只有: samples / env / shadow_ratio / description / note"
	}
	if len(unknown) > 0 {
		return "拒绝: config 含未知字段 " + strings.Join(unknown, ",") +
			" (治理路径只接受白名单字段, 不认识的一律拒绝)"
	}
	return ""
}

// ---------------------------------------------------------------------------
// ④ evo_smoke
// ---------------------------------------------------------------------------

// EvoSmokeTool 晋升前多档冒烟 (H6)。
type EvoSmokeTool struct{ evoToolBase }

func NewEvoSmokeTool() *EvoSmokeTool { return &EvoSmokeTool{} }

func (t *EvoSmokeTool) Name() string { return "evo_smoke" }
func (t *EvoSmokeTool) Description() string {
	return "晋升前多档冒烟: 用少量任务 × 多次采样 × 多个模型档位验证候选产物的解析鲁棒性与奖励覆盖。" +
		"这是 test-before-promote —— 技能/prompt 对弱模型不鲁棒是线上劣化的常见来源, 只在主模型上验过就晋升等于把风险留给 fallback 生效那一刻。" +
		"必须先 evo_smoke 通过再 evo_promote。只有一个档位可跑时会明确拒绝 (单档冒烟不是多档冒烟)。"
}
func (t *EvoSmokeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"id":{"type":"string","description":"待冒烟的草案 ID"},
"env":{"type":"string","description":"EvalEnv 名 (见 evo_list_envs); 省略则用第一个可用任务集"},
"samples":{"type":"integer","description":"每任务每档采样次数, 1-8 (安全字段)"}
},"required":["id"],"additionalProperties":false}`)
}
func (t *EvoSmokeTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *EvoSmokeTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		ID      string `json:"id"`
		Env     string `json:"env"`
		Samples int    `json:"samples"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("参数解析失败: %v", err)
	}
	stateDir := resolveStateDir(tctx)
	p, err := learners.GetProposal(stateDir, in.ID)
	if err != nil {
		return errResult("%v", err)
	}
	tasks, envName, err := loadEnvTasks(stateDir, in.Env)
	if err != nil {
		return errResult("%v", err)
	}
	if in.Samples < 0 || in.Samples > 8 {
		return errResult("samples 需在 1-8 之间 (收到 %d)", in.Samples)
	}

	// 档位组装: 内置一个确定性的"装配档", 再加宿主注入的真实模型档。
	tiers := []replay.Tier{{
		Name:      "assembly",
		Model:     "deterministic",
		Candidate: assemblyCandidate{body: p.Body},
	}}
	if evoTierFactory != nil {
		tiers = append(tiers, evoTierFactory(ctx, p.ID, p.Body)...)
	}
	rep, err := replay.Smoke(ctx, p.ID, tasks, tiers, nil, replay.SmokeConfig{
		Samples: in.Samples,
		OutDir:  filepath.Join(stateDir, "evolution", "smoke"),
	})
	if err != nil {
		return errResult("冒烟执行失败: %v", err)
	}
	out := map[string]any{"env": envName, "report": rep, "text": rep.Format()}
	if evoTierFactory == nil {
		out["note"] = "本环境未注入真实模型档位构造器 (SetEvoTierFactory), 只跑了确定性装配档 —— " +
			"按 H6 这不足以放行, 报告里的 blocking 已如实反映。"
	}
	return okResult(out)
}

// assemblyCandidate 确定性"装配档"候选。
//
// 它不调 LLM, 只做一件事: 把候选正文按任务 objective 做占位符替换后输出。这足以抓住
// 两类真实劣化 —— 候选丢了 {objective} 之类的锚点 (上线即静默失效), 以及候选里留了
// 未定义的占位符。它**不是**模型档位, 所以不满足 H6 的多档要求, 由 MinTiers 拒绝兜住。
type assemblyCandidate struct{ body string }

func (assemblyCandidate) Name() string { return "assembly" }
func (c assemblyCandidate) Produce(_ context.Context, t replay.Task) (string, error) {
	if strings.TrimSpace(c.body) == "" {
		return "", fmt.Errorf("草案无正文可装配 (workflow 类草案没有 prompt 正文, 用真实模型档位冒烟)")
	}
	out := strings.ReplaceAll(c.body, "{objective}", t.Objective)
	out = strings.ReplaceAll(out, "{prev_result}", t.Input)
	out = strings.ReplaceAll(out, "{user_feedback}", "")
	if i := strings.IndexByte(out, '{'); i >= 0 {
		if j := strings.IndexByte(out[i:], '}'); j > 1 {
			return out, fmt.Errorf("装配后仍残留未解析占位符 %s", out[i:i+j+1])
		}
	}
	return out, nil
}

// loadEnvTasks 载入指定 (或第一个可用) 任务集。
func loadEnvTasks(stateDir, name string) ([]replay.Task, string, error) {
	dir := filepath.Join(stateDir, "evolution", "evalenv")
	if strings.TrimSpace(name) != "" {
		// 只取 basename: name 来自 agent 入参, 直接拼路径会被 ../.. 穿出去。
		base := filepath.Base(strings.TrimSuffix(name, ".jsonl")) + ".jsonl"
		tasks, err := replay.LoadTasks(filepath.Join(dir, base))
		if err != nil {
			return nil, "", fmt.Errorf("任务集 %q 载入失败: %w", name, err)
		}
		return tasks, base, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", fmt.Errorf("任务集目录不存在 (%s): 先沉淀 EvalEnv 再冒烟, 见 design/03 §4.5", dir)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, "", fmt.Errorf("没有任何 EvalEnv 任务集 (%s 为空)", dir)
	}
	tasks, err := replay.LoadTasks(filepath.Join(dir, names[0]))
	if err != nil {
		return nil, "", fmt.Errorf("任务集 %q 载入失败: %w", names[0], err)
	}
	return tasks, names[0], nil
}

// ---------------------------------------------------------------------------
// ⑤ evo_run_experiment
// ---------------------------------------------------------------------------

// EvoRunExperimentTool 开一个 shadow 配对实验。
type EvoRunExperimentTool struct{ evoToolBase }

func NewEvoRunExperimentTool() *EvoRunExperimentTool { return &EvoRunExperimentTool{} }

func (t *EvoRunExperimentTool) Name() string { return "evo_run_experiment" }
func (t *EvoRunExperimentTool) Description() string {
	return "启动 shadow 配对实验: 把草案置 shadow 态并登记实验 (基线时刻/样本目标/shadow 比例), " +
		"此后真实运行产生的奖励会被 evo_status 用来算 uplift。shadow 比例上限 50%。" +
		"实验不会自动晋升任何东西 —— 晋升永远是 evo_promote 的显式动作, 且要过不越权闸。"
}
func (t *EvoRunExperimentTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"id":{"type":"string","description":"草案 ID"},
"samples":{"type":"integer","description":"样本量目标 (安全字段), 默认 10"},
"shadow_ratio":{"type":"number","description":"shadow 注入比例 (安全字段), 上限 0.5"},
"note":{"type":"string"}
},"required":["id"],"additionalProperties":false}`)
}
func (t *EvoRunExperimentTool) IsReadOnly(_ json.RawMessage) bool { return false }

// Experiment 一次 shadow 配对实验的登记。
type Experiment struct {
	ID          string  `json:"id"`
	Proposal    string  `json:"proposal"`
	StartedAtMS int64   `json:"started_at_ms"` // 基线切分点: 之前的奖励算基线, 之后算实验组
	SamplesWant int     `json:"samples_want"`
	ShadowRatio float64 `json:"shadow_ratio"`
	Note        string  `json:"note,omitempty"`
	// LastCheckedMS H9 限速: 上次 evo_status 的时刻, 持久化以便进程重启不重置限速。
	LastCheckedMS int64 `json:"last_checked_ms,omitempty"`
}

func (t *EvoRunExperimentTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		ID          string  `json:"id"`
		Samples     int     `json:"samples"`
		ShadowRatio float64 `json:"shadow_ratio"`
		Note        string  `json:"note"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("参数解析失败: %v", err)
	}
	stateDir := resolveStateDir(tctx)
	p, err := learners.GetProposal(stateDir, in.ID)
	if err != nil {
		return errResult("%v", err)
	}
	if in.Samples <= 0 {
		in.Samples = 10
	}
	ratio, clamped := clampShadowRatio(in.ShadowRatio)

	if err := learners.SetProposalStatus(stateDir, p.ID, "shadow"); err != nil {
		return errResult("置 shadow 失败: %v", err)
	}
	exp := Experiment{
		ID:          "exp-" + p.ID,
		Proposal:    p.ID,
		StartedAtMS: time.Now().UnixMilli(),
		SamplesWant: in.Samples,
		ShadowRatio: ratio,
		Note:        in.Note,
	}
	if err := saveExperiment(stateDir, exp); err != nil {
		return errResult("登记实验失败: %v", err)
	}
	out := map[string]any{"experiment": exp, "proposal_status": "shadow"}
	if clamped {
		out["shadow_ratio_clamped"] = fmt.Sprintf("请求的 shadow 比例被压到上限 %.0f%% (design/03 §4.7 护栏)", maxShadowRatio*100)
	}
	return okResult(out)
}

func clampShadowRatio(r float64) (float64, bool) {
	if r <= 0 {
		return maxShadowRatio, false // 未指定: 用上限 (配对实验要 50/50 才最省样本)
	}
	if r > maxShadowRatio {
		return maxShadowRatio, true
	}
	return r, false
}

func experimentsDir(stateDir string) string {
	return filepath.Join(stateDir, "evolution", "experiments")
}

func saveExperiment(stateDir string, e Experiment) error {
	dir := experimentsDir(stateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, filepath.Base(e.ID)+".json"), data, 0o644)
}

func loadExperiment(stateDir, id string) (*Experiment, error) {
	// 只取 basename, 防路径穿越 (id 来自 agent 入参)。
	path := filepath.Join(experimentsDir(stateDir), filepath.Base(id)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("实验 %q 不存在 (先用 evo_run_experiment 启动)", id)
	}
	var e Experiment
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("实验 %q 记录损坏: %w", id, err)
	}
	return &e, nil
}

// ---------------------------------------------------------------------------
// ⑥ evo_status (H9 限速)
// ---------------------------------------------------------------------------

// EvoStatusTool 查实验进度与指标, 内建 30 分钟限速。
type EvoStatusTool struct{ evoToolBase }

func NewEvoStatusTool() *EvoStatusTool { return &EvoStatusTool{} }

func (t *EvoStatusTool) Name() string { return "evo_status" }
func (t *EvoStatusTool) Description() string {
	return "查 shadow 实验的进度与 uplift。**同一实验 30 分钟内只答一次**, 过于频繁的查询会返回 rate_limited 与剩余秒数 —— " +
		"实验要靠真实运行攒样本, 分钟级轮询不会让它变快, 只会烧 token。指标坏了应当早停 (evo_rollback) 而不是继续等。"
}
func (t *EvoStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"id":{"type":"string","description":"实验 ID (形如 exp-wf-xxxx)"}
},"required":["id"],"additionalProperties":false}`)
}
func (t *EvoStatusTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *EvoStatusTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("参数解析失败: %v", err)
	}
	stateDir := resolveStateDir(tctx)
	exp, err := loadExperiment(stateDir, in.ID)
	if err != nil {
		return errResult("%v", err)
	}

	// H9 限速: 把"别轮询"这条纪律做进工具而不是提示词。
	now := time.Now()
	if exp.LastCheckedMS > 0 {
		elapsed := now.Sub(time.UnixMilli(exp.LastCheckedMS))
		if elapsed < evoStatusMinInterval {
			remain := int((evoStatusMinInterval - elapsed).Seconds())
			return okResult(map[string]any{
				"rate_limited":    true,
				"retry_after_sec": remain,
				"min_interval":    evoStatusMinInterval.String(),
				"hint": "同一实验 30 分钟内只答一次。实验靠真实运行攒样本, 轮询不会加快它; " +
					"这段时间应当去做别的事, 或者用 evo_inspect 看闭环总览 (不限速)。",
			})
		}
	}
	exp.LastCheckedMS = now.UnixMilli()
	_ = saveExperiment(stateDir, *exp) // 限速时间戳落盘失败不影响本次回答

	// 真实 uplift: 以实验开始时刻切分 rewards.jsonl, 前后各算加权均值。
	rows := learners.LoadRewards(filepath.Join(stateDir, "evolution", "rewards.jsonl"))
	base, cand := learners.SplitByTime(rows, exp.StartedAtMS)
	baseScore, baseN := learners.MeanRunScore(base)
	candScore, candN := learners.MeanRunScore(cand)
	status := "collecting"
	if candN >= exp.SamplesWant {
		status = "ready"
	}
	return okResult(map[string]any{
		"experiment":      exp,
		"status":          status,
		"baseline_runs":   baseN,
		"baseline_score":  baseScore,
		"candidate_runs":  candN,
		"candidate_score": candScore,
		"uplift":          candScore - baseScore,
		"samples_want":    exp.SamplesWant,
		"hint": "uplift 为正且 candidate_runs 达到 samples_want 才考虑 evo_promote; " +
			"uplift 明显为负应当 evo_rollback 早停。晋升阈值本身是锁定参数, 不可调。",
	})
}

// ---------------------------------------------------------------------------
// ⑦ evo_promote / evo_rollback
// ---------------------------------------------------------------------------

// EvoPromoteTool 过闸晋升。
type EvoPromoteTool struct{ evoToolBase }

func NewEvoPromoteTool() *EvoPromoteTool { return &EvoPromoteTool{} }

func (t *EvoPromoteTool) Name() string { return "evo_promote" }
func (t *EvoPromoteTool) Description() string {
	return "晋升进化产物到 active。target=skill 时晋升某个 shadow 技能, target=proposal 时晋升某份草案。" +
		"晋升**必过不越权闸** (design/03 §4.6 四律): 声明了写/执行类工具的自动产物、frontmatter 解析失败的技能一律拒绝。" +
		"晋升阈值与判据是锁定参数, 不接受覆盖。"
}
func (t *EvoPromoteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"target":{"type":"string","enum":["skill","proposal"],"description":"晋升对象类型"},
"name":{"type":"string","description":"技能名 或 草案 ID"}
},"required":["target","name"],"additionalProperties":false}`)
}
func (t *EvoPromoteTool) IsReadOnly(_ json.RawMessage) bool { return false }

func (t *EvoPromoteTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	target, name, stateDir, errRes := parsePromoteInput(input, tctx)
	if errRes != nil {
		return errRes, nil
	}
	switch target {
	case "skill":
		p := filepath.Join(stateDir, "skills", filepath.Base(name), "SKILL.md")
		// 不越权闸先跑一遍并把理由带回给 agent —— SetStatus 内部也会拦, 但那里只给
		// 一句 error; agent 需要知道具体越了哪一项才可能自己改对。
		if v := govern.CheckSkillPromotion(p, nil); !v.Allowed {
			return errResult("晋升被不越权闸拒绝: %s", strings.Join(v.Reasons, "; "))
		}
		if err := skillaudit.SetStatus(p, "active"); err != nil {
			return errResult("晋升失败: %v", err)
		}
		return okResult(map[string]any{"skill": name, "status": "active", "gate": "no_escalation_passed"})
	case "proposal":
		if err := learners.SetProposalStatus(stateDir, name, "active"); err != nil {
			return errResult("晋升失败: %v", err)
		}
		return okResult(map[string]any{"proposal": name, "status": "active"})
	default:
		return errResult("未知 target %q (仅 skill / proposal)", target)
	}
}

// EvoRollbackTool 一键回滚。
type EvoRollbackTool struct{ evoToolBase }

func NewEvoRollbackTool() *EvoRollbackTool { return &EvoRollbackTool{} }

func (t *EvoRollbackTool) Name() string { return "evo_rollback" }
func (t *EvoRollbackTool) Description() string {
	return "回滚进化产物: 技能退回 shadow (或 archived 退役), 草案退回 proposed。" +
		"指标变坏时应当立刻回滚早停, 而不是等实验跑完。回滚方向永远不需要过闸 —— 收紧权限面不是提权。"
}
func (t *EvoRollbackTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"target":{"type":"string","enum":["skill","proposal"]},
"name":{"type":"string","description":"技能名 或 草案 ID"},
"retire":{"type":"boolean","description":"true=直接退役 (technical: archived), 默认只退回 shadow/proposed"}
},"required":["target","name"],"additionalProperties":false}`)
}
func (t *EvoRollbackTool) IsReadOnly(_ json.RawMessage) bool { return false }

func (t *EvoRollbackTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Target string `json:"target"`
		Name   string `json:"name"`
		Retire bool   `json:"retire"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("参数解析失败: %v", err)
	}
	stateDir := resolveStateDir(tctx)
	switch strings.ToLower(strings.TrimSpace(in.Target)) {
	case "skill":
		to := "shadow"
		if in.Retire {
			to = "archived"
		}
		p := filepath.Join(stateDir, "skills", filepath.Base(in.Name), "SKILL.md")
		if err := skillaudit.SetStatus(p, to); err != nil {
			return errResult("回滚失败: %v", err)
		}
		return okResult(map[string]any{"skill": in.Name, "status": to})
	case "proposal":
		to := "proposed"
		if in.Retire {
			to = "archived"
		}
		if err := learners.SetProposalStatus(stateDir, in.Name, to); err != nil {
			return errResult("回滚失败: %v", err)
		}
		return okResult(map[string]any{"proposal": in.Name, "status": to})
	default:
		return errResult("未知 target %q (仅 skill / proposal)", in.Target)
	}
}

func parsePromoteInput(input json.RawMessage, tctx *tool.ToolContext) (target, name, stateDir string, errRes *tool.ToolResult) {
	var in struct {
		Target string `json:"target"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", "", "", &tool.ToolResult{Content: fmt.Sprintf("参数解析失败: %v", err), IsError: true}
	}
	if strings.TrimSpace(in.Name) == "" {
		return "", "", "", &tool.ToolResult{Content: "name 不能为空", IsError: true}
	}
	return strings.ToLower(strings.TrimSpace(in.Target)), in.Name, resolveStateDir(tctx), nil
}

// RegisterEvolutionTools 注册 evo_* 七件套 (design/03 §4.7)。
//
// 七个工具对应设计表格的七行 (promote/rollback 是同一行的两个方向)。
func RegisterEvolutionTools(reg *tool.Registry) {
	reg.Register(NewEvoListEnvsTool())
	reg.Register(NewEvoInspectTool())
	reg.Register(NewEvoProposeTool())
	reg.Register(NewEvoSmokeTool())
	reg.Register(NewEvoRunExperimentTool())
	reg.Register(NewEvoStatusTool())
	reg.Register(NewEvoPromoteTool())
	reg.Register(NewEvoRollbackTool())
}
