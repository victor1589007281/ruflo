// pooltools_test.go —— 13.7-P0 包装三件单测 (docforge planning-skills-surge §13.7.7)。
//
// 锁定四组 P0 验收:
//  1. token 下降对拍: RegisterPoolTools 装配后 APITools 长度 = 基础 + 包装三件 +
//     已加载 (池关 = 全量), 未加载成员不进 schema 表;
//  2. search→load→调用 E2E: pool_search 命中池内工具/技能 → pool_load 换面 →
//     执行面直调可达 (未加载直调三语义的 ①③);
//  3. 治理叠加拒绝回注: denied 命中 / allowed fail-closed 时 pool_load 拒载,
//     错误写回 tool_result (IsError=true), 不静默;
//  4. 技能门禁: shadow 拒 / model-invocable=false 拒 / active 成功 + MarkInjected;
//     pool_release 收回后直调仍可达 (执行面不拦)。
package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// fakeTool 最小 Tool 实现 (只有名字, Call 恒返回名字)。
type fakeTool struct{ name string }

func (f *fakeTool) Name() string                 { return f.name }
func (f *fakeTool) Description() string          { return "fake " + f.name }
func (f *fakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f *fakeTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: "ran:" + f.name}, nil
}
func (f *fakeTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (f *fakeTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (f *fakeTool) IsReadOnly(_ json.RawMessage) bool { return true }

// setupPool 组装: reg 内含基础工具 Base1/Base2 + 池内工具 evo_inspect/evo_status, skillReg
// 按需注册技能。返回 (reg, pool, skillReg)。
func setupPool(t *testing.T, sk *skills.Skill) (*tool.Registry, *tool.Pool, *skills.Registry) {
	t.Helper()
	reg := tool.NewRegistry()
	reg.Register(&fakeTool{name: "Base1"})
	reg.Register(&fakeTool{name: "Base2"})
	reg.Register(&fakeTool{name: "evo_inspect"})
	reg.Register(&fakeTool{name: "evo_status"})
	var skillReg *skills.Registry
	if sk != nil {
		skillReg = skills.NewRegistry()
		skillReg.Register(sk)
	}
	pool := RegisterPoolTools(reg, skillReg, nil, nil)
	return reg, pool, skillReg
}

// poolCall 直接调用工具 (模拟模型侧一次 tool_use); 命名避开既有 callTool 助手。
func poolCall(t *testing.T, reg *tool.Registry, name, input string) *tool.ToolResult {
	t.Helper()
	tt, ok := reg.Get(name)
	if !ok {
		t.Fatalf("执行面 %s 不可达 (reg.Get 未命中)", name)
	}
	res, err := tt.Call(context.Background(), json.RawMessage(input), nil)
	if err != nil {
		t.Fatalf("%s Call 返回 error: %v", name, err)
	}
	return res
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// 1. token 下降对拍: 广告面 = 基础 + 包装三件 + 已加载
// ---------------------------------------------------------------------------

func TestPoolAdvertisedSurface(t *testing.T) {
	reg, pool, _ := setupPool(t, nil)

	// 池开 (未加载任何成员): 广告面 = Base1/Base2 + 包装三件, evo_inspect/evo_status 隐藏。
	api := pool.FilterAPITools(reg.APITools()) // 与 engine.go queryLoop 同一调用形态
	names := map[string]bool{}
	for _, a := range api {
		names[a.Name] = true
	}
	if !names["Base1"] || !names["Base2"] {
		t.Error("池外基础工具必须保留广告")
	}
	if !names[PoolSearchName] || !names[PoolLoadName] || !names[PoolReleaseName] {
		t.Error("包装三件必须恒常驻广告面")
	}
	if names["evo_inspect"] || names["evo_status"] {
		t.Error("未加载池成员不得进 schema 表 (token 下降验收)")
	}
	// 对拍: 全量 6 件 → 广告面 5 件 (2 基础 + 3 包装)。
	if len(api) != 5 {
		t.Fatalf("APITools len=%d, want 5 (2 基础 + 3 包装)", len(api))
	}

	// system prompt 的 available_tools 段同口径 (prompt 侧单测见 pkg/prompt)。

	// pool_load evo_inspect → 下一轮 schema 表含 evo_inspect, evo_status 仍隐藏。
	res := poolCall(t, reg, PoolLoadName, `{"name":"evo_inspect"}`)
	if res.IsError || !strings.Contains(res.Content, "已加载 evo_inspect") {
		t.Fatalf("pool_load evo_inspect 失败: %+v", res)
	}
	api = pool.FilterAPITools(reg.APITools())
	if len(api) != 6 {
		t.Fatalf("加载后 APITools len=%d, want 6", len(api))
	}
	for _, a := range api {
		if a.Name == "evo_status" {
			t.Error("evo_status 仍应隐藏")
		}
	}

	// pool_release evo_inspect → 广告面收回; 直调仍可达 (执行面不拦)。
	res = poolCall(t, reg, PoolReleaseName, `{"name":"evo_inspect"}`)
	if res.IsError || !strings.Contains(res.Content, "已释放 evo_inspect") {
		t.Fatalf("pool_release 失败: %+v", res)
	}
	if pool.Advertised("evo_inspect") {
		t.Error("release 后 evo_inspect 必须移出广告面")
	}
	if _, ok := reg.Get("evo_inspect"); !ok {
		t.Error("release 后执行面 evo_inspect 必须仍可达 (未加载直调语义①)")
	}
}

// ---------------------------------------------------------------------------
// 2. search→load→调用 E2E (含未加载直调语义②: 未知名报「未知工具」)
// ---------------------------------------------------------------------------

func TestPoolSearchLoadCallE2E(t *testing.T) {
	reg, pool, _ := setupPool(t, nil)

	// 未加载直调语义①: 未加载但执行面可达, 结果不变。
	res := poolCall(t, reg, "evo_inspect", `{}`)
	if res.Content != "ran:evo_inspect" {
		t.Fatalf("未加载直调 evo_inspect 结果=%q, want ran:evo_inspect", res.Content)
	}
	// 语义②: 未知名才报「未知工具」。
	if _, ok := reg.Get("NoSuchTool"); ok {
		t.Error("未知名 reg.Get 不得命中")
	}

	// search: 关键词命中 evo_inspect (名字+描述打分), 空查询全列。
	res = poolCall(t, reg, PoolSearchName, `{"query":"evo"}`)
	if res.IsError || !strings.Contains(res.Content, "evo_inspect (tool)") {
		t.Fatalf("pool_search 未命中 evo_* : %+v", res)
	}
	// 命中条目带「未加载」状态的可加载提示 (非治理限制)。
	if strings.Contains(res.Content, "受治理限制") {
		t.Error("无治理集时不得出现治理限制标记")
	}
	res = poolCall(t, reg, PoolSearchName, `{}`)
	if got := strings.Count(res.Content, "- "); got != 2 {
		t.Fatalf("空查询全列应恰为 2 项 (仅池内资产; 包装三件/基础工具在池外不进检索), got %d:\n%s", got, res.Content)
	}
	// 空查询只列池内资产 (包装三件/基础工具在池外, 不进检索)。
	if strings.Contains(res.Content, "Base1") || strings.Contains(res.Content, PoolSearchName) {
		t.Errorf("pool_search 不得列出池外资产:\n%s", res.Content)
	}

	// limit 截断。
	res = poolCall(t, reg, PoolSearchName, `{"query":"","limit":1}`)
	if got := strings.Count(res.Content, "- "); got != 1 {
		t.Fatalf("limit=1 应只回 1 项, got %d", got)
	}

	// search→load→再调用 E2E。
	res = poolCall(t, reg, PoolLoadName, `{"name":"evo_status"}`)
	if res.IsError {
		t.Fatalf("pool_load evo_status 失败: %+v", res)
	}
	if !pool.IsLoaded("evo_status") {
		t.Fatal("evo_status 应已入加载面")
	}

	// 重复加载: 幂等回注, 不报错。
	res = poolCall(t, reg, PoolLoadName, `{"name":"evo_status"}`)
	if res.IsError || !strings.Contains(res.Content, "无需重复加载") {
		t.Fatalf("重复加载应幂等回注: %+v", res)
	}

	// 池外工具: 拒载回注 (错误回注语义, IsError=true) + 引导文案。
	res = poolCall(t, reg, PoolLoadName, `{"name":"Base1"}`)
	if !res.IsError || !strings.Contains(res.Content, "不在池内") || !strings.Contains(res.Content, "无需加载") {
		t.Fatalf("池外工具加载应拒载回注「不在池内」: %+v", res)
	}

	// 未登记名: 同样拒载回注。
	res = poolCall(t, reg, PoolLoadName, `{"name":"totally-unknown"}`)
	if !res.IsError || !strings.Contains(res.Content, "不在池内") {
		t.Fatalf("未登记名应拒载回注「不在池内」: %+v", res)
	}

	// search 未命中: 回注引导文案。
	res = poolCall(t, reg, PoolSearchName, `{"query":"zzz-no-such"}`)
	if res.IsError || !strings.Contains(res.Content, "没有匹配") {
		t.Fatalf("search 未命中应回引导: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// 3. 治理叠加拒绝回注
// ---------------------------------------------------------------------------

func TestPoolGovernanceRejection(t *testing.T) {
	t.Run("denied 命中即拒", func(t *testing.T) {
		reg := tool.NewRegistry()
		reg.Register(&fakeTool{name: "Base1"})
		reg.Register(&fakeTool{name: "evo_inspect"})
		pool := RegisterPoolTools(reg, nil, map[string]bool{"evo_inspect": true}, nil)
		_ = pool

		res := poolCall(t, reg, PoolLoadName, `{"name":"evo_inspect"}`)
		if !res.IsError || !strings.Contains(res.Content, "受当前治理约束") {
			t.Fatalf("denied 资产必须拒绝加载且回注错误: %+v", res)
		}
		if pool.IsLoaded("evo_inspect") {
			t.Error("拒绝后不得进加载面")
		}
		// search 里该资产带治理标记。
		res = poolCall(t, reg, PoolSearchName, `{"query":"evo"}`)
		if !strings.Contains(res.Content, "受治理限制, 不可加载") {
			t.Fatalf("search 应标记治理限制: %+v", res)
		}
	})

	t.Run("allowed fail-closed", func(t *testing.T) {
		reg := tool.NewRegistry()
		reg.Register(&fakeTool{name: "Base1"})
		reg.Register(&fakeTool{name: "evo_inspect"})
		reg.Register(&fakeTool{name: "evo_status"})
		pool := RegisterPoolTools(reg, nil, nil, map[string]bool{"evo_inspect": true})
		_ = pool

		// 白名单内可加载。
		res := poolCall(t, reg, PoolLoadName, `{"name":"evo_inspect"}`)
		if res.IsError {
			t.Fatalf("白名单内资产必须放行: %+v", res)
		}
		// 白名单外 fail-closed 拒绝。
		res = poolCall(t, reg, PoolLoadName, `{"name":"evo_status"}`)
		if !res.IsError || !strings.Contains(res.Content, "受当前治理约束") {
			t.Fatalf("白名单外资产必须拒绝: %+v", res)
		}
	})
}

// ---------------------------------------------------------------------------
// 4. 技能门禁 + MarkInjected + pool_release
// ---------------------------------------------------------------------------

func ptr[T any](v T) *T { return &v }

func TestPoolLoadSkillGates(t *testing.T) {
	active := &skills.Skill{Name: "sk-active", Description: "an active skill", Body: "# Body\n\nActive content."}
	shadow := &skills.Skill{Name: "sk-shadow", Description: "shadow", Body: "x", Status: skills.StatusShadow}
	noModel := &skills.Skill{Name: "sk-useronly", Description: "user only", Body: "x", ModelInvocable: ptr(false)}

	t.Run("active 成功 + MarkInjected", func(t *testing.T) {
		reg, _, skillReg := setupPool(t, active)
		res := poolCall(t, reg, PoolSearchName, `{"query":"active skill"}`)
		if !strings.Contains(res.Content, "sk-active (skill)") {
			t.Fatalf("pool_search 应命中技能资产: %+v", res)
		}
		res = poolCall(t, reg, PoolLoadName, `{"name":"sk-active"}`)
		if res.IsError || !strings.Contains(res.Content, "## Skill: sk-active") {
			t.Fatalf("技能加载应回注正文: %+v", res)
		}
		if !skillReg.WasInjectedUnchanged("sk-active") {
			t.Error("加载成功必须 MarkInjected (13.6 F1 记账)")
		}
	})

	t.Run("shadow 拒绝", func(t *testing.T) {
		reg, _, _ := setupPool(t, shadow)
		res := poolCall(t, reg, PoolLoadName, `{"name":"sk-shadow"}`)
		if !res.IsError || !strings.Contains(res.Content, "未通过进化门禁") {
			t.Fatalf("shadow 技能必须拒绝: %+v", res)
		}
	})

	t.Run("model-invocable=false 拒绝", func(t *testing.T) {
		reg, _, _ := setupPool(t, noModel)
		res := poolCall(t, reg, PoolLoadName, `{"name":"sk-useronly"}`)
		if !res.IsError || !strings.Contains(res.Content, "仅用户可调用") {
			t.Fatalf("model-invocable=false 必须拒绝: %+v", res)
		}
	})
}

func TestPoolReleaseNoopAndDirectCall(t *testing.T) {
	reg, _, _ := setupPool(t, nil)

	// 未加载就 release: 无操作回注, 不报错。
	res := poolCall(t, reg, PoolReleaseName, `{"name":"evo_status"}`)
	if res.IsError || !strings.Contains(res.Content, "无操作") {
		t.Fatalf("未加载释放应回无操作: %+v", res)
	}

	// release 已加载成员后, 直调仍可达且结果不变 (执行面不拦语义)。
	res = poolCall(t, reg, PoolLoadName, `{"name":"evo_status"}`)
	if res.IsError {
		t.Fatalf("pool_load evo_status 失败: %+v", res)
	}
	res = poolCall(t, reg, PoolReleaseName, `{"name":"evo_status"}`)
	if res.IsError || !strings.Contains(res.Content, "已释放 evo_status") {
		t.Fatalf("pool_release 已加载成员应成功: %+v", res)
	}
	if _, ok := reg.Get("evo_status"); !ok {
		t.Fatal("evo_status 必须在执行面")
	}
	res = poolCall(t, reg, "evo_status", `{}`)
	if res.Content != "ran:evo_status" {
		t.Fatalf("释放后直调 evo_status 结果=%q, want ran:evo_status", res.Content)
	}
}
