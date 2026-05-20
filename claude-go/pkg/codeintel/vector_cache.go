// vector_cache.go — 轻量级查询结果向量缓存（L2 语义缓存）。
//
// 设计:
//   - 基于字符级 3-gram TF 特征提取（与 agent/textsim.go 同源思想）
//   - 使用余弦相似度衡量查询语义相似性
//   - 线程安全，支持 JSON 持久化
//   - 作为 Engine 的 L2 缓存，在 queryCache (TTL 60s) 之后检查
//
// 持久化路径: .claude-code-intel/vector_cache.json
package codeintel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ============================================================================
// 配置常量
// ============================================================================

const (
	defaultSimilarityThreshold = 0.85
	defaultMaxEntries          = 1000
	vectorCacheFileName        = "vector_cache.json"
	vectorCacheDir             = ".claude-code-intel"
	invertedIndexThreshold     = 10000 // 启用倒排索引的条目数阈值
)

// ============================================================================
// 数据模型
// ============================================================================

// QueryVector 查询特征向量。
type QueryVector struct {
	QueryKey   string             `json:"query_key"`   // 归一化后的查询文本
	Features   map[string]float64 `json:"features"`    // 3-gram -> TF 权重
	ResultHash string             `json:"result_hash"` // 结果摘要 hash
	Timestamp  time.Time          `json:"timestamp"`
	UseCount   int                `json:"use_count"`   // 被命中次数
}

// vectorCacheEntry 内部缓存条目，携带完整结果。
type vectorCacheEntry struct {
	QueryVector
	Result *QueryResult `json:"result"`
}

// feedbackEntry 单条用户反馈记录。
type feedbackEntry struct {
	Timestamp time.Time `json:"timestamp"`
	QueryText string    `json:"query_text"`
	Hit       bool      `json:"hit"`       // L2 缓存是否命中
	Rating    int       `json:"rating"`    // 1-5 星评分（0 表示未评分）
	Clicked   bool      `json:"clicked"`   // 用户是否点击了结果
	Resent    bool      `json:"resent"`    // 用户是否在短期内重新查询了类似内容
}

// VectorCache 语义缓存（L2）。
type VectorCache struct {
	mu                  sync.RWMutex
	entries             []vectorCacheEntry
	similarityThreshold float64
	maxEntries          int
	repoPath            string
	invertedIndex       map[string]map[int]struct{} // feature -> entry indices set
	feedbackWindow      []feedbackEntry             // 最近反馈环形缓冲区
	feedbackCap         int                         // 反馈窗口容量
	lsh                 *MinHashLSH                 // MinHash LSH 索引（>100K 时启用）
}

// NewVectorCache 创建语义缓存。
func NewVectorCache(repoPath string) *VectorCache {
	vc := &VectorCache{
		entries:             make([]vectorCacheEntry, 0, defaultMaxEntries),
		similarityThreshold: defaultSimilarityThreshold,
		maxEntries:          defaultMaxEntries,
		repoPath:            repoPath,
		invertedIndex:       make(map[string]map[int]struct{}),
		feedbackWindow:      make([]feedbackEntry, 0, 200),
		feedbackCap:         200,
	}
	_ = vc.Load() // 尝试恢复历史缓存，失败不影响使用
	return vc
}

// buildInvertedIndex 重建倒排索引（在 Lock 内调用）。
func (vc *VectorCache) buildInvertedIndex() {
	vc.invertedIndex = make(map[string]map[int]struct{}, len(vc.entries)*10)
	for i, ent := range vc.entries {
		for feat := range ent.Features {
			if vc.invertedIndex[feat] == nil {
				vc.invertedIndex[feat] = make(map[int]struct{})
			}
			vc.invertedIndex[feat][i] = struct{}{}
		}
	}
}

// addToInvertedIndex 将单条条目的特征加入倒排索引（在 Lock 内调用）。
func (vc *VectorCache) addToInvertedIndex(idx int, features map[string]float64) {
	for feat := range features {
		if vc.invertedIndex[feat] == nil {
			vc.invertedIndex[feat] = make(map[int]struct{})
		}
		vc.invertedIndex[feat][idx] = struct{}{}
	}
}

