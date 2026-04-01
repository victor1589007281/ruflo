// Package providers 的 manager 子模块实现 ProviderManager。
//
// 设计思路（策略模式 + 故障转移 + 近似负载均衡）：
//   - 各厂商后端实现统一的 LLMProvider 接口，管理器只依赖接口，便于插拔与测试替身。
//   - SelectProvider 按策略在「已注册集合」中选一个后端：轮询保证公平；least-loaded 用原子计数近似
//     在途请求数，选当前最闲节点；cost-based 对每个后端调用 EstimateCost，选估算成本最低者（相对排序用）。
//   - CompleteWithFallback 沿 fallback 链（未配置则用注册顺序）顺序尝试 Complete：任一成功即返回，
//     否则携带最后一次错误，实现链式故障转移。
//   - recordAttempt 聚合各 Provider 的请求次数、Token、估算费用与错误次数，供监控与策略调优。
//
// 本文件实现 ProviderManager：维护名称→实现映射、注册顺序、可选 fallback 链、轮询/最小负载/成本三种选型策略，
// CompleteWithFallback 按链依次尝试并记录请求/Token/错误；并发用原子计数近似负载。
package providers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ruflo/ruflo-go/api"
)

// SelectionStrategy 定义管理器在多个已注册后端之间的选择策略别名。
type SelectionStrategy string

const (
	StrategyRoundRobin  SelectionStrategy = "round-robin"  // 轮询
	StrategyLeastLoaded SelectionStrategy = "least-loaded" // 当前进行中请求数最少
	StrategyCostBased   SelectionStrategy = "cost-based" // EstimateCost 最低
)

// ProviderUsage 按提供方聚合调用次数、Token、估算费用与错误次数（尽力从响应填充）。
type ProviderUsage struct {
	Requests int64   // 总请求次数（含失败）
	Tokens   int64   // 累计 Token（输入+输出或 Total）
	Cost     float64 // 累计估算费用
	Errors   int64   // 失败次数
}

// ProviderManager 线程安全地注册多个 LLMProvider，并支持选型与按链故障转移。
type ProviderManager struct {
	mu         sync.RWMutex              // 保护映射与配置
	byName     map[string]LLMProvider    // 名称到实现
	order      []string                  // 注册顺序（轮询与默认链）
	fallback   []string                  // 显式 fallback 顺序；空则用 order
	load       map[string]*atomic.Int64    // 进行中的 Complete 近似计数
	roundRobin atomic.Uint64             // 轮询游标
	usage      map[string]*ProviderUsage   // 名称到用量聚合
}

// NewProviderManager 创建空管理器。
func NewProviderManager() *ProviderManager {
	return &ProviderManager{
		byName: make(map[string]LLMProvider),
		load:   make(map[string]*atomic.Int64),
		usage:  make(map[string]*ProviderUsage),
	}
}

// ensureUsageLocked 在已持锁下获取或创建某名称的用量结构。
func (m *ProviderManager) ensureUsageLocked(name string) *ProviderUsage {
	u := m.usage[name]
	if u == nil {
		u = &ProviderUsage{}
		m.usage[name] = u
	}
	return u
}

// recordAttempt 记录一次调用：Requests++；若 err!=nil 则 Errors++；否则累加 Token 与 estCost。
func (m *ProviderManager) recordAttempt(name string, resp *api.LLMResponse, err error, estCost float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.ensureUsageLocked(name)
	u.Requests++
	if err != nil {
		u.Errors++
		return
	}
	if resp != nil {
		tok := resp.Usage.TotalTokens
		if tok <= 0 {
			tok = resp.Usage.InputTokens + resp.Usage.OutputTokens
		}
		u.Tokens += int64(tok)
		u.Cost += estCost
	}
}

// Initialize 根据环境变量自动注册 OpenAI、Google、Cohere、RuVector 等（Anthropic 需调用方自行注册）。
func (m *ProviderManager) Initialize() error {
	if k := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); k != "" {
		m.RegisterProvider(&OpenAIProvider{APIKey: k})
	}
	if strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")) != "" {
		m.RegisterProvider(NewGoogleProvider())
	}
	if p := NewCohereProviderFromEnv(); strings.TrimSpace(p.APIKey) != "" {
		m.RegisterProvider(p)
	}
	if strings.TrimSpace(os.Getenv("RUVECTOR_BASE_URL")) != "" {
		m.RegisterProvider(NewRuVectorProviderFromEnv())
	}
	return nil
}

// GetUsage 返回各 Provider 用量结构的浅拷贝快照。
func (m *ProviderManager) GetUsage() map[string]ProviderUsage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]ProviderUsage, len(m.usage))
	for name, u := range m.usage {
		if u == nil {
			continue
		}
		out[name] = ProviderUsage{
			Requests: u.Requests,
			Tokens:   u.Tokens,
			Cost:     u.Cost,
			Errors:   u.Errors,
		}
	}
	return out
}

// ClearCache 清空用量统计并将轮询游标归零。
func (m *ProviderManager) ClearCache() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usage = make(map[string]*ProviderUsage)
	m.roundRobin.Store(0)
}

