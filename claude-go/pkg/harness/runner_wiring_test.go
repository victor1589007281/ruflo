// runner_wiring_test.go — 六项"实现存在但没有生产调用方"的能力的端到端接线验收。
//
// 这些用例的共同点：断言**出站请求字节**或**trace/账本内容**，而不是"函数被调用过"。
// 只验后者的测试在装配断链时依然全绿 —— 那正是本仓反复出现的"建成未通电"。
package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// wireUpstream 是可控假上游：failModel 非空时该模型一律 529，其余模型正常收口。
type wireUpstream struct {
	mu        sync.Mutex
	bodies    []string
	models    []string
	failModel string
	text      string
	// onRequest 在每次请求**进入处理**时回调（用于断言请求发出时状态已经写好）。
	onRequest func()
}

func (u *wireUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)

		u.mu.Lock()
		u.bodies = append(u.bodies, string(raw))
		u.models = append(u.models, body.Model)
		fail := u.failModel != "" && body.Model == u.failModel
		text := u.text
		cb := u.onRequest
		u.mu.Unlock()
		if cb != nil {
			cb()
		}

		if fail {
			w.WriteHeader(529) // Anthropic 过载码，不在 net/http 常量表里
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
			return
		}
		if text == "" {
			text = "好的"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(runnerSSEText(text)))
	})
}

func (u *wireUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

func (u *wireUpstream) bodiesSnapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, len(u.bodies))
	copy(out, u.bodies)
	return out
}

func (u *wireUpstream) modelsSnapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, len(u.models))
	copy(out, u.models)
	return out
}

// newWiredRunner 走生产构造路径（NewQueryEngine），并允许测试改写 client/prompt。
func newWiredRunner(t *testing.T, up *wireUpstream, sink TurnSink,
	mutate func(*api.Client, *prompt.Manager), opts ...func(*RunnerOptions)) *Runner {
	t.Helper()
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{
		BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client(),
		RetryCount: 1,
		// 重试基数压到毫秒级：本文件验的是接线，不是退避曲线。
		RetryBase: time.Millisecond, RetryMax: 5 * time.Millisecond,
	}
	promptMgr := prompt.NewManager(t.TempDir())
	promptMgr.OverridePrompt = "你是执行者"
	if mutate != nil {
		mutate(client, promptMgr)
	}

	eng := engine.NewQueryEngine(&engine.Config{Model: "m", MaxTurns: 8},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, promptMgr)

	ro := RunnerOptions{Engine: eng, Sink: sink}
	for _, f := range opts {
		f(&ro)
	}
	return NewRunner(ro)
}

// ---------------------------------------------------------------------------
// R-H5 / R-H11：附件必须真的出现在请求体里
// ---------------------------------------------------------------------------

