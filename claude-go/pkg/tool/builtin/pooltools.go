// pooltools.go —— 13.7-P0 包装三件: pool_search / pool_load / pool_release
// (docforge planning-skills-surge §13.7.2「注册不懒、广告懒」的模型侧入口)。
//
// 语义:
//   - pool_search  检索池内资产 (工具与技能), 只回名称+一句话, 不下发 schema;
//     是 ToolSearch / MCPToolSearch (见 pkg/feishu/mcp_proxy_tools.go) 的统一升级。
//   - pool_load    加载资产: 工具 → 下一轮起进广告面 (schema 随请求下发);
//     技能 → 走 skills.SkillTool 同一加载语义 (active 门禁 + MarkInjected)。
//   - pool_release 卸载工具/技能 (执行面不拦, 只把下一轮广告面收回)。
//
// 执行语义与广告无关 (§13.7.7 三条执行语义, 由现有机械天然保证):
//
//	① 未加载直调不崩溃 —— 执行面是全量注册表 (orchestration.RunToolUse reg.Get);
//	② 未知名才报「未知工具」—— 同上, reg.Get 未命中;
//	③ 加载即换面, 按 run 粒度 —— 广告过滤在 queryLoop 每轮读 Pool.loaded。
//
// 治理叠加 (§13.7.3 三层 AND 的第三层): pool_load 拒绝治理集外资产 —— denied
// 命中即拒, allowed 非 nil 且不含即拒 (fail-closed, 与 engine.Config.toolExposed
// 同语义); 错误回注给模型, 不静默。加载成功面同样受治理: FilterAPITools 过滤后
// 下发, 但执行面 ToolGateHook 仍兜底 (双保险)。
//
// base 保持池外: 包装三件自身永远在广告面 (pool.SetAlways), 否则模型没有入口。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// 池包装三件的工具名 (pool_ 前缀新名, 不与既有 stub 同名纠缠)。
const (
	PoolSearchName  = "pool_search"
	PoolLoadName    = "pool_load"
	PoolReleaseName = "pool_release"
)

// poolAsset 池内资产条目 (search 命中排序的基本单位)。
type poolAsset struct {
	Name string
	Kind string // "tool" | "skill"
	Desc string
	// 检索键的原文快照 (打分用, 不回给模型)。
	text string
}

// poolIndex 池检索索引: 池成员登记时由装配方构建 (打分快照一次算好)。
type poolIndex struct {
	assets []poolAsset
}

// buildPoolIndex 装配时构建检索索引。tool 侧键 = Name+Description;
// skill 侧键 = Name+WhenToUse+Description+正文首段 (§13.7.5 pool_search 语义)。
func buildPoolIndex(pool *tool.Pool, reg *tool.Registry, skillReg *skills.Registry) *poolIndex {
	idx := &poolIndex{}
	if reg != nil {
		for _, name := range pool.MemberNames() {
			if t, ok := reg.Get(name); ok {
				desc := t.Description()
				idx.assets = append(idx.assets, poolAsset{
					Name: name,
					Kind: "tool",
					Desc: desc,
					text: strings.ToLower(name + "\n" + desc),
				})
			}
		}
	}
	if skillReg != nil {
		for _, name := range pool.MemberNames() {
			if s, ok := skillReg.GetAny(name); ok {
				desc := s.Description
				if s.WhenToUse != "" {
					desc += "\nUse when: " + s.WhenToUse
				}
				idx.assets = append(idx.assets, poolAsset{
					Name: s.Name,
					Kind: "skill",
					Desc: desc,
					text: strings.ToLower(s.Name + "\n" + s.WhenToUse + "\n" + desc + "\n" + firstParagraph(s.Body)),
				})
			}
		}
	}
	return idx
}

// firstParagraph 取正文首段 (检索键, 保守截断)。
func firstParagraph(body string) string {
	for _, para := range strings.Split(body, "\n\n") {
		p := strings.TrimSpace(para)
		if p == "" {
			continue
		}
		p = strings.TrimPrefix(p, "# ")
		r := []rune(p)
		if len(r) > 400 {
			p = string(r[:400])
		}
		return p
	}
	return ""
}

