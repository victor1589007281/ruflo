package swarm_intel

// v3 Phase 0: SwarmStore 持久化抽象层。
//
// 动机: v2 的 store.go 是 JSONL 孤岛 —— PheromoneStore/PredictionHistory/ReasoningBank
// 直接绑死本地文件, 引擎无法接入分布式后端 (agentDB serve)。本文件把五类持久化操作
// 收敛成 SwarmStore 接口, JSONLStore 保留 v2 行为作为默认后端, AgentDBStore 是
// 对接 pkg/agentdbclient + pkg/agentdbsemantic 的集群后端骨架。
//
// 注意: 接口刻意只增不减 —— v2 的既有类型 (PheromoneTrail/PredictionRecord/
// ReasoningEntry) 被原样复用, 不重造数据结构。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agentdbclient"
	"github.com/anthropic/claude-go/pkg/agentdbsemantic"
)

// ── 接口定义 ──

// SwarmStore 群体智能五类持久化操作的统一抽象。
//
// 实现约定:
//   - 信素按 domain 分桶, 同一 HypothesisID 后写覆盖先读 (读侧去重);
//   - 预测历史/推理库是只追加日志, 已决记录可由可选接口 predictionResolver 原地回填;
//   - 学习状态整体读写 (体量小, 直接全量快照);
//   - 模拟事件按 runID 过滤读取, 供 Phase 2 的模拟回放/审计。
type SwarmStore interface {
	// 信素
	SaveTrails(domain string, trails []*PheromoneTrail) error
	LoadTrails(domain string) ([]*PheromoneTrail, error)

	// 预测历史
	AppendPrediction(rec *PredictionRecord) error
	RecentPredictions(n int) ([]*PredictionRecord, error)
	// FindSimilar 按语义相关性检索历史预测; P0 为关键词回退, 向量检索见实现内 TODO。
	FindSimilar(question string, limit int) ([]*PredictionRecord, error)

	// 推理库
	StoreReasoning(entry *ReasoningEntry) error
	// SearchReasonings 按语义相关性检索推理路径; P0 为关键词回退, 向量检索见实现内 TODO。
	SearchReasonings(query string, limit int) ([]*ReasoningEntry, error)

	// 学习状态
	SaveLearning(st *LearningState) error
	LoadLearning() (*LearningState, error)

	// 模拟事件
	AppendSimEvent(ev *SimEvent) error
	SimEvents(runID string) ([]*SimEvent, error)

	// 跨会话反思 (W1 Phase 3, tau2 式): RecordOutcome/RegisterResolution 落结构化反思,
	// Predict 检索同域历史反思注入先验 (向量优先, 关键词回退, 同 Filtered ANN 哲学)。
	AppendReflection(rec *ReflectionRecord) error
	SearchReflections(query string, limit int) ([]*ReflectionRecord, error)

	// 信素语义化 (W1 Phase 3): 跨域近似先验检索 —— 不再要求 domain 精确相等,
	// 按问题语义找历史上"近似域"的高强度信素 (向量优先, 关键词回退)。
	FindTrails(query string, limit int) ([]*PheromoneTrail, error)
}

// predictionResolver 可选接口: 后端支持按 ID 原地回填"已决结局"时实现。
// JSONLStore 已实现 (重写 history.jsonl); AgentDBStore 的日志桶是追加型的,
// 骨架阶段返回未实现错误, 调用方据此退回"追加一条 resolved 记录"的兼容路径。
type predictionResolver interface {
	ResolvePrediction(id, actual string, brier float64, resolvedAt time.Time) error
}

// SimEvent 模拟过程事件 (v3 Phase 0 统一类型, 供 social 内核与 Phase 2 回放/审计共用)。
//
// W1/W2 合并说明: 本定义吸收 social_kernel.go 的本地 SimEvent (字段形状保持不变,
// Payload 承载结构化负载), 另加 CreatedAt 由存储层写入时补戳; social_kernel.go 中
// 的同名本地定义与 TODO 已随合并删除。内核现有的 jsonlSimEventRecorder 继续可用,
// 长期统一走 SwarmStore.AppendSimEvent/SimEvents。
type SimEvent struct {
	RunID     string          `json:"run_id"`               // 模拟运行 ID
	Round     int             `json:"round"`                // 事件所在轮次 (系统事件为 0)
	Type      string          `json:"type"`                 // stance | emergence | action | ...
	AgentID   string          `json:"agent_id,omitempty"`   // 产生事件的 agent (系统事件留空)
	Payload   json.RawMessage `json:"payload"`              // 结构化事件负载
	CreatedAt time.Time       `json:"created_at,omitempty"` // 存储层写入时补戳
}

// ── JSONL 后端 (默认, v2 行为保留) ──

// JSONLStore 基于本地 JSONL 文件的 SwarmStore 实现 (v2 默认后端)。
// 信素/预测历史/推理库直接复用 store.go 的 v2 组件, 学习状态与模拟事件
// 沿用同一目录布局, 与 v2 落盘文件逐字节兼容。
type JSONLStore struct {
	trails  *PheromoneStore
	history *PredictionHistory
	bank    *ReasoningBank

	dir        string // base dir (cfg.DataDir)
	learningMu sync.Mutex
	simMu      sync.Mutex
}

