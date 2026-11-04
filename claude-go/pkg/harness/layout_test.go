package harness

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// stable 是测试用的稳定前缀；persona/profile 的 ID 与失效归因对应。
func stable(personaVersion, profileVersion, systemText string) []SegmentInput {
	return []SegmentInput{
		{ID: "persona", Version: personaVersion, Text: "你是墨七，一个陪伴型角色。"},
		{ID: "profile", Version: profileVersion, Text: "用户偏好简短回答。"},
		{ID: "system", Version: "v1", Text: systemText},
	}
}

// TestLayoutPrefixHashStableAcrossIdenticalBuilds 覆盖验收第 1 条：
// 稳定段 ID+Version+Text 全不变时，两次 Build 的 PrefixHash 必须逐字节相同。
func TestLayoutPrefixHashStableAcrossIdenticalBuilds(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	spec := SegmentSpec{
		Stable:          stable("p1", "u1", "工具说明"),
		Dynamic:         []SegmentInput{{ID: "turn", Text: "今天有点累"}},
		BreakpointCount: 1,
	}

	l1, err := b.Build(spec)
	if err != nil {
		t.Fatalf("第一次 Build 失败: %v", err)
	}
	l2, err := b.Build(spec)
	if err != nil {
		t.Fatalf("第二次 Build 失败: %v", err)
	}
	if l1.PrefixHash == "" {
		t.Fatal("PrefixHash 不应为空")
	}
	if l1.PrefixHash != l2.PrefixHash {
		t.Fatalf("稳定段未变但 PrefixHash 变了: %s != %s", l1.PrefixHash, l2.PrefixHash)
	}
	if l2.InvalidationReason != "" {
		t.Fatalf("前缀未变却报了失效原因: %q", l2.InvalidationReason)
	}
}

// TestLayoutDynamicChangeKeepsPrefixHash 覆盖验收第 2 条：只改动态段不得改变 PrefixHash。
func TestLayoutDynamicChangeKeepsPrefixHash(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	st := stable("p1", "u1", "工具说明")

	l1, err := b.Build(SegmentSpec{Stable: st, Dynamic: []SegmentInput{{ID: "turn", Text: "第一轮"}}})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	l2, err := b.Build(SegmentSpec{Stable: st, Dynamic: []SegmentInput{{ID: "turn", Text: "完全不同的第二轮内容"}}})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if l1.PrefixHash != l2.PrefixHash {
		t.Fatalf("动态段变化影响了 PrefixHash: %s != %s", l1.PrefixHash, l2.PrefixHash)
	}
	if l2.InvalidationReason != "" {
		t.Fatalf("仅动态段变化不应报失效，实得 %q", l2.InvalidationReason)
	}
}

// TestLayoutFirstBuildHasNoInvalidation 覆盖：首次 Build 没有可比对象，原因为空。
func TestLayoutFirstBuildHasNoInvalidation(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	l, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s"), InvalidationReason: InvalidationPersona})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if l.InvalidationReason != InvalidationNone {
		t.Fatalf("首轮不应报失效，实得 %q", l.InvalidationReason)
	}
}

// TestLayoutPersonaVersionChange 覆盖：persona 版本变更 → PrefixHash 变且归因 persona。
func TestLayoutPersonaVersionChange(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})

	l1, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s")})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	l2, err := b.Build(SegmentSpec{Stable: stable("p2", "u1", "s")})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if l1.PrefixHash == l2.PrefixHash {
		t.Fatal("persona 版本变更后 PrefixHash 未变")
	}
	if l2.InvalidationReason != InvalidationPersona {
		t.Fatalf("期望归因 %q，实得 %q", InvalidationPersona, l2.InvalidationReason)
	}
}

// TestLayoutProfileVersionChange 覆盖：profile 版本变更 → 归因 profile。
func TestLayoutProfileVersionChange(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	if _, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s")}); err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	l, err := b.Build(SegmentSpec{Stable: stable("p1", "u2", "s")})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if l.InvalidationReason != InvalidationProfile {
		t.Fatalf("期望归因 %q，实得 %q", InvalidationProfile, l.InvalidationReason)
	}
}

// TestLayoutSystemTextChange 覆盖：其他稳定段文本变更 → 归因 system_prompt。
func TestLayoutSystemTextChange(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	if _, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "旧工具说明")}); err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	l, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "新工具说明")})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if l.InvalidationReason != InvalidationSystem {
		t.Fatalf("期望归因 %q，实得 %q", InvalidationSystem, l.InvalidationReason)
	}
}

// TestLayoutExplicitReasonWins 覆盖：调用方显式原因优先于推断。
func TestLayoutExplicitReasonWins(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	if _, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s")}); err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	l, err := b.Build(SegmentSpec{
		Stable:             stable("p1", "u1", "s2"),
		InvalidationReason: InvalidationExplicit,
	})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if l.InvalidationReason != InvalidationExplicit {
		t.Fatalf("显式原因应优先，期望 %q，实得 %q", InvalidationExplicit, l.InvalidationReason)
	}
}