// search 打分检索: 空查询按登记序全列 (上限 limit); 非空按子串命中计分,
// Name 命中权重高于正文命中 (3 : 1), 同分保持登记序。
func (x *poolIndex) search(query string, limit int) []poolAsset {
	return x.searchPersonalized(query, limit, nil)
}

// searchPersonalized 打分检索 + 个性化加权 (13.7-P1): 相关性分子串命中计分,
// 再乘 (1 + α·ThompsonSample) 的 boost (三因子公式「相关性 × (1 + α·团队历史
// 加权分)」); boost nil = 纯相关性 (P0 行为)。同 boost 稳定排序保登记序,
// 空查询无相关性信号, 个性化不表达 (乘法公式的字面语义)。
func (x *poolIndex) searchPersonalized(query string, limit int, boost func(name string) float64) []poolAsset {
	q := strings.ToLower(strings.TrimSpace(query))
	type scored struct {
		asset poolAsset
		score int
	}
	var ss []scored
	for _, a := range x.assets {
		if q == "" {
			ss = append(ss, scored{a, 0})
			continue
		}
		score := 0
		for _, term := range strings.Fields(q) {
			if strings.Contains(strings.ToLower(a.Name), term) {
				score += 3
			}
			if strings.Contains(a.text, term) {
				score++
			}
		}
		if score > 0 {
			ss = append(ss, scored{a, score})
		}
	}
	if q != "" {
		// 先算 boost 再稳定排序: boost 内部有随机采样, 不能放进比较器
		// (同元素多次取值不定 → sort 不满足一致性, 会产生乱序结果)。
		if boost != nil {
			type boosted struct {
				scored
				key float64
			}
			bs := make([]boosted, len(ss))
			for i, s := range ss {
				bs[i] = boosted{scored: s, key: float64(s.score) * boost(s.asset.Name)}
			}
			sort.SliceStable(bs, func(i, j int) bool { return bs[i].key > bs[j].key })
			ss = make([]scored, len(bs))
			for i, b := range bs {
				ss[i] = b.scored
			}
		} else {
			sort.SliceStable(ss, func(i, j int) bool { return ss[i].score > ss[j].score })
		}
	}
	hits := make([]poolAsset, 0, len(ss))
	for _, s := range ss {
		hits = append(hits, s.asset)
	}
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// ---------------------------------------------------------------------------
// pool_search
// ---------------------------------------------------------------------------

// PoolSearchTool 池检索工具。
type PoolSearchTool struct {
	pool   *tool.Pool
	reg    *tool.Registry
	idx    *poolIndex
	policy *PoolPolicy
	// getenv 未命中回退开关的 env 源 (§13.7.7 平滑迁移②)。nil = 默认开
	// (不经 env, 常态)。测试可注入可控 getenv。
	getenv func(string) string
}

// NewPoolSearchTool 构建池检索工具 (policy 可 nil = 纯 P0 行为)。
func NewPoolSearchTool(pool *tool.Pool, reg *tool.Registry, skillReg *skills.Registry, policy ...*PoolPolicy) *PoolSearchTool {
	var p *PoolPolicy
	if len(policy) > 0 {
		p = policy[0]
	}
	return &PoolSearchTool{
		pool:   pool,
		reg:    reg,
		idx:    buildPoolIndex(pool, reg, skillReg),
		policy: p,
	}
}

func (t *PoolSearchTool) Name() string { return PoolSearchName }

func (t *PoolSearchTool) Description() string {
	return "Search the deferred tool & skill pool by keyword. Returns names and one-line descriptions only (no schemas); call pool_load to surface a hit's full schema/instructions on the next turn."
}

func (t *PoolSearchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Keywords describing the capability you need (e.g. 'video generate', 'sql 部署'). Empty lists the first page of the pool."},
			"limit": {"type": "integer", "description": "Max results (default 8)."}
		}
	}`)
}

func (t *PoolSearchTool) IsReadOnly(_ json.RawMessage) bool        { return true }
func (t *PoolSearchTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *PoolSearchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *PoolSearchTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errOnly("pool_search 输入解析失败: %v", err), nil
	}
	if in.Limit <= 0 {
		in.Limit = 8
	}
	// 13.7-P1 三因子: 相关性 × (1 + α·ThompsonSample)。α=0 时加成恒 0,
	// 排序与 P0 逐位一致 (纯相关分子串命中计分, 乘法零元保序)。
	team, role := poolIdentity(tctx)
	hits := t.idx.searchPersonalized(in.Query, in.Limit, func(name string) float64 {
		return 1 + t.policy.rankBoost(team, role, name)
	})
	// 13.7-P2 未命中回退 (§13.7.7 平滑迁移②): 池内检索落空时, 提示模型池外
	// (基础面) 工具本就直接可见, 并指出回退开关语义。回退本身不由 search 实现
	// —— 广告面是 queryLoop 逐轮读 Pool 的, 这里只把"未命中"事实与可用路径讲清。
	if len(hits) == 0 {
		attrs := map[string]any{"candidates": 0, "fallback": true}
		t.policy.writeSpan(ctx, PoolSearchName, in.Query, attrs)
		hint := "池内没有匹配资产。池外工具 (Read/Bash/Grep 等) 本就直接可见可调用; 换更通用的关键词再试, 或直接用现有工具完成任务。"
		if t.fallbackEnabled() {
			hint += " (未命中回退: 本会话已自动保持全量广告, 基础工具不受池化影响。)"
		}
		return &tool.ToolResult{Content: hint}, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("池内命中 %d 项 (pool_load 加载后下一轮生效):\n", len(hits)))
	for _, h := range hits {
		status := ""
		switch {
		case t.pool.IsLoaded(h.Name):
			status = " [已加载]"
		case t.pool.IsResident(h.Name):
			status = " [常驻]"
		case !t.pool.Governed(h.Name):
			status = " [受治理限制, 不可加载]"
		}
		sb.WriteString(fmt.Sprintf("- %s (%s): %s%s\n", h.Name, h.Kind, firstLine(h.Desc), status))
	}
	// 13.7-P1 选择留痕: 这次向模型展示了什么 (验收② 的 E2E 证据源, 同时供
	// 13.8.6 skillaudit 配对归因消费)。
	attrs := map[string]any{"candidates": len(hits)}
	t.policy.writeSpan(ctx, PoolSearchName, in.Query, attrs)
	// 13.7-P2 运行期再聚合: 命中路径说明本会话仍在检索, 后验可能因新 trial
	// (pool_load 弱正/奖励回灌) 变化 —— 顺手刷新常驻面 (轻量内存扫描, 变化在
	// 下一轮广告面生效, 不打扰本次响应)。
	t.policy.SetResidentFromBandit(t.pool)
	return &tool.ToolResult{Content: sb.String()}, nil
}

// fallbackEnabled 未命中回退开关 (nil getenv = 默认开)。
func (t *PoolSearchTool) fallbackEnabled() bool {
	return tool.PoolFallbackEnabled(t.getenv)
}

// ---------------------------------------------------------------------------
// pool_load
// ---------------------------------------------------------------------------

// PoolLoadTool 池加载工具。
type PoolLoadTool struct {
	pool     *tool.Pool
	skillReg *skills.Registry
	reg      *tool.Registry
	policy   *PoolPolicy
}

// NewPoolLoadTool 构建池加载工具 (policy 可 nil = 纯 P0 行为)。
func NewPoolLoadTool(pool *tool.Pool, reg *tool.Registry, skillReg *skills.Registry, policy ...*PoolPolicy) *PoolLoadTool {
	var p *PoolPolicy
	if len(policy) > 0 {
		p = policy[0]
	}
	return &PoolLoadTool{pool: pool, reg: reg, skillReg: skillReg, policy: p}
}

func (t *PoolLoadTool) Name() string { return PoolLoadName }

func (t *PoolLoadTool) Description() string {
	return "Load a deferred tool or skill into this session's advertised surface (takes effect next turn). Tools stay executable even before loading; loading just makes their schema visible."
}

func (t *PoolLoadTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Asset name (a pool_search hit, or a known tool/skill name)."}
		},
		"required": ["name"]
	}`)
}

