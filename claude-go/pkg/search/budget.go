package search

// budget.go —— provider 调用的共享付费预算边界。
//
// MaxCostUSD 仍是既有 L3 入口闸门；SharedBudgetLimitUSD/Window 是额外的
// 跨 Manager 总量预算，二者语义独立。只有真正进入 provider.Search 前才
// reserve，缓存命中和本地单飞等待都不会经过这里。
import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	defaultSharedBudgetWindow         = time.Hour
	defaultSharedBudgetReservationTTL = 5 * time.Minute
)

var (
	errSharedBudgetExhausted = errors.New("shared paid budget exhausted")
	errSharedBudgetEstimate  = errors.New("paid provider requires a positive cost estimate")
)

// SearchCostReporter 允许付费 provider 报告一次实际调用的成本，包括零结果
// 调用的成本。未实现时，Manager 汇总该次返回结果的 Result.Cost。
type SearchCostReporter interface {
	SearchCostUSD(results []Result) float64
}

// providerActualCost 的第二个返回值表示成本是否可核验。无效 reporter
// 或非法结果成本都是“账单未知”，不能按零成本退款。
func providerActualCost(p Provider, results []Result) (float64, bool) {
	if reporter, ok := p.(SearchCostReporter); ok {
		cost := reporter.SearchCostUSD(results)
		return cost, cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0)
	}
	var cost float64
	for _, result := range results {
		if result.Cost < 0 || math.IsNaN(result.Cost) || math.IsInf(result.Cost, 0) {
			return 0, false
		}
		cost += result.Cost
	}
	return cost, !math.IsInf(cost, 0) && !math.IsNaN(cost)
}

func (m *Manager) providerCostEstimate(p Provider) (float64, bool) {
	estimate := m.cfg.Providers[p.Name()].CostEstimateUSD
	paid := p.Tier() == TierL3 || estimate != 0
	return estimate, paid
}

// callProviderWithBudget 是调度器与 provider 之间唯一的付费接入点。
// 免费源即使 Redis 故障也直接调用；付费源在共享预算启用时必须预占成功。
func (m *Manager) callProviderWithBudget(ctx context.Context, p Provider, req SearchRequest) ([]Result, float64, error) {
	if m.budget == nil {
		results, err := p.Search(ctx, req)
		actual, _ := providerActualCost(p, results)
		return results, actual, err
	}
	estimate, paid := m.providerCostEstimate(p)
	if !paid {
		results, err := p.Search(ctx, req)
		actual, _ := providerActualCost(p, results)
		return results, actual, err
	}
	if estimate <= 0 || math.IsNaN(estimate) || math.IsInf(estimate, 0) {
		return nil, 0, Wrap(p.Name(), ErrInvalid, errSharedBudgetEstimate)
	}
	reservation, err := m.budget.reserve(ctx, p.Name(), estimate)
	if err != nil {
		kind := ErrUnavailable
		if errors.Is(err, errSharedBudgetExhausted) {
			kind = ErrRateLimited
		} else if errors.Is(err, errSharedBudgetEstimate) {
			kind = ErrInvalid
		}
		return nil, 0, Wrap(p.Name(), kind, err)
	}

	results, callErr := p.Search(ctx, req)
	actual, knownCost := providerActualCost(p, results)
	if !knownCost {
		// 成功返回但账单无效时，响应和账本都保留预占估算；绝不把 NaN
		// 传播到 JSON，也不把未知成本伪装成免费调用。
		actual = estimate
		m.metrics.Inc("shared_budget_unknown_cost_total", map[string]string{"provider": p.Name()})
	}
	if callErr == nil {
		m.settleReservation(reservation, actual)
	} else if refundableProviderError(callErr) {
		// 明确拒绝/非法输入没有产生结果，按零成本退款。超时、连接中断、
		// 取消和 panic 的结局未知，保留预占到窗口结束，不轻率退款。
		m.settleReservation(reservation, 0)
	}
	return results, actual, callErr
}

func refundableProviderError(err error) bool {
	switch classifyError(err) {
	case ErrInvalid, ErrUnsupported, ErrRateLimited, ErrBlocked:
		return true
	default:
		return false
	}
}

// isSharedBudgetError 区分共享账本故障与 provider 自身故障。前者不能让
// 本地 quota 把健康 provider 冷却到预算窗口之后，也不能污染 provider 指标。
func isSharedBudgetError(err error) bool {
	return errors.Is(err, errSharedBudgetExhausted) ||
		errors.Is(err, errSharedBudgetEstimate) || isBudgetUnavailable(err)
}

func (m *Manager) settleReservation(reservation *budgetReservation, actualUSD float64) {
	if reservation == nil {
		return
	}
	timeout := m.cfg.RedisTimeout
	if timeout <= 0 {
		timeout = defaultRedisTimeout
	}
	// provider 已成功返回时，即使其 context 刚好到期，也要用有界的新
	// context 完成结算；settle 脚本本身幂等，但这里不重试未知结果。
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := m.budget.settle(ctx, reservation, actualUSD); err != nil {
		m.metrics.Inc("shared_budget_settle_errors_total", map[string]string{
			"provider": reservation.provider,
		})
	}
}

func usdMicros(value float64) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1e9 {
		return 0, fmt.Errorf("invalid USD amount")
	}
	return int64(math.Round(value * 1e6)), nil
}
