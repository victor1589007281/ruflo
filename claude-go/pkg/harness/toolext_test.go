package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// 假工具
// ============================================================================

// declaredTool 是实现了 ToolMeta 的工具。
type declaredTool struct {
	name    string
	effect  SideEffect
	confirm bool
	owner   bool
	timeout time.Duration
	maxRes  int
}

func (d *declaredTool) Name() string                           { return d.name }
func (d *declaredTool) Description() string                    { return "declared" }
func (d *declaredTool) InputSchema() json.RawMessage           { return json.RawMessage(`{"type":"object"}`) }
func (d *declaredTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (d *declaredTool) IsReadOnly(json.RawMessage) bool        { return d.effect == SideEffectRead }
func (d *declaredTool) Call(context.Context, json.RawMessage, *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: "ok"}, nil
}
func (d *declaredTool) CheckPermissions(json.RawMessage, *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (d *declaredTool) SideEffect() SideEffect  { return d.effect }
func (d *declaredTool) NeedsConfirmation() bool { return d.confirm }
func (d *declaredTool) OwnerScoped() bool       { return d.owner }
func (d *declaredTool) Timeout() time.Duration  { return d.timeout }
func (d *declaredTool) MaxResultBytes() int     { return d.maxRes }

// plainTool 只实现 tool.Tool，不实现 ToolMeta。
type plainTool struct{ name string }

func (p *plainTool) Name() string                           { return p.name }
func (p *plainTool) Description() string                    { return "plain" }
func (p *plainTool) InputSchema() json.RawMessage           { return json.RawMessage(`{"type":"object"}`) }
func (p *plainTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (p *plainTool) IsReadOnly(json.RawMessage) bool        { return true }
func (p *plainTool) Call(context.Context, json.RawMessage, *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: "ok"}, nil
}
func (p *plainTool) CheckPermissions(json.RawMessage, *tool.ToolContext) *types.PermissionResult {
	return nil
}

// ============================================================================
// ToolMetaOf
// ============================================================================

// TestToolMetaOf_未声明元数据failClosed 是本文件的基石：没有声明的工具
// 必须被当成"写、要确认、按 owner 隔离"，绝不能因为名字看不出危险就当只读。
func TestToolMetaOf_未声明元数据failClosed(t *testing.T) {
	m := ToolMetaOf(&plainTool{name: "exec_sql"})
	if m.SideEffect() != SideEffectWrite {
		t.Errorf("默认 SideEffect = %v, 期望 write", m.SideEffect())
	}
	if !m.NeedsConfirmation() {
		t.Error("默认 NeedsConfirmation 应为 true")
	}
	if !m.OwnerScoped() {
		t.Error("默认 OwnerScoped 应为 true")
	}
	if m.Timeout() != 0 {
		t.Errorf("默认 Timeout 应为 0(不限时), 实际 %v", m.Timeout())
	}
	if m.MaxResultBytes() != 0 {
		t.Errorf("默认 MaxResultBytes 应为 0(不截断), 实际 %d", m.MaxResultBytes())
	}
}

// TestToolMetaOf_已声明元数据原样返回。
func TestToolMetaOf_已声明元数据原样返回(t *testing.T) {
	d := &declaredTool{
		name: "memory_search", effect: SideEffectRead, confirm: false, owner: true,
		timeout: 3 * time.Second, maxRes: 4096,
	}
	m := ToolMetaOf(d)
	if m.SideEffect() != SideEffectRead || m.NeedsConfirmation() || !m.OwnerScoped() ||
		m.Timeout() != 3*time.Second || m.MaxResultBytes() != 4096 {
		t.Errorf("声明值被改写: effect=%v confirm=%v owner=%v timeout=%v max=%d",
			m.SideEffect(), m.NeedsConfirmation(), m.OwnerScoped(), m.Timeout(), m.MaxResultBytes())
	}
}

// TestToolMetaOf_nil工具不panic。
func TestToolMetaOf_nil工具不panic(t *testing.T) {
	m := ToolMetaOf(nil)
	if m == nil {
		t.Fatal("nil 工具应返回默认元数据, 实际 nil")
	}
	if m.SideEffect() != SideEffectWrite {
		t.Errorf("nil 工具的默认 SideEffect = %v, 期望 write", m.SideEffect())
	}
}

// TestSideEffect_StringUnknownFallsForward 未知取值必须呈现成危险的一档。
func TestSideEffect_StringUnknownFallsForward(t *testing.T) {
	if got := SideEffectRead.String(); got != "read" {
		t.Errorf("read = %q", got)
	}
	if got := SideEffectWrite.String(); got != "write" {
		t.Errorf("write = %q", got)
	}
	if got := SideEffectExternal.String(); got != "external" {
		t.Errorf("external = %q", got)
	}
	if got := SideEffect(99).String(); !strings.HasPrefix(got, "external") {
		t.Errorf("未知取值应落到 external, 实际 %q", got)
	}
}

// ============================================================================
// 陪伴档位
// ============================================================================

// TestCompanionAllowedTools_恰好是三类 锁死白名单的内容。
func TestCompanionAllowedTools_恰好是三类(t *testing.T) {
	got := CompanionAllowedTools()
	want := map[string]bool{
		"memory_search":  true,
		"memory_read":    true,
		"memory_correct": true,
		"Read":           true,
	}
	if len(got) != len(want) {
		t.Fatalf("白名单 = %v, 期望 %d 项", got, len(want))
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("白名单出现未预期工具 %q", n)
		}
	}
	// 排序后再比对，确保顺序确定。
	if !sortedStrings(got) {
		t.Errorf("白名单未排序: %v", got)
	}
}

