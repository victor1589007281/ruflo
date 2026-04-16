package swarm_intel

import (
	"math"
	"sync"
	"time"
)

// PheromoneMemory 信素记忆 (ACO 风格)。
// 记录假设路径的强度，通过正反馈和自然衰减实现集体学习。
type PheromoneMemory struct {
	trails map[string]*PheromoneTrail
	mu     sync.RWMutex
}

// NewPheromoneMemory 创建信素记忆。
func NewPheromoneMemory() *PheromoneMemory {
	return &PheromoneMemory{trails: make(map[string]*PheromoneTrail)}
}

// Deposit 增强假设的信素 (被 Agent 支持时调用)。
func (pm *PheromoneMemory) Deposit(hypothesisID, hypothesis string, amount float64, evidence []string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	t, ok := pm.trails[hypothesisID]
	if !ok {
		t = &PheromoneTrail{
			HypothesisID: hypothesisID,
			Hypothesis:   hypothesis,
			DecayRate:    0.1,
		}
		pm.trails[hypothesisID] = t
	}

	t.Strength = math.Min(1.0, t.Strength+amount)
	t.Supporters++
	t.LastUpdate = time.Now()
	for _, e := range evidence {
		found := false
		for _, ex := range t.Evidence {
			if ex == e {
				found = true
				break
			}
		}
		if !found {
			t.Evidence = append(t.Evidence, e)
		}
	}
}

// Evaporate 全局信素衰减 (每轮调用)。
func (pm *PheromoneMemory) Evaporate() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for id, t := range pm.trails {
		t.Strength *= (1 - t.DecayRate)
		if t.Strength < 0.01 {
			delete(pm.trails, id)
		}
	}
}

// TopTrails 返回信素最强的 N 条路径。
func (pm *PheromoneMemory) TopTrails(n int) []*PheromoneTrail {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	all := make([]*PheromoneTrail, 0, len(pm.trails))
	for _, t := range pm.trails {
		all = append(all, t)
	}

	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].Strength > all[i].Strength {
				all[i], all[j] = all[j], all[i]
			}
		}
	}

	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// Snapshot 返回当前信素状态 (hypothesis → strength)。
func (pm *PheromoneMemory) Snapshot() map[string]float64 {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	snap := make(map[string]float64, len(pm.trails))
	for id, t := range pm.trails {
		snap[id] = t.Strength
	}
	return snap
}
