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
	node, err := d.readNodeAt(ent)
	if err != nil {
		return nil, false
	}

	// 4. 写入缓存
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
			var skip interface{}
			if err := decoder.Decode(&skip); err != nil {
				return err
			}
		}
	}

	// 写入索引
	idxData, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}
	if err := os.WriteFile(idxPath, idxData, 0644); err != nil {
		return fmt.Errorf("write index: %w", err)
	}

	return nil
}

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
