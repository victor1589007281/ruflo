// graphify_diskidx.go — Graphify 按需磁盘索引 + LRU 内存缓存。
//
// 解决全量加载 141MB graph.json 的 OOM 风险：
//   1. 首次使用时把 graph.json 拆分为 nodes.jsonl + edges.jsonl + idx.json
//   2. 内存中只保留索引（id/label/source/target → 磁盘偏移）
//   3. 节点和边按需从磁盘读取，LRU 缓存热点数据
//   4. 默认缓存 10000 节点 + 50000 边，约 10MB 内存
package codeintel

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ============================================================================
// LRU 缓存
// ============================================================================

// lruEntry LRU 缓存条目。
type lruEntry struct {
	key   string
	value interface{}
}

// LRUCache 固定容量的 LRU 缓存（非线程安全，外部加锁）。
type LRUCache struct {
	capacity int
	cache    map[string]*lruEntry
	order    []string // 最近使用的在尾部
}

// newLRUCache 创建 LRU 缓存。
func newLRUCache(capacity int) *LRUCache {
	if capacity <= 0 {
		capacity = 1000
	}
	return &LRUCache{
		capacity: capacity,
		cache:    make(map[string]*lruEntry, capacity),
		order:    make([]string, 0, capacity),
	}
}

// get 读取缓存值并把该键标记为最近使用; 未命中返回 (nil, false)。
//
// 注意: 命中路径会原地改写 order (移尾), 所以本方法**不是**只读的 —— 调用方必须
// 持写锁而非读锁, 否则并发 get 会竞争同一个 order 切片。
func (c *LRUCache) get(key string) (interface{}, bool) {
	ent, ok := c.cache[key]
	if !ok {
		return nil, false
	}
	// 移到尾部（最近使用）
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, key)
	return ent.value, true
}

