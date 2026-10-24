package statestore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// MemStore 纯内存后端的 StateStore 实现 (测试/嵌入用)。
// 语义与 FileStore 一致: 值以 JSON 编码存取 (深拷贝语义, 调用方持有的对象
// 与存储内部无共享), bucket 名走同一套消毒规则。并发安全。
type MemStore struct {
	mu    sync.RWMutex
	kv    map[string]map[string]json.RawMessage // bucket -> key -> JSON
	logs  map[string][][]byte                   // bucket -> 行列表
	blobs map[string][]byte                     // hash -> 数据
}

var _ StateStore = (*MemStore)(nil)

// NewMemStore 创建纯内存状态存储。
func NewMemStore() *MemStore {
	return &MemStore{
		kv:    make(map[string]map[string]json.RawMessage),
		logs:  make(map[string][][]byte),
		blobs: make(map[string][]byte),
	}
}

// KV 返回内存 KV 存储; bucket 名非法时返回的实例所有方法均报错。
func (s *MemStore) KV(bucket string) KVStore {
	if err := validateBucket(bucket); err != nil {
		return badBucketKV{err: err}
	}
	return &memKV{store: s, bucket: bucket}
}

// Log 返回内存追加日志; bucket 名非法时返回的实例所有方法均报错。
func (s *MemStore) Log(bucket string) AppendLog {
	if err := validateBucket(bucket); err != nil {
		return badBucketLog{err: err}
	}
	return &memLog{store: s, bucket: bucket}
}

// Blob 返回内存内容寻址 Blob 存储。
func (s *MemStore) Blob() BlobStore {
	return &memBlob{store: s}
}

// ---------- KV ----------

type memKV struct {
	store  *MemStore
	bucket string
}

// Get 读取 key 并反序列化进 out; 键不存在返回 (false, nil) —— 不是错误。
func (k *memKV) Get(key string, out any) (bool, error) {
	k.store.mu.RLock()
	defer k.store.mu.RUnlock()
	m, ok := k.store.kv[k.bucket]
	if !ok {
		return false, nil
	}
	raw, ok := m[key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("statestore: 反序列化 key %q 失败: %w", key, err)
	}
	return true, nil
}

// Put 把 v 序列化成 JSON 后写入 key, bucket 不存在时惰性创建。
//
// 先序列化再取锁: 序列化不碰共享状态, 放到锁里只会把临界区拉长。
func (k *memKV) Put(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("statestore: 序列化 key %q 失败: %w", key, err)
	}
	k.store.mu.Lock()
	defer k.store.mu.Unlock()
	m, ok := k.store.kv[k.bucket]
	if !ok {
		m = make(map[string]json.RawMessage)
		k.store.kv[k.bucket] = m
	}
	m[key] = raw
	return nil
}

// Delete 删除 key; 键不存在也返回 nil (幂等)。
func (k *memKV) Delete(key string) error {
	k.store.mu.Lock()
	defer k.store.mu.Unlock()
	if m, ok := k.store.kv[k.bucket]; ok {
		delete(m, key) // 幂等: 不存在视为已删除
	}
	return nil
}

// Keys 返回 bucket 内全部键, 已排序 —— 顺序稳定, 调用方不必再排一次。
func (k *memKV) Keys() ([]string, error) {
	k.store.mu.RLock()
	defer k.store.mu.RUnlock()
	m := k.store.kv[k.bucket]
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// ---------- Log ----------

type memLog struct {
	store  *MemStore
	bucket string
}

// Append 把 v 序列化成一行 JSON 追加到 bucket 日志尾部。
func (l *memLog) Append(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("statestore: 序列化日志记录失败: %w", err)
	}
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	l.store.logs[l.bucket] = append(l.store.logs[l.bucket], line)
	return nil
}

// ReadAll 按写入顺序把每行交给 fn; fn 返回错误即中止并原样透出。
//
// 先深拷贝出快照再释放读锁 —— 回调期间不持锁, 回调里再调本存储不会自锁死,
// 同时保证遍历过程中新 Append 的行不会插进本次快照 (读到一半的日志会破坏
// "按序"这个前提, 而调用方正是靠它做重放)。
func (l *memLog) ReadAll(fn func(line []byte) error) error {
	// 先快照再回调, 避免回调期间持锁 (回调方可能再次调用本存储)。
	l.store.mu.RLock()
	lines := l.store.logs[l.bucket]
	snapshot := make([][]byte, len(lines))
	for i, ln := range lines {
		cp := make([]byte, len(ln))
		copy(cp, ln)
		snapshot[i] = cp
	}
	l.store.mu.RUnlock()
	for _, line := range snapshot {
		if err := fn(line); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Blob ----------

type memBlob struct {
	store *MemStore
}

// Put 按内容寻址写入 blob, 返回 sha256 十六进制哈希; 同内容重复写入直接返回已有哈希。
//
// 算法解读：
//
//	原理：内容寻址存储 —— 键不是调用方给的, 而是数据本身的 sha256。于是"同一个内容
//	      写两次"天然收敛成一份, 去重不需要任何额外索引或比对。
//	关键不变量：写入前必须**拷贝**入参 (调用方随后改自己的切片不能影响存储),
//	      读取时同样要拷贝出参 (调用方改返回值不能污染存储)。少了任一侧, 存储就与
//	      调用方共享底层数组 —— 表现为"某处莫名其妙改了一条别人写的 blob"。
//	代价与边界：每次写都全量算一次 sha256 (O(n)), 且数据常驻内存、无淘汰, 只适合
//	      测试/嵌入场景; 大 blob 会一直占着。
//	设计权衡：先去重再拷贝 (命中时省一次分配), 而不是先拷贝再判重。
func (b *memBlob) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	if _, ok := b.store.blobs[hash]; ok {
		return hash, nil // 去重
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	b.store.blobs[hash] = cp
	return hash, nil
}

// Get 按哈希取 blob 的**拷贝**; 不存在返回错误。
func (b *memBlob) Get(hash string) ([]byte, error) {
	b.store.mu.RLock()
	defer b.store.mu.RUnlock()
	data, ok := b.store.blobs[hash]
	if !ok {
		return nil, fmt.Errorf("statestore: blob %s 不存在", hash)
	}
	// 必须拷贝出参: 直接返回存储内的切片会让调用方的修改污染存储, 而 blob 是内容寻址的,
	// 内容被改掉之后哈希与数据就对不上了。
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

// Has 判断哈希对应的 blob 是否存在。
func (b *memBlob) Has(hash string) bool {
	b.store.mu.RLock()
	defer b.store.mu.RUnlock()
	_, ok := b.store.blobs[hash]
	return ok
}
