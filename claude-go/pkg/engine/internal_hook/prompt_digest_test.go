// prompt_digest_test.go —— D4.2 第三条验收口径「前缀命中率上升」的**判据**。
//
// ── 这条口径为什么需要一个摘要才能测 ──────────────────────────────────────
//
// 前缀缓存只认"开头逐字节相同"。要判断"这一轮的工具段会不会毁掉上一轮的前缀",
// 就得回答"两个工具段逐字节一样吗"。手里原来只有 ToolsSchemaChars (长度)。
//
// 长度**在池化场景下恰好没有分辨力**: 池化的常态就是"一件进一件出", 换了三件
// 同样大小的工具, 长度纹丝不动 —— 而缓存正是在那一刻整体失效 (前缀从工具段起
// 就不同, 后面的 system 与 messages 全部重算)。
//
// 下面第一条测试就是把这个"长度看不出来"的事实钉死。它同时是本文件的理由:
// 如果哪天长度也能分辨了, 这个摘要就没有存在的必要。
package internal_hook

import (
	"encoding/json"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func apiTool(name, desc, schema string) types.APITool {
	return types.APITool{Name: name, Description: desc, InputSchema: json.RawMessage(schema)}
}

// TestToolsSchemaCharsCannotSeeAToolSwap 长度量不出"换了一件同样大的工具"。
func TestToolsSchemaCharsCannotSeeAToolSwap(t *testing.T) {
	a := []types.APITool{
		apiTool("pool_search", "Search the pool", `{"type":"object"}`),
		apiTool("GenerateChart", "Render a data chart to PNG", `{"type":"object"}`),
	}
	b := []types.APITool{
		apiTool("pool_search", "Search the pool", `{"type":"object"}`),
		apiTool("GenerateImage", "Render a data chart to PNG", `{"type":"object"}`), // 只换了名字
	}
	// 长度: 一模一样 (名字长度相同)。
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if len(ja) != len(jb) {
		t.Fatalf("这条测试的前提是两段长度相同 (好证明长度无效), 实际 %d vs %d", len(ja), len(jb))
	}
	// 摘要: 必须不同 —— 否则这个字段白加。
	if ToolsSurfaceDigest(a) == ToolsSurfaceDigest(b) {
		t.Fatal("换了工具的摘要必须不同, 否则它与长度一样没有分辨力")
	}
}

// TestToolsSurfaceDigest_OrderSensitive 顺序变了摘要必须变。
//
// 前缀缓存看的是**字节序列**, 不是集合。同样一组工具换个顺序下发, 内容没变而
// 前缀变了。摘要若与顺序无关, 就会把一次真实的缓存失效报成"没变" —— 而那是
// **最危险的一种错**: 它让指标看起来很好, 而实际每次都 miss。
func TestToolsSurfaceDigest_OrderSensitive(t *testing.T) {
	x := apiTool("tool_a", "does a", `{}`)
	y := apiTool("tool_b", "does b", `{}`)
	if ToolsSurfaceDigest([]types.APITool{x, y}) == ToolsSurfaceDigest([]types.APITool{y, x}) {
		t.Fatal("顺序敏感的摘要才对 —— 与顺序无关的摘要会把真实的缓存失效报成「没变」")
	}
}

// TestToolsSurfaceDigest_FieldBoundariesMatter 字段边界不能被拼接吃掉。
//
// 没有分隔符的话, ("ab","c") 与 ("a","bc") 会算出同一个摘要 —— 而它们是两个
// 不同的请求, 前缀也确实不同。
func TestToolsSurfaceDigest_FieldBoundariesMatter(t *testing.T) {
	p := apiTool("ab", "c", `{}`)
	q := apiTool("a", "bc", `{}`)
	if ToolsSurfaceDigest([]types.APITool{p}) == ToolsSurfaceDigest([]types.APITool{q}) {
		t.Fatal("字段之间必须有分隔符, 否则 (ab,c) 与 (a,bc) 会撞成同一个摘要")
	}
}

// TestToolsSurfaceDigest_SameInputIsStable 同样的输入必须给同样的摘要。
//
// 这是摘要能当"变没变"用的前提。Go 的 map 迭代顺序是随机的, 若哪天有人图省事
// 把工具塞进 map 再算摘要, 这里会红 —— 而线上表现是"每轮都 miss", 且看不出原因。
func TestToolsSurfaceDigest_SameInputIsStable(t *testing.T) {
	tools := []types.APITool{
		apiTool("pool_search", "Search the pool", `{"type":"object","properties":{"q":{"type":"string"}}}`),
		apiTool("pool_load", "Load an asset", `{"type":"object"}`),
		apiTool("pool_release", "Release an asset", `{"type":"object"}`),
	}
	first := ToolsSurfaceDigest(tools)
	for i := 0; i < 50; i++ {
		if got := ToolsSurfaceDigest(tools); got != first {
			t.Fatalf("第 %d 次算出的摘要不同 (%s vs %s) —— 摘要有随机性, 拿它判「变没变」会每轮都报变", i, got, first)
		}
	}
}

// TestToolsSurfaceDigest_EmptyIsStableAndEmpty 空工具段给确定值。
//
// 空工具段与"没有下发工具"是同一件事, 都给空串 —— 不能给"空串的哈希"(那样
// 空集与一个名为 "" 的工具会撞), 也不能 panic。
func TestToolsSurfaceDigest_EmptyIsStableAndEmpty(t *testing.T) {
	if got := ToolsSurfaceDigest(nil); got != "" {
		t.Fatalf("nil 工具段应给空串, got %q", got)
	}
	if got := ToolsSurfaceDigest([]types.APITool{}); got != "" {
		t.Fatalf("空工具段应给空串, got %q", got)
	}
}

// TestBuildPromptComponentMetrics_CarriesTheDigest 摘要要真的进账本。
//
// 上面几条测的是算法; 这条测**接线** —— 摘要算了却没进 metrics, 与没算一样,
// 而这种情况在算法测试里全绿。
func TestBuildPromptComponentMetrics_CarriesTheDigest(t *testing.T) {
	tools := []types.APITool{apiTool("GenerateChart", "Render a data chart", `{}`)}
	m := BuildPromptComponentMetrics([]string{"sys"}, tools, nil)
	if m.ToolsSchemaDigest == "" {
		t.Fatal("工具段非空时 digest 必须进账本 —— 算了不接线等于没算")
	}
	if m.ToolsSchemaDigest != ToolsSurfaceDigest(tools) {
		t.Fatalf("账本里的 digest 与工具段对不上: %q vs %q", m.ToolsSchemaDigest, ToolsSurfaceDigest(tools))
	}
	// 空工具段时账本里是空 (而不是某个非空的确定值)。
	if m2 := BuildPromptComponentMetrics([]string{"sys"}, nil, nil); m2.ToolsSchemaDigest != "" {
		t.Fatalf("没下发工具时 digest 应为空, got %q", m2.ToolsSchemaDigest)
	}
	// 长度与摘要是同源的两个数: 长度为零而摘要非空 (或反过来) 说明量错了对象。
	if (m.ToolsSchemaChars == 0) != (m.ToolsSchemaDigest == "") {
		t.Fatalf("长度与摘要必须同源: chars=%d digest=%q", m.ToolsSchemaChars, m.ToolsSchemaDigest)
	}
}

// TestPrefixStability_TwoTurnsWithSameSurface 两轮同样的广告面 → 摘要逐字节相同。
//
// 这是"前缀命中率上升"在**单次会话内**的那种情形: 模型连着调两次同样的工具集,
// 中间没有 pool_load/pool_release, 广告面必须一个字节都不变。
func TestPrefixStability_TwoTurnsWithSameSurface(t *testing.T) {
	base := []types.APITool{
		apiTool("pool_search", "Search the deferred pool", `{"type":"object"}`),
		apiTool("pool_load", "Load an asset", `{"type":"object"}`),
		apiTool("pool_release", "Release an asset", `{"type":"object"}`),
		apiTool("Read", "Read a file", `{"type":"object"}`),
	}
	turn1 := BuildPromptComponentMetrics([]string{"sys"}, base, nil)
	turn2 := BuildPromptComponentMetrics([]string{"sys"}, append([]types.APITool{}, base...), nil)
	if turn1.ToolsSchemaDigest != turn2.ToolsSchemaDigest {
		t.Fatalf("两轮广告面相同, 摘要却不同 (%s vs %s) —— 这会让前缀命中率凭空下降",
			turn1.ToolsSchemaDigest, turn2.ToolsSchemaDigest)
	}
	// pool_load 之后多一件 → 摘要必须变 (那是真实的一次失效, 不能报成"没变")。
	afterLoad := append(append([]types.APITool{}, base...),
		apiTool("GenerateChart", "Render a data chart", `{}`))
	turn3 := BuildPromptComponentMetrics([]string{"sys"}, afterLoad, nil)
	if turn3.ToolsSchemaDigest == turn1.ToolsSchemaDigest {
		t.Fatal("池化加载了一件之后广告面变了, 摘要必须变 —— 否则这条口径看不出池化对缓存的真实影响")
	}
}
