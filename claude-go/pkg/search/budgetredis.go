package search

// budgetredis.go —— Redis Lua 实现的原子预算账本。
//
// state.used = 已结算实际成本 + 在途预占估算，单位为微美元。窗口起点和
// 过期判断全部使用 Redis TIME；跨窗口 settle 只删除旧 reservation，绝不
// 把旧窗口退款记进新窗口。付费故障时保留预占是最多暂时少花，而不会超支。
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/redis/go-redis/v9"
)

type sharedBudget struct {
	store         *sharedStore
	limitMicros   int64
	windowMS      int64
	reservationMS int64
	initErr       error
}

type budgetReservation struct {
	budget         *sharedBudget
	provider       string
	key            string
	estimateMicros int64
}

func newSharedBudget(cfg Config, store *sharedStore) *sharedBudget {
	if cfg.SharedBudgetLimitUSD == 0 {
		return nil
	}
	b := &sharedBudget{store: store}
	limit, err := usdMicros(cfg.SharedBudgetLimitUSD)
	if err != nil || limit <= 0 {
		b.initErr = errSharedBudgetEstimate
		return b
	}
	b.limitMicros = limit
	window := cfg.SharedBudgetWindow
	if window <= 0 {
		window = defaultSharedBudgetWindow
	}
	reservationTTL := cfg.SharedBudgetReservationTTL
	if reservationTTL <= 0 {
		reservationTTL = defaultSharedBudgetReservationTTL
	}
	b.windowMS = window.Milliseconds()
	b.reservationMS = reservationTTL.Milliseconds()
	if b.windowMS <= 0 || b.reservationMS <= 0 {
		b.initErr = errSharedBudgetEstimate
	}
	return b
}

var budgetReserveScript = redis.NewScript(`
local limit = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local estimate = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])
if not limit or not window or not estimate or not ttl or estimate <= 0 then
    return -1
end
local nowParts = redis.call("TIME")
local now = tonumber(nowParts[1]) * 1000 + math.floor(tonumber(nowParts[2]) / 1000)
local started = tonumber(redis.call("HGET", KEYS[1], "started"))
local used = tonumber(redis.call("HGET", KEYS[1], "used")) or 0
if not started or now < started or now - started >= window then
    started = now
    used = 0
    redis.call("HSET", KEYS[1], "started", started, "used", 0)
    redis.call("PEXPIRE", KEYS[1], window)
end
local existing = redis.call("GET", KEYS[2])
if existing then
    local boundary = string.find(existing, ":", 1, true)
    if boundary and tonumber(string.sub(existing, 1, boundary - 1)) == started then
        return 1
    end
    return -1
end
if used + estimate > limit then
    return 0
end
redis.call("HINCRBY", KEYS[1], "used", estimate)
redis.call("SET", KEYS[2], tostring(started) .. ":" .. tostring(estimate), "PX", ttl)
return 1
`)

var budgetSettleScript = redis.NewScript(`
local raw = redis.call("GET", KEYS[2])
if not raw then
    return 0
end
redis.call("DEL", KEYS[2])
local boundary = string.find(raw, ":", 1, true)
if not boundary then
    return -1
end
local reservedWindow = tonumber(string.sub(raw, 1, boundary - 1))
local estimate = tonumber(string.sub(raw, boundary + 1))
local actual = tonumber(ARGV[1])
if not reservedWindow or not estimate or not actual or actual < 0 then
    return -1
end
local started = tonumber(redis.call("HGET", KEYS[1], "started"))
if started and started == reservedWindow then
    redis.call("HINCRBY", KEYS[1], "used", actual - estimate)
    local used = tonumber(redis.call("HGET", KEYS[1], "used")) or 0
    if used < 0 then
        redis.call("HSET", KEYS[1], "used", 0)
    end
    return 1
end
return 2
`)

func (b *sharedBudget) stateKey() string {
	return b.store.prefix + ":budget:state"
}

func (b *sharedBudget) reserve(ctx context.Context, provider string, estimateUSD float64) (*budgetReservation, error) {
	if b == nil {
		return nil, nil
	}
	if b.initErr != nil {
		return nil, b.initErr
	}
	if b.store == nil || b.store.client == nil {
		return nil, errSharedRedisUnavailable
	}
	if b.store.initErr != nil {
		return nil, b.store.initErr
	}
	estimate, err := usdMicros(estimateUSD)
	if err != nil || estimate <= 0 {
		return nil, errSharedBudgetEstimate
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, errSharedRedisUnavailable
	}
	key := b.store.prefix + ":budget:reservation:" + hex.EncodeToString(id[:])
	code, err := budgetReserveScript.Run(ctx, b.store.client,
		[]string{b.stateKey(), key}, b.limitMicros, b.windowMS, estimate, b.reservationMS).Int()
	if err != nil {
		// 不回显 Redis 错误（可能含部署地址）；也不假定命令没有执行。
		return nil, errSharedRedisUnavailable
	}
	switch code {
	case 1:
		return &budgetReservation{
			budget: b, provider: provider, key: key, estimateMicros: estimate,
		}, nil
	case 0:
		return nil, errSharedBudgetExhausted
	default:
		return nil, errSharedBudgetEstimate
	}
}

func (b *sharedBudget) settle(ctx context.Context, reservation *budgetReservation, actualUSD float64) error {
	if b == nil || reservation == nil || reservation.budget != b {
		return errSharedBudgetEstimate
	}
	if b.store == nil || b.store.client == nil {
		return errSharedRedisUnavailable
	}
	actual, err := usdMicros(actualUSD)
	if err != nil {
		// 无法核验的实际成本按原估算结算，绝不把未知成本当成零成本退款。
		actual = reservation.estimateMicros
	}
	_, err = budgetSettleScript.Run(ctx, b.store.client,
		[]string{b.stateKey(), reservation.key}, actual).Result()
	if err != nil {
		return errSharedRedisUnavailable
	}
	return nil
}

func isBudgetUnavailable(err error) bool {
	return errors.Is(err, errSharedRedisUnavailable) || errors.Is(err, errSharedRedisConfig)
}
