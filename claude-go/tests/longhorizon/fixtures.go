// fixtures.go —— 长程任务的初始代码, 内嵌在包里。
//
// # 为什么内嵌而不是放 tests/longhorizon/fixtures/ 目录
//
// 两个理由, 都不是风格问题:
//
//  1. **任务集要能自证完整**。ValidateTaskSet 现在可以在单测里断言"每个任务的
//     fixture 都存在" —— 目录形态下这条只能靠 os.Stat, 一旦有人改了路径, 单测
//     仍然全绿而真实 run 在 Materialize 时才炸, 且报错指向文件系统而不是任务定义。
//  2. **fixture 是任务的**判据**的一部分**。VerifyCmd 跑的测试就在这些文件里;
//     把判据与任务放在同一份 diff 里审, 才不会出现"改了测试让任务变简单"这种
//     没人看的改动。
//
// # 每个 fixture 的内容纪律
//
// 同一个包必须满足两件事, 缺一不可:
//
//   - `*_test.go` 是**规格**, 它现在就是完整的、能编译的、且**失败**的。
//     失败是刻意的: 它是"客观终态"的起点 —— 退出码非 0。
//   - 实现文件是**桩**: 签名齐全、编译通过、返回零值或显式错误, 但语义全错。
//     桩不留 TODO 注释以外的方法提示 —— 提示越多, agent 要做的探索越少,
//     任务越短程, 就越偏离"100+ 轮"的初衷。
//
// 于是 `go test ./...` 在初始状态必然是 FAIL, 在实现正确后必然是 PASS,
// 中间没有人为判断的余地。
package longhorizon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// goModFor 生成 fixture 的 go.mod。Go 版本写低一点 (1.21) 是为了不与宿主 toolchain
// 的版本要求打架 —— fixture 是**被测对象**, 它的 go.mod 不该成为"跑不跑得起来"的
// 变量。
func goModFor(mod string) string {
	return "module " + mod + "\n\ngo 1.21\n"
}

