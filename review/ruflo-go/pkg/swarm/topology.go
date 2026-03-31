package swarm

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

const maxMeshDegreeDefault = 10

// TopologyManager maintains graph edges for swarm coordination.
type TopologyManager struct {
	mu sync.RWMutex

	cfg           TopologyConfig
	nodes         map[string]*TopologyNode
	rebalanceMu   sync.Mutex
	lastRebalance time.Time
	rng           *rand.Rand
}

// NewTopologyManager creates a manager with config.
func NewTopologyManager(cfg TopologyConfig) *TopologyManager {
	if cfg.MaxMeshDegree <= 0 {
		cfg.MaxMeshDegree = maxMeshDegreeDefault
	}
	if cfg.HybridRandomPeers <= 0 {
		cfg.HybridRandomPeers = 3
	}
	if len(cfg.RolePriorityOrder) == 0 {
		cfg.RolePriorityOrder = []string{"queen", "coordinator", "architect", "coder", "worker"}
	}
	return &TopologyManager{
		cfg:   cfg,
		nodes: make(map[string]*TopologyNode),
		rng:   rand.New(rand.NewPCG(topologySeed(), topologySeed())),
	}
}

func topologySeed() uint64 {
	return uint64(time.Now().UnixNano())
}

// AddNode adds an agent with role and wires edges per topology.
func (t *TopologyManager) AddNode(agentID, role string) error {
	if agentID == "" {
		return errors.New("topology: empty agent id")
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, exists := t.nodes[agentID]; exists {
		return fmt.Errorf("topology: duplicate node %s", agentID)
	}
	t.nodes[agentID] = &TopologyNode{
		AgentID:   agentID,
		Role:      role,
		Neighbors: make(map[string]struct{}),
		JoinedAt:  time.Now(),
	}

	ids := t.agentIDsUnlocked()
	switch t.cfg.Type {
	case api.TopologyMesh:
		t.connectMeshUnlocked(agentID, ids)
	case api.TopologyHierarchical:
		t.connectHierarchicalUnlocked(agentID, ids, "queen")
	case api.TopologyCentralized:
		t.connectCentralizedUnlocked(agentID, ids)
	case api.TopologyHybrid:
		t.connectHybridUnlocked(agentID, ids)
	default:
		t.connectMeshUnlocked(agentID, ids)
	}
	return nil
}

func (t *TopologyManager) agentIDsUnlocked() []string {
	out := make([]string, 0, len(t.nodes))
	for id := range t.nodes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (t *TopologyManager) connectMeshUnlocked(newID string, all []string) {
	maxDeg := t.cfg.MaxMeshDegree
	candidates := make([]string, 0, len(all))
	for _, id := range all {
		if id == newID {
			continue
		}
		candidates = append(candidates, id)
	}
	t.rng.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	n := newID
	for i := 0; i < len(candidates) && i < maxDeg; i++ {
		peer := candidates[i]
		t.nodes[n].Neighbors[peer] = struct{}{}
		t.nodes[peer].Neighbors[n] = struct{}{}
	}
}

func (t *TopologyManager) connectHierarchicalUnlocked(newID string, all []string, rootRole string) {
	var root string
	for id, node := range t.nodes {
		if node.Role == rootRole {
			root = id
			break
		}
	}
	if root == "" {
		for id := range t.nodes {
			if root == "" || id < root {
				root = id
			}
		}
	}
	if root == "" || newID == root {
		return
	}
	t.nodes[newID].Neighbors[root] = struct{}{}
	t.nodes[root].Neighbors[newID] = struct{}{}
}

func (t *TopologyManager) connectCentralizedUnlocked(newID string, all []string) {
	var coord string
	for id, node := range t.nodes {
		if node.Role == "coordinator" {
			coord = id
			break
		}
	}
	if coord == "" {
		for _, id := range all {
			coord = id
			break
		}
	}
	if coord == "" || newID == coord {
		return
	}
	t.nodes[newID].Neighbors[coord] = struct{}{}
	t.nodes[coord].Neighbors[newID] = struct{}{}
}

func (t *TopologyManager) connectHybridUnlocked(newID string, all []string) {
	var hub string
	for id, node := range t.nodes {
		if node.Role == "queen" || node.Role == "coordinator" {
			hub = id
			break
		}
	}
	if hub == "" {
		for _, id := range all {
			hub = id
			break
		}
	}
	if hub != "" && newID != hub {
		t.nodes[newID].Neighbors[hub] = struct{}{}
		t.nodes[hub].Neighbors[newID] = struct{}{}
	}
	k := t.cfg.HybridRandomPeers
	candidates := make([]string, 0, len(all))
	for _, id := range all {
		if id == newID || id == hub {
			continue
		}
		candidates = append(candidates, id)
	}
	t.rng.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	for i := 0; i < len(candidates) && i < k; i++ {
		peer := candidates[i]
		t.nodes[newID].Neighbors[peer] = struct{}{}
		t.nodes[peer].Neighbors[newID] = struct{}{}
	}
}

// RemoveNode removes an agent and its edges.
func (t *TopologyManager) RemoveNode(agentID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[agentID]
	if !ok {
		return fmt.Errorf("topology: unknown node %s", agentID)
	}
	for peer := range n.Neighbors {
		if pn, ok2 := t.nodes[peer]; ok2 {
			delete(pn.Neighbors, agentID)
		}
	}
	delete(t.nodes, agentID)
	return nil
}

// GetNeighbors returns neighbor ids.
func (t *TopologyManager) GetNeighbors(agentID string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, ok := t.nodes[agentID]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(n.Neighbors))
	for p := range n.Neighbors {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// FindOptimalPath BFS shortest path from -> to.
func (t *TopologyManager) FindOptimalPath(from, to string) ([]string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.nodes[from]; !ok {
		return nil, fmt.Errorf("topology: unknown from %s", from)
	}
	if _, ok := t.nodes[to]; !ok {
		return nil, fmt.Errorf("topology: unknown to %s", to)
	}
	if from == to {
		return []string{from}, nil
	}
	type qitem struct {
		id   string
		path []string
	}
	queue := []qitem{{id: from, path: []string{from}}}
	seen := map[string]struct{}{from: {}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for peer := range t.nodes[cur.id].Neighbors {
			if _, ok := seen[peer]; ok {
				continue
			}
			path := append(append([]string(nil), cur.path...), peer)
			if peer == to {
				return path, nil
			}
			seen[peer] = struct{}{}
			queue = append(queue, qitem{id: peer, path: path})
		}
	}
	return nil, errors.New("topology: no path")
}

// ElectLeader returns agent id with highest role priority among nodes.
func (t *TopologyManager) ElectLeader() (string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.electLeaderUnlocked()
}

// Rebalance rewires with 5s throttle.
func (t *TopologyManager) Rebalance(ctx context.Context) error {
	t.rebalanceMu.Lock()
	if time.Since(t.lastRebalance) < 5*time.Second {
		t.rebalanceMu.Unlock()
		return errors.New("topology: rebalance throttled")
	}
	t.lastRebalance = time.Now()
	t.rebalanceMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.rebuildGraphUnlocked()
	return nil
}

func (t *TopologyManager) rebuildGraphUnlocked() {
	ids := t.agentIDsUnlocked()
	for _, id := range ids {
		t.nodes[id].Neighbors = make(map[string]struct{})
	}
	switch t.cfg.Type {
	case api.TopologyMesh:
		maxDeg := t.cfg.MaxMeshDegree
		for _, id := range ids {
			cands := make([]string, 0, len(ids))
			for _, o := range ids {
				if o != id {
					cands = append(cands, o)
				}
			}
			t.rng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
			n := maxDeg
			if n > len(cands) {
				n = len(cands)
			}
			for i := 0; i < n; i++ {
				p := cands[i]
				t.nodes[id].Neighbors[p] = struct{}{}
				t.nodes[p].Neighbors[id] = struct{}{}
			}
		}
	case api.TopologyHierarchical:
		var root string
		for id, node := range t.nodes {
			if node.Role == "queen" {
				root = id
				break
			}
		}
		if root == "" && len(ids) > 0 {
			root = ids[0]
		}
		for _, id := range ids {
			if id != root {
				t.nodes[id].Neighbors[root] = struct{}{}
				t.nodes[root].Neighbors[id] = struct{}{}
			}
		}
	case api.TopologyCentralized:
		var coord string
		for id, node := range t.nodes {
			if node.Role == "coordinator" {
				coord = id
				break
			}
		}
		if coord == "" && len(ids) > 0 {
			coord = ids[0]
		}
		for _, id := range ids {
			if id != coord {
				t.nodes[id].Neighbors[coord] = struct{}{}
				t.nodes[coord].Neighbors[id] = struct{}{}
			}
		}
	case api.TopologyHybrid:
		var hub string
		for id, node := range t.nodes {
			if node.Role == "queen" || node.Role == "coordinator" {
				hub = id
				break
			}
		}
		if hub == "" && len(ids) > 0 {
			hub = ids[0]
		}
		k := t.cfg.HybridRandomPeers
		for _, id := range ids {
			if hub != "" && id != hub {
				t.nodes[id].Neighbors[hub] = struct{}{}
				t.nodes[hub].Neighbors[id] = struct{}{}
			}
			cands := make([]string, 0, len(ids))
			for _, o := range ids {
				if o != id && o != hub {
					cands = append(cands, o)
				}
			}
			t.rng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
			n := k
			if n > len(cands) {
				n = len(cands)
			}
			for i := 0; i < n; i++ {
				p := cands[i]
				t.nodes[id].Neighbors[p] = struct{}{}
				t.nodes[p].Neighbors[id] = struct{}{}
			}
		}
	default:
		for _, id := range ids {
			t.connectMeshUnlocked(id, ids)
		}
	}
}

func topologyRoleString(agentType api.AgentType) string {
	if agentType == api.AgentTypeQueen {
		return "queen"
	}
	return string(agentType)
}

func (t *TopologyManager) roleMatches(nodeRole string, want api.AgentType) bool {
	return nodeRole == topologyRoleString(want) || nodeRole == string(want)
}

// GetState returns topology type, counts, and current elected leader id.
func (t *TopologyManager) GetState() TopologyState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := len(t.nodes)
	var edges int
	for _, node := range t.nodes {
		edges += len(node.Neighbors)
	}
	edges /= 2
	leader, _ := t.electLeaderUnlocked()
	return TopologyState{
		Type:      t.cfg.Type,
		NodeCount: n,
		EdgeCount: edges,
		Leader:    leader,
	}
}

func (t *TopologyManager) electLeaderUnlocked() (string, error) {
	if len(t.nodes) == 0 {
		return "", errors.New("topology: no nodes")
	}
	priority := make(map[string]int, len(t.cfg.RolePriorityOrder))
	for i, r := range t.cfg.RolePriorityOrder {
		priority[r] = i
	}
	bestID := ""
	bestScore := 1 << 30
	for id, node := range t.nodes {
		sc, ok := priority[node.Role]
		if !ok {
			sc = len(t.cfg.RolePriorityOrder) + 1
		}
		if bestID == "" || sc < bestScore || (sc == bestScore && id < bestID) {
			bestScore = sc
			bestID = id
		}
	}
	return bestID, nil
}

// UpdateNode changes the role associated with an existing node.
func (t *TopologyManager) UpdateNode(agentID string, role api.AgentType) error {
	if agentID == "" {
		return errors.New("topology: empty agent id")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[agentID]
	if !ok {
		return fmt.Errorf("topology: unknown node %s", agentID)
	}
	n.Role = topologyRoleString(role)
	return nil
}

// GetNode returns a defensive copy of a topology node.
func (t *TopologyManager) GetNode(agentID string) (*TopologyNode, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, ok := t.nodes[agentID]
	if !ok || n == nil {
		return nil, false
	}
	nb := make(map[string]struct{}, len(n.Neighbors))
	for k := range n.Neighbors {
		nb[k] = struct{}{}
	}
	return &TopologyNode{
		AgentID:   n.AgentID,
		Role:      n.Role,
		Neighbors: nb,
		JoinedAt:  n.JoinedAt,
	}, true
}

// GetNodesByRole returns nodes whose role matches the agent type.
func (t *TopologyManager) GetNodesByRole(role api.AgentType) []*TopologyNode {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []*TopologyNode
	for _, n := range t.nodes {
		if n == nil || !t.roleMatches(n.Role, role) {
			continue
		}
		nb := make(map[string]struct{}, len(n.Neighbors))
		for k := range n.Neighbors {
			nb[k] = struct{}{}
		}
		out = append(out, &TopologyNode{
			AgentID:   n.AgentID,
			Role:      n.Role,
			Neighbors: nb,
			JoinedAt:  n.JoinedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// GetActiveNodes returns all nodes (present in the live graph).
func (t *TopologyManager) GetActiveNodes() []*TopologyNode {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ids := t.agentIDsUnlocked()
	out := make([]*TopologyNode, 0, len(ids))
	for _, id := range ids {
		n := t.nodes[id]
		if n == nil {
			continue
		}
		nb := make(map[string]struct{}, len(n.Neighbors))
		for k := range n.Neighbors {
			nb[k] = struct{}{}
		}
		out = append(out, &TopologyNode{
			AgentID:   n.AgentID,
			Role:      n.Role,
			Neighbors: nb,
			JoinedAt:  n.JoinedAt,
		})
	}
	return out
}

// IsConnected is true when the graph is empty, a singleton, or one BFS component.
func (t *TopologyManager) IsConnected() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.nodes) <= 1 {
		return true
	}
	ids := t.agentIDsUnlocked()
	start := ids[0]
	seen := map[string]struct{}{start: {}}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for peer := range t.nodes[cur].Neighbors {
			if _, ok := seen[peer]; ok {
				continue
			}
			seen[peer] = struct{}{}
			queue = append(queue, peer)
		}
	}
	return len(seen) == len(t.nodes)
}

// GetConnectionCount returns the number of undirected edges.
func (t *TopologyManager) GetConnectionCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var sum int
	for _, n := range t.nodes {
		sum += len(n.Neighbors)
	}
	return sum / 2
}

// GetAverageConnections returns mean degree (0 if no nodes).
func (t *TopologyManager) GetAverageConnections() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.nodes) == 0 {
		return 0
	}
	var sum int
	for _, n := range t.nodes {
		sum += len(n.Neighbors)
	}
	return float64(sum) / float64(len(t.nodes))
}
