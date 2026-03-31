package memory

import (
	"container/heap"
	"math"
	"math/rand"
	"sync"
	"time"
)

// DistanceMetric selects vector distance for HNSW.
type DistanceMetric int

const (
	// CosineDistance uses 1 - cos(theta); for L2-normalized vectors equals 1 - dot(a,b).
	CosineDistance DistanceMetric = iota
	// EuclideanDistance is L2.
	EuclideanDistance
)

func distanceCosine(a, b []float32) float32 {
	if len(a) != len(b) {
		return math.MaxFloat32
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 1
	}
	cos := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if cos > 1 {
		cos = 1
	}
	if cos < -1 {
		cos = -1
	}
	return float32(1 - cos)
}

func distanceEuclidean(a, b []float32) float32 {
	if len(a) != len(b) {
		return math.MaxFloat32
	}
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return float32(math.Sqrt(s))
}

type hnswNode struct {
	id        uint64
	vector    []float32
	level     int
	neighbors [][]uint64
	deleted   bool
}

type idDist struct {
	id   uint64
	dist float32
}

type candHeap []idDist

func (c candHeap) Len() int           { return len(c) }
func (c candHeap) Less(i, j int) bool { return c[i].dist < c[j].dist }
func (c candHeap) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }
func (c *candHeap) Push(x any)        { *c = append(*c, x.(idDist)) }
func (c *candHeap) Pop() any {
	old := *c
	n := len(old)
	x := old[n-1]
	*c = old[:n-1]
	return x
}

type resMaxHeap []idDist

func (c resMaxHeap) Len() int           { return len(c) }
func (c resMaxHeap) Less(i, j int) bool { return c[i].dist > c[j].dist }
func (c resMaxHeap) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }
func (c *resMaxHeap) Push(x any)        { *c = append(*c, x.(idDist)) }
func (c *resMaxHeap) Pop() any {
	old := *c
	n := len(old)
	x := old[n-1]
	*c = old[:n-1]
	return x
}

// HNSWIndex is a hierarchical navigable small world graph for approximate nearest neighbor search.
type HNSWIndex struct {
	mu             sync.RWMutex
	dim            int
	m              int
	mMax           int
	mMax0          int
	mL             float64
	efConstruction int
	dist           func(a, b []float32) float32
	nodes          map[uint64]*hnswNode
	entryPoint     uint64
	maxLevel       int
	hasEntry       bool
	efSearch       int
	rng            *rand.Rand
}

// HNSWStats summarizes index configuration and size.
type HNSWStats struct {
	NodeCount   int
	MaxLevel    int
	Dimensions  int
	M           int
	EfConstruct int
	EfSearch    int
}