func (t *PoolLoadTool) IsReadOnly(_ json.RawMessage) bool        { return true }
func (t *PoolLoadTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *PoolLoadTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *PoolLoadTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errOnly("pool_load 输入解析失败: %v", err), nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errOnly("pool_load: name 必填"), nil
	}

	// 治理叠加 (§13.7.3 第三层): 治理集外资产拒绝加载, 错误回注不静默。
	// 与 engine.Config.toolExposed 同语义 (denied 命中即拒 / allowed fail-closed)。
	if !t.pool.Governed(name) {
		return errOnly("资产 %q 受当前治理约束 (禁用/白名单), 拒绝加载。若确需使用, 请向用户说明。", name), nil
	}

	// 技能资产: 走 SkillTool 同一加载语义 (active 门禁 + 拒 model_invocable=false)。
	// skillReg 可为 nil (纯工具会话, 如治理测试/无技能装配), nil 视为无技能资产。
	if t.skillReg != nil {
		if s, ok := t.skillReg.GetAny(name); ok {
			return t.loadSkill(ctx, s, tctx)
		}
	}

	// 工具资产: 必须是登记过的池成员。
	if !t.pool.IsMember(name) {
		return errOnly("%q 不在池内。用 pool_search 查找可加载资产; 池外工具本来就可见, 无需加载。", name), nil
	}
	if _, ok := t.reg.Get(name); !ok {
		// 登记/执行面失同步 (理论上不该发生): 显式报错而不是静默标记。
		return errOnly("%q 已登记池内但执行面缺失 (内部状态不一致), 拒绝加载。", name), nil
	}
	if t.pool.Load(name) {
		t.noteLoaded(ctx, tctx, name, map[string]any{})
		return &tool.ToolResult{Content: fmt.Sprintf("已加载 %s (工具)。下一轮起其 schema 随请求下发; 未加载期间它也可直接调用, 结果不变。", name)}, nil
	}
	if t.pool.IsLoaded(name) {
		return &tool.ToolResult{Content: fmt.Sprintf("%s 已在加载面, 无需重复加载。", name)}, nil
	}
	return errOnly("%q 加载失败 (内部状态)", name), nil
}

