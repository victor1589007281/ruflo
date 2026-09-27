package feishu

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/sessionapi"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// session_host.go — 把 SessionManager 适配成 sessionapi.Host。
//
// # 为什么是适配器而不是让 SessionManager 直接实现 Host
//
// Host 的方法名 (Get / List / Clear / Run …) 与 SessionManager 已有的方法重名但
// **语义不同**: sm.Get(chatID) 返回 *Session (内存对象), 而 Host.Get 要返回 Spec
// (身份声明, 且不存在时必须是 ErrNotFound)。同名不同义是最难查的一类错 ——
// 编译器不会拦, 因为签名确实不同, 但读代码的人会一次次读错。
//
// 所以这里刻意用一个独立的、只做转发的类型: 这一层薄到可以一眼看完, 接缝只有一处。
type sessionHost struct {
	sm *SessionManager
}

// SessionHost 返回会话 HTTP 面的 Host 适配器。
//
// 由 Bot.Sessions() 交出去 (见 bot.go), 再经 main.go 的 APIExtensions 挂到 wiki
// 的 mux 上 —— 于是它自动落在 httpauth.Middleware 的覆盖范围里。
func (sm *SessionManager) SessionHost() sessionapi.Host {
	if sm == nil {
		return nil
	}
	return &sessionHost{sm: sm}
}

// Ensure 建立或更新一个会话的身份声明。
//
// 若该会话**已在内存里活着**, 这里会把它卸载 —— 因为身份 (人格 / 白名单 / cwd /
// 模型) 全部参与引擎构造, 而引擎是构造时定型的。改 spec 却留着一个按旧 spec 构造
// 的引擎, 是"配了但没生效"最典型的一种: 接口返回 201、spec 也对, 行为纹丝不动。
//
// 卸载是安全的: 对话历史在 transcript 快照里 (evictOldest 的注释里写着同一条纪律),
// 下次说话时按新 spec 重建引擎并把历史读回来。
func (h *sessionHost) Ensure(sp sessionapi.Spec) error {
	sm := h.sm
	if !ValidSessionID(sp.ID) {
		return fmt.Errorf("非法会话 id %q: 只允许字母数字与 _ . -, 2~64 字符, 且不以 . 开头", sp.ID)
	}
	if sm.isProcessing(sp.ID) {
		return fmt.Errorf("会话 %s 正在处理消息, 改身份会打断它: 请等这一轮结束后再改", sp.ID)
	}
	if err := sm.specs.put(sp); err != nil {
		return fmt.Errorf("会话声明落盘失败: %w", err)
	}
	sm.mu.Lock()
	delete(sm.sessions, sp.ID)
	sm.mu.Unlock()
	sm.dropAdvisorTool(sp.ID)
	log.Printf("[Session] 已登记会话身份: %s", specString(sp))
	return nil
}

func (h *sessionHost) List() []sessionapi.Spec {
	sm := h.sm
	out := sm.specs.all()
	seen := make(map[string]bool, len(out))
	for _, sp := range out {
		seen[sp.ID] = true
	}
	// 内存里活着但**没有 spec** 的会话也要列出来 (飞书聊天就是这一类): 它们确实
	// 是会话, 只是没有人给它们声明过身份。不列的话, 运维看到的"会话列表"里
	// 会缺掉正在跑的那几个 —— 而缺掉的东西没有任何提示。
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for id, sess := range sm.sessions {
		if seen[id] {
			continue
		}
		out = append(out, sessionapi.Spec{ID: id, Name: sess.displayName()})
	}
	return out
}

func (h *sessionHost) Get(id string) (sessionapi.Spec, error) {
	if sp, ok := h.sm.specs.get(id); ok {
		return sp, nil
	}
	// 没有 spec 但在内存里活着 → 给一个只带 id/名的空壳, 而不是 404:
	// 该会话**确实存在**, 报 404 会让调用方以为可以拿它建新会话。
	if sess := h.sm.Get(id); sess != nil {
		return sessionapi.Spec{ID: id, Name: sess.displayName()}, nil
	}
	return sessionapi.Spec{}, sessionapi.ErrNotFound
}

// Delete 删掉身份声明 (不动对话历史 —— 那是 Clear 的事)。
//
// 语义按 sessionapi 的划分: Delete 删的是"这个员工是谁", Clear 清的是"它记得什么"。
func (h *sessionHost) Delete(id string) error {
	if _, err := h.Get(id); err != nil {
		return err
	}
	if h.sm.isProcessing(id) {
		return fmt.Errorf("会话 %s 正在处理消息, 请等这一轮结束后再删", id)
	}
	if err := h.sm.specs.delete(id); err != nil {
		return err
	}
	h.sm.mu.Lock()
	delete(h.sm.sessions, id)
	h.sm.mu.Unlock()
	h.sm.dropAdvisorTool(id)
	return nil
}

