// stats.go —— H4-2 的统计那一半。
//
// # 为什么不能只报两个百分比
//
// 方案 §21.5.6 要求"能给出两组成功率与置信区间"。8 个任务上 A 组 5/8、B 组 6/8
// 这种读数, 单看百分比会得出"提升了 12.5 个百分点"的结论 —— 而那是一个任务的
// 差别, 换成另一个等价任务集就翻号。区间是让读者自己看见"这点差别还没出噪声"的
// 唯一手段。
//
// # 为什么用 Wilson 区间而不是正态近似
//
// n=8 且成功率可能在 0.9 附近时, 正态近似 (p ± 1.96·√(p(1-p)/n)) 会给出越界
// 区间甚至半宽为 0 (p=1 时直接给出 [1,1], 即"我已经确定成功率是 100%"——
// 8 个样本说不了这种话)。Wilson 区间在小 n 与接近 0/1 时都保持有界且合理。
//
// # 为什么配对要用 McNemar 而不是两独立样本检验
//
// 方案 §21.5.6 明写"必须配对, 不能前后比"。配对样本的正确检验是看**不一致对**
// (b = 只在 A 组成败、c = 只在 B 组成败), 两独立样本检验会把"任务难易"这个
// 已经配对消掉的方差重新算进去, 于是**低估**显著性 —— 恰恰在 n 小的时候最危险。
package longhorizon

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// z95 正态分布 95% 分位数 (双侧 α=0.05)。
const z95 = 1.959963984540054

// WilsonInterval 二项比例 p̂ = k/n 的 Wilson 置信区间 (95%)。
//
// n=0 时返回 (0,1) —— "没有任何样本"的区间是全集, 不是 (0,0):
// 后者会被读成"确定成功率为 0"。
func WilsonInterval(k, n int) (lo, hi float64) {
	if n <= 0 {
		return 0, 1
	}
	if k < 0 {
		k = 0
	}
	if k > n {
		k = n
	}
	p := float64(k) / float64(n)
	nf := float64(n)
	denom := 1 + z95*z95/nf
	center := (p + z95*z95/(2*nf)) / denom
	half := (z95 / denom) * math.Sqrt(p*(1-p)/nf+z95*z95/(4*nf*nf))
	lo = center - half
	hi = center + half
	if lo < 0 {
		lo = 0
	}
	if hi > 1 {
		hi = 1
	}
	return lo, hi
}

// McNemarExact 配对二分类的精确 McNemar 检验 (双侧 p 值)。
//
// b = 只在 A 成功的任务数, c = 只在 B 成功的任务数。
// 原假设是 b 与 c 来自同一个二项分布 (p=0.5) —— 即在"两组不一致的那些任务"里,
// 谁赢没有偏向。b+c=0 时返回 1 (没有任何不一致对, 没有证据可言)。
//
// 用**精确**而不是卡方近似: 卡方要求 b+c 足够大 (通常 ≥ 25), 而 8 个任务的
// 不一致对能有 5 个就算多的。近似在 n 小的时候给出的 p 值偏小, 会让人以为
// 看到的效果比实际更真。
func McNemarExact(b, c int) float64 {
	n := b + c
	if n == 0 {
		return 1
	}
	k := b
	if c < k {
		k = c
	}
	// 双侧: 2 * P(X <= min(b,c)), 截断在 1。
	var sum float64
	for i := 0; i <= k; i++ {
		sum += binomCoeff(n, i)
	}
	p := 2 * sum / math.Pow(2, float64(n))
	if p > 1 {
		p = 1
	}
	return p
}

// binomCoeff 计算二项式系数 C(n,k); 用连乘 n-k+1..n 再逐项除 i 的写法避免阶乘溢出,
// k 越界 (k<0 或 k>n) 时返回 0。
func binomCoeff(n, k int) float64 {
	if k < 0 || k > n {
		return 0
	}
	r := 1.0
	for i := 1; i <= k; i++ {
		r = r * float64(n-k+i) / float64(i)
	}
	return r
}

// PairedResult 一次配对 A/B 的结果。
type PairedResult struct {
	// Knob 被测开关名 (报告里用)。
	Knob string
	// N 配对样本数 (每组都跑到的任务数)。
	N int
	// AOnly / BOnly / BothPass / BothFail 四种配对状态的计数。
	// AOnly + BOnly + BothPass + BothFail == N 必须成立 —— 不成立说明有任务
	// 只跑了一组, 配对已经破了, 此时任何统计都不成立 (见 AssertComplete)。
	AOnly    int
	BOnly    int
	BothPass int
	BothFail int
}

// APass / BPass 两组的通过数。
func (r PairedResult) APass() int { return r.BothPass + r.AOnly }
// BPass B 组通过数 = 两组都过 + 仅 B 过。
func (r PairedResult) BPass() int { return r.BothPass + r.BOnly }

