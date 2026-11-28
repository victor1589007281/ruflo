package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// TestCanonicalArgsHash8_KeyOrderInvariant 是 args_hash 的立身之本:
// 键序不同、语义相同的 JSON 必须得到同一个哈希 —— 否则循环检测会把
// 同一个调用误判成两个不同调用。
func TestCanonicalArgsHash8_KeyOrderInvariant(t *testing.T) {
	a := canonicalArgsHash8([]byte(`{"path":"/tmp/x","limit":10,"offset":0}`))
	b := canonicalArgsHash8([]byte(`{"offset":0,"limit":10,"path":"/tmp/x"}`))
	if a == "" || a != b {
		t.Fatalf("键序不变性失败: %q vs %q", a, b)
	}
}

func TestCanonicalArgsHash8_WhitespaceInvariant(t *testing.T) {
	a := canonicalArgsHash8([]byte(`{"a":1,"b":[1,2]}`))
	b := canonicalArgsHash8([]byte("{ \"a\": 1, \"b\": [1, 2] }"))
	if a != b {
		t.Fatalf("空白不变性失败: %q vs %q", a, b)
	}
}

func TestCanonicalArgsHash8_DifferentArgsDiffer(t *testing.T) {
	a := canonicalArgsHash8([]byte(`{"path":"/tmp/x"}`))
	b := canonicalArgsHash8([]byte(`{"path":"/tmp/y"}`))
	if a == b {
		t.Fatalf("不同入参得到相同哈希: %q", a)
	}
}

func TestCanonicalArgsHash8_InvalidJSONFallsBackToRaw(t *testing.T) {
	// 非法 JSON: 退化为原文哈希, 仍要是 8 hex 且稳定。
	raw := []byte(`{"truncated":`)
	a := canonicalArgsHash8(raw)
	b := canonicalArgsHash8(raw)
	if len(a) != 8 || a != b {
		t.Fatalf("非法 JSON 回落失败: %q vs %q", a, b)
	}
	// 与合法 JSON 不混淆。
	c := canonicalArgsHash8([]byte(`{"truncated":1}`))
	if a == c {
		t.Fatalf("非法与合法输入撞哈希: %q", a)
	}
}

func TestCanonicalArgsHash8_Empty(t *testing.T) {
	if got := canonicalArgsHash8(nil); got != "" {
		t.Fatalf("空输入应得空串, 得 %q", got)
	}
	if got := canonicalArgsHash8([]byte{}); got != "" {
		t.Fatalf("空输入应得空串, 得 %q", got)
	}
}

// TestCanonicalArgsHash8_MatchesSpec 钉死验收口径: 重算算法 = canonical JSON
// (unmarshal→marshal) → sha256 → hex[:8]。用独立实现重算一遍比对 —— 这模拟的
// 正是 P1 验收里"用原始 args 重算"的那只手。
func TestCanonicalArgsHash8_MatchesSpec(t *testing.T) {
	raw := []byte(`{"command": "ls -la", "timeout": 120}`)
	want := recomputeArgsHashSpec(t, raw)
	if got := canonicalArgsHash8(raw); got != want {
		t.Fatalf("与规格重算不一致: got %q want %q", got, want)
	}
}

// recomputeArgsHashSpec 是验收口径的独立实现 (不复用被测函数的任何中间产物,
// 只用标准库原语按规格手写一遍)。
func recomputeArgsHashSpec(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("规格重算要求合法 JSON: %v", err)
	}
	canon, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("规格重算 marshal 失败: %v", err)
	}
	h := sha256.Sum256(canon)
	return hex.EncodeToString(h[:])[:8]
}
