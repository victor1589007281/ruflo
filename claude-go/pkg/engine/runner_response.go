package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// FinalResponseContract 是窄面 agent 的交付契约。它只校验可机械验证的结构与
// 证据引用，不判断根因语义；语义仍由 EvidenceBundle 的 E3+ 门控负责。
// 第一次不达标会撤销工具并要求完整重写一次，第二次仍不达标则显式报错，
// 不再把“从缺口小节开始、没有结论”的草稿当成功交付。
type FinalResponseContract struct {
	RequiredSections  []string
	RequireEvidenceID bool
	RepairInstruction string
}

var responseEvidenceIDRE = regexp.MustCompile(`ev_[A-Za-z0-9_]+`)

func validateFinalResponse(text string, contract *FinalResponseContract, known map[string]bool) error {
	if contract == nil {
		return nil
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("最终答案为空")
	}
	for _, section := range contract.RequiredSections {
		if !hasMarkdownSection(text, section) {
			return fmt.Errorf("缺少小节 %q", section)
		}
	}
	cited := responseEvidenceIDRE.FindAllString(text, -1)
	unknown := map[string]bool{}
	for _, id := range cited {
		if !known[id] {
			unknown[id] = true
		}
	}
	if len(unknown) > 0 {
		ids := make([]string, 0, len(unknown))
		for id := range unknown {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return fmt.Errorf("引用了工具结果中不存在的 evidence_id: %s", strings.Join(ids, ", "))
	}
	if contract.RequireEvidenceID && len(known) > 0 && len(cited) == 0 {
		return fmt.Errorf("关键证据必须至少引用一个真实 evidence_id")
	}
	return nil
}

func hasMarkdownSection(text, section string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == section || strings.HasPrefix(line, section+" ") {
			return true
		}
	}
	return false
}

// finalResponseEvidenceIDs 只从 tool_result 取 ID；用户提示和模型草稿不能给自己
// 制造一个“合法”引用。它是防笔误/幻觉的结构闸，不是对工具文本的信任声明。
func finalResponseEvidenceIDs(messages []types.Message) map[string]bool {
	known := map[string]bool{}
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type != types.ContentBlockToolResult || b.IsError {
				continue
			}
			for _, id := range responseEvidenceIDRE.FindAllString(b.Content, -1) {
				known[id] = true
			}
		}
	}
	return known
}

func finalResponseRepairNotice(contract *FinalResponseContract, validationErr error) types.Message {
	instruction := strings.TrimSpace(contract.RepairInstruction)
	if instruction == "" {
		instruction = "保留所有必需小节，只引用工具结果中的 evidence_id，直接给出完整修订稿。"
	}
	return types.Message{
		Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: "【最终答案契约修订】上一稿不能交付：" + validationErr.Error() +
				"。不要调用任何工具，也不要只写差异或省略结论；请基于已有工具结果完整重写。" + instruction,
		}},
	}
}
