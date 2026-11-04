// contextadapt.go — 把业务层的 TurnContextProvider 适配成引擎的 EphemeralProvider
// （Moqi R-H1 的 harness 侧接线）。
//
// # 为什么需要这一层
//
// engine 侧的 EphemeralProvider 只认 []types.ContentBlock 与一段降级原因；业务层
// 需要的是**带 ID 的结构化块**（ID 用于去重、排序与 trace 反查，见 ContextBlock）。
// 两个形状不兼容，而 engine 不能 import harness（那是反向依赖）。
//
// 于是适配点落在 harness：业务层实现 TurnContextProvider（context 与 ID 都在），
// 这里把它转成引擎认识的那个函数。调用次数由引擎保证 —— 每个用户 turn 一次，
// 在第一请求之前（见 engine/ephemeral.go 的 run 级缓存）。
package harness

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/types"
)

// ContextSelection 是一次 provider 产出经去重与预算裁剪后的结果。
//
// Blocks/IDs 是**真正进入请求**的集合；DroppedIDs 是因预算被裁掉的块（按被裁
// 顺序），两者合起来才是 provider 给出的全集（空文本块除外：它们既不在 IDs
// 也不在 DroppedIDs 里，与改造前的去重语义一致）。
type ContextSelection struct {
	Blocks      []types.ContentBlock
	IDs         []string // 进入请求的块 ID（顺序同 Blocks）
	DroppedIDs  []string // 因预算被裁掉的块 ID（按被裁顺序）
	Tokens      int      // 裁剪后估算 token 总量
	TokensKnown bool     // 至少有一个块的 token 数可估/已给
	Degraded    string   // 非空表示发生了降级（去重/裁剪）
	// Citations 是进入请求的那些块携带的引用（R-H3）。
	//
	// 只收**真正发出去的块**的 refs：被裁掉/去重的块对应的引用也不该出现在
	// TurnResult 里，否则用户点开角标会发现模型根本没拿到那条证据。
	// 按 Citation.ID（无 ID 时按 source+uri+snippet）去重并保持首次出现顺序。
	Citations []Citation
}

// adaptContextProvider 包装业务 provider，并在每次调用时记录块 ID 供 trace 使用。
//
// 记录写在**调用时**而不是事后回推：块的去重与裁剪发生在引擎侧，事后再算一遍
// 会与真正发出去的集合不一致 —— 而 trace 里"本轮带了哪几条证据"必须是**发出去
// 的那几条**，否则事后对账会对不上。
//
// 旧签名没有预算概念，这里以 budget=0（不限）走同一条新实现：去重语义与新版本
// 逐字节一致，旧调用点（runner.go）不需要改。
func adaptContextProvider(p TurnContextProvider, req TurnRequest, sink func([]string, string)) engine.EphemeralProvider {
	if p == nil {
		return nil
	}
	var inner func(ContextSelection, string)
	if sink != nil {
		// 旧 sink 只关心"发出去了哪些 ID"与 trace；选择结果的其余字段在这里没有
		// 出口，但仍走同一实现，保证两条路径的去重/降级判定不会分叉。
		inner = func(sel ContextSelection, trace string) { sink(sel.IDs, trace) }
	}
	return adaptContextProviderWithBudget(p, req, 0, inner)
}

