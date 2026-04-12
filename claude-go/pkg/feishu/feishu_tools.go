package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// FeishuSendFileTool 让 LLM 能通过飞书 SDK 向当前对话发送图片或文件。
// 每个实例绑定一个 chatID 和 bot 的发送方法。
type FeishuSendFileTool struct {
	chatID string
	sendFn func(ctx context.Context, chatID string, data []byte, filename, mediaType string) error
}

type feishuSendInput struct {
	FilePath  string `json:"file_path"`
	MediaType string `json:"media_type,omitempty"` // "image" or "file", default auto-detect
}

// NewFeishuSendFileTool 创建绑定到特定 chatID 的发送工具。
func NewFeishuSendFileTool(chatID string, sendFn func(ctx context.Context, chatID string, data []byte, filename, mediaType string) error) *FeishuSendFileTool {
	return &FeishuSendFileTool{chatID: chatID, sendFn: sendFn}
}

func (t *FeishuSendFileTool) Name() string { return "FeishuSendFile" }

func (t *FeishuSendFileTool) Description() string {
	return `通过飞书发送本地文件或图片给用户。支持图片(png/jpg/gif/svg)和任意文件。` +
		`file_path 为服务器上的绝对路径。` +
		`media_type 可选: "image"(图片)、"file"(文件)，留空则自动检测。`
}

func (t *FeishuSendFileTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {"type": "string", "description": "服务器上的文件绝对路径"},
			"media_type": {"type": "string", "enum": ["image", "file"], "description": "发送类型: image=图片, file=文件。留空自动检测。"}
		},
		"required": ["file_path"]
	}`)
}

func (t *FeishuSendFileTool) IsReadOnly(_ json.RawMessage) bool          { return true }
func (t *FeishuSendFileTool) IsConcurrencySafe(_ json.RawMessage) bool   { return true }
func (t *FeishuSendFileTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *FeishuSendFileTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in feishuSendInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("参数解析失败: %v", err), IsError: true}, nil
	}
	if in.FilePath == "" {
		return &tool.ToolResult{Content: "file_path 不能为空", IsError: true}, nil
	}

	data, err := os.ReadFile(in.FilePath)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("读取文件失败: %v", err), IsError: true}, nil
	}
	if len(data) == 0 {
		return &tool.ToolResult{Content: "文件为空", IsError: true}, nil
	}

	filename := filepath.Base(in.FilePath)
	mediaType := in.MediaType
	if mediaType == "" {
		ext := strings.ToLower(filepath.Ext(filename))
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp":
			mediaType = "image"
		default:
			mediaType = "file"
		}
	}

	if err := t.sendFn(ctx, t.chatID, data, filename, mediaType); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("发送失败: %v", err), IsError: true}, nil
	}

	return &tool.ToolResult{Content: fmt.Sprintf("已通过飞书发送 %s (%s, %d bytes)", filename, mediaType, len(data))}, nil
}