func sortedStrings(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

// TestCompanionAllowedTools_危险工具不在其中 直接钉住 R-H9 的禁令。
func TestCompanionAllowedTools_危险工具不在其中(t *testing.T) {
	for _, dangerous := range []string{"Bash", "Write", "Edit", "StrReplace", "Glob", "Grep", "code_exec", "Exec"} {
		if IsCompanionTool(dangerous) {
			t.Errorf("危险工具 %q 出现在陪伴白名单中", dangerous)
		}
	}
}

// TestCompanionAllowedTools_返回副本 防止调用方改到内部真相源。
func TestCompanionAllowedTools_返回副本(t *testing.T) {
	got := CompanionAllowedTools()
	got[0] = "Bash"
	if IsCompanionTool("Bash") {
		t.Error("修改返回值污染了内部白名单")
	}
}

// TestApplyCompanionProfile_nil收窄为完整陪伴集合 是最常见路径。
func TestApplyCompanionProfile_nil收窄为完整陪伴集合(t *testing.T) {
	var allowed []string // nil 切片 = 不受白名单限制
	if err := ApplyCompanionProfile(&allowed); err != nil {
		t.Fatalf("ApplyCompanionProfile: %v", err)
	}
	if allowed == nil {
		t.Fatal("应把 nil 置为陪伴集合, 实际仍为 nil(= 不受限)")
	}
	if len(allowed) == 0 {
		t.Fatal("陪伴集合不能为空")
	}
	for _, n := range allowed {
		if !IsCompanionTool(n) {
			t.Errorf("收窄后出现非陪伴工具 %q", n)
		}
	}
}

// TestApplyCompanionProfile_非nil与陪伴集合求交 —— 白名单只能收窄。
func TestApplyCompanionProfile_非nil与陪伴集合求交(t *testing.T) {
	allowed := []string{"Bash", "Read", "memory_search", "Write", "Grep"}
	if err := ApplyCompanionProfile(&allowed); err != nil {
		t.Fatalf("ApplyCompanionProfile: %v", err)
	}
	want := map[string]bool{"Read": true, "memory_search": true}
	if len(allowed) != len(want) {
		t.Fatalf("求交结果 = %v, 期望 %v", allowed, want)
	}
	for _, n := range allowed {
		if !want[n] {
			t.Errorf("求交结果出现 %q", n)
		}
	}
}

// TestApplyCompanionProfile_空集求交仍为空 且不报错：
// 空集 = 全部拒绝，是合法的 fail-closed 结果，不是配置错误。
func TestApplyCompanionProfile_空集求交仍为空(t *testing.T) {
	allowed := []string{}
	if err := ApplyCompanionProfile(&allowed); err != nil {
		t.Fatalf("空集不应报错: %v", err)
	}
	if len(allowed) != 0 {
		t.Errorf("空集求交后应仍为空, 实际 %v", allowed)
	}
}

// TestApplyCompanionProfile_无交集报错 —— 配置对不上要说出来，不能静默跑一个哑巴会话。
func TestApplyCompanionProfile_无交集报错(t *testing.T) {
	allowed := []string{"Bash", "Write"}
	err := ApplyCompanionProfile(&allowed)
	if err == nil {
		t.Fatal("无交集应报错")
	}
	if !strings.Contains(err.Error(), "无交集") {
		t.Errorf("错误应说明无交集, 实际 %v", err)
	}
}

// TestApplyCompanionProfile_去重且顺序确定。
func TestApplyCompanionProfile_去重且顺序确定(t *testing.T) {
	a := []string{"Read", "memory_read", "Read", "memory_search", "memory_search"}
	b := []string{"memory_search", "Read", "memory_read", "Read"}
	if err := ApplyCompanionProfile(&a); err != nil {
		t.Fatalf("a: %v", err)
	}
	if err := ApplyCompanionProfile(&b); err != nil {
		t.Fatalf("b: %v", err)
	}
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("同一集合的不同输入产生不同白名单: %v vs %v", a, b)
	}
	if len(a) != 3 {
		t.Errorf("应去重到 3 项, 实际 %v", a)
	}
}

