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

	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/hooks"
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
	// Kind "tool" | "skill" | "hook"。
	//
	// 这一栏不只是给模型看的标签 —— 三类资产的 pool_load 语义**完全不同**
	// (工具换下一轮的 schema 广告面 / 技能即时回注正文 / hook 装进 Runner 的
	// 运行期组), 所以它是模型判断"加载它会发生什么"的唯一依据。分类标注错了的
	// 表现不是报错, 是模型按另一类的预期去用它。
	Kind string
	Desc string
	// 检索键的原文快照 (打分用, 不回给模型)。
	text string
	// Aliases 部署侧声明的检索别名 (tool.RecallAliasProvider)。空 = 没声明 ——
	// 与"声明了空表"是两回事, 但都表示"没有额外通路", 所以这里不区分。
	Aliases []string
}

// poolIndex 池检索索引: 池成员登记时由装配方构建 (打分快照一次算好)。
type poolIndex struct {
	assets []poolAsset
	// hookCfgs 池内 hook 资产名 → 解析出的 hook 配置。
	//
	// 存在索引里而不是 pool_load 现场解析: 解析结果**同时**是检索键的来源 (见
	// buildPoolIndex 的 hook 段取命令/URL 作正文), 两处各解析一次就会出现"检索时
	// 认得、加载时认不得"这种只有某个拼写才现形的分叉。索引构建是装配期一次性的,
	// 现场解析是每次 pool_load 一次 —— 同一个纯函数跑两次没有收益, 只有漂移风险。
	hookCfgs map[string][]types.HookConfig
}