func TestRunTurn_AttachmentsReachRequest(t *testing.T) {
	store, err := NewFSBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 真实 PNG 头：部分上游按魔数判定媒体类型，假字节会被拒。
	png := []byte("\x89PNG\r\n\x1a\nfakepayload")
	hash, err := store.Put(png)
	if err != nil {
		t.Fatal(err)
	}
	ref := AttachmentRef{ID: "att-1", Kind: AttachmentImage, MIME: "image/png", BlobHash: hash, Name: "检查报告.png"}

	up := &wireUpstream{text: "看到了图片"}
	sink := &recordingSink{}
	// 模型名 "m" 不在内置能力表里，未知模型按保守口径判为不支持图片。
	// 这条测试验的是附件接线，不是能力表，所以显式声明本部署的模型支持图片。
	r := newWiredRunner(t, up, sink, nil, func(o *RunnerOptions) {
		o.Blobs = store
		o.Modality = func(_, _ string) bool { return true }
	})

	res, err := r.RunTurn(context.Background(), TurnRequest{
		SessionID:   "s1",
		TurnID:      "t1",
		Input:       []types.ContentBlock{{Type: types.ContentBlockText, Text: "看这张图"}},
		Attachments: []AttachmentRef{ref},
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}
	if up.count() == 0 {
		t.Fatal("没有发出模型请求")
	}

	body := up.bodiesSnapshot()[0]
	wantData := base64.StdEncoding.EncodeToString(png)
	if !strings.Contains(body, `"image"`) {
		t.Fatalf("请求体里没有 image 块: %s", body)
	}
	if !strings.Contains(body, wantData) {
		t.Fatalf("请求体里没有附件 base64 数据（附件被静默丢掉了）: %s", body)
	}
	if !strings.Contains(body, "看这张图") {
		t.Fatalf("正文丢失: %s", body)
	}
	// 落库的是引用而不是展开后的字节（R-H5：转录里不得内联完整 base64）。
	if len(sink.users) != 1 || len(sink.users[0].Attachments) != 1 {
		t.Fatalf("sink 记录的用户消息应带 1 条附件引用: %+v", sink.users)
	}
	if strings.Contains(string(mustJSON(t, sink.users[0])), wantData) {
		t.Fatal("用户消息记录里内联了附件字节 —— 转录不应包含完整 base64")
	}
	if !res.Persisted {
		t.Fatalf("正常轮应报 Persisted=true")
	}
}

// TestRunTurn_AttachmentFailureSkipsModel 钉住"解析失败整轮失败、绝不静默丢附件"。
func TestRunTurn_AttachmentFailureSkipsModel(t *testing.T) {
	up := &wireUpstream{}
	// Blobs 未装配。仍然显式声明支持图片：这条测试要钉的是"附件解析失败"，
	// 不让模态判定抢先返回（两者都在发请求之前，但错误分类必须可区分）。
	r := newWiredRunner(t, up, nil, nil, func(o *RunnerOptions) {
		o.Modality = func(_, _ string) bool { return true }
	})

	_, err := r.RunTurn(context.Background(), TurnRequest{
		SessionID:   "s1",
		TurnID:      "t1",
		Input:       []types.ContentBlock{{Type: types.ContentBlockText, Text: "看图"}},
		Attachments: []AttachmentRef{{ID: "a", Kind: AttachmentImage, MIME: "image/png", BlobHash: "nope"}},
	}, nil)
	if err == nil {
		t.Fatal("附件解析失败应返回错误")
	}
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindAttachment {
		t.Fatalf("错误分类应为 attachment, 实际 %v", err)
	}
	if up.count() != 0 {
		t.Fatalf("附件解析失败后仍调了 %d 次模型 —— 模型会基于缺材料给出确定回答", up.count())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// R-H2：缓存计划必须落到 system 数组的实际下标上
// ---------------------------------------------------------------------------

// TestRunTurn_CachePlanReachesSystemBreakpoint 用**两个** system 段区分计划与内置：
// 内置启发式固定打 block 0，计划打稳定段末尾（block 1）。只断言"body 里有
// cache_control"是分不出两者的 —— 关闭计划后测试照样全绿。
func TestRunTurn_CachePlanReachesSystemBreakpoint(t *testing.T) {
	up := &wireUpstream{text: "ok"}
	tr := &recordingTrace{}
	layout := NewLayoutBuilder(LayoutOptions{})
	r := newWiredRunner(t, up, nil, func(c *api.Client, pm *prompt.Manager) {
		c.PromptCacheMode = "on" // auto 在非 Anthropic 域下会整体关闭 cache_control
		pm.OverridePrompt = ""
		pm.CoordinatorPrompt = "协调者提示词"
		pm.AppendPrompt = "附加约束"
	}, func(o *RunnerOptions) {
		o.Layout = layout
		o.Trace = tr
	})

	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "你好"), nil); err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}

	bodies := up.bodiesSnapshot()
	if len(bodies) != 1 {
		t.Fatalf("请求数=%d，want 1", len(bodies))
	}
	var wire struct {
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &wire); err != nil {
		t.Fatalf("解析请求体: %v\n%s", err, bodies[0])
	}
	if len(wire.System) < 2 {
		t.Fatalf("system 段数=%d，want >=2（否则分不出计划与内置）: %s", len(wire.System), bodies[0])
	}
	if _, ok := wire.System[1]["cache_control"]; !ok {
		t.Fatalf("稳定段末尾（system[1]）没有 cache_control —— 计划没到达请求: %s", bodies[0])
	}
	if _, ok := wire.System[0]["cache_control"]; ok {
		t.Fatalf("system[0] 被打上了断点 —— 内置启发式仍在生效，计划被绕过: %s", bodies[0])
	}

	traces := tr.all()
	if len(traces) != 1 || traces[0].PromptLayout == nil {
		t.Fatalf("trace 缺少布局: %+v", traces)
	}
	if got := traces[0].PromptLayout.PlannedSystemBreakpoints; len(got) != 1 || got[0] != 1 {
		t.Fatalf("trace 计划断点=%v，want [1]（与请求同一份）", got)
	}
}

// ---------------------------------------------------------------------------
// R-H1：token 预算裁剪 + 被裁 ID 进 trace
// ---------------------------------------------------------------------------

type fixedProvider struct {
	patch ContextPatch
	err   error
}

