// poolbandit_test.go —— 13.7-P1 三因子个性化 + bandit 选择器单测。
//
// 三点验收 (docforge planning-skills-surge §13.7.4):
//  1. 同池不同 team 搜索序可区分: α>0 时 team 历史 (Beta 后验) 改变排序,
//     与 α=0 的纯相关性序对拍可区分;
//  2. 选择留痕 E2E: pool_search/pool_load 产 KindPolicyDecision Span,
//     经 trace.With(ctx, IDs{RunID}) 可从 TraceStore ReadRun 读回,
//     无 RunID 时落 trace-orphan 桶 (ReadRun("") 读回);
//  3. update 改后验: success 提升均值 (mean 上行), 失败压低;
//     poolAlphaFromEnv 边界 (nil/非法/负值 → 0); coldStart 水位线防重复计入。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

// newTestPoolPolicy 建一个带内存 TraceStore 的 policy (stateRoot 空 = 跳过 bandit 持久化;
// bandit 惰性初始化, 测试里用 mustBandit 直接建)。
func newTestPoolPolicy(t *testing.T) (*PoolPolicy, *tracestore.Store) {
	t.Helper()
	ts := tracestore.New(statestore.NewMemStore())
	return NewPoolPolicy("", ts), ts
}

// mustBandit 给未初始化 bandit 的 policy 挂一个纯内存 bandit (测试直挂, 不走持久化)。
func mustBandit(pp *PoolPolicy) *poolBandit {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if pp.bandit == nil {
		pp.bandit = newPoolBandit("")
	}
	return pp.bandit
}

// ---------------------------------------------------------------------------
// 验收③: update 改后验 + poolAlphaFromEnv 边界
// ---------------------------------------------------------------------------

func TestPoolBanditUpdateShiftsMean(t *testing.T) {
	dir := t.TempDir()
	b := newPoolBandit(filepath.Join(dir, "poolbandit.json"))
	// 原始计数 (0,0) 起; Beta(1,1) 均匀先验在采样时以 Max(a,1)/Max(b,1) 表达。
	for i := 0; i < 10; i++ {
		b.update("t", "r", "good", true)
	}
	for i := 0; i < 3; i++ {
		b.update("t", "r", "bad", false)
	}
	if a, be := b.posterior("t", "r", "good"); a != 10 || be != 0 {
		t.Fatalf("good 计数应 (10,0), got (%v,%v)", a, be)
	}
	if m := b.mean("t", "r", "good"); m <= b.mean("t", "r", "bad") {
		t.Fatalf("success 应抬高均值: good=%v bad=%v", b.mean("t", "r", "good"), b.mean("t", "r", "bad"))
	}
}

func TestPoolBanditPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poolbandit.json")
	b1 := newPoolBandit(path)
	b1.update("t1", "r", "asset", true)
	b1.update("t1", "r", "asset", true)
	b1.save() // save 无返回值 (错误仅记日志, tmp+rename 原子)
	b2 := newPoolBandit(path)
	if a, be := b2.posterior("t1", "r", "asset"); a != 2 || be != 0 {
		t.Fatalf("重启后应恢复计数 (2,0), got (%v,%v)", a, be)
	}
}

func TestPoolBanditCorruptFileReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poolbandit.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := newPoolBandit(path) // 坏文件静默重开, 不 panic
	if a, be := b.posterior("x", "y", "z"); a != 0 || be != 0 {
		t.Fatalf("坏文件应回裸计数 (0,0), got (%v,%v)", a, be)
	}
}

