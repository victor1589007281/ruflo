package engine

// IterationGuard 在引擎侧的两条装配点（主循环 / 隔离 run）必须同口径：
// 被驱逐的工具既不能出现在下发给模型的 apiTools 里，也不能被静默地"只挡一半"。
// 这里测的是 filterEvictedTools 这条纯逻辑（装配点都调它），外加"全被驱逐时
// 放弃驱逐"这条安全阀 —— 模型手里一个工具都没有时既完不成任务也不会收口。

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/types"
)

func evictTool(t *testing.T, d *internal_hook.LoopDetector, tool string) {
	t.Helper()
	input := []byte(`{"same":1}`)
	for i := 0; i < d.EvictAfter; i++ {
		// 每次触发需要连续 Threshold 个同签名；用不同工具名当噪音冲掉滑窗。
		for j := 0; j < d.Window; j++ {
			d.Observe("Noise", []byte(`{"n":`+string(rune('a'+j))+`}`))
		}
		for j := 0; j < d.Threshold; j++ {
			d.Observe(tool, input)
		}
	}
	if !d.IsEvicted(tool) {
		t.Fatalf("%s 未能驱逐", tool)
	}
}

func tools(names ...string) []types.APITool {
	out := make([]types.APITool, 0, len(names))
	for _, n := range names {
		out = append(out, types.APITool{Name: n})
	}
	return out
}

func namesOf(ts []types.APITool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func TestFilterEvictedTools(t *testing.T) {
	d := internal_hook.NewLoopDetector()
	e := &QueryEngine{Config: &Config{EnableLoopDetector: true}, LoopDet: d}
	in := tools("Bash", "Read", "Grep")

	// 未驱逐：原样返回（同一个切片，不做无意义拷贝）
	if got := e.filterEvictedTools(in); len(got) != 3 {
		t.Fatalf("未驱逐时应原样返回, got %v", namesOf(got))
	}

	evictTool(t, d, "Read")
	got := namesOf(e.filterEvictedTools(in))
	if len(got) != 2 || got[0] != "Bash" || got[1] != "Grep" {
		t.Fatalf("驱逐后应摘掉 Read, got %v", got)
	}

	// 关掉开关：即使已驱逐也照旧全发（退回 P2 行为）
	e.Config.DisableToolEviction = true
	if got := e.filterEvictedTools(in); len(got) != 3 {
		t.Fatalf("DisableToolEviction 时应原样返回, got %v", namesOf(got))
	}
}

// 安全阀：驱逐到最后一个都不剩时放弃驱逐。模型没有工具时既干不了活也不会
// 主动收口，只会空转到 max-turns —— 那比"工具还在但被拦"更糟。
func TestFilterEvictedToolsKeepsLastResort(t *testing.T) {
	d := internal_hook.NewLoopDetector()
	d.EvictAfter = 1
	e := &QueryEngine{Config: &Config{EnableLoopDetector: true}, LoopDet: d}
	evictTool(t, d, "Bash")

	if got := namesOf(e.filterEvictedTools(tools("Bash"))); len(got) != 1 || got[0] != "Bash" {
		t.Fatalf("最后一个工具不能被摘掉, got %v", got)
	}
}

// 没有循环检测器（没开 G3）时不做任何过滤。
func TestFilterEvictedToolsNoDetector(t *testing.T) {
	e := &QueryEngine{Config: &Config{}}
	in := tools("Bash", "Read")
	if got := e.filterEvictedTools(in); len(got) != 2 {
		t.Fatalf("无检测器时应原样返回, got %v", namesOf(got))
	}
	var nilEngine *QueryEngine
	if got := nilEngine.filterEvictedTools(in); len(got) != 2 {
		t.Fatalf("nil 引擎应原样返回, got %v", namesOf(got))
	}
}
