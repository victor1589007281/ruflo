// constraints_test.go —— H2-2 硬约束自检的单测。
//
// 验收判据 (方案 §21 H2-2 原文): **「构造一个含硬约束 (如"不许改 x.go") 的 run,
// 压缩后询问该约束, 回答必须仍然正确」**。
//
// 本文件分两层:
//   - 抽取与核验的**判据本身**对不对 (纯函数, 不给任何模型发言权);
//   - 折叠**真的把丢失的约束补回来了**没有 (假上游按脚本作答, 包括"第一次故意
//     漏掉"这种必须能被抓住的形态)。
//
// 第二层是这条能力的关键: 只测第一层的话, 一个"抽取正确但从不重压"的实现会全绿,
// 而它在生产上等于什么都没做。
package compact

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// 第一层: 抽取与核验判据
// ============================================================================

// TestH22_ExtractsProhibitionConstraint 方案里点名的那个例子必须被抽出来。
func TestH22_ExtractsProhibitionConstraint(t *testing.T) {
	msgs := []types.Message{{
		Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText,
			Text: "注意: 不许改 x.go 里的导出签名, 其它随便。"}},
	}}
	cs := ExtractHardConstraints(msgs)
	if len(cs) == 0 {
		t.Fatal("'不许改 x.go' 没被抽成硬约束 —— 这条能力对方案点名的例子都不生效")
	}
	if !cs[0].Verifiable() {
		t.Fatalf("'不许改 x.go' 抽不出判别 token: %+v", cs[0])
	}
	ok := false
	for _, k := range cs[0].Keys {
		if k == "x.go" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("判别 token 里没有 x.go: %+v", cs[0].Keys)
	}
}

// TestH22_NonConstraintNotExtracted 对照臂: 不含触发词的句子不该被当成约束。
//
// 少了这一臂, "抽取"退化成一个恒真的 gobbler (把每条消息都当约束) 也能全绿,
// 而那样重压提示词会被塞满噪声, 真正的约束反而沉底。
func TestH22_NonConstraintNotExtracted(t *testing.T) {
	msgs := []types.Message{{
		Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText,
			Text: "我先看看 y.go 里怎么写的, 然后试着跑一下测试。"}},
	}}
	if cs := ExtractHardConstraints(msgs); len(cs) != 0 {
		t.Fatalf("普通陈述被当成了硬约束: %+v", cs)
	}
}

// TestH22_AssistantNotASource assistant 说过的话不是对它的约束。
func TestH22_AssistantNotASource(t *testing.T) {
	msgs := []types.Message{{
		Type: types.MessageTypeAssistant,
		Content: []types.ContentBlock{{Type: types.ContentBlockText,
			Text: "明白了, 不许改 x.go。"}},
	}}
	if cs := ExtractHardConstraints(msgs); len(cs) != 0 {
		t.Fatalf("assistant 的复述被当成了新约束: %+v", cs)
	}
}

// TestH22_CheckDetectsLoss 核验必须能**判出丢失**。
func TestH22_CheckDetectsLoss(t *testing.T) {
	cs := []HardConstraint{
		{Text: "不许改 x.go", Keys: []string{"x.go"}},
		{Text: "必须只动 auth.go", Keys: []string{"auth.go"}},
	}
	// 摘要只保住了 auth.go。
	rep := CheckConstraints("用户要求只修改 auth.go 的实现。", cs)
	if rep.Preserved != 1 {
		t.Fatalf("Preserved = %d, 期望 1", rep.Preserved)
	}
	if len(rep.Missing) != 1 || rep.Missing[0].Keys[0] != "x.go" {
		t.Fatalf("没判出 x.go 那条丢失: %+v", rep.Missing)
	}
	if rep.OK() {
		t.Fatal("丢了约束却报 OK —— 核验闸门是恒真的")
	}
}

// TestH22_CheckPassesWhenAllPreserved 对照臂: 全保住时不能报丢失。
//
// 与上一条互为反向。只有一条时, "永远报丢失"与"永远报通过"各能骗过一边。
func TestH22_CheckPassesWhenAllPreserved(t *testing.T) {
	cs := []HardConstraint{
		{Text: "不许改 x.go", Keys: []string{"x.go"}},
		{Text: "必须只动 auth.go", Keys: []string{"auth.go"}},
	}
	rep := CheckConstraints("约束: 不许改 x.go; 只动 auth.go。", cs)
	if !rep.OK() || rep.Preserved != 2 {
		t.Fatalf("全保住却报丢失: %+v", rep)
	}
}

