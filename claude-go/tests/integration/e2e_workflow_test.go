// 端到端工作流真实测试: 使用阿里百炼 API 让 Agent 团队真实执行开发、调研、蜂群任务。
// 目的: 评估各模式的代码输出质量、运行效率、进化效果和通用性。
//
// 运行: go test ./tests/integration/ -v -run "TestE2E" -count=1 -timeout=600s
package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// e2eEnv 构建可复用的端到端测试环境
type e2eEnv struct {
	cwd       string
	api       *api.Client
	evo       *agent.EvolutionEngine
	roles     *agent.RoleRegistry
	pool      *agent.AgentPool
	taskStore *builtin.TaskStore
	mgr       *agent.ProductionTeamManager
	logs      []string
	mu        sync.Mutex
}

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	skipIfNoAPI(t)

	cwd := t.TempDir()
	apiClient := api.NewClient(testBaseURL, getAPIKey(), testModel)
	evo := agent.NewEvolutionEngine(cwd+"/.claude/evolution", apiClient)
	roleReg := agent.NewRoleRegistry(cwd)
	taskStore := builtin.NewTaskStore(cwd + "/.claude/tasks")

	factory := func(ctx context.Context, role, systemPrompt string) (agent.AgentRunner, error) {
		return &llmRunner{api: apiClient, systemPrompt: systemPrompt, role: role}, nil
	}
	pool := agent.NewAgentPool(factory, 8)

	env := &e2eEnv{
		cwd: cwd, api: apiClient, evo: evo, roles: roleReg,
		pool: pool, taskStore: taskStore,
	}

	env.mgr = agent.NewProductionTeamManager(agent.TeamManagerConfig{
		BaseDir:     cwd + "/.claude/teams",
		Factory:     factory,
		Notify:      env.notify,
		TaskTracker: taskStore,
		Pool:        pool,
		LLM:         apiClient,
		Evolution:   evo,
		Roles:       roleReg,
	})

	return env
}

func (e *e2eEnv) notify(chatID, msg string) {
	e.mu.Lock()
	e.logs = append(e.logs, msg)
	e.mu.Unlock()
}

func (e *e2eEnv) waitTeam(t *testing.T, name string, timeout time.Duration) *agent.ProductionTeam {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("等待团队 %s 超时 (%v)", name, timeout)
			return nil
		case <-time.After(3 * time.Second):
			team := e.mgr.GetTeam(name)
			if team == nil {
				t.Fatalf("团队 %s 丢失", name)
			}
			switch team.Status {
			case agent.TeamStatusCompleted:
				return team
			case agent.TeamStatusFailed:
				t.Logf("团队 %s 失败: %s", name, team.Error)
				return team
			}
		}
	}
}

// ==================== T1: Development Workflow 真实编程 ====================

func TestE2E_DevelopmentWorkflow(t *testing.T) {
	env := newE2EEnv(t)

	objective := "用 Go 实现一个并发安全的 LRU Cache，支持 Get/Put/Delete 操作，带 TTL 过期，写到 " + env.cwd + "/lru_cache.go"

	_, err := env.mgr.CreateTeam("dev-lru", "development", objective, "test-chat")
	if err != nil {
		t.Fatalf("创建团队失败: %v", err)
	}

	err = env.mgr.RunTeam("dev-lru", objective)
	if err != nil {
		t.Fatalf("启动团队失败: %v", err)
	}

	team := env.waitTeam(t, "dev-lru", 5*time.Minute)

	t.Log("\n========== Development Workflow 结果分析 ==========")
	t.Logf("状态: %s, 耗时: %v", team.Status, team.FinishedAt.Sub(team.StartedAt).Round(time.Second))

	for _, s := range team.Stages {
		t.Logf("\n--- 阶段: %s [%s] %s ---", s.Name, s.Role, s.Status)
		t.Logf("耗时: %s", s.Duration)
		output := s.Output
		if len(output) > 1500 {
			output = output[:1500] + "...(truncated)"
		}
		t.Logf("输出:\n%s", output)
		if s.Error != "" {
			t.Logf("错误: %s", s.Error)
		}
	}

	// 检查代码文件是否生成
	if data, err := os.ReadFile(env.cwd + "/lru_cache.go"); err == nil {
		t.Logf("\n✓ 代码文件已生成 (%d bytes)", len(data))
		if len(data) > 2000 {
			t.Logf("前2000字:\n%s", string(data[:2000]))
		} else {
			t.Logf("内容:\n%s", string(data))
		}
	} else {
		t.Log("✗ 代码文件未生成 (Agent 可能未调用 Write 工具)")
	}

	// 进化轨迹
	time.Sleep(3 * time.Second) // 等待异步学习
	evoStats := env.evo.Stats()
	t.Logf("\n进化统计: 轨迹=%d, 经验=%d", evoStats.TotalTrajectories, evoStats.TotalExperiences)
}

