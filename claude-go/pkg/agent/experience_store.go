// experience_store.go — 经验库的共享存储层。
//
// 背景（2026-11-14）：经验库原来只落 <state>/evolution/experiences.json，整文件覆盖写。
// worker 的 state 卷又按 pod 名做 subPathExpr，rollout 换 pod 名 = 换空目录 = 经验清零；
// 多副本之间也无法共享。这里把经验库改成 **agentDB 按条目 ID 的 KV 存储**：
//
//	PUT /v1/store/{bucket}/{experience_id}   一条经验一个 key（不再整库覆盖）
//	GET /v1/store/{bucket}/keys              列出全部条目
//	GET /v1/store/{bucket}/{id}              读取单条
//	DELETE /v1/store/{bucket}/{id}           物理删除（压实墓碑时用）
//
// 合并语义：
//   - 同 ID 以 UpdatedAt（缺失回退 CreatedAt）较新者为准；相等时远端优先。
//   - 删除写墓碑（deleted=true + updated_at），而不是直接删 key —— 这样持有旧本地
//     缓存的进程不会把已删除条目"复活"；墓碑由 Compact 定期清理。
//   - 写入前先读当前值，旧版本不覆盖新版本（putIfNewer），避免多副本乱序写回退。
//
// 压实：一条经验一个 key，本身没有 append-only 日志的膨胀问题；Compact 只清理
// 超过 TTL 的墓碑。Consolidate 每次最多触发一次（24h 节流）。
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	experienceStoreModeEnv    = "CLAUDE_GO_EXPERIENCE_STORE" // file|agentdb|auto|off
	experienceBucketEnv       = "CLAUDE_GO_EXPERIENCE_BUCKET"
	experienceTombstoneTTLEnv = "CLAUDE_GO_EXPERIENCE_TOMBSTONE_TTL"
	defaultExperienceBucket   = "claude-go-experience"
	defaultTombstoneTTL       = 90 * 24 * time.Hour
	experienceValueLimit      = 8 << 20 // agentDB /v1/store 单值上限
	experienceStoreParallel   = 8
)

// ExperienceTombstone 是一条经验被删除后的墓碑。
type ExperienceTombstone struct {
	ID        string
	UpdatedAt time.Time
}

// ExperienceSnapshot 是 agentDB 中当前的经验库快照。
type ExperienceSnapshot struct {
	Records    map[string]*Experience
	Tombstones map[string]time.Time
}

// LiveCount 返回快照中的有效经验条数。
func (s *ExperienceSnapshot) LiveCount() int {
	if s == nil {
		return 0
	}
	return len(s.Records)
}

// TombstoneCount 返回快照中的墓碑数。
func (s *ExperienceSnapshot) TombstoneCount() int {
	if s == nil {
		return 0
	}
	return len(s.Tombstones)
}

// ExperienceSyncResult 是一次同步的计数。
type ExperienceSyncResult struct {
	Written int // 实际写入（新增或更新）
	Skipped int // 远端已有更新的版本，跳过
	Deleted int // 写入的墓碑数
}

// ExperienceStore 是经验库共享存储的接口。
type ExperienceStore interface {
	Name() string
	Load(ctx context.Context) (*ExperienceSnapshot, error)
	Sync(ctx context.Context, upserts []*Experience, tombstones []ExperienceTombstone) (ExperienceSyncResult, error)
	Compact(ctx context.Context, ttl time.Duration) (int, error)
}

// storedExperience 是 KV 里的值封装。保留 v 便于以后演进格式。
type storedExperience struct {
	V         int         `json:"v"`
	Deleted   bool        `json:"deleted,omitempty"`
	ID        string      `json:"id"`
	UpdatedAt time.Time   `json:"updated_at"`
	Record    *Experience `json:"record,omitempty"`
}

// AgentDBExperienceStore 用 agentDB 的 /v1/store KV 存经验库。
type AgentDBExperienceStore struct {
	baseURL  string
	bucket   string
	client   *http.Client
	parallel int
}

// NewAgentDBExperienceStore 构造一个 agentDB 经验库存储。
func NewAgentDBExperienceStore(baseURL, bucket string) *AgentDBExperienceStore {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if bucket == "" {
		bucket = defaultExperienceBucket
	}
	return &AgentDBExperienceStore{
		baseURL:  baseURL,
		bucket:   bucket,
		client:   &http.Client{Timeout: 10 * time.Second},
		parallel: experienceStoreParallel,
	}
}

// Name 实现 ExperienceStore。
func (s *AgentDBExperienceStore) Name() string { return "agentdb" }

// Bucket 返回当前使用的 bucket 名（CLI/日志用）。
func (s *AgentDBExperienceStore) Bucket() string { return s.bucket }