// TestApplyCompanionProfile_nil指针报错。
func TestApplyCompanionProfile_nil指针报错(t *testing.T) {
	if err := ApplyCompanionProfile(nil); err == nil {
		t.Fatal("nil 指针应报错")
	}
}

// ============================================================================
// IdempotencyKey
// ============================================================================

// TestIdempotencyKey_键序无关 是最核心的性质：同一次调用的参数不应因
// JSON 字段顺序不同而被算成两次。
func TestIdempotencyKey_键序无关(t *testing.T) {
	a := IdempotencyKey("memory_write", json.RawMessage(`{"a":1,"b":2}`))
	b := IdempotencyKey("memory_write", json.RawMessage(`{ "b" : 2 , "a" : 1 }`))
	if a != b {
		t.Errorf("键序/空白影响了幂等键:\n a=%s\n b=%s", a, b)
	}
}

// TestIdempotencyKey_跨进程稳定 用固定黄金值钉住实现：
// 换哈希算法或引入随机盐会让历史幂等键全部失效（重放去重整体崩掉）。
func TestIdempotencyKey_跨进程稳定(t *testing.T) {
	// sha256("Bash" + 0x00 + `{"a":1,"b":2}`)
	const want = "983f152d761345c9088414239f72adbff0d8fcf6624b861c1751513c512a2e3b"
	got := IdempotencyKey("Bash", json.RawMessage(`{"b":2,"a":1}`))
	if got != want {
		t.Errorf("黄金值不匹配:\n got=%s\nwant=%s", got, want)
	}
}

// TestIdempotencyKey_工具名参与哈希。
func TestIdempotencyKey_工具名参与哈希(t *testing.T) {
	in := json.RawMessage(`{"x":1}`)
	if IdempotencyKey("Read", in) == IdempotencyKey("Write", in) {
		t.Error("不同工具名产生相同幂等键")
	}
}

// TestIdempotencyKey_参数不同则键不同。
func TestIdempotencyKey_参数不同则键不同(t *testing.T) {
	if IdempotencyKey("T", json.RawMessage(`{"x":1}`)) == IdempotencyKey("T", json.RawMessage(`{"x":2}`)) {
		t.Error("不同参数产生相同幂等键")
	}
}

// TestIdempotencyKey_大整数不丢精度：默认 float64 会把 2^53 与 2^53+1 压到一起，
// 两个不同的数据库 id 于是撞成同一个幂等键——去重会把两个不同操作合并成一个。
func TestIdempotencyKey_大整数不丢精度(t *testing.T) {
	a := IdempotencyKey("db", json.RawMessage(`{"id":9007199254740992}`))
	b := IdempotencyKey("db", json.RawMessage(`{"id":9007199254740993}`))
	if a == b {
		t.Error("相邻大整数被压成同一个键 (UseNumber 未生效)")
	}
}

// TestIdempotencyKey_null与空输入等价。
func TestIdempotencyKey_null与空输入等价(t *testing.T) {
	a := IdempotencyKey("noargs", nil)
	b := IdempotencyKey("noargs", json.RawMessage(`null`))
	c := IdempotencyKey("noargs", json.RawMessage("   "))
	if a != b || b != c {
		t.Errorf("空输入形态不同键: %s / %s / %s", a, b, c)
	}
}

// TestIdempotencyKey_非法JSON仍确定且可区分。
func TestIdempotencyKey_非法JSON仍确定且可区分(t *testing.T) {
	bad1 := json.RawMessage(`{"x":`)
	bad2 := json.RawMessage(`{"y":`)
	k1 := IdempotencyKey("T", bad1)
	k2 := IdempotencyKey("T", bad2)
	if k1 == "" {
		t.Fatal("非法 JSON 不应返回空键")
	}
	if k1 != IdempotencyKey("T", bad1) {
		t.Error("同一非法输入两次结果不同")
	}
	if k1 == k2 {
		t.Error("不同非法输入不应共用同一个键")
	}
}

