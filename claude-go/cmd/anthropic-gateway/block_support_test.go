// block_support_test.go — validateBlockSupport: 翻译协议丢弃块之前必须 400。
//
// 背景. pkg/api 的翻译器 (openai.go / responses.go) 对承载不了的 user 块已会响
// 错误, 但网关侧这道预校验是客户端真正看到的拒绝: 错误信息带着解析后的模型别名
// 与协议, 且在写任何响应头之前 400 —— 流式路径的错误文本到不了 claude CLI
// (streamMessages 只补 message_stop), 没有这道闸, 用户看到的是一条没头没尾的
// 截断流, 而不是"这个模型看不了 PDF"。
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func userMsg(t *testing.T, content string) []types.APIMessage {
	t.Helper()
	return []types.APIMessage{{Role: "user", Content: json.RawMessage(content)}}
}

// TestValidateBlockSupport_PassthroughUnvalidated anthropic 透传不做校验:
// 块原样到达上游, 上游的 400 是事实信号, 网关不该 preempt。
func TestValidateBlockSupport_PassthroughUnvalidated(t *testing.T) {
	for _, proto := range []string{"", "anthropic"} {
		msgs := userMsg(t, `[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QUJD"}},{"type":"text","text":"读这个 PDF"}]`)
		if err := validateBlockSupport(msgs, proto, "opencode:claude-haiku-5-5"); err != nil {
			t.Fatalf("协议 %q 不该拦截 document 块: %v", proto, err)
		}
	}
}

// TestValidateBlockSupport_DocumentRejectedOnTranslated 两种翻译协议都拒绝
// document (PDF) 块, 且错误信息里能同时看到模型别名、协议与块类型 —— 缺任何一个,
// 排障时都得再翻一层日志。
func TestValidateBlockSupport_DocumentRejectedOnTranslated(t *testing.T) {
	for _, proto := range []string{"openai", "openai-responses"} {
		msgs := userMsg(t, `[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QUJD"}},{"type":"text","text":"读这个 PDF"}]`)
		err := validateBlockSupport(msgs, proto, "opencode:glm-5.3-flash")
		if err == nil {
			t.Fatalf("协议 %q 必须拒绝 document 块", proto)
		}
		for _, want := range []string{"opencode:glm-5.3-flash", proto, "document"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("错误信息缺 %q: %v", want, err)
			}
		}
	}
}

// TestValidateBlockSupport_ImageAcceptedOnTranslated 图片块在两种翻译协议上都放行
// (openai.go 译 image_url, responses.go 译 input_image) —— 校验器不能把
// 翻译器已支持的块误杀。
func TestValidateBlockSupport_ImageAcceptedOnTranslated(t *testing.T) {
	img := `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aVBORw0KGgo="}},{"type":"text","text":"什么颜色?"}]`
	for _, proto := range []string{"openai", "openai-responses"} {
		if err := validateBlockSupport(userMsg(t, img), proto, "opencode:glm-5.3-flash"); err != nil {
			t.Fatalf("协议 %q 不该拦截 image 块: %v", proto, err)
		}
	}
}

// TestValidateBlockSupport_AssistantHistoryIgnored 校验只看 user 消息:
// assistant 历史里的未知块 (redacted_thinking 等) 由翻译器的 assistant 路径
// 保持旧行为丢弃, 不能在这里被误杀 —— 否则本来能续跑的会话会 400。
func TestValidateBlockSupport_AssistantHistoryIgnored(t *testing.T) {
	msgs := []types.APIMessage{
		{Role: "assistant", Content: json.RawMessage(`[{"type":"redacted_thinking","data":"AAAA"},{"type":"text","text":"ok"}]`)},
		{Role: "user", Content: json.RawMessage(`"plain string content"`)},
	}
	if err := validateBlockSupport(msgs, "openai", "opencode:mimo-v2.5"); err != nil {
		t.Fatalf("assistant 历史与字符串 content 不该被校验: %v", err)
	}
}