// noteLoaded 成功加载后的 P1 记账: bandit 弱正 trial + 选择留痕 Span
// (attrs.skills 与 13.8.6 配对归因的 <role_skills> 反解同字段语义)。nil policy 零成本。
func (t *PoolLoadTool) noteLoaded(ctx context.Context, tctx *tool.ToolContext, name string, attrs map[string]any) {
	if t.policy == nil {
		return
	}
	team, role := poolIdentity(tctx)
	t.policy.noteSelection(team, role, name)
	attrs["selected"] = name
	if role != "" {
		attrs["role"] = role
	}
	if team != "" {
		attrs["team"] = team
	}
	t.policy.writeSpan(ctx, PoolLoadName, name, attrs)
}

// loadSkill 技能加载: 语义对齐 skills.SkillTool.Call (active 门禁 / 模型可调用 /
// 未找到回退提示), 成功时同样 MarkInjected (13.6 F1 已注入记账)。
func (t *PoolLoadTool) loadSkill(ctx context.Context, s *skills.Skill, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	if !s.IsActive() {
		return errOnly("技能 %q 处于 %s 态, 未通过进化门禁, 不可加载 (需先晋升为 active)。",
			s.Name, strings.TrimSpace(s.Status)), nil
	}
	if !s.ModelCallable() {
		return errOnly("技能 %q 配置为仅用户可调用 (model-invocable: false), 模型侧不可加载。", s.Name), nil
	}
	if !t.pool.Load(s.Name) && t.pool.IsLoaded(s.Name) {
		return &tool.ToolResult{Content: fmt.Sprintf("%s 已在加载面, 无需重复加载。", s.Name)}, nil
	}
	// pool_load 的技能语义 = Skill 加载: 正文直接回注 (即「加载即换面」),
	// 并做已注入记账。
	content := fmt.Sprintf("## Skill: %s\n\n%s", s.Name, s.Body)
	if s.SkillDir != "" {
		content += fmt.Sprintf("\n\n---\nSkill directory: %s", s.SkillDir)
	}
	t.skillReg.MarkInjected(s.Name)
	t.noteLoaded(ctx, tctx, s.Name, map[string]any{"skills": []string{s.Name}})
	return &tool.ToolResult{Content: content}, nil
}

