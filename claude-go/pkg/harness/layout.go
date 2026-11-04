package harness

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/anthropic/claude-go/pkg/types"
)

// DefaultMaxBreakpoints 是 Anthropic 上游单请求 cache_control 断点上限。
//
// 这是 provider 的硬限制而不是本包的策略：超过时上游会让**整个请求**失败，
// 而不是静默丢掉多余的断点（丢断点会让人误以为缓存生效，实际每次都重新计费）。
// 因此 Build 在发送前就把超限判死，见 Build 的注释。
const DefaultMaxBreakpoints = 4

// ProviderCacheUnknownTokens 表示 provider 侧缓存读写 token **未采集**。
//
// 用 -1 而不是 0：本仓被"恒 0 与未采集长得一样"坑过（见 TurnResult.UsageKnown）。
// 调用方即使忽略 ProviderCacheKnown 标志，也不会把哨兵误读成真实的 0 命中。
const ProviderCacheUnknownTokens = -1

// 失效归因用的稳定段 ID。persona/profile 的版本变更是一次**合法**的缓存失效，
// 业务层补一张画像就该承担这次重算；其余稳定段变化归为 system_prompt，
// 通常是代码/配置漂移，需要人看一眼。
const (
	segmentIDPersona = "persona"
	segmentIDProfile = "profile"
)

// LayoutOptions 是 LayoutBuilder 的构造参数。
type LayoutOptions struct {
	// MaxBreakpoints 是允许的 cache_control 断点数；0 表示取 DefaultMaxBreakpoints。
	// 负数同样回落到默认值：负数没有语义，让它把每个请求都判死不是调用方想要的。
	MaxBreakpoints int

	// ProviderCacheMetrics 读取 provider **真实**返回的 cache read/creation token。
	//
	// ok=false 表示这一次没采到（上游没回 usage、请求失败、字段缺失都算没采到），
	// 此时 Record 报 unknown，而不是 0。nil 等价于永远没采到。
	ProviderCacheMetrics func() (readTokens, creationTokens int, ok bool)

	// OnObservation 接收每一次 Record 产生的观测；可为 nil。
	//
	// 用回调而不是只留 LastObservation，是因为观测通常要直接进 trace/指标，
	// 让调用方自己轮询会丢中间样本。回调在锁外执行，允许它反查 builder。
	OnObservation func(LayoutObservation) error
}

// SegmentInput 是组装 prompt 时交给 builder 的一段输入。
//
// ID 是**失效归因的身份**：同一个 ID 跨轮必须代表同一段语义，否则归因会张冠李戴。
// Version 是显式版本号，改它就等于声明"这次缓存失效是预期的"（R-H2）。
type SegmentInput struct {
	ID      string
	Version string
	Text    string
}

// SegmentSpec 描述一次组装的分层结果。
type SegmentSpec struct {
	// Stable 是稳定前缀，只在版本变更时改变。它的 ID+Version+Text 共同决定 PrefixHash。
	Stable []SegmentInput
	// Dynamic 是断点之后的动态内容（本轮临时上下文、用户输入等），不参与 PrefixHash。
	Dynamic []SegmentInput
	// BreakpointCount 是调用方准备放置的 cache_control 断点数。
	BreakpointCount int
	// InvalidationReason 是调用方显式声明的失效原因；为空则按变更内容推断。
	// 只在 PrefixHash 真的变化时生效 —— 前缀没变却报失效会让归因变成噪音。
	InvalidationReason string
}

// LayoutObservation 是一次布局观测。
//
// 关键设计：本地 hash 与 provider token **分开报**。只报本地 hash"命中"是自欺，
// 那只是本地记账，provider 是否真的读了缓存只有 provider 的 usage 能回答。
// 因此这里不存在合并后的 "hit" 字段 —— 合并完就再也拆不开了。
type LayoutObservation struct {
	// PrefixHash 是本地布局 hash（等于 PromptLayout.PrefixHash）。
	PrefixHash string
	// InvalidationReason 是本轮前缀失效原因；空串表示前缀未变。
	InvalidationReason string
	// BreakpointCount 是本轮实际放置的断点数。
	BreakpointCount int

	// ProviderCacheReadTokens / ProviderCacheCreationTokens 是 provider 真实读数。
	// ProviderCacheKnown=false 时二者恒为 ProviderCacheUnknownTokens（-1），不是 0。
	ProviderCacheReadTokens     int
	ProviderCacheCreationTokens int
	ProviderCacheKnown          bool

	// Usage 是原始 usage，仅作对照。**不要**拿它判断 provider 缓存是否采集到：
	// 未采集时它的零值与真实零消耗无法区分，这正是上面 ProviderCacheKnown 存在的理由。
	Usage types.Usage
}