// TestH22_UnverifiableIsNotPreserved 抽不出判别 token 的约束**既不算保住也不算
// 丢失**, 必须单独计数。
//
// 这是防"空闸门集合上的全过恒真": 若把不可核验的约束直接算作 Preserved,
// 一个全是"要小心"这类无落点约束的 run 会报出 100% 通过率, 而核验其实一件事
// 都没验。
func TestH22_UnverifiableIsNotPreserved(t *testing.T) {
	cs := []HardConstraint{
		{Text: "一定要小心一点", Keys: nil},
		{Text: "不许改 x.go", Keys: []string{"x.go"}},
	}
	rep := CheckConstraints("摘要里啥都没提。", cs)
	if rep.Total != 2 || rep.Verifiable != 1 {
		t.Fatalf("基数不对: Total=%d Verifiable=%d", rep.Total, rep.Verifiable)
	}
	if rep.Unverifiable() != 1 {
		t.Fatalf("Unverifiable = %d, 期望 1", rep.Unverifiable())
	}
	if rep.Preserved != 0 {
		t.Fatalf("不可核验的约束被算成了已保住 (Preserved=%d)—— 核验变成恒真的了", rep.Preserved)
	}
}

// TestH22_KeysIgnoreSentencePunctuation 断句不能把路径劈开。
//
// "不许改 x.go" 里的点一旦被当句末, key 就变成 "x" 与 "go", 抽取静默失效 ——
// 而那时上面每一条断言的表现都是"这条句子抽不出 token", 看起来像内容问题。
func TestH22_KeysIgnoreSentencePunctuation(t *testing.T) {
	cs := ExtractHardConstraints([]types.Message{{
		Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText,
			Text: "不许改 x.go. 就这么定。"}},
	}})
	if len(cs) == 0 {
		t.Fatal("带英文句点的中文约束没被抽出")
	}
	found := false
	for _, k := range cs[0].Keys {
		if k == "x.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("路径被断句劈开了, keys=%v", cs[0].Keys)
	}
}

// TestH22_DedupsRepeatedConstraints 同一个约束在多轮里被重复申明只算一条。
func TestH22_DedupsRepeatedConstraints(t *testing.T) {
	line := "不许改 x.go 的导出签名。"
	msgs := []types.Message{
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: line}}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: line}}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: line}}},
	}
	cs := ExtractHardConstraints(msgs)
	if len(cs) != 1 {
		t.Fatalf("同一条约束抽出了 %d 份 —— 重压提示词会被同一句刷屏", len(cs))
	}
}

// ============================================================================
// 第二层: 折叠真的把丢失的约束补回来了没有
// ============================================================================

// scriptedCompactor 起一个**按脚本作答**的假上游。
//
// answers 按请求序号取; 用完之后一律回最后一条。每个请求的正文都记进 prompts,
// 供断言"重压请求里确实带着被漏掉的那条约束"。
//
// 为什么必须能按脚本作答: "模型第一次漏掉、第二次补上"正是本能力要处理的形态,
// 而一个只会老实照抄的假上游永远复现不出它 —— 那样测出来的只是"没问题时不重压",
// 恰好把唯一的失效路径漏掉。
func scriptedCompactor(t *testing.T, maxCtx int, answers ...string) (*Compactor, *[]string, *atomic.Int64, func()) {
	t.Helper()
	var calls atomic.Int64
	var mu = &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readAllBody(r)
		*mu = append(*mu, string(raw))
		i := int(calls.Add(1)) - 1
		if i >= len(answers) {
			i = len(answers) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.APIResponse{
			ID: "msg_sum", Type: "message", Role: "assistant", Model: "test",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: answers[i]}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 10, OutputTokens: 10},
		})
	}))
	c := NewCompactor(&api.Client{BaseURL: srv.URL, APIKey: "k", Model: "test",
		Client: srv.Client(), RetryCount: 0}, maxCtx)
	return c, mu, &calls, srv.Close
}

func readAllBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var buf [1 << 20]byte
	n := 0
	for n < len(buf) {
		m, err := r.Body.Read(buf[n:])
		n += m
		if err != nil {
			break
		}
	}
	return buf[:n], nil
}

