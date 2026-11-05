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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// ── agentDB 后端 (集群骨架) ──

// AgentDBStore 基于 agentDB serve (HTTP 数据面) 的 SwarmStore 实现。
//
// 键值映射策略 (P0):
//   - 信素: KV bucket, key=domain (快照整存整取, 即 batch 语义);
//   - 学习状态: KV bucket, key="state" (全量快照);
//   - 模拟事件: Log bucket 逐条追加, 读全量后按 runID 过滤;
//   - 预测历史/推理库: Log bucket 逐条追加。
//
// TODO(v3): FindSimilar/SearchReasonings 的 Filtered ANN 向量化 —— 骨架阶段沿用
// 关键词回退 (与 JSONL 同路径), 升级时走 pkg/agentdbsemantic 的 Memory/Retrieve 端点,
// 接口签名不变, 调用方零改动。
type AgentDBStore struct {
	cli *agentdbclient.Client
	sem *agentdbsemantic.Client // P0 预留: Filtered ANN 接入点 (当前未使用)

	baseURL string
}

// 编译期断言: AgentDBStore 满足接口。
var _ SwarmStore = (*AgentDBStore)(nil)

// agentDB 桶名 (单引擎全局共享, 集群内多进程经 serve 串行化, 服从 first-committer-wins)。
const (
	adbBucketTrails   = "swarm_intel_trails"   // 信素快照 (KV)
	adbBucketLearning = "swarm_intel_learning" // 学习状态 (KV)
	adbLogPredictions = "swarm_intel_predictions"
	adbLogReasonings  = "swarm_intel_reasonings"
	adbLogSimEvents   = "swarm_intel_sim_events"
)

// NewAgentDBStore 创建 agentDB 后端; 构造时做一次可达性探针 (KV.Keys),
// 失败返回错误 —— 调用方 (NewEngine) 负责回退 JSONL 并告警, 不 crash。
func NewAgentDBStore(baseURL string) (*AgentDBStore, error) {
	cli := agentdbclient.New(baseURL)
	if _, err := cli.KV(adbBucketLearning).Keys(); err != nil {
		return nil, fmt.Errorf("agentDB 不可达 (%s): %w", baseURL, err)
	}
	return &AgentDBStore{
		cli:     cli,
		sem:     agentdbsemantic.New(baseURL),
		baseURL: baseURL,
	}, nil
}

// SaveTrails 把领域信素快照整存到 KV (batch 写)。
func (s *AgentDBStore) SaveTrails(domain string, trails []*PheromoneTrail) error {
	return s.cli.KV(adbBucketTrails).Put(safeDomainKey(domain), trails)
}

// LoadTrails 读取领域信素快照; 键不存在返回空。
func (s *AgentDBStore) LoadTrails(domain string) ([]*PheromoneTrail, error) {
	var trails []*PheromoneTrail
	ok, err := s.cli.KV(adbBucketTrails).Get(safeDomainKey(domain), &trails)
	if err != nil || !ok {
		return nil, err
	}
	return trails, nil
}

// AppendPrediction 追加一条预测记录 (history 逐条 append)。
func (s *AgentDBStore) AppendPrediction(rec *PredictionRecord) error {
	return s.cli.Log(adbLogPredictions).Append(rec)
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
	err := s.cli.Log(adbLogPredictions).ReadAll(func(line []byte) error {
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

// FindSimilar 关键词重叠检索相似历史预测。
// TODO(v3 Filtered ANN): 走 s.sem 的 Memory/Retrieve 向量化检索替换本回退实现。
func (s *AgentDBStore) FindSimilar(question string, limit int) ([]*PredictionRecord, error) {
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

// StoreReasoning 存储一条推理路径。
func (s *AgentDBStore) StoreReasoning(entry *ReasoningEntry) error {
	return s.cli.Log(adbLogReasonings).Append(entry)
}

// SearchReasonings 关键词重叠检索相似推理路径。
// TODO(v3 Filtered ANN): 走 s.sem 的 Memory/Retrieve 向量化检索替换本回退实现。
func (s *AgentDBStore) SearchReasonings(query string, limit int) ([]*ReasoningEntry, error) {
	var all []*ReasoningEntry
	err := s.cli.Log(adbLogReasonings).ReadAll(func(line []byte) error {
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

// SaveLearning 全量快照学习状态到 KV。
func (s *AgentDBStore) SaveLearning(st *LearningState) error {
	return s.cli.KV(adbBucketLearning).Put("state", st)
}

// LoadLearning 读回学习状态; 键不存在返回 (nil, nil)。
func (s *AgentDBStore) LoadLearning() (*LearningState, error) {
	var st LearningState
	ok, err := s.cli.KV(adbBucketLearning).Get("state", &st)
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
	return s.cli.Log(adbLogSimEvents).Append(ev)
}

// SimEvents 读全量事件后按 runID 过滤。
func (s *AgentDBStore) SimEvents(runID string) ([]*SimEvent, error) {
	var events []*SimEvent
	err := s.cli.Log(adbLogSimEvents).ReadAll(func(line []byte) error {
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