func (p fixedProvider) ProvideTurnContext(_ context.Context, _ TurnContext) (ContextPatch, error) {
	return p.patch, p.err
}

func TestRunTurn_ContextBudgetPruningVisibleInTrace(t *testing.T) {
	up := &wireUpstream{text: "答复"}
	tr := &recordingTrace{}
	provider := fixedProvider{patch: ContextPatch{
		Blocks: []ContextBlock{
			{ID: "low", Text: "低优先级证据-LOW", Priority: 1, Tokens: 100},
			{ID: "high", Text: "高优先级证据-HIGH", Priority: 9, Tokens: 100},
		},
		Trace: "retrieval:2hits",
	}}
	r := newWiredRunner(t, up, nil, nil, func(o *RunnerOptions) {
		o.ContextProvider = provider
		o.ContextTokenBudget = 100
		o.Trace = tr
	})

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "问题"), nil)
	if err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}
	body := up.bodiesSnapshot()[0]
	if !strings.Contains(body, "HIGH") {
		t.Fatalf("高优先级块没进请求: %s", body)
	}
	if strings.Contains(body, "LOW") {
		t.Fatalf("预算超限的低优先级块仍进了请求（裁剪没生效）: %s", body)
	}
	if !containsString(res.Degraded, "context_budget_pruned") {
		t.Errorf("TurnResult.Degraded 未标记预算裁剪: %v", res.Degraded)
	}

	traces := tr.all()
	if len(traces) != 1 {
		t.Fatalf("trace 数=%d，want 1", len(traces))
	}
	got := traces[0]
	if len(got.ContextBlockIDs) != 1 || got.ContextBlockIDs[0] != "high" {
		t.Errorf("ContextBlockIDs=%v，want [high]", got.ContextBlockIDs)
	}
	if len(got.ContextDroppedIDs) != 1 || got.ContextDroppedIDs[0] != "low" {
		t.Errorf("ContextDroppedIDs=%v，want [low]（丢了什么必须可查）", got.ContextDroppedIDs)
	}
	if got.ContextTokens != 100 || !got.ContextTokensKnown {
		t.Errorf("ContextTokens=%d known=%v，want 100/true", got.ContextTokens, got.ContextTokensKnown)
	}
	if got.ContextTrace != "retrieval:2hits" {
		t.Errorf("ContextTrace=%q，未透传 provider 的检索 trace", got.ContextTrace)
	}
}

// TestRunTurn_ContextProviderFailureIsVisible 钉住"检索失败降级不中断，但必须可见"。
func TestRunTurn_ContextProviderFailureIsVisible(t *testing.T) {
	up := &wireUpstream{text: "仍然回答"}
	tr := &recordingTrace{}
	r := newWiredRunner(t, up, nil, nil, func(o *RunnerOptions) {
		o.ContextProvider = fixedProvider{err: errors.New("检索服务超时")}
		o.Trace = tr
	})

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "问题"), nil)
	if err != nil {
		t.Fatalf("检索失败不该中断本轮: %v", err)
	}
	if res.Text == "" {
		t.Fatal("检索失败后模型仍应回答")
	}
	if !containsString(res.Degraded, "context_degraded") {
		t.Fatalf("TurnResult.Degraded 未标记 context_degraded: %v", res.Degraded)
	}
}

// ---------------------------------------------------------------------------
// R-H17：账本 + 回退链
// ---------------------------------------------------------------------------

