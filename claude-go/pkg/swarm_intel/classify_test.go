package swarm_intel

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 场景识别的离线判据: 目录校验 / 关键词兜底 / prompt 目录单一来源。

func TestClassify_PromptCoversAllModes(t *testing.T) {
	p := classifyPrompt("测试问题")
	for _, m := range ListSimulationModes() {
		if !strings.Contains(p, m.Mode) {
			t.Fatalf("prompt 缺少模式 %s", m.Mode)
		}
		hint := modeHints[m.Mode]
		if hint == "" {
			t.Fatalf("模式 %s 缺路由 hint (Desc 面向用户, hint 面向路由器)", m.Mode)
		}
	}
}

func TestClassify_KeywordFallback(t *testing.T) {
	cases := map[string]string{
		"网传某品牌矿泉水致癌, 官方辟谣后大家还信吗":      "rumor",
		"新款流感疫苗在社区推广, 居民接种意愿会怎样变化":    "health",
		"这家奶茶店新品上线后口碑会爆吗":               "consumer",
		"产品团队要在两个方案里拍板, 大家会怎么选":         "team",
		"班里开始流行用错题本, 其他同学会跟进吗":          "education",
		"小区强制垃圾分类三个月后居民会习惯吗":            "climate",
		"央行加息后房价会怎么走":                    "market",
		"某市出台网约车新规后各方反应如何":              "policy",
		"某明星塌房传闻是真是假, 粉丝 belief 会反转吗": "rumor",
	}
	for in, want := range cases {
		got := keywordClassify(in)
		if !got.Fallback {
			t.Fatalf("%q: 兜底结果应带 Fallback 标记", in)
		}
		if got.Primary() != want {
			t.Fatalf("%q: 应路由到 %s, 实际 %s", in, want, got.Primary())
		}
		if len(got.Modes) != 1 {
			t.Fatalf("%q: 兜底只应给 1 个模式", in)
		}
	}
	// 无关键词 → social 兜底
	if got := keywordClassify("随便聊聊").Primary(); got != "social" {
		t.Fatalf("无关键词应兜底 social, 实际 %s", got)
	}
}

func TestClassify_ModeMatchAndKnown(t *testing.T) {
	if !isKnownMode("rumor") || !isKnownMode("climate") {
		t.Fatal("新模式应在目录内")
	}
	if isKnownMode("不存在的模式") {
		t.Fatal("目录外模式应判 false")
	}
	m := modeMatch("health", 0.8)
	if m.Name != "公共健康" || m.Emoji != "🩺" || m.Score != 0.8 {
		t.Fatalf("modeMatch 应回填目录名/emoji/score: %+v", m)
	}
	if c := (&Classification{}).Primary(); c != "social" {
		t.Fatalf("空 Classification 主模式应兜底 social, 实际 %s", c)
	}
}

// classifyLLM 可控的假 LLM: 固定响应/错误, 验证 ClassifyObjective 的解析路径。
type classifyLLM struct {
	resp string
	err  error
}

func (c *classifyLLM) SimpleComplete(_ context.Context, _, _ string) (string, error) {
	return c.resp, c.err
}

// 生产实测抓到过的坑: LLM 回显 social 重复 + 目录外模式时, 预置默认会污染去重结果。
func TestClassify_LLMPathDedupesAndValidates(t *testing.T) {
	llm := &classifyLLM{resp: `{"kind":"simulate","modes":[{"mode":"social","score":0.5},{"mode":"policy","score":0.9},{"mode":"social","score":0.7},{"mode":"ghost","score":0.9}],"reason":"重复与非法都应被去掉"}`}
	e := NewEngine(llm, Config{StoreBackend: "jsonl", DataDir: t.TempDir()})
	cls := e.ClassifyObjective(context.Background(), "测试")
	if cls.Fallback {
		t.Fatal("有 LLM 响应时不应走兜底")
	}
	if len(cls.Modes) != 2 || cls.Modes[0].Mode != "social" || cls.Modes[1].Mode != "policy" {
		t.Fatalf("去重+目录校验后应剩 [social,policy]: %+v", cls.Modes)
	}
	if cls.Primary() != "social" {
		t.Fatalf("主模式应取 LLM 排序第一个: %s", cls.Primary())
	}
	// LLM 报错 → 关键词兜底, 不硬失败
	llm.err = errors.New("gateway boom")
	cls2 := e.ClassifyObjective(context.Background(), "社区居民的流感疫苗接种意愿会如何变化")
	if !cls2.Fallback || cls2.Primary() != "health" {
		t.Fatalf("LLM 失败应关键词兜底到 health: %+v", cls2)
	}
}
