package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/anthropic/claude-go/pkg/api"
)

// 飞书消息长度限制 (富文本卡片约 30KB, 普通文本约 4000 字符)
const (
	maxTextMessageLen = 4000
	maxCardContentLen = 28000
)

// Bot 飞书机器人。
// 通过 WebSocket 长连接接收飞书消息事件，
// 将用户消息桥接到 claude-go 的 QueryEngine，
// 然后将 AI 回复通过飞书 IM API 发送给用户。
//
// 核心流程:
//  1. Start() 建立 WebSocket 长连接
//  2. 飞书推送 im.message.receive_v1 事件
//  3. handleMessage() 提取消息文本
//  4. SessionManager 获取/创建会话
//  5. QueryEngine 处理消息 (异步 goroutine)
//  6. sendReply() 将结果回复到飞书
//
// 关键约束:
//   - 事件回调必须 3 秒内返回 → 异步处理
//   - 群聊中默认仅响应 @机器人 的消息
//   - 支持 /clear 和 /help 等斜杠命令
type Bot struct {
	config    *BotConfig
	client    *lark.Client       // 飞书 API 客户端 (用于发送消息)
	wsClient  *larkws.Client     // WebSocket 长连接客户端
	sessions  *SessionManager    // 会话管理器
	apiClient *api.Client        // AI API 客户端
	startTime time.Time          // 启动时间
}

// NewBot 创建飞书机器人。
// 参数:
//   - config: 机器人配置 (AppID, AppSecret, AI API 配置等)
//
// 初始化流程:
//  1. 创建飞书 API 客户端 (用于主动发消息)
//  2. 创建 AI API 客户端
//  3. 创建 SessionManager
//  4. 注册消息事件处理器
//  5. 创建 WebSocket 长连接客户端
func NewBot(config *BotConfig) (*Bot, error) {
	if config.AppID == "" || config.AppSecret == "" {
		return nil, fmt.Errorf("飞书 AppID 和 AppSecret 不能为空")
	}
	if config.APIKey == "" {
		return nil, fmt.Errorf("AI API Key 不能为空")
	}

	// 确定飞书 API 域名
	var domain string
	switch config.Domain {
	case "lark":
		domain = "https://open.larksuite.com"
	default:
		domain = "https://open.feishu.cn"
	}

	// 创建飞书 API 客户端 (用于发送消息)
	larkClient := lark.NewClient(config.AppID, config.AppSecret,
		lark.WithOpenBaseUrl(domain),
	)

	// 创建 AI API 客户端
	var aiClient *api.Client
	if config.BaseURL != "" {
		aiClient = api.NewClient(config.BaseURL, config.APIKey, config.Model)
	} else {
		aiClient = api.NewDashScopeClient(config.APIKey, config.Model)
	}

	bot := &Bot{
		config:    config,
		client:    larkClient,
		apiClient: aiClient,
		startTime: time.Now(),
	}

	// 创建会话管理器
	bot.sessions = NewSessionManager(config, aiClient)

	// 注册飞书事件处理器
	// dispatcher.NewEventDispatcher 的两个参数(verificationToken, encryptKey)
	// 在长连接模式下必须填空字符串 (SDK 内部处理加密鉴权)
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(bot.onMessageReceive)

	// 创建 WebSocket 长连接客户端
	logLevel := larkcore.LogLevelInfo
	if config.Debug {
		logLevel = larkcore.LogLevelDebug
	}

	var wsOpts []larkws.ClientOption
	wsOpts = append(wsOpts, larkws.WithEventHandler(eventHandler))
	wsOpts = append(wsOpts, larkws.WithLogLevel(logLevel))

	// 飞书国内使用 FeishuBaseUrl, 国际版使用 LarkBaseUrl
	if config.Domain == "lark" {
		wsOpts = append(wsOpts, larkws.WithDomain(lark.LarkBaseUrl))
	} else {
		wsOpts = append(wsOpts, larkws.WithDomain(lark.FeishuBaseUrl))
	}

	bot.wsClient = larkws.NewClient(config.AppID, config.AppSecret, wsOpts...)

	return bot, nil
}

// Start 启动飞书机器人 (阻塞)。
// 建立 WebSocket 长连接，开始接收事件。
// 连接成功后会打印 "connected to wss://..."
// 主线程阻塞直到 context 取消或连接断开。
func (b *Bot) Start(ctx context.Context) error {
	log.Printf("[飞书Bot] 启动中... AppID=%s, Domain=%s, Model=%s",
		b.config.AppID, b.config.Domain, b.config.Model)
	log.Printf("[飞书Bot] 工作目录: %s", b.config.Cwd)
	log.Printf("[飞书Bot] 会话超时: %v, 最大会话数: %d",
		b.config.SessionTimeout, b.config.MaxSessions)

	return b.wsClient.Start(ctx)
}

