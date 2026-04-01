// Package memory 提供基于 HNSW（Hierarchical Navigable Small World，层次化可导航小世界图）
// 的近似最近邻（ANN）向量检索能力。
//
// # 算法来源与核心思想
//
// HNSW 由 Malkov & Yashunin 在论文
// "Efficient and robust approximate nearest neighbor search using Hierarchical Navigable Small World graphs"
// (arXiv:1603.09320, 2018) 中系统阐述。其思想是在向量空间上构造多层图：
//   - 底层（layer 0）边更密，保证召回；高层边更稀，形成“高速公路”，类似跳表的多层结构。
//   - 查询时从最高层的入口点出发，在每一层做贪心式局部搜索，逐层下降到 layer 0，再用较大的 ef 扩展候选集。
//   - 插入时为每个新点随机采样层级 l，常用分布为 P(level ≥ l) ∝ exp(-l / mL)，等价于对指数分布采样；
//     本实现中 mL 默认取 1/ln(2)（与论文中 M 相关的常数可调），randomLevel 使用 -ln(U)*mL 的 floor。
//
// # 复杂度（典型与论文一致的数量级）
//
// 在良好参数下，单次插入与检索的期望复杂度约为 O(log N) 量级的图遍历（与层数、M、ef 有关）；
// 最坏情况受图度数与 ef 上界约束。空间上除存向量外，每层至多 O(M) 条出边，总边数约 O(N·M·层数)。
//
// # 本文件实现说明
//
// Insert 完成层级选择、自上而下寻近邻、建立双向边并在超限时用 pruneNeighbors 做度数裁剪；
// Search 自顶向下贪心后于第 0 层用 ef 控制 beam 宽度；Delete 采用软删除（deleted 标记）并清理全图指向该点的边。
package memory

import (
	"container/heap"
	"math"
	"math/rand"
	"sync"
	"time"
)

// DistanceMetric 指定 HNSW 中向量间距离度量方式。
type DistanceMetric int

const (
	// CosineDistance uses 1 - cos(theta); for L2-normalized vectors equals 1 - dot(a,b).
	CosineDistance DistanceMetric = iota
	// EuclideanDistance is L2.
	EuclideanDistance
)

// distanceCosine 计算余弦距离 1-cos(θ)；若向量已 L2 归一化，等价于 1-点积。
// 时间复杂度 O(d)，d 为维度；额外空间 O(1)。
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

// distanceEuclidean 计算欧氏距离（L2 范数）。
// 时间复杂度 O(d)，空间 O(1)。
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

// hnswNode 表示 HNSW 图中的一个顶点。
//   - id: 业务侧向量唯一标识
//   - vector: 原始向量（维度须与索引 dim 一致）
//   - level: 该点参与的最高层编号（0..level）
//   - neighbors[lc]: 第 lc 层的出边邻居 id 列表（双向维护由 Insert/addReverse/pruneNeighbors 完成）
//   - deleted: 软删除标记，Search 会跳过；边在 Delete 中显式剔除
type hnswNode struct {
	id        uint64
	vector    []float32
	level     int
	neighbors [][]uint64
	deleted   bool
}

// idDist 为搜索过程中的（节点 id，到查询向量的距离）对。
type idDist struct {
	id   uint64
	dist float32
}

// candHeap 为小根堆：候选集中当前距离查询最近的点优先扩展（贪心搜索前沿）。
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

// resMaxHeap 为“最大堆”语义（Less 使堆顶为当前 ef 个候选中距离最远者），用于维护动态 top-ef。
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

// HNSWIndex 为层次化可导航小世界图索引，用于近似 K 近邻搜索。
//
// 字段含义：
//   - mu: 读写锁，保护并发访问
//   - dim: 向量维度
//   - m / mMax / mMax0: 每层（及第 0 层）最大邻居数上界，控制图稀疏度与精度
//   - mL: 层级随机分布参数，与论文中 1/ln(M) 类指数衰减选取层数一致思想
//   - efConstruction: 插入时每层 beam 宽度，越大图质量通常越好、构建越慢
//   - dist: 距离函数（余弦或欧氏）
//   - nodes: id -> 节点
//   - entryPoint / maxLevel / hasEntry: 全局入口与最高层信息
//   - efSearch: 查询时第 0 层扩展宽度（可由调用方 Search 的 ef 覆盖）
//   - rng: 层级随机数源
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

// HNSWStats 汇总索引规模与超参数，便于监控与调优。
type HNSWStats struct {
	NodeCount   int
	MaxLevel    int
	Dimensions  int
	M           int
	EfConstruct int
	EfSearch    int
}

// NewHNSWIndex 创建索引，默认 M=16、mL=1/ln(2)、efConstruction=200、efSearch=64。
// 时间复杂度 O(1)，空间 O(1)（惰性分配 nodes）。
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

// randomLevel 按指数型分布采样新点层级：floor(-ln(U) * mL)，并截断到 [0,16]。
// 期望层数与 mL 成反比，对应论文中“高层节点稀少、低层稠密”的性质。
// 时间复杂度 O(1)。
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