// 编译期断言: JSONLStore 满足接口。
var _ SwarmStore = (*JSONLStore)(nil)
var _ predictionResolver = (*JSONLStore)(nil)

// NewJSONLStore 创建 JSONL 后端; dir 布局与 v2 完全一致:
// <dir>/pheromones/*.jsonl, <dir>/predictions/history.jsonl,
// <dir>/learning/reasoning_bank.jsonl + learning_state.json, <dir>/simulation/sim_events.jsonl。
func NewJSONLStore(dir string) (*JSONLStore, error) {
	trails, err := NewPheromoneStore(dir)
	if err != nil {
		return nil, err
	}
	history, err := NewPredictionHistory(dir)
	if err != nil {
		return nil, err
	}
	bank, err := NewReasoningBank(dir)
	if err != nil {
		return nil, err
	}
	return &JSONLStore{trails: trails, history: history, bank: bank, dir: dir}, nil
}

// SaveTrails 持久化信素快照 (委托 v2 PheromoneStore)。
func (s *JSONLStore) SaveTrails(domain string, trails []*PheromoneTrail) error {
	return s.trails.Save(domain, trails)
}

// LoadTrails 加载领域历史信素 (委托 v2 PheromoneStore)。
func (s *JSONLStore) LoadTrails(domain string) ([]*PheromoneTrail, error) {
	return s.trails.Load(domain)
}

// AppendPrediction 追加一条预测记录 (委托 v2 PredictionHistory)。
func (s *JSONLStore) AppendPrediction(rec *PredictionRecord) error {
	return s.history.Append(rec)
}

// RecentPredictions 取最近 N 条预测 (委托 v2 PredictionHistory)。
// 注意 v2 Recent 把 n 当"取末尾 n 条" (n=0 是空集); 接口语义是 n<=0 不限条数,
// 故用最大上限代取全量, 文件读取路径与 v2 保持逐字节一致。
func (s *JSONLStore) RecentPredictions(n int) ([]*PredictionRecord, error) {
	if n <= 0 {
		n = int(^uint(0) >> 1)
	}
	return s.history.Recent(n)
}

// scoredPrediction 带相关度分数的预测记录, FindSimilar 两后端共用。
type scoredPrediction struct {
	rec   *PredictionRecord
	score float64
}

// FindSimilar 关键词重叠检索相似历史预测。
// TODO(v3): 接入 agentDB Filtered ANN 后替换为向量检索 (与 AgentDBStore 同一升级路径)。
func (s *JSONLStore) FindSimilar(question string, limit int) ([]*PredictionRecord, error) {
	s.history.mu.Lock()
	defer s.history.mu.Unlock()
	data, err := os.ReadFile(s.history.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	words := strings.Fields(strings.ToLower(question))
	var candidates []scoredPrediction
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec PredictionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if score := keywordScore(words, strings.ToLower(rec.Question)); score > 0.3 {
			candidates = append(candidates, scoredPrediction{rec: &rec, score: score})
		}
	}
	return topPredictions(candidates, limit), nil
}

// topPredictions 把打分候选按相关度降序裁到 limit 条 (limit<=0 表示不限)。
func topPredictions(candidates []scoredPrediction, limit int) []*PredictionRecord {
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	var result []*PredictionRecord
	for i, c := range candidates {
		if limit > 0 && i >= limit {
			break
		}
		result = append(result, c.rec)
	}
	return result
}

// StoreReasoning 存储一条推理路径 (委托 v2 ReasoningBank)。
func (s *JSONLStore) StoreReasoning(entry *ReasoningEntry) error {
	return s.bank.Store(entry)
}

// SearchReasonings 关键词重叠检索相似推理路径 (委托 v2 ReasoningBank)。
func (s *JSONLStore) SearchReasonings(query string, limit int) ([]*ReasoningEntry, error) {
	return s.bank.FindSimilar(query, limit)
}

// learningPath 学习状态落盘路径 (与 v2 learning.go 完全一致)。
func (s *JSONLStore) learningPath() string {
	return filepath.Join(s.dir, "learning", "learning_state.json")
}

// SaveLearning 原子写学习状态 (tmp+rename, 与 v2 相同)。
func (s *JSONLStore) SaveLearning(st *LearningState) error {
	s.learningMu.Lock()
	defer s.learningMu.Unlock()
	p := s.learningPath()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LoadLearning 读取学习状态; 缺失/损坏返回 (nil, nil) —— 调用方据此安全跳过恢复。
func (s *JSONLStore) LoadLearning() (*LearningState, error) {
	s.learningMu.Lock()
	defer s.learningMu.Unlock()
	data, err := os.ReadFile(s.learningPath())
	if err != nil {
		return nil, nil //nolint:nilerr // 缺失即无历史, 不是错误
	}
	var st LearningState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, nil //nolint:nilerr // 损坏文件按无历史处理, 与 v2 容错语义一致
	}
	return &st, nil
}

