// toolgate.go — R-H9 三条"声明了却不执行"的元数据在执行侧落地。
//
// # 为什么这些闸必须在 pkg/tool
//
// R-H9 的六项元数据里，`SideEffect` / `NeedsConfirmation` / `OwnerScoped` 由业务层
// 消费（决定"这个部署允不允许"), 但 `Timeout` / `IdempotencyKey` / `MaxResultBytes`
// 只能在**执行点**生效: 只有这里能看到真实入参、能起定时器、能缓存首次结果。
//
// # 依赖与接口
//
// pkg/tool 不能 import pkg/harness（harness → engine → tool，反向成环）。所以这里
// 用**结构对接**：凡是实现了同名同签名方法的工具自动满足本文件的接口，不需要
// 适配代码。这与 ResultLimiter 是同一套做法（见 tool.go 的注释）。
//
// # 零值 = 旧行为
//
// 三个闸全部是"工具没声明就什么都不做"：
//
//	没实现 ToolMeta      → 无超时、无去重、元数据取 fail-closed 默认值
//	Timeout() == 0       → 不起定时器（连 context.WithTimeout 都不调用）
//	未开启去重            → 热量为零
//
// 这条纪律的来源是 R-H16: 新能力关闭时请求与执行必须逐字节等于改造前。给所有存量
// 工具默认加一次静默去重或默认超时，上线后的表现是"某工具偶尔少执行一次"或
// "某工具在高峰期随机失败"，而工具本身一行没改 —— 那种缺陷极难归因。
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// ToolMetaTimeout 是工具**可选实现**的超时声明（Moqi R-H9 的 Timeout）。
//
// 与 harness.ToolMeta.Timeout 同名同签名，所以实现了 harness 契约的工具天然满足
// 本接口。放在 pkg/tool 是因为只有执行点能真正施加超时。
type ToolMetaTimeout interface {
	// Timeout 返回单次执行上限；0 = 不限时。
	Timeout() time.Duration
}

// ToolMetaConfirmation 是工具**可选实现**的确认声明（Moqi R-H9 的 NeedsConfirmation）。
//
// # 与 harness.ToolMeta 默认值的**方向差异**（重要）
//
// harness 侧 `ToolMetaOf` 对未声明工具返回 fail-closed 默认值（写、需确认、
// 按 owner 隔离）—— 那是**策略判断**，业务层拿它决定"这个部署允不允许"。
//
// 本文件是**执行闸**，所以未声明一律等于"不设闸"。理由很直接：若这里也默认
// NeedsConfirmation=true，那么仓库里每一个存量工具（Read/Glob/Bash/…）都会在
// 上线瞬间变成"必须确认才能执行"，而它们一行代码都没改 —— 这不是安全加固，
// 是把整个产品停摆。真正的 fail-closed 已经在策略层做过了。
type ToolMetaConfirmation interface {
	// NeedsConfirmation 表示执行前是否需要宿主显式确认。
	NeedsConfirmation() bool
}

// NeedsConfirmationOf 返回工具是否要求确认；未实现或为 nil 时返回 false。
func NeedsConfirmationOf(t Tool) bool {
	if t == nil {
		return false
	}
	if c, ok := t.(ToolMetaConfirmation); ok {
		return c.NeedsConfirmation()
	}
	return false
}

// IdempotentTool 是工具**可选实现**的"本轮内结果可按 (工具, 参数) 复用"声明。
//
// # 为什么是工具自己声明，而不是按 IsReadOnly 自动推导
//
// 直觉是"只读工具的结果当然可以复用"。但那是错的：同一轮里模型完全可以
//
//	Read(a.txt) → Edit(a.txt) → Read(a.txt)
//
// 第二次 Read 若命中第一次的缓存，模型会拿到**改动前**的内容，然后基于它继续
// 编辑 —— 结果是静默的数据损坏，而每一层都认为自己是对的。所以"结果可复用"
// 不是从 IsReadOnly 推出来的性质，它需要工具作者知道"我的结果不随同轮写入变化"
// （比如按内容 hash 寻址的记忆检索、纯计算的格式化工具）。
//
// 默认 false（不去重）：去重是优化，不是正确性依赖。副作用工具的真幂等靠把
// 幂等键传给下游，而不是靠本地缓存。
type IdempotentTool interface {
	Idempotent() bool
}

// IdempotentOf 返回工具是否声明了本轮内结果可复用；未实现或为 nil 时返回 false。
func IdempotentOf(t Tool) bool {
	if t == nil {
		return false
	}
	if i, ok := t.(IdempotentTool); ok {
		return i.Idempotent()
	}
	return false
}