// Destroy 注销全部 Provider 并清空内部状态（映射、顺序、fallback、负载计数、用量、轮询游标）。
func (m *ProviderManager) Destroy() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byName = make(map[string]LLMProvider)
	m.order = nil
	m.fallback = nil
	m.load = make(map[string]*atomic.Int64)
	m.usage = make(map[string]*ProviderUsage)
	m.roundRobin.Store(0)
}

// RegisterProvider 按 p.Name() 注册或替换实现；新名称追加到 order，并为该名称初始化负载原子量。
func (m *ProviderManager) RegisterProvider(p LLMProvider) {
	if p == nil {
		return
	}
	name := p.Name()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byName[name]; !ok {
		m.order = append(m.order, name)
	}
	m.byName[name] = p
	if m.load[name] == nil {
		m.load[name] = new(atomic.Int64)
	}
}

// GetProvider 按名称返回已注册的 Provider；名称经 TrimSpace 匹配。
func (m *ProviderManager) GetProvider(name string) (LLMProvider, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.byName[strings.TrimSpace(name)]
	return p, ok
}

// ListProviders 返回已注册名称列表，顺序与首次注册顺序一致。
func (m *ProviderManager) ListProviders() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

// SetFallbackChain 设置 CompleteWithFallback 使用的 Provider 名称顺序（链中名称应在后续调用时已注册）。
func (m *ProviderManager) SetFallbackChain(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallback = append([]string(nil), names...)
}

// FallbackChain 返回当前配置的 fallback 顺序副本。
func (m *ProviderManager) FallbackChain() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.fallback...)
}

// SelectProvider 根据 strategy 从已注册后端中选一个；当策略为 cost-based 时用 req 调用各后端的 EstimateCost 比较。
// 算法要点：轮询用原子自增取模 order；least-loaded 遍历 order 比较 load 原子值；cost-based 全量扫描取最小估算成本。
func (m *ProviderManager) SelectProvider(strategy string, req api.LLMRequest) (LLMProvider, error) {
	s := SelectionStrategy(strings.ToLower(strings.TrimSpace(strategy)))
	if s == "" {
		s = StrategyRoundRobin
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.order) == 0 {
		return nil, errors.New("providers: no providers registered")
	}
	switch s {
	case StrategyRoundRobin, "round_robin", "rr":
		i := m.roundRobin.Add(1) - 1
		name := m.order[int(i)%len(m.order)]
		return m.byName[name], nil
	case StrategyLeastLoaded, "least_loaded":
		var best string
		var bestLoad int64 = math.MaxInt64
		for _, name := range m.order {
			var ld int64
			if x := m.load[name]; x != nil {
				ld = x.Load()
			}
			if ld < bestLoad {
				bestLoad, best = ld, name
			}
		}
		return m.byName[best], nil
	case StrategyCostBased, "cost", "cost_based":
		var best LLMProvider
		var bestCost float64 = math.MaxFloat64
		for _, name := range m.order {
			p := m.byName[name]
			c := p.EstimateCost(req)
			if c < bestCost {
				bestCost, best = c, p
			}
		}
		return best, nil
	default:
		return nil, fmt.Errorf("providers: unknown strategy %q", strategy)
	}
}

// CompleteWithFallback 按 FallbackChain（若为空则按注册顺序）依次调用 Complete。
// 算法：对链中每个名称在调用前后对 load 做 +1/-1，避免长时间占用计数；成功则立即返回；失败则记录用量并尝试下一节点。
func (m *ProviderManager) CompleteWithFallback(ctx context.Context, req api.LLMRequest) (*api.LLMResponse, error) {
	m.mu.RLock()
	chain := append([]string(nil), m.fallback...)
	if len(chain) == 0 {
		chain = append([]string(nil), m.order...)
	}
	byName := make(map[string]LLMProvider, len(m.byName))
	for k, v := range m.byName {
		byName[k] = v
	}
	m.mu.RUnlock()

	if len(chain) == 0 {
		return nil, errors.New("providers: no providers registered")
	}

	var lastErr error
	for _, name := range chain {
		p, ok := byName[name]
		if !ok {
			continue
		}
		ld := m.load[name]
		if ld != nil {
			ld.Add(1)
		}
		resp, err := p.Complete(ctx, req)
		if ld != nil {
			ld.Add(-1)
		}
		m.recordAttempt(name, resp, err, p.EstimateCost(req))
		if err == nil && resp != nil {
			return resp, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("providers: fallback exhausted")
}

// HealthCheckAll 对每个已注册 Provider 并发无关地顺序调用 HealthCheck，返回 name→error 映射。
func (m *ProviderManager) HealthCheckAll(ctx context.Context) map[string]error {
	m.mu.RLock()
	names := append([]string(nil), m.order...)
	byName := make(map[string]LLMProvider, len(m.byName))
	for k, v := range m.byName {
		byName[k] = v
	}
	m.mu.RUnlock()

	out := make(map[string]error, len(names))
	for _, name := range names {
		p := byName[name]
		out[name] = p.HealthCheck(ctx)
	}
	return out
}