func TestRunTurn_LedgerAndFallbackChain(t *testing.T) {
	up := &wireUpstream{failModel: "m", text: "备用模型答复"}
	tr := &recordingTrace{}

	var pricingMu sync.Mutex
	var pricedModels []string
	ledger := NewCostLedger(func(model string, u types.Usage) float64 {
		pricingMu.Lock()
		pricedModels = append(pricedModels, model)
		pricingMu.Unlock()
		return float64(u.InputTokens+u.OutputTokens) * 0.5
	})

	r := newWiredRunner(t, up, nil, func(c *api.Client, _ *prompt.Manager) {
		c.FallbackModels = []string{"backup"}
	}, func(o *RunnerOptions) {
		o.Ledger = ledger
		o.Trace = tr
	})

	res, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "你好"), nil)
	if err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}

	models := up.modelsSnapshot()
	if len(models) < 2 || models[0] != "m" || models[len(models)-1] != "backup" {
		t.Fatalf("请求模型序列=%v，want 主模型 m 之后切到 backup", models)
	}

	traces := tr.all()
	if len(traces) != 1 {
		t.Fatalf("trace 数=%d，want 1", len(traces))
	}
	chain := traces[0].FallbackChain
	if len(chain) != 1 {
		t.Fatalf("FallbackChain=%+v，want 1 条（静默换模型无法与合法回退区分）", chain)
	}
	if chain[0].FromModel != "m" || chain[0].ToModel != "backup" || chain[0].Reason != "upstream_529" {
		t.Fatalf("回退事件=%+v，want m→backup(upstream_529)", chain[0])
	}

	snap := ledger.Snapshot()
	if len(snap.Fallbacks) != 1 || snap.Fallbacks[0].ToModel != "backup" {
		t.Fatalf("账本回退事件=%+v，want 1 条 backup", snap.Fallbacks)
	}
	if !res.UsageKnown {
		t.Fatal("上游回了 usage，UsageKnown 应为 true")
	}
	if !res.CostKnown || res.Cost <= 0 {
		t.Fatalf("账本已装配且价目可用，Cost/CostKnown 应为正/true: cost=%v known=%v", res.Cost, res.CostKnown)
	}
	pricingMu.Lock()
	defer pricingMu.Unlock()
	// 计价必须按**实际作答**的模型：按主模型单价计费会把回退成本算错。
	if len(pricedModels) == 0 || pricedModels[len(pricedModels)-1] != "backup" {
		t.Fatalf("计价模型=%v，want 最后一次为 backup", pricedModels)
	}
}

// ---------------------------------------------------------------------------
// R-H4：同 TurnID 重放不重跑模型、不重复落库
// ---------------------------------------------------------------------------

func TestRunTurn_ReplaySameTurnIDDoesNotCallModel(t *testing.T) {
	up := &wireUpstream{text: "第一次回答"}
	sink := &recordingSink{}
	r := newWiredRunner(t, up, sink, nil)

	first, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "你好"), nil)
	if err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	if up.count() != 1 {
		t.Fatalf("首轮请求数=%d，want 1", up.count())
	}

	second, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "你好"), nil)
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if up.count() != 1 {
		t.Fatalf("重放又调了模型（请求数=%d）—— 同一 TurnID 必须返回同一份回答", up.count())
	}
	if second.Text != first.Text {
		t.Fatalf("重放文本=%q，首次=%q", second.Text, first.Text)
	}
	if !second.Persisted {
		t.Fatal("重放结果应标记已持久化")
	}
	if !containsString(second.Degraded, "turn_replayed") {
		t.Errorf("Degraded 未标记 turn_replayed: %v", second.Degraded)
	}
	if len(sink.assists) != 1 {
		t.Fatalf("助手消息落库次数=%d，want 1（重放不得重复写）", len(sink.assists))
	}
	if len(sink.users) != 1 {
		t.Fatalf("用户消息落库次数=%d，want 1", len(sink.users))
	}
}

// ---------------------------------------------------------------------------
// Q-H11：模型调用之前必须留下 running 状态
// ---------------------------------------------------------------------------

func TestRunTurn_WritesRunningStateBeforeModel(t *testing.T) {
	up := &wireUpstream{text: "ok"}
	sink := &recordingSink{}

	// 在**上游收到请求的那一刻**检查 running 标记是否已经写入。后置断言
	// （跑完再看 states）无法区分"模型调用前写入"与"收尾时才补写"—— 后者
	// 同样能让 RecoverInFlight 在正常路径上什么也看不到。
	var (
		seenMu      sync.Mutex
		sawRunning  bool
		requestSeen int
	)
	up.onRequest = func() {
		sink.mu.Lock()
		for _, st := range sink.states {
			if st.State == stateRunning {
				seenMu.Lock()
				sawRunning = true
				seenMu.Unlock()
			}
		}
		sink.mu.Unlock()
		seenMu.Lock()
		requestSeen++
		seenMu.Unlock()
	}

	r := newWiredRunner(t, up, sink, nil)
	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}

	seenMu.Lock()
	defer seenMu.Unlock()
	if requestSeen == 0 {
		t.Fatal("上游没收到请求")
	}
	if !sawRunning {
		t.Fatal("模型请求已发出时还没有 running 状态 —— RecoverInFlight 永远扫不到在途 turn")
	}
	if len(sink.states) < 2 {
		t.Fatalf("MarkTurn 次数=%d，want >=2（running + 终态）", len(sink.states))
	}
	if last := sink.states[len(sink.states)-1]; last.State != stateCompleted {
		t.Fatalf("最后一条状态=%q，want %q", last.State, stateCompleted)
	}
}