// ConfirmationChecker 回答"这一次调用是否已被宿主确认"。
//
// 由装配方实现（业务层在用户点过"允许"之后构造）。pkg/tool 不能自己问用户：
// 它在 API 服务里跑，没有终端；在这里调 promptUserApproval 只会读到 EOF 并
// 静默判成拒绝。
type ConfirmationChecker interface {
	// IsConfirmed 报告 (工具名, 入参) 是否已获确认。
	//
	// 入参参与判定：同一个工具对不同文件的一次确认，不应放行下一次针对另一个
	// 文件（尤其是删文件类操作）的调用。具体粒度由实现决定，接口只要求它能
	// 看到入参。
	IsConfirmed(toolName string, input json.RawMessage) bool
}

// TimeoutOf 返回工具声明的超时；未实现或为 nil 时返回 0（不限时）。
func TimeoutOf(t Tool) time.Duration {
	if t == nil {
		return 0
	}
	if d, ok := t.(ToolMetaTimeout); ok {
		return d.Timeout()
	}
	return 0
}

// ErrToolTimedOut 是工具超时的 typed error。
//
// 单独一个错误值而不是把 context.DeadlineExceeded 直接往上抛：调用方需要区分
// "工具自己超时了"与"整轮被取消了"（后者的 ctx.Err() 是 context.Canceled）。
// 两者的处置完全不同 —— 超时可以只让这一个工具失败并让模型重试，取消则必须
// 终止整轮。混成一个错误会让这两条路无法分辨。
var ErrToolTimedOut = errors.New("tool: 执行超时")