// adaptContextProviderWithBudget 与 adaptContextProvider 相同，但带 token 预算，
// 并通过 sink 回报完整选择结果（含 trace 与被裁 ID）。
//
// provider 返回 error 时仍降级而不是向引擎抛错：检索是可选增强，失败不该让用户
// 这一轮收不到回复。此时不调用 sink —— 没有 patch 就没有 trace，空结果由调用方
// 的零值自然表达（与旧实现一致）。
func adaptContextProviderWithBudget(p TurnContextProvider, req TurnRequest, budget int, sink func(ContextSelection, string)) engine.EphemeralProvider {
	if p == nil {
		return nil
	}
	return func(ctx context.Context, view engine.EphemeralView) ([]types.ContentBlock, string, error) {
		tc := TurnContext{
			OwnerID:     NormalizeOwner(req.OwnerID),
			SessionID:   req.SessionID,
			TurnID:      req.TurnID,
			EpisodeID:   req.EpisodeID,
			Input:       req.Input,
			Attachments: req.Attachments,
			RecentTurns: recentTurns(view.History),
			SentUpTo:    view.SentUpTo,
			History:     view.History,
		}
		patch, err := p.ProvideTurnContext(ctx, tc)
		if err != nil {
			// critical 是业务显式要求的"失败即拒绝"（R-H1）：上抛给引擎，
			// 由它终止本轮。其余失败仍按可选增强处理，降级不中断。
			var crit *engine.EphemeralCriticalError
			if errors.As(err, &crit) {
				return nil, "", crit
			}
			return nil, "context_degraded", nil
		}
		sel := dedupeAndPrune(patch.Blocks, budget)
		// provider 自报的降级优先：它知道 harness 看不到的原因（如检索超时）。
		// 只有它没说时，才把去重/裁剪这条结构性降级上报给引擎。
		degraded := patch.Degraded
		if degraded == "" {
			degraded = sel.Degraded
		}
		if sink != nil {
			sink(sel, patch.Trace)
		}
		return sel.Blocks, degraded, nil
	}
}

// EstimateContextTokens 估算一段文本的 token 数：rune 数除以 4 向上取整，非空
// 文本至少 1。
//
// 这是**下界估算**，不是上游 tokenizer 的复刻。中英文混排、代码与 JSON 的真实
// token 密度差别很大，这里只保证"长文本一定估得比短文本多"这一单调性，并刻意
// 偏小：预算裁剪宁可少裁（多带几百 token），也不要因为估高而错裁一条关键证据。
// 需要精确控制预算的调用方应让 provider 在 ContextBlock.Tokens 里自报真实计数。
func EstimateContextTokens(text string) int {
	if text == "" {
		return 0
	}
	n := utf8.RuneCountInString(text)
	return (n + 3) / 4
}

// selectedBlock 是去重后、裁剪前的一个候选块，保留裁剪所需的元信息。
type selectedBlock struct {
	id       string
	block    types.ContentBlock
	priority int
	tokens   int
	refs     []Citation
}

// dedupeAndPrune 去重（按 ID 保留第一个）后按 token 预算裁剪。
// budget <= 0 表示不限。
//
// 裁剪规则：反复裁掉 Priority 最小的已选块；**同 Priority 时裁掉靠后的那个**。
// 后者是因为 provider 用顺序表达"证据的叙述次序"——同权条目里靠前的那条通常
// 是更早、更基础的证据，从尾部开始丢能让保留集尽量维持原本的前缀；如果裁靠前
// 的，等于把叙述的起点挖掉，剩下的上下文会变得难以理解。
//
// 裁剪只做删除，不重排：Blocks 始终保持原始相对顺序。
func dedupeAndPrune(in []ContextBlock, budget int) ContextSelection {
	seen := make(map[string]bool, len(in))
	selected := make([]selectedBlock, 0, len(in))
	duplicateDropped := false
	for _, b := range in {
		if b.ID != "" {
			if seen[b.ID] {
				duplicateDropped = true
				continue
			}
			// 先登记再判空文本，与改造前的 dedupeBlocks 完全一致：同 ID 的第一条
			// 即使是空块，后续同 ID 也按重复处理，不会"空块让位给后一个"。
			seen[b.ID] = true
		}
		if strings.TrimSpace(b.Text) == "" {
			continue // 空块会让部分上游 400；不计入 IDs，也不算 dropped
		}
		tokens := b.Tokens
		if tokens <= 0 {
			// provider 没给（或给了非法值）时才估算；自报值优先，因为只有它知道
			// 自己的真实 tokenizer。
			tokens = EstimateContextTokens(b.Text)
		}
		selected = append(selected, selectedBlock{
			id:       b.ID,
			block:    types.ContentBlock{Type: types.ContentBlockText, Text: b.Text},
			priority: b.Priority,
			tokens:   tokens,
			refs:     b.Refs,
		})
	}

	total := 0
	for _, s := range selected {
		total += s.tokens
	}

	dropped := make([]string, 0)
	pruned := false
	for budget > 0 && total > budget && len(selected) > 0 {
		// 找到 Priority 最小的位置；同 Priority 取更靠后的那个（i 递增，故
		// <= 号天然把最后一次出现留到最后）。
		idx := 0
		for i := 1; i < len(selected); i++ {
			if selected[i].priority <= selected[idx].priority {
				idx = i
			}
		}
		if selected[idx].id != "" {
			dropped = append(dropped, selected[idx].id)
		}
		total -= selected[idx].tokens
		selected = append(selected[:idx], selected[idx+1:]...)
		pruned = true
	}

	blocks := make([]types.ContentBlock, len(selected))
	ids := make([]string, 0, len(selected))
	tokensKnown := false
	total = 0
	for i, s := range selected {
		blocks[i] = s.block
		if s.id != "" {
			ids = append(ids, s.id)
		}
		total += s.tokens
		if s.tokens > 0 {
			tokensKnown = true
		}
	}

	degraded := ""
	switch {
	case pruned:
		// 预算裁剪优先于去重：它解释了"为什么少了块"，信息量更大。
		degraded = "context_budget_pruned"
	case duplicateDropped:
		degraded = "context_duplicate_id_dropped"
	}
	return ContextSelection{
		Blocks:      blocks,
		IDs:         ids,
		DroppedIDs:  dropped,
		Tokens:      total,
		TokensKnown: tokensKnown,
		Degraded:    degraded,
		Citations:   collectCitations(selected),
	}
}

