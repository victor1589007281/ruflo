// 外部测试包 (package govern_test): 它可以 import skillaudit / skills / agent 而不成环
// —— 生产依赖方向是 skillaudit → govern → agent, 反向 import 只发生在测试二进制里。
//
// # 这个文件补的是 §4.6 记账里那句「全量灰度未验」
//
// 四律的代码本轮之前就齐了 (不越权 govern / 必留痕 audit 行 / 必过闸 status 排除 /
// 可回滚 SetStatus), 缺的是**端到端跑一遍**: 从来没有人验证过"晋升之后运行期真的能
// 看见它、回滚之后真的又看不见"。这不是形式问题 —— 五级生命周期若不落到运行期可见性
// 上, 那它就只是 SKILL.md 里的一个字符串。
//
// 本文件把这条链真跑一遍, 全部走**生产函数**, 不 mock 任何一环:
//
//	skills.Registry.LoadFromDirs   运行期技能装载 (真装载器)
//	agent.EvolutionEngine.RecordReward  奖励落盘 (真写方, 真 rewards.jsonl)
//	skillaudit.Audit               门禁裁决 (真判据、真阈值)
//	skillaudit.SetStatus           手动晋升/回滚 (evo_promote / evo_rollback 走的同一条)
//	govern.CheckSkillPromotion     不越权闸 (被上面两条内部调用)
//
// "灰度"在本仓的实际形态是**二值**的 (shadow 不进运行期清单 / active 进), evo_run_experiment
// 的 shadow_ratio 只落在实验 JSON 里、运行期没有消费方。所以"全量灰度"能验的就是
// shadow→active→运行期全量可见这一跳, 以及它可逆。这一点如实记在下面的断言注释里,
// 不把"没有比例灰度"这件事糊过去。
package govern_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/evolution/skillaudit"
	"github.com/anthropic/claude-go/pkg/skills"
)

// lifecycleEnv 一次治理演练的真实状态目录。
type lifecycleEnv struct {
	state     string
	skillsDir string
	rewards   string
	ee        *agent.EvolutionEngine
}

