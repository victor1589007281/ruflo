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