// onMessageReceive 处理飞书消息接收事件。
// 对应事件: im.message.receive_v1
//
// 处理流程:
//  1. 提取消息元数据 (chat_id, message_id, sender_id)
//  2. 判断消息类型 (仅处理文本消息)
//  3. 群聊中检查是否 @机器人
//  4. 解析斜杠命令 (/clear, /help, /status)
//  5. 异步处理: goroutine 中调用 SessionManager.ProcessMessage()
//  6. 发送处理中提示 → 等待结果 → 发送回复
//
// 注意: 此回调必须在 3 秒内返回, 所以 AI 处理放在 goroutine 中
func (b *Bot) onMessageReceive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}

	msg := event.Event.Message
	sender := event.Event.Sender

	chatID := deref(msg.ChatId)
	messageID := deref(msg.MessageId)
	msgType := deref(msg.MessageType)
	chatType := deref(msg.ChatType)
	senderID := ""
	if sender != nil && sender.SenderId != nil {
		senderID = deref(sender.SenderId.OpenId)
	}

	if b.config.Debug {
		log.Printf("[飞书Bot] 收到消息: chat=%s, sender=%s, type=%s, chatType=%s",
			chatID, senderID, msgType, chatType)
	}

	// 仅处理文本消息
	if msgType != "text" {
		return nil
	}

	// 提取文本内容
	content := deref(msg.Content)
	var textContent FeishuTextContent
	if err := json.Unmarshal([]byte(content), &textContent); err != nil {
		log.Printf("[飞书Bot] 解析消息内容失败: %v", err)
		return nil
	}

	userText := strings.TrimSpace(textContent.Text)

	// 群聊中检查 @机器人
	// 飞书 @mention 格式: "@_user_xxx " 前缀
	if chatType == "group" && b.config.MentionOnly {
		mentions := msg.Mentions
		if len(mentions) == 0 {
			return nil // 群聊中未 @ 机器人，忽略
		}
		// 移除 @mention 文本 (格式: @_user_N )
		for _, mention := range mentions {
			if mention.Key != nil {
				userText = strings.ReplaceAll(userText, deref(mention.Key), "")
			}
		}
		userText = strings.TrimSpace(userText)
	}

	if userText == "" {
		return nil
	}

	// 处理斜杠命令
	if handled := b.handleSlashCommand(ctx, chatID, messageID, userText); handled {
		return nil
	}

	// 异步处理消息 (避免阻塞飞书回调的 3 秒超时)
	go b.processAndReply(chatID, messageID, userText)

	return nil
}

// handleSlashCommand 处理斜杠命令
// 支持的命令:
//   - /clear: 清除当前会话历史
//   - /help:  显示帮助信息
//   - /status: 显示机器人状态
func (b *Bot) handleSlashCommand(ctx context.Context, chatID, messageID, text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))

	switch {
	case lower == "/clear" || lower == "/reset":
		b.sessions.ClearSession(chatID)
		b.sendTextReply(ctx, messageID, "对话已清除，开始新的会话。")
		return true

	case lower == "/help":
		help := "**Claude Code (Go) 飞书机器人**\n\n" +
			"直接发送消息与我对话，我可以:\n" +
			"- 编写和分析代码\n" +
			"- 执行 Shell 命令\n" +
			"- 读写和编辑文件\n" +
			"- 搜索代码库\n\n" +
			"**命令:**\n" +
			"- /clear - 清除对话历史\n" +
			"- /help - 显示此帮助\n" +
			"- /status - 查看运行状态"
		b.sendTextReply(ctx, messageID, help)
		return true

	case lower == "/status":
		total, active := b.sessions.Stats()
		uptime := time.Since(b.startTime).Round(time.Second)
		status := fmt.Sprintf("**运行状态**\n"+
			"- 运行时长: %v\n"+
			"- 总会话数: %d\n"+
			"- 活跃处理: %d\n"+
			"- AI 模型: %s\n"+
			"- 工作目录: %s",
			uptime, total, active, b.config.Model, b.config.Cwd)
		b.sendTextReply(ctx, messageID, status)
		return true
	}

	return false
}