// historyWithConstraint 造一段超阈值的历史, 硬约束**全部排在最前面**。
//
// 位置不是细节: 尾部 6 条受尾部保护、不参与折叠, 约束若排在末尾就根本没被折,
// 测出来的是"没被折的东西还在不在" —— 与 H2-2 要验的事无关, 而且会以"约束都
// 保住了"的假象通过。本文件里踩过一次: 第二版把补加的约束 append 在尾巴上,
// 断言就恒真了。
func historyWithConstraint(n, chars int, constraints ...string) []types.Message {
	var msgs []types.Message
	for i, c := range constraints {
		msgs = append(msgs, types.Message{
			Type:    types.MessageTypeUser,
			UUID:    "c" + string(rune('a'+i%26)),
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: c}},
		})
	}
	for i := 0; i < n; i++ {
		msgs = append(msgs, types.Message{
			Type:    types.MessageTypeUser,
			UUID:    "u" + string(rune('a'+i%26)),
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: strings.Repeat("x", chars)}},
		})
	}
	return msgs
}

// 方案里点名的那条约束 —— 多个测试共用, 避免各写一份措辞导致 keys 不一致。
const constraintXGo = "硬性要求: 不许改 x.go 的导出函数签名。"
const constraintAuthGo = "另外必须只动 auth.go。"

// TestH22_RetriesWhenConstraintDropped 核心: 第一次摘要漏掉约束 ⇒ 触发重压,
// 且最终落到上下文里的摘要**带回了那条约束**。
func TestH22_RetriesWhenConstraintDropped(t *testing.T) {
	// 第一次故意漏掉 x.go; 第二次补上。
	c, prompts, calls, closeSrv := scriptedCompactor(t, 3000,
		"任务进行到一半, 改了几个文件。",
		"任务进行到一半。硬约束: 不许改 x.go 的导出函数签名。",
	)
	defer closeSrv()

	var got CompactionStats
	c.SetOnCompact(func(_ context.Context, st CompactionStats) { got = st })

	out, err := c.AutoCompact(context.Background(), historyWithConstraint(10, 2000, constraintXGo), "test")
	if err != nil {
		t.Fatalf("AutoCompact: %v", err)
	}
	if out == nil {
		t.Fatal("没折叠")
	}
	if calls.Load() != 2 {
		t.Fatalf("摘要请求次数 = %d, 期望 2 (第一次 + 重压一次)", calls.Load())
	}

	// 重压请求里必须**带着约束块**, 否则重压只是盲目重试 —— "不合格则重压"里的
	// "不合格"没有被传下去, 模型只会把同一份历史再摘一遍。
	//
	// 判据取 ConstraintRetryMarker 而不是 "含 x.go": 历史正文里本来就写着 x.go,
	// 后者恒真 (实测: 把重压的约束参数改成 nil, 该断言仍然通过)。
	if !strings.Contains((*prompts)[1], ConstraintRetryMarker) {
		t.Fatalf("重压请求里没有约束块 —— 模型只被告知'重来一次', 不知道漏了哪条:\n%s",
			(*prompts)[1][:minInt(len((*prompts)[1]), 600)])
	}
	if !strings.Contains((*prompts)[1], "x.go") {
		t.Fatalf("重压请求的约束块里没有那条被漏掉的约束原文:\n%s",
			(*prompts)[1][:minInt(len((*prompts)[1]), 600)])
	}
	// 对照臂: 第一次请求里**不该**有这段块。少了这一臂, "重压请求含约束块"会被
	// 任何"每次都带约束块"的实现满足 —— 那种实现等于无条件折两次。
	if strings.Contains((*prompts)[0], ConstraintRetryMarker) {
		t.Fatal("第一次摘要请求里就带了重压约束块 —— 重压不再是一次例外")
	}

	// 最终产物里的摘要必须带 x.go。
	summary := summaryTextOf(t, out)
	if !strings.Contains(summary, "x.go") {
		t.Fatalf("重压后摘要里仍然没有 x.go —— 这段历史之后再也没人看得见那条约束:\n%s", summary)
	}

	// 读数：约束被抽出、可核验、最终 0 条丢失, 且确实重压过。
	if got.Constraints < 1 || got.ConstraintsVerifiable < 1 {
		t.Fatalf("约束读数不对: %+v", got)
	}
	if got.ConstraintsMissing != 0 {
		t.Fatalf("重压后仍报 %d 条丢失", got.ConstraintsMissing)
	}
	if !got.Recompacted {
		t.Fatal("漏了约束却没报重压 —— 读数与行为对不上")
	}
}

