package harness

import (
	"context"
	"errors"
	"testing"
)

// TestOwnerNormalize 钉住：空/空白 → DefaultOwnerID，其余原样（不 strip 内部、
// 不改大小写）。
func TestOwnerNormalize(t *testing.T) {
	cases := map[string]string{
		"":        DefaultOwnerID,
		"   ":     DefaultOwnerID,
		"\t\n ":   DefaultOwnerID,
		"alice":   "alice",
		" alice ": "alice",
		"Alice":   "Alice",
	}
	for in, want := range cases {
		if got := NormalizeOwner(in); got != want {
			t.Fatalf("NormalizeOwner(%q)=%q，want %q", in, got, want)
		}
	}
	if DefaultOwnerID == "" {
		t.Fatal("DefaultOwnerID 不能为空（空默认会让隔离断言恒真）")
	}
}

// TestOwnerContext 钉住：WithOwner/OwnerFromContext 往返；缺失或空白走默认；
// 自定义 key 类型不会被同名其他值干扰。
func TestOwnerContext(t *testing.T) {
	ctx := WithOwner(context.Background(), "alice")
	if got := OwnerFromContext(ctx); got != "alice" {
		t.Fatalf("got %q，want alice", got)
	}
	if got := OwnerFromContext(WithOwner(context.Background(), "  ")); got != DefaultOwnerID {
		t.Fatalf("空白 owner 未归一: %q", got)
	}
	if got := OwnerFromContext(context.Background()); got != DefaultOwnerID {
		t.Fatalf("无 owner 的 ctx got %q，want %q", got, DefaultOwnerID)
	}
	var nilCtx context.Context
	if got := OwnerFromContext(nilCtx); got != DefaultOwnerID {
		t.Fatalf("nil ctx got %q", got)
	}
	if WithOwner(nilCtx, "x") != nil {
		t.Fatal("WithOwner(nil) 应返回 nil，与 context 包约定一致")
	}
	// 别的包用字符串 key "owner" 写入的值不得被读到（类型不同）。
	poisoned := context.WithValue(context.Background(), "owner", "mallory")
	if got := OwnerFromContext(poisoned); got != DefaultOwnerID {
		t.Fatalf("字符串 key 污染了 owner: %q", got)
	}
	// 后写覆盖先写。
	ctx2 := WithOwner(WithOwner(context.Background(), "a"), "b")
	if got := OwnerFromContext(ctx2); got != "b" {
		t.Fatalf("覆盖失败: %q", got)
	}
}

// TestOwnerCheckSameOwner 钉住：同 owner（含空/默认互认）放行，不同 owner 返回
// 分类为 safety 的 typed error 且 errors.Is 能认出哨兵。
func TestOwnerCheckSameOwner(t *testing.T) {
	alice := WithOwner(context.Background(), "alice")

	if err := CheckSameOwner(alice, "alice"); err != nil {
		t.Fatalf("同 owner 被拒: %v", err)
	}
	// 归一化后等价：显式默认 vs 空目标。
	if err := CheckSameOwner(WithOwner(context.Background(), ""), ""); err != nil {
		t.Fatalf("空/默认互认失败: %v", err)
	}
	if err := CheckSameOwner(WithOwner(context.Background(), " local "), "local"); err != nil {
		t.Fatalf("空白归一后应同 owner: %v", err)
	}
	// 大小写是身份的一部分，不得互相放行。
	if err := CheckSameOwner(alice, "Alice"); err == nil {
		t.Fatal("大小写不同被误放行")
	}

	err := CheckSameOwner(alice, "bob")
	if err == nil {
		t.Fatal("跨 owner 未报错")
	}
	if !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("errors.Is 未认出哨兵: %v", err)
	}
	var herr *Error
	if !errors.As(err, &herr) {
		t.Fatalf("不是 *harness.Error: %T", err)
	}
	if herr.Kind != ErrKindSafety {
		t.Fatalf("Kind=%q，want safety", herr.Kind)
	}

	// 无身份 ctx（默认 local）访问他人资源必须被拒 —— 这正是空 owner 恒真漏洞的
	// 反面用例。
	if err := CheckSameOwner(context.Background(), "alice"); err == nil {
		t.Fatal("默认 owner 访问 alice 资源被放行")
	}
}

// TestOwnerScopedKey 钉住：namespaced key 单射 —— 不同 (owner,key) 拆分不碰撞，
// 含分隔符/长度边界也不碰撞。
func TestOwnerScopedKey(t *testing.T) {
	if OwnerScopedKey("a", "bc") == OwnerScopedKey("ab", "c") {
		t.Fatal("长度前缀失效：a/bc 与 ab/c 碰撞")
	}
	if OwnerScopedKey("alice", "k") == OwnerScopedKey("alice", "k2") {
		t.Fatal("不同 key 碰撞")
	}
	if OwnerScopedKey("alice", "k") == OwnerScopedKey("alice2", "k") {
		t.Fatal("不同 owner 碰撞")
	}
	// 分隔符本身出现在字段里时也要单射。
	adversarial := [][2]string{
		{"a:b", "c"},
		{"a", "b:c"},
		{"a/b", "c"},
		{"a", "b/c"},
		{"1:a/b", "c"},
		{"a", "1:b/c"},
		{"", "x"},
		{"x", ""},
	}
	seen := make(map[string][2]string, len(adversarial))
	for _, pair := range adversarial {
		k := OwnerScopedKey(pair[0], pair[1])
		if prev, ok := seen[k]; ok {
			t.Fatalf("碰撞: %v 与 %v 都得到 %q", prev, pair, k)
		}
		seen[k] = pair
	}
	// 空 owner 归一为默认，故与显式默认同 key。
	if OwnerScopedKey("", "k") != OwnerScopedKey(DefaultOwnerID, "k") {
		t.Fatal("空 owner 未归一（同一逻辑 owner 会分裂成两套命名空间）")
	}
	// 形状可自解释：<len>:<owner>/<key>。
	if got, want := OwnerScopedKey("alice", "sess/1"), "5:alice/sess/1"; got != want {
		t.Fatalf("OwnerScopedKey=%q，want %q", got, want)
	}
}
