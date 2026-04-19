package memory

import (
	"log"
	"time"
)

// DecayManager 遗忘管理器
// 实现 Ebbinghaus 分类衰减 + 间隔重复 + 永久豁免 + 归档
type DecayManager struct {
	factStore *FactStore

	// 配置
	ArchiveThreshold float64 // retention < 此值时归档 (默认 0.05)
	EvergreenThreshold float64 // importance >= 此值时永不遗忘 (默认 0.9)
	RetrievalBonus   float64 // 每次被召回时 strength +此值 (默认 0.2)
	MaxActiveFacts   int     // 活跃事实上限 (默认 2000)
}

// NewDecayManager 创建遗忘管理器
func NewDecayManager(factStore *FactStore) *DecayManager {
	return &DecayManager{
		factStore:          factStore,
		ArchiveThreshold:   0.05,
		EvergreenThreshold: 0.9,
		RetrievalBonus:     0.2,
		MaxActiveFacts:     2000,
	}
}

// RunCycle 执行一次完整的衰减周期
// 返回: (归档数, 强化数)
func (dm *DecayManager) RunCycle() (archived, reinforced int) {
	if dm.factStore == nil {
		return 0, 0
	}

	dm.factStore.mu.Lock()
	defer dm.factStore.mu.Unlock()

	now := time.Now()

	for _, fact := range dm.factStore.facts {
		if fact.Archived {
			continue
		}

		// 永久豁免检查
		if fact.Importance >= dm.EvergreenThreshold && !fact.Evergreen {
			fact.Evergreen = true
			reinforced++
			continue
		}

		// 计算保留率
		retention := fact.Retention()

		// 归档低保留率的事实
		if retention < dm.ArchiveThreshold {
			fact.Archived = true
			archived++
			continue
		}

		// 间隔重复强化: 最近 7 天内被访问过的事实增强 strength
		if !fact.LastAccess.IsZero() && now.Sub(fact.LastAccess) < 7*24*time.Hour && fact.AccessCount > 0 {
			reinforced++
		}
	}

	// 如果活跃事实超过上限，归档保留率最低的
	activeFacts := make([]*MemoryFact, 0)
	for _, f := range dm.factStore.facts {
		if !f.Archived {
			activeFacts = append(activeFacts, f)
		}
	}

	if len(activeFacts) > dm.MaxActiveFacts {
		type scored struct {
			fact      *MemoryFact
			retention float64
		}
		var scoredFacts []scored
		for _, f := range activeFacts {
			if !f.Evergreen {
				scoredFacts = append(scoredFacts, scored{f, f.Retention()})
			}
		}
		// 按 retention 排序 (低→高)
		for i := 0; i < len(scoredFacts)-1; i++ {
			for j := i + 1; j < len(scoredFacts); j++ {
				if scoredFacts[j].retention < scoredFacts[i].retention {
					scoredFacts[i], scoredFacts[j] = scoredFacts[j], scoredFacts[i]
				}
			}
		}
		// 归档多余的
		excess := len(activeFacts) - dm.MaxActiveFacts
		for i := 0; i < excess && i < len(scoredFacts); i++ {
			scoredFacts[i].fact.Archived = true
			archived++
		}
	}

	if archived > 0 || reinforced > 0 {
		dm.factStore.dirty = true
		log.Printf("[Decay] 周期完成: 归档 %d 条, 强化 %d 条", archived, reinforced)
	}

	return
}

// StrengthenRetrieval 召回时强化记忆
func (dm *DecayManager) StrengthenRetrieval(factID string) {
	if dm.factStore == nil {
		return
	}
	dm.factStore.mu.Lock()
	defer dm.factStore.mu.Unlock()

	if fact, ok := dm.factStore.facts[factID]; ok {
		fact.Strength += dm.RetrievalBonus
		fact.AccessCount++
		fact.LastAccess = time.Now()
		dm.factStore.dirty = true
	}
}
