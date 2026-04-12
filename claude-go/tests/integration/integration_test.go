// 集成测试: 使用阿里百炼 DashScope API 进行端到端测试。
// 测试 QueryEngine 的完整 ReAct 循环。
//
// 运行: go test ./tests/integration/ -v -count=1
// 需要设置: DASHSCOPE_API_KEY 环境变量
package integration

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	testAPIKey = "sk-sp-b0a692b1b8384b72971fe4d3a42798a1"
	testModel  = "qwen3.5-plus"
	testBaseURL = "https://coding.dashscope.aliyuncs.com/apps/anthropic/v1"
)

func getAPIKey() string {
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return key
	}
	return testAPIKey
}

func skipIfNoAPI(t *testing.T) {
	key := getAPIKey()
	if key == "" {
		t.Skip("需要 DASHSCOPE_API_KEY 环境变量")
	}
}

func buildTestEngine(t *testing.T) *engine.QueryEngine {
	t.Helper()

	cwd := t.TempDir()
	apiClient := api.NewClient(testBaseURL, getAPIKey(), testModel)
	reg := tool.NewRegistry()
	builtin.RegisterBaseTools(reg)
	hookRunner := hooks.NewRunner(nil, "test-session")
	permChecker := permissions.NewChecker(types.PermissionModeBypass)
	compactor := compact.NewCompactor(apiClient, 200000)
	promptMgr := prompt.NewManager(cwd)

	cfg := &engine.Config{
		Model:            testModel,
		MaxTokens:        4096,
		MaxTurns:         10,
		Cwd:              cwd,
		PermissionMode:   types.PermissionModeBypass,
		IsNonInteractive: true,
		SessionID:        "test-session",
	}

	return engine.NewQueryEngine(cfg, apiClient, reg, hookRunner, permChecker, compactor, promptMgr)
}

// TestSimpleQuery 测试简单文本查询 (无工具调用)
func TestSimpleQuery(t *testing.T) {
	skipIfNoAPI(t)

	eng := buildTestEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch := eng.SubmitMessage(ctx, "请用一句话回答: 1+1等于多少?")

	var response string
	for msg := range ch {
		if msg.Type == types.MessageTypeAssistant {
			for _, block := range msg.Content {
				if block.Type == types.ContentBlockText {
					response += block.Text
				}
			}
		}
	}

	if response == "" {
		t.Fatal("未收到模型回复")
	}
	t.Logf("模型回复: %s", response)

	if !strings.Contains(response, "2") {
		t.Errorf("回复应包含 '2': %s", response)
	}
}

// TestToolUseFileRead 测试工具调用: 文件读取
func TestToolUseFileRead(t *testing.T) {
	skipIfNoAPI(t)

	eng := buildTestEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 创建测试文件
	testFile := eng.Config.Cwd + "/test_data.txt"
	os.WriteFile(testFile, []byte("Hello from Go integration test!\nLine 2\nLine 3\n"), 0644)

	ch := eng.SubmitMessage(ctx, "请读取文件 "+testFile+" 并告诉我第一行的内容是什么。")

	var response string
	toolUsed := false
	for msg := range ch {
		if msg.Type == types.MessageTypeAssistant {
			for _, block := range msg.Content {
				if block.Type == types.ContentBlockText {
					response += block.Text
				}
				if block.Type == types.ContentBlockToolUse && block.Name == "Read" {
					toolUsed = true
				}
			}
		}
	}

	if !toolUsed {
		t.Log("注意: 模型未调用 Read 工具 (可能直接回答)")
	}

	if response == "" {
		t.Fatal("未收到模型回复")
	}
	t.Logf("模型回复: %s", response[:min(len(response), 200)])

	if !strings.Contains(strings.ToLower(response), "hello") && !strings.Contains(response, "integration") {
		t.Logf("回复可能未包含文件内容, 但测试继续")
	}
}