// NewHNSWIndex creates an index with sensible defaults (M=16, mL=1/ln(2)).
func NewHNSWIndex(dim int, metric DistanceMetric) *HNSWIndex {
	m := 16
	mL := 1 / math.Log(2)
	df := distanceCosine
	if metric == EuclideanDistance {
		df = distanceEuclidean
	}
	return &HNSWIndex{
		dim:            dim,
		m:              m,
		mMax:           m,
		mMax0:          2 * m,
		mL:             mL,
		efConstruction: 200,
		efSearch:       64,
		dist:           df,
		nodes:          make(map[uint64]*hnswNode),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (h *HNSWIndex) randomLevel() int {
	u := h.rng.Float64()
	if u <= 0 {
		u = 1e-12
	}
	lv := int(math.Floor(-math.Log(u) * h.mL))
	if lv < 0 {
		lv = 0
	}
	if lv > 16 {
		lv = 16
	}
	return lv
}

func (h *HNSWIndex) layerMaxNeighbors(level int) int {
	if level == 0 {
		return h.mMax0
	}
	return h.mMax
}

// Insert adds or updates a vector under id. Vector length must match dim.
func (h *HNSWIndex) Insert(id uint64, vector []float32) error {
	if len(vector) != h.dim {
		return errDimMismatch
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.insertUnderLock(id, vector)
}

func (h *HNSWIndex) insertUnderLock(id uint64, vector []float32) error {
	vecCopy := append([]float32(nil), vector...)

	if !h.hasEntry {
		lv := h.randomLevel()
		n := &hnswNode{
			id:        id,
			vector:    vecCopy,
			level:     lv,
			neighbors: make([][]uint64, lv+1),
		}
		h.nodes[id] = n
		h.entryPoint = id
		h.maxLevel = lv
		h.hasEntry = true
		return nil
	}

	if old, ok := h.nodes[id]; ok {
		old.vector = vecCopy
		old.deleted = false
		return nil
	}

	newLevel := h.randomLevel()
	curMax := h.maxLevel
	ep := h.entryPoint

	for lc := curMax; lc > newLevel; lc-- {
		res := h.searchLayerBase(ep, vecCopy, lc, 1)
		if len(res) == 0 {
			break
		}
		ep = res[0].id
	}

	n := &hnswNode{
		id:        id,
		vector:    vecCopy,
		level:     newLevel,
		neighbors: make([][]uint64, newLevel+1),
	}
	h.nodes[id] = n

	for lc := minInt(newLevel, curMax); lc >= 0; lc-- {
		ef := h.efConstruction
		if ef < h.m {
			ef = h.m
		}
		W := h.searchLayerBase(ep, vecCopy, lc, ef)
		if len(W) == 0 {
			continue
		}
		ep = W[0].id
		if lc <= newLevel {
			mconn := h.layerMaxNeighbors(lc)
			neighbors := h.nearestDistinct(W, mconn, id)
			n.neighbors[lc] = neighbors
			for _, nb := range neighbors {
				h.addReverse(nb, id, lc, mconn)
			}
		}
	}

	if newLevel > curMax {
		h.maxLevel = newLevel
		h.entryPoint = id
	}
	return nil
}

// Rebuild clears the graph and re-inserts all non-deleted nodes.
func (h *HNSWIndex) Rebuild() {
	h.mu.Lock()
	defer h.mu.Unlock()
	type pair struct {
		id uint64
		v  []float32
	}
	var pairs []pair
	for id, n := range h.nodes {
		if n.deleted {
			continue
		}
		pairs = append(pairs, pair{id: id, v: append([]float32(nil), n.vector...)})
	}
	h.nodes = make(map[uint64]*hnswNode)
	h.hasEntry = false
	h.entryPoint = 0
	h.maxLevel = 0
	h.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	for _, p := range pairs {
		_ = h.insertUnderLock(p.id, p.v)
	}
}

// Stats returns index dimensions and hyperparameters.
func (h *HNSWIndex) Stats() HNSWStats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	efs := h.efSearch
	if efs <= 0 {
		efs = 64
	}
	return HNSWStats{
		NodeCount:   h.nonDeletedCountLocked(),
		MaxLevel:    h.maxLevel,
		Dimensions:  h.dim,
		M:           h.m,
		EfConstruct: h.efConstruction,
		EfSearch:    efs,
	}
}

func (h *HNSWIndex) nonDeletedCountLocked() int {
	n := 0
	for _, node := range h.nodes {
		if node != nil && !node.deleted {
			n++
		}
	}
	return n
}

// Clear removes all nodes from the index.
func (h *HNSWIndex) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nodes = make(map[uint64]*hnswNode)
	h.hasEntry = false
	h.entryPoint = 0
	h.maxLevel = 0
}

// Has reports whether a non-deleted id exists in the index.
func (h *HNSWIndex) Has(id uint64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n, ok := h.nodes[id]
	return ok && n != nil && !n.deleted
}

// Size returns the number of non-deleted nodes.
func (h *HNSWIndex) Size() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.nonDeletedCountLocked()
}

func (h *HNSWIndex) nearestDistinct(W []idDist, m int, self uint64) []uint64 {
	out := make([]uint64, 0, m)
	for _, x := range W {
		if x.id == self {
			continue
		}
		out = append(out, x.id)
		if len(out) >= m {
			break
		}
	}
	return out
}

func (h *HNSWIndex) addReverse(nbID, newID uint64, lc int, mconn int) {
	nb := h.nodes[nbID]
	if nb == nil || nb.deleted || lc > nb.level {
		return
	}
	for _, x := range nb.neighbors[lc] {
		if x == newID {
			return
		}
	}
	nb.neighbors[lc] = append(nb.neighbors[lc], newID)
	if len(nb.neighbors[lc]) > mconn {
		nb.neighbors[lc] = h.pruneNeighbors(nb, lc, mconn)
	}
}

func (h *HNSWIndex) pruneNeighbors(nb *hnswNode, lc int, mconn int) []uint64 {
	type nd struct {
		id   uint64
		dist float32
	}
	arr := make([]nd, 0, len(nb.neighbors[lc]))
	for _, oid := range nb.neighbors[lc] {
		on := h.nodes[oid]
		if on == nil || on.deleted {
			continue
		}
		arr = append(arr, nd{id: oid, dist: h.dist(nb.vector, on.vector)})
	}
	for i := 0; i < len(arr); i++ {
		for j := i + 1; j < len(arr); j++ {
			if arr[j].dist < arr[i].dist {
				arr[i], arr[j] = arr[j], arr[i]
			}
		}
	}
	out := make([]uint64, 0, mconn)
	for i := 0; i < len(arr) && len(out) < mconn; i++ {
		out = append(out, arr[i].id)
	}
	return out
}

