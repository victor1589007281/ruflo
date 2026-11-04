// modality.go — 模态能力协商（Moqi R-H11）。
//
// # 要解决的问题
//
// 改造前的行为是：给任何模型发 image 块，请求照常发出。支持图片的模型正常回，
// 不支持的模型要么 400（业务层看到的是一条无上下文的模型错误），要么**把图片
// 当空内容忽略** —— 后者最坏：用户传了病例照片，模型客气地回了一句没看图的话，
// 没有任何信号说明它没看。
//
// 这里在**发请求之前**做一次能力判定，不支持就返回 typed error（ErrKindModality），
// 业务层据此显式降级（"我看不到图片，请描述一下"）或换模型，而不是静默当文本。
//
// # 为什么默认表是"保守白名单"
//
// 表的默认值只包含**确认支持**视觉的模型族。一个未知模型默认判为不支持图片，
// 于是新模型上线时最坏情况是"明确报错、用户被要求描述"，而不是"静默忽略图片、
// 用户以为被看见了"。前者可修，后者是数据质量事故。
package harness

import (
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// modalityPolicy 描述一类输入能否发给某模型。
type modalityPolicy struct {
	image    bool
	document bool
	audio    bool
}

// defaultModalityTable 是内置的保守能力表，按模型名子串匹配（小写）。
//
// 只列确认支持视觉的族。可经 RunnerOptions.Modality 整体覆盖 —— 装配方比这张表
// 更了解自己的部署。
var defaultModalityTable = []struct {
	match  []string
	policy modalityPolicy
}{
	// Anthropic Claude 全系支持图片与 PDF（document）。
	{[]string{"claude"}, modalityPolicy{image: true, document: true}},
	// OpenAI 视觉模型。
	{[]string{"gpt-4", "gpt-5"}, modalityPolicy{image: true, document: false}},
	// 智谱 GLM-4V 系列。
	{[]string{"glm-4v", "glm-4.5v"}, modalityPolicy{image: true}},
	// Google Gemini 支持图片与 PDF。
	{[]string{"gemini"}, modalityPolicy{image: true, document: true}},
	// Qwen-VL 系列。
	{[]string{"qwen-vl", "qwen2-vl", "qwen2.5-vl"}, modalityPolicy{image: true}},
	// Kimi 视觉模型（kimi-latest-vision / moonshot-v1-vision）。
	{[]string{"vision"}, modalityPolicy{image: true}},
	// DeepSeek 当前文本模型不支持图片输入。
	{[]string{"deepseek"}, modalityPolicy{}},
}

// lookupModality 返回 (policy, known)。
//
// "known=false" 与 "支持=false" 是两件事：前者是"这张表不认识它"，后者是"知道它
// 不支持"。日志里必须能分开，否则加新模型时看不出是表没更新还是真的不支持。
func lookupModality(model string) (modalityPolicy, bool) {
	m := strings.ToLower(model)
	for _, e := range defaultModalityTable {
		for _, pat := range e.match {
			if strings.Contains(m, pat) {
				return e.policy, true
			}
		}
	}
	return modalityPolicy{}, false
}

// modalityOf 判定某模型是否支持某类输入。
//
// 顺序：宿主覆盖 → 内置表 → 保守默认（不支持）。
func modalityOf(model, kind string, override func(model, kind string) bool) bool {
	if override != nil {
		return override(model, kind)
	}
	p, _ := lookupModality(model)
	switch kind {
	case AttachmentImage:
		return p.image
	case AttachmentPDF:
		return p.document
	case AttachmentAudio:
		return p.audio
	case AttachmentText:
		// 纯文本输入任何模型都支持；不支持文本的模型不存在。
		return true
	}
	// 未知种类：保守判不支持，理由同文件头。
	return false
}

// checkModalities 在发请求前校验本轮输入里的模态。
//
// model 应当是**本轮实际会用的模型**：调用方有角色覆盖就用覆盖值，否则用
// engine 的默认模型（见 runner.go 的 effectiveModel）。改造前这里在 model=="" 时
// 直接放行，理由是"不知道默认模型就不该拒绝" —— 但 Runner 明明能从
// Engine.Config.Model 读到它，于是每个没配 ModelRoles 的部署都静默跳过了整段
// 能力协商：带图片的请求被发给纯文本模型，用户以为图片被看到了。
//
// 真的解析不出模型（Engine 也未配置 Model）时仍然不能凭空拒绝**纯文本**轮：
// 那类请求本来就不需要能力判定。但只要输入里带非文本内容，"未知"就必须按
// 不支持处理 —— 保守白名单的全部意义就在于此。
func checkModalities(model string, blocks []types.ContentBlock, atts []AttachmentRef, override func(model, kind string) bool) error {
	if model == "" {
		if hasNonTextInput(blocks, atts) {
			return fmt.Errorf("%w: 无法确定本轮模型（engine 未配置 Model），拒绝发送图片/文档/音频", ErrUnsupportedModality)
		}
		return nil
	}
	for _, b := range blocks {
		switch b.Type {
		case types.ContentBlockImage:
			if !modalityOf(model, AttachmentImage, override) {
				return fmt.Errorf("%w: 模型 %s 不接受图片输入", ErrUnsupportedModality, model)
			}
		case types.ContentBlockDocument:
			if !modalityOf(model, AttachmentPDF, override) {
				return fmt.Errorf("%w: 模型 %s 不接受文档输入", ErrUnsupportedModality, model)
			}
		}
	}
	for _, a := range atts {
		switch a.Kind {
		case AttachmentImage, AttachmentPDF, AttachmentAudio:
			if !modalityOf(model, a.Kind, override) {
				return fmt.Errorf("%w: 模型 %s 不接受 %s 附件（%s）",
					ErrUnsupportedModality, model, a.Kind, a.Name)
			}
		}
	}
	return nil
}

// hasNonTextInput 判断本轮输入里是否含需要能力协商的内容。
func hasNonTextInput(blocks []types.ContentBlock, atts []AttachmentRef) bool {
	for _, b := range blocks {
		switch b.Type {
		case types.ContentBlockImage, types.ContentBlockDocument:
			return true
		}
	}
	for _, a := range atts {
		switch a.Kind {
		case AttachmentImage, AttachmentPDF, AttachmentAudio:
			return true
		}
	}
	return false
}

// ErrUnsupportedModality 是模态不支持的哨兵错误。
//
// 业务层用 errors.Is 分流：它不该重试（同一个模型重试同样输入必然还是错），
// 而应该降级或换模型。包装进 harness.Error 后 Kind 为 ErrKindModality。
var ErrUnsupportedModality = errUnsupportedModality{}

type errUnsupportedModality struct{}

func (errUnsupportedModality) Error() string { return "harness: 模型不支持该模态" }
func (errUnsupportedModality) Unwrap() error { return nil }