// callWithTimeout 在 t 声明的超时内执行 fn。
//
// # 为什么用独立 child ctx 而不是直接给调用方的 ctx 加 deadline
//
// 直接 cancel 调用方 ctx 会连带取消同批次并发的其他工具、以及整轮的流式读取 ——
// 表现是"一个慢工具把整轮干掉了"，与"只让这一个调用失败"的目标相反。所以派生
// 一个 child：超时只影响本次 Call。
//
// # 为什么要看 ctx.Err() 而不是只看 context.DeadlineExceeded
//
// 一个实现良好的工具会把 ctx 传下去并在 ctx.Done() 时返回 ctx.Err()；但存量工具
// 可能压根不读 ctx（写文件、跑 sync 计算）。那种情况下 fn 会在超时之后才返回，
// 而返回值本身没有任何"超时"痕迹。所以判定分两步：先看 child ctx 是否已到期
// （这是**权威**判据），再参考 fn 返回的 error。
//
// 注意：Go 无法强杀一个不读 ctx 的 goroutine。这里的超时语义是"不再等它"，
// 而不是"终止它"。返回值取超时错误，但后台 goroutine 可能仍在跑 —— 这一点必须
// 在注释里说清，否则调用方会以为超时后副作用一定没有发生。
func callWithTimeout(ctx context.Context, timeout time.Duration, fn func(context.Context) (*ToolResult, error)) (*ToolResult, error) {
	if timeout <= 0 {
		return fn(ctx)
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type outcome struct {
		res *ToolResult
		err error
	}
	done := make(chan outcome, 1) // 带缓冲: 超时后 goroutine 仍能写入并退出, 不泄漏
	go func() {
		res, err := fn(callCtx)
		done <- outcome{res, err}
	}()

	select {
	case <-callCtx.Done():
		// 父 ctx 被取消（整轮终止）与本次超时必须分开报：前者的正确处置是终止
		// 整轮，后者只是这一个工具失败。用 ctx.Err() 而不是 callCtx.Err() 判断
		// 归因 —— callCtx 在父取消时同样是 Canceled，再看一层才能分辨。
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w（上限 %s）", ErrToolTimedOut, timeout)
	case out := <-done:
		// fn 先返回、但 child ctx 也已经到期：这是"刚好在边界上完成"。判为超时
		// 会误杀一个已经成功的调用；判为成功会让"超时"变成不可复现的随机。
		// 取**成功**：副作用已经发生了，报告失败会让调用方重试，而重试会再产生
		// 一次副作用 —— 那是比"晚了一点"严重得多的问题。
		return out.res, out.err
	}
}

// IdempotencyGate 按 (工具名 + 参数 hash) 记住首次结果，重复调用直接返回同一结果。
//
// # 为什么去重必须在执行侧
//
// R-H9 的验收是"同一幂等键重复调用返回同一结果"。在调用方去重做不到这一点：
// 模型一轮里可能发出两个**相同**的 tool_use（同一文件读两次、同一查询发两遍），
// 引擎会把它们都送进执行。只有执行点知道"这两次其实是同一次调用"。
//
// # 只缓存**成功**结果
//
// 失败不缓存：一次网络抖动导致的失败如果被记住，这个 key 就永久毒化了 —— 重试
// 永远拿到同一个错误，而调用方以为自己只是运气不好。错误必须可重试。
//
// # 并发
//
// 同一个 key 的两个**并发**调用只应执行一次。用一个"进行中"占位（inflight map）
// 而不是简单的读-判-写：否则两个 goroutine 同时看到"没有缓存"，会双双执行，
// 副作用发生两次 —— 而去重本来的目的就是防止这个。
type IdempotencyGate struct {
	mu       sync.Mutex
	done     map[string]*ToolResult
	inflight map[string]chan struct{}
	// maxEntries 是缓存条目上限；<=0 = 不限。
	//
	// 有上限是因为这个 map 的生命周期是整个 run/会话：一个长跑 agent 会产生
	// 几万条 tool 调用，无界缓存会把内存吃掉。超限时的策略是**清空重来**而不是
	// LRU：清空之后最坏是"某个旧 key 被重新执行一次"，而 LRU 需要维护双向链表，
	// 复杂度和 bug 面都更大。去重是优化，不是正确性依赖 —— 副作用工具的真去重
	// 靠调用方传幂等键给下游。
	maxEntries int
	enabled    bool
	// hits/misses 供观测（测试与指标用）。
	hits   int
	misses int
}

// NewIdempotencyGate 构造去重闸。enabled=false 时所有方法零开销。
func NewIdempotencyGate(enabled bool, maxEntries int) *IdempotencyGate {
	return &IdempotencyGate{
		enabled:    enabled,
		maxEntries: maxEntries,
		done:       make(map[string]*ToolResult),
		inflight:   make(map[string]chan struct{}),
	}
}

// Do 以 key 为幂等键执行 fn。同一 key 的并发/后续调用复用首次成功结果。
//
// returned 为 true 表示这次调用复用了缓存结果（没有执行 fn）。
func (g *IdempotencyGate) Do(ctx context.Context, key string, fn func() (*ToolResult, error)) (*ToolResult, bool, error) {
	if g == nil || !g.enabled || key == "" {
		res, err := fn()
		return res, false, err
	}

	for {
		g.mu.Lock()
		if res, ok := g.done[key]; ok {
			g.hits++
			g.mu.Unlock()
			// 返回副本: 调用方可能改写 Content 或 Images 切片, 共享同一个对象会让
			// "第二次调用"看到被第一次改过的内容 —— 而幂等的承诺是"同一结果"。
			return copyResult(res), true, nil
		}
		if wait, ok := g.inflight[key]; ok {
			g.mu.Unlock()
			// 有人正在执行同一个 key: 等它完成再回头取缓存。
			//
			// 必须同时监听 ctx.Done(): 若那个执行者卡住, 这里不能无限期挂着 ——
			// 调用方的超时/取消语义必须能穿透等待。
			select {
			case <-wait:
				continue // 回到循环顶部, 这次应该能命中了
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
		}
		wait := make(chan struct{})
		g.inflight[key] = wait
		g.misses++
		g.mu.Unlock()

		res, err := fn()

		g.mu.Lock()
		delete(g.inflight, key)
		if err == nil && res != nil {
			// 只在成功时落缓存（见类型注释）。写之前检查上限。
			if g.maxEntries > 0 && len(g.done) >= g.maxEntries {
				// 清空重来: 见 maxEntries 注释。不清 inflight —— 那些还在跑。
				g.done = make(map[string]*ToolResult, g.maxEntries)
			}
			g.done[key] = copyResult(res)
		}
		close(wait)
		g.mu.Unlock()
		return res, false, err
	}
}

// Stats 返回 (命中, 未命中) 计数。
func (g *IdempotencyGate) Stats() (hits, misses int) {
	if g == nil {
		return 0, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits, g.misses
}

// Size 返回已缓存条目数。
func (g *IdempotencyGate) Size() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.done)
}

// copyResult 浅拷贝 ToolResult。
//
// Images 切片必须**另起一份底层数组**：浅拷贝结构体只会复制 slice header, 两个
// 结果共用同一段底层数组 —— 调用方 append 到 Images 会写到共享数组的下一格,
// 而那一格可能正是另一个结果的元素。Content 是 string（不可变），直接复制即可。
func copyResult(r *ToolResult) *ToolResult {
	if r == nil {
		return nil
	}
	cp := *r
	if r.Images != nil {
		cp.Images = append([]types.MediaSource(nil), r.Images...)
	}
	return &cp
}
