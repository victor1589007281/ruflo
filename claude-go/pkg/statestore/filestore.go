package statestore

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// FileStore 文件后端的 StateStore 实现。
//
// 磁盘布局:
//
//	<root>/kv/<bucket>.json        整文件 map[string]json.RawMessage
//	<root>/log/<bucket>.jsonl      O_APPEND 追加, 每行一条 JSON
//	<root>/blob/<hash前2字符>/<hash>  内容寻址, sha256
//
// 写入纪律: KV 与 Blob 均走"临时文件 + 原子 rename", 永不原地改写;
// Log 走 O_APPEND 单次 write, 崩溃最多留下一个尾部半行 (ReadAll 容忍)。
// 并发: 进程内每 bucket 一把 RWMutex; 跨进程并发写不在本实现保证范围。
type FileStore struct {
	root string

	mu      sync.Mutex               // 保护下面两张锁表
	kvLocks map[string]*sync.RWMutex // bucket -> KV 锁
	logMus  map[string]*sync.Mutex   // bucket -> Log 锁
}

var _ StateStore = (*FileStore)(nil)

// NewFileStore 创建文件后端状态存储, root 为数据根目录 (惰性创建)。
func NewFileStore(root string) *FileStore {
	return &FileStore{
		root:    root,
		kvLocks: make(map[string]*sync.RWMutex),
		logMus:  make(map[string]*sync.Mutex),
	}
}

// validateBucket 校验 bucket 名: 仅允许 [a-zA-Z0-9._-], 且非空。
// 字符白名单天然排除路径分隔符与空字节, 防止路径穿越。
func validateBucket(bucket string) error {
	if bucket == "" {
		return fmt.Errorf("statestore: bucket 名不能为空")
	}
	for _, r := range bucket {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return fmt.Errorf("statestore: 非法 bucket 名 %q (仅允许 [a-zA-Z0-9._-])", bucket)
		}
	}
	return nil
}

// kvLock 取 bucket 对应的 KV 读写锁 (惰性创建)。
func (s *FileStore) kvLock(bucket string) *sync.RWMutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.kvLocks[bucket]
	if !ok {
		l = &sync.RWMutex{}
		s.kvLocks[bucket] = l
	}
	return l
}

// logMu 取 bucket 对应的 Log 互斥锁 (惰性创建)。
func (s *FileStore) logMu(bucket string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.logMus[bucket]
	if !ok {
		l = &sync.Mutex{}
		s.logMus[bucket] = l
	}
	return l
}

// KV 返回文件后端 KV 存储; bucket 名非法时返回的实例所有方法均报错。
func (s *FileStore) KV(bucket string) KVStore {
	if err := validateBucket(bucket); err != nil {
		return badBucketKV{err: err}
	}
	return &fileKV{
		path: filepath.Join(s.root, "kv", bucket+".json"),
		lock: s.kvLock(bucket),
	}
}

// Log 返回文件后端追加日志; bucket 名非法时返回的实例所有方法均报错。
func (s *FileStore) Log(bucket string) AppendLog {
	if err := validateBucket(bucket); err != nil {
		return badBucketLog{err: err}
	}
	return &fileLog{
		path: filepath.Join(s.root, "log", bucket+".jsonl"),
		mu:   s.logMu(bucket),
	}
}

// Blob 返回文件后端内容寻址 Blob 存储。
func (s *FileStore) Blob() BlobStore {
	return &fileBlob{dir: filepath.Join(s.root, "blob")}
}

// ---------- 非法 bucket 名的错误载体 ----------

// badBucketKV bucket 名校验失败时的 KVStore 占位实现: 所有方法返回同一错误。
type badBucketKV struct{ err error }

func (b badBucketKV) Get(string, any) (bool, error) { return false, b.err }
func (b badBucketKV) Put(string, any) error         { return b.err }
func (b badBucketKV) Delete(string) error           { return b.err }
func (b badBucketKV) Keys() ([]string, error)       { return nil, b.err }

// badBucketLog bucket 名校验失败时的 AppendLog 占位实现。
type badBucketLog struct{ err error }

func (b badBucketLog) Append(any) error                      { return b.err }
func (b badBucketLog) ReadAll(func(line []byte) error) error { return b.err }

// ---------- KV ----------

// fileKV 单 bucket 的文件 KV: 整文件 map[string]json.RawMessage。
type fileKV struct {
	path string
	lock *sync.RWMutex
}

// load 读整个 bucket 文件; 文件不存在返回空 map。
func (k *fileKV) load() (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(k.path)
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("statestore: 读取 KV 文件失败: %w", err)
	}
	m := map[string]json.RawMessage{}
	if len(data) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("statestore: 解析 KV 文件 %s 失败: %w", k.path, err)
	}
	return m, nil
}