// simEventsPath 模拟事件日志路径。
func (s *JSONLStore) simEventsPath() string {
	return filepath.Join(s.dir, "simulation", "sim_events.jsonl")
}

// AppendSimEvent 追加一条模拟事件; CreatedAt 为零值时由存储层补戳。
func (s *JSONLStore) AppendSimEvent(ev *SimEvent) error {
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	s.simMu.Lock()
	defer s.simMu.Unlock()
	p := s.simEventsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(ev)
}

// SimEvents 读取某次模拟运行的全部事件 (按 runID 过滤; 文件按追加序即时间序)。
func (s *JSONLStore) SimEvents(runID string) ([]*SimEvent, error) {
	s.simMu.Lock()
	defer s.simMu.Unlock()
	data, err := os.ReadFile(s.simEventsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var events []*SimEvent
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev SimEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.RunID == runID {
			events = append(events, &ev)
		}
	}
	return events, nil
}

// ResolvePrediction 按 ID 原地回填已决结局: 重写 history.jsonl 中匹配的记录
// (标 Resolved/ActualLabel/BrierScore/ResolvedAt)。找不到 ID 返回错误。
func (s *JSONLStore) ResolvePrediction(id, actual string, brier float64, resolvedAt time.Time) error {

	s.history.mu.Lock()
	defer s.history.mu.Unlock()
	data, err := os.ReadFile(s.history.file)
	if err != nil {
		return err
	}
	var lines []string
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var rec PredictionRecord
		if err := json.Unmarshal([]byte(trimmed), &rec); err == nil && rec.ID == id {
			rec.ActualLabel = actual
			rec.BrierScore = brier
			rec.Resolved = true
			rec.ResolvedAt = &resolvedAt
			if updated, err := json.Marshal(&rec); err == nil {
				trimmed = string(updated)
			}
			found = true
		}
		lines = append(lines, trimmed)
	}
	if !found {
		return fmt.Errorf("prediction %s not found", id)
	}
	tmp := s.history.file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.history.file)
}

// ── JSONL 后端: W1 Phase 3 (跨会话反思 + 信素语义化) ──

// reflectionsPath 跨会话反思日志路径 (与 reasoning_bank/learning_state 同目录布局)。
func (s *JSONLStore) reflectionsPath() string {
	return filepath.Join(s.dir, "learning", "reflections.jsonl")
}

// AppendReflection 追加一条结构化反思 (append-only, 与预测历史同哲学)。
func (s *JSONLStore) AppendReflection(rec *ReflectionRecord) error {
	s.learningMu.Lock()
	defer s.learningMu.Unlock()
	p := s.reflectionsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(rec)
}

// SearchReflections 关键词重叠检索历史反思 (打分区 = Question + Lesson)。
func (s *JSONLStore) SearchReflections(query string, limit int) ([]*ReflectionRecord, error) {
	s.learningMu.Lock()
	defer s.learningMu.Unlock()
	data, err := os.ReadFile(s.reflectionsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	words := strings.Fields(strings.ToLower(query))
	type scored struct {
		rec   *ReflectionRecord
		score float64
	}
	var candidates []scored
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec ReflectionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		text := strings.ToLower(rec.Question + " " + rec.Lesson)
		if score := keywordScore(words, text); score > 0.3 {
			candidates = append(candidates, scored{rec: &rec, score: score})
		}
	}
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	var result []*ReflectionRecord
	for i, c := range candidates {
		if limit > 0 && i >= limit {
			break
		}
		result = append(result, c.rec)
	}
	return result, nil
}

// FindTrails 跨域关键词检索信素先验: 扫描全部域文件, 按 Hypothesis+Evidence
// 与问题的重叠率打分, 返回 top-N (近似域先验的 P0 形态; 向量版见 AgentDBStore)。
func (s *JSONLStore) FindTrails(query string, limit int) ([]*PheromoneTrail, error) {
	entries, err := os.ReadDir(s.trails.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	words := strings.Fields(strings.ToLower(query))
	type scored struct {
		trail *PheromoneTrail
		score float64
	}
	var candidates []scored
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.trails.dir, entry.Name()))
		if err != nil {
			continue
		}
		seen := make(map[string]bool) // 域文件内同一 HypothesisID 只计最后一次
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var t PheromoneTrail
			if err := json.Unmarshal([]byte(line), &t); err != nil {
				continue
			}
			if seen[t.HypothesisID] {
				continue
			}
			seen[t.HypothesisID] = true
			text := strings.ToLower(t.Hypothesis + " " + strings.Join(t.Evidence, " "))
			if score := keywordScore(words, text); score > 0.3 {
				candidates = append(candidates, scored{trail: &t, score: score})
			}
		}
	}
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	var result []*PheromoneTrail
	for i, c := range candidates {
		if limit > 0 && i >= limit {
			break
		}
		result = append(result, c.trail)
	}
	return result, nil
}