// Load 读取全部经验与墓碑。任意一个 key 读取失败都整体失败 —— 部分快照会把
// "没读到"误判成"已删除"，宁可 fail-open 回退本地文件。
func (s *AgentDBExperienceStore) Load(ctx context.Context) (*ExperienceSnapshot, error) {
	keys, err := s.listKeys(ctx)
	if err != nil {
		return nil, err
	}
	snap := &ExperienceSnapshot{
		Records:    make(map[string]*Experience, len(keys)),
		Tombstones: make(map[string]time.Time),
	}
	type readResult struct {
		key   string
		raw   []byte
		found bool
		err   error
	}
	results := make([]readResult, len(keys))
	runConcurrent(ctx, s.parallel, len(keys), func(i int) {
		raw, found, err := s.getKey(ctx, keys[i])
		results[i] = readResult{key: keys[i], raw: raw, found: found, err: err}
	})
	for _, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("读取经验 %q 失败: %w", r.key, r.err)
		}
		if !r.found {
			continue
		}
		if env, ok := decodeStoredExperience(r.raw); ok {
			id := env.ID
			if id == "" {
				id = r.key
			}
			if env.Deleted {
				at := env.UpdatedAt
				if prev, exists := snap.Tombstones[id]; !exists || at.After(prev) {
					snap.Tombstones[id] = at
				}
				continue
			}
			if env.Record == nil {
				continue
			}
			if env.Record.ID == "" {
				env.Record.ID = id
			}
			snap.Records[id] = env.Record
			continue
		}
		// 兼容直接存 Experience JSON 的历史值。
		var rec Experience
		if err := json.Unmarshal(r.raw, &rec); err == nil && rec.ID != "" {
			snap.Records[rec.ID] = &rec
			continue
		}
		log.Printf("[Evolution] 经验库跳过无法解析的 key %q", r.key)
	}
	return snap, nil
}

// Sync 写入 upsert 与墓碑。写前读当前版本，旧版本不覆盖新版本。
func (s *AgentDBExperienceStore) Sync(ctx context.Context, upserts []*Experience, tombstones []ExperienceTombstone) (ExperienceSyncResult, error) {
	type job struct {
		id        string
		at        time.Time
		value     []byte
		tombstone bool
	}
	jobs := make([]job, 0, len(upserts)+len(tombstones))
	for _, e := range upserts {
		if e == nil || e.ID == "" {
			continue
		}
		at := e.UpdatedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		raw, err := json.Marshal(storedExperience{V: 1, ID: e.ID, UpdatedAt: at, Record: e})
		if err != nil {
			return ExperienceSyncResult{}, fmt.Errorf("序列化经验 %q 失败: %w", e.ID, err)
		}
		jobs = append(jobs, job{id: e.ID, at: at, value: raw})
	}
	for _, t := range tombstones {
		if t.ID == "" {
			continue
		}
		at := t.UpdatedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		raw, err := json.Marshal(storedExperience{V: 1, Deleted: true, ID: t.ID, UpdatedAt: at})
		if err != nil {
			return ExperienceSyncResult{}, fmt.Errorf("序列化墓碑 %q 失败: %w", t.ID, err)
		}
		jobs = append(jobs, job{id: t.ID, at: at, value: raw, tombstone: true})
	}

	type outcome struct {
		written bool
		err     error
	}
	outs := make([]outcome, len(jobs))
	runConcurrent(ctx, s.parallel, len(jobs), func(i int) {
		written, err := s.putIfNewer(ctx, jobs[i].id, jobs[i].at, jobs[i].value)
		outs[i] = outcome{written: written, err: err}
	})

	var res ExperienceSyncResult
	var errs []string
	for i, o := range outs {
		if o.err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", jobs[i].id, o.err))
			continue
		}
		if !o.written {
			res.Skipped++
			continue
		}
		if jobs[i].tombstone {
			res.Deleted++
		} else {
			res.Written++
		}
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("%d 条经验写入失败: %s", len(errs), strings.Join(errs, "; "))
	}
	return res, nil
}

