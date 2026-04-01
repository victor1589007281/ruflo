// 嵌入服务（service.go）
//
// 设计思路：将「文本→向量」后端抽象为 EmbeddingProvider，上层 EmbeddingService 统一加锁、缓存与相似度计算，
// 便于在哈希嵌入、远程 API、本地 ONNX 等实现之间切换而少改调用方。
//
// 整体架构：EmbeddingService 使用 RWMutex 保护 provider 与 map 缓存；缓存淘汰为 FIFO（keyOrder 队首删除），非严格 LRU；
// Compare/BatchEmbed 在持锁下串行调用 embedLocked，简化并发正确性。CosineSimilarity 为无状态纯函数。
package embeddings

import (
	"errors"
	"math"
	"sync"
)

// EmbeddingProvider 嵌入后端契约：Dimensions 应与 Embed 返回切片长度一致；BatchEmbed 允许并行或顺序实现。
type EmbeddingProvider interface {
	Name() string
	Embed(text string) ([]float32, error)
	BatchEmbed(texts []string) ([][]float32, error)
	Dimensions() int
}

// HashEmbeddingProvider 默认零依赖实现：Embed/BatchEmbed 均调用 HashEmbed384，永不返回错误。
type HashEmbeddingProvider struct{}

// Name 返回固定标识 "hash384"，用于日志与指标区分 Provider。
func (HashEmbeddingProvider) Name() string { return "hash384" }

// Embed 委托 HashEmbed384，恒成功返回。
func (HashEmbeddingProvider) Embed(text string) ([]float32, error) {
	return HashEmbed384(text), nil
}

// BatchEmbed 逐条 Embed，维度与顺序与输入一一对应。
func (HashEmbeddingProvider) BatchEmbed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = HashEmbed384(t)
	}
	return out, nil
}

// Dimensions 返回 HashEmbeddingDim（384）。
func (HashEmbeddingProvider) Dimensions() int { return HashEmbeddingDim }

// EmbeddingService 线程安全的嵌入门面：可选按完整文本键缓存向量拷贝，避免重复计算。
type EmbeddingService struct {
	mu        sync.RWMutex         // 写锁：Embed/BatchEmbed/SetProvider；读锁：GetProvider
	provider  EmbeddingProvider    // 当前后端；nil 时 embedLocked 报错
	cache     map[string][]float32 // 明文键→向量；SetProvider 时整体丢弃
	cacheSize int                  // 最大条目数；≤0 关闭缓存
	keyOrder  []string             // 插入 FIFO 顺序；超容量时从队首删键
}

// NewEmbeddingService 默认 provider 为 HashEmbeddingProvider；cacheSize<0 规范为 0。
func NewEmbeddingService(cacheSize int) *EmbeddingService {
	if cacheSize < 0 {
		cacheSize = 0
	}
	return &EmbeddingService{
		provider:  HashEmbeddingProvider{},
		cache:     make(map[string][]float32),
		cacheSize: cacheSize,
	}
}

// rememberLocked 在已持互斥锁下：拷贝 vec 存入 map；新 key 追加 keyOrder；while len(cache)>cacheSize 删除最旧 key。
func (s *EmbeddingService) rememberLocked(key string, vec []float32) {
	if s.cacheSize <= 0 {
		return
	}
	cp := append([]float32(nil), vec...)
	if _, ok := s.cache[key]; !ok {
		s.keyOrder = append(s.keyOrder, key)
	}
	s.cache[key] = cp
	for len(s.cache) > s.cacheSize && len(s.keyOrder) > 0 {
		old := s.keyOrder[0]
		s.keyOrder = s.keyOrder[1:]
		delete(s.cache, old)
	}
}

// embedLocked 已持锁：cache 命中则深拷贝返回；未命中则 Provider.Embed、rememberLocked、再返回拷贝。调用方须已 Lock。
func (s *EmbeddingService) embedLocked(text string) ([]float32, error) {
	if s.provider == nil {
		return nil, errors.New("embeddings: no provider")
	}
	if s.cacheSize > 0 {
		if v, ok := s.cache[text]; ok {
			return append([]float32(nil), v...), nil
		}
	}
	v, err := s.provider.Embed(text)
	if err != nil {
		return nil, err
	}
	s.rememberLocked(text, v)
	return append([]float32(nil), v...), nil
}

// Embed 对外 API：全局互斥下委托 embedLocked，保证与 BatchEmbed 的缓存一致性。
func (s *EmbeddingService) Embed(text string) ([]float32, error) {
	if s == nil {
		return nil, errors.New("embeddings: nil service")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.embedLocked(text)
}

// BatchEmbed 在单把 Lock 内顺序 embedLocked 每条文本；中途失败则整体返回错误（无部分成功语义）。
func (s *EmbeddingService) BatchEmbed(texts []string) ([][]float32, error) {
	if s == nil {
		return nil, errors.New("embeddings: nil service")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := s.embedLocked(t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Compare 计算 dot(va,vb)；若 Provider 产出单位向量（如 HashEmbed384），则 dot 等于余弦相似度 cosθ。
// 数值误差可能导致 |dot|>1，故钳制到 [-1,1]。
func (s *EmbeddingService) Compare(a, b string) (float64, error) {
	if s == nil {
		return 0, errors.New("embeddings: nil service")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	va, err := s.embedLocked(a)
	if err != nil {
		return 0, err
	}
	vb, err := s.embedLocked(b)
	if err != nil {
		return 0, err
	}
	if len(va) != len(vb) {
		return 0, errors.New("embeddings: dimension mismatch")
	}
	var dot float64
	for i := range va {
		dot += float64(va[i]) * float64(vb[i])
	}
	if dot > 1 {
		dot = 1
	}
	if dot < -1 {
		dot = -1
	}
	return dot, nil
}

// SetProvider 写锁下原子替换 provider 并重建空 cache/keyOrder，防止跨维度向量混读。
func (s *EmbeddingService) SetProvider(p EmbeddingProvider) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = p
	s.cache = make(map[string][]float32)
	s.keyOrder = nil
}

// GetProvider 读锁返回 provider 引用（调用方勿在无锁下修改实现内部可变状态）。
func (s *EmbeddingService) GetProvider() EmbeddingProvider {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.provider
}

// CosineSimilarity 计算 cosθ = (a·b)/(‖a‖₂‖b‖₂)；维度不等或任一向量 L2 范数为 0 时返回 0；浮点钳制 [-1,1]。
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	v := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}