// set 写入或更新缓存条目, 容量满时淘汰最久未使用的键。
//
// 算法解读：
//
//	原理：固定容量 LRU —— 用 order 切片维护使用顺序 (尾部最新), 命中与写入都把键
//	      移到尾部, 容量满时从头部淘汰一个。目的是让热点节点/边常驻内存, 又不让
//	      内存随图规模无界增长 (全量 graph.json 有 141MB, 不能整个装进内存)。
//	关键不变量：order 与 cache 的键集合恒等且 order 无重复 —— 淘汰时取 order[0] 后
//	      必须同时 delete(cache, ...), 否则 order 里会残留指向已删条目的键, 下次淘汰
//	      就淘汰了个空气, 实际容量被慢慢放大。
//	代价与边界：命中与写入都要线性扫描 order 定位键 (O(容量)), 容量取 10000 节点 /
//	      50000 边时这是每次查询的固定开销; 非线程安全, 锁由 DiskGraphIndex.mu 在外层加。
//	      注意: 零值 LRUCache{}(capacity=0, order 为空) 走到淘汰分支会 panic —— 只能
//	      经 newLRUCache 构造, 不要直接 new 一个空结构体。
//	设计权衡：选 slice 而非 container/list + map[string]*list.Element 的组合, 是不想为
//	      O(1) 定位引入第二个数据结构; 用 O(容量) 的移动代价换实现最短、最好读。
func (c *LRUCache) set(key string, value interface{}) {
	if ent, ok := c.cache[key]; ok {
		ent.value = value
		// 移到尾部
		for i, k := range c.order {
			if k == key {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		c.order = append(c.order, key)
		return
	}
	if len(c.cache) >= c.capacity {
		// 淘汰最久未使用的（头部）
		evictKey := c.order[0]
		c.order = c.order[1:]
		delete(c.cache, evictKey)
	}
	c.cache[key] = &lruEntry{key: key, value: value}
	c.order = append(c.order, key)
}

// len 返回当前缓存条目数 (仅用于统计展示)。
func (c *LRUCache) len() int {
	return len(c.cache)
}

// ============================================================================
// 磁盘索引
// ============================================================================

// diskIndexEntry 单个记录的磁盘位置。
type diskIndexEntry struct {
	Offset int64 `json:"off"`
	Length int   `json:"len"`
}

// diskIndex 内存中的索引结构。
type diskIndex struct {
	NodesByID     map[string]diskIndexEntry   `json:"nid"`
	NodesByLabel  map[string][]diskIndexEntry `json:"lbl"`
	EdgesBySource map[string][]diskIndexEntry `json:"src"`
	EdgesByTarget map[string][]diskIndexEntry `json:"tgt"`
	BuiltAt       int64                       `json:"ts"`
}

// DiskGraphIndex 按需磁盘图索引 + LRU 缓存。
type DiskGraphIndex struct {
	repoPath string
	mu       sync.RWMutex

	nodesFile *os.File
	edgesFile *os.File
	idx       diskIndex

	nodeCache *LRUCache // id -> *GraphNode
	edgeCache *LRUCache // "src:target:rel" -> *GraphLink (unused but reserved)
	listCache *LRUCache // "src:"+source -> []*GraphLink
	inCache   *LRUCache // "tgt:"+target -> []*GraphLink

	metrics *MetricsCollector
}

// OpenDiskGraphIndex 打开或构建磁盘索引。
func OpenDiskGraphIndex(repoPath string, maxCacheNodes, maxCacheEdges int) (*DiskGraphIndex, error) {
	idxPath := filepath.Join(repoPath, "graphify-out", "graph.idx.json")
	nodesPath := filepath.Join(repoPath, "graphify-out", "graph.nodes.jsonl")
	edgesPath := filepath.Join(repoPath, "graphify-out", "graph.edges.jsonl")
	graphPath := filepath.Join(repoPath, "graphify-out", "graph.json")

	// 检查是否需要重建索引
	needBuild := false
	if _, err := os.Stat(idxPath); err != nil {
		needBuild = true
	} else {
		graphStat, _ := os.Stat(graphPath)
		idxStat, _ := os.Stat(idxPath)
		if graphStat != nil && idxStat != nil && graphStat.ModTime().After(idxStat.ModTime()) {
			needBuild = true
		}
	}
	if needBuild {
		if err := buildDiskIndex(repoPath); err != nil {
			return nil, err
		}
	}

	nf, err := os.Open(nodesPath)
	if err != nil {
		return nil, fmt.Errorf("open nodes.jsonl: %w", err)
	}
	defer func() {
		if err != nil {
			nf.Close()
		}
	}()

	ef, err := os.Open(edgesPath)
	if err != nil {
		return nil, fmt.Errorf("open edges.jsonl: %w", err)
	}
	defer func() {
		if err != nil {
			ef.Close()
		}
	}()

	data, err := os.ReadFile(idxPath)
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	var idx diskIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("decode index: %w", err)
	}

	return &DiskGraphIndex{
		repoPath:  repoPath,
		nodesFile: nf,
		edgesFile: ef,
		idx:       idx,
		nodeCache: newLRUCache(maxCacheNodes),
		edgeCache: newLRUCache(maxCacheEdges),
		listCache: newLRUCache(maxCacheEdges / 2),
		inCache:   newLRUCache(maxCacheEdges / 2),
		metrics:   NewMetricsCollector(repoPath),
	}, nil
}

// Close 关闭磁盘文件。
func (d *DiskGraphIndex) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var firstErr error
	if d.nodesFile != nil {
		if err := d.nodesFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if d.edgesFile != nil {
		if err := d.edgesFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// GetNode 按需读取节点（带缓存）。
func (d *DiskGraphIndex) GetNode(id string) (*GraphNode, bool) {
	// 1. 缓存
	d.mu.RLock()
	if v, ok := d.nodeCache.get(id); ok {
		d.mu.RUnlock()
		if d.metrics != nil {
			d.metrics.RecordCacheHit("node")
		}
		return v.(*GraphNode), true
	}
	d.mu.RUnlock()
	if d.metrics != nil {
		d.metrics.RecordCacheMiss("node")
	}

	// 2. 索引
	ent, ok := d.idx.NodesByID[id]
	if !ok {
		return nil, false
	}

	// 3. 磁盘读取
	// 磁盘 IO 刻意放在读锁**之外**: readNodeAt 用 ReadAt (不动文件游标, 并发安全),
	// 持锁做一次冷读会把其他所有查询一起阻塞在 IO 上。
	node, err := d.readNodeAt(ent)
	if err != nil {
		return nil, false
	}

	// 4. 写入缓存
	// 回填必须持写锁: set 会改写 LRU 的 order 切片。
	d.mu.Lock()
	d.nodeCache.set(id, node)
	d.mu.Unlock()
	return node, true
}

// GetNodesByLabel 按标签读取节点（批量，无单节点缓存）。
func (d *DiskGraphIndex) GetNodesByLabel(label string) []*GraphNode {
	ents, ok := d.idx.NodesByLabel[label]
	if !ok {
		return nil
	}
	var out []*GraphNode
	for _, ent := range ents {
		if n, ok := d.GetNodeByOffset(ent); ok {
			out = append(out, n)
		}
	}
	return out
}

// GetNodeByOffset 通过偏移读取节点（不经过 ID 缓存）。
func (d *DiskGraphIndex) GetNodeByOffset(ent diskIndexEntry) (*GraphNode, bool) {
	node, err := d.readNodeAt(ent)
	if err != nil {
		return nil, false
	}
	return node, true
}

// GetEdgesBySource 按 source 读取出边列表（带缓存）。
func (d *DiskGraphIndex) GetEdgesBySource(source string) []*GraphLink {
	// 1. 列表缓存
	d.mu.RLock()
	if v, ok := d.listCache.get("src:" + source); ok {
		d.mu.RUnlock()
		if d.metrics != nil {
			d.metrics.RecordCacheHit("list")
		}
		return v.([]*GraphLink)
	}
	d.mu.RUnlock()
	if d.metrics != nil {
		d.metrics.RecordCacheMiss("list")
	}

	// 2. 索引
	ents, ok := d.idx.EdgesBySource[source]
	if !ok {
		return nil
	}

	// 3. 批量磁盘读取
	links := make([]*GraphLink, 0, len(ents))
	for _, ent := range ents {
		link, err := d.readLinkAt(ent)
		if err != nil {
			continue
		}
		links = append(links, link)
	}

	// 4. 写入缓存
	d.mu.Lock()
	d.listCache.set("src:"+source, links)
	d.mu.Unlock()
	return links
}

// GetEdgesByTarget 按 target 读入边列表（带缓存）。
func (d *DiskGraphIndex) GetEdgesByTarget(target string) []*GraphLink {
	d.mu.RLock()
	if v, ok := d.inCache.get("tgt:" + target); ok {
		d.mu.RUnlock()
		if d.metrics != nil {
			d.metrics.RecordCacheHit("in")
		}
		return v.([]*GraphLink)
	}
	d.mu.RUnlock()
	if d.metrics != nil {
		d.metrics.RecordCacheMiss("in")
	}

	ents, ok := d.idx.EdgesByTarget[target]
	if !ok {
		return nil
	}

	links := make([]*GraphLink, 0, len(ents))
	for _, ent := range ents {
		link, err := d.readLinkAt(ent)
		if err != nil {
			continue
		}
		links = append(links, link)
	}

	d.mu.Lock()
	d.inCache.set("tgt:"+target, links)
	d.mu.Unlock()
	return links
}

// GetAllNodeIDs 返回所有节点 ID（用于遍历）。
func (d *DiskGraphIndex) GetAllNodeIDs() []string {
	ids := make([]string, 0, len(d.idx.NodesByID))
	for id := range d.idx.NodesByID {
		ids = append(ids, id)
	}
	return ids
}

// Stats 返回索引统计和缓存大小。
func (d *DiskGraphIndex) Stats() map[string]interface{} {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return map[string]interface{}{
		"nodes_indexed":    len(d.idx.NodesByID),
		"labels_indexed":   len(d.idx.NodesByLabel),
		"sources_indexed":  len(d.idx.EdgesBySource),
		"targets_indexed":  len(d.idx.EdgesByTarget),
		"node_cache_size":  d.nodeCache.len(),
		"edge_list_cache":  d.listCache.len(),
		"in_list_cache":    d.inCache.len(),
	}
}

// ============================================================================
// 私有磁盘读取
// ============================================================================

// readNodeAt 按索引里的 (偏移, 长度) 从 nodes.jsonl 精确读出一条并反序列化为节点。
//
// 注意: 用 ReadAt 而非 Seek+Read 是刻意的 —— ReadAt 不改动文件描述符的当前位置,
// 因此本方法可被多个 goroutine 并发调用而无需持 d.mu, 这正是 GetNode 能把磁盘 IO
// 放在读锁**之外**执行的前提。
func (d *DiskGraphIndex) readNodeAt(ent diskIndexEntry) (*GraphNode, error) {
	buf := make([]byte, ent.Length)
	if _, err := d.nodesFile.ReadAt(buf, ent.Offset); err != nil {
		return nil, err
	}
	if d.metrics != nil {
		d.metrics.RecordDiskRead(int64(ent.Length))
	}
	var node GraphNode
	if err := json.Unmarshal(buf, &node); err != nil {
		return nil, err
	}
	return &node, nil
}

// readLinkAt 按索引偏移从 edges.jsonl 精确读出一条边并反序列化为 GraphLink。
//
// 与 readNodeAt 同构: 同样用 ReadAt 以保证并发安全 (见其注释)。
func (d *DiskGraphIndex) readLinkAt(ent diskIndexEntry) (*GraphLink, error) {
	buf := make([]byte, ent.Length)
	if _, err := d.edgesFile.ReadAt(buf, ent.Offset); err != nil {
		return nil, err
	}
	if d.metrics != nil {
		d.metrics.RecordDiskRead(int64(ent.Length))
	}
	var link GraphLink
	if err := json.Unmarshal(buf, &link); err != nil {
		return nil, err
	}
	return &link, nil
}

// ============================================================================
// 索引构建器（一次性）
// ============================================================================

// buildDiskIndex 一次性把 graph.json 拆成 nodes.jsonl + edges.jsonl + graph.idx.json。
//
// 算法解读：
//
//	原理：把一个大 JSON 对象流式拆解 —— 用 json.Decoder 逐 token 走, 遇 "nodes" 就把
//	      数组里每个元素原样追加进 nodes.jsonl, 同时记下它的 (偏移, 长度) 进内存索引;
//	      "links"/"edges" 同理进 edges.jsonl。之后查询只读索引 + 按偏移 pread, 不再
//	      需要把整图载入内存。
//	关键不变量：写 JSONL 与记索引必须**同一次**写出 (writeJSONL 同时返回 offset/length),
//	      偏移一错后续所有按需读取都会读出半条 JSON; 且 nodes.jsonl 与 edges.jsonl 的
//	      偏移各自独立, 不能混用 (两个文件各有一个游标)。
//	代价与边界：全量扫一遍源文件, 时间与图规模成正比, 但内存只需索引 (id→偏移)。
//	      nodes.jsonl / edges.jsonl / graph.idx.json 三份产物不是原子写出的 —— 中途失败
//	      会留下半截文件, 下次 OpenDiskGraphIndex 只按 idx 与 graph.json 的 mtime 判断
//	      是否重建, 半截的 jsonl 不会被察觉。
//	设计权衡：没有把边按 source/target 分组重排落盘 (那需要先全读一遍再写), 而是保持
//	      原序 + 内存里存同一段的多个偏移表 (EdgesBySource / EdgesByTarget), 用索引的
//	      冗余换一次顺序写。
func buildDiskIndex(repoPath string) error {
	graphPath := filepath.Join(repoPath, "graphify-out", "graph.json")
	nodesPath := filepath.Join(repoPath, "graphify-out", "graph.nodes.jsonl")
	edgesPath := filepath.Join(repoPath, "graphify-out", "graph.edges.jsonl")
	idxPath := filepath.Join(repoPath, "graphify-out", "graph.idx.json")

	f, err := os.Open(graphPath)
	if err != nil {
		return fmt.Errorf("open graph.json: %w", err)
	}
	defer f.Close()

	// 创建输出文件
	nf, err := os.Create(nodesPath)
	if err != nil {
		return fmt.Errorf("create nodes.jsonl: %w", err)
	}
	defer nf.Close()

	ef, err := os.Create(edgesPath)
	if err != nil {
		return fmt.Errorf("create edges.jsonl: %w", err)
	}
	defer ef.Close()

	idx := diskIndex{
		NodesByID:     make(map[string]diskIndexEntry),
		NodesByLabel:  make(map[string][]diskIndexEntry),
		EdgesBySource: make(map[string][]diskIndexEntry),
		EdgesByTarget: make(map[string][]diskIndexEntry),
		BuiltAt:       time.Now().Unix(),
	}

	// 流式解析 graph.json
	decoder := json.NewDecoder(f)

	// 期望 { "nodes": [...], "links": [...] }
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode root: %w", err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("expected object root")
	}

	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key := keyToken.(string)
		switch key {
		case "nodes":
			if err := decodeNodes(decoder, nf, &idx); err != nil {
				return fmt.Errorf("decode nodes: %w", err)
			}
		case "links", "edges":
			if err := decodeEdges(decoder, ef, &idx); err != nil {
				return fmt.Errorf("decode edges: %w", err)
			}
		default:
			// 未知顶层键 (version / meta 之类) 整段丢弃: 索引只服务图遍历, 多存一份
			// 只是给磁盘和后续反序列化加负担。
			var skip interface{}
			if err := decoder.Decode(&skip); err != nil {
				return err
			}
		}
	}

	// 写入索引
	// 索引最后写: jsonl 已先落盘, 它相当于这批产物的"提交点" —— 先有数据后有索引,
	// 索引缺失时上层会重建; 反过来 (先写索引后写数据) 则会留下指向空洞的索引。
	idxData, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}
	if err := os.WriteFile(idxPath, idxData, 0644); err != nil {
		return fmt.Errorf("write index: %w", err)
	}

	return nil
}