// TestLayoutLengthPrefixPreventsCollision 覆盖：长度前缀保证字段拼接不撞哈希。
// 若直接拼接 ID+Version+Text，"ab"+"c" 与 "a"+"b"+"c" 字节流相同。
func TestLayoutLengthPrefixPreventsCollision(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	l1, err := b.Build(SegmentSpec{Stable: []SegmentInput{
		{ID: "ab", Version: "", Text: "c"},
	}})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	l2, err := b.Build(SegmentSpec{Stable: []SegmentInput{
		{ID: "a", Version: "b", Text: "c"},
	}})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if l1.PrefixHash == l2.PrefixHash {
		t.Fatal("字段边界不同的布局撞出了相同 PrefixHash，长度前缀失效")
	}
}

// TestLayoutOrderSensitive 覆盖：稳定段顺序变化必须改变 hash（请求字节确实变了）。
func TestLayoutOrderSensitive(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	st := stable("p1", "u1", "s")
	l1, err := b.Build(SegmentSpec{Stable: st})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	rev := []SegmentInput{st[2], st[1], st[0]}
	l2, err := b.Build(SegmentSpec{Stable: rev})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if l1.PrefixHash == l2.PrefixHash {
		t.Fatal("稳定段顺序变化后 PrefixHash 未变")
	}
}

// TestLayoutBreakpointLimitRejected 覆盖：超上限必须在 Build 阶段报错，
// 且错误里要能被调用方看到"上限"和"请求数"。
func TestLayoutBreakpointLimitRejected(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{MaxBreakpoints: 2})
	dyn := []SegmentInput{
		{ID: "d1", Text: "a"}, {ID: "d2", Text: "b"}, {ID: "d3", Text: "c"},
	}
	_, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s"), Dynamic: dyn, BreakpointCount: 3})
	if err == nil {
		t.Fatal("断点数超上限应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "2") || !strings.Contains(msg, "3") {
		t.Fatalf("错误信息应同时包含上限与请求数，实得: %s", msg)
	}
	if !strings.Contains(msg, "上限") {
		t.Fatalf("错误信息应点明上限，实得: %s", msg)
	}
}

// TestLayoutDefaultBreakpointLimit 覆盖默认上限为 4（Anthropic 上游限制）。
func TestLayoutDefaultBreakpointLimit(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	dyn := make([]SegmentInput, 5)
	for i := range dyn {
		dyn[i] = SegmentInput{ID: "d", Text: "x"}
	}
	if _, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s"), Dynamic: dyn, BreakpointCount: 4}); err != nil {
		t.Fatalf("4 个断点应被接受，得到: %v", err)
	}
	if _, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s"), Dynamic: dyn, BreakpointCount: 5}); err == nil {
		t.Fatal("5 个断点应超过默认上限")
	}
}

// TestLayoutBreakpointsIndexDynamic 覆盖：断点数精确、下标全部落在动态段内且递增。
func TestLayoutBreakpointsIndexDynamic(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	dyn := []SegmentInput{{ID: "a", Text: "1"}, {ID: "b", Text: "2"}, {ID: "c", Text: "3"}}
	l, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s"), Dynamic: dyn, BreakpointCount: 2})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if len(l.Breakpoints) != 2 {
		t.Fatalf("期望 2 个断点，实得 %d", len(l.Breakpoints))
	}
	for i, idx := range l.Breakpoints {
		if idx < 0 || idx >= len(l.DynamicSegments) {
			t.Fatalf("断点 %d 下标 %d 越界（动态段 %d）", i, idx, len(l.DynamicSegments))
		}
		if i > 0 && idx <= l.Breakpoints[i-1] {
			t.Fatalf("断点下标必须递增，实得 %v", l.Breakpoints)
		}
	}
}

// TestLayoutBreakpointsExceedDynamicRejected 覆盖：断点多于动态段时同样在发送前失败。
func TestLayoutBreakpointsExceedDynamicRejected(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	if _, err := b.Build(SegmentSpec{
		Stable:          stable("p1", "u1", "s"),
		Dynamic:         []SegmentInput{{ID: "only", Text: "x"}},
		BreakpointCount: 2,
	}); err == nil {
		t.Fatal("断点数超过动态段数应报错")
	}
}

// TestLayoutSegmentHashAndBytes 覆盖：每段 Hash 只取决于文本，Bytes == len(Text)。
func TestLayoutSegmentHashAndBytes(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{})
	l, err := b.Build(SegmentSpec{
		Stable:  []SegmentInput{{ID: "persona", Version: "v1", Text: "你好"}},
		Dynamic: []SegmentInput{{ID: "turn", Text: "累"}},
	})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if len(l.StableSegments) != 1 || len(l.DynamicSegments) != 1 {
		t.Fatalf("段数不符: stable=%d dynamic=%d", len(l.StableSegments), len(l.DynamicSegments))
	}
	if l.StableSegments[0].Bytes != len("你好") {
		t.Fatalf("Bytes 应为字节长度 %d，实得 %d", len("你好"), l.StableSegments[0].Bytes)
	}
	if l.StableSegments[0].Hash == "" {
		t.Fatal("段 Hash 不应为空")
	}
	// 只升版本、文本不变：段内 hash 不变（版本已由 PrefixHash 承担）。
	l2, err := b.Build(SegmentSpec{
		Stable:  []SegmentInput{{ID: "persona", Version: "v2", Text: "你好"}},
		Dynamic: []SegmentInput{{ID: "turn", Text: "累"}},
	})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if l2.StableSegments[0].Hash != l.StableSegments[0].Hash {
		t.Fatal("仅版本变化不应改变段内文本 hash")
	}
	if l2.PrefixHash == l.PrefixHash {
		t.Fatal("版本变化必须改变 PrefixHash")
	}
}