// Compact 清理超过 ttl 的墓碑。返回物理删除的条数。
func (s *AgentDBExperienceStore) Compact(ctx context.Context, ttl time.Duration) (int, error) {
	if ttl <= 0 {
		ttl = defaultTombstoneTTL
	}
	keys, err := s.listKeys(ctx)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		key string
		at  time.Time
	}
	cands := make([]candidate, len(keys))
	valid := make([]bool, len(keys))
	runConcurrent(ctx, s.parallel, len(keys), func(i int) {
		raw, found, err := s.getKey(ctx, keys[i])
		if err != nil || !found {
			return
		}
		env, ok := decodeStoredExperience(raw)
		if !ok || !env.Deleted {
			return
		}
		cands[i] = candidate{key: keys[i], at: env.UpdatedAt}
		valid[i] = true
	})
	cutoff := time.Now().Add(-ttl)
	var toDelete []string
	for i := range cands {
		if valid[i] && !cands[i].at.IsZero() && cands[i].at.Before(cutoff) {
			toDelete = append(toDelete, cands[i].key)
		}
	}
	type delOutcome struct{ err error }
	outs := make([]delOutcome, len(toDelete))
	runConcurrent(ctx, s.parallel, len(toDelete), func(i int) {
		outs[i] = delOutcome{err: s.deleteKey(ctx, toDelete[i])}
	})
	deleted := 0
	var errs []string
	for i, o := range outs {
		if o.err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", toDelete[i], o.err))
			continue
		}
		deleted++
	}
	if len(errs) > 0 {
		return deleted, fmt.Errorf("%d 条墓碑清理失败: %s", len(errs), strings.Join(errs, "; "))
	}
	return deleted, nil
}

// ─── HTTP 细节 ────────────────────────────────────────────────────────────────

func (s *AgentDBExperienceStore) storeURL(key string) string {
	u := s.baseURL + "/v1/store/" + url.PathEscape(s.bucket)
	if key != "" {
		u += "/" + url.PathEscape(key)
	}
	return u
}

func (s *AgentDBExperienceStore) listKeys(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.storeURL("")+"/keys", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list keys: %s: %s", resp.Status, truncateExperienceLog(string(body), 200))
	}
	var payload struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("list keys 解析失败: %w", err)
	}
	return payload.Keys, nil
}

func (s *AgentDBExperienceStore) getKey(ctx context.Context, key string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.storeURL(key), nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, false, fmt.Errorf("get %q: %s: %s", key, resp.Status, truncateExperienceLog(string(body), 200))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, experienceValueLimit))
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func (s *AgentDBExperienceStore) putKey(ctx context.Context, key string, value []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.storeURL(key), strings.NewReader(string(value)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("put %q: %s: %s", key, resp.Status, truncateExperienceLog(string(body), 200))
	}
	return nil
}

func (s *AgentDBExperienceStore) deleteKey(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.storeURL(key), nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("delete %q: %s: %s", key, resp.Status, truncateExperienceLog(string(body), 200))
	}
	return nil
}

// putIfNewer 先读当前值；远端版本更新则跳过，避免多副本乱序写回退。
func (s *AgentDBExperienceStore) putIfNewer(ctx context.Context, key string, at time.Time, value []byte) (bool, error) {
	raw, found, err := s.getKey(ctx, key)
	if err != nil {
		return false, err
	}
	if found {
		if curAt, ok := storedValueVersion(raw); ok && at.Before(curAt) {
			return false, nil
		}
	}
	return true, s.putKey(ctx, key, value)
}

func decodeStoredExperience(raw []byte) (storedExperience, bool) {
	var env storedExperience
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, false
	}
	if env.Record == nil && !env.Deleted {
		return env, false
	}
	return env, true
}

func storedValueVersion(raw []byte) (time.Time, bool) {
	if env, ok := decodeStoredExperience(raw); ok {
		if !env.UpdatedAt.IsZero() {
			return env.UpdatedAt, true
		}
		if env.Record != nil {
			at, _ := ExperienceVersion(env.Record)
			return at, true
		}
		return time.Time{}, false
	}
	var rec Experience
	if err := json.Unmarshal(raw, &rec); err == nil && rec.ID != "" {
		at, _ := ExperienceVersion(&rec)
		return at, true
	}
	return time.Time{}, false
}

// ─── 合并与版本 ───────────────────────────────────────────────────────────────