// LayoutBuilder 把一次组装转成 PromptLayout，并跨轮判定稳定前缀是否失效。
//
// 并发安全：Build 可被多个 goroutine 同时调用。变更检测必须读-比-写在一个临界区里，
// 否则两个并发 Build 会互相把对方当成"上一轮"，归因随机漂移。
type LayoutBuilder struct {
	maxBreakpoints  int
	providerMetrics func() (int, int, bool)
	onObservation   func(LayoutObservation) error

	mu       sync.Mutex
	prev     map[string]SegmentInput // 上一次成功 Build 的稳定段快照，按 ID 索引
	prevHash string                  // 上一次成功 Build 的 PrefixHash
	hasPrev  bool
	last     LayoutObservation
	hasLast  bool
}

// 编译期确认 builder 可以直接注册为布局观测 sink。
var _ PromptLayoutSink = (*LayoutBuilder)(nil)

// NewLayoutBuilder 构造 builder。MaxBreakpoints 为 0（或负数）时取默认上限 4。
func NewLayoutBuilder(opts LayoutOptions) *LayoutBuilder {
	max := opts.MaxBreakpoints
	if max <= 0 {
		max = DefaultMaxBreakpoints
	}
	return &LayoutBuilder{
		maxBreakpoints:  max,
		providerMetrics: opts.ProviderCacheMetrics,
		onObservation:   opts.OnObservation,
	}
}

// MaxBreakpoints 返回本构建器允许的断点数上限（未显式配置时是 DefaultMaxBreakpoints）。
//
// 导出是给装配层的：调用方需要知道该在自己的 prompt 里留几个 cache_control
// 位置，而不是先 Build 一次失败再回读错误。
func (b *LayoutBuilder) MaxBreakpoints() int {
	if b == nil {
		return 0
	}
	return b.maxBreakpoints
}

// Build 计算本轮布局，并与上一次成功 Build 的稳定段比较得出失效原因。
//
// 所有校验都在改状态之前完成：一次失败的 Build 不能被当成"上一轮"，否则下一次
// 成功 Build 会与一个从未真正发送的布局比较，归因和 PrefixHash 都会失真。
func (b *LayoutBuilder) Build(spec SegmentSpec) (PromptLayout, error) {
	n := spec.BreakpointCount
	if n < 0 {
		return PromptLayout{}, fmt.Errorf("harness: prompt layout 断点数不能为负：%d", n)
	}
	// 上游超限是整请求失败，不是降级。必须在发出请求前失败，否则调用方只会看到
	// 一个语义不明的 400，还白白消耗一次往返。
	if n > b.maxBreakpoints {
		return PromptLayout{}, fmt.Errorf(
			"harness: prompt layout 请求 %d 个 cache_control 断点，超过 provider 上限 %d：超限会让上游整请求失败，不会静默丢弃",
			n, b.maxBreakpoints)
	}
	if n > len(spec.Dynamic) {
		return PromptLayout{}, fmt.Errorf(
			"harness: prompt layout 需要 %d 个断点，但动态段只有 %d 个：断点必须落在稳定前缀之后",
			n, len(spec.Dynamic))
	}

	seen := make(map[string]struct{}, len(spec.Stable))
	for _, s := range spec.Stable {
		if _, dup := seen[s.ID]; dup {
			return PromptLayout{}, fmt.Errorf(
				"harness: prompt layout 稳定段 ID %q 重复：失效归因按 ID 判定，重复 ID 会让归因失真", s.ID)
		}
		seen[s.ID] = struct{}{}
	}

	layout := PromptLayout{
		StableSegments:  buildSegments(spec.Stable),
		DynamicSegments: buildSegments(spec.Dynamic),
		PrefixHash:      hashStablePrefix(spec.Stable),
		Breakpoints:     trailingBreakpoints(len(spec.Dynamic), n),
	}

	b.mu.Lock()
	if b.hasPrev && layout.PrefixHash != b.prevHash {
		if spec.InvalidationReason != "" {
			layout.InvalidationReason = spec.InvalidationReason
		} else {
			layout.InvalidationReason = inferInvalidation(spec.Stable, b.prev)
		}
	}
	// 首轮没有可比对象，InvalidationReason 保持空串；即使调用方传了显式原因也忽略，
	// 因为"第一次建缓存"不是失效。
	b.prev = snapshotStable(spec.Stable)
	b.prevHash = layout.PrefixHash
	b.hasPrev = true
	b.mu.Unlock()

	return layout, nil
}

// RecordPromptLayout 实现 PromptLayoutSink。
//
// 本地 hash 与 provider 真实缓存读写分开报；provider 侧没采到时置 unknown 哨兵。
// usage 参数原样带上仅供对照，不作为 provider 真相来源（见 LayoutObservation.Usage）。
func (b *LayoutBuilder) RecordPromptLayout(_ context.Context, layout PromptLayout, usage types.Usage) error {
	obs := LayoutObservation{
		PrefixHash:                  layout.PrefixHash,
		InvalidationReason:          layout.InvalidationReason,
		BreakpointCount:             len(layout.Breakpoints),
		ProviderCacheReadTokens:     ProviderCacheUnknownTokens,
		ProviderCacheCreationTokens: ProviderCacheUnknownTokens,
		ProviderCacheKnown:          false,
		Usage:                       usage,
	}
	if b.providerMetrics != nil {
		read, creation, ok := b.providerMetrics()
		if ok {
			obs.ProviderCacheReadTokens = read
			obs.ProviderCacheCreationTokens = creation
			obs.ProviderCacheKnown = true
		}
	}

	b.mu.Lock()
	b.last = obs
	b.hasLast = true
	b.mu.Unlock()

	// 回调在锁外执行：它可能反查 LastObservation 或写 trace，持锁调用会自锁。
	if b.onObservation != nil {
		if err := b.onObservation(obs); err != nil {
			return err
		}
	}
	return nil
}

