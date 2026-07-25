package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

// evoCtx 造一个把 stateDir 落在临时目录里的 ToolContext。
//
// resolveStateDir 走 basedir.ResolveDefault(cwd) —— 项目下有 .claude 就用它。
// 这里预建 .claude 目录, 于是 stateDir 可预测且不碰用户真实状态目录。
func evoCtx(t *testing.T) (*tool.ToolContext, string) {
	t.Helper()
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	tctx := &tool.ToolContext{Cwd: cwd}
	return tctx, resolveStateDir(tctx)
}

func callTool(t *testing.T, tl tool.Tool, in string, tctx *tool.ToolContext) *tool.ToolResult {
	t.Helper()
	res, err := tl.Call(context.Background(), json.RawMessage(in), tctx)
	if err != nil {
		t.Fatalf("%s 返回 error (工具约定是把失败放进 ToolResult.IsError): %v", tl.Name(), err)
	}
	if res == nil {
		t.Fatalf("%s 返回 nil result", tl.Name())
	}
	return res
}

// --- 注册与契约 ---

// 设计要求的是"注册进工具池、agent 自助"的 evo_* 七件套 (promote/rollback 同一行两个方向)。
func TestRegisterEvolutionTools_八个工具全注册(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterEvolutionTools(reg)
	want := []string{
		"evo_list_envs", "evo_inspect", "evo_propose", "evo_smoke",
		"evo_run_experiment", "evo_status", "evo_promote", "evo_rollback",
	}
	for _, n := range want {
		if _, ok := reg.Get(n); !ok {
			t.Errorf("工具 %s 未注册 —— agent 拿不到就等于只有 CLI", n)
		}
	}
}

// 基础工具集必须包含 evo_*: 这是"通电"的判据 (CLI 的 buildEngine 走 RegisterBaseTools)。
func TestRegisterBaseTools_包含evo工具(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterBaseTools(reg, nil)
	if _, ok := reg.Get("evo_propose"); !ok {
		t.Error("RegisterBaseTools 应包含 evo_* —— 否则 CLI/admin 档位的 agent 调不到")
	}
}

// InputSchema 必须是合法 JSON 且 additionalProperties:false (防模型自作主张塞参数)。
func TestEvoTools_schema合法且禁额外字段(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterEvolutionTools(reg)
	for _, name := range []string{"evo_list_envs", "evo_inspect", "evo_propose", "evo_smoke",
		"evo_run_experiment", "evo_status", "evo_promote", "evo_rollback"} {
		tl, _ := reg.Get(name)
		var schema map[string]any
		if err := json.Unmarshal(tl.InputSchema(), &schema); err != nil {
			t.Errorf("%s 的 schema 不是合法 JSON: %v", name, err)
			continue
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%s 应设 additionalProperties:false", name)
		}
		if strings.TrimSpace(tl.Description()) == "" {
			t.Errorf("%s 缺描述 (agent 靠它决定何时用)", name)
		}
	}
}

// --- H7 锁定字段护栏 (本文件最重要的一组) ---

func TestEvoPropose_锁定字段被显式拒绝(t *testing.T) {
	tctx, _ := evoCtx(t)
	tl := NewEvoProposeTool()
	for field := range lockedFields {
		in := `{"kind":"induce","config":{"` + field + `":1}}`
		res := callTool(t, tl, in, tctx)
		if !res.IsError {
			t.Errorf("请求改锁定字段 %s 应被拒绝, 却成功了", field)
			continue
		}
		if !strings.Contains(res.Content, "锁定") {
			t.Errorf("字段 %s 的拒绝理由应说明它被锁定, got %q", field, res.Content)
		}
	}
}

