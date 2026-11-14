package main

import (
	"net/http/httptest"
	"testing"
)

// TestAppLabelReflectsSelfReportedHeader 守的是"APP 归因在日志里可见"这条链:
// 自报头 → stampAppContext 落 ctx (claimed:<id>) → 日志字段。
//
// 三层各自都不难, 但少任何一层, "这个 APP 的请求到底带没带标签"就退化成不可
// 核对 —— 上游 opencode 会丢弃未知头, ctx 又只在进程内。所以这里按端到端断言,
// 而不是分别断言实现细节。
func TestAppLabelReflectsSelfReportedHeader(t *testing.T) {
	bare := httptest.NewRequest("POST", "/v1/messages", nil)
	if got := gatewayAppLabel(bare); got != "unknown" {
		t.Errorf("没带自报头时应是 unknown, 实际 %q", got)
	}

	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-App-Id", "moqi")
	r = r.WithContext(stampAppContext(r))
	if got := gatewayAppLabel(r); got != "claimed:moqi" {
		t.Errorf("自报头应落成 claimed:moqi, 实际 %q", got)
	}
}
