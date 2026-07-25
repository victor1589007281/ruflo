package api

// call_interceptor.go —— LLM 调用切面 (design/01 §4.10 的 CallInterceptor)。
//
// ---------------------------------------------------------------------------
// 为什么切面在 pkg/api 而不是 pkg/graph
// ---------------------------------------------------------------------------
//
// §4.10 定义了两个对偶切面: NodeInterceptor (节点执行) 与 CallInterceptor (LLM 调用)。
// 前者的挂载点在图引擎内核, 后者的挂载点只能在**真正发 HTTP 的那一层** —— 也就是
// 这里。放在图层是做不到的: 一个节点内可以发任意多次 LLM 调用 (QueryEngine 的多轮
// 工具循环), 图层数一次节点执行, 数不到调用。
//
// 唯一挂载点 = Client.SendMessage。选它而不是"每个公开方法各挂一次"的理由:
// SimpleComplete / CompleteDiag / RawComplete 全部汇流到它, 挂这一处就覆盖非流式全部。
//
// **流式路径 (StreamMessage) 刻意不挂**: 切面的形状是"收一个调用、还一个结果",
// 而流式还的是两个 channel。硬要包就得把 channel 语义塞进 LLMResult, 那会让第三方
// 拦截器面对两种完全不同的返回形态。流式路径上的限流/熔断本来就各自就位
// (isCircuitOpen + Guard.Acquire 两处都有), 不挂链不等于不设防。记账在此。
//
// ---------------------------------------------------------------------------
// 链语义 (与 pkg/graph 的节点链逐条对齐, 刻意的)
// ---------------------------------------------------------------------------
//
//   - **顺序确定**: 按注册顺序 [0] 最外层。
//   - **next 必须恰好调一次**: 不调 = 调用被悄悄吞掉 (调用方拿到零值响应却以为成功);
//     调两次 = 同一次逻辑调用发两次 HTTP, 直接翻倍烧钱。两种误用都被检测并**点名**
//     哪个拦截器, 不静默容忍 —— 与节点链同理由 (这是第三方注入点)。
//   - **明示拒绝**: 不调 next 而返回一个非 nil error 是正当用法 (预算/权限拒绝),
//     与"忘了调 next"可区分。
//   - **panic 不穿透但也不当放行**: 转成 error 返回。fail-closed。
//   - **空链零开销**: 链为空时 SendMessage 直通原路径, 不构造任何中间对象 ——
//     8+ 下游平台在用这条路, 不能为一个默认关闭的切面付常态成本。
//
// ---------------------------------------------------------------------------
// 生产内置拦截器: 为什么只有 TokenLedger 一个
// ---------------------------------------------------------------------------
//
// §4.10 表最后一行是 "RateLimiter / CircuitBreaker: 沿用 api.Client 熔断, 前移到
// CallInterceptor 统一观测"。这里做的是"统一观测"那一半 —— 链上能拿到完整的
// LLMCallRecord (含 GuardWaitSec / CircuitOpened / CircuitBlocked / 三种 token 计数),
// 于是限流与熔断的**可观测面**成了切面的一等公民, 第三方不必去读 Client 私有字段。
//
// **没有**把 Guard.Acquire 与 isCircuitOpen 真的搬进链里: 那两处现在同时服务流式与
// 非流式两条路径, 搬进只覆盖非流式的链会让流式路径失去准入控制 —— 那是把一个可用的
// 防护改成半个。真正缺的那一半 (失败后并发折半的 AIMD 背压在**图层**没有等价物) 落在
// pkg/agent 的 ratelimit 节点拦截器里, 因为要折半的是"同时在跑几个节点", 不是"同时在
// 飞几个 HTTP 请求" —— 后者 RateLimitGuard 早就有了。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

// LLMCall 一次逻辑 LLM 调用的入参 (拦截器可读, 也可改后传给 next)。
type LLMCall struct {
	Model        string             // 本次生效模型 (含 fallback 解析结果)
	Request      string             // 粗粒度标签: "messages"
	Messages     []types.APIMessage //
	SystemPrompt []string           //
	Tools        []types.APITool    //
	MaxTokens    int                //
	Trace        trace.IDs          // 四元组 (RunID/NodeID/TurnID), 供归因
}