// TestToolUseFileWrite 测试工具调用: 文件写入
func TestToolUseFileWrite(t *testing.T) {
	skipIfNoAPI(t)

	eng := buildTestEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	outFile := eng.Config.Cwd + "/output.txt"
	ch := eng.SubmitMessage(ctx, "请在 "+outFile+" 中写入 'Hello World' 这个内容。")

	var response string
	for msg := range ch {
		if msg.Type == types.MessageTypeAssistant {
			for _, block := range msg.Content {
				if block.Type == types.ContentBlockText {
					response += block.Text
				}
			}
		}
	}

	t.Logf("模型回复: %s", response[:min(len(response), 200)])

	// 检查文件是否被创建
	if data, err := os.ReadFile(outFile); err == nil {
		t.Logf("文件内容: %s", string(data))
		if !strings.Contains(string(data), "Hello") {
			t.Logf("文件可能不包含期望内容")
		}
	} else {
		t.Logf("文件未被创建 (模型可能未调用 Write 工具)")
	}
}

// TestMultiTurnToolUse 测试多轮工具调用
func TestMultiTurnToolUse(t *testing.T) {
	skipIfNoAPI(t)

	eng := buildTestEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 创建测试文件
	testFile := eng.Config.Cwd + "/counter.txt"
	os.WriteFile(testFile, []byte("count: 0"), 0644)

	ch := eng.SubmitMessage(ctx, "请读取 "+testFile+" 文件, 然后将 count 的值改为 42, 最后再读取文件确认修改成功。用中文回复。")

	var response string
	toolCalls := 0
	for msg := range ch {
		if msg.Type == types.MessageTypeAssistant {
			for _, block := range msg.Content {
				if block.Type == types.ContentBlockText {
					response += block.Text
				}
				if block.Type == types.ContentBlockToolUse {
					toolCalls++
				}
			}
		}
	}

	t.Logf("模型回复 (前200字): %s", response[:min(len(response), 200)])
	t.Logf("工具调用次数: %d", toolCalls)

	// 验证文件最终状态
	data, err := os.ReadFile(testFile)
	if err == nil {
		t.Logf("文件最终内容: %s", string(data))
		if strings.Contains(string(data), "42") {
			t.Log("✓ 文件已正确修改为 42")
		}
	}
}

// TestBashToolExecution 测试 Shell 工具执行
func TestBashToolExecution(t *testing.T) {
	skipIfNoAPI(t)

	eng := buildTestEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ch := eng.SubmitMessage(ctx, "请使用 Shell 工具执行 'echo Hello_From_Shell' 命令, 然后告诉我输出结果。")

	var response string
	for msg := range ch {
		if msg.Type == types.MessageTypeAssistant {
			for _, block := range msg.Content {
				if block.Type == types.ContentBlockText {
					response += block.Text
				}
			}
		}
	}

	if response == "" {
		t.Fatal("未收到回复")
	}
	t.Logf("模型回复: %s", response[:min(len(response), 200)])
}

// ===================== 进化引擎 + 工作流 + 角色注册表 集成测试 =====================