// ── agentDB 后端 (集群骨架) ──
// AgentDBStore 基于 agentDB serve (HTTP 数据面) 的 SwarmStore 实现。
//
// 键值映射策略 (P0):
//   - 信素: KV bucket, key=domain (快照整存整取, 即 batch 语义);
//   - 学习状态: KV bucket, key="state" (全量快照);
//   - 模拟事件: Log bucket 逐条追加, 读全量后按 runID 过滤;
//   - 预测历史/推理库: Log bucket 逐条追加 + Memory 向量桶双写 (Filtered ANN)。
//
// A7 命名空间硬隔离: 桶名加 namespace 前缀 (`<ns>_swarm_intel_*`)。空 ns 用
// `default`。多 ns 部署 (env AGENTDB_NAMESPACE=swarm) 互不污染。
//
// Filtered ANN (v3 Phase 1):
//   - 写入: AppendPrediction/StoreReasoning 同步写 log 桶 (v2 兼容) + Memory 向量桶
//     (Filtered ANN 索引); 向量桶写失败仅 warn, 主写路径 (log 桶) 不回滚。
//   - 检索: FindSimilar/SearchReasonings 优先走 Memory.SearchMemory 向量化命中,
//     向量端点不可达或零命中时回退关键词重叠 (与 P0 行为一致)。
//   - 接口签名不变, 调用方零改动。
type AgentDBStore struct {
	cli *agentdbclient.Client
	sem *agentdbsemantic.Client // Filtered ANN: Memory 端点客户端

	baseURL string
	ns      string // A7 命名空间前缀 (空 = "default")
	bkt     agentDBBuckets
}

// agentDBBuckets 按 ns 前缀构造的实际桶名 (A7 硬隔离)。
type agentDBBuckets struct {
	trails      string
	learning    string
	predict     string
	reason      string
	sim         string
	reflections string // A6 事件流名 (非 agentstore 桶, 同样吃 ns 前缀做隔离)
}

// newAgentDBBuckets 由 ns 构造五类桶名; ns 为空时回落 "default"。
func newAgentDBBuckets(ns string) agentDBBuckets {
	if ns == "" {
		ns = "default"
	}
	return agentDBBuckets{
		trails:      ns + "_swarm_intel_trails",
		learning:    ns + "_swarm_intel_learning",
		predict:     ns + "_swarm_intel_predictions",
		reason:      ns + "_swarm_intel_reasonings",
		sim:         ns + "_swarm_intel_sim_events",
		reflections: ns + "_swarm_intel_reflections",
	}
}

// 编译期断言: AgentDBStore 满足接口。
var _ SwarmStore = (*AgentDBStore)(nil)

// NewAgentDBStore 创建 agentDB 后端; 构造时做一次可达性探针 (KV.Keys),
// 失败返回错误 —— 调用方 (NewEngine) 负责回退 JSONL 并告警, 不 crash。
//
// A7: namespace 从 env AGENTDB_NAMESPACE 读取 (Config.AgentDBNamespace 留作后续显式传参口)。
// 同一 ns 内多进程共享桶 (serve 串行化); 不同 ns 完全隔离 (桶名前缀不同)。
func NewAgentDBStore(baseURL string) (*AgentDBStore, error) {
	ns := strings.TrimSpace(os.Getenv("AGENTDB_NAMESPACE"))
	bkt := newAgentDBBuckets(ns)
	cli := agentdbclient.New(baseURL)
	if _, err := cli.KV(bkt.learning).Keys(); err != nil {
		return nil, fmt.Errorf("agentDB 不可达 (%s): %w", baseURL, err)
	}
	return &AgentDBStore{
		cli:     cli,
		sem:     agentdbsemantic.New(baseURL),
		baseURL: baseURL,
		ns:      ns,
		bkt:     bkt,
	}, nil
}

// SaveTrails 把领域信素快照整存到 KV (batch 写) + Memory 向量桶双写 (信素语义化)。
// 向量索引失败仅 warn: FindTrails 会自动回退跨域关键词扫描, 不影响主存储。
func (s *AgentDBStore) SaveTrails(domain string, trails []*PheromoneTrail) error {
	if err := s.cli.KV(s.bkt.trails).Put(safeDomainKey(domain), trails); err != nil {
		return err
	}
	s.indexTrailMemory(domain, trails)
	return nil
}

// indexTrailMemory 把信素假设逐条索引进 Memory 向量桶 (信素语义化写入侧)。
// SessionID = HypothesisID (跨域唯一, 用作向量命中 → 信素记录的重联键);
// Tags 带域键, 供未来 serve 侧 tag-filter 检索预留 (当前客户端命中后重联)。
func (s *AgentDBStore) indexTrailMemory(domain string, trails []*PheromoneTrail) {
	if s.sem == nil {
		return
	}
	for _, t := range trails {
		if t == nil || t.Hypothesis == "" {
			continue
		}
		content := t.Hypothesis
		if len(t.Evidence) > 0 {
			content = t.Hypothesis + " | " + strings.Join(t.Evidence, " / ")
		}
		if _, err := s.sem.StoreMemory(context.Background(), agentdbsemantic.Memory{
			ID:        t.HypothesisID, // MemoryHit.ID 返回记录 ID, 用作向量命中→信素重联键
			Content:   content,
			Type:      agentdbsemantic.MemorySemantic,
			SessionID: t.HypothesisID,
			Tags:      []string{"swarm_trail", safeDomainKey(domain), t.HypothesisID},
		}); err != nil {
			log.Printf("[swarm_intel] WARN Memory 向量索引失败 (trail %s): %v", t.HypothesisID, err)
		}
	}
}

