// runner_wire.go — runner 与各契约实现的接线辅助（Moqi R-H1/R-H2/R-H4/R-H5/R-H17）。
//
// runner.go 只描述"一轮的时序"，本文件放三件不属于时序、但每条路径都要用的东西：
//   - 布局与缓存计划的一次性计算（同一份结果既发给上游，也写进 trace）；
//   - 账本 / 回退链的形状转换（账本没有配置时 trace 仍要有回退链）；
//   - provider 降级收集与重放结果的组装。
//
// 单独成文件是为了让"请求发出去的字节"与"trace 里记的字节"只有一个来源：
// 只要出现第二处 Build / 第二次下标计算，两边迟早分叉，而分叉只在账单上可见。
package harness

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/types"
)

// turnLayout 计算本轮布局与出站缓存计划。
//
// 返回值（layout, plan, systemBreakpoints, err）中 plan 为 nil 表示"本轮不干预
// 缓存"，应保持 client.CachePlan 为 nil 交给 api 内置启发式（与 R-H16 的
// "能力关闭时请求字节不变"一致）。
//
// 为什么布局断点数取 min(预算, 动态段个数)：默认预算 4，而一个用户 turn 通常只有
// 一条正文文本块。直接拿 4 去 Build 会被它自己的"断点必须落在动态段上"判死 ——
// 于是布局观测永远是空的、缓存计划永远算不出来，而错误被 recordTrace 吞掉。
// 这不是"少打几个点"，而是如实表达"布局本身就是 1 个动态段"；多出来的预算留给
// 消息侧内置启发式（见 cachePlanMessageReserve）。
func (r *Runner) turnLayout(req TurnRequest) (PromptLayout, *api.CachePlan, []int, error) {
	if r.opts.Layout == nil {
		return PromptLayout{}, nil, nil, nil
	}

	var system []string
	if r.opts.Engine != nil && r.opts.Engine.PromptMgr != nil {
		// 与 engine.go 组装 systemPrompt 用的是同一个方法。稳定段必须取自**同一来源**，
		// 否则计划里的下标描述的是另一份 prompt。
		system = r.opts.Engine.PromptMgr.BuildEffectiveSystemPrompt(r.opts.Engine.Tools)
	}
	stable := make([]SegmentInput, 0, len(system))
	for i, seg := range system {
		stable = append(stable, SegmentInput{ID: "system/" + strconv.Itoa(i), Text: seg})
	}
	dynamic := make([]SegmentInput, 0, len(req.Input))
	for i, b := range req.Input {
		if b.Type == types.ContentBlockText {
			dynamic = append(dynamic, SegmentInput{ID: "input/" + strconv.Itoa(i), Text: b.Text})
		}
	}

	max := r.opts.Layout.MaxBreakpoints()
	n := max
	if n > len(dynamic) {
		n = len(dynamic)
	}
	layout, err := r.opts.Layout.Build(SegmentSpec{
		Stable:          stable,
		Dynamic:         dynamic,
		BreakpointCount: n,
	})
	if err != nil {
		return PromptLayout{}, nil, nil, err
	}

	plan, indexes, err := TurnCachePlan(stable, len(dynamic), max)
	if err != nil {
		return PromptLayout{}, nil, nil, err
	}
	// trace 记的是**实际计划发给上游**的那一份（与 plan.SystemIndexes 同源复制，
	// 见 TurnCachePlan）。没有计划时留空，表示本轮未干预，而不是"计划为空"。
	layout.PlannedSystemBreakpoints = indexes
	return layout, plan, indexes, nil
}

// effectiveModel 是账本计价用的模型名。
//
// TurnResult.Model 的契约是"空串 = 沿用 engine 默认"，不能为一个内部账本去改它
// （那会破坏旧消费方对空串的判断）。但账本按模型名查价，空串必然落到"未知模型"
// 上 —— 于是每一轮都记成成本未知，账本等于没用。这里把"未覆盖"解析成 Config.Model，
// 只影响账本口径，不回写 TurnResult。
func (r *Runner) effectiveModel(requested string) string {
	if requested != "" {
		return requested
	}
	if r.opts.Engine != nil && r.opts.Engine.Config != nil {
		return r.opts.Engine.Config.Model
	}
	return ""
}

