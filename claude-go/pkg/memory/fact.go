package memory

import (
	"math"
	"time"
)

// MemoryCategory 记忆分类 (决定衰减速率)
type MemoryCategory string

const (
	CategoryFact       MemoryCategory = "fact"       // API签名、架构决策 — 衰减极慢
	CategoryPreference MemoryCategory = "preference"  // 用户偏好、编码风格
	CategoryGoal       MemoryCategory = "goal"        // 当前目标、里程碑
	CategoryEvent      MemoryCategory = "event"       // 部署、bug修复等事件
	CategoryContext    MemoryCategory = "context"      // 临时讨论、调试信息 — 衰减最快
)

// CategoryDecayRates Ebbinghaus 分类衰减系数
// R = e^(-decayRate * hours_since_access / strength)
var CategoryDecayRates = map[MemoryCategory]float64{
	CategoryFact:       0.01,
	CategoryPreference: 0.05,
	CategoryGoal:       0.15,
	CategoryEvent:      0.25,
	CategoryContext:    0.60,
}

// MemoryFact 统一记忆单元 (跨 TieredStore / Dreaming / Evolution 的通用结构)
type MemoryFact struct {
	ID          string         `json:"id"`
	Content     string         `json:"content"`
	Category    MemoryCategory `json:"category"`
	Importance  float64        `json:"importance"`     // 0-1
	Strength    float64        `json:"strength"`       // 记忆强度 (间隔重复累加)
	DecayRate   float64        `json:"decayRate"`      // 基于分类的衰减系数
	Topics      []string       `json:"topics,omitempty"`
	Source      string         `json:"source"`         // chat/team/dream/evolution/pre_compact/blackboard
	CreatedAt   time.Time      `json:"createdAt"`
	LastAccess  time.Time      `json:"lastAccess"`
	AccessCount int            `json:"accessCount"`
	Consolidated bool          `json:"consolidated"`   // 是否已被整合到 L3 (markdown)
	Archived    bool           `json:"archived"`       // 是否已归档 (冷存储)
	Evergreen   bool           `json:"evergreen"`      // 永久豁免遗忘
}

// DefaultDecayRate 获取分类默认衰减率
func DefaultDecayRate(cat MemoryCategory) float64 {
	if rate, ok := CategoryDecayRates[cat]; ok {
		return rate
	}
	return 0.25
}

// Retention 计算当前保留度 (Ebbinghaus + 间隔重复 + 分类衰减)
// R = importance × e^(-decayRate × hours / strength)
func (f *MemoryFact) Retention() float64 {
	if f.Evergreen {
		return f.Importance
	}
	hoursSinceAccess := time.Since(f.LastAccess).Hours()
	if hoursSinceAccess < 0 {
		hoursSinceAccess = 0
	}
	strength := f.Strength
	if strength < 1 {
		strength = 1
	}
	// 间隔重复: 每次访问 +0.2 强度
	spacedRepetition := strength + 0.2*float64(f.AccessCount)
	return f.Importance * math.Exp(-f.DecayRate*hoursSinceAccess/spacedRepetition)
}

// Touch 标记被访问 (增强间隔重复)
func (f *MemoryFact) Touch() {
	f.AccessCount++
	f.LastAccess = time.Now()
	f.Strength += 0.2
}

// ShouldArchive 判断是否应归档 (retention < threshold)
func (f *MemoryFact) ShouldArchive(threshold float64) bool {
	if f.Evergreen {
		return false
	}
	return f.Retention() < threshold
}

// NewMemoryFact 创建记忆事实
func NewMemoryFact(id, content string, cat MemoryCategory, importance float64, source string, topics []string) *MemoryFact {
	now := time.Now()
	f := &MemoryFact{
		ID:         id,
		Content:    content,
		Category:   cat,
		Importance: importance,
		Strength:   1.0,
		DecayRate:  DefaultDecayRate(cat),
		Topics:     topics,
		Source:     source,
		CreatedAt:  now,
		LastAccess: now,
	}
	if importance >= 0.9 {
		f.Evergreen = true
	}
	return f
}
