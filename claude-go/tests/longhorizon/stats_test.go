package longhorizon

import (
	"math"
	"strings"
	"testing"
)

// TestH42_威尔逊区间有界 —— 正态近似在 n 小的时候会越界, 这条盯着它。
func TestH42_威尔逊区间有界(t *testing.T) {
	cases := []struct {
		k, n       int
		loMin      float64 // 下界必须 >= 这个 (用来抓"跑出负区间")
		hiMax      float64 // 上界必须 <= 这个 (用来抓"跑出 >1")
		wantLoNear float64
		wantHiNear float64
	}{
		{0, 8, 0, 1, 0, 0.324},
		{8, 8, 0, 1, 0.676, 1},
		{4, 8, 0, 1, 0.215, 0.785},
		{3, 4, 0, 1, 0.30, 0.954},
	}
	for _, c := range cases {
		lo, hi := WilsonInterval(c.k, c.n)
		if lo < 0 || hi > 1 {
			t.Fatalf("k=%d n=%d 区间 [%v, %v] 越界", c.k, c.n, lo, hi)
		}
		if lo > hi {
			t.Fatalf("k=%d n=%d 区间端点反了: [%v, %v]", c.k, c.n, lo, hi)
		}
		if math.Abs(lo-c.wantLoNear) > 0.02 {
			t.Fatalf("k=%d n=%d 下界 = %.4f, 期望约 %.3f", c.k, c.n, lo, c.wantLoNear)
		}
		if math.Abs(hi-c.wantHiNear) > 0.02 {
			t.Fatalf("k=%d n=%d 上界 = %.4f, 期望约 %.3f", c.k, c.n, hi, c.wantHiNear)
		}
	}
}

// TestH42_零样本不是零成功率 —— 反面臂, 也是这条最容易写错的地方。
//
// n=0 时若返回 (0,0), 报告会读成"确定成功率为 0"。而 0/8 若返回 [0,0],
// 报告会读成"8 个任务一个都做不成是确定的"—— 实际上限能到 32%,
// 正是"样本太少还不能下结论"这句话的量化形态。
func TestH42_零样本不是零成功率(t *testing.T) {
	lo, hi := WilsonInterval(0, 0)
	if lo == 0 && hi == 0 {
		t.Fatal("n=0 给出 [0,0] —— 会被读成「确定成功率 0」, 而实际是「没有样本」")
	}
	_, hi8 := WilsonInterval(0, 8)
	if hi8 <= 0.2 {
		t.Fatalf("0/8 的区间上界 = %.3f, 太小 —— 8 个任务全失败说明不了成功率低于 20%%", hi8)
	}
}

// TestH42_McNemar精确值 用可手算的已知值钉住。
//
// 这几个数是配对检验的核心口径, 算错了整台 A/B 的结论都反。取的都是
// "只有 B 组赢"的极端情形, 于是 p 值有闭式: 2 / 2^n。
func TestH42_McNemar精确值(t *testing.T) {
	cases := []struct {
		b, c int
		want float64
	}{
		{0, 0, 1.0},     // 没有不一致对 —— 没有证据
		{1, 0, 1.0},     // 2/2 = 1, 截断在 1
		{3, 0, 0.25},    // 2/8
		{4, 0, 0.125},   // 2/16
		{5, 0, 0.0625},  // 2/32 —— 刚好不显著
		{6, 0, 0.03125}, // 2/64 —— 显著
		{0, 6, 0.03125}, // 对称
	}
	for _, c := range cases {
		got := McNemarExact(c.b, c.c)
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("McNemarExact(%d,%d) = %v, 期望 %v", c.b, c.c, got, c.want)
		}
	}
}

// TestH42_McNemar对称 —— 方向不该影响显著性。
func TestH42_McNemar对称(t *testing.T) {
	for _, p := range [][2]int{{1, 4}, {2, 9}, {7, 3}, {0, 5}} {
		if a, b := McNemarExact(p[0], p[1]), McNemarExact(p[1], p[0]); a != b {
			t.Fatalf("McNemarExact(%d,%d)=%v 与 (%d,%d)=%v 不对称",
				p[0], p[1], a, p[1], p[0], b)
		}
	}
}

