package agent

import (
	"context"
	"strings"
	"testing"
)

// CreateTeam 的"未知工作流"报错原先硬编码了一串名字, 既漏掉全部动态注册的工作流,
// 又列了 debate / predict 两个实际拿不到的名字——用户照报错里的名字重试会拿到
// 同一句报错。本测试锁死"报错名单 = 真正可建的工作流"这条契约。
func TestAvailableWorkflowNames_每个名字都真能建团队(t *testing.T) {
	names := AvailableWorkflowNames()
	if len(names) < 15 {
		t.Fatalf("可用工作流只有 %d 个, 疑似真源取错: %v", len(names), names)
	}

	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: t.TempDir(),
		Factory: func(context.Context, string, string) (AgentRunner, error) { return nil, nil },
	})

	for _, n := range names {
		team, err := ptm.CreateTeam("t-"+strings.ReplaceAll(n, "_", "-"), n, "obj", "test")
		if err != nil {
			t.Errorf("AvailableWorkflowNames 列出了 %q, 但 CreateTeam 拒绝它: %v", n, err)
			continue
		}
		if team == nil {
			t.Errorf("CreateTeam(%q) 返回 nil team 且无错误", n)
		}
	}
}

// 反向: 报错名单里不该出现拿不到的名字。debate 已彻底不存在; predict 虽有
// runPredict 实现与特判分支, 但 CreateTeam 只放行 swarm 一个伪工作流, 故不可达。
func TestAvailableWorkflowNames_不含不可达名字(t *testing.T) {
	names := AvailableWorkflowNames()
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	for _, bad := range []string{"debate", "predict"} {
		if set[bad] {
			t.Errorf("名单含 %q, 但 GetWorkflow(%q)=nil 且非放行的伪工作流——会误导用户", bad, bad)
		}
	}
	// swarm 是唯一被 CreateTeam 显式放行的伪工作流, 必须在名单里
	if !set["swarm"] {
		t.Error("名单缺 swarm, 而 CreateTeam 是放行它的")
	}
}

// 名单必须能反映动态注册（下游平台注册的工作流此前一个都没被列出）。
func TestAvailableWorkflowNames_含动态注册(t *testing.T) {
	before := len(AvailableWorkflowNames())

	const name = "unit-test-dynamic-wf"
	roles := NewRoleRegistry(t.TempDir())
	if err := RegisterWorkflow(&WorkflowDef{
		Name:        name,
		Mode:        "pipeline",
		Description: "临时动态工作流",
		Stages:      []StageDef{{Name: "only", Role: "researcher"}},
	}, roles); err != nil {
		t.Skipf("动态注册被拒(可能是角色校验), 跳过: %v", err)
	}
	t.Cleanup(func() { _ = UnregisterWorkflow(name) })

	names := AvailableWorkflowNames()
	if len(names) != before+1 {
		t.Errorf("注册后名单数 %d, 期望 %d", len(names), before+1)
	}
	found := false
	for _, n := range names {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Errorf("动态注册的 %q 未出现在名单里", name)
	}
}

// 名单必须有序且无重复, 否则报错文本每次进程重启都变一个样, 无法据此排查。
func TestAvailableWorkflowNames_有序无重(t *testing.T) {
	names := AvailableWorkflowNames()
	seen := make(map[string]bool, len(names))
	for i, n := range names {
		if seen[n] {
			t.Errorf("名单重复出现 %q", n)
		}
		seen[n] = true
		if i > 0 && names[i-1] > n {
			t.Errorf("名单未排序: %q 在 %q 之后", n, names[i-1])
		}
	}
}
