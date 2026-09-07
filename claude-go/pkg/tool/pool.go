// pool.go —— 13.7-P0 统一池骨架: 「注册不懒、广告懒」。
//
// 规划锚点 (docforge planning-skills-surge §13.7.2/§13.7.7): 工具/技能全量注册进
// Registry (执行面不拦, 未加载直调按全量语义执行), 只把**广告层**变懒 —— 每请求
// 下发集 = 基础工具(池外) + 包装三件(pool_search/load/release) + 已加载面。
//
// 状态为什么放 pkg/tool: 注册方向是 skills→tool、builtin→skills, pkg/tool 不能
// 引用 skills 类型, 所以 Pool 只存纯字符串状态 (成员/已加载/恒常驻/治理集),
// 资产语义 (tool 的 schema、skill 的正文) 由 builtin 侧的包装三件解释。
//
// 平滑迁移 (§13.7.7 三条): ① 默认关 —— Registry.pool 为 nil 时 FilterAdSurface
// 原样返回, 行为零变化; ② 54 内置全量保留 —— 只有显式登记的池成员才参与懒化;
// ③ 未命中回退 —— 池成员未加载时执行面照常可达 (orchestration 走 reg.Get 全量表)。
//
// 已知边界 (P0 记录): RunIsolated (pkg/engine/runner.go) 的 APITools 消费点暂不
// 接池过滤 —— 隔离执行体拿全量广告面, 行为保守正确。
package tool

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anthropic/claude-go/pkg/types"
)

// EnvToolsPool 池化开关 (仿 weakmodel.Resolve 的 env 先例): 显式设置才开启,
// 默认关 = 行为零变化。装配方 (CLI/feishu) 在注册完成后按它决定是否激活池。
const EnvToolsPool = "CLAUDE_GO_TOOLS_POOL"

// EnvPoolFallback pool_search 未命中回退全量广告的开关 (§13.7.7 平滑迁移②):
// 默认开 (可发现性兜底 —— 未命中时整会话回到池化前行为); 显式 falsy (0/false/
// off/no) 才关。只在池已启用时有意义。
const EnvPoolFallback = "CLAUDE_GO_POOL_FALLBACK"