func newLifecycleEnv(t *testing.T) *lifecycleEnv {
	t.Helper()
	state := t.TempDir()
	env := &lifecycleEnv{
		state:     state,
		skillsDir: filepath.Join(state, "skills"),
		rewards:   filepath.Join(state, "evolution", "rewards.jsonl"),
		ee:        agent.NewEvolutionEngine(filepath.Join(state, "evolution"), nil),
	}
	if err := os.MkdirAll(env.skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return env
}

// writeAutoSkill 造一份 AutoCreator.MaybeCreate 真实产物格式的自动提炼技能。
// created_at 取一个过去时刻 —— Audit 只认创建时间之后的奖励。
func (e *lifecycleEnv) writeAutoSkill(t *testing.T, name, allowedTools string) string {
	t.Helper()
	dir := filepath.Join(e.skillsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fm := "---\nname: " + name + "\ndescription: 自动提炼的技能\nwhen_to_use: 演练\n" +
		"created_at: 2020-01-01T00:00:00Z\nauto_generated: true\nstatus: shadow\n"
	if allowedTools != "" {
		fm += "allowed-tools: " + allowedTools + "\n"
	}
	fm += "---\n\n技能正文。\n"
	p := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(p, []byte(fm), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// runtimeVisible 重新走一遍**运行期**技能装载, 回答"agent 此刻拿不拿得到这个技能"。
//
// 必须每次重新 LoadFromDirs: 治理改的是磁盘上的 status, 运行期看到的是装载器的产物。
// 复用同一个 registry 就验不出"晋升有没有真的改变运行期可见面"这件事。
func (e *lifecycleEnv) runtimeVisible(t *testing.T, name string) bool {
	t.Helper()
	reg := skills.NewRegistry()
	reg.LoadFromDirs([]string{e.skillsDir}, "managed")
	_, ok := reg.Get(name)
	return ok
}

// governanceVisible 治理视图 (GetAny): shadow 技能必须仍然看得见, 否则门禁无从裁决。
func (e *lifecycleEnv) governanceVisible(t *testing.T, name string) (*skills.Skill, bool) {
	t.Helper()
	reg := skills.NewRegistry()
	reg.LoadFromDirs([]string{e.skillsDir}, "managed")
	return reg.GetAny(name)
}

// TestGovernance五级生命周期_影子到全量灰度再回滚 端到端跑一遍治理链 (design/03 §4.6)。
func TestGovernance五级生命周期_影子到全量灰度再回滚(t *testing.T) {
	env := newLifecycleEnv(t)
	const name = "auto-gate-runner"
	path := env.writeAutoSkill(t, name, "[Read, Grep]")

	// ── 阶段 1: shadow (proposed→shadow 已由 AutoCreator 落成) ──
	// 「必过闸」的运行期效力: 没过闸的产物不进 agent 的动作空间。
	if env.runtimeVisible(t, name) {
		t.Fatal("shadow 技能不得出现在运行期清单 —— 未过闸的产物进了动作空间就是闸形同虚设")
	}
	if sk, ok := env.governanceVisible(t, name); !ok || sk.Status != skills.StatusShadow {
		t.Fatalf("治理视图必须看得见 shadow 技能 (否则门禁无从裁决), got ok=%v", ok)
	}

	// ── 阶段 2: 攒真实奖励证据 ──
	// 经生产写方落真 rewards.jsonl; 值全部来自确定性门禁语义 (编译/测试通过)。
	for i := 0; i < 3; i++ {
		env.ee.RecordReward(agent.RewardEvent{
			RunID: "run-lifecycle", Team: "tm",
			Source: agent.RewardSourceGateCompile, Value: 1, Raw: "pass",
		})
	}
	if _, err := os.Stat(env.rewards); err != nil {
		t.Fatalf("奖励未落盘, 后面的门禁裁决就无从谈起: %v", err)
	}

	// ── 阶段 3: dry-run 不得改盘 ──
	// 「可回滚」的前提是默认不落地: 门禁先给结论, 落地是单独一步。
	res, err := skillaudit.Audit(env.skillsDir, env.rewards, false)
	if err != nil {
		t.Fatalf("Audit dry-run 失败: %v", err)
	}
	if !contains(res.Promoted, name) {
		t.Fatalf("奖励达标且权限面合法, dry-run 应判 promote, got %+v", res)
	}
	if env.runtimeVisible(t, name) {
		t.Fatal("dry-run 不得改变运行期可见面 —— 那样 --apply 这个开关就没有意义了")
	}

	// ── 阶段 4: apply → 全量灰度 ──
	res, err = skillaudit.Audit(env.skillsDir, env.rewards, true)
	if err != nil {
		t.Fatalf("Audit apply 失败: %v", err)
	}
	if !contains(res.Promoted, name) {
		t.Fatalf("apply 应晋升该技能, got %+v", res)
	}
	// 这一条就是「全量灰度」的全部实质: 晋升后运行期**真的**看得见。
	// (本仓的灰度是二值的 —— shadow 完全不可见 / active 全量可见; 比例灰度
	//  shadow_ratio 只存在于实验 JSON, 运行期无消费方。)
	if !env.runtimeVisible(t, name) {
		t.Fatal("晋升 active 后运行期必须看得见 —— 看不见就说明五级生命周期没落到运行期")
	}
	// 「必留痕」: 谱系行必须写进 frontmatter, 且带裁决依据。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "audit: active@") {
		t.Errorf("晋升必须留痕 (audit 行), 现文件:\n%s", data)
	}
	if !strings.Contains(string(data), "reward_avg=") || !strings.Contains(string(data), "samples=") {
		t.Errorf("留痕必须带裁决依据 (reward_avg/samples), 否则事后无法复核, 现文件:\n%s", data)
	}

	// ── 阶段 5: 一键回滚 ──
	if err := skillaudit.SetStatus(path, "shadow"); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if env.runtimeVisible(t, name) {
		t.Fatal("回滚后运行期必须立刻看不见 —— 否则「可回滚」只是改了个字符串")
	}
	// 退役方向同样可达 (五级的最后一级)。
	if err := skillaudit.SetStatus(path, "archived"); err != nil {
		t.Fatalf("退役失败: %v", err)
	}
	if env.runtimeVisible(t, name) {
		t.Fatal("archived 技能不得出现在运行期清单")
	}
}

// TestGovernance不越权闸_奖励达标也拦住越权产物 「不越权」必须是 fail-closed 的:
// 分数够高不能换来权限面放宽。
func TestGovernance不越权闸_奖励达标也拦住越权产物(t *testing.T) {
	env := newLifecycleEnv(t)
	const name = "auto-shell-runner"
	path := env.writeAutoSkill(t, name, "[Read, Bash]") // 自动产物声明执行类工具

	for i := 0; i < 5; i++ {
		env.ee.RecordReward(agent.RewardEvent{
			RunID: "run-esc", Team: "tm",
			Source: agent.RewardSourceGateTest, Value: 1, Raw: "pass",
		})
	}

	res, err := skillaudit.Audit(env.skillsDir, env.rewards, true)
	if err != nil {
		t.Fatalf("Audit 失败: %v", err)
	}
	if contains(res.Promoted, name) {
		t.Fatal("声明 Bash 的自动产物即使奖励满分也绝不能晋升")
	}
	if !contains(res.Rejected, name) {
		t.Fatalf("越权产物必须单列进 Rejected (与 Held 区分开), got %+v", res)
	}
	if reasons := res.RejectReasons[name]; len(reasons) == 0 ||
		!strings.Contains(strings.Join(reasons, " "), "Bash") {
		t.Errorf("拒绝理由要点名越界的工具, got %v", res.RejectReasons[name])
	}
	if env.runtimeVisible(t, name) {
		t.Fatal("被拒的产物不得进入运行期清单")
	}

	// agent 可达的手动通道 (evo_promote → SetStatus) 也必须过同一道闸, 否则机械强制
	// 就只是给自动通道加的装饰。
	if err := skillaudit.SetStatus(path, "active"); err == nil {
		t.Fatal("手动晋升越权产物必须报错 —— 手动通道能绕过等于闸不存在")
	}
	if env.runtimeVisible(t, name) {
		t.Fatal("手动晋升被拒后, 运行期可见面不得改变")
	}
}

// TestGovernance弱信号不得压过闸 新接入的弱奖励源 (verdict.heuristic) 数量再多,
// 也不能把一个被确定性门禁判负的技能顶过晋升线。
//
// 这条断言存在的理由: Audit 原先取的是**未加权**均值, 于是"接一个新的弱信号源"这个
// 纯增量动作会直接稀释一道 fail-closed 的治理闸 —— 跑得越多越容易晋升。
func TestGovernance弱信号不得压过闸(t *testing.T) {
	env := newLifecycleEnv(t)
	const name = "auto-weak-signal"
	env.writeAutoSkill(t, name, "[Read]")

	// 两条确定性门禁说"不行" (权重 1.0 各)。
	for i := 0; i < 2; i++ {
		env.ee.RecordReward(agent.RewardEvent{
			RunID: "run-weak", Team: "tm",
			Source: agent.RewardSourceGateTest, Value: -1, Raw: "fail",
		})
	}
	// 十条启发式过程信号说"跑完了" (权重 0.15 各)。
	for i := 0; i < 10; i++ {
		env.ee.RecordReward(agent.RewardEvent{
			RunID: "run-weak", Team: "tm", NodeID: "s" + string(rune('a'+i)),
			Source: agent.RewardSourceVerdictHeuristic, Value: 1,
		})
	}

	res, err := skillaudit.Audit(env.skillsDir, env.rewards, true)
	if err != nil {
		t.Fatalf("Audit 失败: %v", err)
	}
	if contains(res.Promoted, name) {
		t.Fatal("10 条弱过程信号不得把两条确定性门禁否决顶过晋升线 " +
			"—— 未加权均值 (-2+10)/12 = +0.67 会直接晋升; 加权后 (-2+1.5)/3.5 = -0.14, 保持 hold")
	}
	if env.runtimeVisible(t, name) {
		t.Fatal("未晋升的技能不得进入运行期清单")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