// collectCitations 收集仍在保留集里的块的引用，按稳定键去重并保持首次出现顺序。
//
// 去重键优先用 Citation.ID；没有 ID 时退回 source+uri+snippet —— 业务层可能
// 只给了可读字段。空引用（全字段为空）直接丢弃：它渲染出来是一个空角标，
// 对用户没有任何信息量。
func collectCitations(selected []selectedBlock) []Citation {
	seen := make(map[string]bool)
	var out []Citation
	for _, s := range selected {
		for _, c := range s.refs {
			if c.ID == "" && c.Source == "" && c.Title == "" && c.URI == "" && c.Snippet == "" {
				continue
			}
			key := "id:" + c.ID
			if c.ID == "" {
				key = "raw:" + c.Source + "\x00" + c.URI + "\x00" + c.Snippet
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}

// dedupeBlocks 按 ID 去重并转成引擎的内容块。
//
// 去重规则（R-H1「顺序确定、按 ID 去重」）：**保留第一个**同 ID 块。
// 不按 Priority 重排：顺序是 provider 表达"证据的叙述次序"的唯一手段，
// harness 重排会把"最相关的一条在最后"变成随机。
//
// 保留旧签名供既有调用点使用；budget=0 即不裁剪，行为与改造前一致。
func dedupeBlocks(in []ContextBlock) ([]types.ContentBlock, []string) {
	sel := dedupeAndPrune(in, 0)
	return sel.Blocks, sel.IDs
}

// recentTurns 从历史里抽出用户/助手文本，供 provider 判断"这一轮该检索什么"。
//
// 只取文本块：附件与工具结果由业务层另有索引，塞进摘要只会让 prompt 变长而
// 判断质量不升。取最后 maxRecentTurns 轮，顺序为时间正序。
func recentTurns(history []types.Message) []TurnSummary {
	const maxRecentTurns = 6
	out := make([]TurnSummary, 0, maxRecentTurns)
	for _, m := range history {
		var sb strings.Builder
		for _, b := range m.Content {
			if b.Type == types.ContentBlockText && b.Text != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(b.Text)
			}
		}
		if sb.Len() == 0 {
			continue
		}
		out = append(out, TurnSummary{TurnID: string(m.UUID), Text: sb.String()})
	}
	if len(out) > maxRecentTurns {
		out = out[len(out)-maxRecentTurns:]
	}
	return out
}