// PoolFallbackEnabled 解析未命中回退开关。与 PoolEnabled 相反, 默认开:
// 只有显式 falsy 值才关 (回退是保护语义, 不是灰度功能)。
func PoolFallbackEnabled(getenv func(string) string) bool {
	if getenv == nil {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(getenv(EnvPoolFallback))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// PoolEnabled 解析池化开关。只认真值 (1/true/on/yes), 其余 (含未设置) 一律关。
func PoolEnabled(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(getenv(EnvToolsPool))) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// Pool 池状态: 池成员登记 + 已加载面 + 恒常驻名单 + 高后验常驻面 + 治理集。
// 并发安全 (pool_load/release 与 queryLoop 广告过滤跨 goroutine)。
type Pool struct {
	mu      sync.RWMutex
	members []string        // 登记序 (pool_search 结果排序稳定)
	member  map[string]bool // 成员集
	always  map[string]bool // 恒常驻广告面的资产 (包装三件)
	loaded  map[string]bool // 已加载面 (本会话内)
	// resident 高后验常驻面 (13.7-P2): bandit 聚合后验达标的资产, 免 search
	// 直接常驻广告面。由 SetResident (policy 侧聚合判定) 写入, 与 loaded 平级 ——
	// 常驻是"广告面名额"不是"加载状态" (Release 只收回 loaded, 不收回 resident;
	// resident 的降权/退出由后验回落后的下一次聚合自然收回)。
	resident map[string]bool
	// 治理集 (单一真源仍是 engine.Config.toolExposed; NewQueryEngine 时 WireToolPool
	// 拷贝进来, pool_load 用它在加载点 fail-closed)。
	denied  map[string]bool
	allowed map[string]bool // nil = 未设白名单
}

// NewPool 创建空池。
func NewPool() *Pool {
	return &Pool{
		member:   make(map[string]bool),
		always:   make(map[string]bool),
		loaded:   make(map[string]bool),
		resident: make(map[string]bool),
	}
}

// AddMember 登记池成员 (已登记的忽略)。成员在广告面默认**不可见**,
// 直到 pool_load; 未加载时执行面照常可达。
func (p *Pool) AddMember(name string) {
	if name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.member[name] {
		return
	}
	p.member[name] = true
	p.members = append(p.members, name)
}

// SetAlways 标记恒常驻广告面的资产 (包装三件; 也可用于池内但必须始终可见的工具)。
func (p *Pool) SetAlways(names ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range names {
		if n != "" {
			p.always[n] = true
		}
	}
}

// SetGovernance 装配治理集 (与 Config.toolExposed 同语义):
// denied 命中即拒; allowed 非 nil 且不含即拒 (fail-closed, 空集拒一切)。
func (p *Pool) SetGovernance(denied, allowed map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.denied = denied
	p.allowed = allowed
}

// Governed 治理集判定。true = 治理集放行 (可加载/可执行)。
func (p *Pool) Governed(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.denied != nil && p.denied[name] {
		return false
	}
	if p.allowed != nil && !p.allowed[name] {
		return false
	}
	return true
}

// Load 把池成员标记为已加载 (下一轮起进广告面)。
// 返回是否发生状态变化 (重复加载返回 false)。
func (p *Pool) Load(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.member[name] || p.loaded[name] {
		return false
	}
	p.loaded[name] = true
	return true
}

// Release 卸载已加载成员 (下一轮起移出广告面)。执行面不受影响。
func (p *Pool) Release(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loaded[name] {
		return false
	}
	delete(p.loaded, name)
	return true
}

// IsLoaded 报告成员当前是否已加载。
func (p *Pool) IsLoaded(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.loaded[name]
}

// SetResident 更新高后验常驻面 (13.7-P2): 整面替换 (调用方传本轮聚合后的完整
// 名单)。只接受池成员; 治理集外的名字照旧拒绝 —— 常驻是广告位, 不能变成绕过
// AllowedTools 的后门。返回是否发生变化 (供调用方决定是否记日志/落留痕)。
func (p *Pool) SetResident(names []string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	next := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" && p.member[n] && !(p.denied != nil && p.denied[n]) && !(p.allowed != nil && !p.allowed[n]) {
			next[n] = true
		}
	}
	changed := len(next) != len(p.resident)
	if !changed {
		for n := range next {
			if !p.resident[n] {
				changed = true
				break
			}
		}
	}
	if changed {
		p.resident = next
	}
	return changed
}

// ResidentNames 按登记序返回当前常驻面名单。
func (p *Pool) ResidentNames() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.members))
	for _, n := range p.members {
		if p.resident[n] {
			out = append(out, n)
		}
	}
	return out
}

// IsResident 报告 name 是否在高后验常驻面。
func (p *Pool) IsResident(name string) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.resident[name]
}

// IsMember 报告 name 是否池成员。
func (p *Pool) IsMember(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.member[name]
}

// MemberNames 按登记序返回全部成员名。
func (p *Pool) MemberNames() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.members))
	copy(out, p.members)
	return out
}

