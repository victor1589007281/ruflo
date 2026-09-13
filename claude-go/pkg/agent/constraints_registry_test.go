package agent_test

// constraints_registry_test.go —— ConstraintSet 档位表与**真实工具注册表**的双向核对。
//
// 为什么它必须在外部测试包 (agent_test) 而不是与 constraints_test.go 同包:
// 本文件要 import pkg/tool/builtin, 而 builtin 的 evotools.go → pkg/evolution/govern
// → 回过头 import pkg/agent (govern 复用 ConstraintSet.Narrow 做单调性判定, 那是对的
// —— 复制一份带测试的单调性实现只会漂移)。同包测试文件加上这条 import 就构成
// **测试二进制的导入环**, 整个 pkg/agent 测试直接 setup failed。
// 外部测试包正是 Go 为这种情形留的逃生门: agent_test 可以 import 那些 import agent 的包。

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// toolNameCaps 真实工具名 → 特权位。这是本测试唯一的"人工事实", 粒度比档位表细
// 一级; 档位表 (profileCaps) 则必须能从它 + 真实注册内容推导出来。
var toolNameCaps = map[string]agent.Capability{
	"Read": agent.CapReadFS, "Glob": agent.CapReadFS, "Grep": agent.CapReadFS, "LSP": agent.CapReadFS,
	"Write": agent.CapWriteFS, "StrReplace": agent.CapWriteFS,
	"EnterWorktree": agent.CapWriteFS, "ExitWorktree": agent.CapWriteFS,
	"Bash": agent.CapExec, "Shell": agent.CapExec, // Shell 是 Bash 的旧名 (别名), 两者同维
	"TaskOutput": agent.CapExec, "TaskStop": agent.CapExec,
	"WebFetch": agent.CapNet, "WebSearch": agent.CapNet, "FetchKLine": agent.CapNet, "FetchQuote": agent.CapNet,
	"code_intel_query": agent.CapReadIndex, "code_intel_status": agent.CapReadIndex, "code_intel_branch": agent.CapReadIndex,
	"code_intel_init": agent.CapWriteIndex, "code_intel_update": agent.CapWriteIndex,
	"EnterPlanMode": agent.CapPlan, "ExitPlanMode": agent.CapPlan,
	"TodoWrite": agent.CapTask, "TaskCreate": agent.CapTask, "TaskGet": agent.CapTask, "TaskUpdate": agent.CapTask, "TaskList": agent.CapTask,
	"AskUserQuestion": agent.CapInteract,
	"GenerateImage":   agent.CapMedia, "GenerateGIF": agent.CapMedia, "GenerateVideo": agent.CapMedia,
	"GeneratePPTX": agent.CapMedia, "GenerateChart": agent.CapMedia, "GenerateSpeech": agent.CapMedia,
	"TeamCreate": agent.CapTeam, "TeamDelete": agent.CapTeam, "TeamMailbox": agent.CapTeam,
	"Config": agent.CapAdminExtra, "Skill": agent.CapAdminExtra, "ComputerUseStatus": agent.CapAdminExtra,
	"CronCreate": agent.CapAdminExtra, "CronDelete": agent.CapAdminExtra, "CronList": agent.CapAdminExtra,
	// 进化操作台 evo_*(design/03 §4.7)。归 CapAdminExtra 而非新开一维, 因为它们是
	// `Skill` 工具的特化 —— 都在改"agent 将来能用什么", 而 Skill 已在这一维。
	// **只读的三个 (list_envs/inspect/status) 同样占特权位**: 把它们标 0 等于宣称
	// "任何档位都可以安全持有它们", 而它们暴露的是进化控制面(有哪些实验、影子技能、
	// 阈值), 那不是我们想立的断言。
	"evo_list_envs": agent.CapAdminExtra, "evo_inspect": agent.CapAdminExtra,
	"evo_status": agent.CapAdminExtra, "evo_propose": agent.CapAdminExtra,
	"evo_smoke": agent.CapAdminExtra, "evo_run_experiment": agent.CapAdminExtra,
	"evo_promote": agent.CapAdminExtra, "evo_rollback": agent.CapAdminExtra,
	// 不扩大能力边界的输出/检索成型类工具, 不占特权位 (见 agent.Capability 注释)。
	"StructuredOutput": 0, "ToolSearch": 0,
}