// Record 是 RecordPromptLayout 的简写，便于业务侧直接调用。
func (b *LayoutBuilder) Record(ctx context.Context, layout PromptLayout, usage types.Usage) error {
	return b.RecordPromptLayout(ctx, layout, usage)
}

// LastObservation 返回最近一次 Record 的观测；尚未 Record 时 ok=false。
func (b *LayoutBuilder) LastObservation() (LayoutObservation, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last, b.hasLast
}

// buildSegments 把输入转成契约段：Hash 只取 Text，Bytes 取字节长度。
//
// Hash 不含 Version：版本已经参与 PrefixHash，段内 hash 描述的是"这段字节有没有变"，
// 两者混在一起会让"仅升版本号、内容没变"看起来像内容变化。
func buildSegments(in []SegmentInput) []PromptSegment {
	if len(in) == 0 {
		return nil
	}
	out := make([]PromptSegment, len(in))
	for i, s := range in {
		out[i] = PromptSegment{
			ID:      s.ID,
			Version: s.Version,
			Hash:    hashText(s.Text),
			Bytes:   len(s.Text),
		}
	}
	return out
}

// hashStablePrefix 只哈希稳定段。
//
// 编码对每个字段做 8 字节大端长度前缀，并先写段数：否则 ["ab","c"] 与 ["a","bc"]
// 拼接后的字节流完全相同，不同布局会撞出同一个 PrefixHash，缓存"命中"就成了假的。
// 顺序敏感是刻意的：调换稳定段顺序会改变实际请求字节，必须视为失效。
func hashStablePrefix(stable []SegmentInput) string {
	h := sha256.New()
	var buf [8]byte
	writeLen := func(n int) {
		binary.BigEndian.PutUint64(buf[:], uint64(n))
		h.Write(buf[:])
	}
	writeField := func(s string) {
		writeLen(len(s))
		h.Write([]byte(s))
	}

	writeLen(len(stable))
	for _, s := range stable {
		writeField(s.ID)
		writeField(s.Version)
		writeField(s.Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashText 是单段文本的稳定哈希（sha256，十六进制）。
func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// trailingBreakpoints 取动态段的尾部 n 个下标。
//
// 断点标记的是"缓存到哪个前缀为止"，放在尾部覆盖最长的前缀，断点之间也天然递增。
func trailingBreakpoints(dynamicLen, n int) []int {
	if n <= 0 {
		return nil
	}
	out := make([]int, n)
	base := dynamicLen - n
	for i := 0; i < n; i++ {
		out[i] = base + i
	}
	return out
}

// inferInvalidation 按变更内容推断失效原因。
//
// 优先级 persona > profile > system：一次 Build 里同时改了画像和系统提示时，
// 报更具体的那个，业务层才能判断"这次失效是我要的"。缺段/多段也视为变更。
func inferInvalidation(current []SegmentInput, prev map[string]SegmentInput) string {
	var persona, profile, other bool

	for _, s := range current {
		old, existed := prev[s.ID]
		if existed && old == s {
			continue
		}
		oldVersion := ""
		if existed {
			oldVersion = old.Version
		}
		switch {
		case s.ID == segmentIDPersona && oldVersion != s.Version:
			persona = true
		case s.ID == segmentIDProfile && oldVersion != s.Version:
			profile = true
		default:
			other = true
		}
	}
	for id := range prev {
		if !containsStable(current, id) {
			switch id {
			case segmentIDPersona:
				persona = true
			case segmentIDProfile:
				profile = true
			default:
				other = true
			}
		}
	}

	switch {
	case persona:
		return InvalidationPersona
	case profile:
		return InvalidationProfile
	case other:
		return InvalidationSystem
	default:
		// 走不到这里：调用方只在 PrefixHash 变化时才归因。留作兜底，
		// 比如未来哈希加入新字段而这里忘了同步。
		return InvalidationSystem
	}
}

// containsStable 判断当前稳定段里是否还有该 ID。
func containsStable(current []SegmentInput, id string) bool {
	for _, s := range current {
		if s.ID == id {
			return true
		}
	}
	return false
}

// snapshotStable 复制稳定段快照。
//
// 必须复制：SegmentSpec 的切片由调用方持有，复用同一底层数组（很常见：改一个字段
// 再 Build）时，若直接存引用，整张 prev 会跟着变，"上一轮"就成了"这一轮"。
func snapshotStable(stable []SegmentInput) map[string]SegmentInput {
	out := make(map[string]SegmentInput, len(stable))
	for _, s := range stable {
		out[s.ID] = s
	}
	return out
}