// Clear 清空对话历史, 身份保留。
//
// 直接复用 ClearSession —— 它是唯一允许删 transcript 快照的路径
// (session.go:1162 的注释: evictOldest/cleanup 只卸内存, 只有 /clear 代表
// 用户显式"忘记")。这里不自造一套: 两套"清空"迟早会在行为上分叉。
func (h *sessionHost) Clear(id string) error {
	if _, err := h.Get(id); err != nil {
		return err
	}
	h.sm.ClearSession(id)
	return nil
}

// Busy 只读探测。
func (h *sessionHost) Busy(id string) bool { return h.sm.isProcessing(id) }

// Reserve 占住会话并分配这次运行的 runID。
//
// ★ runID 在**这里**分配而不是由 HTTP 层编: 它要同时被写进 ctx 的 trace.IDs.RunID
// (决定轨迹桶名) 并回给调用方当轮询键。两处必须是同一个字符串, 否则轮询拿到的
// runId 去 ReadRun 会读回空 —— 而"读不到"与"没采集"在读数上一模一样。
//
// 占位标志 (reservedRunID) 在这里同步置上, 由 runSessionMessage 的收尾 defer 释放
// (与 SetProcessing(false) 同一处)。见 Session.reservedRunID 的注释。
func (h *sessionHost) Reserve(id string) (string, error) {
	sess := h.sm.ensureSession(id)

	// ★ 占位标志的置上必须与"是否已忙"的判断在**同一个临界区**里 —— 那正是它存在
	// 的理由 (见 sessionapi.Host.Reserve 的注释)。
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.processing || sess.reservationLive(time.Now()) {
		return "", sessionapi.ErrBusy
	}
	// runID 在这里分配: 它要同时写进 ctx 的 trace.IDs.RunID (决定轨迹桶名) 并回给
	// 调用方当轮询键, 两处必须逐字节相同 (见本函数上面的注释)。
	runID := fmt.Sprintf("run-%s-%d", id, h.sm.nextRunSeq())
	sess.reservedRunID = runID
	sess.reservedAt = time.Now()
	return runID, nil
}

// Run 在会话里跑一条消息。本方法的调用方 (sessionapi) 保证同会话不会并发进来
// (它靠 Reserve 把这一点变成原子的)。
//
// ★ 它**不**在这里释放占位: 释放的唯一出口是 runSessionMessage 的收尾 defer
// (经 beginSessionRun 返回的 endRun, 见 session_trace.go)。两处都放会变成
// "谁先跑谁说了算", 而少放一处的后果是那个会话永久 409 —— 表象与"会话卡住了"
// 完全一样, 极难归因。单出口 + 租约兜底 (reservationLive) 才既简单又自愈。
func (h *sessionHost) Run(ctx context.Context, id, text string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return h.sm.ProcessMessage(ctx, id, text)
}

// RunStream 实现 sessionapi.StreamHost (G1): 跑一条消息并把中间事件
// (文本增量/工具调用) 实时 emit 给调用方。回复文本的语义与 Run 完全一致
// (同样的 trace 作用域、处理中标志、记忆沉淀), 只是多了个事件出口。
func (h *sessionHost) RunStream(ctx context.Context, id, text string, emit func(sessionapi.Event)) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return h.sm.ProcessMessageStream(ctx, id, text, emit)
}

// Inject 实现 sessionapi.InjectHost (G3): 往**正在跑的** run 注入一条 user 消息。
// 会话空闲 (引擎没有循环在跑) 时返回 false —— 见 engine.InjectQueue.Inject 的注释:
// "注入一个空闲引擎"与"开一轮新的"是两件不同的事, 静默降级是语义替换。
func (h *sessionHost) Inject(id, text string) bool {
	sess := h.sm.Get(id)
	if sess == nil || sess.Engine == nil {
		return false
	}
	return sess.Engine.InjectMessage(midRunMailMessage(text))
}

// Trace 实现 sessionapi.TraceHost (G2): 导出一个 run 的全部 span,
// InputRef/OutputRef 解析成内联文本 (超长按 blob 原样引用, 由调用方决定)。
func (h *sessionHost) Trace(runID string) ([]tracestore.Span, error) {
	if h.sm.traceStore == nil {
		return nil, fmt.Errorf("轨迹底座未初始化")
	}
	spans, err := h.sm.traceStore.ReadRun(runID)
	if err != nil {
		return nil, err
	}
	for i := range spans {
		resolveRefInline(h.sm.traceStore, &spans[i].InputRef)
		resolveRefInline(h.sm.traceStore, &spans[i].OutputRef)
	}
	return spans, nil
}

// traceResolveCap 轨迹导出时单条正文内联上限 (64KiB)。更大的正文留在 blob 里,
// 导出去的 JSON 保留 size 供调用方判断"被截了" —— 一份评价素材 10MB 的 prompt
// 全文没有任何帮助, 只会先把读它的人 (或 LLM 评委) 的上下文撑爆。
const traceResolveCap = 64 * 1024

