// projection.go — F11 asOf 投影 (dsh session-projection 的 Go 侧适配)。
//
// 规格原文 (docforge planning-dsh-adopt 13.6.3): "ProjectionDefinition fold +
// asOfSeq 时间旅行 (任意历史时刻的会话视图)"。TraceStore 支持按 run 还原,
// 但"复现当时上下文"只能靠 span 拼装 —— 学习管线每次都要重新手写一次拼装。
//
// dsh 语义锚点 (packages/session/session-projection):
//   - ProjectionDefinition: 一个纯同步 fold 单元 (init + apply), 不订阅不轮询,
//     只声明"给定状态+事件→下一个状态"。
//   - whole-value rule: 状态承载事件必须带**变更后的完整状态**, 绝不带裸增量
//     —— 每个 fold 的迁移因此天然幂等可重放。
//   - asOfSeq 水位: 投影值与水位来自同一日志切点, "读到 seq=N 的视图"自述
//     自己看到了哪条事件为止。
//
// 本仓对应物: tracestore 的 Log (bucket per TraceID) 就是事件流, Span 就是
// 事件。泛型 Projection[K, E] 让任意域声明自己的 fold; asOf 投影按 seq 截流
// 重放, 任意历史时刻的会话/运行视图可机械重建。
package tracestore

import "fmt"

// ProjectionDefinition 一个投影单元: 纯 fold + 键名 + 状态版本。
//
// apply 必须是纯函数 (不碰 IO/时间/全局) —— asOf 重放的正确性完全押在它上面。
// 对不关心的事件返回原 state 即可 (Go 无 Object.is, 但返回同一引用让调用方
// 能跳过深拷贝)。
type ProjectionDefinition[S any, E any] struct {
	// Key 投影键 (域内唯一, 如 "context" / "tool_timeline")。
	Key string
	// StateVersion 状态结构版本。checkpoint 恢复时版本不匹配即弃用旧状态从
	// init 重放 (对齐 dsh restore 的 ver 校验纪律)。
	StateVersion int
	// Init 空日志的初始状态。
	Init func() S
	// Apply 纯迁移: 前一状态 + 一个事件 → 下一状态。
	Apply func(state S, event E) S
}

// SpanFold 是作用在 Span 上的 ProjectionDefinition 别名 (本仓最常见形态)。
type SpanFold = ProjectionDefinition[any, Span]

// Projection 一个已装配的投影器: 把 fold 应用到 tracestore Log 上,
// 支持任意 asOf 切点重放。
type Projection[S any] struct {
	store *Store
	def   ProjectionDefinition[S, Span]
}

// NewProjection 装配一个投影器。def.Apply 必须是纯函数。
func NewProjection[S any](store *Store, def ProjectionDefinition[S, Span]) (*Projection[S], error) {
	if store == nil {
		return nil, ErrNoProjection
	}
	if def.Init == nil || def.Apply == nil || def.Key == "" {
		return nil, fmt.Errorf("tracestore: 投影 %q 装配不完整 (Key/Init/Apply 必填)", def.Key)
	}
	return &Projection[S]{store: store, def: def}, nil
}

// Projected 一次投影的结果: 值 + 水位 (自述看到了哪条事件为止)。
type Projected[S any] struct {
	// AsOfSeq 水位: 本结果覆盖到 Span.Seq 之前的全部事件 (Log 无 seq 字段,
	// 以 0 基写入序号代之 —— Log.Append 顺序即全局序, 语义与 dsh seq 一致)。
	AsOfSeq int
	// State fold 后的状态值。
	State S
	// Count 该切点之前实际 apply 的事件数 (与 AsOfSeq 相等; 保留双字段是为了
	// 将来 Log 引入 seq 字段时两者可分叉 —— 水位是日志位置, 计数是Fold 事实)。
	Count int
}

// AsOf 在指定切点重放: 取 TraceID 的全部 Span, apply 到第 asOf 条 (0 基,
// 不含)为止。asOf<0 或超过事件总数时取到日志末尾 (全量视图)。
//
// 时间旅行 = 只重放前缀: Span 的写入序号稳定 (append-only log), 同一切点
// 重放必然得到同一状态 (fold 纯函数 + 全量事件确定)。
func (p *Projection[S]) AsOf(traceID string, asOf int) (Projected[S], error) {
	spans, err := p.store.ReadRun(traceID)
	if err != nil {
		var zero Projected[S]
		return zero, err
	}
	return foldSpans(p.def, spans, asOf), nil
}

// Latest 投影到日志末尾 (当前视图)。
func (p *Projection[S]) Latest(traceID string) (Projected[S], error) {
	return p.AsOf(traceID, -1)
}

// foldSpans 纯重放: spans[:cut] 逐个 apply。cut<=0 且 asOf<0 → 全量;
// cut 超界取末尾。
func foldSpans[S any](def ProjectionDefinition[S, Span], spans []Span, asOf int) Projected[S] {
	cut := len(spans)
	if asOf >= 0 && asOf < cut {
		cut = asOf
	}
	state := def.Init()
	for i := 0; i < cut; i++ {
		state = def.Apply(state, spans[i])
	}
	return Projected[S]{AsOfSeq: cut, State: state, Count: cut}
}

// ProjectionCheckpoint 单元的持久化检查点行 (dsh restore 的 (key→{ver,seq,val}))。
type ProjectionCheckpoint[S any] struct {
	Key          string `json:"key"`
	StateVersion int    `json:"ver"`
	Seq          int    `json:"seq"`
	State        S      `json:"val"`
}

// Restore 从检查点续放: 校验版本与水位可用性, 可用则从 checkpoint.Seq 起重放
// 后缀, 不可用则从 init 全量重放。返回的 asOf 是日志末尾水位。
//
// 与 dsh restore 语义的差异 (有意为之): dsh 在 baseSeq>0 且 checkpoint 不可用
// 时 fail-loud, 因为它的持久化行是唯一非重放来源; 本仓 Span 全文都在 Log 里,
// 全量重放永远是可行兜底, "静默降级为重放"比"报错让调用方重来"更贴合本仓
// "轨迹缺失不影响交付"的既有纪律。
func Restore[S any](p *Projection[S], ckpt ProjectionCheckpoint[S], traceID string) (Projected[S], error) {
	if p == nil {
		var zero Projected[S]
		return zero, ErrNoProjection
	}
	spans, err := p.store.ReadRun(traceID)
	if err != nil {
		var zero Projected[S]
		return zero, err
	}
	usable := ckpt.StateVersion == p.def.StateVersion && ckpt.Seq >= 0 && ckpt.Seq <= len(spans)
	from, state := 0, p.def.Init()
	if usable {
		from, state = ckpt.Seq, ckpt.State
	}
	for i := from; i < len(spans); i++ {
		state = p.def.Apply(state, spans[i])
	}
	return Projected[S]{AsOfSeq: len(spans), State: state, Count: len(spans)}, nil
}

// ErrNoProjection nil store 上调用的哨兵 (与 tracestore 其余 nil 安全不同,
// 投影必须显式失败 —— nil store 意味着装配错误而非"没有数据")。
var ErrNoProjection = fmt.Errorf("tracestore: projection on nil store")
