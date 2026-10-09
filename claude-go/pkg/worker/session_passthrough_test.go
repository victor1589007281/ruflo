package worker

import (
	"context"
	"encoding/json"
	"testing"

	cgtrace "github.com/anthropic/claude-go/pkg/trace"
)

// 会话身份必须能完整穿过控制面 → worker 的进程边界。
//
// 这条链断在哪都**不报错**: 任务照跑、产出照出, 只是对端出站请求少一个
// X-Claude-Code-Session-Id, 上游 opencode 于是按兜底分片, 提示缓存恒不命中。
// 唯一的观测口径是 llm.jsonl 的 cache_read —— 等到账上看见已经晚了。
// 所以在这里钉住每一跳。
func TestSessionPassthrough_会话身份穿过任务边界(t *testing.T) {
	const sid = "if-jd-analysis-abc123def456"

	// 出发: 控制面按业务键把会话身份挂上 ctx。
	ctx := cgtrace.With(context.Background(), cgtrace.IDs{
		RunID: "run-team-1", NodeID: "jd-profile-analyst", SessionID: sid,
	})
	if got := cgtrace.From(ctx).SessionID; got != sid {
		t.Fatalf("ctx 里会话身份就没挂上: %q", got)
	}

	// 第一跳: 编解码往返。会话身份是**新增字段**, 最容易在 JSON tag 上丢。
	payload, err := EncodeStageTask(StageTask{
		RunID: "run-team-1", NodeID: "jd-profile-analyst", SessionID: sid,
		Role: "coder", UserPrompt: "x",
	})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	st, err := DecodeStageTask(payload)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if st.SessionID != sid {
		t.Fatalf("会话身份在载荷往返中丢了: 期望 %q 实际 %q", sid, st.SessionID)
	}

	// 第二跳: 载荷 → 执行体任务。
	if got := st.ToRuntimeTask().SessionID; got != sid {
		t.Fatalf("会话身份在 ToRuntimeTask 中丢了: %q", got)
	}

	// 第三跳: worker 侧用它重建 ctx —— 这是远程进程里唯一的复原点,
	// 复原不出来后面整条链都是白发。
	wctx := cgtrace.With(context.Background(), cgtrace.IDs{
		RunID: st.RunID, NodeID: st.NodeID, SessionID: st.SessionID,
	})
	ids := cgtrace.From(wctx)
	if ids.SessionID != sid {
		t.Fatalf("worker 侧 ctx 复原丢了会话身份: %q", ids.SessionID)
	}
	// RunID 必须同时还在: 会话身份是**额外**带的, 不是把 RunID 顶掉。
	// 顶掉了奖励归因就错账, 而账错得静悄悄。
	if ids.RunID != "run-team-1" {
		t.Fatalf("RunID 被会话身份挤掉了: %q", ids.RunID)
	}
}

// 缺省必须省略: 老控制面不带 session_id 时, 新 worker 解出来是空串,
// 于是退回"按 RunID 分片"的老行为 —— 升级顺序不该有硬依赖。
func TestSessionPassthrough_缺省省略且退回老行为(t *testing.T) {
	raw := json.RawMessage(`{"run_id":"r","node_id":"n","role":"coder","user_prompt":"x"}`)
	st, err := DecodeStageTask(raw)
	if err != nil {
		t.Fatalf("解老载荷失败 (老控制面 + 新 worker 的组合会整个跑不起来): %v", err)
	}
	if st.SessionID != "" {
		t.Fatalf("老载荷里没有 session_id, 解出来却是 %q", st.SessionID)
	}
	if st.ToRuntimeTask().SessionID != "" {
		t.Fatal("空会话身份不该被凭空造出来")
	}
}