// save 整文件写回: 临时文件 + 原子 rename, 永不原地改写。
func (k *fileKV) save(m map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("statestore: 序列化 KV bucket 失败: %w", err)
	}
	return atomicWrite(k.path, data)
}

func (k *fileKV) Get(key string, out any) (bool, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	m, err := k.load()
	if err != nil {
		return false, err
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

func (k *fileKV) Put(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("statestore: 序列化 key %q 失败: %w", key, err)
	}
	k.lock.Lock()
	defer k.lock.Unlock()
	m, err := k.load()
	if err != nil {
		return err
	}
	m[key] = raw
	return k.save(m)
}

func (k *fileKV) Delete(key string) error {
	k.lock.Lock()
	defer k.lock.Unlock()
	m, err := k.load()
	if err != nil {
		return err
	}
	if _, ok := m[key]; !ok {
		return nil // 幂等: 不存在视为已删除
	}
	delete(m, key)
	return k.save(m)
}

func (k *fileKV) Keys() ([]string, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	m, err := k.load()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// ---------- Log ----------

// fileLog 单 bucket 的 JSONL 追加日志。
type fileLog struct {
	path string
	mu   *sync.Mutex
}

func (l *fileLog) Append(v any) error {
	line, err := json.Marshal(v) // json.Marshal 不会产生换行, 单条记录恒为单行
	if err != nil {
		return fmt.Errorf("statestore: 序列化日志记录失败: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("statestore: 创建日志目录失败: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("statestore: 打开日志文件失败: %w", err)
	}
	defer f.Close()
	// 单次 write 提交 "line\n": 崩溃最多留下一个尾部半行, 由 ReadAll 容忍。
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("statestore: 追加日志失败: %w", err)
	}
	return nil
}

// maxLogLine 单行日志上限 (16MB), 防止 bufio.Scanner 因超长行报错。
const maxLogLine = 16 * 1024 * 1024

func (l *fileLog) ReadAll(fn func(line []byte) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil // 空日志
	}
	if err != nil {
		return fmt.Errorf("statestore: 打开日志文件失败: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxLogLine)
	var pendingInvalid bool // 已遇到坏行, 等待确认其是否为尾部截断
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue // 空行忽略
		}
		if pendingInvalid {
			// 坏行之后仍有内容 → 非尾部截断, 属数据损坏
			return fmt.Errorf("statestore: 日志 %s 第 %d 行前存在非法 JSON 行 (非尾部截断)", l.path, lineNo)
		}
		if !json.Valid(raw) {
			pendingInvalid = true // 暂定为尾部截断行, 若后面无内容则静默跳过
			continue
		}
		line := make([]byte, len(raw)) // 拷贝: scanner 内部缓冲会被复用
		copy(line, raw)
		if err := fn(line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("statestore: 读取日志失败: %w", err)
	}
	return nil // pendingInvalid 为尾部截断行, 静默跳过 (崩溃安全)
}

// ---------- Blob ----------

// fileBlob 内容寻址 Blob: <dir>/<hash前2字符>/<hash>。
type fileBlob struct {
	dir string
}

// blobPath 由哈希计算存储路径; 哈希必须是 64 位十六进制。
func (b *fileBlob) blobPath(hash string) (string, error) {
	if len(hash) != 64 {
		return "", fmt.Errorf("statestore: 非法 blob 哈希 %q (须为 64 位十六进制)", hash)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", fmt.Errorf("statestore: 非法 blob 哈希 %q: %w", hash, err)
	}
	return filepath.Join(b.dir, hash[:2], hash), nil
}

func (b *fileBlob) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	path, err := b.blobPath(hash)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return hash, nil // 已存在, 内容寻址天然去重
	}
	if err := atomicWrite(path, data); err != nil {
		return "", err
	}
	return hash, nil
}

func (b *fileBlob) Get(hash string) ([]byte, error) {
	path, err := b.blobPath(hash)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("statestore: blob %s 不存在", hash)
	}
	if err != nil {
		return nil, fmt.Errorf("statestore: 读取 blob 失败: %w", err)
	}
	return data, nil
}

func (b *fileBlob) Has(hash string) bool {
	path, err := b.blobPath(hash)
	if err != nil {
		return false
	}
	_, statErr := os.Stat(path)
	return statErr == nil
}

// ---------- 公共工具 ----------

// atomicWrite 原子写文件: 同目录临时文件 + fsync + rename, 永不原地改写。
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("statestore: 创建目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("statestore: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后不存在, 失败时清理残骸
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("statestore: 写临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("statestore: 落盘临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("statestore: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("statestore: 原子替换失败: %w", err)
	}
	return nil
}
