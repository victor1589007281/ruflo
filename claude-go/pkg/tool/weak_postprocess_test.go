package tool

import (
	"strings"
	"testing"
)

func TestDeterministicPostprocessLineTruncation(t *testing.T) {
	long := strings.Repeat("x", 5000)
	in := "header\n" + long + "\nfooter"
	out := deterministicToolResultPostprocess(in)
	if !strings.Contains(out, "[line truncated]") {
		t.Error("超长行应被截断并标注")
	}
	if strings.Contains(out, long) {
		t.Error("截断后不应保留完整超长行")
	}
	if !strings.Contains(out, "header") || !strings.Contains(out, "footer") {
		t.Error("短行应原样保留")
	}
}

func TestDeterministicPostprocessHeadTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 6000; i++ {
		b.WriteString("line-" + strings.Repeat("a", 3) + "\n")
	}
	out := deterministicToolResultPostprocess(b.String())
	if !strings.Contains(out, "omitted") {
		t.Error("超行数应保头尾并标注省略量")
	}
	lines := strings.Split(out, "\n")
	if len(lines) > 3200 {
		t.Errorf("保头尾后行数应约 3001, 得 %d", len(lines))
	}
	// 头部第一行与尾部最后一行都在
	if !strings.HasPrefix(out, "line-") {
		t.Error("头部应保留")
	}
}

func TestDeterministicPostprocessSmallContentUntouched(t *testing.T) {
	in := "short\nmultiline\ncontent"
	if out := deterministicToolResultPostprocess(in); out != in {
		t.Errorf("小内容不应改动, 得 %q", out)
	}
}

func TestCompactToolResultContentWeakPostprocessGate(t *testing.T) {
	long := strings.Repeat("y", 5000)
	in := long // 单行 5000 字符, 低于默认 maxChars 阈值
	// 关: 不动 (未超字符预算)
	if out := compactToolResultContent("Shell", in, &ToolContext{}); out != in {
		t.Error("WeakResultPostprocess 关闭时不应做行截断")
	}
	// 开: 单行截断生效
	out := compactToolResultContent("Shell", in, &ToolContext{WeakResultPostprocess: true})
	if !strings.Contains(out, "[line truncated]") {
		t.Error("WeakResultPostprocess 开启后应截断超长行")
	}
}
