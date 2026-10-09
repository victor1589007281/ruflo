package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 核心不变量: 任何输入下返回值都必须是合法 UTF-8。
// 这是这个包存在的全部理由 —— 裸 s[:n] 会让中文变成半个字。
func TestTruncate_NeverSplitsRune(t *testing.T) {
	// 全中文: 每个字 3 字节, 按字节切必然劈字。
	zh := strings.Repeat("中文测试", 20)
	for n := 1; n <= utf8.RuneCountInString(zh)+2; n++ {
		got := Truncate(zh, n, "...")
		if !utf8.ValidString(got) {
			t.Fatalf("Truncate(zh, %d) 产出非法 UTF-8: %q", n, got)
		}
	}
	for n := 1; n <= len(zh)+2; n++ {
		got := TruncateBytes(zh, n, "...")
		if !utf8.ValidString(got) {
			t.Fatalf("TruncateBytes(zh, %d) 产出非法 UTF-8: %q", n, got)
		}
	}
}

// 截断点落在最容易劈字的位置: n 是 3 的倍数附近 (正好切在一个字的中间字节)。
func TestTruncate_RuneBoundaryAtMultiplesOfThree(t *testing.T) {
	zh := "中文字符测试内容"
	// 7 个字 = 21 字节。按字节切 3/6/9... 全是字中间。
	for n := 1; n <= 8; n++ {
		got := Truncate(zh, n, "")
		if got != string([]rune(zh)[:n]) {
			t.Errorf("Truncate(zh, %d) = %q, 期望前 %d 个字", n, got, n)
		}
	}
}

// 未超长时原样返回, 不追加 suffix。
// (调用方普遍用 "maxChars<=0 或本来就短" 做放行, 多贴一个 "..." 会让短串凭空变长。)
func TestTruncate_NoSuffixWhenFits(t *testing.T) {
	if got := Truncate("短", 10, "..."); got != "短" {
		t.Errorf("未超长不该追加 suffix, got %q", got)
	}
	if got := Truncate("短", 1, "..."); got != "短" {
		t.Errorf("恰好等于上限也不该追加 suffix, got %q", got)
	}
	if got := Truncate("任意", 0, "..."); got != "任意" {
		t.Errorf("maxRunes<=0 应原样返回, got %q", got)
	}
	if got := Truncate("任意", -1, "..."); got != "任意" {
		t.Errorf("负数上限应原样返回, got %q", got)
	}
}

// 字节预算语义: 结果字节数不超过 maxBytes + suffix。
func TestTruncateBytes_RespectsByteBudget(t *testing.T) {
	zh := strings.Repeat("中文测试", 10) // 120 字节 / 40 字
	for n := 1; n <= 40; n++ {
		got := TruncateBytes(zh, n, "")
		if len(got) > n {
			t.Fatalf("TruncateBytes(zh, %d) 产出 %d 字节, 超预算", n, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("TruncateBytes(zh, %d) 非法 UTF-8", n)
		}
	}
	// 恰好 3 的倍数: 不能切在第 2 个字节上。
	if got := TruncateBytes(zh, 3, ""); got != "中" {
		t.Errorf("TruncateBytes(zh, 3) = %q, 期望 %q", got, "中")
	}
	if got := TruncateBytes(zh, 4, ""); got != "中" {
		t.Errorf("TruncateBytes(zh, 4) 应回退到 rune 边界 %q, got %q", "中", got)
	}
}

// 多字节与 ASCII 混排 (参考文档是这种形态), 两种语义都要安全。
func TestTruncate_MixedContent(t *testing.T) {
	mixed := "The API 接口返回 400 错误 means the request was rejected 请检查参数"
	for n := 1; n <= 4; n++ {
		if got := Truncate(mixed, n, "…"); !utf8.ValidString(got) {
			t.Fatalf("Truncate(mixed, %d) 非法 UTF-8", n)
		}
	}
	for n := 1; n <= 12; n++ {
		if got := TruncateBytes(mixed, n, "…"); !utf8.ValidString(got) {
			t.Fatalf("TruncateBytes(mixed, %d) 非法 UTF-8", n)
		}
	}
}

func TestTruncateMiddle(t *testing.T) {
	zh := strings.Repeat("中", 100) // 100 个字
	got := TruncateMiddle(zh, 20, "...")
	if !utf8.ValidString(got) {
		t.Fatalf("TruncateMiddle 非法 UTF-8")
	}
	if utf8.RuneCountInString(got) > 20 {
		t.Errorf("TruncateMiddle 超出上限: %d 个字", utf8.RuneCountInString(got))
	}
	if !strings.Contains(got, "...") {
		t.Errorf("应包含标记, got %q", got)
	}
	// 未超长原样返回。
	if got := TruncateMiddle("短", 20, "..."); got != "短" {
		t.Errorf("未超长应原样返回, got %q", got)
	}
}

// 边界: 空串 / 单字节 / 上限比标记还短。
func TestTruncate_EdgeCases(t *testing.T) {
	if got := Truncate("", 5, "..."); got != "" {
		t.Errorf("空串应返回空串, got %q", got)
	}
	if got := TruncateBytes("", 5, "..."); got != "" {
		t.Errorf("空串应返回空串, got %q", got)
	}
	// maxRunes 比标记还短: 结果仍不得超过 maxRunes, 且必须合法。
	got := TruncateMiddle(strings.Repeat("中", 50), 2, "...")
	if !utf8.ValidString(got) {
		t.Errorf("标记比上限长时仍须合法 UTF-8, got %q", got)
	}
}
