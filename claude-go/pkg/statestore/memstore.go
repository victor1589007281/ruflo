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

func (k *memKV) Delete(key string) error {
	k.store.mu.Lock()
	defer k.store.mu.Unlock()
	if m, ok := k.store.kv[k.bucket]; ok {
		delete(m, key) // 幂等: 不存在视为已删除
	}
	return nil
}

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

func (b *memBlob) Get(hash string) ([]byte, error) {
	b.store.mu.RLock()
	defer b.store.mu.RUnlock()
	data, ok := b.store.blobs[hash]
	if !ok {
		return nil, fmt.Errorf("statestore: blob %s 不存在", hash)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

func (b *memBlob) Has(hash string) bool {
	b.store.mu.RLock()
	defer b.store.mu.RUnlock()
	_, ok := b.store.blobs[hash]
	return ok
}
