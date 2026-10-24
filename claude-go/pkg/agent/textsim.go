package agent

import (
	"strings"
	"unicode"
)

// SimpleSimilarity 计算两段文本的相似度 (0~1)，不依赖外部 embedding 模型。
// 算法: 字符级 3-gram 的 Dice 系数，对中英文均有效。
func SimpleSimilarity(a, b string) float64 {
	if a == "" && b == "" {
		return 1.0
	}
	if a == "" || b == "" {
		return 0.0
	}

	setA := charShingles(normalizeText(a), 3)
	setB := charShingles(normalizeText(b), 3)

	return diceCoefficient(setA, setB)
}

// normalizeText 归一化文本: 转小写、非字母数字一律替换成空格、压缩连续空白。
//
// 这一步是相似度可比的前提 —— 标点与大小写差异不该影响"两段文字说的是不是同一件事"。
func normalizeText(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// charShingles 把文本切成字符级 k-gram 集合 (按 rune 切, 因此对中文同样成立)。
//
// 文本短于 k 时退化成"整串一个 shingle" —— 不返回空集, 否则短文本之间会一律算出 0 相似度。
func charShingles(s string, k int) map[string]struct{} {
	runes := []rune(s)
	if len(runes) < k {
		return map[string]struct{}{s: {}}
	}
	set := make(map[string]struct{}, len(runes)-k+1)
	for i := 0; i <= len(runes)-k; i++ {
		set[string(runes[i:i+k])] = struct{}{}
	}
	return set
}

// diceCoefficient 计算两个集合的 Dice 系数 2|A∩B| / (|A|+|B|), 取值 0~1。
//
// 算法解读：
//
//	原理：把两段文本的字符 3-gram 当集合, 用交集大小衡量重叠程度。选 Dice 而不是
//	      Jaccard: Dice 对长度差异更宽容 (分母是两集合大小之和而不是并集), 短文本
//	      之间不会因为一方多几个字就骤降。
//	关键不变量：两集合都空时返回 1.0 (空 == 空视为完全相同), 但只有一个空时
//	      返回 0.0 —— 非对称处理, 由调用方 SimpleSimilarity 在前面拦住空串。
//	代价与边界：O(|A|) 的集合查找, 无外部依赖; 但**词序与语义完全丢失** ——
//	      字符重排的句子 ("我爱你" vs "你爱我") 会算出极高的相似度。
//	设计权衡：用字符 n-gram 而不是词级或 embedding, 是为了零依赖、零网络调用、
//	      对中英文一致; 代价是只能当"粗略去重/召回"用, 不能当语义判据。
func diceCoefficient(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	intersection := 0
	for k := range a {
		if _, ok := b[k]; ok {
			intersection++
		}
	}
	return float64(2*intersection) / float64(len(a)+len(b))
}