func resolveRefInline(store *tracestore.Store, ref *tracestore.Ref) {
	if ref == nil || store == nil || ref.Blob == "" || ref.Inline != "" {
		return
	}
	text, err := store.Resolve(*ref)
	if err != nil {
		return // blob 读不到: 保留原样 (Size 仍在), 不因一条 blob 坏掉整个导出
	}
	if len(text) > traceResolveCap {
		text = text[:traceResolveCap] + "\n…[截断]"
	}
	ref.Inline = text
}

// Tokens 从轨迹底座累加这次运行的 token。采不到返回 (nil, nil) —— 见
// sessionapi.TokensFromTrace 的注释 ("没采集" 与 "消耗 0" 必须可分辨)。
func (h *sessionHost) Tokens(runID string) (*sessionapi.Usage, error) {
	return sessionapi.TokensFromTrace(h.sm.traceStore, runID)
}

// ── SessionManager 上的支撑方法 ─────────────────────────────────────────────

// ensureSession 取得 (必要时创建) 会话对象。
//
// 名字刻意不叫 getOrCreateLocked: 它**不**在调用方持锁的前提下被调用, 自己取锁。
// Reserve 要的原子性是"占位标志"那一把 (在 Session.mu 上), 与"会话对象在不在 map 里"
// 是两把不相干的锁 —— 混在一起说会让人以为这里也有一处竞态。
func (sm *SessionManager) ensureSession(chatID string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if s, ok := sm.sessions[chatID]; ok {
		s.Touch()
		return s
	}
	if len(sm.sessions) >= sm.maxSessions {
		sm.evictOldest()
	}
	s := sm.createSession(chatID)
	sm.sessions[chatID] = s
	return s
}

// isProcessing 会话是否忙 (含已占位但尚未开跑的那一段)。
//
// ★ 两个条件缺一不可: 只看 processing 会在"Reserve 已成功、Run 还没把 processing
// 置起来"的那个窗口里报"闲" —— 那个窗口正是 409 要挡的东西。
func (sm *SessionManager) isProcessing(chatID string) bool {
	if sm == nil {
		return false
	}
	s := sm.Get(chatID)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processing || s.reservationLive(time.Now())
}

// reservationLive 占位是否仍然有效 (需在 s.mu 下调用)。
//
// # 为什么要租约而不是"占上就一直算"
//
// 占位的释放只有一个出口 (runSessionMessage 的收尾 defer)。那个出口在正常路径上
// 一定会走到, 但有一种路径走不到: Reserve 成功之后、Run 还没来得及把请求交给
// runSessionMessage 之前, 进程崩了 / 请求被取消了。那样占位就永远留着, 这个会话
// 此后恒 409 —— 而"永久忙"与"正在跑一个很长的任务"在外部读数上完全一样。
//
// 租约让这种占位自己过期。超时值取得远大于任何单次运行 (见 reservationLease),
// 所以正常运行永远不会被误判成过期。
func (s *Session) reservationLive(now time.Time) bool {
	if s.reservedRunID == "" {
		return false
	}
	return now.Sub(s.reservedAt) < reservationLease
}

// reservationLease 占位租约时长。
//
// 取得比"单次运行的真实上界"还长: 平台侧一次真实巡检 223 秒, 团队 run 可以到
// 十几分钟。租约太短会把一个**正在跑**的会话判成空闲, 于是第二条消息挤进来 ——
// 那不是"自愈", 那是把 409 的保证悄悄取消掉。宁可让一次崩溃后的占位多留一会儿
// (代价: 那一个会话等这么久), 也不要在正常运行时误判。
const reservationLease = 2 * time.Hour

// releaseReserved 清掉占位 (由 runSessionMessage 的收尾 defer 调用)。
func (s *Session) releaseReserved() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reservedRunID = ""
	s.reservedAt = time.Time{}
}

// reservedRunIDFor 读占位里分配的 runID (不清除 —— 清除在收尾 defer 里做,
// 这样"已占位"这个事实在整个运行期间都成立)。
func (s *Session) reservedRunIDFor() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reservedRunID
}

// specFor 取会话的 spec (没有则返回零值 + false)。
func (sm *SessionManager) specFor(chatID string) (sessionapi.Spec, bool) {
	if sm == nil || sm.specs == nil {
		return sessionapi.Spec{}, false
	}
	return sm.specs.get(chatID)
}

// pinnedProfileFor 取 spec 钉死的工具档位 (Spec.ToolProfile)。
// 返回 ok=false 表示没钉 —— 调用方维持按消息文本的启发式。
func (sm *SessionManager) pinnedProfileFor(chatID string) (builtin.ToolProfile, bool) {
	sp, ok := sm.specFor(chatID)
	if !ok || sp.ToolProfile == "" {
		return "", false
	}
	return builtin.NormalizeToolProfile(builtin.ToolProfile(sp.ToolProfile)), true
}