// TestH22_NoRetryWhenConstraintPreserved 对照臂: 摘要本来就保住了 ⇒ **不重压**。
//
// 少了这一臂, "请求次数 == 2"无法把"按需重压"与"无条件折两次"区分开 —— 后者
// 每次折叠都多花一次摘要请求的钱。
func TestH22_NoRetryWhenConstraintPreserved(t *testing.T) {
	c, _, calls, closeSrv := scriptedCompactor(t, 3000,
		"不许改 x.go 的导出签名, 其余按计划推进。",
	)
	defer closeSrv()

	var got CompactionStats
	c.SetOnCompact(func(_ context.Context, st CompactionStats) { got = st })

	if _, err := c.AutoCompact(context.Background(), historyWithConstraint(10, 2000, constraintXGo), "test"); err != nil {
		t.Fatalf("AutoCompact: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("摘要请求次数 = %d, 期望 1 (约束已保住, 不该重压)", calls.Load())
	}
	if got.Recompacted {
		t.Fatal("约束保住了却报了重压")
	}
	if got.ConstraintsMissing != 0 {
		t.Fatalf("ConstraintsMissing = %d, 期望 0", got.ConstraintsMissing)
	}
}

// TestH22_NoRetryWhenBothPreserved 两条约束都保住时一次请求搞定。
func TestH22_NoRetryWhenBothPreserved(t *testing.T) {
	c, _, calls, closeSrv := scriptedCompactor(t, 3000,
		"约束: 不许改 x.go; 只动 auth.go。",
	)
	defer closeSrv()

	var got CompactionStats
	c.SetOnCompact(func(_ context.Context, st CompactionStats) { got = st })

	if _, err := c.AutoCompact(context.Background(),
		historyWithConstraint(10, 2000, constraintXGo, constraintAuthGo), "test"); err != nil {
		t.Fatalf("AutoCompact: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("两条约束都保住了, 却发了 %d 次摘要请求", calls.Load())
	}
	if got.Recompacted {
		t.Fatal("没有丢失却重压了")
	}
	if got.ConstraintsVerifiable != 2 || got.ConstraintsMissing != 0 {
		t.Fatalf("读数不对: %+v", got)
	}
}

// TestH22_RetryWorseThanFirstIsRejected 重压结果**比第一次更差**时必须丢弃。
//
// 这是"无条件采纳重压"与"取更全的那一版"的分界。构造: 第一次只漏 1 条, 重压把
// 两条都漏了 —— 采纳它等于用一次请求把结果换坏。
//
// 没有这条断言时, "重压只在有缺失时发生"仍然成立、读数也仍然非零, 但结果会变差,
// 而变差这件事没有任何地方报出来。
func TestH22_RetryWorseThanFirstIsRejected(t *testing.T) {
	c, _, calls, closeSrv := scriptedCompactor(t, 3000,
		"目前只记得: 不许改 x.go。", // 漏 auth.go -> 触发重压
		"这次什么都没提。",         // 两条都漏 -> 比第一次差
	)
	defer closeSrv()

	var got CompactionStats
	c.SetOnCompact(func(_ context.Context, st CompactionStats) { got = st })

	out, err := c.AutoCompact(context.Background(),
		historyWithConstraint(10, 2000, constraintXGo, constraintAuthGo), "test")
	if err != nil {
		t.Fatalf("AutoCompact: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("请求次数 = %d, 期望 2", calls.Load())
	}
	if !got.Recompacted {
		t.Fatal("第一次确实漏了 auth.go, 却没触发重压")
	}
	// 关键: 最终留下的必须是**更全的那一版**。
	if sum := summaryTextOf(t, out); !strings.Contains(sum, "x.go") {
		t.Fatalf("更差的重压结果被采纳了 —— 第一次摘要里的 x.go 被换没了:\n%s", sum)
	}
	if got.ConstraintsMissing != 1 {
		t.Fatalf("ConstraintsMissing = %d, 期望 1 —— 被丢弃的那版不该影响读数", got.ConstraintsMissing)
	}
}

// TestH22_PartialRetryReportsRemainder 重压有改善但没补齐时, 采纳它, 并**如实报出
// 还剩几条**。
//
// 这是真实世界最常见的落点 (模型补了一部分), 而"有没有改善"这个布尔远远不够:
// 读数若因为"比上次好"就报 0, 长程任务里"这次折叠丢了什么"就永远看不见。
func TestH22_PartialRetryReportsRemainder(t *testing.T) {
	c, _, calls, closeSrv := scriptedCompactor(t, 3000,
		"什么都没提。",           // 两条都漏
		"只记得一个: 不许改 x.go。", // 补回一条, auth.go 仍缺
	)
	defer closeSrv()

	var got CompactionStats
	c.SetOnCompact(func(_ context.Context, st CompactionStats) { got = st })

	out, err := c.AutoCompact(context.Background(),
		historyWithConstraint(10, 2000, constraintXGo, constraintAuthGo), "test")
	if err != nil {
		t.Fatalf("AutoCompact: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("请求次数 = %d, 期望 2", calls.Load())
	}
	if s2 := summaryTextOf(t, out); !strings.Contains(s2, "x.go") {
		t.Fatalf("有改善的重压结果没被采纳:\n%s", s2)
	}
	if got.ConstraintsMissing != 1 {
		t.Fatalf("ConstraintsMissing = %d, 期望 1 —— 残缺被报成了 0", got.ConstraintsMissing)
	}
	if got.ConstraintsVerifiable != 2 {
		t.Fatalf("ConstraintsVerifiable = %d, 期望 2", got.ConstraintsVerifiable)
	}
}

// TestH22_AtMostOneRetry 重压**至多一次** —— 第二次仍缺不继续试。
//
// 这条是成本闸门: 没有它, 一个"永远漏同一条约束"的摘要器会让每次折叠都烧 3、4
// 次请求, 而收益是零 (第二次失败说明判据或模型态度不会因为再来一次而改变)。
func TestH22_AtMostOneRetry(t *testing.T) {
	// 两次回答都漏 —— 第三次请求不该出现。
	c, _, calls, closeSrv := scriptedCompactor(t, 3000,
		"第一次摘要, 什么都没提。",
		"重压摘要, 还是什么都没提。",
		"第三次不该被发出来。",
	)
	defer closeSrv()

	if _, err := c.AutoCompact(context.Background(), historyWithConstraint(10, 2000, constraintXGo), "test"); err != nil {
		t.Fatalf("AutoCompact: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("摘要请求次数 = %d, 期望恰好 2 (重压至多一次)", calls.Load())
	}
}

// TestH22_NoConstraintsNoRetry 历史里没有硬约束时: 不重压, 一次请求。
func TestH22_NoConstraintsNoRetry(t *testing.T) {
	c, _, calls, closeSrv := scriptedCompactor(t, 3000, "普通的摘要。")
	defer closeSrv()

	var got CompactionStats
	c.SetOnCompact(func(_ context.Context, st CompactionStats) { got = st })

	if _, err := c.AutoCompact(context.Background(), bigHistory(10, 2000), "test"); err != nil {
		t.Fatalf("AutoCompact: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("请求次数 = %d, 期望 1", calls.Load())
	}
	if got.Constraints != 0 || got.Recompacted {
		t.Fatalf("没约束却报出约束读数: %+v", got)
	}
}

// TestH22_RetryFailureKeepsFirstSummary 重压请求本身失败时, 保留第一次的摘要
// (而不是整次折叠失败)。
//
// 折叠失败是一条比"摘要不完美"严重得多的故障路径: 上下文压不下去, 下一轮直接
// 撞 prompt_too_long。重压是增强不是前置条件。
func TestH22_RetryFailureKeepsFirstSummary(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) >= 2 {
			// 第二次请求直接断连 —— 模拟重压失败。
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.APIResponse{
			ID: "m", Type: "message", Role: "assistant", Model: "test",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "第一版摘要(漏了 x.go)"}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 10, OutputTokens: 10},
		})
	}))
	defer srv.Close()

	c := NewCompactor(&api.Client{BaseURL: srv.URL, APIKey: "k", Model: "test",
		Client: srv.Client(), RetryCount: 0}, 3000)

	out, err := c.AutoCompact(context.Background(), historyWithConstraint(10, 2000, constraintXGo), "test")
	if err != nil {
		t.Fatalf("重压失败不该让整次折叠失败: %v", err)
	}
	if out == nil {
		t.Fatal("折叠结果为空 —— 重压失败把好的那版一起丢了")
	}
	if s := summaryTextOf(t, out); !strings.Contains(s, "第一版摘要") {
		t.Fatalf("保留下来的不是第一次的摘要:\n%s", s)
	}
}

// summarTextOf 从折叠产物里取摘要正文。
func summaryTextOf(t *testing.T, msgs []types.Message) string {
	t.Helper()
	for _, m := range msgs {
		if strings.HasPrefix(m.UUID, "summary-") {
			for _, b := range m.Content {
				if b.Type == types.ContentBlockText {
					return b.Text
				}
			}
		}
	}
	t.Fatal("折叠产物里没有摘要消息")
	return ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
