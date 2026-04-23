//go:build ignore

// test_parenting.go — 轻量级测试: 创建并运行 parenting 团队。
// 用法: cd /home/victor/base/git/temp/ruflo/claude-go && go run test_parenting.go

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
	apiKey    = "sk-sp-b0a692b1b8384b72971fe4d3a42798a1"
	baseURL   = "https://coding.dashscope.aliyuncs.com/apps/anthropic/v1"
	model     = "qwen3.6-plus"
	stateDir  = "/home/victor/.claude-go"
	cwd       = "/home/victor/base/git/gitee/claudeGo"
	chatID    = "test-cli-parenting"
	objective = "孩子8岁，数学成绩下降，上课注意力不集中，在家写作业总是拖延，家长很焦虑，请给综合建议。"
)

func main() {
	// 解析 stateDir
	sd := basedir.ResolveDefault(stateDir, cwd)

	// 初始化日志
	logDir := sd + "/logs"
	os.MkdirAll(logDir, 0o755)
	logging.Init(&logging.LogConfig{Dir: logDir})

	// AI 客户端
	apiClient := api.NewClient(baseURL, apiKey, model)

	// 通知回调
	notifyFn := func(chatID, msg string) {
		lines := strings.Split(msg, "\n")
		for _, line := range lines {
			log.Printf("[Notify] %s", line)
		}
	}

	// LLM Runner Factory
	factory := func(ctx context.Context, role, systemPrompt string) (agent.AgentRunner, error) {
		return &llmRunner{api: apiClient, systemPrompt: systemPrompt, role: role}, nil
	}

	// 任务追踪器
	taskStore := builtin.NewTaskStore(sd + "/.dashboard/tasks")

	// 角色注册表
	roles := agent.NewRoleRegistry(sd + "/roles")

	// Agent 池
	pool := agent.NewAgentPool(factory, 8)

	// 团队管理器
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

	teamName := fmt.Sprintf("parenting-test-%d", time.Now().Unix())

	fmt.Printf("🚀 创建团队: %s\n", teamName)
	fmt.Printf("   工作流: parenting\n")
	fmt.Printf("   目标: %s\n", objective)

	team, err := mgr.CreateTeam(teamName, "parenting", objective, chatID)
	if err != nil {
		log.Fatalf("创建团队失败: %v", err)
	}
	fmt.Printf("   Agent 数: %d\n", len(team.Agents))

	if err := mgr.RunTeam(teamName, objective); err != nil {
		log.Fatalf("启动团队失败: %v", err)
	}

	// 监控执行
	fmt.Println("⏳ 等待执行完成 (最长 15 分钟)...")
	deadline := time.Now().Add(15 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t := mgr.GetTeam(teamName)
			if t == nil {
				log.Fatal("团队丢失!")
			}
			age := time.Since(t.StartedAt).Round(time.Second)
				// orchestrated 模式下 stages 只在完成时填充, 用状态显示进度
				if t.Status == agent.TeamStatusRunning {
					fmt.Printf("  [%s] 状态=running\n", age)
				} else {
					fmt.Printf("  [%s] 状态=%s 阶段数=%d\n", age, t.Status, len(t.Stages))
				}

			switch t.Status {
			case agent.TeamStatusCompleted:
				fmt.Println("✅ 团队执行完成!")
				printStageResults(t)
				return
			case agent.TeamStatusFailed:
				fmt.Printf("❌ 团队执行失败: %s\n", t.Error)
				printStageResults(t)
				return
			}

			if time.Now().After(deadline) {
				fmt.Println("⏱ 超时，停止团队")
				mgr.StopTeam(teamName)
				printStageResults(mgr.GetTeam(teamName))
				return
			}
		}
	}
}

func printStageResults(t *agent.ProductionTeam) {
	for _, s := range t.Stages {
		status := string(s.Status)
		outputLen := len(s.Output)
		fmt.Printf("  ▸ %s: %s (%d chars)\n", s.Name, status, outputLen)
		if s.Error != "" {
			errMsg := s.Error
			if len(errMsg) > 120 {
				errMsg = errMsg[:120] + "..."
			}
			fmt.Printf("    错误: %s\n", errMsg)
		}
	}
}

// llmRunner 实现 agent.AgentRunner 接口
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