// LoadTrails 读取领域信素快照; 键不存在返回空。
func (s *AgentDBStore) LoadTrails(domain string) ([]*PheromoneTrail, error) {
	var trails []*PheromoneTrail
	ok, err := s.cli.KV(s.bkt.trails).Get(safeDomainKey(domain), &trails)
	if err != nil || !ok {
		return nil, err
	}
	return trails, nil
}

// AppendPrediction 追加一条预测记录 (log 桶 + Memory 向量桶双写)。
// Memory 写失败仅 warn 不回滚 (向量检索是增强, 主存储是 log 桶)。
func (s *AgentDBStore) AppendPrediction(rec *PredictionRecord) error {
	if err := s.cli.Log(s.bkt.predict).Append(rec); err != nil {
		return err
	}
	s.indexPredictionMemory(rec)
	return nil
}

// indexPredictionMemory 把预测记录索引进 Memory 向量桶 (Filtered ANN 写入侧)。
// 向量桶索引失败仅 warn: 检索侧会自动回退到关键词重叠, 不影响主存储。
func (s *AgentDBStore) indexPredictionMemory(rec *PredictionRecord) {
	if s.sem == nil || rec == nil || rec.Question == "" {
		return
	}
	content := rec.Question
	if len(rec.Outcomes) > 0 {
		outcomes := make([]string, 0, len(rec.Outcomes))
		for _, o := range rec.Outcomes {
			outcomes = append(outcomes, o.Outcome)
		}
		content = rec.Question + " | " + strings.Join(outcomes, " / ")
	}
	if _, err := s.sem.StoreMemory(context.Background(), agentdbsemantic.Memory{
		ID:        rec.ID, // MemoryHit.ID 返回记录 ID, 用作向量命中→预测记录重联键
		Content:   content,
		Type:      agentdbsemantic.MemorySemantic,
		SessionID: rec.ID,
		Tags:      []string{"swarm_predict", rec.ID},
	}); err != nil {
		log.Printf("[swarm_intel] WARN Memory 向量索引失败 (prediction %s): %v", rec.ID, err)
	}
}