// LLMResult 一次逻辑 LLM 调用的产出。
//
// Record 是 api.Client 自己算出的那份 LLMCallRecord (token / 耗时 / 限流等待 / 熔断
// 状态全在里面)。**可能为零值**: 熔断拒绝等早退路径下 Client 也会 emit 一条记录,
// 但若上游没接记录汇聚 (见 recordSink), 这里就是零值 —— 零值与"这次没花 token"
// 必须可区分, 所以判定 token 时要看 Record.Timestamp 是否非零。
type LLMResult struct {
	Response *types.APIResponse
	Record   LLMCallRecord
}

// CallExec 链中的"下一跳"。拦截器必须恰好调用一次。
type CallExec func(context.Context, LLMCall) (LLMResult, error)

// CallInterceptor LLM 调用切面 (design/01 §4.10)。
type CallInterceptor interface {
	// Name 用于归因与开关配置, 必须稳定且唯一。
	Name() string
	Around(ctx context.Context, call LLMCall, next CallExec) (LLMResult, error)
}

var (
	errCallNextNotCalled = errors.New("api: 拦截器未调用 next (调用被吞掉)")
	errCallNextTwice     = errors.New("api: 拦截器重复调用 next (同一次调用发了两次请求)")
)

// FuncCallInterceptor 用函数构造调用拦截器 (测试与轻量注入用)。
type FuncCallInterceptor struct {
	N  string
	Fn func(ctx context.Context, call LLMCall, next CallExec) (LLMResult, error)
}

// Name 见 CallInterceptor。
func (f FuncCallInterceptor) Name() string {
	if f.N == "" {
		return "func"
	}
	return f.N
}

// Around 见 CallInterceptor。
func (f FuncCallInterceptor) Around(ctx context.Context, call LLMCall, next CallExec) (LLMResult, error) {
	if f.Fn == nil {
		return next(ctx, call)
	}
	return f.Fn(ctx, call, next)
}

// chainCall 把 [i0, i1, ..., in] 与终点 exec 组装成 i0(i1(...(exec)))。
//
// 每层包一个"恰好一次"计数器。计数器是 per-调用 局部变量而不是结构体字段: 同一条链
// 会被多个 goroutine 并发使用 (多 agent 并发调 LLM), 字段会互相踩。
func chainCall(ics []CallInterceptor, exec CallExec) CallExec {
	for i := len(ics) - 1; i >= 0; i-- {
		ic, next := ics[i], exec
		exec = func(ctx context.Context, call LLMCall) (res LLMResult, err error) {
			calls := 0
			var twice bool
			guarded := func(c context.Context, cl LLMCall) (LLMResult, error) {
				calls++
				if calls > 1 {
					// 不能直接 panic: 上层 recover 会把它算成拦截器崩溃, 归因错人。
					twice = true
					return LLMResult{}, fmt.Errorf("%w: %s", errCallNextTwice, ic.Name())
				}
				return next(c, cl)
			}
			defer func() {
				if r := recover(); r != nil {
					res = LLMResult{}
					err = fmt.Errorf("api: 调用拦截器 %s panic: %v", ic.Name(), r)
				}
			}()
			res, err = ic.Around(ctx, call, guarded)
			switch {
			case twice && err == nil:
				// 拦截器吞掉了"重复调用"的错误还报成功 —— 必须响。
				err = fmt.Errorf("%w: %s", errCallNextTwice, ic.Name())
			case calls == 0 && err == nil:
				// 既没调 next 又没报错 = 典型的"忘了调 next"。
				// 若拦截器返回了 error, 那是明示拒绝, 不报本错。
				err = fmt.Errorf("%w: %s", errCallNextNotCalled, ic.Name())
			}
			return res, err
		}
	}
	return exec
}

