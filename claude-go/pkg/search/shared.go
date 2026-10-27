package search

// shared.go —— 可选 Redis 共享缓存的最小存储层。
//
// SEARCH_REDIS_URL 只在这里解析，绝不把 URL 或解析错误写入日志/响应，避免
// URL 中的用户名和密码外泄。Redis 数据键再次哈希内部缓存身份，因此查询、
// 原始问题和 URL 不会以明文出现在 keyspace。
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisKeyPrefix = "claude-go:search"
	defaultRedisTimeout   = 250 * time.Millisecond
	defaultRedisStaleTTL  = time.Hour
	sharedCacheDegraded   = "shared_cache_unavailable"
)

var (
	errSharedRedisUnavailable = errors.New("shared Redis unavailable")
	errSharedRedisConfig      = errors.New("invalid shared Redis configuration")
)

type sharedStore struct {
	client   *redis.Client
	prefix   string
	staleTTL time.Duration
	initErr  error
}

// sharedEntry 用 Redis 服务器时间判断新鲜度；数据键额外保留 staleTTL，
// 使临时抓取故障可以显式使用 stale，而正常读取只看 fresh。
type sharedEntry struct {
	FreshUntil int64           `json:"fresh_until"`
	Payload    json.RawMessage `json:"payload"`
}

type sharedValue struct {
	payload   json.RawMessage
	remaining time.Duration
	found     bool
	fresh     bool
}

func cacheDigest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func newSharedStore(cfg Config) *sharedStore {
	if strings.TrimSpace(cfg.RedisURL) == "" {
		return nil
	}
	s := &sharedStore{prefix: cfg.RedisKeyPrefix, staleTTL: cfg.RedisStaleTTL}
	if s.prefix == "" {
		s.prefix = defaultRedisKeyPrefix
	}
	if s.staleTTL <= 0 {
		s.staleTTL = defaultRedisStaleTTL
	}
	opt, err := redis.ParseURL(strings.TrimSpace(cfg.RedisURL))
	if err != nil {
		// ParseURL 的错误可能回显完整 URL；这里只保留固定、可公开的错误。
		s.initErr = errSharedRedisConfig
		return s
	}
	timeout := cfg.RedisTimeout
	if timeout <= 0 {
		timeout = defaultRedisTimeout
	}
	opt.DialTimeout = timeout
	opt.ReadTimeout = timeout
	opt.WriteTimeout = timeout
	opt.PoolTimeout = timeout
	// 预占和 settle 都不能由客户端盲目重试：响应丢失时结果未知，宁可
	// 保守占用到窗口结束，也不重复记账或重复退款。
	opt.MaxRetries = 0
	s.client = redis.NewClient(opt)
	return s
}

func (s *sharedStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *sharedStore) key(kind, identity string) string {
	return s.prefix + ":" + kind + ":" + cacheDigest(identity)
}

func (s *sharedStore) redisErr(err error) error {
	if err == nil {
		return nil
	}
	if s != nil && s.initErr != nil {
		return s.initErr
	}
	return errSharedRedisUnavailable
}

var sharedPutScript = redis.NewScript(`
local now = redis.call("TIME")
local millis = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
local fresh = millis + tonumber(ARGV[1])
local value = "{\"fresh_until\":" .. fresh .. ",\"payload\":" .. ARGV[3] .. "}"
redis.call("SET", KEYS[1], value, "PX", ARGV[2])
return 1
`)

var sharedGetScript = redis.NewScript(`
local value = redis.call("GET", KEYS[1])
if not value then
    return {0, 0, ""}
end
local now = redis.call("TIME")
local millis = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
return {1, millis, value}
`)

func (s *sharedStore) save(ctx context.Context, kind, identity string, value any, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return errSharedRedisUnavailable
	}
	if s.initErr != nil {
		return s.initErr
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	freshMS := ttl.Milliseconds()
	if freshMS < 1 {
		freshMS = 1
	}
	retention := ttl + s.staleTTL
	if retention <= ttl {
		retention = time.Duration(1<<63 - 1)
	}
	retentionMS := retention.Milliseconds()
	if retentionMS <= freshMS {
		retentionMS = freshMS + 1
	}
	_, err = sharedPutScript.Run(ctx, s.client, []string{s.key(kind, identity)},
		freshMS, retentionMS, string(payload)).Result()
	return s.redisErr(err)
}

func redisInteger(value any) (int64, bool) {
	switch v := value.(type) {
	case int64:
		return v, true
	case string:
		var out int64
		if err := json.Unmarshal([]byte(v), &out); err != nil {
			return 0, false
		}
		return out, true
	default:
		return 0, false
	}
}

func (s *sharedStore) load(ctx context.Context, kind, identity string) (sharedValue, error) {
	if s == nil || s.client == nil {
		return sharedValue{}, errSharedRedisUnavailable
	}
	if s.initErr != nil {
		return sharedValue{}, s.initErr
	}
	parts, err := sharedGetScript.Run(ctx, s.client, []string{s.key(kind, identity)}).Slice()
	if err != nil {
		return sharedValue{}, s.redisErr(err)
	}
	if len(parts) != 3 {
		return sharedValue{}, errSharedRedisUnavailable
	}
	found, ok := redisInteger(parts[0])
	if !ok || found == 0 {
		return sharedValue{}, nil
	}
	now, ok := redisInteger(parts[1])
	if !ok {
		return sharedValue{}, errSharedRedisUnavailable
	}
	raw, ok := parts[2].(string)
	if !ok {
		return sharedValue{}, errSharedRedisUnavailable
	}
	var entry sharedEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil || len(entry.Payload) == 0 {
		return sharedValue{}, errSharedRedisUnavailable
	}
	remaining := time.Duration(entry.FreshUntil-now) * time.Millisecond
	return sharedValue{
		payload: entry.Payload, remaining: remaining,
		found: true, fresh: remaining > 0,
	}, nil
}

func (s *sharedStore) saveSearch(ctx context.Context, identity string, resp SearchResponse, ttl time.Duration) error {
	return s.save(ctx, "cache:search", identity, cloneSearchResponse(resp), ttl)
}

func (s *sharedStore) loadSearch(ctx context.Context, identity string) (SearchResponse, time.Duration, bool, error) {
	value, err := s.load(ctx, "cache:search", identity)
	if err != nil || !value.found || !value.fresh {
		return SearchResponse{}, 0, false, err
	}
	var resp SearchResponse
	if err := json.Unmarshal(value.payload, &resp); err != nil {
		return SearchResponse{}, 0, false, errSharedRedisUnavailable
	}
	return cloneSearchResponse(resp), value.remaining, true, nil
}

func (s *sharedStore) saveDocument(ctx context.Context, identity string, doc *Document, ttl time.Duration) error {
	return s.save(ctx, "cache:fetch", identity, cloneDocument(doc), ttl)
}

func (s *sharedStore) loadDocument(ctx context.Context, identity string) (*Document, time.Duration, bool, bool, error) {
	value, err := s.load(ctx, "cache:fetch", identity)
	if err != nil || !value.found {
		return nil, 0, false, false, err
	}
	var doc *Document
	if err := json.Unmarshal(value.payload, &doc); err != nil || doc == nil {
		return nil, 0, false, false, errSharedRedisUnavailable
	}
	return cloneDocument(doc), value.remaining, true, value.fresh, nil
}
