//go:build ignore

// test_all_workflows.go — 统一测试: code-review, testing, parenting, hiring 四个团队。
// 用法: cd /home/victor/base/git/temp/ruflo/claude-go && go run test_all_workflows.go

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

const (
	apiKey  = "sk-sp-b0a692b1b8384b72971fe4d3a42798a1"
	baseURL = "https://coding.dashscope.aliyuncs.com/apps/anthropic/v1"
	model   = "qwen3.6-plus"
	stateDir = "/home/victor/.claude-go"
	cwd     = "/home/victor/base/git/gitee/claudeGo"
)

type workflowCase struct {
	name      string
	chatID    string
	objective string
	timeout   time.Duration
}

var cases = []workflowCase{
	{
		name:      "code-review",
		chatID:    "test-cli-code-review",
		objective: "审查以下 Go 代码：\n\n```go\npackage main\n\nimport \"fmt\"\n\nfunc main() {\n    var users []User\n    for _, u := range getUsers() {\n        if u.Age > 18 {\n            users = append(users, u)\n        }\n    }\n    fmt.Println(users)\n}\n\ntype User struct {\n    ID   int\n    Name string\n    Age  int\n}\n\nfunc getUsers() []User {\n    return []User{{1, \"Alice\", 25}, {2, \"Bob\", 16}, {3, \"Charlie\", 30}}\n}\n```\n\n请从代码规范、安全性、性能、并发安全等维度进行全面审查。",
		timeout:   15 * time.Minute,
	},
	{
		name:      "testing",
		chatID:    "test-cli-testing",
		objective: "为以下 Go 工具包生成完整的测试方案：\n\n```go\npackage cache\n\nimport \"sync\"\n\ntype Cache struct {\n    mu    sync.RWMutex\n    items map[string]interface{}\n    ttl   map[string]time.Time\n}\n\nfunc New() *Cache {\n    return &Cache{items: make(map[string]interface{}), ttl: make(map[string]time.Time)}\n}\n\nfunc (c *Cache) Set(key string, value interface{}, ttl time.Duration) {\n    c.mu.Lock()\n    defer c.mu.Unlock()\n    c.items[key] = value\n    c.ttl[key] = time.Now().Add(ttl)\n}\n\nfunc (c *Cache) Get(key string) (interface{}, bool) {\n    c.mu.RLock()\n    defer c.mu.RUnlock()\n    v, ok := c.items[key]\n    if !ok {\n        return nil, false\n    }\n    if time.Now().After(c.ttl[key]) {\n        return nil, false\n    }\n    return v, true\n}\n```\n\n请生成单元测试、集成测试、属性测试和混沌测试方案。",
		timeout:   15 * time.Minute,
	},
	{
		name:      "parenting",
		chatID:    "test-cli-parenting",
		objective: "孩子8岁，数学成绩下降，上课注意力不集中，在家写作业总是拖延，家长很焦虑，请给综合建议。",
		timeout:   15 * time.Minute,
	},
	{
		name:      "hiring",
		chatID:    "test-cli-hiring",
		objective: "我是一名有3年经验的 Go 后端开发工程师，准备应聘一家大厂的 P7 高级开发岗位。JD 要求：精通 Go/Java 至少一门语言，熟悉分布式系统、微服务架构，有 K8s/Docker 实践经验，具备高并发系统设计能力，有大型项目主导经验。我的简历：主导过电商订单系统重构（QPS 从 2K 提升到 20K），熟悉 etcd/gRPC/prometheus 生态，有 K8s operator 开发经验，但缺乏系统设计面试经验。请帮我做全面的求职准备。",
		timeout:   15 * time.Minute,
	},
}