// Advertised 广告面判定: 恒常驻 ∪ 池外资产(未登记) ∪ 已加载 ∪ 高后验常驻。
// nil 池恒 true (默认关, 行为零变化)。
func (p *Pool) Advertised(name string) bool {
	if p == nil {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.always[name] || !p.member[name] {
		return true
	}
	return p.loaded[name] || p.resident[name]
}

// FilterAPITools 按广告面过滤 API 工具列表 (queryLoop 每轮调用)。
// nil 池原样返回同一切片 (零分配, 行为零变化)。
func (p *Pool) FilterAPITools(tools []types.APITool) []types.APITool {
	if p == nil {
		return tools
	}
	out := tools[:0]
	for _, t := range tools {
		if p.Advertised(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// PoolSnapshot 池状态快照 (观测出口, 13.7.9 池观测链路): 心跳上报与 dashboard
// 取数的只读视图。不携带治理集内部结构 —— 布尔位只回答"是否有白名单在位"。
type PoolSnapshot struct {
	Enabled   bool      `json:"enabled"`   // 池是否已装配 (nil 池 false)
	Members   int       `json:"members"`   // 池成员数 (下沉面)
	Loaded    []string  `json:"loaded"`    // 已加载面 (登记序)
	Resident  []string  `json:"resident"`  // 高后验常驻面 (登记序)
	Always    []string  `json:"always"`    // 恒常驻 (包装三件)
	HaveAllowed bool    `json:"haveAllowed"` // allowed 白名单非 nil
}

// Snapshot 只读快照。nil 池返回 Enabled=false 零值结构 (不 panic, 心跳路径安全)。
func (p *Pool) Snapshot() PoolSnapshot {
	if p == nil {
		return PoolSnapshot{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	s := PoolSnapshot{Members: len(p.members), HaveAllowed: p.allowed != nil}
	for _, n := range p.members {
		switch {
		case p.loaded[n]:
			s.Loaded = append(s.Loaded, n)
		}
		if p.resident[n] {
			s.Resident = append(s.Resident, n)
		}
	}
	s.Enabled = len(p.members) > 0
	for n := range p.always {
		s.Always = append(s.Always, n)
	}
	sort.Strings(s.Always)
	return s
}
// engine.Config.toolExposed 完全一致 (denied 命中即拒; allowed 非 nil 且不含即拒)。
// 引擎侧不感知 pool 的存在 —— Registry 内部即可拿到。
func (r *Registry) WireToolPool(denied, allowed map[string]bool) *Pool {
	p := NewPool()
	p.SetGovernance(denied, allowed)
	r.pool = p
	return p
}

// Pool 返回挂载的池 (未装配为 nil)。
func (r *Registry) Pool() *Pool { return r.pool }

// ---------------------------------------------------------------------------
// 13.7.9 池观测: 包级最近池登记 (worker 心跳上报的解耦通道)
// ---------------------------------------------------------------------------

// 观测依赖方向是 worker→tool (心跳要报池摘要), 而 tool 不能引 worker (反向依赖);
// bot 进程内的会话装配又发生在 tool 包之外。所以用包级 atomic 登记最近装配的池:
// RegisterPoolTools 装配时登记, worker 心跳经 ObservedPool() 取快照 —— 不接线
// bot, 不持引用, nil 默认值即"未观测到池"。
var observedPool atomic.Pointer[Pool]

// ObservePool 登记/更新包级最近池 (RegisterPoolTools 装配路径调用)。
func ObservePool(p *Pool) {
	if p != nil {
		observedPool.Store(p)
	}
}

// ObservedPool 返回包级登记的最近池 (nil = 本进程未装配过池)。
func ObservedPool() *Pool { return observedPool.Load() }

// ---------------------------------------------------------------------------
// 13.7-P2 数量护栏: 软上限
// ---------------------------------------------------------------------------

// 池规模软上限 (§13.7.7「数量护栏」: active 技能 500 / 池内 tool 200; 超限触发
// "晋升进基础面 or 归档"仲裁)。软语义: 不阻塞注册/加载, 只在装配与 search 时
// 告警提示 —— 治理裁决 (skillaudit 晋升/退役) 独立照旧, 护栏不越权。
const (
	MaxPoolTools    = 200
	MaxActiveSkills = 500
	MaxShadowSkills = 500 // shadow 积压同软上限, 超限告警 (审计消费速度不足)
)

// PoolGuardrail 单次护栏检查结果 (供调用方组装告警文案)。
type PoolGuardrail struct {
	Kind     string // "tool" | "skill" | "shadow"
	Current  int
	Limit    int
	Exceeded bool
}

// CheckGuardrails 软上限检查 (超限不拒绝, 返回告警事实由调用方裁决)。
// toolCount = 池内 tool 成员数; skillCount = active 技能数; shadowCount = shadow 积压。
func CheckGuardrails(toolCount, skillCount, shadowCount int) []PoolGuardrail {
	checks := []PoolGuardrail{
		{Kind: "tool", Current: toolCount, Limit: MaxPoolTools},
		{Kind: "skill", Current: skillCount, Limit: MaxActiveSkills},
		{Kind: "shadow", Current: shadowCount, Limit: MaxShadowSkills},
	}
	for i := range checks {
		checks[i].Exceeded = checks[i].Current > checks[i].Limit
	}
	return checks
}