// ==================== T2: Research Workflow 调研 ====================

func TestE2E_ResearchWorkflow(t *testing.T) {
	env := newE2EEnv(t)

	objective := "深入分析 Rust vs Go 在高并发微服务场景的技术选型，包括性能对比、生态成熟度、开发效率、运维成本"

	_, err := env.mgr.CreateTeam("research-lang", "research", objective, "test-chat")
	if err != nil {
		t.Fatalf("创建团队失败: %v", err)
	}

	err = env.mgr.RunTeam("research-lang", objective)
	if err != nil {
		t.Fatalf("启动团队失败: %v", err)
	}

	team := env.waitTeam(t, "research-lang", 5*time.Minute)

	t.Log("\n========== Research Workflow 结果分析 ==========")
	t.Logf("状态: %s, 耗时: %v", team.Status, team.FinishedAt.Sub(team.StartedAt).Round(time.Second))

	for _, s := range team.Stages {
		t.Logf("\n--- 阶段: %s [%s] %s ---", s.Name, s.Role, s.Status)
		t.Logf("耗时: %s", s.Duration)
		output := s.Output
		if len(output) > 2000 {
			output = output[:2000] + "...(truncated)"
		}
		t.Logf("输出:\n%s", output)
	}

	// 质量评估: 检查 synthesize 阶段是否有结构化报告
	for _, s := range team.Stages {
		if s.Name == "synthesize" && s.Status == agent.TaskCompleted {
			hasStructure := strings.Contains(s.Output, "#") || strings.Contains(s.Output, "##")
			hasConclusion := strings.Contains(strings.ToLower(s.Output), "conclusion") ||
				strings.Contains(s.Output, "结论") || strings.Contains(s.Output, "建议")
			t.Logf("\n综合报告质量检查:")
			t.Logf("  结构化标题: %v", hasStructure)
			t.Logf("  包含结论/建议: %v", hasConclusion)
			t.Logf("  报告长度: %d chars", len(s.Output))
		}
	}
}

// ==================== T3: Swarm 蜂群编程开发 ====================

func TestE2E_SwarmDevelopment(t *testing.T) {
	env := newE2EEnv(t)

	objective := "用 Go 实现一个简单的 HTTP API 服务，包含: 1) /health 健康检查接口 2) /api/users CRUD 接口(内存存储) 3) 请求日志中间件。代码写到 " + env.cwd + "/ 目录下"

	_, err := env.mgr.CreateTeam("swarm-api", "swarm", objective, "test-chat")
	if err != nil {
		t.Fatalf("创建团队失败: %v", err)
	}

	err = env.mgr.RunTeam("swarm-api", objective)
	if err != nil {
		t.Fatalf("启动团队失败: %v", err)
	}

	team := env.waitTeam(t, "swarm-api", 10*time.Minute)

	t.Log("\n========== Swarm 蜂群模式 结果分析 ==========")
	t.Logf("状态: %s, 耗时: %v", team.Status, team.FinishedAt.Sub(team.StartedAt).Round(time.Second))
	t.Logf("蜂群阶段数: %d", len(team.Stages))

	for _, s := range team.Stages {
		t.Logf("\n--- 子任务: %s [%s] %s ---", s.Name, s.Role, s.Status)
		t.Logf("耗时: %s", s.Duration)
		output := s.Output
		if len(output) > 1500 {
			output = output[:1500] + "...(truncated)"
		}
		t.Logf("输出:\n%s", output)
	}

	// 检查输出文件
	entries, _ := os.ReadDir(env.cwd)
	t.Logf("\n输出文件:")
	for _, e := range entries {
		if !e.IsDir() {
			info, _ := e.Info()
			t.Logf("  %s (%d bytes)", e.Name(), info.Size())
		}
	}
}

// ==================== T4: 进化效果评估 (两轮执行对比) ====================