// 拒绝必须是整体拒绝, 不能"忽略该字段但照常执行" —— 部分执行会让 agent 以为改动生效。
func TestEvoPropose_锁定字段拒绝时不执行归纳(t *testing.T) {
	tctx, stateDir := evoCtx(t)
	res := callTool(t, NewEvoProposeTool(),
		`{"kind":"induce","config":{"promote_threshold":0.01}}`, tctx)
	if !res.IsError {
		t.Fatal("应拒绝")
	}
	// 归纳若执行过会建出 proposals 目录 (哪怕没产草案也会走 MkdirAll)。
	if _, err := os.Stat(filepath.Join(stateDir, "evolution", "proposals")); err == nil {
		t.Error("拒绝后不该执行归纳 (proposals 目录被建出来了)")
	}
}

func TestEvoPropose_未知config字段被拒(t *testing.T) {
	tctx, _ := evoCtx(t)
	res := callTool(t, NewEvoProposeTool(), `{"kind":"induce","config":{"随便写的字段":1}}`, tctx)
	if !res.IsError || !strings.Contains(res.Content, "未知字段") {
		t.Errorf("未知 config 字段应被拒 (白名单 fail-closed), got %q", res.Content)
	}
}

func TestEvoPropose_安全字段放行(t *testing.T) {
	tctx, _ := evoCtx(t)
	res := callTool(t, NewEvoProposeTool(),
		`{"kind":"induce","config":{"samples":5,"description":"试一下"}}`, tctx)
	if res.IsError {
		t.Errorf("安全字段应放行, got %q", res.Content)
	}
}

func TestRejectLockedFields_无法解析的config也拒绝(t *testing.T) {
	if rejectLockedFields(json.RawMessage(`"不是对象"`)) == "" {
		t.Error("治理路径不该接受看不懂的输入 (fail-closed)")
	}
	if rejectLockedFields(nil) != "" {
		t.Error("未提供 config 不该被拒")
	}
}

// --- evo_propose prompt 路径 ---

func TestEvoPropose_prompt候选落proposed态(t *testing.T) {
	tctx, stateDir := evoCtx(t)
	res := callTool(t, NewEvoProposeTool(),
		`{"kind":"prompt","target":"development/implement","body":"按 {objective} 实现, 必须先跑通编译。"}`, tctx)
	if res.IsError {
		t.Fatalf("合法 prompt 候选应被接受: %q", res.Content)
	}
	var p struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Content), &p); err != nil {
		t.Fatalf("返回体应是草案 JSON: %v", err)
	}
	if p.Status != "proposed" {
		t.Errorf("新候选必须是 proposed 态 (未过闸), got %q", p.Status)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "evolution", "proposals", p.ID+".json")); err != nil {
		t.Errorf("草案应落盘: %v", err)
	}
}

func TestEvoPropose_缺参数与未知kind(t *testing.T) {
	tctx, _ := evoCtx(t)
	tl := NewEvoProposeTool()
	if res := callTool(t, tl, `{"kind":"prompt"}`, tctx); !res.IsError {
		t.Error("kind=prompt 缺 target/body 应报错")
	}
	if res := callTool(t, tl, `{"kind":"train"}`, tctx); !res.IsError {
		t.Error("未知 kind 应报错")
	}
}

// --- shadow 比例护栏 ---

func TestClampShadowRatio_上限50(t *testing.T) {
	if got, clamped := clampShadowRatio(0.9); got != maxShadowRatio || !clamped {
		t.Errorf("超限应被压到 %.2f 并标记, got %.2f clamped=%v", maxShadowRatio, got, clamped)
	}
	if got, clamped := clampShadowRatio(0.3); got != 0.3 || clamped {
		t.Errorf("合法值应原样通过, got %.2f clamped=%v", got, clamped)
	}
	if got, _ := clampShadowRatio(0); got != maxShadowRatio {
		t.Errorf("未指定应用上限 (配对实验 50/50 最省样本), got %.2f", got)
	}
}