// decodeNodes 流式解析 graph.json 的 "nodes" 数组, 逐条落 JSONL 并登记索引。
//
// 算法解读：
//
//	原理：用 json.Decoder 的 token 流手写一个数组遍历 —— 先吃掉 '[', 然后 while
//	      dec.More() 逐元素 Decode 成 RawMessage (原样保留字节, 不重新序列化), 最后
//	      消费 ']'。选 RawMessage 是为了让落盘的 JSONL 与源文件**逐字节一致**, 顺带
//	      避免一次 encode 的开销与字段重排。
//	关键不变量：索引里记的 offset/length 必须与写入 JSONL 的字节完全对应 —— writeJSONL
//	      把 '\n' 也算进 length, 所以 length 是"含换行的整条长度", 读取端拿它开 buffer
//	      刚好覆盖一条 (含换行, json.Unmarshal 容忍尾部空白)。
//	代价与边界：单遍 O(n), 内存只留每条元素的字节切片 (RawMessage) 而非整图。个别节点
//	      Unmarshal 失败时**跳过** (continue) 而不是报错 —— 该节点不会进索引, 后续查询
//	      表现为"查不到", 不会中断整个构建。
//	设计权衡：手写 token 遍历而不是 decoder.Decode 成 map, 是为了不被 json.Decoder 的
//	      缓冲行为牵着走 (它可能一次预读多字节), 从而保证写入偏移是我们自己算的。
func decodeNodes(dec *json.Decoder, f *os.File, idx *diskIndex) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return fmt.Errorf("expected array for nodes")
	}

	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}

		var node struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			continue
		}

		offset, length, err := writeJSONL(f, raw)
		if err != nil {
			return err
		}
		ent := diskIndexEntry{Offset: offset, Length: length}
		// 同一份偏移同时挂到 id 与 label 两张表: 按 id 取是 O(1), 按 label 取是
		// O(该 label 的节点数), 两者都不需要回扫文件。
		idx.NodesByID[node.ID] = ent
		idx.NodesByLabel[node.Label] = append(idx.NodesByLabel[node.Label], ent)
	}

	// 消费关闭 bracket
	token, err = dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != ']' {
		return fmt.Errorf("expected end of nodes array")
	}
	return nil
}