// buildPoolIndex 装配时构建检索索引。tool 侧键 = Name+Description;
// skill 侧键 = Name+WhenToUse+Description+正文首段 (§13.7.5 pool_search 语义);
// hook 侧键 = Name+Description+正文首段 (与 skill 同形 —— hook 资产的语义同样在
// 正文里, `hooks:` 那几行只是接线)。
//
// assets 可为 nil (没有资产体系 / 调用方未接): 那时 hook 侧一件都不进, 与改造前
// 逐字节一致 —— 六种资产在绝大多数部署上还是空的, 空目录不该改变任何既有行为。
func buildPoolIndex(pool *tool.Pool, reg *tool.Registry, skillReg *skills.Registry, assets *asset.Set) *poolIndex {
	idx := &poolIndex{hookCfgs: map[string][]types.HookConfig{}}
	if reg != nil {
		for _, name := range pool.MemberNames() {
			if t, ok := reg.Get(name); ok {
				desc := t.Description()
				// 别名是**可选**接口: 实现了就带上, 没实现就没有额外检索通路。
				// 做成可选而不是必选, 是为了不让几十个工具为了满足签名而返回
				// 一个空切片 —— 那是"没声明"伪装成"声明过了" (见 tool.RecallAliasProvider)。
				//
				// ⚠️ 这里**只**断言 RecallAliasProvider, 不断言 AliasedTool: 后者是
				// 工具**名**的别名 (派发用), 断言它会把注册别名读成检索词 —— 见
				// RecallAliasProvider 的说明。
				var aliases []string
				if ap, ok := t.(tool.RecallAliasProvider); ok {
					aliases = ap.RecallAliases()
				}
				idx.assets = append(idx.assets, poolAsset{
					Name:    name,
					Kind:    "tool",
					Desc:    desc,
					text:    strings.ToLower(name + "\n" + desc),
					Aliases: aliases,
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
	// hook 侧 (21.8.12 P3「hook 进池」): 只收**池成员**里的 hook 资产。
	//
	// 为什么先看 IsMember 而不是直接遍历全部 hook 资产: 池的语义是"登记的才在池里",
	// 未登记的资产对 pool_search 不可见、也不该能被 pool_load 加载。绕开成员表直接
	// 全收, 会让"哪些 hook 进池"这件事没有单一真源 —— 装配方决定登记什么就不再算数。
	//
	// ★ 这一段与 RegisterPoolTools 里"把 hook 资产登记为池成员"那一段是**一对**。
	// 只登记不收 = 池里列着名字但 search 永远命不中; 只收不登记 = search 命中而
	// IsMember 拦下。两者都不会报错, 只表现为"hook 进池这件事没发生"。
	if assets != nil {
		for _, name := range pool.MemberNames() {
			a := assets.Get(asset.KindHook, name)
			if a == nil || !a.IsActive() {
				// 影子/归档的 hook 不进池 —— 与 skills 的 ModelVisibleActive 同一条
				// 理由: 能被模型直接加载的资产若包含影子态, 灰度就形同虚设。
				continue
			}
			cfgs, problems := hooks.ParseAssetHooks(a)
			// 解析出零条 (声明写坏了 / 只写了正文没写 `hooks:`) 时**不进池**。
			//
			// 这不只是省一行: 一条进池却装不出任何 hook 的资产, 会让模型经历
			// search 命中 → load 成功 → 什么都没发生。而"什么都没发生"与"模型自己
			// 判断无需加载"在事后完全同形。
			//
			// 问题本身**必须留痕**(纪律 ②): 一个 hook 资产没进池, 从池这一侧看就是
			// "它不存在" —— 而那正是 job 作者最需要知道的那种沉默。这里逐条打出来,
			// 与组装器那边的 BindingInvalidValue 是同一条事实的两个出口 (池与装配
			// 各自独立失联, 所以两边都要报)。
			if len(cfgs) == 0 {
				for _, p := range problems {
					log.Printf("[toolpool][warn] hook 资产 %q 未进池: %s", name, p)
				}
				if len(problems) == 0 {
					log.Printf("[toolpool][warn] hook 资产 %q 未进池: 没有解析出任何 hook 声明 (需要 hooks: 段)", name)
				}
				continue
			}
			desc := a.Description
			if desc == "" {
				desc = firstProseLine(a.Body)
			}
			idx.assets = append(idx.assets, poolAsset{
				Name: a.Name,
				Kind: "hook",
				Desc: desc,
				text: strings.ToLower(a.Name + "\n" + desc + "\n" + firstParagraph(a.Body)),
			})
			idx.hookCfgs[a.Name] = cfgs
		}
	}
	return idx
}

// firstProseLine 取正文首行非标题内容 (与 pkg/asset 的同名函数同语义)。
//
// 本包不引 pkg/asset 的私有实现, 但**引它的行为**: 资产描述为空时回落到正文首行是
// asset 层的既定规则 (ParseContent 已经这么做过一次)。这里再判一次是因为池不经过
// asset.Set 的渲染路径, 直接拿 *asset.Asset。重写一行而不是导出 asset 的一个内部
// 辅助: 那个函数短到没有维护成本, 而把它导出会让 pkg/asset 多一个只为一个调用方
// 存在的 API。
func firstProseLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := []rune(line)
		if len(r) > 200 {
			return string(r[:200]) + "…"
		}
		return line
	}
	return ""
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

// docs 把索引摊成检索引擎输入 (与 x.assets 同序 —— 融合结果靠下标回指资产)。
func (x *poolIndex) docs() []tool.Doc {
	out := make([]tool.Doc, len(x.assets))
	for i, a := range x.assets {
		out[i] = tool.Doc{Name: a.Name, Text: a.text, Aliases: a.Aliases}
	}
	return out
}

// Rank 实现 tool.Ranker: 池内检索的对外形状。
//
// 度量 (tool.EvaluateRecall) 只认这个接口, 所以保留集测的是**这条路径**,
// 不是测试里另写的一份等价实现 —— 后者绿了说明不了生产召回得怎么样。
func (x *poolIndex) Rank(query string, limit int) []string {
	hits := x.searchPersonalized(query, limit, nil)
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Name)
	}
	return out
}

// search 检索: 空查询按登记序全列 (上限 limit); 非空走两路召回 + RRF 融合,
// 同分保持登记序。
func (x *poolIndex) search(query string, limit int) []poolAsset {
	return x.searchPersonalized(query, limit, nil)
}

// searchPersonalized 检索 + 个性化加权 (13.7-P1): 相关性由**两路召回融合**得到,
// 再乘 (1 + α·ThompsonSample) 的 boost (三因子公式「相关性 × (1 + α·团队历史
// 加权分)」); boost nil = 纯相关性。空查询无相关性信号, 个性化不表达。
//
// ── 相关性那一路为什么换了实现 (D4.2) ─────────────────────────────────────
//
// 原来是**子串命中计分**: 查询词得逐字出现在 name/text 里, `score > 0` 才进候选。
// 它的失效方式不是"排得不好"而是**排不出来**: 换个说法、换个语言, 得分就是 0,
// 于是返回空集 —— 而空集在模型那里等于"这个能力不存在"。
//
// 现在换成 BM25 + 词面覆盖两路, RRF 融合 (实现在 pkg/tool/recall.go, 那里有
// 为什么是这两路、以及为什么 RRF 在这里**不是**向量检索的说明)。保留下来的
// 语义有两条:
//
//	· 空查询 = 按登记序全列 (个性化不表达, 乘法公式的字面语义);
//	· 非空查询里**同分保登记序** —— 融合时并列共享名次, 所以两个相关性完全
//	  一样的资产拿到同一个融合分, 稳定排序把它们交还给登记序。
//
// 第二条是**承重的**: 个性化 boost 是乘在相关性分上的, 如果相关性那边已经因为
// 登记顺序产生了名次差, boost 就会去放大一个纯粹的偶然。α=0 (boost 恒 1) 时
// 排序必须与纯相关性逐位一致, 那条对拍测试 (poolbandit_test.go) 盯的就是它。
func (x *poolIndex) searchPersonalized(query string, limit int, boost func(name string) float64) []poolAsset {
	q := strings.TrimSpace(query)
	if q == "" {
		hits := append([]poolAsset(nil), x.assets...)
		if limit > 0 && len(hits) > limit {
			hits = hits[:limit]
		}
		return hits
	}
	docs := x.docs()
	fused := tool.FuseRRF([][]tool.Ranked{
		tool.RankBM25(q, docs),
		tool.RankCoverage(q, docs),
	})
	type scored struct {
		asset poolAsset
		score float64
	}
	ss := make([]scored, 0, len(fused))
	for _, r := range fused {
		ss = append(ss, scored{asset: x.assets[r.Idx], score: r.Score})
	}
	// 先算 boost 再稳定排序: boost 内部有随机采样, 不能放进比较器
	// (同元素多次取值不定 → sort 不满足一致性, 会产生乱序结果)。
	if boost != nil {
		type boosted struct {
			scored
			key float64
		}
		bs := make([]boosted, len(ss))
		for i, s := range ss {
			bs[i] = boosted{scored: s, key: s.score * (1 + boost(s.asset.Name))}
		}
		sort.SliceStable(bs, func(i, j int) bool { return bs[i].key > bs[j].key })
		for i, b := range bs {
			ss[i] = b.scored
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

// NewPoolSearchTool 构建池检索工具 (自建索引; policy 可 nil = 纯 P0 行为;
// assets 可 nil = 池里不收 hook 资产, 与改造前逐字节一致)。
func NewPoolSearchTool(pool *tool.Pool, reg *tool.Registry, skillReg *skills.Registry, assets *asset.Set, policy ...*PoolPolicy) *PoolSearchTool {
	var p *PoolPolicy
	if len(policy) > 0 {
		p = policy[0]
	}
	return NewPoolSearchToolWithIndex(pool, reg, p, buildPoolIndex(pool, reg, skillReg, assets))
}

// NewPoolSearchToolWithIndex 用**调用方给**的索引构建 (装配路径专用)。
//
// 为什么要有这个: 索引里现在住着 hookCfgs, 而 pool_load/release 也要用它 ——
// 三个工具必须看**同一张表**。让每个工具各自 buildPoolIndex 一次, 得到的不是
// "同一张表的三份拷贝"而是三个独立快照: 任何一处 (未来的) 增量登记都会让它们
// 分叉, 而分叉的表现是"search 命中、load 说不在池内"这种没有任何报错的怪相。
func NewPoolSearchToolWithIndex(pool *tool.Pool, reg *tool.Registry, policy *PoolPolicy, idx *poolIndex) *PoolSearchTool {
	if idx == nil {
		idx = buildPoolIndex(pool, reg, nil, nil)
	}
	return &PoolSearchTool{pool: pool, reg: reg, idx: idx, policy: policy}
}

// Name 返回工具对外名 "pool_search"。
func (t *PoolSearchTool) Name() string { return PoolSearchName }

// Description 返回给模型看的说明: 只回名称 + 一句话, 不下发 schema, 命中后要 pool_load。
func (t *PoolSearchTool) Description() string {
	return "Search the deferred tool, skill & hook pool by keyword. Returns names and one-line descriptions only (no schemas); call pool_load to surface a hit's full schema/instructions on the next turn."
}

// InputSchema 返回入参 JSON Schema (query 可空 = 列首页; limit 默认 8)。
func (t *PoolSearchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Keywords describing the capability you need (e.g. 'video generate', 'sql 部署'). Empty lists the first page of the pool."},
			"limit": {"type": "integer", "description": "Max results (default 8)."}
		}
	}`)
}

// IsReadOnly 恒只读: 检索不改变池状态 (写 Span 属观测, 不算副作用)。
func (t *PoolSearchTool) IsReadOnly(_ json.RawMessage) bool        { return true }
// IsConcurrencySafe 恒可并发: 检索只读索引快照。
func (t *PoolSearchTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
// CheckPermissions 不附加 Deny —— 池检索本身不构成越权 (治理在 pool_load 侧把关)。
func (t *PoolSearchTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 按 query 检索池并渲染成"名称 (类别): 一句话 [状态]"清单回给模型。
//
// 算法解读：
//
//	原理：检索走 searchPersonalized —— BM25 + 词面覆盖两路召回, RRF 融合后再乘
//	      (1 + α·ThompsonSample) 的个性化加成; 本函数只做 limit 归一、命中渲染、
//	      未命中回退提示与选择留痕。
//	关键不变量：α=0 (policy 为 nil 或 bandit 不可用) 时排序必须与纯相关性逐位一致 ——
//	      boost 是乘在相关性分上的, 相关性那边若已因登记顺序产生名次差, boost 会去
//	      放大一个纯粹的偶然 (对拍测试 poolbandit_test.go 盯的就是这条)。
//	代价与边界：每次调用对全池做一次 BM25 + 覆盖打分 (池规模 ~40, 可忽略); 命中路径
//	      末尾还会顺手刷一次常驻面聚合 (内存扫描, 下一轮广告面才生效)。
//	设计权衡：未命中时不再说"池内没有匹配资产", 而是给 EmptyRecallNote —— 池里明明
//	      有资产、只是没有共同词时, 前一种说法是假话, 模型会照它行事 (判定能力不存在)。
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
		// ⚠️ 这句话原来是"池内没有匹配资产"。池里有 42 件、只是没有一件与查询
		// 有共同词时, 那**是一句假话** —— 而模型会照它行事 (判定"这个能力不
		// 存在" → 放弃, 或拿别的工具硬凑结论)。见 tool.EmptyRecallNote。
		return &tool.ToolResult{Content: tool.EmptyRecallNote(in.Query, len(t.idx.assets), t.fallbackEnabled())}, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("池内命中 %d 项 (pool_load 加载后下一轮生效):\n", len(hits)))
	for _, h := range hits {
		// 状态标记只用于**告知**, 不改变检索结果: 受治理限制的资产仍会被列出,
		// 只是提前说明它加载不了 (否则模型会去 load 一次再吃一个拒载错误)。
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
	// hookCfgs 池内 hook 资产名 → 解析出的配置 (与 PoolSearchTool.idx 同源同物)。
	//
	// 同一个 *poolIndex 的两个出口: search 用它算检索键, load 用它取要装的配置。
	// 不各建一份的理由 —— 见 poolIndex.hookCfgs 的注释。
	idx *poolIndex
}

// NewPoolLoadTool 构建池加载工具 (policy 可 nil = 纯 P0 行为)。
//
// idx 为 nil 时现建一个 (hook 侧为空) 的索引: 直接留 nil 会让下面的
// `t.idx.hookCfgs[name]` 解空指针 —— 而"零值索引"与"没索引"在这条路径上
// 语义本来就一样 (都表示"没有 hook 信息可用")。
func NewPoolLoadTool(pool *tool.Pool, reg *tool.Registry, skillReg *skills.Registry, idx *poolIndex, policy ...*PoolPolicy) *PoolLoadTool {
	var p *PoolPolicy
	if len(policy) > 0 {
		p = policy[0]
	}
	if idx == nil {
		idx = buildPoolIndex(pool, reg, skillReg, nil)
	}
	return &PoolLoadTool{pool: pool, reg: reg, skillReg: skillReg, policy: p, idx: idx}
}

// Name 返回工具对外名 "pool_load"。
func (t *PoolLoadTool) Name() string { return PoolLoadName }

// Description 返回给模型看的说明: 加载下一轮生效; 工具加载前也可直接调用, 结果不变。
func (t *PoolLoadTool) Description() string {
	return "Load a deferred asset into this session (takes effect next turn). Tools/skills: surfaces their schema/instructions. Hooks: installs them into this session's hook runner. Tools stay executable even before loading."
}

// InputSchema 返回入参 JSON Schema (name 必填)。
func (t *PoolLoadTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Asset name (a pool_search hit, or a known tool/skill/hook name)."}
		},
		"required": ["name"]
	}`)
}

// IsReadOnly 报只读: 加载只改本会话的广告面, 不写盘也不改远端状态。
func (t *PoolLoadTool) IsReadOnly(_ json.RawMessage) bool        { return true }
// IsConcurrencySafe 报可并发: 加载标记与 Runner 装卸各自有锁, 不依赖调用序。
func (t *PoolLoadTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
// CheckPermissions 不附加 Deny —— 治理集外的资产由 Call 内的 pool.Governed 拒绝并回注原因。
func (t *PoolLoadTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 把一件池内资产加载进本会话 (下一轮起生效)。
//
// 算法解读：
//
//	原理：按资产类别分派到三条语义完全不同的路径 —— 工具只置加载标记 (下一轮广告面
//	      带上它的 schema)、技能即时回注正文并 MarkInjected、hook 真的装进 Runner 的
//	      运行期组。分派顺序是 技能 → hook → 工具。
//	关键不变量：**加载成功必须真的发生** —— sink 为 nil (池装配早于 Runner 构造) 时
//	      必须显式报错而不是回"已加载"; 否则读数说装了而 Runner 上什么都没有。
//	代价与边界：每次调用最多一次 map 查表 + 一次 InstallHookGroup; 幂等分支会**重装**
//	      一次 hook 组, 因为"加载标记"与"Runner 上的组"是两处可能不同步的状态。
//	设计权衡：hook 只认索引里登记过的名字 (t.idx.hookCfgs), 这一句同时充当成员与合法性
//	      判定, 不再另查一遍池 —— 代价是索引与登记表必须成对维护 (见 buildPoolIndex)。
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

	// hook 资产 (21.8.12 P3「hook 进池」)。
	//
	// 放在技能之后、工具之前, 且只认**索引里登记过的** hook 资产 (t.idx.hookCfgs):
	// 索引只收"是池成员 + active + 解析得出 hook"的资产, 所以这一句同时也是
	// 成员与合法性的判定, 不需要再查一遍。
	//
	// ★ 三种失效各自不同, 必须分开报:
	//   · sink 为 nil  —— 池装配早于 Runner 构造时会发生。**不能静默成功**: 报
	//     "已加载"而 Runner 上什么都没装, 是本批最贵的那类缺陷 (读数说装了)。
	//   · Load 返回 false 且已加载 —— 幂等, 与工具体裁一致。
	//   · 装入 0 条 —— 理论上不会发生 (索引已滤掉空配置), 如实报内部不一致。
	if cfgs, isHook := t.idx.hookCfgs[name]; isHook {
		sink := t.pool.HookSink()
		if sink == nil {
			return errOnly("资产 %q 是 hook, 但本会话的运行时不支持运行期装载 hook (装配顺序: 池先于 hook 运行器)。"+
				"这条 hook **没有生效** —— 需要它在启动配置里声明, 而不是运行期加载。", name), nil
		}
		if t.pool.Load(name) {
			n := sink.InstallHookGroup(name, cfgs)
			if n == 0 {
				return errOnly("%q 装入 hook 运行器失败 (内部状态不一致), 已回滚加载标记。", name), nil
			}
			decls := hooks.HookDeclNames(cfgs)
			t.noteLoaded(ctx, tctx, name, map[string]any{"hooks": decls})
			// 回注里写出**绑定到哪些事件/类型** (不带 target —— command 里常有凭据与
			// 内网地址, 见 hooks.HookSummary 的同一条理由)。技能分支回注正文, 是因为
			// 技能的语义在正文里; hook 没有正文, 声明清单就是它的全部读数 ——
			// 不写出来, 模型无法判断这次加载是否如它所愿 (装错了事件它也看不见)。
			return &tool.ToolResult{Content: fmt.Sprintf(
				"已加载 %s (hook, %d 条: %s): 本会话接下来的相应事件会经过它。pool_release 可卸载 (只影响本会话)。",
				name, n, strings.Join(decls, ", "))}, nil
		}
		if t.pool.IsLoaded(name) {
			// 幂等分支**也要**重新装一次: 加载标记与 Runner 上的组是两处状态, 而
			// 它们可能不同步 (会话重建 Runner 后标记还在)。重装是幂等的 (同名整体
			// 替换), 不重装则会出现"标记说已加载、Runner 上没有"。
			sink.InstallHookGroup(name, cfgs)
			return &tool.ToolResult{Content: fmt.Sprintf("%s 已在加载面, 无需重复加载。", name)}, nil
		}
		return errOnly("%q 加载失败 (内部状态)", name), nil
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
	idx      *poolIndex
}

// NewPoolReleaseTool 构建池卸载工具 (idx 为 nil 时现建, 理由同 NewPoolLoadTool)。
func NewPoolReleaseTool(pool *tool.Pool, skillReg *skills.Registry, idx *poolIndex) *PoolReleaseTool {
	if idx == nil {
		idx = buildPoolIndex(pool, nil, skillReg, nil)
	}
	return &PoolReleaseTool{pool: pool, skillReg: skillReg, idx: idx}
}

// Name 返回工具对外名 "pool_release"。
func (t *PoolReleaseTool) Name() string { return PoolReleaseName }

// Description 返回给模型看的说明: 只收回广告面/卸载 hook, 执行面从不拦截。
func (t *PoolReleaseTool) Description() string {
	return "Release a deferred asset from this session (next turn). Tools/skills: drops them from the advertised surface; execution is never blocked — the tool stays callable by name. Hooks: uninstalls them from this session's hook runner (they stop firing)."
}

// InputSchema 返回入参 JSON Schema (name 必填)。
func (t *PoolReleaseTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Asset name to release."}
		},
		"required": ["name"]
	}`)
}

// IsReadOnly 报只读: 释放只收广告面/卸 hook, 不动磁盘与远端。
func (t *PoolReleaseTool) IsReadOnly(_ json.RawMessage) bool        { return true }
// IsConcurrencySafe 报可并发: 释放是幂等的标记操作。
func (t *PoolReleaseTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
// CheckPermissions 不附加 Deny —— 卸载自己的加载面不需要额外授权。
func (t *PoolReleaseTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 把一件资产移出本会话的加载面; hook 走"真卸载", 工具/技能只收回广告位。
//
// 算法解读：
//
//	原理：工具与技能的"加载"只是广告面的可见性, 所以释放 = 收回标记; hook 的"加载"
//	      是真的把配置装进了 Runner, 所以释放必须**真的卸下**, 否则模型以为关掉了,
//	      实际每次工具调用还在过它。
//	关键不变量：hook 分支里 `released` 与 `uninstalled` 必须分别如实上报 —— 两者本
//	      不该不同步, 一旦只剩一个成立就是状态不一致, 不能糊成一句"已释放"。
//	代价与边界：O(1) 查表 + 一次 UninstallHookGroup; 未加载时是无操作, 不报错。
//	设计权衡：不因"没装 hook sink"而跳过卸载尝试 —— sink 为 nil 时按未卸下报, 让
//	      "运行时不支持 hook"这件事保持可见。
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
	// hook 资产 (21.8.12 P3): 释放必须**真的卸下** —— 与工具/技能不同, hook 的
	// "加载"确实是加载 (装进 Runner 后本会话的相应事件真的会经过它)。只把加载标记
	// 收回而留着 Runner 上的组, 是一条**静默继续运行**的 hook: 模型以为关掉了,
	// 实际每个工具调用还在过它。这是本批最危险的一种"释放"。
	if _, isHook := t.idx.hookCfgs[name]; isHook {
		released := t.pool.Release(name)
		uninstalled := false
		if sink := t.pool.HookSink(); sink != nil {
			uninstalled = sink.UninstallHookGroup(name)
		}
		switch {
		case released && uninstalled:
			return &tool.ToolResult{Content: fmt.Sprintf("已释放 %s (hook): 已从本会话的 hook 运行器卸载, 后续事件不再经过它。", name)}, nil
		case released:
			// 加载标记收回了, 而运行器上没有这组 —— 两者本来就不该不同步, 如实报。
			return &tool.ToolResult{Content: fmt.Sprintf("已收回 %s 的加载标记, 但运行器上并未装着这组 hook (状态不一致)。", name)}, nil
		default:
			return &tool.ToolResult{Content: fmt.Sprintf("%s 不在当前加载面 (未加载或已释放), 无操作。", name)}, nil
		}
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
//  4. hook 入池 (assets 非空时, 21.8.12 P3) —— 只登记 active 且真能解析出 hook 的
//     hook 资产; 与 buildPoolIndex 的 hook 段是一对 (那里只收成员);
//  5. pool.SetAlways(包装三件名) + 注册包装三件本体。
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
// 13.7-P3: assets 可选传 *asset.Set (hook 资产索引); 不传 = 池里不收 hook。
// hook 的运行时装卸口 (HookGroupSink) **不在这里注入** —— 池装配早于 hook
// Runner 构造 (CLI/飞书两条路径都如此), 由装配方随后调 pool.SetHookSink()。
// 没注入时 pool_load 一条 hook 会如实报"运行时不支持", 不静默成功。
func RegisterPoolTools(reg *tool.Registry, skillReg *skills.Registry, assets *asset.Set, denied, allowed map[string]bool, opts ...*PoolPolicy) *tool.Pool {
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
	// hook 入池 (21.8.12 P3「hook 进池」, D3 的落点: G 通电后才做)。
	//
	// 登记条件与 buildPoolIndex 的 hook 段**必须一致** (active + 解析得出 hook):
	// 那里只收成员, 这里只登记收得到的。两处条件若漂开, 表现是"登记了却永远命不中"
	// 或"命中了却被 IsMember 拦下" —— 都只表现为"hook 进池这件事没发生"。
	//
	// 为什么条件要看解析结果而不是只看 kind: 一个只有正文、没写 `hooks:` 段的 hook
	// 资产进池之后, 模型 search 得到、load 成功、而什么都没装上。宁可**不进池**
	// (池里根本没有它 → 模型不会误以为这个能力可用), 也不能进池装不出来。
	if assets != nil {
		for _, name := range assets.Names(asset.KindHook) {
			a := assets.Get(asset.KindHook, name)
			if a == nil || !a.IsActive() {
				continue
			}
			if cfgs, _ := hooks.ParseAssetHooks(a); len(cfgs) > 0 {
				pool.AddMember(name)
			}
		}
	}
	pool.SetAlways(PoolSearchName, PoolLoadName, PoolReleaseName)
	// 13.7.9 池观测: 装配即登记包级最近池 (worker 心跳经 tool.ObservedPool 取数)。
	tool.ObservePool(pool)

	// 索引只建**一次**, 两个工具共用。分建两份的后果不是多算一次 BM25, 而是
	// hookCfgs 变成两份可能不同步的表: search 认得的名字 load 取不到配置
	// (或反过来) —— 而两处都在"能跑"的状态里, 只有那个名字现形。
	idx := buildPoolIndex(pool, reg, skillReg, assets)
	st := NewPoolSearchToolWithIndex(pool, reg, policy, idx)
	st.getenv = os.Getenv
	reg.Register(st)
	reg.Register(NewPoolLoadTool(pool, reg, skillReg, idx, policy))
	reg.Register(NewPoolReleaseTool(pool, skillReg, idx))

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

// countShadowSkills 影子态技能数 (护栏"影子积压"一路的输入; skillReg nil = 0)。
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

// mediaToolNames 第一期下沉的媒体工具名 (GenerateImage/GIF/Video/PPTX/Chart/Speech)。
func mediaToolNames() []string {
	return []string{
		"GenerateImage", "GenerateGIF", "GenerateVideo",
		"GeneratePPTX", "GenerateChart", "GenerateSpeech",
	}
}

// codeIntelToolNames 第一期下沉的 CodeIntel 五件 (init/update/status/query/branch)。
func codeIntelToolNames() []string {
	return []string{
		"code_intel_init", "code_intel_update", "code_intel_status",
		"code_intel_query", "code_intel_branch",
	}
}