func main() {
	sd := basedir.ResolveDefault(stateDir, cwd)
	logDir := sd + "/logs"
	os.MkdirAll(logDir, 0o755)
	logging.Init(&logging.LogConfig{Dir: logDir})

	apiClient := api.NewClient(baseURL, apiKey, model)

	notifyFn := func(chatID, msg string) {
		lines := strings.Split(msg, "\n")
		for _, line := range lines {
			log.Printf("[Notify] %s", line)
		}
	}

	factory := func(ctx context.Context, role, systemPrompt string) (agent.AgentRunner, error) {
		return &llmRunner{api: apiClient, systemPrompt: systemPrompt, role: role}, nil
	}

	taskStore := builtin.NewTaskStore(sd + "/.dashboard/tasks")
	roles := agent.NewRoleRegistry(sd + "/roles")
	pool := agent.NewAgentPool(factory, 8)

	mgr := agent.NewProductionTeamManager(agent.TeamManagerConfig{
		BaseDir:     sd + "/teams",
		Cwd:         cwd,
		Factory:     factory,
		Notify:      notifyFn,
		Pool:        pool,
		LLM:         apiClient,
		Evolution:   nil,
		Roles:       roles,
		TaskTracker: taskStore,
	})

	results := make(map[string]*teamResult)

	for i, tc := range cases {
		fmt.Printf("\n%s 测试 %d/%d: %s %s\n", strings.Repeat("=", 60), i+1, len(cases), tc.name, strings.Repeat("=", 60))

		teamName := fmt.Sprintf("%s-test-%d", tc.name, time.Now().Unix())

		fmt.Printf("🚀 创建团队: %s\n", teamName)
		fmt.Printf("   工作流: %s\n", tc.name)
		fmt.Printf("   目标: %s\n", truncate(tc.objective, 100))

		team, err := mgr.CreateTeam(teamName, tc.name, tc.objective, tc.chatID)
		if err != nil {
			log.Printf("❌ 创建团队 %s 失败: %v", teamName, err)
			results[tc.name] = &teamResult{status: "failed", error: err.Error()}
			continue
		}
		fmt.Printf("   Agent 数: %d\n", len(team.Agents))

		if err := mgr.RunTeam(teamName, tc.objective); err != nil {
			log.Printf("❌ 启动团队 %s 失败: %v", teamName, err)
			results[tc.name] = &teamResult{status: "failed", error: err.Error()}
			continue
		}

		fmt.Println("⏳ 等待执行完成...")
		result := monitorTeam(mgr, teamName, tc.timeout)
		results[tc.name] = result

		if result.status == "completed" {
			fmt.Printf("✅ %s 执行完成! 耗时: %s\n", tc.name, result.duration)
		} else {
			fmt.Printf("❌ %s 执行失败: %s\n", tc.name, result.error)
		}
	}

	// 打印汇总
	fmt.Printf("\n%s 全部测试完成 %s\n", strings.Repeat("=", 60), strings.Repeat("=", 60))
	printSummary(results)
}

type teamResult struct {
	status   string
	duration time.Duration
	stages   []stageInfo
	error    string
}

type stageInfo struct {
	name      string
	status    string
	outputLen int
	errMsg    string
}

func monitorTeam(mgr *agent.ProductionTeamManager, teamName string, timeout time.Duration) *teamResult {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t := mgr.GetTeam(teamName)
			if t == nil {
				return &teamResult{status: "failed", error: "团队丢失"}
			}
			age := time.Since(t.StartedAt).Round(time.Second)
			if t.Status == agent.TeamStatusRunning {
				fmt.Printf("  [%s] %s: running\n", age, teamName)
			} else {
				fmt.Printf("  [%s] %s: %s (阶段数=%d)\n", age, teamName, t.Status, len(t.Stages))
			}

			switch t.Status {
			case agent.TeamStatusCompleted:
				r := &teamResult{
					status:   "completed",
					duration: time.Since(t.StartedAt),
				}
				for _, s := range t.Stages {
					si := stageInfo{
						name:      s.Name,
						status:    string(s.Status),
						outputLen: len(s.Output),
						errMsg:    s.Error,
					}
					r.stages = append(r.stages, si)
				}
				return r
			case agent.TeamStatusFailed:
				return &teamResult{
					status:   "failed",
					duration: time.Since(t.StartedAt),
					error:    t.Error,
				}
			}

			if time.Now().After(deadline) {
				fmt.Println("⏱ 超时，停止团队")
				mgr.StopTeam(teamName)
				t = mgr.GetTeam(teamName)
				r := &teamResult{
					status:   "timeout",
					duration: timeout,
					error:    "执行超时",
				}
				for _, s := range t.Stages {
					si := stageInfo{
						name:      s.Name,
						status:    string(s.Status),
						outputLen: len(s.Output),
						errMsg:    s.Error,
					}
					r.stages = append(r.stages, si)
				}
				return r
			}
		}
	}
}

func printSummary(results map[string]*teamResult) {
	fmt.Println("\n📊 测试汇总:")
	fmt.Printf("  %-15s %-10s %-10s %s\n", "工作流", "状态", "耗时", "阶段")
	fmt.Println(strings.Repeat("-", 80))

	for _, name := range []string{"code-review", "testing", "parenting", "hiring"} {
		r, ok := results[name]
		if !ok {
			fmt.Printf("  %-15s %-10s %-10s 未执行\n", name, "skipped", "-")
			continue
		}
		dur := "-"
		if r.duration > 0 {
			dur = r.duration.Round(time.Second).String()
		}
		nStages := len(r.stages)
		fmt.Printf("  %-15s %-10s %-10s %d 阶段\n", name, r.status, dur, nStages)

		for _, s := range r.stages {
			fmt.Printf("    ▸ %-25s %s (%d chars)\n", s.name, s.status, s.outputLen)
			if s.errMsg != "" {
				fmt.Printf("      错误: %s\n", truncate(s.errMsg, 80))
			}
		}
	}

	passed := 0
	for _, r := range results {
		if r.status == "completed" {
			passed++
		}
	}
	fmt.Printf("\n通过: %d/%d\n", passed, len(results))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

type llmRunner struct {
	api          *api.Client
	systemPrompt string
	role         string
}

func (r *llmRunner) Name() string { return r.role }

func (r *llmRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	result, err := r.api.SimpleComplete(ctx, r.systemPrompt, userPrompt)
	if err != nil {
		return "", fmt.Errorf("LLM 调用失败 (%s): %w", r.role, err)
	}
	return result, nil
}
