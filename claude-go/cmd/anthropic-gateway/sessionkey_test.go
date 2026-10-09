package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqWithSession(v string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://x/v1/messages", nil)
	if v != "" {
		r.Header.Set(claudeSessionHeader, v)
	}
	return r
}

// 对照组的键原样透传: 不归一化的键必须逐字节不变, 否则关掉实验也会改路由。
func TestNormalizeSessionKey_Passthrough(t *testing.T) {
	for _, v := range []string{"run-1", "gw-abc", "uuid-4f2a", "", "含有中文的会话"} {
		if got := normalizeSessionKey(v); got != v {
			t.Errorf("不含分隔符的键应原样返回: %q → %q", v, got)
		}
	}
}

// 实验臂的键归一为 RunID 前缀。
func TestNormalizeSessionKey_StripsNode(t *testing.T) {
	got := normalizeSessionKey("run-1\x00architect")
	if got != "run-1" {
		t.Errorf("应归一为 RunID, got %q", got)
	}
}

// 关键不变量: 同一个 run 的实验臂键与对照键归一后**必须落到同一个会话**。
//
// 这是"中途关掉实验不造成全量重算"的全部依据 —— 若不成立, 关开关这个动作本身
// 就会让所有在途 run 的缓存作废。
func TestNormalizeSessionKey_TreatAndControlCollapse(t *testing.T) {
	aliased := normalizeSessionKey("run-7\x00tester")
	control := normalizeSessionKey("run-7")
	if aliased != control {
		t.Fatalf("实验键与对照键应归一到同一会话: %q vs %q", aliased, control)
	}
}

// \x00 开头 (RunID 为空、只有 NodeID) 是畸形输入, 原样返回而不是归一成空串 ——
// 归一成空串会让 opencodeSessionID 走 seed 兜底, 把一次真实会话降级成进程级常量。
func TestNormalizeSessionKey_LeadingSeparator(t *testing.T) {
	v := "\x00orphan"
	if got := normalizeSessionKey(v); got != v {
		t.Errorf("畸形键应原样返回, got %q", got)
	}
}

// 归一化缓存有上界: 它由外部输入 (客户端可任意构造的会话头) 驱动写入,
// 无界会是一条内存增长路径。
func TestNormalizeSessionKey_CacheIsBounded(t *testing.T) {
	for i := 0; i < sessionKeyAliasMax*3; i++ {
		normalizeSessionKey("run-" + itoaTest(i) + "\x00node")
	}
	sessionKeyAliasMu.Lock()
	n := len(sessionKeyAlias)
	sessionKeyAliasMu.Unlock()
	if n > sessionKeyAliasMax {
		t.Errorf("归一化缓存超上界: %d > %d", n, sessionKeyAliasMax)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 端到端: 带实验臂会话头的入站请求, 网关只用归一后的键。
func TestOpencodeSessionID_NormalizesArmKey(t *testing.T) {
	if got := opencodeSessionID(reqWithSession("run-9\x00coder"), ""); got != "run-9" {
		t.Errorf("应归一为 run-9, got %q", got)
	}
	// 对照组原样。
	if got := opencodeSessionID(reqWithSession("run-9"), ""); got != "run-9" {
		t.Errorf("对照组应原样透传, got %q", got)
	}
	// 无会话头 → 进程级稳定兜底 (两次调用相同, 不是每次请求都换)。
	a := opencodeSessionID(reqWithSession(""), "")
	b := opencodeSessionID(reqWithSession(""), "")
	if a != b {
		t.Errorf("缺头时的兜底会话 ID 应稳定: %q vs %q", a, b)
	}
	// seed 派生路径稳定。
	if x, y := opencodeSessionID(reqWithSession(""), "seed-x"), opencodeSessionID(reqWithSession(""), "seed-x"); x != y {
		t.Errorf("同 seed 应派生同 ID: %q vs %q", x, y)
	}
	if x, y := opencodeSessionID(reqWithSession(""), "seed-x"), opencodeSessionID(reqWithSession(""), "seed-y"); x == y {
		t.Errorf("不同 seed 应派生不同 ID, 都是 %q", x)
	}
}