// ValidateCallInterceptors 拒绝 nil / 空名 / 重名 (Name 是归因与开关的键)。
func ValidateCallInterceptors(ics []CallInterceptor) error {
	seen := map[string]bool{}
	for _, ic := range ics {
		if ic == nil {
			return errors.New("api: 调用拦截器为 nil")
		}
		n := strings.TrimSpace(ic.Name())
		if n == "" {
			return errors.New("api: 调用拦截器 Name 为空")
		}
		if seen[n] {
			return fmt.Errorf("api: 调用拦截器重名 %q", n)
		}
		seen[n] = true
	}
	return nil
}

// —— 进程级链 ——
//
// 为什么要进程级而不是只有 per-Client 字段: Client 在全仓被 WithModel /
// ConfiguredClone 大量克隆 (每个团队角色一份), 只有 per-Client 字段的话装配方
// 得追着每一个克隆去装, 漏一个就是一个观测黑洞。进程级链对所有克隆生效。
// per-Client 字段 (Client.CallInterceptors) 仍保留, 用于只想影响一个客户端的场合;
// 两者拼接时进程级在外层。
var (
	globalCallMu  sync.RWMutex
	globalCallICs []CallInterceptor
)

// RegisterCallInterceptor 注册进程级调用拦截器 (幂等: 同名视为已注册)。
//
// ⚠️ 只应在进程装配阶段调用。运行期注册是安全的 (有锁), 但会让"这次调用过了哪些
// 拦截器"随时间变化, 排查时对不上。
func RegisterCallInterceptor(ic CallInterceptor) error {
	if ic == nil {
		return errors.New("api: 调用拦截器为 nil")
	}
	if strings.TrimSpace(ic.Name()) == "" {
		return errors.New("api: 调用拦截器 Name 为空")
	}
	globalCallMu.Lock()
	defer globalCallMu.Unlock()
	for _, e := range globalCallICs {
		if e.Name() == ic.Name() {
			return nil // 幂等: 每次运行装配都调一次也不会叠加
		}
	}
	globalCallICs = append(globalCallICs, ic)
	return nil
}

// UnregisterCallInterceptor 摘掉一个进程级拦截器 (测试与运维回滚用)。
func UnregisterCallInterceptor(name string) {
	globalCallMu.Lock()
	defer globalCallMu.Unlock()
	out := globalCallICs[:0]
	for _, e := range globalCallICs {
		if e.Name() != name {
			out = append(out, e)
		}
	}
	globalCallICs = out
}

// GlobalCallInterceptorNames 按注册序列名 (**不排序**: 顺序即语义)。
func GlobalCallInterceptorNames() []string {
	globalCallMu.RLock()
	defer globalCallMu.RUnlock()
	out := make([]string, 0, len(globalCallICs))
	for _, e := range globalCallICs {
		out = append(out, e.Name())
	}
	return out
}

// callChain 本次调用生效的链 (进程级在外, per-Client 在内)。空链返回 nil。
func (c *Client) callChain() []CallInterceptor {
	globalCallMu.RLock()
	n := len(globalCallICs)
	var out []CallInterceptor
	if n > 0 {
		out = append(out, globalCallICs...)
	}
	globalCallMu.RUnlock()
	if len(c.CallInterceptors) > 0 {
		out = append(out, c.CallInterceptors...)
	}
	return out
}

// —— 记录汇聚: 让链拿到 Client 自己算出的 LLMCallRecord ——

// recordSinkKey ctx 键: 承载本次调用的记录槽。
type recordSinkKey struct{}

// recordSink 一次调用的记录槽 (Client 写, 链读)。
//
// 为什么用 ctx 传而不是让 SendMessage 多返回一个值: SendMessage 的签名被全仓与下游
// 平台使用, 改签名的波及面远大于收益。ctx 槽只在链非空时挂上, 空链路径完全不受影响。
type recordSink struct {
	mu  sync.Mutex
	rec LLMCallRecord
	set bool
}

func withRecordSink(ctx context.Context) (context.Context, *recordSink) {
	s := &recordSink{}
	return context.WithValue(ctx, recordSinkKey{}, s), s
}