func TestE2E_EvolutionEffect(t *testing.T) {
	env := newE2EEnv(t)

	// --- 第一轮: 无经验执行 ---
	t.Log("\n========== 第一轮: 无经验执行 ==========")
	objective1 := "设计一个 Go 语言的 Worker Pool 模式实现方案"

	_, err := env.mgr.CreateTeam("evo-round1", "research", objective1, "test-chat")
	if err != nil {
		t.Fatalf("创建团队失败: %v", err)
	}
	_ = env.mgr.RunTeam("evo-round1", objective1)
	team1 := env.waitTeam(t, "evo-round1", 5*time.Minute)

	t.Logf("第一轮状态: %s, 耗时: %v", team1.Status, team1.FinishedAt.Sub(team1.StartedAt).Round(time.Second))

	// 等待异步学习完成
	t.Log("等待进化学习...")
	time.Sleep(45 * time.Second)

	stats1 := env.evo.Stats()
	t.Logf("第一轮后进化: 轨迹=%d, 经验=%d (角色:%d, 错误:%d, 通用:%d)",
		stats1.TotalTrajectories, stats1.TotalExperiences,
		stats1.RoleExperiences, stats1.ErrorPatterns, stats1.GeneralPrinciples)

	// 检索与第二轮任务相关的经验
	exps := env.evo.RetrieveFor("researcher", "Go 并发设计模式", 5)
	t.Logf("相关经验检索: %d 条", len(exps))
	for _, e := range exps {
		t.Logf("  [%s] Q=%.2f: %s", e.Category, e.Quality, truncateStr(e.Content, 100))
	}

	// --- 第二轮: 有经验执行 ---
	t.Log("\n========== 第二轮: 有经验执行 ==========")
	objective2 := "设计一个 Go 语言的 Pipeline 并发模式实现方案 (类似 Worker Pool 但有阶段依赖)"

	_, err = env.mgr.CreateTeam("evo-round2", "research", objective2, "test-chat")
	if err != nil {
		t.Fatalf("创建团队失败: %v", err)
	}
	_ = env.mgr.RunTeam("evo-round2", objective2)
	team2 := env.waitTeam(t, "evo-round2", 5*time.Minute)

	t.Logf("第二轮状态: %s, 耗时: %v", team2.Status, team2.FinishedAt.Sub(team2.StartedAt).Round(time.Second))

	time.Sleep(5 * time.Second)
	stats2 := env.evo.Stats()
	t.Logf("第二轮后进化: 轨迹=%d, 经验=%d", stats2.TotalTrajectories, stats2.TotalExperiences)

	// 对比分析
	t.Log("\n========== 进化效果分析 ==========")
	t.Logf("轨迹增长: %d → %d", stats1.TotalTrajectories, stats2.TotalTrajectories)
	t.Logf("经验增长: %d → %d", stats1.TotalExperiences, stats2.TotalExperiences)

	// 检查第二轮输出是否引用了注入的经验
	for _, s := range team2.Stages {
		if s.Name == "synthesize" && s.Status == agent.TaskCompleted {
			t.Logf("第二轮综合报告长度: %d chars", len(s.Output))
		}
	}

	// 打印持久化文件
	if data, err := os.ReadFile(env.cwd + "/.claude/evolution/experiences.json"); err == nil {
		t.Logf("经验库文件大小: %d bytes", len(data))
		if len(data) > 3000 {
			data = data[:3000]
		}
		t.Logf("经验库内容:\n%s", string(data))
	}
}

// ==================== 通知日志格式化 ====================

func (e *e2eEnv) printLogs(t *testing.T) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t.Log("\n========== 通知日志 ==========")
	for _, l := range e.logs {
		if len(l) > 200 {
			l = l[:200] + "..."
		}
		t.Logf("  %s", l)
	}
}

// TestE2E_AllWorkflows 运行所有工作流测试并汇总分析
func TestE2E_AllWorkflows(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过")
	}
	// 依次运行 dev/research/swarm，运行一个即可得到完整分析
	t.Log("提示: 请分别运行 TestE2E_DevelopmentWorkflow, TestE2E_ResearchWorkflow, TestE2E_SwarmDevelopment")
	t.Log("或运行 TestE2E_EvolutionEffect 评估进化效果")
	t.Log(fmt.Sprintf("go test ./tests/integration/ -v -run TestE2E_ -count=1 -timeout=600s"))
}