// fallbackChain 把本轮观测到的回退转成 trace 链，并在配置了账本时同时记账。
//
// 账本为 nil 时**仍然**返回链：回退是"这一轮最后是谁答的"的事实，trace 不能因为
// 没装账本就把这段事实丢掉。两条路径共用 fallbackEventReason，避免同一个语义在
// 不同调用点出现两种拼写。
func fallbackChain(l *CostLedger, key CostKey, infos []api.FallbackInfo) []FallbackEvent {
	if len(infos) == 0 {
		return nil
	}
	if l != nil {
		return RecordTurnFallbacks(l, key, infos)
	}
	out := make([]FallbackEvent, 0, len(infos))
	for _, info := range infos {
		out = append(out, FallbackEvent{
			CostKey:   key,
			FromModel: info.FromModel,
			ToModel:   info.ToModel,
			Reason:    fallbackEventReason(info),
		})
	}
	return out
}

// degradingProvider 把 provider 自报/抛出的降级记进本轮 trace 状态。
//
// 为什么需要这一层：engine 侧虽然累积了 ephemeralDegraded，但那个切片目前没有
// 任何出口（终态不带它）。适配器在 provider 返回 error 时也不调用 selection sink，
// 于是"检索超时导致本轮没有证据"这件事在 TurnResult 与 trace 里**完全不可见** ——
// 用户只会看到回答变差，却查不到原因。这里在 provider 边界捕获，属于唯一还能
// 看见它的位置。
type degradingProvider struct {
	inner TurnContextProvider
	// timeout 是硬超时（R-H1）。<=0 时直接同步调用，不额外起 goroutine。
	timeout   time.Duration
	onDegrade func(string)
}

// ProvideTurnContext 执行 provider，并把失败分成三档（R-H1）：
//
//	正常            → 返回 patch；自报 degrad 记入本轮
//	error + 非 critical → 降级：空 patch + context_degraded，本轮继续
//	error + critical    → 上抛 engine.EphemeralCriticalError，本轮失败
//
// 超时走中间那档。硬超时用 goroutine 而不是只依赖 provider 检查 ctx：检索服务
// 连不上时，不检查 ctx 的实现会把整轮拖住；这里保证到点就返回。provider 一般
// 会在 ctx 取消后自己退出，channel 有缓冲，不会因为没人接收而泄漏。
func (d *degradingProvider) ProvideTurnContext(ctx context.Context, tc TurnContext) (ContextPatch, error) {
	if d.timeout <= 0 {
		patch, err := d.inner.ProvideTurnContext(ctx, tc)
		return d.finish(patch, err)
	}

	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	type result struct {
		patch ContextPatch
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		patch, err := d.inner.ProvideTurnContext(callCtx, tc)
		ch <- result{patch: patch, err: err}
	}()

	select {
	case r := <-ch:
		return d.finish(r.patch, r.err)
	case <-callCtx.Done():
		// 超时/外层取消：半份 patch 一律不要，避免"看起来有证据但被截断"。
		d.degrade("context_degraded")
		return ContextPatch{}, fmt.Errorf("harness: 逐轮上下文提供器超时 (%s): %w", d.timeout, callCtx.Err())
	}
}

// finish 统一 normal / degrade / critical 三档，避免两条调用路径分叉。
func (d *degradingProvider) finish(patch ContextPatch, err error) (ContextPatch, error) {
	if err != nil {
		if patch.Critical {
			return patch, &engine.EphemeralCriticalError{Err: err}
		}
		d.degrade("context_degraded")
		return patch, err
	}
	if patch.Degraded != "" {
		d.degrade(patch.Degraded)
	}
	return patch, nil
}

func (d *degradingProvider) degrade(reason string) {
	if d.onDegrade != nil {
		d.onDegrade(reason)
	}
}

// replayResult 用已持久化的助手消息组装一个"重放"结果。
//
// 不重跑模型：同一 TurnID 的第二次请求必须是**同一份回答**。重新调用模型会得到
// 另一段文字，而业务层/user 看到的是"重试了一次，回复变了"。
//
// CostKnown 保持 false：AssistantRecord 不带成本，账本又是进程内的，重启后无法
// 复原当时的金额。把 0 当成已知成本会让重放轮看起来免费 —— 正是 R-H17 明令
// 禁止的"缺失记成零"。
func replayResult(req TurnRequest, rec AssistantRecord) *TurnResult {
	res := emptyResult(req)
	res.Blocks = rec.Blocks
	res.Text = joinTextBlocks(rec.Blocks)
	res.Citations = rec.Citations
	res.Usage = rec.Usage
	res.UsageKnown = rec.UsageKnown
	res.Terminal = rec.Terminal
	res.Safety = rec.Safety
	res.Model = rec.Model
	res.Persisted = true
	res.Degraded = append(res.Degraded, "turn_replayed")
	return res
}

// joinTextBlocks 把 text 块按原顺序拼成一段文本，与 drain 的 delta 拼接同口径
// （delta 之间不插分隔符，模型自己输出的换行原样保留）。
func joinTextBlocks(blocks []types.ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == types.ContentBlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