// RecentPredictions 读全量日志后取末尾 N 条 (追加序即时间序)。
func (s *AgentDBStore) RecentPredictions(n int) ([]*PredictionRecord, error) {
	all, err := s.readAllPredictions()
	if err != nil {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// readAllPredictions 拉取预测日志全部记录; 桶不存在按空处理 (追加型后端的常态)。
func (s *AgentDBStore) readAllPredictions() ([]*PredictionRecord, error) {
	var all []*PredictionRecord
	err := s.cli.Log(s.bkt.predict).ReadAll(func(line []byte) error {
		var rec PredictionRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil // 跳过坏行, 与 JSONL 后端宽容语义一致
		}
		all = append(all, &rec)
		return nil
	})
	if err != nil {
		if isNotFoundErr(err) {
			return nil, nil
		}
		return nil, err
	}
	return all, nil
}

// scoredReasoning 带相关度分数的推理路径, SearchReasonings 后端内排序用。
type scoredReasoning struct {
	entry *ReasoningEntry
	score float64
}

// FindSimilar 语义向量检索相似历史预测 (Filtered ANN)。
// 优先走 Memory.SearchMemory 向量化命中 (服务端嵌入 question+outcomes); 不可达或
// 零命中时回退关键词重叠 (与 P0 行为一致, 保证向量桶冷启动时仍可工作)。
func (s *AgentDBStore) FindSimilar(question string, limit int) ([]*PredictionRecord, error) {
	// 先取向量命中 (limit*4 留余量: Memory 桶是全局混部,
	// 预测/推理/信素/反思共用, 非本类命中重联时为 nil 仍占窗口位,
	// 窗口过窄会把真正同类的命中挤出 —— 服务端有 tag filter 后再收紧)
	hits, err := s.sem.SearchMemory(context.Background(), question, limit*4, 0)
	if err == nil && len(hits) > 0 {
		// 按 score 排序取 top-N
		sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		var result []*PredictionRecord
		for i, h := range hits {
			if limit > 0 && i >= limit {
				break
			}
			if rec := s.findPredictionByID(h.ID); rec != nil {
				result = append(result, rec)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}
	// 向量未命中或失败: 回退关键词重叠 (P0 行为)
	all, err := s.readAllPredictions()
	if err != nil {
		return nil, err
	}
	words := strings.Fields(strings.ToLower(question))
	var candidates []scoredPrediction
	for _, rec := range all {
		if score := keywordScore(words, strings.ToLower(rec.Question)); score > 0.3 {
			candidates = append(candidates, scoredPrediction{rec: rec, score: score})
		}
	}
	return topPredictions(candidates, limit), nil
}

// findPredictionByID 在 log 桶中按 ID 精确匹配 (Memory 命中 → 完整记录)。
// 找不到返回 nil (向量桶和 log 桶可能有短暂不一致, 容忍)。
func (s *AgentDBStore) findPredictionByID(id string) *PredictionRecord {
	if id == "" {
		return nil
	}
	all, err := s.readAllPredictions()
	if err != nil {
		return nil
	}
	for _, rec := range all {
		if rec.ID == id {
			return rec
		}
	}
	return nil
}

// StoreReasoning 存储一条推理路径 (log 桶 + Memory 向量桶双写)。
func (s *AgentDBStore) StoreReasoning(entry *ReasoningEntry) error {
	if err := s.cli.Log(s.bkt.reason).Append(entry); err != nil {
		return err
	}
	s.indexReasoningMemory(entry)
	return nil
}

// indexReasoningMemory 把推理路径索引进 Memory 向量桶 (Filtered ANN 写入侧)。
func (s *AgentDBStore) indexReasoningMemory(entry *ReasoningEntry) {
	if s.sem == nil || entry == nil || entry.Question == "" {
		return
	}
	content := entry.Question
	if entry.Reasoning != "" {
		content = entry.Question + " | " + entry.Reasoning
	}
	if _, err := s.sem.StoreMemory(context.Background(), agentdbsemantic.Memory{
		ID:        entry.Question, // 用 question 做 ID=去重键 (重复推理覆盖) + 向量命中重联键
		Content:   content,
		Type:      agentdbsemantic.MemorySemantic,
		SessionID: entry.Question,
		Tags:      []string{"swarm_reasoning", entry.Question},
	}); err != nil {
		log.Printf("[swarm_intel] WARN Memory 向量索引失败 (reasoning): %v", err)
	}
}

// SearchReasonings 语义向量检索相似推理路径 (Filtered ANN)。
// 优先走 Memory.SearchMemory; 不可达或零命中时回退关键词重叠 (与 P0 行为一致)。
func (s *AgentDBStore) SearchReasonings(query string, limit int) ([]*ReasoningEntry, error) {
	hits, err := s.sem.SearchMemory(context.Background(), query, limit*4, 0)
	if err == nil && len(hits) > 0 {
		sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		var result []*ReasoningEntry
		for i, h := range hits {
			if limit > 0 && i >= limit {
				break
			}
			if entry := s.findReasoningByQuestion(h.ID); entry != nil {
				result = append(result, entry)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}
	// 向量未命中或失败: 回退关键词重叠 (P0 行为)
	var all []*ReasoningEntry
	err = s.cli.Log(s.bkt.reason).ReadAll(func(line []byte) error {
		var entry ReasoningEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil
		}
		all = append(all, &entry)
		return nil
	})
	if err != nil {
		if isNotFoundErr(err) {
			return nil, nil
		}
		return nil, err
	}
	words := strings.Fields(strings.ToLower(query))
	var candidates []scoredReasoning
	for _, entry := range all {
		if score := keywordScore(words, strings.ToLower(entry.Question)); score > 0.3 {
			candidates = append(candidates, scoredReasoning{entry: entry, score: score})
		}
	}
	var result []*ReasoningEntry
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	for i, c := range candidates {
		if limit > 0 && i >= limit {
			break
		}
		result = append(result, c.entry)
	}
	return result, nil
}

// findReasoningByQuestion 在 log 桶中按 question 精确匹配 (Memory 命中 → 完整记录)。
func (s *AgentDBStore) findReasoningByQuestion(question string) *ReasoningEntry {
	if question == "" {
		return nil
	}
	var found *ReasoningEntry
	_ = s.cli.Log(s.bkt.reason).ReadAll(func(line []byte) error {
		var entry ReasoningEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil
		}
		if entry.Question == question {
			found = &entry
		}
		return nil
	})
	return found
}

// SaveLearning 全量快照学习状态到 KV + 追加版本链 (A3 as-of)。
// KV 写失败返回错误 (主路径); 链写失败仅 warn (增强路径)。
func (s *AgentDBStore) SaveLearning(st *LearningState) error {
	if err := s.cli.KV(s.bkt.learning).Put("state", st); err != nil {
		return err
	}
	// A3: 同时写 vkv 链, 让 as-of 查询能回溯历史学习状态。
	raw, err := json.Marshal(st)
	if err == nil {
		if _, err := s.cli.VersionedKV(s.bkt.learning + "/state").Put(raw); err != nil {
			log.Printf("[swarm_intel] WARN 学习状态版本链写失败: %v", err)
		}
	}
	return nil
}

// LoadLearning 读回学习状态; 键不存在返回 (nil, nil)。
// A3: 默认读最新 (asOf=0); 需要历史版本时调 LoadLearningAsOf。
func (s *AgentDBStore) LoadLearning() (*LearningState, error) {
	return s.LoadLearningAsOf(0)
}

// LoadLearningAsOf A3: 读 as-of 时刻的学习状态。asOfUnixNano=0 等价最新。
// 优先走 vkv 链 (有版本信息); 链未命中回退普通 KV (v2 兼容, PUT /v1/kv 写入的初始数据)。
func (s *AgentDBStore) LoadLearningAsOf(asOfUnixNano int64) (*LearningState, error) {
	entry, err := s.cli.VersionedKV(s.bkt.learning + "/state").Get(asOfUnixNano)
	if err == nil && entry != nil {
		var st LearningState
		if err := json.Unmarshal(entry.Value, &st); err == nil {
			return &st, nil
		}
	}
	// vkv 未命中 (早期写入没有链): 回退普通 KV
	var st LearningState
	ok, err := s.cli.KV(s.bkt.learning).Get("state", &st)
	if err != nil || !ok {
		return nil, err
	}
	return &st, nil
}

// AppendSimEvent 追加一条模拟事件 (逐条 append); CreatedAt 为零值时补戳。
func (s *AgentDBStore) AppendSimEvent(ev *SimEvent) error {
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	return s.cli.Log(s.bkt.sim).Append(ev)
}

// SimEvents 读全量事件后按 runID 过滤。
func (s *AgentDBStore) SimEvents(runID string) ([]*SimEvent, error) {
	var events []*SimEvent
	err := s.cli.Log(s.bkt.sim).ReadAll(func(line []byte) error {
		var ev SimEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil
		}
		if ev.RunID == runID {
			events = append(events, &ev)
		}
		return nil
	})
	if err != nil {
		if isNotFoundErr(err) {
			return nil, nil
		}
		return nil, err
	}
	return events, nil
}

// ── AgentDB 后端: W1 Phase 3 (跨会话反思 + 信素语义化) ──

// AppendReflection 追加一条结构化反思: A6 事件流 (主存储, append-only) +
// Memory 向量桶双写 (语义检索增强, 失败仅 warn)。
func (s *AgentDBStore) AppendReflection(rec *ReflectionRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := s.cli.EventLog(s.bkt.reflections).Append(raw); err != nil {
		return err
	}
	s.indexReflectionMemory(rec)
	return nil
}

// indexReflectionMemory 把反思索引进 Memory 向量桶 (检索侧用 SessionID=rec.ID 重联)。
func (s *AgentDBStore) indexReflectionMemory(rec *ReflectionRecord) {
	if s.sem == nil || rec == nil || rec.Question == "" {
		return
	}
	if _, err := s.sem.StoreMemory(context.Background(), agentdbsemantic.Memory{
		ID:        rec.ID, // MemoryHit.ID 返回记录 ID, 用作向量命中→反思记录重联键
		Content:   rec.Question + " | " + rec.Lesson,
		Type:      agentdbsemantic.MemorySemantic,
		SessionID: rec.ID,
		Tags:      []string{"swarm_reflection", rec.Domain, rec.ID},
	}); err != nil {
		log.Printf("[swarm_intel] WARN Memory 向量索引失败 (reflection %s): %v", rec.ID, err)
	}
}

// SearchReflections 语义向量检索同域历史反思 (Filtered ANN 同款回退哲学):
// 优先 SearchMemory 命中 → 用 rec.ID 重联事件流里的完整记录; 不可达或零命中
// 时回退全量事件流的关键词重叠打分。
func (s *AgentDBStore) SearchReflections(query string, limit int) ([]*ReflectionRecord, error) {
	hits, err := s.sem.SearchMemory(context.Background(), query, limit*4, 0)
	if err == nil && len(hits) > 0 {
		sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		byID, err := s.readAllReflectionsByID()
		if err == nil {
			var result []*ReflectionRecord
			for i, h := range hits {
				if limit > 0 && i >= limit {
					break
				}
				if rec := byID[h.ID]; rec != nil {
					result = append(result, rec)
				}
			}
			if len(result) > 0 {
				return result, nil
			}
		}
		// 向量命中但重联失败 (事件流缺记录): 继续走关键词回退, 不静默丢检索能力。
	}
	// 回退: 全量事件流关键词打分 (P0 行为, 向量桶冷启动/不可达时仍可工作)。
	all, err := s.readAllReflections()
	if err != nil {
		return nil, err
	}
	words := strings.Fields(strings.ToLower(query))
	type scored struct {
		rec   *ReflectionRecord
		score float64
	}
	var candidates []scored
	for _, rec := range all {
		text := strings.ToLower(rec.Question + " " + rec.Lesson)
		if score := keywordScore(words, text); score > 0.3 {
			candidates = append(candidates, scored{rec: rec, score: score})
		}
	}
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	var result []*ReflectionRecord
	for i, c := range candidates {
		if limit > 0 && i >= limit {
			break
		}
		result = append(result, c.rec)
	}
	return result, nil
}

// readAllReflections 游标循环拉全量反思事件 (A6 每次最多 1000 条, 循环续读)。
// 反思体量小 (每本预测一条), 全量拉取是刻意的简单性取舍。
func (s *AgentDBStore) readAllReflections() ([]*ReflectionRecord, error) {
	el := s.cli.EventLog(s.bkt.reflections)
	var all []*ReflectionRecord
	since := int64(0)
	for i := 0; i < 100; i++ { // 10 万条硬护栏, 超过说明该上分页消费
		events, next, err := el.Read(since, 1000)
		if err != nil {
			if isNotFoundErr(err) {
				return nil, nil
			}
			return nil, err
		}
		for _, e := range events {
			var rec ReflectionRecord
			if err := json.Unmarshal(e.Payload, &rec); err == nil {
				all = append(all, &rec)
			}
		}
		if len(events) == 0 {
			return all, nil
		}
		since = next
	}
	return all, fmt.Errorf("reflections stream exceeds 100k events, cursor loop aborted")
}

// readAllReflectionsByID 读全量反思并按 ID 建索引 (向量命中 → 完整记录重联)。
func (s *AgentDBStore) readAllReflectionsByID() (map[string]*ReflectionRecord, error) {
	all, err := s.readAllReflections()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*ReflectionRecord, len(all))
	for _, rec := range all {
		byID[rec.ID] = rec
	}
	return byID, nil
}

// FindTrails 跨域近似信素先验检索 (信素语义化检索侧):
// 优先 SearchMemory 向量命中 → 按 HypothesisID 重联各域快照; 不可达或零命中
// 时回退全域快照的关键词重叠打分。
func (s *AgentDBStore) FindTrails(query string, limit int) ([]*PheromoneTrail, error) {
	byHID, err := s.loadAllTrailsByID()
	if err != nil {
		return nil, err
	}
	hits, err := s.sem.SearchMemory(context.Background(), query, limit*4, 0)
	if err == nil && len(hits) > 0 {
		sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		var result []*PheromoneTrail
		for i, h := range hits {
			if limit > 0 && i >= limit {
				break
			}
			if t := byHID[h.ID]; t != nil {
				result = append(result, t)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}
	// 回退: 全域关键词打分 (P0 行为)。
	words := strings.Fields(strings.ToLower(query))
	type scored struct {
		trail *PheromoneTrail
		score float64
	}
	var candidates []scored
	for _, t := range byHID {
		text := strings.ToLower(t.Hypothesis + " " + strings.Join(t.Evidence, " "))
		if score := keywordScore(words, text); score > 0.3 {
			candidates = append(candidates, scored{trail: t, score: score})
		}
	}
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	var result []*PheromoneTrail
	for i, c := range candidates {
		if limit > 0 && i >= limit {
			break
		}
		result = append(result, c.trail)
	}
	return result, nil
}

// loadAllTrailsByID 列出信素 KV 桶全部域键, 加载所有快照, 按 HypothesisID 建索引。
// 信素体量小 (每域数十条), 全量加载是刻意的简单性取舍; 域数膨胀后再引入
// 向量桶直出重构 (MemoryHit 已带 Content, 可免重联)。
func (s *AgentDBStore) loadAllTrailsByID() (map[string]*PheromoneTrail, error) {
	keys, err := s.cli.KV(s.bkt.trails).Keys()
	if err != nil {
		if isNotFoundErr(err) {
			return map[string]*PheromoneTrail{}, nil
		}
		return nil, err
	}
	byHID := make(map[string]*PheromoneTrail)
	for _, key := range keys {
		var trails []*PheromoneTrail
		ok, err := s.cli.KV(s.bkt.trails).Get(key, &trails)
		if err != nil || !ok {
			continue
		}
		for _, t := range trails {
			if t != nil {
				byHID[t.HypothesisID] = t
			}
		}
	}
	return byHID, nil
}

// ResolvePrediction agentDB 骨架未实现: 日志桶追加型, 无原地改写端点。
// 返回错误让调用方退回"追加一条 resolved 记录"的兼容路径。
// TODO(v3): 若 serve 增加日志桶 rewrite/CAS 端点, 在此实现原地回填。
func (s *AgentDBStore) ResolvePrediction(id, actual string, brier float64, resolvedAt time.Time) error {
	return fmt.Errorf("agentdb: ResolvePrediction 未实现 (追加型日志桶), 请走追加 resolved 记录路径")
}

// isNotFoundErr 判定 agentdbclient 错误是否为 404 (桶/键不存在的唯一可识别特征)。
// 客户端把状态码折进错误串, 骨架阶段据此区分"还没有数据"与"服务坏了"。
func isNotFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "404")
}

// ── 共享小工具 ──

// safeDomainKey 领域名 → 存储键 (小写+空格转下划线, 空名回落 "general"; 与 v2 domainFile 同规则)。
func safeDomainKey(domain string) string {
	safe := strings.ReplaceAll(strings.ToLower(domain), " ", "_")
	if safe == "" {
		safe = "general"
	}
	return safe
}

// keywordScore 关键词重叠率: words 中出现在 lower 文本里的比例 (与 v2 ReasoningBank 同口径)。
func keywordScore(words []string, lower string) float64 {
	if len(words) == 0 {
		return 0
	}
	overlap := 0.0
	for _, w := range words {
		if strings.Contains(lower, w) {
			overlap++
		}
	}
	return overlap / float64(len(words))
}