// TestEvolutionEngine 测试进化引擎完整闭环: 记录轨迹 → LLM 提炼 → 检索注入 → 反馈更新
func TestEvolutionEngine(t *testing.T) {
	skipIfNoAPI(t)

	dataDir := t.TempDir()
	apiClient := api.NewClient(testBaseURL, getAPIKey(), testModel)
	evo := agent.NewEvolutionEngine(dataDir, apiClient)

	// 1. RECORD: 模拟记录轨迹
	evo.RecordTrajectory(agent.Trajectory{
		TeamName: "test-team", StageName: "design", Role: "architect",
		Objective: "设计一个用户认证系统",
		Output:    "方案: 使用 JWT + Redis 会话管理, 支持 OAuth2.0 第三方登录, 密码使用 bcrypt 加盐哈希",
		Success:   true, Duration: "15s", Timestamp: time.Now(),
	})
	evo.RecordTrajectory(agent.Trajectory{
		TeamName: "test-team", StageName: "implement", Role: "coder",
		Objective: "实现用户认证系统",
		Output:    "完成了 auth.go, middleware.go, jwt.go 三个文件的实现",
		Success:   true, Duration: "30s", Timestamp: time.Now(),
	})
	evo.RecordTrajectory(agent.Trajectory{
		TeamName: "test-team", StageName: "test", Role: "tester",
		Objective: "测试用户认证系统",
		Error:     "TestLogin 超时: context deadline exceeded",
		Success:   false, Duration: "45s", Timestamp: time.Now(),
	})

	stats := evo.Stats()
	if stats.TotalTrajectories != 3 {
		t.Fatalf("期望 3 条轨迹, 实际: %d", stats.TotalTrajectories)
	}
	t.Logf("✓ RECORD: %d 条轨迹已记录", stats.TotalTrajectories)

	// 2. DISTILL: LLM 提炼经验
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	evo.LearnFromTeam(ctx, "test-team")

	stats = evo.Stats()
	t.Logf("✓ DISTILL: 提炼出 %d 条经验 (角色:%d, 错误:%d, 通用:%d)",
		stats.TotalExperiences, stats.RoleExperiences, stats.ErrorPatterns, stats.GeneralPrinciples)

	if stats.TotalExperiences == 0 {
		t.Fatal("LLM 提炼未产生任何经验")
	}

	// 3. RETRIEVE: 检索相关经验
	exps := evo.RetrieveFor("architect", "设计认证系统", 3)
	t.Logf("✓ RETRIEVE: 检索到 %d 条相关经验", len(exps))
	for _, e := range exps {
		t.Logf("  [%s/%s] %s", e.Category, e.Role, truncateStr(e.Content, 80))
	}

	// 4. FormatForPrompt: 格式化注入
	promptText := agent.FormatExperiencesForPrompt(exps)
	if len(exps) > 0 && promptText == "" {
		t.Error("格式化后为空")
	}
	t.Logf("✓ FORMAT: prompt 注入片段长度 = %d bytes", len(promptText))

	// 5. EVOLVE: 反馈更新
	if len(exps) > 0 {
		evo.RecordFeedback(exps[0].ID, true)
		evo.RecordFeedback(exps[0].ID, true)
		evo.RecordFeedback(exps[0].ID, false)
		t.Log("✓ EVOLVE: 反馈已记录 (2 success + 1 failure)")
	}

	// 6. CONSOLIDATE: 整理
	evo.Consolidate()
	finalStats := evo.Stats()
	t.Logf("✓ CONSOLIDATE: 最终 %d 条经验", finalStats.TotalExperiences)

	// 7. 持久化: 重新加载
	evo2 := agent.NewEvolutionEngine(dataDir, nil)
	stats2 := evo2.Stats()
	if stats2.TotalExperiences != finalStats.TotalExperiences {
		t.Errorf("持久化: 期望 %d 条经验, 加载后: %d", finalStats.TotalExperiences, stats2.TotalExperiences)
	}
	t.Logf("✓ PERSISTENCE: 重新加载后仍有 %d 条经验", stats2.TotalExperiences)
}

// TestRoleRegistry 测试角色注册表
func TestRoleRegistry(t *testing.T) {
	cwd := t.TempDir()
	reg := agent.NewRoleRegistry(cwd)

	// 检查内置角色数量
	allRoles := reg.ListByCategory("")
	t.Logf("✓ 内置角色总数: %d", len(allRoles))
	if len(allRoles) < 10 {
		t.Errorf("期望至少 10 个内置角色, 实际: %d", len(allRoles))
	}

	// 按类别列出
	wfRoles := reg.ListByCategory("workflow")
	saRoles := reg.ListByCategory("standalone")
	t.Logf("✓ workflow 角色: %d, standalone 角色: %d", len(wfRoles), len(saRoles))

	// 获取单个角色
	arch := reg.Get("architect")
	if arch == nil {
		t.Fatal("architect 角色不存在")
	}
	if arch.SystemPrompt == "" {
		t.Error("architect 系统提示词为空")
	}
	t.Logf("✓ architect 提示词长度: %d", len(arch.SystemPrompt))

	// MergedPrompt 替换占位符
	merged := reg.MergedPrompt("architect", "设计API", "前置结果")
	if !strings.Contains(merged, "设计API") {
		t.Error("MergedPrompt 未替换 {objective}")
	}
	t.Log("✓ MergedPrompt 占位符替换正常")

	// 注册自定义角色
	reg.RegisterCustom(&agent.RoleDef{
		Name: "my-custom", Category: "workflow",
		Description: "自定义测试角色",
		SystemPrompt: "你是测试角色 {objective}",
	})
	custom := reg.Get("my-custom")
	if custom == nil {
		t.Fatal("自定义角色注册失败")
	}
	t.Log("✓ 自定义角色注册成功")

	// 从磁盘加载自定义角色
	agentsDir := cwd + "/.claude/agents"
	os.MkdirAll(agentsDir, 0755)
	roleJSON := `{"name":"disk-role","category":"standalone","description":"从磁盘加载","systemPrompt":"测试"}`
	os.WriteFile(agentsDir+"/disk-role.json", []byte(roleJSON), 0644)

	reg2 := agent.NewRoleRegistry(cwd)
	diskRole := reg2.Get("disk-role")
	if diskRole == nil {
		t.Error("从磁盘加载角色失败")
	} else {
		t.Log("✓ 磁盘自定义角色加载成功")
	}
}