// removeFromInvertedIndex 从倒排索引中移除指定索引（在 Lock 内调用）。
func (vc *VectorCache) removeFromInvertedIndex(idx int) {
	for _, ids := range vc.invertedIndex {
		delete(ids, idx)
	}
}

// ============================================================================
// 特征提取
// ============================================================================

// ExtractFeatures 将查询语句转换为稀疏 3-gram TF 特征向量。
func ExtractFeatures(query string) map[string]float64 {
	norm := normalizeQuery(query)
	runes := []rune(norm)
	if len(runes) == 0 {
		return map[string]float64{}
	}

	// 统计 3-gram 词频
	freq := make(map[string]int)
	k := 3
	if len(runes) < k {
		shingle := string(runes)
		freq[shingle] = 1
	} else {
		for i := 0; i <= len(runes)-k; i++ {
			shingle := string(runes[i : i+k])
			freq[shingle]++
		}
	}

	// 计算 TF（此处简化为归一化频率）
	total := 0
	for _, c := range freq {
		total += c
	}
	features := make(map[string]float64, len(freq))
	if total > 0 {
		for shingle, c := range freq {
			features[shingle] = float64(c) / float64(total)
		}
	}
	return features
}

// normalizeQuery 归一化查询文本：小写、保留字母/数字/空格、压缩空格。
func normalizeQuery(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// ============================================================================
// 相似度计算
// ============================================================================

// CosineSimilarity 计算两个稀疏特征向量的余弦相似度。
func CosineSimilarity(a, b map[string]float64) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	if len(a) == 0 || len(b) == 0 {
		return 0.0
	}

	var dot, normA, normB float64
	for k, va := range a {
		normA += va * va
		if vb, ok := b[k]; ok {
			dot += va * vb
		}
	}
	for _, vb := range b {
		normB += vb * vb
	}

	if normA == 0 || normB == 0 {
		return 0.0
	}
	return dot / (sqrtApprox(normA) * sqrtApprox(normB))
}

// sqrtApprox 使用牛顿迭代法快速近似平方根（避免 math 包）。
func sqrtApprox(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 10; i++ {
		z = (z + x/z) / 2
		if z*z == x {
			break
		}
	}
	return z
}

// ============================================================================
// 缓存操作
// ============================================================================

// FindSimilar 查找与 query 语义相似的缓存结果。
// 返回最相似的缓存结果及其命中状态。
// 当缓存条目数超过 invertedIndexThreshold 时，使用倒排索引加速候选筛选。
func (vc *VectorCache) FindSimilar(query string) (*QueryResult, bool) {
	vc.mu.RLock()
	defer vc.mu.RUnlock()

	if len(vc.entries) == 0 {
		return nil, false
	}

	queryFeatures := ExtractFeatures(query)
	var bestIdx int = -1
	var bestSim float64

	// 三级查询策略：小规模线性遍历 / 中规模倒排索引 / 超大规模 LSH
	entryCount := len(vc.entries)
	switch {
	case entryCount <= invertedIndexThreshold:
		// 线性遍历
		for i, ent := range vc.entries {
			sim := CosineSimilarity(queryFeatures, ent.Features)
			if sim > bestSim {
				bestSim = sim
				bestIdx = i
			}
		}
	case entryCount <= lshThreshold:
		// 倒排索引
		candidates := make(map[int]struct{})
		for feat := range queryFeatures {
			if ids, ok := vc.invertedIndex[feat]; ok {
				for id := range ids {
					candidates[id] = struct{}{}
				}
			}
		}
		if len(candidates) == 0 {
			for i, ent := range vc.entries {
				sim := CosineSimilarity(queryFeatures, ent.Features)
				if sim > bestSim {
					bestSim = sim
					bestIdx = i
				}
			}
		} else {
			for idx := range candidates {
				if idx < 0 || idx >= entryCount {
					continue
				}
				sim := CosineSimilarity(queryFeatures, vc.entries[idx].Features)
				if sim > bestSim {
					bestSim = sim
					bestIdx = idx
				}
			}
		}
	default:
		// MinHash LSH：先通过签名桶快速筛选候选，再精确计算
		if vc.lsh != nil {
			candidates := vc.lsh.Query(queryFeatures)
			if len(candidates) == 0 {
				for i, ent := range vc.entries {
					sim := CosineSimilarity(queryFeatures, ent.Features)
					if sim > bestSim {
						bestSim = sim
						bestIdx = i
					}
				}
			} else {
				for _, idx := range candidates {
					if idx < 0 || idx >= entryCount {
						continue
					}
					sim := CosineSimilarity(queryFeatures, vc.entries[idx].Features)
					if sim > bestSim {
						bestSim = sim
						bestIdx = idx
					}
				}
			}
		} else {
			// LSH 未初始化，降级为全量遍历
			for i, ent := range vc.entries {
				sim := CosineSimilarity(queryFeatures, ent.Features)
				if sim > bestSim {
					bestSim = sim
					bestIdx = i
				}
			}
		}
	}

	if bestIdx < 0 || bestSim < vc.similarityThreshold {
		return nil, false
	}

	// 更新命中统计
	vc.entries[bestIdx].UseCount++
	vc.entries[bestIdx].Timestamp = time.Now()
	return vc.entries[bestIdx].Result, true
}