// fixtures 是 fixture 名 -> (相对路径 -> 文件内容)。
var fixtures = map[string]map[string]string{
	"lexer": {
		"go.mod": goModFor("example.com/lexer"),
		"lexer/lexer.go": `package lexer

// Kind 一个 token 的种类。
type Kind string

const (
	Number Kind = "number"
	Ident  Kind = "ident"
	Op     Kind = "op"
	LParen Kind = "("
	RParen Kind = ")"
)

// Token 一个词法单元。Pos 是它在输入里的字节偏移。
type Token struct {
	Kind Kind
	Text string
	Pos  int
}

// Lex 把输入切成 token 序列。
//
// 规格见 lexer/lexer_test.go —— 规格是判据, 不是本注释。
// 未知字符必须返回错误, 且错误信息里带该字符的位置。
func Lex(input string) ([]Token, error) {
	return nil, nil
}
`,
		"lexer/lexer_test.go": `package lexer

import (
	"strings"
	"testing"
)

func kinds(ts []Token) []Kind {
	out := make([]Kind, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Kind)
	}
	return out
}

func TestLexBasic(t *testing.T) {
	ts, err := Lex("12 + a3*(7)")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	want := []Kind{Number, Op, Ident, Op, LParen, Number, RParen}
	if got := kinds(ts); len(got) != len(want) {
		t.Fatalf("token 数 = %d (%v), 期望 %d (%v)", len(got), got, len(want), want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("第 %d 个 token 是 %v, 期望 %v (全部: %v)", i, got[i], want[i], got)
			}
		}
	}
	if ts[0].Text != "12" {
		t.Fatalf("数字 token 的文本 = %q, 期望 12", ts[0].Text)
	}
	if ts[2].Text != "a3" {
		t.Fatalf("标识符 token 的文本 = %q, 期望 a3", ts[2].Text)
	}
}

func TestLexPositions(t *testing.T) {
	ts, err := Lex("a + bb")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(ts) != 3 {
		t.Fatalf("token 数 = %d, 期望 3", len(ts))
	}
	for i, wantPos := range []int{0, 2, 4} {
		if ts[i].Pos != wantPos {
			t.Fatalf("第 %d 个 token 的位置 = %d, 期望 %d", i, ts[i].Pos, wantPos)
		}
	}
}

func TestLexSkipsSpaces(t *testing.T) {
	ts, err := Lex("   \t\n  x")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	// 位置是**字节偏移**: "   \t\n  x" 里 x 在第 7 个字节 (3 空格 + \t + \n + 2 空格)。
	if len(ts) != 1 || ts[0].Text != "x" || ts[0].Pos != 7 {
		t.Fatalf("空白未被正确跳过: %+v", ts)
	}
}

func TestLexUnknownCharErrors(t *testing.T) {
	_, err := Lex("a # b")
	if err == nil {
		t.Fatal("未知字符 # 必须报错, 实得 nil")
	}
	if !strings.Contains(err.Error(), "2") {
		t.Fatalf("错误信息里必须带位置 2, 实得: %v", err)
	}
}

func TestLexEmpty(t *testing.T) {
	ts, err := Lex("")
	if err != nil {
		t.Fatalf("空输入不该报错: %v", err)
	}
	if len(ts) != 0 {
		t.Fatalf("空输入应得 0 个 token, 实得 %d", len(ts))
	}
}
`,
	},

	"rpn": {
		"go.mod": goModFor("example.com/rpn"),
		"rpn/rpn.go": `package rpn

// Eval 计算逆波兰表达式。
//
// 支持 + - * / 与一元负号 (~)。空白分隔。
// 除零、操作数不足、末尾有剩余操作数, 都必须返回错误。
// 规格见 rpn/rpn_test.go。
func Eval(expr string) (float64, error) {
	return 0, nil
}
`,
		"rpn/rpn_test.go": `package rpn

import (
	"math"
	"testing"
)

func TestEvalBasic(t *testing.T) {
	cases := []struct {
		expr string
		want float64
	}{
		{"3 4 +", 7},
		{"5 1 2 + 4 * + 3 -", 14},
		{"2 3 4 * +", 14},
		{"10 2 /", 5},
		{"5 ~ 3 +", -2}, // ~ 是**后缀**一元运算: 对栈顶取负 (RPN 里操作数先行)
	}
	for _, c := range cases {
		got, err := Eval(c.expr)
		if err != nil {
			t.Fatalf("Eval(%q) 报错: %v", c.expr, err)
		}
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("Eval(%q) = %v, 期望 %v", c.expr, got, c.want)
		}
	}
}

func TestEvalDivideByZero(t *testing.T) {
	if _, err := Eval("1 0 /"); err == nil {
		t.Fatal("除零必须报错, 实得 nil")
	}
}

func TestEvalUnderflow(t *testing.T) {
	if _, err := Eval("1 +"); err == nil {
		t.Fatal("操作数不足必须报错, 实得 nil")
	}
}

func TestEvalLeftover(t *testing.T) {
	if _, err := Eval("1 2"); err == nil {
		t.Fatal("末尾剩两个操作数必须报错, 实得 nil")
	}
}

func TestEvalEmpty(t *testing.T) {
	if _, err := Eval("   "); err == nil {
		t.Fatal("空表达式必须报错, 实得 nil")
	}
}
`,
	},

	"lru": {
		"go.mod": goModFor("example.com/lru"),
		"lru/lru.go": `package lru

// Cache 一个带容量上限的 LRU 缓存。
//
// Get/Set 必须是 O(1) (测试会用大量操作压它)。
// Hits/Misses/Evictions 三个计数必须精确。
// 规格见 lru/lru_test.go。
type Cache struct {
	// 字段由实现者决定 —— 但不要改成导出, 测试只用下面这几个导出方法。
	cap int
}

// New 建一个容量为 capacity 的缓存。capacity <= 0 时按 1 处理。
func New(capacity int) *Cache {
	return &Cache{cap: capacity}
}

// Get 取值。第二个返回值报告这次是否命中。
func (c *Cache) Get(key string) (string, bool) {
	return "", false
}

// Set 写入。若因此发生淘汰, Evictions 加一。
func (c *Cache) Set(key, value string) {}

// Len 当前条目数。
func (c *Cache) Len() int { return 0 }

// Hits / Misses / Evictions 三个计数。
func (c *Cache) Hits() int      { return 0 }
func (c *Cache) Misses() int    { return 0 }
func (c *Cache) Evictions() int { return 0 }
`,
		"lru/lru_test.go": `package lru

import (
	"fmt"
	"testing"
)

func TestSetGet(t *testing.T) {
	c := New(3)
	c.Set("a", "1")
	if v, ok := c.Get("a"); !ok || v != "1" {
		t.Fatalf("Get(a) = (%q,%v), 期望 (1,true)", v, ok)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, 期望 1", c.Len())
	}
}

func TestMissAndCounters(t *testing.T) {
	c := New(2)
	if _, ok := c.Get("nope"); ok {
		t.Fatal("不存在的键不该命中")
	}
	c.Set("a", "1")
	c.Get("a")
	if c.Hits() != 1 {
		t.Fatalf("Hits = %d, 期望 1", c.Hits())
	}
	if c.Misses() != 1 {
		t.Fatalf("Misses = %d, 期望 1", c.Misses())
	}
}

func TestEvictionOrder(t *testing.T) {
	c := New(2)
	c.Set("a", "1")
	c.Set("b", "2")
	c.Get("a") // a 变为最近使用
	c.Set("c", "3")
	if _, ok := c.Get("b"); ok {
		t.Fatal("b 是最久未使用的, 应被淘汰")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a 刚被访问过, 不该被淘汰")
	}
	if c.Evictions() != 1 {
		t.Fatalf("Evictions = %d, 期望 1", c.Evictions())
	}
}

func TestUpdateExistingDoesNotEvict(t *testing.T) {
	c := New(2)
	c.Set("a", "1")
	c.Set("b", "2")
	c.Set("a", "9")
	if c.Evictions() != 0 {
		t.Fatalf("覆盖已有键不该淘汰, Evictions = %d", c.Evictions())
	}
	if v, _ := c.Get("a"); v != "9" {
		t.Fatalf("覆盖后 Get(a) = %q, 期望 9", v)
	}
}

func TestCapacityFloor(t *testing.T) {
	c := New(0)
	c.Set("a", "1")
	c.Set("b", "2")
	if c.Len() != 1 {
		t.Fatalf("capacity=0 应按 1 处理, Len = %d", c.Len())
	}
}

func TestManyOpsStayCorrect(t *testing.T) {
	c := New(64)
	for i := 0; i < 5000; i++ {
		c.Set(fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}
	if c.Len() != 64 {
		t.Fatalf("Len = %d, 期望 64", c.Len())
	}
	if v, ok := c.Get("k4999"); !ok || v != "v4999" {
		t.Fatalf("最后一个键应还在: (%q,%v)", v, ok)
	}
	if _, ok := c.Get("k0"); ok {
		t.Fatal("最早的键应已被淘汰")
	}
}
`,
	},

	"semver": {
		"go.mod": goModFor("example.com/semver"),
		"semver/semver.go": `package semver

// Version 一个语义化版本。
type Version struct {
	Major, Minor, Patch int
	Pre                 string // 预发布标识 (不含 '-' 前缀), 空表示没有
}

// Parse 解析 "1.2.3" 或 "1.2.3-beta.1"。
func Parse(s string) (Version, error) {
	return Version{}, nil
}

// String 还原成规范写法。
func (v Version) String() string { return "" }

// Compare 返回 -1 / 0 / 1。预发布版本**小于**同号的正式版。
func Compare(a, b Version) int { return 0 }

// Satisfies 判定 v 是否满足约束。支持 "^1.2.3" "~1.2.3" ">=1.2.0" "=1.2.3"。
// 规格见 semver/semver_test.go。
func Satisfies(v Version, constraint string) (bool, error) {
	return false, nil
}
`,
		"semver/semver_test.go": `package semver

import "testing"

func TestParse(t *testing.T) {
	v, err := Parse("1.2.3")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if v.Major != 1 || v.Minor != 2 || v.Patch != 3 || v.Pre != "" {
		t.Fatalf("解析结果 = %+v", v)
	}
	if got := v.String(); got != "1.2.3" {
		t.Fatalf("String() = %q, 期望 1.2.3", got)
	}
}

func TestParsePre(t *testing.T) {
	v, err := Parse("1.2.3-beta.1")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if v.Pre != "beta.1" {
		t.Fatalf("Pre = %q, 期望 beta.1", v.Pre)
	}
	if got := v.String(); got != "1.2.3-beta.1" {
		t.Fatalf("String() = %q", got)
	}
}

func TestParseRejects(t *testing.T) {
	for _, bad := range []string{"", "1", "1.2", "1.2.3.4", "a.b.c", "1.2.x"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q) 必须报错, 实得 nil", bad)
		}
	}
}

func TestCompare(t *testing.T) {
	mk := func(s string) Version {
		v, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		return v
	}
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.0", "2.0.0", -1},
		{"1.0.0-beta", "1.0.0", -1}, // 预发布小于正式
		{"1.0.0-alpha", "1.0.0-beta", -1},
	}
	for _, c := range cases {
		if got := Compare(mk(c.a), mk(c.b)); got != c.want {
			t.Fatalf("Compare(%s,%s) = %d, 期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSatisfiesCaret(t *testing.T) {
	mk := func(s string) Version { v, _ := Parse(s); return v }
	yes := []string{"1.2.3", "1.2.4", "1.9.0"}
	no := []string{"2.0.0", "1.2.2", "0.9.9"}
	for _, s := range yes {
		ok, err := Satisfies(mk(s), "^1.2.3")
		if err != nil || !ok {
			t.Fatalf("^1.2.3 应接受 %s (ok=%v err=%v)", s, ok, err)
		}
	}
	for _, s := range no {
		ok, err := Satisfies(mk(s), "^1.2.3")
		if err != nil || ok {
			t.Fatalf("^1.2.3 应拒绝 %s (ok=%v err=%v)", s, ok, err)
		}
	}
}

func TestSatisfiesTildeAndCmp(t *testing.T) {
	mk := func(s string) Version { v, _ := Parse(s); return v }
	if ok, _ := Satisfies(mk("1.2.9"), "~1.2.3"); !ok {
		t.Fatal("~1.2.3 应接受 1.2.9")
	}
	if ok, _ := Satisfies(mk("1.3.0"), "~1.2.3"); ok {
		t.Fatal("~1.2.3 应拒绝 1.3.0")
	}
	if ok, _ := Satisfies(mk("2.0.0"), ">=1.2.0"); !ok {
		t.Fatal(">=1.2.0 应接受 2.0.0")
	}
	if ok, _ := Satisfies(mk("1.2.3"), "=1.2.3"); !ok {
		t.Fatal("=1.2.3 应接受 1.2.3")
	}
}

func TestSatisfiesBadConstraint(t *testing.T) {
	v, _ := Parse("1.2.3")
	if _, err := Satisfies(v, "~~1.2.3"); err == nil {
		t.Fatal("非法约束必须报错, 实得 nil")
	}
}
`,
	},

	"retry": {
		"go.mod": goModFor("example.com/retry"),
		"retry/retry.go": `package retry

import (
	"context"
	"time"
)

// Clock 让测试不用真睡觉。
type Clock interface {
	// Sleep 睡 d, 或在 ctx 取消时提前返回 ctx.Err()。
	Sleep(ctx context.Context, d time.Duration) error
	// Now 当前时间。
	Now() time.Time
}

// Policy 重试策略。
type Policy struct {
	MaxAttempts int           // 总尝试次数 (含首次)。<=0 按 1 处理。
	Base        time.Duration // 首次退避。<=0 按 0 处理。
	Max         time.Duration // 退避上限。<=0 表示不限。
	Retryable   func(error) bool
}

// Do 按策略执行 fn, 返回最后一次的结果。
// 退避序列: Base, 2*Base, 4*Base ... 被 Max 截断。
// 规格见 retry/retry_test.go。
func Do(ctx context.Context, c Clock, p Policy, fn func(context.Context) error) error {
	return nil
}
`,
		"retry/retry_test.go": `package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct {
	slept []time.Duration
	t     time.Time
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.slept = append(f.slept, d)
	f.t = f.t.Add(d)
	return nil
}
func (f *fakeClock) Now() time.Time { return f.t }

func TestSucceedsFirstTry(t *testing.T) {
	fc := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	err := Do(context.Background(), fc, Policy{MaxAttempts: 3, Base: time.Second},
		func(context.Context) error { calls++; return nil })
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if calls != 1 {
		t.Fatalf("调用次数 = %d, 期望 1", calls)
	}
	if len(fc.slept) != 0 {
		t.Fatalf("首次成功不该退避, 实得 %v", fc.slept)
	}
}

func TestBackoffSequence(t *testing.T) {
	fc := &fakeClock{t: time.Unix(0, 0)}
	boom := errors.New("boom")
	calls := 0
	err := Do(context.Background(), fc, Policy{MaxAttempts: 4, Base: time.Second},
		func(context.Context) error { calls++; return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("应返回最后一次的错误, 实得 %v", err)
	}
	if calls != 4 {
		t.Fatalf("调用次数 = %d, 期望 4", calls)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(fc.slept) != len(want) {
		t.Fatalf("退避次数 = %d (%v), 期望 %d", len(fc.slept), fc.slept, len(want))
	}
	for i := range want {
		if fc.slept[i] != want[i] {
			t.Fatalf("第 %d 次退避 = %v, 期望 %v", i, fc.slept[i], want[i])
		}
	}
}

func TestBackoffCap(t *testing.T) {
	fc := &fakeClock{t: time.Unix(0, 0)}
	err := Do(context.Background(), fc, Policy{
		MaxAttempts: 5, Base: time.Second, Max: 3 * time.Second,
	}, func(context.Context) error { return errors.New("x") })
	if err == nil {
		t.Fatal("应当返回错误")
	}
	for i, d := range fc.slept {
		if d > 3*time.Second {
			t.Fatalf("第 %d 次退避 %v 超过上限 3s", i, d)
		}
	}
}

func TestNonRetryableStopsImmediately(t *testing.T) {
	fc := &fakeClock{t: time.Unix(0, 0)}
	permanent := errors.New("permanent")
	calls := 0
	err := Do(context.Background(), fc, Policy{
		MaxAttempts: 5, Base: time.Second,
		Retryable: func(e error) bool { return !errors.Is(e, permanent) },
	}, func(context.Context) error { calls++; return permanent })
	if !errors.Is(err, permanent) {
		t.Fatalf("应原样返回, 实得 %v", err)
	}
	if calls != 1 {
		t.Fatalf("不可重试的错误应只调一次, 实得 %d", calls)
	}
}

func TestContextCancelStops(t *testing.T) {
	fc := &fakeClock{t: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := Do(ctx, fc, Policy{MaxAttempts: 5, Base: time.Second},
		func(context.Context) error { calls++; return errors.New("x") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled, 实得 %v", err)
	}
	if calls > 1 {
		t.Fatalf("ctx 已取消时不该继续重试, 调用次数 = %d", calls)
	}
}

func TestMaxAttemptsFloor(t *testing.T) {
	fc := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	_ = Do(context.Background(), fc, Policy{MaxAttempts: 0},
		func(context.Context) error { calls++; return errors.New("x") })
	if calls != 1 {
		t.Fatalf("MaxAttempts<=0 应按 1 处理, 调用次数 = %d", calls)
	}
}
`,
	},

	"matrix": {
		"go.mod": goModFor("example.com/matrix"),
		"matrix/matrix.go": `package matrix

// Matrix 行优先存储的矩阵。
type Matrix struct {
	Rows, Cols int
	Data       []float64 // 长度必须是 Rows*Cols
}

// New 建一个 rows x cols 的全零矩阵。
func New(rows, cols int) *Matrix { return &Matrix{Rows: rows, Cols: cols} }

// At / Set 下标访问 (越界 panic 即可 —— 测试不查越界)。
func (m *Matrix) At(r, c int) float64     { return 0 }
func (m *Matrix) Set(r, c int, v float64) {}

// Mul 矩阵乘。a.Cols != b.Rows 时返回错误。
func Mul(a, b *Matrix) (*Matrix, error) { return nil, nil }

// Transpose 转置。
func Transpose(m *Matrix) *Matrix { return nil }

// Det 行列式。非方阵返回错误。
func Det(m *Matrix) (float64, error) { return 0, nil }

// Equal 逐元素近似相等 (容差 1e-9)。
func Equal(a, b *Matrix) bool { return false }
`,
		"matrix/matrix_test.go": `package matrix

import (
	"math"
	"testing"
)

func from(vals [][]float64) *Matrix {
	m := New(len(vals), len(vals[0]))
	for r := range vals {
		for c := range vals[r] {
			m.Set(r, c, vals[r][c])
		}
	}
	return m
}

func TestAtSet(t *testing.T) {
	m := New(2, 3)
	m.Set(1, 2, 4.5)
	if got := m.At(1, 2); got != 4.5 {
		t.Fatalf("At(1,2) = %v, 期望 4.5", got)
	}
	if m.Rows != 2 || m.Cols != 3 {
		t.Fatalf("维度 = %dx%d", m.Rows, m.Cols)
	}
}

func TestMul(t *testing.T) {
	a := from([][]float64{{1, 2, 3}, {4, 5, 6}})
	b := from([][]float64{{7, 8}, {9, 10}, {11, 12}})
	c, err := Mul(a, b)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	want := [][]float64{{58, 64}, {139, 154}}
	if c.Rows != 2 || c.Cols != 2 {
		t.Fatalf("结果维度 = %dx%d", c.Rows, c.Cols)
	}
	for r := 0; r < 2; r++ {
		for cc := 0; cc < 2; cc++ {
			if d := math.Abs(c.At(r, cc) - want[r][cc]); d > 1e-9 {
				t.Fatalf("结果[%d][%d] = %v, 期望 %v", r, cc, c.At(r, cc), want[r][cc])
			}
		}
	}
}

func TestMulDimensionMismatch(t *testing.T) {
	a := New(2, 3)
	b := New(2, 2)
	if _, err := Mul(a, b); err == nil {
		t.Fatal("维度不匹配必须报错, 实得 nil")
	}
}

func TestTranspose(t *testing.T) {
	a := from([][]float64{{1, 2, 3}, {4, 5, 6}})
	tr := Transpose(a)
	if tr.Rows != 3 || tr.Cols != 2 {
		t.Fatalf("转置维度 = %dx%d, 期望 3x2", tr.Rows, tr.Cols)
	}
	if tr.At(2, 1) != 6 {
		t.Fatalf("转置[2][1] = %v, 期望 6", tr.At(2, 1))
	}
}

func TestDet(t *testing.T) {
	cases := []struct {
		m    [][]float64
		want float64
	}{
		{[][]float64{{5}}, 5},
		{[][]float64{{1, 2}, {3, 4}}, -2},
		{[][]float64{{6, 1, 1}, {4, -2, 5}, {2, 8, 7}}, -306},
	}
	for i, c := range cases {
		got, err := Det(from(c.m))
		if err != nil {
			t.Fatalf("用例 %d 报错: %v", i, err)
		}
		if math.Abs(got-c.want) > 1e-6 {
			t.Fatalf("用例 %d 行列式 = %v, 期望 %v", i, got, c.want)
		}
	}
}

func TestDetNonSquare(t *testing.T) {
	if _, err := Det(New(2, 3)); err == nil {
		t.Fatal("非方阵求行列式必须报错, 实得 nil")
	}
}

func TestEqual(t *testing.T) {
	a := from([][]float64{{1, 2}, {3, 4}})
	b := from([][]float64{{1, 2}, {3, 4}})
	if !Equal(a, b) {
		t.Fatal("相同矩阵应判等")
	}
	b.Set(1, 1, 4.1)
	if Equal(a, b) {
		t.Fatal("相差 0.1 不该判等")
	}
}
`,
	},

	"csvq": {
		"go.mod": goModFor("example.com/csvq"),
		"csvq/csvq.go": `package csvq

import "io"

// Table 一张表: 列名 + 行。
type Table struct {
	Cols []string
	Rows [][]string
}

// ParseCSV 解析带表头的 CSV。
func ParseCSV(r io.Reader) (*Table, error) { return nil, nil }

// Query 执行最小查询。支持:
//
//	select a, b            (省略 = 全部列)
//	where a == "x"         (只支持 == 与 != 和字符串字面量)
//	order by a [desc]
//	limit N
//
// 子句顺序固定为 select / where / order / limit, 各自可出现 0 或 1 次。
// 规格见 csvq/csvq_test.go。
func (t *Table) Query(q string) (*Table, error) { return nil, nil }

// Col 按列名取列号, 不存在返回 -1。
func (t *Table) Col(name string) int { return -1 }
`,
		"csvq/csvq_test.go": `package csvq

import (
	"strings"
	"testing"
)

const sample = "name,age,city\nbob,30,sh\nalice,25,bj\ncarol,35,sh\n"

func mustParse(t *testing.T) *Table {
	t.Helper()
	tb, err := ParseCSV(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return tb
}

func TestParseCSV(t *testing.T) {
	tb := mustParse(t)
	if len(tb.Cols) != 3 || tb.Cols[0] != "name" || tb.Cols[2] != "city" {
		t.Fatalf("列名 = %v", tb.Cols)
	}
	if len(tb.Rows) != 3 {
		t.Fatalf("行数 = %d, 期望 3", len(tb.Rows))
	}
	if tb.Rows[1][0] != "alice" {
		t.Fatalf("第二行首列 = %q", tb.Rows[1][0])
	}
}

func TestCol(t *testing.T) {
	tb := mustParse(t)
	if got := tb.Col("age"); got != 1 {
		t.Fatalf("Col(age) = %d, 期望 1", got)
	}
	if got := tb.Col("nope"); got != -1 {
		t.Fatalf("Col(nope) = %d, 期望 -1", got)
	}
}

func TestQuerySelect(t *testing.T) {
	tb := mustParse(t)
	out, err := tb.Query("select name, city")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(out.Cols) != 2 || out.Cols[0] != "name" || out.Cols[1] != "city" {
		t.Fatalf("列 = %v", out.Cols)
	}
	if len(out.Rows) != 3 || out.Rows[0][0] != "bob" || out.Rows[0][1] != "sh" {
		t.Fatalf("行 = %v", out.Rows)
	}
}

func TestQueryWhere(t *testing.T) {
	tb := mustParse(t)
	out, err := tb.Query("select name where city == \"sh\"")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(out.Rows) != 2 {
		t.Fatalf("行数 = %d, 期望 2 (%v)", len(out.Rows), out.Rows)
	}
	for _, r := range out.Rows {
		if r[0] == "alice" {
			t.Fatal("alice 的 city 是 bj, 不该被选中")
		}
	}
}

func TestQueryWhereNotEqual(t *testing.T) {
	tb := mustParse(t)
	out, err := tb.Query("select name where city != \"sh\"")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(out.Rows) != 1 || out.Rows[0][0] != "alice" {
		t.Fatalf("行 = %v, 期望只有 alice", out.Rows)
	}
}

func TestQueryOrder(t *testing.T) {
	tb := mustParse(t)
	out, err := tb.Query("select name order by age")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if out.Rows[0][0] != "alice" || out.Rows[2][0] != "carol" {
		t.Fatalf("升序结果 = %v", out.Rows)
	}
	desc, err := tb.Query("select name order by age desc")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if desc.Rows[0][0] != "carol" {
		t.Fatalf("降序结果 = %v", desc.Rows)
	}
}

func TestQueryLimit(t *testing.T) {
	tb := mustParse(t)
	out, err := tb.Query("select name order by age limit 2")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(out.Rows) != 2 {
		t.Fatalf("行数 = %d, 期望 2", len(out.Rows))
	}
}

func TestQueryErrors(t *testing.T) {
	tb := mustParse(t)
	for _, bad := range []string{
		"select nope",              // 未知列
		"select name where x == \"y\"", // 未知列
		"select name limit",        // limit 缺参数
		"delete name",              // 不支持的语句
	} {
		if _, err := tb.Query(bad); err == nil {
			t.Fatalf("Query(%q) 必须报错, 实得 nil", bad)
		}
	}
}
`,
	},

	"jsonpath": {
		"go.mod": goModFor("example.com/jsonpath"),
		"jsonpath/jsonpath.go": `package jsonpath

// Get 按最小路径语法取值。
//
// 支持: $.a.b  $.a[0]  $.a[0].b
// 不支持通配/切片/递归 —— 遇到就返回错误。
// 缺字段必须返回错误 (而不是零值), 这是本包与"随便取一下"的全部区别。
// 规格见 jsonpath/jsonpath_test.go。
func Get(doc []byte, path string) (any, error) { return nil, nil }
`,
		"jsonpath/jsonpath_test.go": `package jsonpath

import "testing"

const doc = ` + "`" + `{
  "user": {"name": "bob", "tags": ["a", "b"], "age": 30},
  "count": 2,
  "ok": true
}` + "`" + `

func TestGetNested(t *testing.T) {
	v, err := Get([]byte(doc), "$.user.name")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if v != "bob" {
		t.Fatalf("取值 = %v (%T), 期望 bob", v, v)
	}
}

func TestGetIndex(t *testing.T) {
	v, err := Get([]byte(doc), "$.user.tags[1]")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if v != "b" {
		t.Fatalf("取值 = %v, 期望 b", v)
	}
}

func TestGetTypes(t *testing.T) {
	v, err := Get([]byte(doc), "$.count")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if f, ok := v.(float64); !ok || f != 2 {
		t.Fatalf("数值取值 = %v (%T), 期望 float64(2)", v, v)
	}
	b, err := Get([]byte(doc), "$.ok")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if b != true {
		t.Fatalf("布尔取值 = %v, 期望 true", b)
	}
}

func TestGetMissingFieldErrors(t *testing.T) {
	for _, p := range []string{"$.user.nope", "$.nope", "$.user.tags[9]", "$.count.x"} {
		if _, err := Get([]byte(doc), p); err == nil {
			t.Fatalf("Get(%q) 必须报错 (缺字段/越界/类型不符), 实得 nil", p)
		}
	}
}

func TestGetRejectsUnsupported(t *testing.T) {
	for _, p := range []string{"$..name", "$.user.*", "$.user.tags[0:2]", "user.name", ""} {
		if _, err := Get([]byte(doc), p); err == nil {
			t.Fatalf("Get(%q) 必须报错 (不支持的语法), 实得 nil", p)
		}
	}
}

func TestGetBadJSON(t *testing.T) {
	if _, err := Get([]byte("{not json"), "$.a"); err == nil {
		t.Fatal("非法 JSON 必须报错, 实得 nil")
	}
}
`,
	},
}