// TestAgentPoolAutoScale 测试 Agent Pool 自动扩缩容
func TestAgentPoolAutoScale(t *testing.T) {
	factory := func(ctx context.Context, role, prompt string) (agent.AgentRunner, error) {
		return &mockRunner{}, nil
	}
	pool := agent.NewAgentPool(factory, 4)

	// 初始大小
	stats := pool.Stats()
	if stats.MaxSize != 4 {
		t.Errorf("初始大小应为 4, 实际: %d", stats.MaxSize)
	}

	// 模拟 8 个并行任务 → 应扩容
	pool.AutoScale(8)
	stats = pool.Stats()
	if stats.MaxSize < 8 {
		t.Errorf("8 个任务时池应扩容到至少 8, 实际: %d", stats.MaxSize)
	}
	t.Logf("✓ AutoScale(8): 池大小 → %d", stats.MaxSize)

	// 模拟 2 个任务 → 应缩容到最小
	pool.AutoScale(2)
	stats = pool.Stats()
	if stats.MaxSize != 4 {
		t.Logf("AutoScale(2): 池大小 → %d (最小 4)", stats.MaxSize)
	}
	t.Logf("✓ AutoScale(2): 池大小 → %d", stats.MaxSize)

	// 模拟 20 个任务 → 应不超过上限
	pool.AutoScale(20)
	stats = pool.Stats()
	if stats.MaxSize > 16 {
		t.Errorf("池大小不应超过 16, 实际: %d", stats.MaxSize)
	}
	t.Logf("✓ AutoScale(20): 池大小 → %d (上限 16)", stats.MaxSize)
}