// ExperienceFingerprint 返回经验内容的稳定指纹（不含 UpdatedAt，便于判断"内容真的变了"）。
func ExperienceFingerprint(e *Experience) string {
	if e == nil {
		return ""
	}
	cp := *e
	cp.UpdatedAt = time.Time{}
	raw, err := json.Marshal(&cp)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

// ExperienceVersion 返回经验版本：UpdatedAt 优先，缺失回退 CreatedAt；第二返回值是
// 内容指纹，仅用于时间戳相等时的确定性 tie-break。
func ExperienceVersion(e *Experience) (time.Time, string) {
	if e == nil {
		return time.Time{}, ""
	}
	at := e.UpdatedAt
	if at.IsZero() {
		at = e.CreatedAt
	}
	return at, ExperienceFingerprint(e)
}

// mergeExperienceState 把本地经验与 agentDB 快照合并。
//
// 返回：
//   - merged: 合并后的有效经验（已排序）
//   - push:   本地更新/本地独有、需要回推到 agentDB 的条目
//   - synced: 已经在 agentDB 里的条目 ID→指纹（用于初始化 persist 差异基线）
func mergeExperienceState(local []*Experience, remote *ExperienceSnapshot) (merged []*Experience, push []*Experience, synced map[string]string) {
	if remote == nil {
		remote = &ExperienceSnapshot{}
	}
	localMap := make(map[string]*Experience, len(local))
	for _, e := range local {
		if e == nil || e.ID == "" {
			continue
		}
		if prev, ok := localMap[e.ID]; ok {
			pat, ptie := ExperienceVersion(prev)
			at, tie := ExperienceVersion(e)
			if at.After(pat) || (at.Equal(pat) && tie > ptie) {
				localMap[e.ID] = e
			}
			continue
		}
		localMap[e.ID] = e
	}

	ids := make(map[string]struct{}, len(localMap)+len(remote.Records)+len(remote.Tombstones))
	for id := range localMap {
		ids[id] = struct{}{}
	}
	for id := range remote.Records {
		ids[id] = struct{}{}
	}
	for id := range remote.Tombstones {
		ids[id] = struct{}{}
	}

	synced = make(map[string]string, len(remote.Records))
	for id := range ids {
		l := localMap[id]
		r := remote.Records[id]
		tomb, hasTomb := remote.Tombstones[id]

		var lat, rat time.Time
		if l != nil {
			lat, _ = ExperienceVersion(l)
		}
		if r != nil {
			rat, _ = ExperienceVersion(r)
		}
		// 墓碑不早于本地/远端记录时，删除生效（相等也算删除）。
		if hasTomb && (l == nil || !lat.After(tomb)) && (r == nil || !rat.After(tomb)) {
			continue
		}
		switch {
		case r != nil && (l == nil || !lat.After(rat)):
			// 远端不旧于本地：以远端为准（相等时选远端，天然幂等）。
			merged = append(merged, r)
			synced[id] = ExperienceFingerprint(r)
		case l != nil:
			// 本地更新或本地独有：先用本地，稍后回推。
			merged = append(merged, l)
			push = append(push, l)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		if !merged[i].CreatedAt.Equal(merged[j].CreatedAt) {
			return merged[i].CreatedAt.Before(merged[j].CreatedAt)
		}
		return merged[i].ID < merged[j].ID
	})
	return merged, push, synced
}

// ─── 配置 ─────────────────────────────────────────────────────────────────────

// newExperienceStoreFromEnv 按环境变量构造共享存储。默认 file（不改变现有部署行为）；
// 生产 Deployment 显式设 CLAUDE_GO_EXPERIENCE_STORE=agentdb 启用。
func newExperienceStoreFromEnv() ExperienceStore {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(experienceStoreModeEnv)))
	switch mode {
	case "", "file", "off":
		return nil
	case "auto":
		// auto: 配了 agentDB 地址才启用，没配就静默用本地文件。
	case "agentdb":
		// 显式启用，下面没地址会打日志。
	default:
		log.Printf("[Evolution] 未知的 %s=%q，回退本地文件存储", experienceStoreModeEnv, mode)
		return nil
	}

	base := strings.TrimSpace(os.Getenv("CLAUDE_GO_AGENTDB_URL"))
	if base == "" {
		base = strings.TrimSpace(os.Getenv("AGENTDB_URL"))
	}
	if base == "" {
		if mode == "agentdb" {
			log.Printf("[Evolution] %s=agentdb 但未配置 CLAUDE_GO_AGENTDB_URL/AGENTDB_URL，回退本地文件存储", experienceStoreModeEnv)
		}
		return nil
	}
	bucket := strings.TrimSpace(os.Getenv(experienceBucketEnv))
	if bucket == "" {
		bucket = defaultExperienceBucket
	}
	return NewAgentDBExperienceStore(base, bucket)
}

// TombstoneTTLFromEnv 返回墓碑保留时长，默认 90 天。
func TombstoneTTLFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(experienceTombstoneTTLEnv))
	if raw == "" {
		return defaultTombstoneTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultTombstoneTTL
	}
	return d
}

// ─── 小工具 ───────────────────────────────────────────────────────────────────

func runConcurrent(ctx context.Context, parallel, n int, fn func(int)) {
	if n <= 0 {
		return
	}
	if parallel <= 1 {
		for i := 0; i < n; i++ {
			if ctx.Err() != nil {
				return
			}
			fn(i)
		}
		return
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

func truncateExperienceLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