// fixtureFiles 取一个 fixture 的文件表。
func fixtureFiles(name string) (map[string]string, bool) {
	f, ok := fixtures[name]
	return f, ok
}

// Materialize 把 fixture 展开到 dir 下 (dir 不存在则创建)。
//
// 文件按路径排序写入, 于是同样的 fixture 在任何机器上产生**逐字节相同**的目录 ——
// H4-2 要两组运行"除被测开关外无其他差异", 而初始代码是那份差异的一部分。
// map 遍历顺序随机, 不排序的话两个 tmpdir 里的写入顺序不同 (内容虽同), 一旦将来
// 有人在展开时加时间戳/随机量, 顺序就会泄漏进产物。
func Materialize(dir, name string) error {
	files, ok := fixtures[name]
	if !ok {
		return fmt.Errorf("fixture %q 不存在 (已有: %v)", name, FixtureNames())
	}
	if len(files) == 0 {
		return fmt.Errorf("fixture %q 是空的", name)
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		// 拒绝逃逸路径: fixture 内容是我们自己写的, 但 Materialize 是导出的,
		// 一个 "../" 就能让它往工作目录外写文件。
		clean := filepath.Clean(p)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return fmt.Errorf("fixture %q 里的路径 %q 逃逸出工作目录", name, p)
		}
		full := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("建目录失败 %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(files[p]), 0o644); err != nil {
			return fmt.Errorf("写文件失败 %s: %w", full, err)
		}
	}
	return nil
}
