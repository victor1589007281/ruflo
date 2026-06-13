package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- stub agent runtime (无 LLM) ---

type stubRec struct {
	mu      sync.Mutex
	roles   []string
	prompts []string
}

func (r *stubRec) add(role, prompt string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roles = append(r.roles, role)
	r.prompts = append(r.prompts, prompt)
}
func (r *stubRec) snap() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.roles...), append([]string(nil), r.prompts...)
}

type stubRunner struct {
	role string
	rec  *stubRec
}

func (s *stubRunner) Execute(_ context.Context, prompt string) (string, error) {
	s.rec.add(s.role, prompt)
	// 返回一段普通技术文章文本(避开 planner/researcher 的伪工具调用校验)。
	return "# 文章\n\n这是一篇关于该主题的详尽技术文章, 涵盖背景、原理、实现与结论。" +
		strings.Repeat("段落内容, 论据充分。", 40), nil
}

// TestRefineTeamEndToEnd 端到端验证 RefineTeam: 把一个"已完成"的 pipeline 团队按指定阶段精修,
// 只重跑该阶段及其后续(前序复用检查点), 且用户反馈被注入重跑阶段的 prompt, 完成后留痕并清空。
func TestRefineTeamEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	baseDir := filepath.Join(tmp, "teams")
	rec := &stubRec{}
	factory := func(_ context.Context, role, _ string) (AgentRunner, error) {
		return &stubRunner{role: role, rec: rec}, nil
	}
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: baseDir,
		Factory: factory,
		Notify:  func(_, _ string) {},
	})

	team, err := ptm.CreateTeam("rt", "techblog", "写一篇关于向量数据库的技术博客", "test")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	// 模拟"已完成"团队: 写满检查点 + 置 completed。
	wf := GetWorkflow("techblog")
	cps := map[string]*Checkpoint{}
	for _, st := range wf.Stages {
		cps[st.Name] = &Checkpoint{StageName: st.Name, Status: "completed", Output: "原始产出:" + st.Name, SavedAt: time.Unix(1, 0)}
	}
	data, _ := json.MarshalIndent(cps, "", "  ")
	if err := os.WriteFile(filepath.Join(team.dataDir, "checkpoints.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	team.mu.Lock()
	team.Status = TeamStatusCompleted
	for _, st := range wf.Stages {
		team.Stages = append(team.Stages, StageResult{Name: st.Name, Role: st.Role, Status: TaskCompleted, Output: "原始:" + st.Name})
	}
	team.mu.Unlock()
	team.persist()

	// 精修: 从 article-writing 起重跑 (article-writing/self-critique/formatting), 前 3 阶段复用。
	feedback := "请补充基准测试数据并改进结论部分"
	if err := ptm.RefineTeam("rt", feedback, "article-writing"); err != nil {
		t.Fatalf("RefineTeam: %v", err)
	}
	team.WaitDone()

	// 1) 终态成功
	if !isSuccessfulTeamStatus(team.Status) {
		t.Fatalf("精修后状态应成功, got %s (err=%s)", team.Status, team.Error)
	}

	roles, prompts := rec.snap()

	// 2) 只重跑了 article-writing/self-critique/formatting 三个阶段的角色, 前序未重跑
	reran := map[string]bool{}
	for _, r := range roles {
		reran[r] = true
	}
	for _, want := range []string{"tech-writer", "tech-critic", "article-formatter"} {
		if !reran[want] {
			t.Errorf("应重跑角色 %q, 实际重跑=%v", want, roles)
		}
	}
	for _, notWant := range []string{"source-analyst", "tech-investigator", "fact-checker"} {
		if reran[notWant] {
			t.Errorf("角色 %q 不应重跑(应复用检查点), 实际重跑=%v", notWant, roles)
		}
	}

	// 3) 用户反馈注入了重跑阶段的 prompt
	foundFeedback := false
	for _, p := range prompts {
		if strings.Contains(p, feedback) {
			foundFeedback = true
			break
		}
	}
	if !foundFeedback {
		t.Error("用户反馈未注入任何重跑阶段的 prompt")
	}

	// 4) 完成后反馈清空 + 留痕 + 标记已采纳
	team.mu.Lock()
	pf := team.PendingFeedback
	hist := append([]RefineEntry(nil), team.RefineHistory...)
	team.mu.Unlock()
	if pf != "" {
		t.Errorf("完成后 PendingFeedback 应清空, got %q", pf)
	}
	if len(hist) != 1 {
		t.Fatalf("RefineHistory 应有 1 条, got %d", len(hist))
	}
	if hist[0].Feedback != feedback || hist[0].TargetStage != "article-writing" {
		t.Errorf("RefineHistory 记录不符: %+v", hist[0])
	}
	if hist[0].Accepted == nil || !*hist[0].Accepted {
		t.Error("精修成功应标记 Accepted=true")
	}
}