// ARate / BRate 两组成功率。
func (r PairedResult) ARate() float64 {
	if r.N == 0 {
		return 0
	}
	return float64(r.APass()) / float64(r.N)
}

// BRate 见上。
func (r PairedResult) BRate() float64 {
	if r.N == 0 {
		return 0
	}
	return float64(r.BPass()) / float64(r.N)
}

// AssertComplete 核对配对完整: 四种状态之和必须等于 N。
//
// 这条挡的是最隐蔽的一种假结论: 某个任务只在 A 组跑了 (B 组超时/崩了被跳过),
// 于是 N 与实际样本数不符。若直接按四格算, 那个任务会**静默消失**,
// 而成功率是拿剩下的算的 —— 分母小了, 百分比高了, 没人会注意到。
func (r PairedResult) AssertComplete() error {
	sum := r.AOnly + r.BOnly + r.BothPass + r.BothFail
	if r.N <= 0 {
		return fmt.Errorf("配对样本数 N=%d —— 没有可统计的样本", r.N)
	}
	if sum != r.N {
		return fmt.Errorf("配对四格之和 = %d, 但 N = %d —— "+
			"有任务只跑了一组 (没配成对), 此时的成功率分母是错的",
			sum, r.N)
	}
	return nil
}

// Report 生成人可读的报告。零值配置下也要能读 —— 报告是交付物。
func (r PairedResult) Report() string {
	if err := r.AssertComplete(); err != nil {
		return fmt.Sprintf("【配对不完整, 统计无效】%v", err)
	}
	aLo, aHi := WilsonInterval(r.APass(), r.N)
	bLo, bHi := WilsonInterval(r.BPass(), r.N)
	p := McNemarExact(r.AOnly, r.BOnly)

	var sb strings.Builder
	fmt.Fprintf(&sb, "配对 A/B: 开关 %s, N=%d\n", r.Knob, r.N)
	fmt.Fprintf(&sb, "  A (关): %d/%d = %.1f%%  95%%CI [%.1f%%, %.1f%%]\n",
		r.APass(), r.N, r.ARate()*100, aLo*100, aHi*100)
	fmt.Fprintf(&sb, "  B (开): %d/%d = %.1f%%  95%%CI [%.1f%%, %.1f%%]\n",
		r.BPass(), r.N, r.BRate()*100, bLo*100, bHi*100)
	fmt.Fprintf(&sb, "  四格: 都过=%d 都挂=%d 只 A 过=%d 只 B 过=%d\n",
		r.BothPass, r.BothFail, r.AOnly, r.BOnly)
	fmt.Fprintf(&sb, "  配对 McNemar 精确检验 p = %.4f\n", p)

	switch {
	case r.AOnly == 0 && r.BOnly == 0:
		fmt.Fprintf(&sb, "  结论: 两组在**每一个任务上**结果相同 —— 这个开关在本任务集上没有可测的效果\n")
	case p < 0.05:
		winner := "B (开)"
		if r.AOnly > r.BOnly {
			winner = "A (关)"
		}
		fmt.Fprintf(&sb, "  结论: 差异显著 (p<0.05), %s 更好\n", winner)
	default:
		fmt.Fprintf(&sb, "  结论: 差异**不显著** (p=%.4f) —— "+
			"不一致对只有 %d 个, 这个样本量下分辨不出方向\n", p, r.AOnly+r.BOnly)
	}
	return sb.String()
}

// BuildPairedResult 由逐任务的判定构造结果。
//
// outcomes 的 key 是任务 ID; 两个 map 的**键集合必须相同** —— 不同意味着有任务
// 只跑了一个组, 那是配对破裂, 必须报错而不是取交集 (取交集会把没跑的任务静默
// 从分母里删掉)。
func BuildPairedResult(knob string, aOutcomes, bOutcomes map[string]bool) (PairedResult, error) {
	res := PairedResult{Knob: knob}

	var onlyA, onlyB []string
	for id := range aOutcomes {
		if _, ok := bOutcomes[id]; !ok {
			onlyA = append(onlyA, id)
		}
	}
	for id := range bOutcomes {
		if _, ok := aOutcomes[id]; !ok {
			onlyB = append(onlyB, id)
		}
	}
	if len(onlyA) > 0 || len(onlyB) > 0 {
		sort.Strings(onlyA)
		sort.Strings(onlyB)
		return res, fmt.Errorf("两组跑的任务集不一致: 只在 A 组有 %v, 只在 B 组有 %v —— "+
			"配对已破, 取交集会把没跑完的任务从分母里删掉, 成功率随之虚高", onlyA, onlyB)
	}

	for id, aOK := range aOutcomes {
		bOK := bOutcomes[id]
		switch {
		case aOK && bOK:
			res.BothPass++
		case !aOK && !bOK:
			res.BothFail++
		case aOK && !bOK:
			res.AOnly++
		default:
			res.BOnly++
		}
	}
	res.N = len(aOutcomes)
	return res, res.AssertComplete()
}