// ---------------------------------------------------------------------------
// pool_release
// ---------------------------------------------------------------------------

// PoolReleaseTool 池卸载工具。
type PoolReleaseTool struct {
	pool     *tool.Pool
	skillReg *skills.Registry
}

// NewPoolReleaseTool 构建池卸载工具。
func NewPoolReleaseTool(pool *tool.Pool, skillReg *skills.Registry) *PoolReleaseTool {
	return &PoolReleaseTool{pool: pool, skillReg: skillReg}
}

func (t *PoolReleaseTool) Name() string { return PoolReleaseName }

func (t *PoolReleaseTool) Description() string {
	return "Release a deferred tool or skill from this session's advertised surface (next turn). Execution is never blocked — the tool remains callable by name; only its schema/instructions stop being re-sent."
}

func (t *PoolReleaseTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Asset name to release."}
		},
		"required": ["name"]
	}`)
}

func (t *PoolReleaseTool) IsReadOnly(_ json.RawMessage) bool        { return true }
func (t *PoolReleaseTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *PoolReleaseTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

func (t *PoolReleaseTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errOnly("pool_release 输入解析失败: %v", err), nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return errOnly("pool_release: name 必填"), nil
	}
	if t.pool.Release(name) {
		return &tool.ToolResult{Content: fmt.Sprintf("已释放 %s: 下一轮起移出广告面; 直接按名调用仍可执行, 不受影响。", name)}, nil
	}
	return &tool.ToolResult{Content: fmt.Sprintf("%s 不在当前加载面 (未加载或已释放), 无操作。", name)}, nil
}

// errOnly 错误回注辅助 (errResult 的单返回值形态): 包装三件所有拒载/错误都
// 走这里 —— 错误回注给模型, 不静默 (§13.7.2)。
func errOnly(format string, args ...any) *tool.ToolResult {
	res, _ := errResult(format, args...)
	return res
}

// firstLine 取描述首行 (与 prompt.go 同语义, 本文件内小实现避免导出纠缠)。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// 装配
// ---------------------------------------------------------------------------

// RegisterPoolTools 装配包装三件 + 池状态挂载 (装配方在**全部池成员注册完成后**调用)。
//
// 步骤:
//  1. reg.WireToolPool(denied, allowed) —— 池挂上 Registry, 治理集从引擎同源拷贝;
//  2. 登记自训练/动态工具为池成员 (第一期: evo_* 八件 + Media 六件 + CodeIntel 五件;
//     内置 54 件保留广告), 仅登记注册表中实际存在的名字 (执行面同步, pool_load
//     直调语义依赖);
//  3. 技能入池 (skillReg 非空时) —— pool_search 检索 + pool_load 一致路径;
//  4. pool.SetAlways(包装三件名) + 注册包装三件本体。
//
// 返回池实例 (nil = 未装配)。装配方把它交给引擎侧 (同实例, 广告过滤直接生效)。
//
// 13.7-P1: opts 可选传 *PoolPolicy (个性化 + 选择留痕); 不传 = 纯 P0 行为,
// 全部既有调用与测试零改动。传入时 pool_search 排序按 Thompson 后验加成、
// pool_load 记 trial + 写 KindPolicyDecision Span。
// 13.7-P2: policy 非 nil 且已带 stateRoot 时, 装配即做一次常驻面聚合 (高后验
// 资产免 search 直接进广告面) + 软上限护栏告警; 之后每次 pool_search 命中后再
// 聚合一次 (轻量, 后验表内存扫描)。fallback getenv 注入 pool_search 的未命中
// 回退提示 (nil = PoolFallbackEnabled(os.Getenv) 默认开)。
func RegisterPoolTools(reg *tool.Registry, skillReg *skills.Registry, denied, allowed map[string]bool, opts ...*PoolPolicy) *tool.Pool {
	var policy *PoolPolicy
	if len(opts) > 0 {
		policy = opts[0]
	}
	pool := reg.WireToolPool(denied, allowed)
	// 第一期下沉集: 高频低价值/自训练资产先进池 (evo_*、Media、CodeIntel);
	// 54 内置 (Read/Bash/...) 保留广告 —— 平滑迁移②。
	// 只登记执行面已存在的名字: 测试装配可能不含全部下沉集, 而登记一个
	// 执行面缺失的名字会让 pool_load 永远撞「执行面缺失」分支。
	deferred := make([]string, 0, 19)
	deferred = append(deferred, evolutionToolNames()...)
	deferred = append(deferred, mediaToolNames()...)
	deferred = append(deferred, codeIntelToolNames()...)
	for _, name := range deferred {
		if _, ok := reg.Get(name); ok {
			pool.AddMember(name)
		}
	}
	// 技能入池 (skillReg 非空时): pool_search 检索键覆盖 WhenToUse/正文首段,
	// pool_load 走 SkillTool 同一加载语义 (active 门禁 + MarkInjected)。
	// 技能在 L1 短清单仍恒可见 (名称写出), 入池只影响 pool_search 检索与
	// pool_load 的一致路径; 技能加载本就即时回注正文, 不改变换面时机。
	if skillReg != nil {
		for _, s := range skillReg.ModelVisibleActive() {
			pool.AddMember(s.Name)
		}
	}
	pool.SetAlways(PoolSearchName, PoolLoadName, PoolReleaseName)
	// 13.7.9 池观测: 装配即登记包级最近池 (worker 心跳经 tool.ObservedPool 取数)。
	tool.ObservePool(pool)

	st := NewPoolSearchTool(pool, reg, skillReg, policy)
	st.getenv = os.Getenv
	reg.Register(st)
	reg.Register(NewPoolLoadTool(pool, reg, skillReg, policy))
	reg.Register(NewPoolReleaseTool(pool, skillReg))

	// 13.7-P2 装配期一次常驻聚合 + 护栏检查 (bandit 可用时; 纯 P0 装配零开销)。
	// 运行期再聚合由 pool_search 命中路径触发 (见 PoolSearchTool.Call 之后)。 ——
	// 常驻面与加载面平级, SetResident 只动广告位, 不动治理判定。
	if policy != nil && len(pool.MemberNames()) > 0 {
		if names := policy.SetResidentFromBandit(pool); len(names) > 0 {
			log.Printf("[toolpool] 常驻面聚合: %d 项高后验资产免 search 常驻 (%v)", len(names), names)
		}
		for _, g := range tool.CheckGuardrails(countPoolTools(pool, reg), countActiveSkills(skillReg), countShadowSkills(skillReg)) {
			if g.Exceeded {
				log.Printf("[toolpool] 护栏告警: %s 超软上限 %d/%d —— 建议触发\"晋升进基础面 or 归档\"仲裁 (不阻塞运行)", g.Kind, g.Current, g.Limit)
			}
		}
	}
	return pool
}

// countPoolTools 池内 tool 成员数 (skill 成员在 skillReg 里, 不计)。
func countPoolTools(pool *tool.Pool, reg *tool.Registry) int {
	if pool == nil || reg == nil {
		return 0
	}
	n := 0
	for _, name := range pool.MemberNames() {
		if _, ok := reg.Get(name); ok {
			n++
		}
	}
	return n
}

// countActiveSkills / countShadowSkills 技能侧盘点 (skillReg nil = 0)。
func countActiveSkills(skillReg *skills.Registry) int {
	if skillReg == nil {
		return 0
	}
	return len(skillReg.Active())
}

func countShadowSkills(skillReg *skills.Registry) int {
	if skillReg == nil {
		return 0
	}
	return len(skillReg.WithStatus("shadow"))
}

// evolutionToolNames / mediaToolNames / codeIntelToolNames 第一期下沉集清单。
// (与 register.go/evotools.go 的注册名单同源; 名单漂移由池单测钉住。)
func evolutionToolNames() []string {
	return []string{
		"evo_list_envs", "evo_inspect", "evo_propose", "evo_smoke",
		"evo_run_experiment", "evo_status", "evo_promote", "evo_rollback",
	}
}

func mediaToolNames() []string {
	return []string{
		"GenerateImage", "GenerateGIF", "GenerateVideo",
		"GeneratePPTX", "GenerateChart", "GenerateSpeech",
	}
}

func codeIntelToolNames() []string {
	return []string{
		"code_intel_init", "code_intel_update", "code_intel_status",
		"code_intel_query", "code_intel_branch",
	}
}
