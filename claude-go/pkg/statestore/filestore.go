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

// Get 恒失败: bucket 名非法, 连键都不必看。
func (b badBucketKV) Get(string, any) (bool, error) { return false, b.err }

// Put 恒失败, 直接回传 bucket 名校验错误。
func (b badBucketKV) Put(string, any) error         { return b.err }

// Delete 恒失败, 直接回传 bucket 名校验错误。
func (b badBucketKV) Delete(string) error           { return b.err }

// Keys 恒失败, 返回 nil 切片 (而非空切片)。
func (b badBucketKV) Keys() ([]string, error)       { return nil, b.err }

// badBucketLog bucket 名校验失败时的 AppendLog 占位实现。
type badBucketLog struct{ err error }

// Append 恒失败, 直接回传 bucket 名校验错误。
func (b badBucketLog) Append(any) error                      { return b.err }

// ReadAll 恒失败, 不调用回调。
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

// Get 取单键并反序列化到 out; 键不存在返回 (false, nil), 反序列化失败返回错误。
//
// 注意：持的是 RLock, 但 load 会整文件读盘 —— 与 Put/Delete 的写锁互斥,
// 所以同一 bucket 上读与写不会交错, 代价是每次读都要重解析整份 JSON。
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

// Put 写入单键: 读整份 map → 改一个键 → 整文件原子写回。
//
// 算法解读：
//
//	原理：这个后端没有增量写, 单键更新就是"读-改-写" (RMW) 整份 JSON 文件。要正确,
//		整个 RMW 必须在同一把写锁里完成。
//	关键不变量：**加锁区间覆盖 load 与 save 两者**。若把 Marshal 放在锁外 (本函数确实
//		先 Marshal 再加锁 —— 那是安全的, 因为 v 是调用方私有值), 但 load 一旦脱离锁,
//		两个并发 Put 就会互相覆盖: 后写的那份 map 来自更早的快照, 先写的那条键就丢了。
//	代价与边界：O(文件大小) 的写放大 —— 每改一个键就重写整个 bucket 文件并 fsync 一次。
//		bucket 越大越慢, 这是**单键高频写**的退化点; 跨进程并发写则完全不在保证范围。
//	设计权衡：换成"每键一文件"能消掉写放大, 但会让 Keys() 变成目录扫描、且失去
//		"整 bucket 一次原子替换"这个便宜的全量一致性。当前场景 (配置/检查点类低频写)
//		选整文件替换更划算。
func (k *fileKV) Put(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("statestore: 序列化 key %q 失败: %w", key, err)
	}
	// 加锁区间必须**覆盖 load 到 save 的整个 RMW**: 一旦 load 在锁外取快照,
	// 两个并发 Put 就会用各自的旧快照整文件覆盖, 后写的那份把先写的键抹掉。
	k.lock.Lock()
	defer k.lock.Unlock()
	m, err := k.load()
	if err != nil {
		return err
	}
	m[key] = raw
	return k.save(m)
}

// Delete 删除单键; 键本就不存在时直接返回, 不触发一次多余的整文件写回 (幂等)。
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

// Keys 返回全部键并**按字典序排序**; map 遍历本身无序, 排序是为了让调用方拿到稳定输出。
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

// Append 把一条记录序列化成单行 JSON 并以 O_APPEND 追加; 目录按需创建。
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

// ReadAll 逐行读取日志并回调 fn; 文件不存在视为空日志, 尾部半行静默跳过。
//
// 算法解读：
//
//	原理：JSONL 追加写只有"整行原子"这一个保证, 崩溃可能留下一个写到一半的尾行。
//		这里用**延迟判定**把它与真正的数据损坏区分开: 遇到非法 JSON 行先记为
//		pendingInvalid 而不报错, 只有后续**还有内容**时才升级为错误。
//	关键不变量："非法行只允许出现在文件末尾"。pendingInvalid 一旦置位, 下一行任何
//		非空内容都会触发报错 —— 否则中间的损坏会被当成尾部截断悄悄吞掉, 调用方
//		拿到的就是一份静默残缺的日志。
//	代价与边界：单遍扫描, O(字节数); 单行上限 16MB (maxLogLine), 超长行会让
//		scanner 直接报错而不是截断。持的是互斥锁而非读锁, 所以读日志会阻塞写日志。
//	设计权衡：不引入校验和/长度前缀 (那要改写入格式, 破坏 append-only 的可 grep 性),
//		而是把"容忍一次"这件事限制在文件最末, 用最小的格式约束换崩溃安全。
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

// Put 按 sha256 内容寻址存一份 Blob, 返回哈希; 同内容重复写入直接命中已有文件。
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

// Get 按哈希读取 Blob 内容; 文件缺失时返回"不存在"错误 (而非空内容)。
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

// Has 判断 Blob 是否已存在; 哈希格式非法同样返回 false。
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
//
// 算法解读：
//
//	原理：POSIX rename 在同一文件系统内是原子的 —— 读者要么看到旧文件, 要么看到
//		新文件, 永远看不到"写了一半"的中间态。所以先把内容写进同目录的临时文件,
//		落盘后再改名顶上。
//	关键不变量：临时文件必须与目标**同目录**。跨目录 rename 会退化成"复制+删除"
//	(EXDEV), 原子性随之消失 —— 这也是这里不用 os.TempDir() 的原因。
//	代价与边界：每次写都要 CreateTemp + Write + fsync + rename 四次系统调用, 且
//		fsync 让写入变慢; 另外**没有 fsync 父目录**, 所以机器掉电后 rename 本身
//		可能还没落盘 (文件系统日志通常能兜住, 但严格说不保证)。
//	设计权衡：不用"原地改写 + 备份文件"那套 (读者可能读到半截, 崩溃后还要判断该
//		用哪一份); 临时文件命名用 CreateTemp 的随机后缀, 天然避免并发写互相覆盖。
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
