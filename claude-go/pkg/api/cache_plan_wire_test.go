package api

import (
	"net/http"
	"testing"
)

// TestWithCachePlan 锁住"按 run 设置计划"这个克隆入口的两个语义：
//
//  1. 不污染原对象：原 Client 的 CachePlan 必须仍为 nil。Client 被并发 run 共享，
//     原地写会让本轮断点泄漏到其它轮的请求里。
//  2. 克隆共享底层资源：http.Client / Guard / 拦截器不能因为克隆而被替换 ——
//     否则每个带计划的回合都会得到一条独立连接池，限流也会失效。
//
// nil 计划另有第三条：必须返回**同一个指针**。这是"未启用时逐字节不变"的最强
// 可执行形式 —— 不只是行为等价，连对象身份都没变，因此不存在任何字段被拷贝后
// 又悄悄分叉的可能。
func TestWithCachePlan(t *testing.T) {
	httpClient := &http.Client{}
	plan := &CachePlan{SystemIndexes: []int{2}}
	base := &Client{
		BaseURL: "https://example.invalid",
		Model:   "claude-test",
		Client:  httpClient,
		Guard:   &RateLimitGuard{},
	}

	clone := base.WithCachePlan(plan)

	if clone == base {
		t.Fatal("带计划的 WithCachePlan 必须返回副本, 不能返回原指针")
	}
	if base.CachePlan != nil {
		t.Fatalf("WithCachePlan 原地改了原 client: CachePlan=%+v", base.CachePlan)
	}
	if clone.CachePlan != plan {
		t.Fatalf("副本的 CachePlan 不是同一个计划指针: got %p want %p", clone.CachePlan, plan)
	}
	if clone.BaseURL != base.BaseURL {
		t.Fatalf("副本 BaseURL 变了: got %q want %q", clone.BaseURL, base.BaseURL)
	}
	if clone.Model != base.Model {
		t.Fatalf("副本 Model 变了: got %q want %q", clone.Model, base.Model)
	}
	if clone.Client != httpClient {
		t.Fatal("副本没有共享原 http.Client: 会导致每个回合各建一条连接池")
	}
	if clone.Guard != base.Guard {
		t.Fatal("副本没有共享原 Guard: 限流会被克隆绕过")
	}

	// nil 计划: 必须原样返回，且原对象依然没有计划。
	if got := base.WithCachePlan(nil); got != base {
		t.Fatalf("nil 计划应返回同一指针, got %p want %p", got, base)
	}
	if base.CachePlan != nil {
		t.Fatalf("nil 计划路径改动原 client: CachePlan=%+v", base.CachePlan)
	}
}