// Store 存储查询-结果对到语义缓存。
func (vc *VectorCache) Store(query string, result *QueryResult) {
	if result == nil {
		return
	}

	vc.mu.Lock()
	defer vc.mu.Unlock()

	key := normalizeQuery(query)
	features := ExtractFeatures(query)
	hash := resultHash(result)

	// 如果已存在相同 QueryKey，则更新
	for i := range vc.entries {
		if vc.entries[i].QueryKey == key {
			vc.entries[i].Features = features
			vc.entries[i].ResultHash = hash
			vc.entries[i].Result = result
			vc.entries[i].Timestamp = time.Now()
			return
		}
	}

	// 新增条目
	ent := vectorCacheEntry{
		QueryVector: QueryVector{
			QueryKey:   key,
			Features:   features,
			ResultHash: hash,
			Timestamp:  time.Now(),
			UseCount:   0,
		},
		Result: result,
	}
	idx := len(vc.entries)
	vc.entries = append(vc.entries, ent)
	vc.addToInvertedIndex(idx, features)

	// 超大规模时维护 LSH 索引
	if len(vc.entries) > lshThreshold {
		if vc.lsh == nil {
			vc.lsh = newMinHashLSH()
		}
		vc.lsh.Add(idx, features)
	}

	// 超过上限时淘汰最久未使用（按 UseCount 升序，再按 Timestamp 升序）
	if len(vc.entries) > vc.maxEntries {
		vc.evictOne()
	}
}

// evictOne 淘汰一条缓存（在 Lock 内调用）。
func (vc *VectorCache) evictOne() {
	if len(vc.entries) == 0 {
		return
	}
	// 按 UseCount 升序，再按 Timestamp 升序排序，淘汰第一个
	best := 0
	for i := 1; i < len(vc.entries); i++ {
		if vc.entries[i].UseCount < vc.entries[best].UseCount {
			best = i
		} else if vc.entries[i].UseCount == vc.entries[best].UseCount &&
			vc.entries[i].Timestamp.Before(vc.entries[best].Timestamp) {
			best = i
		}
	}
	// 删除 best（索引会移位，需重建倒排索引和 LSH）
	vc.entries = append(vc.entries[:best], vc.entries[best+1:]...)
	if len(vc.entries) >= invertedIndexThreshold {
		vc.buildInvertedIndex()
	}
	if len(vc.entries) > lshThreshold && vc.lsh != nil {
		vc.lsh.Rebuild(vc.entries)
	}
}

// SetThreshold 动态设置相似度阈值。
func (vc *VectorCache) SetThreshold(threshold float64) {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	if threshold >= 0 && threshold <= 1 {
		vc.similarityThreshold = threshold
	}
}

// SetMaxEntries 动态设置最大条目数。
func (vc *VectorCache) SetMaxEntries(max int) {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	if max > 0 {
		vc.maxEntries = max
		for len(vc.entries) > vc.maxEntries {
			vc.evictOne()
		}
	}
}

// Len 返回当前缓存条目数。
func (vc *VectorCache) Len() int {
	vc.mu.RLock()
	defer vc.mu.RUnlock()
	return len(vc.entries)
}