// layerMaxNeighbors 返回指定层允许的最大邻居数；第 0 层通常更大以加强连通性。
func (h *HNSWIndex) layerMaxNeighbors(level int) int {
	if level == 0 {
		return h.mMax0
	}
	return h.mMax
}

// Insert 在索引中插入或更新 id 对应向量（维度须等于 dim）。
//
// 算法要点（与 HNSW 论文一致）：
//  1. 若为空图，随机层级创建首节点并设为入口。
//  2. 若 id 已存在则更新向量并清除 deleted。
//  3. 否则采样 newLevel；从 maxLevel 向下到 newLevel+1，每层用 ef=1 贪心找最近点作为入口 ep。
//  4. 从 min(newLevel, maxLevel) 降到 0：searchLayerBase 取 efConstruction 个近邻，用 nearestDistinct 取每层至多 m 个邻居，
//     建立新点→邻居边，并对邻居 addReverse；超限时 pruneNeighbors 按距离保留最近子集（启发式裁剪，保持稀疏与连通）。
//  5. 若 newLevel 超过当前 maxLevel，提升入口为新点。
//
// 期望时间复杂度约 O(log N · efConstruction · M) 量级（与层数、度数相关）；空间复制向量 O(d)。
func (h *HNSWIndex) Insert(id uint64, vector []float32) error {
	if len(vector) != h.dim {
		return errDimMismatch
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.insertUnderLock(id, vector)
}

// insertUnderLock 在已持写锁前提下执行插入逻辑。
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

// Rebuild 清空图并对所有未删除节点重新插入，用于修复结构或大量删除后的重整。
// 时间复杂度 O(N·插入代价)，额外空间 O(N·d) 存快照。
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

// Stats 返回索引规模与超参数快照。时间复杂度 O(N)（需遍历节点计数）。
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

// nonDeletedCountLocked 在已持锁下统计未删除节点数。O(N)。
func (h *HNSWIndex) nonDeletedCountLocked() int {
	n := 0
	for _, node := range h.nodes {
		if node != nil && !node.deleted {
			n++
		}
	}
	return n
}

// Clear 移除所有节点并重置入口。O(N) 释放 map 引用。
func (h *HNSWIndex) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nodes = make(map[uint64]*hnswNode)
	h.hasEntry = false
	h.entryPoint = 0
	h.maxLevel = 0
}

// Has 判断非删除的 id 是否存在。O(1) 均摊。
func (h *HNSWIndex) Has(id uint64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n, ok := h.nodes[id]
	return ok && n != nil && !n.deleted
}

// Size 返回未删除节点数。O(N)。
func (h *HNSWIndex) Size() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.nonDeletedCountLocked()
}

// nearestDistinct 从已按距离排序的近邻列表 W 中取前 m 个非 self 的 id（论文中启发式选邻的简化实现：按距离优先）。
// 时间复杂度 O(|W|)。
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

// addReverse 在邻居 nb 的第 lc 层加入指向 newID 的反向边；若超过 mconn 则调用 pruneNeighbors 裁剪。
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

// pruneNeighbors 对节点 nb 在层 lc 的邻居按到 nb 向量距离排序，保留最近的 mconn 条边。
// 这是 HNSW 中“启发式邻居裁剪”的简化版（论文 Algorithm 4 的变体），用于在固定度数下维持小世界导航性质。
// 时间复杂度 O(k²)（k 为当前邻居数，此处用简单交换排序）；空间 O(k)。
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

// searchLayerBase 在单层 lc 上从入口 ep 出发，维护大小至多为 ef 的动态近邻集（resMaxHeap）与候选堆（candHeap）。
// 算法：反复弹出最近候选，若其距离已差于结果集中最远点且结果已满 ef 则终止；否则扩展其邻居。
// 对应论文单层 greedy + 动态候选列表搜索；ef 越大召回越高、耗时越长。
// 时间复杂度约 O(ef · M · log ef)（堆操作与邻居扩展）；空间 O(ef + visited)。
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

// NeighborHit 表示一次 Search 返回的邻居及其距离。
type NeighborHit struct {
	ID       uint64
	Distance float32
}

// Search 在 layer 0 返回至多 k 个最近邻。流程：自 maxLevel 至 1 每层 ef=1 贪心下降缩小入口，
// 再在 layer 0 用 ef（不小于 k）做 searchLayerBase 扩展，过滤软删除节点。
//
// 时间复杂度约 O(L·log*) + O(ef·M·log ef)，L 为层数；空间 O(ef)。
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

// Delete 软删除：标记 deleted 并遍历全图移除指向该 id 的边；若删除的是入口则 repickEntryLocked 选最高层存活点为新入口。
// 时间复杂度 O(N·平均度数·层数)；不立即压缩 nodes map，Rebuild 可彻底重建。
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

// repickEntryLocked 在已持锁下将入口改为层级最高的未删除节点；若无则清空索引。
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