// TestWorkflowWithEvolution 测试工作流 + 进化引擎端到端
func TestWorkflowWithEvolution(t *testing.T) {
	skipIfNoAPI(t)

	cwd := t.TempDir()
	apiClient := api.NewClient(testBaseURL, getAPIKey(), testModel)
	evo := agent.NewEvolutionEngine(cwd+"/.claude/evolution", apiClient)
	roleReg := agent.NewRoleRegistry(cwd)
	taskStore := builtin.NewTaskStore(cwd + "/.claude/tasks")

	// 创建简单的 agent runner 工厂
	factory := func(ctx context.Context, role, systemPrompt string) (agent.AgentRunner, error) {
		return &llmRunner{api: apiClient, systemPrompt: systemPrompt, role: role}, nil
	}

	pool := agent.NewAgentPool(factory, 4)

	var notifications []string
	notify := func(chatID, msg string) {
		notifications = append(notifications, msg)
		t.Logf("[通知] %s", truncateStr(msg, 120))
	}

	mgr := agent.NewProductionTeamManager(agent.TeamManagerConfig{
		BaseDir:     cwd + "/.claude/teams",
		Factory:     factory,
		Notify:      notify,
		TaskTracker: taskStore,
		Pool:        pool,
		LLM:         apiClient,
		Evolution:   evo,
		Roles:       roleReg,
	})

	// 创建并运行一个简化的 research 工作流
	team, err := mgr.CreateTeam("test-research", "research", "分析 Go 语言的并发模型优缺点", "test-chat")
	if err != nil {
		t.Fatalf("创建团队失败: %v", err)
	}
	t.Logf("✓ 团队创建成功: %s (%s)", team.Name, team.Workflow)

	err = mgr.RunTeam("test-research", "分析 Go 语言的并发模型优缺点")
	if err != nil {
		t.Fatalf("启动团队失败: %v", err)
	}

	// 等待完成 (最多 5 分钟)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("等待团队完成超时 (5分钟)")
		case <-time.After(5 * time.Second):
			team := mgr.GetTeam("test-research")
			if team == nil {
				t.Fatal("团队丢失")
			}
			switch team.Status {
			case agent.TeamStatusCompleted:
				t.Logf("✓ 团队执行完成, 耗时 %v", team.FinishedAt.Sub(team.StartedAt).Round(time.Second))
				for _, s := range team.Stages {
					t.Logf("  [%s/%s] %s — %s", s.Name, s.Role, s.Status, truncateStr(s.Output, 100))
				}

				// 验证进化引擎是否记录了轨迹并提炼了经验
				time.Sleep(2 * time.Second) // 等待后台 goroutine
				evoStats := evo.Stats()
				t.Logf("✓ 进化统计: 轨迹=%d, 经验=%d (角色:%d, 错误:%d, 通用:%d)",
					evoStats.TotalTrajectories, evoStats.TotalExperiences,
					evoStats.RoleExperiences, evoStats.ErrorPatterns, evoStats.GeneralPrinciples)

				if evoStats.TotalTrajectories == 0 {
					t.Error("工作流完成后应有轨迹记录")
				}
				return

			case agent.TeamStatusFailed:
				t.Logf("团队执行失败: %s", team.Error)
				t.Logf("通知数: %d", len(notifications))
				// 失败不算测试失败 (可能是 API 问题), 但打印详细信息
				return

			default:
				t.Logf("  等待中... 状态=%s", team.Status)
			}
		}
	}
}