// TestH42_配对不完整必须报错 —— 分母错了成功率就虚高。
func TestH42_配对不完整必须报错(t *testing.T) {
	bad := PairedResult{Knob: "k", N: 5, BothPass: 2, BothFail: 1} // 和 = 3 ≠ 5
	if err := bad.AssertComplete(); err == nil {
		t.Fatal("四格之和与 N 不符时必须报错, 实得 nil")
	}
	if !strings.Contains(bad.Report(), "配对不完整") {
		t.Fatal("报告必须显式说出配对不完整 —— 不能给出一份看起来正常的成功率")
	}
}

// TestH42_两组任务集不一致必须报错 —— 反面臂。
//
// 取交集是这里最诱人的错误写法: 它能让函数不报错、成功率高一点点 (没跑完的任务
// 从分母里消失了), 而报告上完全看不出来。
func TestH42_两组任务集不一致必须报错(t *testing.T) {
	a := map[string]bool{"lexer": true, "rpn": true}
	b := map[string]bool{"lexer": false}
	if _, err := BuildPairedResult("k", a, b); err == nil {
		t.Fatal("两组任务集不一致时必须报错, 实得 nil —— 取交集会让没跑的任务静默消失")
	}
}

// TestH42_配对统计从原始判定构造 —— 主臂。
func TestH42_配对统计从原始判定构造(t *testing.T) {
	a := map[string]bool{"t1": true, "t2": true, "t3": false, "t4": false, "t5": true}
	b := map[string]bool{"t1": true, "t2": false, "t3": true, "t4": false, "t5": true}
	res, err := BuildPairedResult("k", a, b)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if res.N != 5 {
		t.Fatalf("N = %d, 期望 5", res.N)
	}
	if res.BothPass != 2 || res.BothFail != 1 || res.AOnly != 1 || res.BOnly != 1 {
		t.Fatalf("四格 = 都过%d 都挂%d 只A过%d 只B过%d, 期望 2/1/1/1",
			res.BothPass, res.BothFail, res.AOnly, res.BOnly)
	}
	if res.APass() != 3 || res.BPass() != 3 {
		t.Fatalf("通过数 A=%d B=%d, 期望各 3", res.APass(), res.BPass())
	}
	if math.Abs(res.ARate()-0.6) > 1e-9 {
		t.Fatalf("A 成功率 = %v, 期望 0.6", res.ARate())
	}
}

// TestH42_读数不足时必须说不显著 —— 防"8 个任务里多对 1 个就宣布胜利"。
func TestH42_读数不足时必须说不显著(t *testing.T) {
	// 5 对 4, 只有 1 个任务不同 —— 换成另一个等价任务集就翻号。
	res := PairedResult{Knob: "k", N: 8, AOnly: 1, BOnly: 0, BothPass: 4, BothFail: 3}
	rep := res.Report()
	if !strings.Contains(rep, "不显著") {
		t.Fatalf("只有 1 个不一致对时报告必须说不显著, 实得:\n%s", rep)
	}
}

// TestH42_每个任务结果都相同时必须明说 —— 这是"没有可测效果"而不是"显著提升 0%"。
func TestH42_每个任务结果都相同时必须明说(t *testing.T) {
	res := PairedResult{Knob: "k", N: 4, BothPass: 3, BothFail: 1}
	rep := res.Report()
	if !strings.Contains(rep, "每一个任务上") {
		t.Fatalf("结果全同时必须显式说明, 否则读者会以为差异存在只是没测到:\n%s", rep)
	}
}

// TestH42_报告里必须同时出现区间与p值 报告是交付物。
func TestH42_报告里必须同时出现区间与p值(t *testing.T) {
	res := PairedResult{Knob: "gate", N: 8, BothPass: 5, BothFail: 1, AOnly: 1, BOnly: 1}
	rep := res.Report()
	for _, want := range []string{"95%CI", "McNemar", "gate"} {
		if !strings.Contains(rep, want) {
			t.Fatalf("报告里缺少 %q:\n%s", want, rep)
		}
	}
}