func realProfileTools(t *testing.T, profile builtin.ToolProfile) []string {
	t.Helper()
	reg := tool.NewRegistry()
	builtin.RegisterProfileToolsWithStore(reg, nil, nil, profile)
	return reg.Names()
}

func TestProfileCaps_每个工具都已分类(t *testing.T) {
	// 往任何档位加新工具而忘了分类, 这里就会红 —— 强制新工具必须明确它的特权维度,
	// 否则档位偏序 (Narrow 的判据) 会静默失真。
	for _, p := range []builtin.ToolProfile{
		builtin.ToolProfileChat, builtin.ToolProfileAnalysis, builtin.ToolProfileTeam,
		builtin.ToolProfileResearch, builtin.ToolProfileCoding, builtin.ToolProfileAdmin,
	} {
		for _, name := range realProfileTools(t, p) {
			if _, ok := toolNameCaps[name]; !ok {
				t.Errorf("档位 %s 里的工具 %q 未在 toolNameCaps 分类; "+
					"请给它定特权维度并同步 profileCaps", p, name)
			}
		}
	}
}

func TestProfileCaps_与真实注册表双向一致(t *testing.T) {
	profiles := []builtin.ToolProfile{
		builtin.ToolProfileChat, builtin.ToolProfileAnalysis, builtin.ToolProfileTeam,
		builtin.ToolProfileResearch, builtin.ToolProfileCoding, builtin.ToolProfileAdmin,
	}
	// 每个档位的"特权工具集" (剔除不占位的成型类工具)。
	privileged := map[builtin.ToolProfile]map[string]bool{}
	for _, p := range profiles {
		set := map[string]bool{}
		for _, name := range realProfileTools(t, p) {
			if toolNameCaps[name] != 0 {
				set[name] = true
			}
		}
		privileged[p] = set
	}

	subset := func(a, b map[string]bool) bool {
		for n := range a {
			if !b[n] {
				return false
			}
		}
		return true
	}

	// 双向一致: 「特权位表说 child 可收窄自 parent」⟺「真实特权工具集 child ⊆ parent」。
	// 少一个方向就只能证明表不冒进 / 表不保守其中一半。
	for _, child := range profiles {
		for _, parent := range profiles {
			byTable := agent.ProfileNarrows(string(child), string(parent))
			byReal := subset(privileged[child], privileged[parent])
			if byTable != byReal {
				t.Errorf("%s ⊆ %s: 特权位表说 %v, 真实注册表说 %v —— profileCaps 与 "+
					"pkg/tool/builtin/profile.go 漂移了", child, parent, byTable, byReal)
			}
		}
	}
	// 顺带钉住 admin 是顶元素这一历史行为 (admin = 全部内置工具)。
	for _, p := range profiles {
		if !subset(privileged[p], privileged[builtin.ToolProfileAdmin]) {
			t.Errorf("admin 应是 %s 的超集", p)
		}
	}
}

func TestProfileCaps_档位名与builtin常量逐字一致(t *testing.T) {
	// 两处字符串必须相同, 否则宿主传进来的档位名会全部"不可识别"→ Narrow 全拒。
	pairs := map[string]builtin.ToolProfile{
		agent.ProfileChat: builtin.ToolProfileChat, agent.ProfileResearch: builtin.ToolProfileResearch,
		agent.ProfileCoding: builtin.ToolProfileCoding, agent.ProfileTeam: builtin.ToolProfileTeam,
		agent.ProfileAnalysis: builtin.ToolProfileAnalysis, agent.ProfileAdmin: builtin.ToolProfileAdmin,
	}
	if len(pairs) != len(agent.KnownProfiles()) {
		t.Fatalf("档位数量不一致: 本包 %d, 对照 %d", len(agent.KnownProfiles()), len(pairs))
	}
	for mine, theirs := range pairs {
		if mine != string(theirs) {
			t.Errorf("档位名不一致: %q vs %q", mine, theirs)
		}
	}
}