// RecordFeedback 记录用户对某次查询结果的反馈。
// 当反馈窗口满时自动触发阈值校准。
func (vc *VectorCache) RecordFeedback(queryText string, hit bool, rating int, clicked, resent bool) {
	vc.mu.Lock()
	defer vc.mu.Unlock()

	fe := feedbackEntry{
		Timestamp: time.Now(),
		QueryText: queryText,
		Hit:       hit,
		Rating:    rating,
		Clicked:   clicked,
		Resent:    resent,
	}
	vc.feedbackWindow = append(vc.feedbackWindow, fe)
	if len(vc.feedbackWindow) > vc.feedbackCap {
		vc.feedbackWindow = vc.feedbackWindow[len(vc.feedbackWindow)-vc.feedbackCap:]
	}

	// 每收集 20 条反馈自动校准一次
	if len(vc.feedbackWindow)%20 == 0 {
		vc.autoCalibrateLocked()
	}
}

// autoCalibrateLocked 根据反馈窗口自动调整相似度阈值（在 Lock 内调用）。
func (vc *VectorCache) autoCalibrateLocked() {
	if len(vc.feedbackWindow) < 20 {
		return
	}

	var hitCount, goodCount, badCount int
	for _, fe := range vc.feedbackWindow {
		if fe.Hit {
			hitCount++
			// 点击了且没有重新查询，或评分 >= 4，视为优质命中
			if (fe.Clicked && !fe.Resent) || fe.Rating >= 4 {
				goodCount++
			}
			// 重新查询了，或评分 <= 2，视为劣质命中
			if fe.Resent || fe.Rating <= 2 {
				badCount++
			}
		}
	}
	if hitCount == 0 {
		return
	}

	effectivePrecision := float64(goodCount) / float64(hitCount)
	badRate := float64(badCount) / float64(hitCount)

	// 校准策略
	oldThreshold := vc.similarityThreshold
	switch {
	case badRate > 0.3 && effectivePrecision < 0.6:
		// 劣质命中过多，提高门槛
		vc.similarityThreshold += 0.03
	case effectivePrecision > 0.9 && badRate < 0.1:
		// 精度很高，可以尝试降低门槛提高命中率
		vc.similarityThreshold -= 0.02
	case badRate > 0.15:
		// 轻微提高门槛
		vc.similarityThreshold += 0.01
	}

	// 阈值 clamp
	if vc.similarityThreshold < 0.5 {
		vc.similarityThreshold = 0.5
	}
	if vc.similarityThreshold > 0.99 {
		vc.similarityThreshold = 0.99
	}
	_ = oldThreshold // 可用于日志记录
}

// AutoCalibrate 手动触发阈值校准。
func (vc *VectorCache) AutoCalibrate() {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	vc.autoCalibrateLocked()
}

// FeedbackSummary 返回反馈窗口摘要（用于调试/监控）。
func (vc *VectorCache) FeedbackSummary() map[string]interface{} {
	vc.mu.RLock()
	defer vc.mu.RUnlock()

	var hitCount, goodCount, badCount, totalRating, ratedCount int
	for _, fe := range vc.feedbackWindow {
		if fe.Hit {
			hitCount++
			if (fe.Clicked && !fe.Resent) || fe.Rating >= 4 {
				goodCount++
			}
			if fe.Resent || fe.Rating <= 2 {
				badCount++
			}
		}
		if fe.Rating > 0 {
			totalRating += fe.Rating
			ratedCount++
		}
	}

	avgRating := 0.0
	if ratedCount > 0 {
		avgRating = float64(totalRating) / float64(ratedCount)
	}
	precision := 0.0
	if hitCount > 0 {
		precision = float64(goodCount) / float64(hitCount)
	}

	return map[string]interface{}{
		"threshold":      vc.similarityThreshold,
		"feedback_count": len(vc.feedbackWindow),
		"hit_count":      hitCount,
		"good_count":     goodCount,
		"bad_count":      badCount,
		"precision":      precision,
		"avg_rating":     avgRating,
	}
}

// ============================================================================
// 持久化
// ============================================================================