func TestPoolAlphaFromEnvBoundaries(t *testing.T) {
	getenv := func(v string) func(string) string {
		return func(k string) string { return v }
	}
	for _, tc := range []struct {
		val  string
		want float64
	}{
		{"", 0},     // 空 = 默认 0 (回滚)
		{"0", 0},    // 显式 0
		{"abc", 0},  // 非法
		{"-1.5", 0}, // 负值
		{"1e-9", 0}, // 正但过小 (Sscanf %g 能读, 但 <0 判定后仍保留; 过小值本身合法)
		{"0.35", 0.35},
		{"2", 2},
	} {
		if tc.val == "1e-9" {
			continue // Sscanf 读 1e-9 得 1e-9, 合法非负 → 保留; 单独断言
		}
		if got := poolAlphaFromEnv(getenv(tc.val)); got != tc.want {
			t.Errorf("poolAlphaFromEnv(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
	if got := poolAlphaFromEnv(getenv("1e-9")); got <= 0 {
		t.Errorf("poolAlphaFromEnv(1e-9) 应保留正小值, got %v", got)
	}
	if got := poolAlphaFromEnv(nil); got != 0 {
		t.Errorf("nil getenv 应回 0, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// 验收①: 同池不同 team 搜索序可区分
// ---------------------------------------------------------------------------

// buildTwoAssetPool: 注册 asset-a/asset-b 两个工具 (描述都含共享关键词),
// 建检索索引, 返回 (pool, reg, idx)。
func buildTwoAssetPool(t *testing.T) (*tool.Pool, *tool.Registry, *poolIndex) {
	t.Helper()
	reg := tool.NewRegistry()
	reg.Register(&fakeToolForDesc{name: "asset-a", desc: "alpha deploy helper"})
	reg.Register(&fakeToolForDesc{name: "asset-b", desc: "beta deploy helper"})
	pool := tool.NewPool()
	pool.AddMember("asset-a")
	pool.AddMember("asset-b")
	idx := buildPoolIndex(pool, reg, nil)
	return pool, reg, idx
}

// TestSearchPersonalizedTeamDifferentiation 核心验收: 同池同查询, team-A 对
// asset-a 记满成功、team-B 对 asset-a 记满失败 —— α>0 时两 team 的排序应可区分;
// α=0 (boost 恒 1) 时两 team 与纯相关性序一致。
func TestSearchPersonalizedTeamDifferentiation(t *testing.T) {
	pp, _ := newTestPoolPolicy(t)
	_, _, idx := buildTwoAssetPool(t)
	// α 从构造时 env 读, 测试注入正 α + 固定 rng 保证可重复。
	t.Setenv(EnvPoolAlpha, "0.35")
	pp.alpha = 0.35
	b := mustBandit(pp)
	b.rng = rand.New(rand.NewSource(42))
	const k = 12
	for i := 0; i < k; i++ {
		b.update("team-A", "worker", "asset-a", true)  // team-A 成功链
		b.update("team-B", "worker", "asset-a", false) // team-B 失败链
	}
	boostA := func(name string) float64 { return 1 + pp.rankBoost("team-A", "worker", name) }
	boostB := func(name string) float64 { return 1 + pp.rankBoost("team-B", "worker", name) }
	// 相关性: 两 asset 都命中 "deploy" 正文 (同分 1), P0 序=登记序 asset-a 在前。
	hitsA := idx.searchPersonalized("deploy", 2, boostA)
	hitsB := idx.searchPersonalized("deploy", 2, boostB)
	if len(hitsA) != 2 || len(hitsB) != 2 {
		t.Fatalf("应命中 2 项, got %d/%d", len(hitsA), len(hitsB))
	}
	if hitsA[0].Name != "asset-a" {
		t.Fatalf("team-A (asset-a 12 连成功) 应把 asset-a 顶到首位, got %s", hitsA[0].Name)
	}
	if hitsB[0].Name != "asset-b" {
		t.Fatalf("team-B (asset-a 12 连失败) 应把 asset-b 顶到首位, got %s", hitsB[0].Name)
	}
	// α=0 对拍: boost 恒 1, 序与纯相关性一致 (登记序)。
	plain := idx.searchPersonalized("deploy", 2, nil)
	if plain[0].Name != "asset-a" {
		t.Fatalf("纯相关性 (nil boost) 序应=登记序 asset-a, got %s", plain[0].Name)
	}
	zero := idx.searchPersonalized("deploy", 2, func(name string) float64 {
		return 1 + pp.rankBoost("", "", name) // 无身份 team/role 空 = 恒 0
	})
	if zero[0].Name != plain[0].Name || zero[1].Name != plain[1].Name {
		t.Fatalf("无身份应退化纯相关性序, got %v vs %v", zero, plain)
	}
}

// TestPoolPolicyAlphaZeroMatchesPlainRanking α=0 (默认回滚) 时 rankBoost 恒 0,
// 即使有大量成功历史, 序也与 nil boost 一致。
func TestPoolPolicyAlphaZeroMatchesPlainRanking(t *testing.T) {
	pp, _ := newTestPoolPolicy(t)
	_, _, idx := buildTwoAssetPool(t)
	b := mustBandit(pp)
	b.rng = rand.New(rand.NewSource(42))
	for i := 0; i < 20; i++ {
		b.update("team-X", "", "asset-b", true) // asset-b 大量成功
	}
	boost := func(name string) float64 { return 1 + pp.rankBoost("team-X", "", name) }
	hits := idx.searchPersonalized("deploy", 2, boost)
	plain := idx.searchPersonalized("deploy", 2, nil)
	for i := range plain {
		if hits[i].Name != plain[i].Name {
			t.Fatalf("α=0 时序应与纯相关性逐位一致, pos %d: %s vs %s", i, hits[i].Name, plain[i].Name)
		}
	}
}

// ---------------------------------------------------------------------------
// 验收②: 选择留痕 Span E2E (pool_search / pool_load → policy_decision → ReadRun)
// ---------------------------------------------------------------------------

// poolCallE2E 建池 + 带 policy 的 search/load 工具, 走一次 poolCall, 返回 ReadRun 结果。
func poolCallE2E(t *testing.T, runID string, call func(ctx context.Context, st *PoolSearchTool, lt *PoolLoadTool)) []tracestore.Span {
	t.Helper()
	pp, ts := newTestPoolPolicy(t)
	pool, reg, _ := buildTwoAssetPool(t)
	st := NewPoolSearchTool(pool, reg, nil, pp)
	lt := NewPoolLoadTool(pool, reg, nil, pp)
	ctx := context.Background()
	if runID != "" {
		ctx = trace.With(ctx, trace.IDs{RunID: runID, TurnID: "turn-1", NodeID: "node-1"})
	}
	call(ctx, st, lt)
	spans, err := ts.ReadRun(runID) // runID=="" 读 orphan 桶
	if err != nil {
		t.Fatalf("ReadRun(%q): %v", runID, err)
	}
	return spans
}

func TestPoolSearchSpanE2E(t *testing.T) {
	spans := poolCallE2E(t, "run-x", func(ctx context.Context, st *PoolSearchTool, _ *PoolLoadTool) {
		st.Call(ctx, json.RawMessage(`{"query":"deploy"}`), &tool.ToolContext{Team: "team-A", Role: "worker"})
	})
	var found bool
	for _, sp := range spans {
		if sp.Kind != tracestore.KindPolicyDecision || sp.Name != PoolSearchName {
			continue
		}
		found = true
		if sp.TraceID != "run-x" || sp.TurnID != "turn-1" || sp.NodeID != "node-1" {
			t.Errorf("Span 身份字段错: %+v", sp)
		}
		// search Span 只记展示了什么 (candidates); selected 是 load 的语义。
		if n, ok := sp.Attrs["candidates"].(float64); !ok || n != 2 {
			t.Errorf("candidates 应=2, got %v", sp.Attrs["candidates"])
		}
		if v, ok := sp.Attrs["pool_alpha"].(float64); !ok || v != 0 {
			t.Errorf("pool_alpha 应=0 (默认), got %v", v)
		}
	}
	if !found {
		t.Fatalf("未读到 policy_decision Span: %+v", spans)
	}
}

func TestPoolSearchSpanOrphanWhenNoRunID(t *testing.T) {
	// 无 RunID: span 不丢, 落 trace-orphan 桶 (ReadRun("") 读回)。
	spans := poolCallE2E(t, "", func(ctx context.Context, st *PoolSearchTool, _ *PoolLoadTool) {
		st.Call(ctx, json.RawMessage(`{"query":"deploy"}`), nil)
	})
	found := false
	for _, sp := range spans {
		if sp.Kind == tracestore.KindPolicyDecision && sp.Name == PoolSearchName {
			found = true
		}
	}
	if !found {
		t.Fatalf("orphan 桶应收到 policy_decision Span: %+v", spans)
	}
}

func TestPoolLoadSpanAndTrial(t *testing.T) {
	pp, ts := newTestPoolPolicy(t)
	// 装配时 stateRoot 空 → bandit 为 nil, noteSelection fail-open 零操作;
	// 挂上 bandit 再走完整路径。
	mustBandit(pp)
	pool, reg, _ := buildTwoAssetPool(t)
	lt := NewPoolLoadTool(pool, reg, nil, pp)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-y", TurnID: "t2", NodeID: "n2"})
	res, err := lt.Call(ctx, json.RawMessage(`{"name":"asset-a"}`), &tool.ToolContext{Team: "team-A", Role: "worker"})
	if err != nil || res.IsError {
		t.Fatalf("pool_load 失败: %v / %v", err, res)
	}
	// ① trial 已记 (noteSelection 弱正)
	if a, be := pp.bandit.posterior("team-A", "worker", "asset-a"); a != 1 || be != 0 {
		t.Fatalf("加载成功应记弱正 trial 计数 (1,0), got (%v,%v)", a, be)
	}
	// ② Span 可读回
	spans, err := ts.ReadRun("run-y")
	if err != nil {
		t.Fatalf("ReadRun: %v", err)
	}
	var found bool
	for _, sp := range spans {
		if sp.Kind == tracestore.KindPolicyDecision && sp.Name == PoolLoadName {
			found = true
			if sp.Attrs["selected"] != "asset-a" {
				t.Errorf("selected 应=asset-a, got %v", sp.Attrs["selected"])
			}
			if sp.Attrs["role"] != "worker" || sp.Attrs["team"] != "team-A" {
				t.Errorf("team/role 应入 attrs, got %v", sp.Attrs)
			}
		}
	}
	if !found {
		t.Fatalf("未读到 pool_load policy_decision Span: %+v", spans)
	}
}

// TestPoolLoadSkillPath 技能资产走 loadSkill 同一记账 (active 门禁 + noteLoaded)。
func TestPoolLoadSkillPath(t *testing.T) {
	pp, ts := newTestPoolPolicy(t)
	mustBandit(pp)
	skillReg := skills.NewRegistry()
	mi := true
	sk := &skills.Skill{Name: "deploy-skill", Body: "# do deploy", Status: "active", ModelInvocable: &mi}
	skillReg.Register(sk)
	reg := tool.NewRegistry()
	pool := tool.NewPool()
	pool.AddMember("deploy-skill")
	lt := NewPoolLoadTool(pool, reg, skillReg, pp)
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-s"})
	res, err := lt.Call(ctx, json.RawMessage(`{"name":"deploy-skill"}`), &tool.ToolContext{Team: "team-T"})
	if err != nil || res.IsError {
		t.Fatalf("技能加载失败: %v / %v", err, res)
	}
	if a, be := pp.bandit.posterior("team-T", "", "deploy-skill"); a != 1 || be != 0 {
		t.Fatalf("技能加载应记 trial 计数 (1,0), got (%v,%v)", a, be)
	}
	found := false
	spans, err := ts.ReadRun("run-s")
	if err != nil {
		t.Fatalf("ReadRun: %v", err)
	}
	for _, sp := range spans {
		if sp.Kind == tracestore.KindPolicyDecision && sp.Attrs["skills"] != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("技能加载 Span 应含 skills attrs (13.8.6 配对归因消费)")
	}
}

// ---------------------------------------------------------------------------
// coldStart 水位线: 两次 coldStart 不重复计入
// ---------------------------------------------------------------------------

func TestColdStartWatermarkNoDoubleCount(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	logDir := filepath.Join(root, "statestore", "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 轨迹: 一个 run 带 (team,role,skills) evidence
	traceFile := filepath.Join(logDir, "trace-run-w1.jsonl")
	span := map[string]any{
		"trace_id": "run-w1", "span_id": "s1", "kind": "policy_decision",
		"attrs": map[string]any{"team": "team-W", "role": "worker", "skills": []any{"deploy-skill"}},
	}
	raw, _ := json.Marshal(span)
	if err := os.WriteFile(traceFile, []byte(string(raw)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 奖励: 两条 run-w1 正分
	evFile := filepath.Join(root, "evolution", "rewards.jsonl")
	if err := os.MkdirAll(filepath.Dir(evFile), 0o755); err != nil {
		t.Fatal(err)
	}
	var ev strings.Builder
	for i := 0; i < 2; i++ {
		ev.WriteString(fmt.Sprintf(`{"ts":%d,"run_id":"run-w1","source":"test","value":1,"weight":1}`+"\n", 1000+i))
	}
	if err := os.WriteFile(evFile, []byte(ev.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	b := newPoolBandit(filepath.Join(dir, "poolbandit.json"))
	b.coldStart(root)
	// 回灌粒度 = (run, skill) 一次 trial, 与奖励行数无关 (行数只推进水位线)。
	if a, be := b.posterior("team-W", "worker", "deploy-skill"); a != 1 || be != 0 {
		t.Fatalf("正分 run 应记一次 trial 计数 (1,0), got (%v,%v)", a, be)
	}
	if b.rewardLines != 2 {
		t.Fatalf("水位线应=2, got %d", b.rewardLines)
	}
	// 第二次 coldStart (模拟重启): 同文件不再重复计入。
	b2 := newPoolBandit(filepath.Join(dir, "poolbandit.json"))
	if a, be := b2.posterior("team-W", "worker", "deploy-skill"); a != 1 || be != 0 {
		t.Fatalf("重启载入应保持计数 (1,0) (水位线防重), got (%v,%v)", a, be)
	}
	b2.coldStart(root)
	if a, be := b2.posterior("team-W", "worker", "deploy-skill"); a != 1 || be != 0 {
		t.Fatalf("第二次 coldStart 不得重复计入, got (%v,%v)", a, be)
	}
	// 追加新奖励行 → 只计入增量 (同 run 再聚合出一次 trial)。
	f, _ := os.OpenFile(evFile, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"ts":2000,"run_id":"run-w1","source":"test","value":1,"weight":1}` + "\n")
	f.Close()
	b3 := newPoolBandit(filepath.Join(dir, "poolbandit.json"))
	b3.coldStart(root)
	if a, _ := b3.posterior("team-W", "worker", "deploy-skill"); a != 2 {
		t.Fatalf("增量 1 行应再记一次 trial, got alpha=%v", a)
	}
}

// fakeToolForDesc 带自定义描述的最小 Tool 实现 (检索索引构建用; fakeTool 描述恒定)。
type fakeToolForDesc struct {
	name, desc string
}

func (f *fakeToolForDesc) Name() string                           { return f.name }
func (f *fakeToolForDesc) Description() string                    { return f.desc }
func (f *fakeToolForDesc) InputSchema() json.RawMessage           { return json.RawMessage(`{"type":"object"}`) }
func (f *fakeToolForDesc) IsConcurrencySafe(json.RawMessage) bool { return true }
func (f *fakeToolForDesc) CheckPermissions(json.RawMessage, *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (f *fakeToolForDesc) IsReadOnly(json.RawMessage) bool { return true }
func (f *fakeToolForDesc) Call(context.Context, json.RawMessage, *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: "ok"}, nil
}