// TestLayoutRecordKnownProviderMetrics 覆盖：provider 真实 token 与本地 hash 分开报。
func TestLayoutRecordKnownProviderMetrics(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{
		ProviderCacheMetrics: func() (int, int, bool) { return 1200, 300, true },
	})
	l, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s"), Dynamic: []SegmentInput{{ID: "t", Text: "x"}}})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	usage := types.Usage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 1200, CacheCreationInputTokens: 300}
	if err := b.Record(context.Background(), l, usage); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}
	obs, ok := b.LastObservation()
	if !ok {
		t.Fatal("Record 后应有观测")
	}
	if obs.PrefixHash != l.PrefixHash {
		t.Fatalf("观测应带本地 PrefixHash，实得 %q", obs.PrefixHash)
	}
	if !obs.ProviderCacheKnown {
		t.Fatal("provider 指标可用时 Known 应为 true")
	}
	if obs.ProviderCacheReadTokens != 1200 || obs.ProviderCacheCreationTokens != 300 {
		t.Fatalf("provider token 未如实上报: read=%d creation=%d", obs.ProviderCacheReadTokens, obs.ProviderCacheCreationTokens)
	}
}

// TestLayoutRecordUnknownProviderMetrics 覆盖：没采到必须报 unknown 哨兵，不能报 0。
func TestLayoutRecordUnknownProviderMetrics(t *testing.T) {
	cases := map[string]*LayoutBuilder{
		"nil metrics": NewLayoutBuilder(LayoutOptions{}),
		"ok=false": NewLayoutBuilder(LayoutOptions{
			ProviderCacheMetrics: func() (int, int, bool) { return 999, 999, false },
		}),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			l, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s")})
			if err != nil {
				t.Fatalf("Build 失败: %v", err)
			}
			// 故意传一个全零 usage：它不得被当成"provider 报了 0"。
			if err := b.Record(context.Background(), l, types.Usage{}); err != nil {
				t.Fatalf("Record 失败: %v", err)
			}
			obs, ok := b.LastObservation()
			if !ok {
				t.Fatal("Record 后应有观测")
			}
			if obs.ProviderCacheKnown {
				t.Fatal("provider 指标未采集时 Known 必须为 false")
			}
			if obs.ProviderCacheReadTokens == 0 || obs.ProviderCacheCreationTokens == 0 {
				t.Fatal("未采集必须用哨兵而不是 0，否则与真实零命中无法区分")
			}
			if obs.ProviderCacheReadTokens != ProviderCacheUnknownTokens || obs.ProviderCacheCreationTokens != ProviderCacheUnknownTokens {
				t.Fatalf("未采集应报 unknown 哨兵 %d，实得 read=%d creation=%d",
					ProviderCacheUnknownTokens, obs.ProviderCacheReadTokens, obs.ProviderCacheCreationTokens)
			}
		})
	}
}

// TestLayoutRecordObserverErrorPropagates 覆盖：观测回调的失败要能冒泡给调用方。
func TestLayoutRecordObserverErrorPropagates(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{
		OnObservation: func(LayoutObservation) error { return errSentinel{msg: "observer failed"} },
	})
	l, err := b.Build(SegmentSpec{Stable: stable("p1", "u1", "s")})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if err := b.Record(context.Background(), l, types.Usage{}); err == nil {
		t.Fatal("观测回调失败应返回 error")
	}
}

type errSentinel struct{ msg string }

func (e errSentinel) Error() string { return e.msg }

// TestLayoutConcurrentBuild 覆盖：Build 的读-比-写在同一临界区，并发下不产生竞态。
// 这里只断言不 panic、无错误、各次哈希与稳定段一致；配合 -race 才有完整意义。
func TestLayoutConcurrentBuild(t *testing.T) {
	b := NewLayoutBuilder(LayoutOptions{MaxBreakpoints: 4})
	st := stable("p1", "u1", "s")
	const n = 32

	var wg sync.WaitGroup
	var mu sync.Mutex
	hashes := make(map[string]struct{}, n)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			l, err := b.Build(SegmentSpec{
				Stable:          st,
				Dynamic:         []SegmentInput{{ID: "turn", Text: "x"}},
				BreakpointCount: 1,
			})
			if err != nil {
				t.Errorf("并发 Build 失败: %v", err)
				return
			}
			mu.Lock()
			hashes[l.PrefixHash] = struct{}{}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(hashes) != 1 {
		t.Fatalf("稳定段相同时并发 Build 产生了 %d 个不同 PrefixHash", len(hashes))
	}
}