// persistEntry 用于 JSON 序列化的结构（不包含完整的 QueryResult）。
type persistEntry struct {
	QueryKey   string             `json:"query_key"`
	Features   map[string]float64 `json:"features"`
	ResultHash string             `json:"result_hash"`
	Timestamp  time.Time          `json:"timestamp"`
	UseCount   int                `json:"use_count"`
	ResultJSON json.RawMessage    `json:"result_json"`
}

// persistData 顶层持久化结构。
type persistData struct {
	Version    string         `json:"version"`
	Threshold  float64        `json:"threshold"`
	MaxEntries int            `json:"max_entries"`
	Entries    []persistEntry `json:"entries"`
}

// Save 将缓存持久化到 JSON 文件。
func (vc *VectorCache) Save() error {
	vc.mu.RLock()
	defer vc.mu.RUnlock()

	path := vc.cachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data := persistData{
		Version:    "1.0",
		Threshold:  vc.similarityThreshold,
		MaxEntries: vc.maxEntries,
		Entries:    make([]persistEntry, 0, len(vc.entries)),
	}

	for _, ent := range vc.entries {
		var raw json.RawMessage
		if ent.Result != nil {
			b, err := json.Marshal(ent.Result)
			if err == nil {
				raw = b
			}
		}
		data.Entries = append(data.Entries, persistEntry{
			QueryKey:   ent.QueryKey,
			Features:   ent.Features,
			ResultHash: ent.ResultHash,
			Timestamp:  ent.Timestamp,
			UseCount:   ent.UseCount,
			ResultJSON: raw,
		})
	}

	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal vector cache: %w", err)
	}
	return os.WriteFile(path, out, 0644)
}

// Load 从 JSON 文件恢复缓存。
func (vc *VectorCache) Load() error {
	path := vc.cachePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var pd persistData
	if err := json.Unmarshal(data, &pd); err != nil {
		return fmt.Errorf("parse vector cache: %w", err)
	}

	vc.mu.Lock()
	defer vc.mu.Unlock()

	if pd.Threshold > 0 {
		vc.similarityThreshold = pd.Threshold
	}
	if pd.MaxEntries > 0 {
		vc.maxEntries = pd.MaxEntries
	}

	entries := make([]vectorCacheEntry, 0, len(pd.Entries))
	for _, pe := range pd.Entries {
		var result *QueryResult
		if len(pe.ResultJSON) > 0 {
			var qr QueryResult
			if err := json.Unmarshal(pe.ResultJSON, &qr); err == nil {
				result = &qr
			}
		}
		entries = append(entries, vectorCacheEntry{
			QueryVector: QueryVector{
				QueryKey:   pe.QueryKey,
				Features:   pe.Features,
				ResultHash: pe.ResultHash,
				Timestamp:  pe.Timestamp,
				UseCount:   pe.UseCount,
			},
			Result: result,
		})
	}
	vc.entries = entries
	if len(vc.entries) >= invertedIndexThreshold {
		vc.buildInvertedIndex()
	}
	if len(vc.entries) > lshThreshold {
		if vc.lsh == nil {
			vc.lsh = newMinHashLSH()
		}
		vc.lsh.Rebuild(vc.entries)
	}
	return nil
}

// cachePath 返回缓存文件路径。
func (vc *VectorCache) cachePath() string {
	return filepath.Join(vc.repoPath, vectorCacheDir, vectorCacheFileName)
}

// ============================================================================
// 辅助函数
// ============================================================================

// resultHash 计算 QueryResult 的摘要 hash。
func resultHash(qr *QueryResult) string {
	if qr == nil {
		return ""
	}
	// 使用 QueryType + Tokens + LatencyMs 作为摘要
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%d", qr.QueryType, qr.Tokens, qr.LatencyMs)
	if qr.Results != nil {
		b, _ := json.Marshal(qr.Results)
		// 只取前 256 字节避免过大
		if len(b) > 256 {
			b = b[:256]
		}
		h.Write(b)
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// ============================================================================
// 与 Engine 集成（L2 缓存）
// ============================================================================

// Engine 扩展方法已在 query.go 中通过字段访问，此处提供辅助。

// findSimilarInVectorCache 是 Engine 内部调用 L2 缓存的辅助函数。
// 在 query.go 中通过 e.vectorCache.FindSimilar 调用。
