package feishu

// worker_runtime.go —— 把 Bot 装配好的 agent 执行体暴露给独立 worker 进程
// (design/02 §3.3 L3)。
//
// 单独一个文件而不是往 bot.go 里加: bot.go 是 2000+ 行的装配中心, 多个改动方向
// 同时在动它; 这里只有两个只读访问器, 放新文件零冲突。
//
// 为什么 worker 需要经 Bot 拿工厂: §3.3 列的 worker 职责 (pkg/engine 的
// QueryEngine loop + pkg/prompt 的系统提示词组装 + pkg/skills + pkg/tool 四档
// 工具画像 + pkg/permissions + pkg/sandbox + MCP) 全部装配在 SessionManager 里,
// 而 `SessionManager.CreateAgentRunner` 就是它们的唯一出口。worker 复用它 =
// "远程执行与本地执行是同一条执行路径"; 自己再拼一套只会得到第二种行为。

import "github.com/anthropic/claude-go/pkg/agent"

// AgentFactory 返回 agent 执行工厂 (即 SessionManager.CreateAgentRunner)。
// Bot 未装配会话管理器时返回 nil, 调用方据此拒绝启动 (不要静默降级)。
func (b *Bot) AgentFactory() agent.CreateAgentFunc {
	if b == nil || b.sessions == nil {
		return nil
	}
	return b.sessions.CreateAgentRunner
}

// AgentRuntime 把工厂收编成 AgentRuntime (agent.NewLocalRuntime 的便捷包装)。
// name 建议以 "local-" 打头, 让 Placement 的 Prefer:"local" 生效。
func (b *Bot) AgentRuntime(name string, caps agent.RuntimeCaps) agent.AgentRuntime {
	f := b.AgentFactory()
	if f == nil {
		return nil
	}
	return agent.NewLocalRuntime(name, caps, f)
}