func (h *HNSWIndex) searchLayerBase(ep uint64, q []float32, lc, ef int) []idDist {
	if !h.hasEntry {
		return nil
	}
	visited := make(map[uint64]struct{}, ef*4)
	var cands candHeap
	var res resMaxHeap
	heap.Init(&cands)
	heap.Init(&res)

	start := h.nodes[ep]
	if start == nil || start.deleted {
		return nil
	}
	d0 := h.dist(q, start.vector)
	heap.Push(&cands, idDist{id: ep, dist: d0})
	heap.Push(&res, idDist{id: ep, dist: d0})
	visited[ep] = struct{}{}

	for cands.Len() > 0 {
		c := heap.Pop(&cands).(idDist)
		furthest := float32(-1)
		if res.Len() > 0 {
			furthest = res[0].dist
		}
		if c.dist > furthest && res.Len() >= ef {
			break
		}
		cur := h.nodes[c.id]
		if cur == nil || lc > cur.level {
			continue
		}
		for _, nb := range cur.neighbors[lc] {
			if _, ok := visited[nb]; ok {
				continue
			}
			visited[nb] = struct{}{}
			nbn := h.nodes[nb]
			if nbn == nil || nbn.deleted {
				continue
			}
			d := h.dist(q, nbn.vector)
			furthest2 := float32(-1)
			if res.Len() > 0 {
				furthest2 = res[0].dist
			}
			if res.Len() < ef || d < furthest2 {
				heap.Push(&cands, idDist{id: nb, dist: d})
				heap.Push(&res, idDist{id: nb, dist: d})
				for res.Len() > ef {
					heap.Pop(&res)
				}
			}
		}
	}
	n := res.Len()
	out := make([]idDist, n)
	for i := 0; i < n; i++ {
		out[n-1-i] = heap.Pop(&res).(idDist)
	}
	// out: farthest -> nearest from pops; reverse to nearest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// NeighborHit is one HNSW search neighbor (internal index result).
type NeighborHit struct {
	ID       uint64
	Distance float32
}

// Search returns up to k nearest neighbors at layer 0 using ef expansion.
func (h *HNSWIndex) Search(query []float32, k int, ef int) []NeighborHit {
	if k <= 0 {
		return nil
	}
	if ef < k {
		ef = k
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.hasEntry || len(query) != h.dim {
		return nil
	}
	ep := h.entryPoint
	for lc := h.maxLevel; lc > 0; lc-- {
		nearest := h.searchLayerBase(ep, query, lc, 1)
		if len(nearest) > 0 {
			ep = nearest[0].id
		}
	}
	cands := h.searchLayerBase(ep, query, 0, ef)
	out := make([]NeighborHit, 0, k)
	for _, c := range cands {
		n := h.nodes[c.id]
		if n == nil || n.deleted {
			continue
		}
		out = append(out, NeighborHit{ID: c.id, Distance: c.dist})
		if len(out) >= k {
			break
		}
	}
	return out
}

// Delete marks a point as removed and strips graph references.
func (h *HNSWIndex) Delete(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, ok := h.nodes[id]
	if !ok {
		return
	}
	n.deleted = true
	for _, node := range h.nodes {
		for lc := 0; lc <= node.level && lc < len(node.neighbors); lc++ {
			nb := node.neighbors[lc][:0]
			for _, x := range node.neighbors[lc] {
				if x != id {
					nb = append(nb, x)
				}
			}
			node.neighbors[lc] = nb
		}
	}
	if h.entryPoint == id {
		h.repickEntryLocked()
	}
}

func (h *HNSWIndex) repickEntryLocked() {
	var best uint64
	maxLv := -1
	for uid, node := range h.nodes {
		if node.deleted {
			continue
		}
		if node.level > maxLv {
			maxLv = node.level
			best = uid
		}
	}
	if maxLv < 0 {
		h.hasEntry = false
		h.entryPoint = 0
		h.maxLevel = 0
		return
	}
	h.entryPoint = best
	h.maxLevel = maxLv
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var errDimMismatch = &dimMismatchError{}

type dimMismatchError struct{}

func (e *dimMismatchError) Error() string { return "memory: vector dimension mismatch" }