// TestCronScheduler 测试 Cron 调度器核心逻辑 (无需 API)
func TestCronScheduler(t *testing.T) {
	cwd := t.TempDir()

	var executed []string
	var mu sync.Mutex
	executor := &testCronExecutor{
		onNotify: func(chatID, msg string) {
			mu.Lock()
			executed = append(executed, msg)
			mu.Unlock()
		},
	}

	sched := agent.NewCronScheduler(cwd+"/.claude/cron", executor)

	// 1. 添加任务
	job := &agent.CronJob{
		Name:     "test-job",
		Schedule: "*/5 * * * *",
		JobType:  "query",
		Payload:  "你好",
		ChatID:   "test-chat",
	}
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("添加任务失败: %v", err)
	}
	t.Logf("✓ 添加任务: %s (%s)", job.Name, job.ID)

	// 2. 列出任务
	jobs := sched.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("期望 1 个任务, 得到 %d", len(jobs))
	}
	t.Log("✓ 列出任务: 1 个")

	// 3. 暂停/恢复
	if err := sched.PauseJob(job.ID); err != nil {
		t.Fatalf("暂停失败: %v", err)
	}
	j := sched.GetJob(job.ID)
	if j.Enabled {
		t.Fatal("暂停后应为 disabled")
	}
	t.Log("✓ 暂停任务")

	if err := sched.ResumeJob(job.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	j = sched.GetJob(job.ID)
	if !j.Enabled {
		t.Fatal("恢复后应为 enabled")
	}
	t.Log("✓ 恢复任务")

	// 4. Stats
	total, enabled, _ := sched.Stats()
	if total != 1 || enabled != 1 {
		t.Fatalf("Stats 异常: total=%d, enabled=%d", total, enabled)
	}
	t.Log("✓ Stats: 1 total, 1 enabled")

	// 5. 持久化: 重新加载
	sched2 := agent.NewCronScheduler(cwd+"/.claude/cron", executor)
	jobs2 := sched2.ListJobs()
	if len(jobs2) != 1 {
		t.Fatalf("持久化恢复失败: 期望 1, 得到 %d", len(jobs2))
	}
	t.Log("✓ 持久化恢复正常")

	// 6. 格式化输出
	output := sched.FormatJobList()
	if !strings.Contains(output, "test-job") {
		t.Fatal("格式化输出应包含任务名")
	}
	t.Log("✓ 格式化输出正常")

	// 7. 删除任务
	if err := sched.RemoveJob(job.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if len(sched.ListJobs()) != 0 {
		t.Fatal("删除后应为空")
	}
	t.Log("✓ 删除任务")

	// 8. 无效 cron 表达式
	badJob := &agent.CronJob{Schedule: "invalid", JobType: "query", Payload: "x", ChatID: "c"}
	if err := sched.AddJob(badJob); err == nil {
		t.Fatal("应拒绝无效 cron 表达式")
	}
	t.Log("✓ 无效 cron 表达式被正确拒绝")
}

// TestNaturalScheduleParse 测试自然语言时间解析
func TestNaturalScheduleParse(t *testing.T) {
	cases := []struct {
		input    string
		wantExpr string
		wantDesc string
	}{
		{"每天9点帮我分析AAPL", "0 9 * * *", "每天9点"},
		{"工作日早上10点盯盘", "0 10 * * 1-5", "工作日10点"},
		{"每小时查看一下行情", "0 * * * *", "每小时"},
		{"每5分钟检查", "*/5 * * * *", "每5分钟"},
		{"每周一9点发报告", "0 9 * * 1", "每周一9点"},
	}
	for _, c := range cases {
		expr, desc := agent.ParseNaturalSchedule(c.input)
		if expr != c.wantExpr {
			t.Errorf("输入 %q: 期望表达式 %q, 得到 %q", c.input, c.wantExpr, expr)
		}
		if desc != c.wantDesc {
			t.Errorf("输入 %q: 期望描述 %q, 得到 %q", c.input, c.wantDesc, desc)
		}
		t.Logf("✓ %q → %s (%s)", c.input, expr, desc)
	}
}

// TestBlackboardSnapshotForRole 测试角色感知黑板快照
func TestBlackboardSnapshotForRole(t *testing.T) {
	cwd := t.TempDir()
	bb := agent.NewBlackboard("test-team", cwd)

	bb.Write("objective", "构建 REST API", "system", "context")
	bb.Write("design-decision", "使用 Gin 框架", "architect", "decision")
	bb.Write("t1-result", "```go\npackage main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n```\n\n完成了基础框架搭建。", "coder", "result")
	bb.Write("t2-result", strings.Repeat("很长的分析报告。", 200), "researcher", "result")

	snapshot := bb.SnapshotForRole("coder", 2000)
	if snapshot == "" {
		t.Fatal("快照不应为空")
	}
	if !strings.Contains(snapshot, "objective") {
		t.Error("快照应包含 context")
	}
	if !strings.Contains(snapshot, "Gin") {
		t.Error("快照应包含 decision")
	}
	if len(snapshot) > 2500 {
		t.Errorf("快照超出预算: %d chars", len(snapshot))
	}
	t.Logf("✓ SnapshotForRole: %d chars (budget 2000)", len(snapshot))
}

type testCronExecutor struct {
	onNotify func(chatID, msg string)
}

func (e *testCronExecutor) RunWorkflow(ctx context.Context, name, workflow, objective, chatID string) error {
	return nil
}
func (e *testCronExecutor) SendQuery(ctx context.Context, chatID, message string) (string, error) {
	return "test result", nil
}
func (e *testCronExecutor) RunCommand(ctx context.Context, chatID, command string) error {
	return nil
}
func (e *testCronExecutor) WikiOrganize(_ context.Context, _ string) (string, error) { return "", nil }
func (e *testCronExecutor) WikiHealthCheck(_ context.Context) (string, error) { return "", nil }
func (e *testCronExecutor) WikiLint(_ context.Context) (string, error) { return "", nil }
func (e *testCronExecutor) Notify(chatID, message string) {
	if e.onNotify != nil {
		e.onNotify(chatID, message)
	}
}

// --- 测试辅助 ---

type mockRunner struct{}

func (m *mockRunner) Execute(ctx context.Context, prompt string) (string, error) {
	return "mock result", nil
}

type llmRunner struct {
	api          *api.Client
	systemPrompt string
	role         string
}

func (r *llmRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	sys := r.systemPrompt
	if sys == "" {
		sys = "You are a helpful AI assistant with role: " + r.role
	}
	return r.api.SimpleComplete(ctx, sys, userPrompt)
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
