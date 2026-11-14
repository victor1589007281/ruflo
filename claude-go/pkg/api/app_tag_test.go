// app_tag_test.go — 出站 APP 标签必须活过客户端克隆。
//
// # 为什么这条测试必须走 SendMessage
//
// 引擎的每一轮都按模型/角色克隆 Client (engine.go 的 WithModel、harness 的
// ConfiguredClone)，业务应用只在**原始** client 上设了 X-App-Id。克隆构造函数
// 少复制一个字段时: 单测全绿、直连单模型也对, 只有走了角色路由的真实回合丢掉
// 标签 —— 网关只能记成 unknown, 事后无法归因。所以断言假上游**真正收到的头**。
package api

import (
	"net/http"
	"testing"
)

const testAppHeader = "X-App-Id"

func appTagClient(rt *headerRT) *Client {
	return &Client{
		BaseURL:      "https://api.example.com/v1",
		APIKey:       "k",
		Model:        "base-model",
		Client:       &http.Client{Transport: rt},
		RetryCount:   1,
		ExtraHeaders: http.Header{testAppHeader: {"moqi"}},
	}
}

// TestWithModelKeepsAppTagOnTheWire: 角色/阶段路由用的 WithModel 副本必须带标签。
func TestWithModelKeepsAppTagOnTheWire(t *testing.T) {
	rt := &headerRT{}
	c := appTagClient(rt)
	sendOnce(t, c.WithModel("role-model"))

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.headers) != 1 {
		t.Fatalf("请求数=%d, want 1", len(rt.headers))
	}
	if got := rt.headers[0].Get(testAppHeader); got != "moqi" {
		t.Fatalf("克隆体出站请求的 %s=%q, want moqi —— role/phase 路由回合的 APP 标签丢了", testAppHeader, got)
	}
}

// TestConfiguredClonesKeepAppTagOnTheWire: 换端点的克隆 (含 Full) 同样要带。
func TestConfiguredClonesKeepAppTagOnTheWire(t *testing.T) {
	for name, clone := range map[string]func(*Client) *Client{
		"ConfiguredClone": func(c *Client) *Client {
			return c.ConfiguredClone("https://api.example.com/v1", "k2", "m2", nil)
		},
		"ConfiguredCloneFull": func(c *Client) *Client {
			return c.ConfiguredCloneFull("https://api.example.com/v1", "k2", "m2", nil, "", "")
		},
		"WithModelThenConfigured": func(c *Client) *Client {
			return c.WithModel("m2").ConfiguredClone("https://api.example.com/v1", "k2", "m3", nil)
		},
	} {
		rt := &headerRT{}
		sendOnce(t, clone(appTagClient(rt)))
		rt.mu.Lock()
		got := ""
		if len(rt.headers) > 0 {
			got = rt.headers[0].Get(testAppHeader)
		}
		rt.mu.Unlock()
		if got != "moqi" {
			t.Errorf("%s: 出站 %s=%q, want moqi", name, testAppHeader, got)
		}
	}
}

// TestCloneHeadersAreDeepCopied: 克隆体与原件各持一份 map。
//
// 引擎每轮克隆一次; 共用 map 时某个克隆体后续 Set 的请求级头会泄漏进并发回合,
// 表现为"某次请求莫名带上了别的回合的会话/标签" —— 事后极难归因。
func TestCloneHeadersAreDeepCopied(t *testing.T) {
	rt := &headerRT{}
	c := appTagClient(rt)

	for name, clone := range map[string]*Client{
		"WithModel":       c.WithModel("m2"),
		"ConfiguredClone": c.ConfiguredClone("https://api.example.com/v1", "k2", "m2", nil),
	} {
		clone.ExtraHeaders.Set(testAppHeader, "mutated")
		clone.ExtraHeaders.Set("X-Only-On-Clone", "1")
		if got := c.ExtraHeaders.Get(testAppHeader); got != "moqi" {
			t.Errorf("%s: 改克隆体污染了原件: %s=%q", name, testAppHeader, got)
		}
		if got := c.ExtraHeaders.Get("X-Only-On-Clone"); got != "" {
			t.Errorf("%s: 克隆体新增的头泄漏进了原件: %q", name, got)
		}
		clone.ExtraHeaders.Set(testAppHeader, "moqi")
		clone.ExtraHeaders.Del("X-Only-On-Clone")
	}

	// 新克隆必须重新读原件的当前值, 而不是复制第一次的副本。
	c.ExtraHeaders.Set(testAppHeader, "moqi-v2")
	if got := c.WithModel("m3").ExtraHeaders.Get(testAppHeader); got != "moqi-v2" {
		t.Errorf("新克隆体读到的是旧值 %q, want moqi-v2", got)
	}
}