// TestIdempotencyKey_键序无关含嵌套 证明规范化是递归的。
func TestIdempotencyKey_键序无关含嵌套(t *testing.T) {
	a := IdempotencyKey("T", json.RawMessage(`{"outer":{"a":1,"b":[{"x":2,"y":3}]}}`))
	b := IdempotencyKey("T", json.RawMessage(`{"outer":{"b":[{"y":3,"x":2}],"a":1}}`))
	if a != b {
		t.Error("嵌套对象键序影响了幂等键")
	}
}

// TestIdempotencyKey_长度固定 是 64 位 hex。
func TestIdempotencyKey_长度是64(t *testing.T) {
	if got := IdempotencyKey("T", json.RawMessage(`{}`)); len(got) != 64 {
		t.Errorf("键长度 = %d, 期望 64 (sha256 hex)", len(got))
	}
}

// ============================================================================
// TruncateResult
// ============================================================================

// TestTruncateResult_不超限原样返回。
func TestTruncateResult_不超限原样返回(t *testing.T) {
	s := "hello"
	out, tr := TruncateResult(s, 10)
	if out != s || tr {
		t.Errorf("未超限被改动: out=%q truncated=%v", out, tr)
	}
}

// TestTruncateResult_非正上限不截断 与 MaxResultBytes 的 0 语义一致。
func TestTruncateResult_非正上限不截断(t *testing.T) {
	if out, tr := TruncateResult("hello", 0); out != "hello" || tr {
		t.Errorf("maxBytes=0 应不截断: out=%q truncated=%v", out, tr)
	}
	if out, tr := TruncateResult("hello", -1); out != "hello" || tr {
		t.Errorf("maxBytes<0 应不截断: out=%q truncated=%v", out, tr)
	}
}

// TestTruncateResult_ASCII精确截断。
func TestTruncateResult_ASCII精确截断(t *testing.T) {
	out, tr := TruncateResult("hello world", 5)
	if out != "hello" || !tr {
		t.Errorf("out=%q truncated=%v", out, tr)
	}
}

// TestTruncateResult_不切断多字节rune 是本函数存在的理由。
// 逐个字节上限扫一遍：任何一处切在多字节字符中间都会产生非法 UTF-8。
func TestTruncateResult_不切断多字节rune(t *testing.T) {
	// 混合 1/2/3/4 字节字符：a(1) é(2) 汉(3) 😀(4)
	s := "aé汉😀bc"
	for limit := 1; limit < len(s); limit++ {
		out, tr := TruncateResult(s, limit)
		if !tr {
			continue
		}
		if !utf8.ValidString(out) {
			t.Fatalf("limit=%d 产生了非法 UTF-8: %q (% x)", limit, out, []byte(out))
		}
		if !strings.HasPrefix(s, out) {
			t.Fatalf("limit=%d 结果不是原串前缀: %q", limit, out)
		}
		if len(out) > limit {
			t.Fatalf("limit=%d 结果超长: %d 字节", limit, len(out))
		}
	}
}

// TestTruncateResult_边界恰在rune起点 不白退一个字符。
func TestTruncateResult_边界恰在rune起点(t *testing.T) {
	// "汉" 占 3 字节；limit=3 恰好落在第一个字符之后。
	out, tr := TruncateResult("汉汉", 3)
	if out != "汉" || !tr {
		t.Errorf("边界处理错误: out=%q truncated=%v", out, tr)
	}
}

// TestTruncateResult_四字节字符中间 必须退到字符起点（退 1/2/3 字节）。
func TestTruncateResult_四字节字符中间(t *testing.T) {
	// 😀 = F0 9F 98 80 (4 字节)。紧随其后的 'b' 让 len 超过 limit。
	s := "😀b"
	for _, limit := range []int{1, 2, 3} {
		out, tr := TruncateResult(s, limit)
		if !tr {
			t.Fatalf("limit=%d 应截断", limit)
		}
		if out != "" {
			t.Errorf("limit=%d 应退到 0 字节, 实际 %q", limit, out)
		}
	}
}

// TestTruncateResult_仅ASCII超限 覆盖常见路径。
func TestTruncateResult_仅ASCII超限(t *testing.T) {
	s := strings.Repeat("x", 100)
	out, tr := TruncateResult(s, 10)
	if len(out) != 10 || !tr {
		t.Errorf("len=%d truncated=%v", len(out), tr)
	}
}

// TestTruncateResult_空串。
func TestTruncateResult_空串(t *testing.T) {
	out, tr := TruncateResult("", 5)
	if out != "" || tr {
		t.Errorf("空串: out=%q truncated=%v", out, tr)
	}
}