// processAndReply 异步处理消息并发送回复。
// 此方法运行在独立的 goroutine 中:
//  1. 发送「正在思考」提示消息
//  2. 调用 SessionManager.ProcessMessage() 执行 AI 推理
//  3. 将结果分段发送回飞书 (考虑消息长度限制)
//  4. 如果出错，发送错误提示
func (b *Bot) processAndReply(chatID, messageID, userText string) {
	ctx := context.Background()

	// 发送「正在思考」提示
	if b.config.ThinkingMessage != "" {
		b.sendTextReply(ctx, messageID, b.config.ThinkingMessage)
	}

	// 处理消息
	response, err := b.sessions.ProcessMessage(ctx, chatID, userText)
	if err != nil {
		log.Printf("[飞书Bot] 处理消息失败: chat=%s, err=%v", chatID, err)
		b.sendTextMessage(ctx, chatID, fmt.Sprintf("处理失败: %v", err))
		return
	}

	// 分段发送 (飞书文本消息有长度限制)
	b.sendLongMessage(ctx, chatID, response)
}

// sendTextReply 回复指定消息 (引用回复)
func (b *Bot) sendTextReply(ctx context.Context, messageID, text string) {
	content, _ := json.Marshal(FeishuTextContent{Text: text})

	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()

	resp, err := b.client.Im.Message.Reply(ctx, req)
	if err != nil {
		log.Printf("[飞书Bot] 回复消息失败: %v", err)
		return
	}
	if !resp.Success() {
		log.Printf("[飞书Bot] 回复消息失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
}

// sendTextMessage 发送文本消息到指定 chat
func (b *Bot) sendTextMessage(ctx context.Context, chatID, text string) {
	content, _ := json.Marshal(FeishuTextContent{Text: text})

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType("text").
			ReceiveId(chatID).
			Content(string(content)).
			Build()).
		Build()

	resp, err := b.client.Im.Message.Create(ctx, req)
	if err != nil {
		log.Printf("[飞书Bot] 发送消息失败: %v", err)
		return
	}
	if !resp.Success() {
		log.Printf("[飞书Bot] 发送消息失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
}

// sendCardMessage 发送卡片消息 (用于富文本/代码块)
func (b *Bot) sendCardMessage(ctx context.Context, chatID, title, markdownContent string) {
	card := FeishuCardContent{
		Config: &CardConfig{WideScreenMode: true},
		Header: &CardHeader{
			Title:    &CardText{Content: title, Tag: "plain_text"},
			Template: "blue",
		},
		Elements: []CardElement{
			{
				Tag: "markdown",
				Content: markdownContent,
			},
		},
	}

	content, _ := json.Marshal(card)

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType("interactive").
			ReceiveId(chatID).
			Content(string(content)).
			Build()).
		Build()

	resp, err := b.client.Im.Message.Create(ctx, req)
	if err != nil {
		log.Printf("[飞书Bot] 发送卡片消息失败: %v", err)
		return
	}
	if !resp.Success() {
		log.Printf("[飞书Bot] 发送卡片消息失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
}

// sendLongMessage 发送长消息 (自动分段)。
// 飞书文本消息有 ~4000 字符限制。
// 对于超长内容，先尝试卡片消息 (限制更高)，
// 如仍超限则按段落分割发送多条消息。
func (b *Bot) sendLongMessage(ctx context.Context, chatID, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}

	// 短消息直接发送文本
	if len(content) <= maxTextMessageLen {
		b.sendTextMessage(ctx, chatID, content)
		return
	}

	// 中等长度用卡片消息
	if len(content) <= maxCardContentLen {
		b.sendCardMessage(ctx, chatID, "Claude Code", content)
		return
	}

	// 超长消息: 分段发送
	chunks := splitMessage(content, maxCardContentLen)
	for i, chunk := range chunks {
		title := fmt.Sprintf("Claude Code (%d/%d)", i+1, len(chunks))
		b.sendCardMessage(ctx, chatID, title, chunk)
		// 避免发送过快触发限流
		if i < len(chunks)-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// splitMessage 按段落边界分割长文本
func splitMessage(content string, maxLen int) []string {
	if len(content) <= maxLen {
		return []string{content}
	}

	var chunks []string
	remaining := content

	for len(remaining) > 0 {
		if len(remaining) <= maxLen {
			chunks = append(chunks, remaining)
			break
		}

		// 在 maxLen 范围内找最近的段落边界
		cutPoint := maxLen
		if idx := strings.LastIndex(remaining[:maxLen], "\n\n"); idx > maxLen/2 {
			cutPoint = idx + 2
		} else if idx := strings.LastIndex(remaining[:maxLen], "\n"); idx > maxLen/2 {
			cutPoint = idx + 1
		}

		chunks = append(chunks, remaining[:cutPoint])
		remaining = remaining[cutPoint:]
	}

	return chunks
}

// deref 安全解引用字符串指针
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