func TestEvoRunExperiment_钳制并置shadow(t *testing.T) {
	tctx, stateDir := evoCtx(t)
	prop := callTool(t, NewEvoProposeTool(),
		`{"kind":"prompt","target":"a/b","body":"用 {objective} 干活"}`, tctx)
	var p struct{ ID string }
	_ = json.Unmarshal([]byte(prop.Content), &p)

	res := callTool(t, NewEvoRunExperimentTool(),
		`{"id":"`+p.ID+`","shadow_ratio":0.95,"samples":7}`, tctx)
	if res.IsError {
		t.Fatalf("启动实验失败: %q", res.Content)
	}
	if !strings.Contains(res.Content, "被压到上限") {
		t.Errorf("超限的 shadow 比例应被明确告知已钳制, got %q", res.Content)
	}
	var out struct {
		Experiment Experiment `json:"experiment"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if out.Experiment.ShadowRatio != maxShadowRatio || out.Experiment.SamplesWant != 7 {
		t.Errorf("实验参数不对: %+v", out.Experiment)
	}
	// 草案应已置 shadow
	data, err := os.ReadFile(filepath.Join(stateDir, "evolution", "proposals", p.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"status": "shadow"`) {
		t.Errorf("实验开始后草案应置 shadow: %s", data)
	}
}

// --- H9 限速 ---

func TestEvoStatus_同实验30分钟内限速(t *testing.T) {
	tctx, _ := evoCtx(t)
	prop := callTool(t, NewEvoProposeTool(),
		`{"kind":"prompt","target":"a/b","body":"用 {objective} 干活"}`, tctx)
	var p struct{ ID string }
	_ = json.Unmarshal([]byte(prop.Content), &p)
	_ = callTool(t, NewEvoRunExperimentTool(), `{"id":"`+p.ID+`"}`, tctx)

	st := NewEvoStatusTool()
	first := callTool(t, st, `{"id":"exp-`+p.ID+`"}`, tctx)
	if first.IsError || strings.Contains(first.Content, "rate_limited") {
		t.Fatalf("首次查询不该被限速: %q", first.Content)
	}
	second := callTool(t, st, `{"id":"exp-`+p.ID+`"}`, tctx)
	if !strings.Contains(second.Content, `"rate_limited": true`) {
		t.Fatalf("30 分钟内第二次查询必须 rate_limited (H9 把纪律做进工具层): %q", second.Content)
	}
	if !strings.Contains(second.Content, "retry_after_sec") {
		t.Error("限速响应应给出剩余秒数")
	}
}

