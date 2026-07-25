package dashboard

// l5_control_plane.go — design/02 §3.5 L5 用户层: dashboard → 控制面的出站配置。
//
// 修的是设计稿点名的这一条: 「dashboard→控制面仍以惰性回调为主路径且 BotAPIURL
// **硬编码回环**」。cmd/claude-go 里的 botAPIURL() 按 wiki.apiPort 拼
// "http://127.0.0.1:<port>", 于是只读 dashboard 与控制面必须同机 —— T2/T3
// (单机多进程 / 多机 K8s) 形态下 dashboard 永远找不到控制面。
//
// 这里补的是**配置通道**而不是改默认值:
//   - 未设环境变量时, 行为与此前完全一致 (仍用 Config.BotAPIURL, 即回环);
//   - 设了 CLAUDE_GO_BOT_API_URL 就打那个地址, 跨进程/跨机即可用。
//
// 为什么用环境变量而不是加 Config 字段: Config 字段得由 cmd/claude-go 填,
// 而那一层同时是 dashboard 的五个子命令 + 飞书挂载两条装配路径; 环境变量对
// systemd/K8s 清单是一等公民 (12-factor), 且两条装配路径同时生效, 不会漏一条。

import (
	"os"
	"strings"
)

// 控制面地址与凭据的环境变量名。
const (
	// EnvBotAPIURL 覆盖控制面 (飞书 bot 的 :18080) 地址。
	EnvBotAPIURL = "CLAUDE_GO_BOT_API_URL"
	// EnvBotAPIToken 覆盖转发时携带的 Bearer token。
	// 控制面侧启用 wiki.apiSecret 后必须设置, 否则转发 401。
	EnvBotAPIToken = "CLAUDE_GO_BOT_API_TOKEN"
)

// botAPIBase 返回控制面基址 (无尾斜杠); 空 = 未配置, 调用方按"无控制面"处理。
// 优先级: 环境变量 > Config.BotAPIURL。
func (s *Server) botAPIBase() string {
	if v := strings.TrimSpace(os.Getenv(EnvBotAPIURL)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return strings.TrimRight(strings.TrimSpace(s.cfg.BotAPIURL), "/")
}

// botAPIToken 返回转发凭据; 优先级同 botAPIBase。
func (s *Server) botAPIToken() string {
	if v := strings.TrimSpace(os.Getenv(EnvBotAPIToken)); v != "" {
		return v
	}
	return s.cfg.BotAPIToken
}