// decodeEdges 流式解析 graph.json 的 "links"/"edges" 数组, 逐条落 JSONL 并登记双向索引。
//
// 与 decodeNodes 是同一套 token 遍历 (其解读见上), 差别只在这里同时登记
// EdgesBySource 与 EdgesByTarget 两张偏移表 —— 一条边被记两次, 换取"按任一端点
// 取邻居"都不需要扫全表。注意: 两个函数除被解析的结构体外几乎逐行相同, 改一处时
// 另一处容易漏 (当前没有任何抽象把它们合并)。
func decodeEdges(dec *json.Decoder, f *os.File, idx *diskIndex) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return fmt.Errorf("expected array for edges")
	}

	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}

		var link struct {
			Source string `json:"source"`
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &link); err != nil {
			continue
		}

		offset, length, err := writeJSONL(f, raw)
		if err != nil {
			return err
		}
		ent := diskIndexEntry{Offset: offset, Length: length}
		idx.EdgesBySource[link.Source] = append(idx.EdgesBySource[link.Source], ent)
		idx.EdgesByTarget[link.Target] = append(idx.EdgesByTarget[link.Target], ent)
	}

	token, err = dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != ']' {
		return fmt.Errorf("expected end of edges array")
	}
	return nil
}

// writeJSONL 将 JSON 数据写入文件并返回偏移和长度（包含换行符）。
func writeJSONL(f *os.File, raw json.RawMessage) (offset int64, length int, err error) {
	offset, err = f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, 0, err
	}
	raw = append(raw, '\n')
	if _, err := f.Write(raw); err != nil {
		return 0, 0, err
	}
	return offset, len(raw), nil
}
