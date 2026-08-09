//go:build codex_smoke

package feishu

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
)

func TestCodexOptimizedFeishuScenarios(t *testing.T) {
	cfgPath := os.Getenv("CLAUDE_GO_SMOKE_CONFIG")
	if cfgPath == "" {
		t.Skip("CLAUDE_GO_SMOKE_CONFIG is not set")
	}

	jsonCfg, err := LoadJSONConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := DefaultBotConfig()
	jsonCfg.ApplyToBot(cfg)
	cfg.Wiki.APIPort = 0
	cfg.ThinkingMessage = ""

	bot, err := NewBot(cfg)
	if err != nil {
		t.Fatalf("new bot: %v", err)
	}
	t.Cleanup(func() {
		if bot.mcpMgr != nil {
			bot.mcpMgr.Shutdown()
		}
	})

	agentPool := agent.NewAgentPool(bot.sessions.CreateAgentRunner, 8)
	bot.teamMgr = agent.NewProductionTeamManager(agent.TeamManagerConfig{
		BaseDir: bot.layout.Teams,
		Cwd:     cfg.Cwd,
		Factory: bot.sessions.CreateAgentRunner,
		Notify:  func(chatID, msg string) { t.Logf("team notify [%s]: %.180s", chatID, msg) },
		MediaNotify: func(chatID string, data []byte, filename, mediaType string) error {
			t.Logf("media notify [%s]: %s (%s, %d bytes)", chatID, filename, mediaType, len(data))
			return nil
		},
		TaskTracker:        &dagTaskAdapter{store: bot.taskStore},
		Pool:               agentPool,
		LLM:                bot.apiClient,
		Evolution:          bot.evolution,
		Dreamer:            &dreamAdapter{dreamer: bot.dreamer},
		Roles:              bot.sessions.roleRegistry,
		PlanConfigResolver: agent.NewPlanConfigResolver(bot.modelResolver),
	})
	bot.teamMgr.SetMemoryWriter(&memoryAdapter{store: bot.memStore})
	bot.sessions.SetTeamManager(bot.teamMgr)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	chatID := "codex-smoke-optimized-6"

	analysisText := "帮我分析 claude-go 的飞书 Bot 架构，并给出改造计划。"
	// 复杂任务自动 Plan Mode 已内建于引擎 (AutoModeHook), 无需再注入 [AutoPlanBuild] 前缀。
	response, err := bot.sessions.ProcessMessage(ctx, chatID, analysisText)
	if err != nil {
		t.Fatalf("analysis message: %v", err)
	}
	t.Logf("analysis response chars=%d", len(response))

	objective := "开发个golang ToDo 应用，输出到工作目录下"
	teamName := fmt.Sprintf("codex-smoke-development-opt6-%d", time.Now().Unix()%100000)
	team, err := bot.teamMgr.CreateTeam(teamName, "development", objective, chatID)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	bot.injectSessionContext(chatID, team)
	if err := bot.teamMgr.RunTeam(teamName, objective); err != nil {
		t.Fatalf("run team: %v", err)
	}

	deadline := time.After(25 * time.Minute)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			_ = bot.teamMgr.StopTeam(teamName)
			t.Fatalf("team %s timed out", teamName)
		case <-ticker.C:
			current := bot.teamMgr.GetTeam(teamName)
			if current == nil {
				t.Fatalf("team %s disappeared", teamName)
			}
			t.Logf("team status=%s progress=%d/%d", current.Status, completedAgentCount(current), len(current.Agents))
			switch current.Status {
			case agent.TeamStatusCompleted, agent.TeamStatusDeliveredWithRemediation:
				return
			case agent.TeamStatusFailed, agent.TeamStatusStopped:
				t.Fatalf("team ended with status=%s error=%s", current.Status, current.Error)
			}
		}
	}
}

func completedAgentCount(team *agent.ProductionTeam) int {
	if team == nil {
		return 0
	}
	done := 0
	for _, ag := range team.Agents {
		if ag.Status == agent.AgentStatusCompleted {
			done++
		}
	}
	return done
}
