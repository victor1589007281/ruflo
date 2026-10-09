package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 关闭 (占比 0) 时会话键必须与改造前**逐字节一致**: 只认 RunID。
// 这是实验的底线 —— 关掉开关就要能回到原状, 而不是"差不多原状"。
func TestSessionShard_OffIsByteIdenticalToBefore(t *testing.T) {
	SetSessionShardPercent(0)
	defer SetSessionShardPercent(0)
	for _, node := range []string{"", "stage-a", "stage-b"} {
		got := sessionKeyForTrace(Trace{RunID: "run-1", NodeID: node})
		if got != "run-1" {
			t.Errorf("关闭实验时会话键应为 RunID, NodeID=%q 时 got %q", node, got)
		}
	}
	// 没有 RunID 时不发会话头 (与改造前一致)。
	if got := sessionKeyForTrace(Trace{NodeID: "stage-a"}); got != "" {
		t.Errorf("无 RunID 时不应发会话头, got %q", got)
	}
}

// 全实验组: 每个节点各占一个会话键, 且同 run 的不同节点键不同。
func TestSessionShard_RunNodeSplitsByStage(t *testing.T) {
	SetSessionShardPercent(100)
	defer SetSessionShardPercent(0)

	a := sessionKeyForTrace(Trace{RunID: "run-1", NodeID: "architect"})
	b := sessionKeyForTrace(Trace{RunID: "run-1", NodeID: "coder"})
	if a == b {
		t.Fatalf("不同节点应得到不同会话键, 都是 %q", a)
	}
	if a == "run-1" || b == "run-1" {
		t.Error("实验臂不应退化成对照组")
	}
	// 同一 (run, node) 稳定。
	if again := sessionKeyForTrace(Trace{RunID: "run-1", NodeID: "architect"}); again != a {
		t.Errorf("同一 (run,node) 的会话键应稳定: %q vs %q", a, again)
	}
}

// 拼接用 \x00 分隔: ("a:b","c") 与 ("a","b:c") 不能拼出同一个键 ——
// 那会让两个不相干的节点共享会话, 正好抵消实验臂想测的东西。
func TestSessionShard_JoinIsUnambiguous(t *testing.T) {
	SetSessionShardPercent(100)
	defer SetSessionShardPercent(0)
	x := sessionKeyForTrace(Trace{RunID: "a:b", NodeID: "c"})
	y := sessionKeyForTrace(Trace{RunID: "a", NodeID: "b:c"})
	if x == y {
		t.Fatalf("不同 (run,node) 对拼出了相同会话键 %q", x)
	}
}

// 分桶按 RunID 哈希, **不是随机数**: 一次 run 横跨几十上百个请求, 每个请求各自
// 随机会让同一次 run 中途换会话 —— 既毁缓存, 也让组别在 run 内部混杂。
func TestSessionShard_ArmIsStableWithinRun(t *testing.T) {
	SetSessionShardPercent(50)
	defer SetSessionShardPercent(0)
	for i := 0; i < 200; i++ {
		run := fmt.Sprintf("run-%d", i)
		first := armForRun(run)
		for j := 0; j < 5; j++ {
			if got := armForRun(run); got != first {
				t.Fatalf("%s 的臂不稳定: %q vs %q", run, first, got)
			}
		}
	}
}

// 分桶比例落在设定值附近 (允许统计涨落, 但不允许系统性偏移)。
func TestSessionShard_BucketDistribution(t *testing.T) {
	defer SetSessionShardPercent(0)
	for _, pct := range []int{25, 50, 75} {
		SetSessionShardPercent(pct)
		treat := 0
		const n = 20000
		for i := 0; i < n; i++ {
			if armForRun(fmt.Sprintf("run-%d", i)) == ArmRunNode {
				treat++
			}
		}
		got := float64(treat) * 100 / n
		if got < float64(pct)-3 || got > float64(pct)+3 {
			t.Errorf("占比 %d%% 时实验组实测 %.1f%%, 偏离过大", pct, got)
		}
	}
}

// 极端值不走分桶: 0 = 全对照, 100 = 全实验组 (省掉哈希)。
func TestSessionShard_Extremes(t *testing.T) {
	defer SetSessionShardPercent(0)
	SetSessionShardPercent(0)
	if armForRun("") != ArmRun || armForRun("r") != ArmRun {
		t.Error("占比 0 时一切归对照组")
	}
	SetSessionShardPercent(100)
	if armForRun("") != ArmRunNode || armForRun("r") != ArmRunNode {
		t.Error("占比 100 时一切归实验组")
	}
	// 超范围夹到边界。
	SetSessionShardPercent(-5)
	if SessionShardPercent() != 0 {
		t.Error("负值应夹到 0")
	}
	SetSessionShardPercent(999)
	if SessionShardPercent() != 100 {
		t.Error("超 100 应夹到 100")
	}
}

// 环境变量解析: 认不出的值按**关闭**处理, 不能悄悄打开实验臂。
func TestInitSessionShardFromEnv(t *testing.T) {
	defer SetSessionShardPercent(0)
	cases := []struct {
		env  string
		want int
	}{
		{"", 0}, {"off", 0}, {"OFF", 0}, {"false", 0},
		{"on", 100}, {"all", 100},
		{"0", 0}, {"50", 50}, {"100", 100}, {"1", 1},
		{"yes-please", 0}, {"abc", 0}, {"-3", 0},
	}
	for _, c := range cases {
		t.Setenv("CLAUDE_GO_SESSION_SHARD", c.env)
		if got := InitSessionShardFromEnv(); got != c.want {
			t.Errorf("CLAUDE_GO_SESSION_SHARD=%q → %d, 期望 %d", c.env, got, c.want)
		}
	}
}

// setTraceHeaders 端到端: 对照组一字节不变, 实验臂带上臂标识头。
func TestSetTraceHeaders_SessionShard(t *testing.T) {
	defer SetSessionShardPercent(0)

	SetSessionShardPercent(0)
	req := httptest.NewRequest(http.MethodPost, "http://x/messages", nil)
	setTraceHeaders(req, WithTrace(req.Context(), Trace{RunID: "run-1", NodeID: "n1"}))
	if got := req.Header.Get(SessionHeader); got != "run-1" {
		t.Errorf("对照组会话头应为 RunID, got %q", got)
	}

	SetSessionShardPercent(100)
	req2 := httptest.NewRequest(http.MethodPost, "http://x/messages", nil)
	setTraceHeaders(req2, WithTrace(req2.Context(), Trace{RunID: "run-1", NodeID: "n1"}))
	if got := req2.Header.Get(SessionHeader); got == "run-1" || got == "" {
		t.Errorf("实验臂会话头应含节点, got %q", got)
	}
	if got := req2.Header.Get(SessionArmHeader); got != string(ArmRunNode) {
		t.Errorf("臂标识头应为 %q, got %q", ArmRunNode, got)
	}
}