// sinkRecord 由 Client 在 emit 记录时调用 (无槽则什么都不做)。
//
// 记**最后一条**: 一次 SendMessage 内部可能因 429/5xx 重试 emit 多条, 最后一条才是
// 本次调用的终局 (Status=success 或最终失败), 中间那些重试记录已由 metrics 侧各自入账。
func sinkRecord(ctx context.Context, rec LLMCallRecord) {
	s, _ := ctx.Value(recordSinkKey{}).(*recordSink)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.rec, s.set = rec, true
	s.mu.Unlock()
}

func (s *recordSink) snapshot() LLMCallRecord {
	if s == nil {
		return LLMCallRecord{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec
}

// —— 内置: TokenLedger ——

// TokenLedger 按 trace 归因累加真实 token 用量的调用拦截器。
//
// 它存在的理由很具体: pkg/graph 的 BudgetManager 有一道 MaxTokens 闸, 但内核拿不到
// 真实用量 —— 它只能读 NodeResult.Tokens, 而 runner 从不回报, 于是那道闸恒不生效
// (interceptor.go 里写着"runner 不报则此项不生效")。真实用量只有这一层知道
// (LLMCallRecord.TotalTokens)。这个台账把它按 (RunID, NodeID) 记下来, 供图层的
// tokens 节点拦截器取差值回报。
//
// 为什么按 (RunID, NodeID) 而不只按 RunID: 预算要按节点算 (PerNodeMaxRuns 的同类
// 需求), 而且 map 分片会让同一个 NodeID 并发出现多次 —— 累加值天然是这些分片之和。
type TokenLedger struct {
	mu    sync.Mutex
	total map[string]int64 // key = RunID + "\x00" + NodeID
	// unattributed 没有 RunID 的调用 (会话/dashboard 等非团队路径) 的合计。
	// 单独记而不是丢弃: 否则"总量对不上"时无从判断是漏记还是真没花。
	unattributed int64
}

// NewTokenLedger 造一个台账。
func NewTokenLedger() *TokenLedger {
	return &TokenLedger{total: map[string]int64{}}
}

// Name 见 CallInterceptor。
func (l *TokenLedger) Name() string { return "tokens" }

// Around 见 CallInterceptor。台账只观测, 从不拒绝调用。
func (l *TokenLedger) Around(ctx context.Context, call LLMCall, next CallExec) (LLMResult, error) {
	res, err := next(ctx, call)
	// 记 TotalTokens 而不是 Input+Output: 前者已含 cache 读写两类, 是 Client 自己
	// 算好的口径, 两处各算一遍必然漂移。
	if n := int64(res.Record.TotalTokens); n > 0 {
		l.add(call.Trace.RunID, call.Trace.NodeID, n)
	}
	return res, err
}

func (l *TokenLedger) add(runID, nodeID string, n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if strings.TrimSpace(runID) == "" {
		l.unattributed += n
		return
	}
	l.total[runID+"\x00"+nodeID] += n
}

// Tokens 取某 (RunID, NodeID) 累计用量。
func (l *TokenLedger) Tokens(runID, nodeID string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total[runID+"\x00"+nodeID]
}

// RunTokens 取某 RunID 下全部节点的累计用量。
func (l *TokenLedger) RunTokens(runID string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var sum int64
	prefix := runID + "\x00"
	for k, v := range l.total {
		if strings.HasPrefix(k, prefix) {
			sum += v
		}
	}
	return sum
}

// Unattributed 无 RunID 归因的合计用量 (非团队路径)。
func (l *TokenLedger) Unattributed() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unattributed
}

// Forget 丢掉某 RunID 的台账 (团队跑完后由调用方清理, 防长跑进程内存单调增长)。
func (l *TokenLedger) Forget(runID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prefix := runID + "\x00"
	for k := range l.total {
		if strings.HasPrefix(k, prefix) {
			delete(l.total, k)
		}
	}
}

// DefaultTokenLedger 进程级台账 (图层的 tokens 拦截器取的就是它)。
var DefaultTokenLedger = NewTokenLedger()