// 限速时间戳要落盘 —— 否则进程重启就把限速清零, 纪律形同虚设。
func TestEvoStatus_限速时间戳持久化(t *testing.T) {
	tctx, stateDir := evoCtx(t)
	prop := callTool(t, NewEvoProposeTool(),
		`{"kind":"prompt","target":"a/b","body":"用 {objective} 干活"}`, tctx)
	var p struct{ ID string }
	_ = json.Unmarshal([]byte(prop.Content), &p)
	_ = callTool(t, NewEvoRunExperimentTool(), `{"id":"`+p.ID+`"}`, tctx)
	_ = callTool(t, NewEvoStatusTool(), `{"id":"exp-`+p.ID+`"}`, tctx)

	exp, err := loadExperiment(stateDir, "exp-"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exp.LastCheckedMS == 0 {
		t.Error("LastCheckedMS 应落盘, 否则重启即重置限速")
	}
}

func TestEvoStatus_实验不存在给可操作提示(t *testing.T) {
	tctx, _ := evoCtx(t)
	res := callTool(t, NewEvoStatusTool(), `{"id":"exp-不存在"}`, tctx)
	if !res.IsError || !strings.Contains(res.Content, "evo_run_experiment") {
		t.Errorf("应提示先启动实验, got %q", res.Content)
	}
}

// --- 路径安全 (入参来自 agent) ---

func TestLoadEnvTasks_拒绝路径穿越(t *testing.T) {
	_, stateDir := evoCtx(t)
	// 在 evalenv 之外放一个任务集, 用 ../ 试图读它
	outside := filepath.Join(stateDir, "evolution", "secret.jsonl")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte(`{"id":"x","objective":"o"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadEnvTasks(stateDir, "../secret"); err == nil {
		t.Fatal("../ 必须穿不出 evalenv 目录")
	}
}

func TestLoadExperiment_拒绝路径穿越(t *testing.T) {
	_, stateDir := evoCtx(t)
	if _, err := loadExperiment(stateDir, "../../../etc/passwd"); err == nil {
		t.Fatal("实验 id 的 ../ 必须被 filepath.Base 掉")
	}
}

// --- evo_list_envs / evo_inspect ---

func TestEvoListEnvs_无任务集时说明而非报错(t *testing.T) {
	tctx, _ := evoCtx(t)
	res := callTool(t, NewEvoListEnvsTool(), `{}`, tctx)
	if res.IsError {
		t.Fatalf("没有任务集不是错误, 应正常返回并说明: %q", res.Content)
	}
	if !strings.Contains(res.Content, "任务集目录不存在") {
		t.Errorf("应说明为什么是空的, got %q", res.Content)
	}
}

func TestEvoListEnvs_识别验证器种类(t *testing.T) {
	tctx, stateDir := evoCtx(t)
	dir := filepath.Join(stateDir, "evolution", "evalenv")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"id":"a","objective":"o1","expect":["OK"]}` + "\n" +
		`{"id":"b","objective":"o2","gate":"go build ./..."}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "coding.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	res := callTool(t, NewEvoListEnvsTool(), `{}`, tctx)
	var out struct {
		Envs []EvalEnvInfo `json:"envs"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Envs) != 1 || out.Envs[0].Tasks != 2 {
		t.Fatalf("应识别 1 个环境 2 个任务: %+v", out.Envs)
	}
	e := out.Envs[0]
	if e.WithExpect != 1 || e.WithGate != 1 {
		t.Errorf("验证器计数不对: %+v", e)
	}
	if !strings.Contains(e.VerifierKinds, "确定性断言") || !strings.Contains(e.VerifierKinds, "真门禁") {
		t.Errorf("验证器种类应两种都报: %q", e.VerifierKinds)
	}
}

func TestEvoInspect_总览与按ID(t *testing.T) {
	tctx, _ := evoCtx(t)
	tl := NewEvoInspectTool()
	if res := callTool(t, tl, `{}`, tctx); res.IsError {
		t.Errorf("空目录的总览不该报错: %q", res.Content)
	}
	if res := callTool(t, tl, `{"id":"wf-不存在"}`, tctx); !res.IsError {
		t.Error("不存在的草案应报错")
	}
}

// --- evo_smoke ---

// 未注入真实档位构造器时, 只有确定性装配档 → 必须拒绝并说明 (不能假装通过)。
func TestEvoSmoke_仅装配档时拒绝并说明(t *testing.T) {
	tctx, stateDir := evoCtx(t)
	SetEvoTierFactory(nil) // 确保干净
	dir := filepath.Join(stateDir, "evolution", "evalenv")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "e.jsonl"),
		[]byte(`{"id":"a","objective":"实现登录","expect":["实现登录"]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prop := callTool(t, NewEvoProposeTool(),
		`{"kind":"prompt","target":"a/b","body":"目标: {objective}"}`, tctx)
	var p struct{ ID string }
	_ = json.Unmarshal([]byte(prop.Content), &p)

	res := callTool(t, NewEvoSmokeTool(), `{"id":"`+p.ID+`","samples":1}`, tctx)
	if res.IsError {
		t.Fatalf("冒烟执行本身不该报错: %q", res.Content)
	}
	if !strings.Contains(res.Content, `"passed": false`) {
		t.Errorf("只有装配档时必须拒绝放行, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "未注入真实模型档位构造器") {
		t.Errorf("必须说明为什么只有一档, got %q", res.Content)
	}
}

// 装配档能抓住"候选丢了占位符"这类真实劣化。
func TestAssemblyCandidate_残留占位符报错(t *testing.T) {
	c := assemblyCandidate{body: "目标 {objective}, 另有 {未定义的槽}"}
	if _, err := c.Produce(context.Background(), replayTask("实现登录")); err == nil {
		t.Fatal("残留未解析占位符应报错")
	}
	c2 := assemblyCandidate{body: "目标 {objective}"}
	out, err := c2.Produce(context.Background(), replayTask("实现登录"))
	if err != nil {
		t.Fatalf("正常装配不该报错: %v", err)
	}
	if !strings.Contains(out, "实现登录") {
		t.Errorf("占位符应被替换: %q", out)
	}
	if _, err := (assemblyCandidate{}).Produce(context.Background(), replayTask("x")); err == nil {
		t.Error("无正文的草案 (workflow 类) 应明确报错而不是产出空串")
	}
}

// --- evo_promote / evo_rollback ---

func TestEvoPromote_技能不存在时被不越权闸拒绝(t *testing.T) {
	tctx, _ := evoCtx(t)
	res := callTool(t, NewEvoPromoteTool(), `{"target":"skill","name":"不存在的技能"}`, tctx)
	if !res.IsError {
		t.Fatal("读不到 SKILL.md 必须拒绝晋升 (fail-closed)")
	}
	if !strings.Contains(res.Content, "不越权闸") {
		t.Errorf("应明确是被哪道闸拒绝, got %q", res.Content)
	}
}

func TestEvoPromote_自动技能声明Bash被拒(t *testing.T) {
	tctx, stateDir := evoCtx(t)
	dir := filepath.Join(stateDir, "skills", "auto-bad")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: auto-bad\ndescription: 自动提炼\nauto_generated: true\nstatus: shadow\nallowed-tools: Read, Bash\n---\n正文\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	res := callTool(t, NewEvoPromoteTool(), `{"target":"skill","name":"auto-bad"}`, tctx)
	if !res.IsError {
		t.Fatal("自动技能声明 Bash 必须拒绝晋升")
	}
	if !strings.Contains(res.Content, "Bash") {
		t.Errorf("拒绝理由要点名越界工具, got %q", res.Content)
	}
	// 文件里的 status 不能被改动 (拒绝路径不留半成品)。
	data, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if !strings.Contains(string(data), "status: shadow") {
		t.Errorf("被拒后 status 必须仍是 shadow: %s", data)
	}
}

func TestEvoRollback_草案回退与退役(t *testing.T) {
	tctx, stateDir := evoCtx(t)
	prop := callTool(t, NewEvoProposeTool(),
		`{"kind":"prompt","target":"a/b","body":"目标 {objective}"}`, tctx)
	var p struct{ ID string }
	_ = json.Unmarshal([]byte(prop.Content), &p)

	if res := callTool(t, NewEvoRollbackTool(),
		`{"target":"proposal","name":"`+p.ID+`","retire":true}`, tctx); res.IsError {
		t.Fatalf("回滚失败: %q", res.Content)
	}
	data, _ := os.ReadFile(filepath.Join(stateDir, "evolution", "proposals", p.ID+".json"))
	if !strings.Contains(string(data), `"status": "archived"`) {
		t.Errorf("retire=true 应置 archived: %s", data)
	}
}

func TestEvoRollback_未知target报错(t *testing.T) {
	tctx, _ := evoCtx(t)
	if res := callTool(t, NewEvoRollbackTool(), `{"target":"weights","name":"x"}`, tctx); !res.IsError {
		t.Error("未知 target 应报错")
	}
}

// 全部 evo 工具的权限契约: 无需确认 (H8 全自助), 安全性由闸门而非提示保证。
func TestEvoTools_权限契约(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterEvolutionTools(reg)
	for _, n := range []string{"evo_propose", "evo_promote", "evo_rollback"} {
		tl, _ := reg.Get(n)
		if tl.CheckPermissions(nil, nil) != nil {
			t.Errorf("%s 不该要求权限确认 (否则 H8 全自助不成立)", n)
		}
		if tl.IsReadOnly(nil) {
			t.Errorf("%s 会写盘, IsReadOnly 应为 false", n)
		}
	}
	for _, n := range []string{"evo_list_envs", "evo_inspect", "evo_status", "evo_smoke"} {
		tl, _ := reg.Get(n)
		if !tl.IsReadOnly(nil) {
			t.Errorf("%s 应标只读", n)
		}
	}
}
